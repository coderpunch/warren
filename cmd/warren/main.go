// Command warren is a Kubernetes orchestrator for Open Terminal sandboxes.
package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
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
// An empty variable is the same as an absent variable.
func envDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func parseFlags(args []string) (config, error) {
	var c config
	allowSetCookie := false
	if v := os.Getenv("WARREN_ALLOW_SET_COOKIE"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return c, fmt.Errorf("WARREN_ALLOW_SET_COOKIE: %q is not true or false", v)
		}
		allowSetCookie = b
	}
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
	fs.BoolVar(&c.allowSetCookie, "allow-set-cookie", allowSetCookie, "let an application in a sandbox set browser cookies (not recommended)")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	// The flag package stops at the first argument that is not a flag and
	// ignores all subsequent flags. Refuse such a command line.
	if fs.NArg() > 0 {
		return c, fmt.Errorf("unexpected argument %q (write a boolean flag as --flag=false)", fs.Arg(0))
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

func devBackend(c config, logger *slog.Logger) (backend.Backend, error) {
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
	return backend.Static{Server: backend.Target{URL: u, APIKey: k}}, nil
}

// reloadInterval is the time between two checks of the key file and the
// certificate files. Kubernetes changes the files of a mounted Secret when
// the Secret changes.
const reloadInterval = 30 * time.Second

// watchKeys reads the key file again when its content changes. Thus an
// administrator can replace the connection key with no restart.
func watchKeys(ctx context.Context, path string, keys *auth.Keys, logger *slog.Logger) {
	digest := func() ([sha256.Size]byte, []string, error) {
		list, err := auth.ReadKeyFile(path)
		return sha256.Sum256([]byte(strings.Join(list, "\n"))), list, err
	}
	last, _, _ := digest()
	ticker := time.NewTicker(reloadInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		now, list, err := digest()
		if err != nil {
			logger.Warn("cannot read the key file. The current keys stay.", "error", err.Error())
			continue
		}
		if now == last {
			continue
		}
		last = now
		if err := keys.Replace(list); err != nil {
			logger.Warn("the new key file is not valid. The current keys stay.", "error", err.Error())
			continue
		}
		logger.Info("connection keys replaced", "keys", keys.Len())
	}
}

// certLoader reads the TLS certificate again when its files change. Thus a
// renewed certificate needs no restart.
type certLoader struct {
	certFile, keyFile string
	logger            *slog.Logger

	mu      sync.Mutex
	cert    *tls.Certificate
	stamp   string
	checked time.Time
}

func (l *certLoader) fileStamp() string {
	var b strings.Builder
	for _, f := range []string{l.certFile, l.keyFile} {
		if st, err := os.Stat(f); err == nil {
			fmt.Fprintf(&b, "%d:%d;", st.ModTime().UnixNano(), st.Size())
		}
	}
	return b.String()
}

func (l *certLoader) load() error {
	cert, err := tls.LoadX509KeyPair(l.certFile, l.keyFile)
	if err != nil {
		return err
	}
	l.cert = &cert
	return nil
}

func (l *certLoader) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if time.Since(l.checked) > reloadInterval {
		l.checked = time.Now()
		if stamp := l.fileStamp(); stamp != l.stamp {
			l.stamp = stamp
			if err := l.load(); err != nil {
				l.logger.Warn("cannot read the new TLS certificate. The current certificate stays.", "error", err.Error())
			} else {
				l.logger.Info("TLS certificate replaced")
			}
		}
	}
	return l.cert, nil
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
	sandboxes, err := devBackend(c, logger)
	if err != nil {
		return err
	}

	logger.Warn("warren trusts the identity headers of each client that has the connection key. " +
		"Keep the key secret, and permit network connections to warren only from Open WebUI.")
	if c.tlsCertFile == "" {
		logger.Warn("TLS is off. The connection key goes through the network as clear text.")
	}
	if c.allowSetCookie {
		logger.Warn("allow-set-cookie is on. An application in a sandbox can set cookies for the origin of Open WebUI.")
	}

	px := proxy.New(proxy.Options{
		Keys:           keys,
		Policies:       policies,
		Backend:        sandboxes,
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
		loader := &certLoader{certFile: c.tlsCertFile, keyFile: c.tlsKeyFile, logger: logger}
		if err := loader.load(); err != nil {
			return fmt.Errorf("TLS certificate: %w", err)
		}
		loader.stamp, loader.checked = loader.fileStamp(), time.Now()
		main.TLSConfig = &tls.Config{
			MinVersion:     tls.VersionTLS13,
			GetCertificate: loader.get,
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
	go watchKeys(ctx, c.apiKeyFile, keys, logger)

	errc := make(chan error, 2)
	go func() {
		logger.Info("listen", "addr", c.listen, "tls", c.tlsCertFile != "",
			"policies", len(policies.List()), "keys", keys.Len())
		if c.tlsCertFile != "" {
			errc <- main.ListenAndServeTLS("", "")
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
	// A second signal stops the process immediately.
	stop()
	logger.Info("shutdown")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = admin.Shutdown(shutdownCtx)
	// Close the WebSockets with a close message. Their audit records are
	// written before the process stops.
	if err := px.Shutdown(shutdownCtx); err != nil {
		logger.Warn("some WebSockets did not close in time")
	}
	if err := main.Shutdown(shutdownCtx); err != nil {
		logger.Warn("some requests did not complete in time")
		_ = main.Close()
	}
	return nil
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
