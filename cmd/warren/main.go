// Command warren is a Kubernetes orchestrator for Open Terminal sandboxes.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/h3xcat/warren/internal/api"
	"github.com/h3xcat/warren/internal/auth"
	"github.com/h3xcat/warren/internal/backend"
	"github.com/h3xcat/warren/internal/policy"
	"github.com/h3xcat/warren/internal/proxy"
	"github.com/h3xcat/warren/internal/server"
)

type config struct {
	listen         string
	adminListen    string
	apiKeyFile     string
	policyFile     string
	tlsCertFile    string
	tlsKeyFile     string
	devUpstreamURL string
	devUpstreamKey string
	logLevel       string
	allowSetCookie bool
}

// envDefault returns the value of an environment variable, or a default.
func envDefault(name, def string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return def
}

func parseFlags(args []string) (config, error) {
	var c config
	fs := flag.NewFlagSet("warren", flag.ContinueOnError)
	fs.StringVar(&c.listen, "listen", envDefault("WARREN_LISTEN", ":8080"), "address for Open WebUI traffic")
	fs.StringVar(&c.adminListen, "admin-listen", envDefault("WARREN_ADMIN_LISTEN", ":9090"), "address for health checks")
	fs.StringVar(&c.apiKeyFile, "api-key-file", envDefault("WARREN_API_KEY_FILE", ""), "file with the connection key (one key on each line)")
	fs.StringVar(&c.policyFile, "policy-file", envDefault("WARREN_POLICY_FILE", ""), "YAML file with the sandbox policies")
	fs.StringVar(&c.tlsCertFile, "tls-cert-file", envDefault("WARREN_TLS_CERT_FILE", ""), "TLS certificate file (optional)")
	fs.StringVar(&c.tlsKeyFile, "tls-key-file", envDefault("WARREN_TLS_KEY_FILE", ""), "TLS private key file (optional)")
	fs.StringVar(&c.devUpstreamURL, "dev-upstream-url", envDefault("WARREN_DEV_UPSTREAM_URL", ""), "DEVELOPMENT ONLY: send all sandboxes to this one Open Terminal server")
	fs.StringVar(&c.devUpstreamKey, "dev-upstream-key-file", envDefault("WARREN_DEV_UPSTREAM_KEY_FILE", ""), "DEVELOPMENT ONLY: file with the API key of that server")
	fs.StringVar(&c.logLevel, "log-level", envDefault("WARREN_LOG_LEVEL", "info"), "debug, info, warn, or error")
	fs.BoolVar(&c.allowSetCookie, "allow-set-cookie", envDefault("WARREN_ALLOW_SET_COOKIE", "") == "true", "let a sandbox set browser cookies (not recommended)")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if c.apiKeyFile == "" {
		return c, errors.New("--api-key-file is necessary")
	}
	if c.policyFile == "" {
		return c, errors.New("--policy-file is necessary")
	}
	if (c.tlsCertFile == "") != (c.tlsKeyFile == "") {
		return c, errors.New("--tls-cert-file and --tls-key-file must be set together")
	}
	return c, nil
}

func newLogger(level string) (*slog.Logger, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("log level: %w", err)
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l})), nil
}

func devResolver(c config, logger *slog.Logger) (backend.Resolver, error) {
	if c.devUpstreamURL == "" {
		return nil, errors.New("no sandbox backend: the Kubernetes backend is not in this version; set --dev-upstream-url for development")
	}
	u, err := url.Parse(c.devUpstreamURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("--dev-upstream-url is not an http or https URL: %q", c.devUpstreamURL)
	}
	if u.Path != "" && u.Path != "/" {
		return nil, errors.New("--dev-upstream-url must not have a path")
	}
	u.Path = ""
	if c.devUpstreamKey == "" {
		return nil, errors.New("--dev-upstream-key-file is necessary with --dev-upstream-url")
	}
	key, err := os.ReadFile(c.devUpstreamKey)
	if err != nil {
		return nil, fmt.Errorf("read upstream key: %w", err)
	}
	k := strings.TrimSpace(string(key))
	if k == "" {
		return nil, errors.New("the upstream key file is empty")
	}
	logger.Warn("DEVELOPMENT MODE: all users share one Open Terminal server. There is no isolation.",
		"upstream", u.String())
	return backend.Static{Target: backend.Target{URL: u, APIKey: k}}, nil
}

func run(args []string) error {
	c, err := parseFlags(args)
	if err != nil {
		return err
	}
	logger, err := newLogger(c.logLevel)
	if err != nil {
		return err
	}
	keys, err := auth.LoadKeyFile(c.apiKeyFile)
	if err != nil {
		return err
	}
	policies, err := policy.Load(c.policyFile)
	if err != nil {
		return err
	}
	resolver, err := devResolver(c, logger)
	if err != nil {
		return err
	}

	logger.Warn("warren trusts the identity headers of each client that has the connection key. " +
		"Keep the key secret, and permit network connections to warren only from Open WebUI.")
	if c.tlsCertFile == "" {
		logger.Warn("TLS is off. The connection key goes through the network as clear text.")
	}

	px := proxy.New(proxy.Options{
		Keys:           keys,
		Policies:       policies,
		Resolver:       resolver,
		Logger:         logger,
		AllowSetCookie: c.allowSetCookie,
	})
	main := &http.Server{
		Addr: c.listen,
		Handler: &server.Server{
			Keys:   keys,
			API:    &api.Handler{Policies: policies},
			Proxy:  px,
			Logger: logger,
		},
		// No read or write timeout: uploads, downloads, long requests, and
		// WebSockets have no time limit. The header timeout stops slow
		// clients that send no request.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	if c.tlsCertFile != "" {
		main.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS13,
			// WebSockets need HTTP/1.1.
			NextProtos: []string{"http/1.1"},
		}
	}

	adminMux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) }
	adminMux.HandleFunc("GET /healthz", ok)
	adminMux.HandleFunc("GET /readyz", ok)
	admin := &http.Server{
		Addr:              c.adminListen,
		Handler:           adminMux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 2)
	go func() {
		logger.Info("listen", "addr", c.listen, "tls", c.tlsCertFile != "", "policies", len(policies.List()))
		if c.tlsCertFile != "" {
			errc <- main.ListenAndServeTLS(c.tlsCertFile, c.tlsKeyFile)
		} else {
			errc <- main.ListenAndServe()
		}
	}()
	go func() {
		logger.Info("admin listen", "addr", c.adminListen)
		errc <- admin.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	logger.Info("shutdown")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = admin.Shutdown(shutdownCtx)
	return main.Shutdown(shutdownCtx)
}

func main() {
	if err := run(os.Args[1:]); err != nil && !errors.Is(err, http.ErrServerClosed) {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "warren:", err)
		os.Exit(1)
	}
}
