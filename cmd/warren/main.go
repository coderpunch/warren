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
	"sync/atomic"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/h3xcat/warren/internal/api"
	"github.com/h3xcat/warren/internal/auth"
	"github.com/h3xcat/warren/internal/backend"
	"github.com/h3xcat/warren/internal/kube"
	"github.com/h3xcat/warren/internal/metrics"
	"github.com/h3xcat/warren/internal/openapi"
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
	shutdownDelay  time.Duration

	kubeconfig       string
	sandboxNamespace string
	sandboxImage     string
	idleTimeout      time.Duration
	startTimeout     time.Duration
	maxPerUser       int
	maxSandboxes     int
	maxVolumes       int
	maxVolumesTotal  int
	volumeDelay      time.Duration
	openAPIFile      string
}

// defaultImage is the image variant of Open Terminal that needs no root
// privilege (docs/open-terminal-api.md, section 8.5).
const defaultImage = "ghcr.io/open-webui/open-terminal:0.18.1-openshift"

func envDuration(name string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a duration", name, v)
	}
	return d, nil
}

func envInt(name string, def int) (int, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a number", name, v)
	}
	return n, nil
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
	idle, err := envDuration("WARREN_IDLE_TIMEOUT", 30*time.Minute)
	if err != nil {
		return c, err
	}
	startTimeout, err := envDuration("WARREN_START_TIMEOUT", 10*time.Minute)
	if err != nil {
		return c, err
	}
	maxPerUser, err := envInt("WARREN_MAX_SANDBOXES_PER_USER", 5)
	if err != nil {
		return c, err
	}
	maxSandboxes, err := envInt("WARREN_MAX_SANDBOXES", 0)
	if err != nil {
		return c, err
	}
	maxVolumes, err := envInt("WARREN_MAX_VOLUMES_PER_USER", 20)
	if err != nil {
		return c, err
	}
	maxVolumesTotal, err := envInt("WARREN_MAX_VOLUMES", 0)
	if err != nil {
		return c, err
	}
	volumeDelay, err := envDuration("WARREN_VOLUME_DELETE_DELAY", 10*time.Minute)
	if err != nil {
		return c, err
	}
	shutdownDelay, err := envDuration("WARREN_SHUTDOWN_DELAY", 5*time.Second)
	if err != nil {
		return c, err
	}
	fs := flag.NewFlagSet("warren", flag.ContinueOnError)
	fs.StringVar(&c.kubeconfig, "kubeconfig", envDefault("WARREN_KUBECONFIG", ""), "kubeconfig file (the default is the configuration of the pod)")
	fs.StringVar(&c.sandboxNamespace, "sandbox-namespace", envDefault("WARREN_SANDBOX_NAMESPACE", ""), "namespace for the sandbox pods")
	fs.StringVar(&c.sandboxImage, "sandbox-image", envDefault("WARREN_SANDBOX_IMAGE", defaultImage), "container image of a sandbox, for a policy that sets no image")
	fs.DurationVar(&c.idleTimeout, "idle-timeout", idle, "stop a sandbox after this time with no use, for a policy that sets no time (0 = never)")
	fs.DurationVar(&c.startTimeout, "start-timeout", startTimeout, "delete a sandbox that is not ready after this time")
	fs.IntVar(&c.maxPerUser, "max-sandboxes-per-user", maxPerUser, "maximum number of sandboxes that one user can start (0 = no limit)")
	fs.IntVar(&c.maxSandboxes, "max-sandboxes", maxSandboxes, "maximum number of sandboxes (0 = no limit)")
	fs.IntVar(&c.maxVolumes, "max-volumes-per-user", maxVolumes, "maximum number of volumes of the chat scope and the model scope that one user can have (0 = no limit)")
	fs.IntVar(&c.maxVolumesTotal, "max-volumes", maxVolumesTotal, "maximum number of volumes (0 = no limit)")
	fs.DurationVar(&c.volumeDelay, "volume-delete-delay", volumeDelay, "after a start of warren, delete no volume for this time")
	fs.DurationVar(&c.shutdownDelay, "shutdown-delay", shutdownDelay, "after a stop signal, continue to serve for this time, until Kubernetes sends no more requests")
	fs.StringVar(&c.openAPIFile, "openapi-file", envDefault("WARREN_OPENAPI_FILE", ""), "OpenAPI document for the tool list (the default is the document of Open Terminal 0.18.1)")
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
	if c.devUpstreamURL == "" && c.sandboxNamespace == "" {
		return c, errors.New("--sandbox-namespace is necessary")
	}
	if c.idleTimeout < 0 || c.startTimeout <= 0 || c.maxPerUser < 0 || c.maxSandboxes < 0 || c.maxVolumes < 0 || c.maxVolumesTotal < 0 || c.volumeDelay < 0 || c.shutdownDelay < 0 {
		return c, errors.New("a time or a limit has a negative value")
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

// kubeBackend makes the Kubernetes backend.
func kubeBackend(c config, policies *policy.Store, logger *slog.Logger, m *metrics.Metrics) (*kube.Manager, error) {
	var restConfig *rest.Config
	var err error
	if c.kubeconfig != "" {
		restConfig, err = clientcmd.BuildConfigFromFlags("", c.kubeconfig)
	} else {
		restConfig, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("configuration for Kubernetes: %w", err)
	}
	// The default of client-go is 5 requests each second for the process.
	// One start of a sandbox needs 4 requests.
	restConfig.QPS = 50
	restConfig.Burst = 100
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("client for Kubernetes: %w", err)
	}
	return kube.New(kube.Config{
		Client:            client,
		Namespace:         c.sandboxNamespace,
		Image:             c.sandboxImage,
		Policies:          policies,
		IdleTimeout:       c.idleTimeout,
		StartTimeout:      c.startTimeout,
		MaxPerUser:        c.maxPerUser,
		MaxTotal:          c.maxSandboxes,
		MaxVolumesPerUser: c.maxVolumes,
		MaxVolumes:        c.maxVolumesTotal,
		VolumeDeleteDelay: c.volumeDelay,
		Logger:            logger,
		Metrics:           m,
	})
}

func devBackend(c config, logger *slog.Logger) (backend.Backend, error) {
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
var reloadInterval = 30 * time.Second

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

// watchPolicies reads the policy file again when its content changes. Thus
// an administrator can change a policy with no restart, and the open
// terminals stay open. If the new file is not valid, the current policies
// stay.
//
// last is the digest of the file content that is in use. The caller gives
// it, thus a change between the first read and the start of this function
// is not lost.
func watchPolicies(ctx context.Context, path string, last [sha256.Size]byte, store *policy.Store, logger *slog.Logger) {
	digest := func() ([sha256.Size]byte, []byte, error) {
		data, err := os.ReadFile(path)
		return sha256.Sum256(data), data, err
	}
	ticker := time.NewTicker(reloadInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		now, data, err := digest()
		if err != nil {
			logger.Warn("cannot read the policy file. The current policies stay.", "error", err.Error())
			continue
		}
		if now == last {
			continue
		}
		last = now
		set, err := policy.Parse(data)
		if err != nil {
			logger.Error("the new policy file is not valid. The current policies stay.", "error", err.Error())
			continue
		}
		store.Replace(set)
		logger.Info("policies replaced", "policies", len(set.List()))
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
	policyData, err := os.ReadFile(c.policyFile)
	if err != nil {
		return fmt.Errorf("read policy file: %w", err)
	}
	policySet, err := policy.Parse(policyData)
	if err != nil {
		return fmt.Errorf("policy file %s: %w", c.policyFile, err)
	}
	policies := policy.NewStore(policySet)
	tools := openapi.Default()
	if c.openAPIFile != "" {
		data, err := os.ReadFile(c.openAPIFile)
		if err != nil {
			return fmt.Errorf("OpenAPI file: %w", err)
		}
		if tools, err = openapi.Parse(data); err != nil {
			return fmt.Errorf("OpenAPI file %s: %w", c.openAPIFile, err)
		}
	}

	metric := metrics.New()
	var sandboxes backend.Backend
	var manager *kube.Manager
	var instances api.Instances
	if c.devUpstreamURL != "" {
		if sandboxes, err = devBackend(c, logger); err != nil {
			return err
		}
	} else {
		if manager, err = kubeBackend(c, policies, logger, metric); err != nil {
			return err
		}
		sandboxes, instances = manager, manager
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
		OpenAPI:        tools,
		Metrics:        metric,
	})
	main := &http.Server{
		Addr: c.listen,
		Handler: &server.Server{
			Keys:    keys,
			API:     &api.Handler{Policies: policies, Instances: instances, DefaultImage: c.sandboxImage},
			Proxy:   px,
			Logger:  logger,
			Metrics: metric,
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
	var draining atomic.Bool
	adminMux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		// After a stop signal, tell Kubernetes to send no more requests.
		if draining.Load() {
			http.Error(w, "warren stops", http.StatusServiceUnavailable)
			return
		}
		// Warren is ready when it knows the sandboxes of the cluster.
		if manager != nil && !manager.Ready() {
			http.Error(w, "the sandbox cache is not ready", http.StatusServiceUnavailable)
			return
		}
		ok(w, r)
	})
	adminMux.Handle("GET /metrics", metric.Handler())
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
	go watchPolicies(ctx, c.policyFile, sha256.Sum256(policyData), policies, logger)

	errc := make(chan error, 3)
	// The pod cache must operate until the last request is complete. Thus
	// the manager does not stop on the signal. It stops when run returns.
	managerCtx, stopManager := context.WithCancel(context.Background())
	defer stopManager()
	if manager != nil {
		go func() {
			if err := manager.Run(managerCtx); err != nil {
				errc <- err
			}
		}()
	}
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
	logger.Info("shutdown", "delay", c.shutdownDelay.String())
	// Kubernetes removes a pod that stops from its Service a short time
	// after the signal. Continue to serve in that time.
	draining.Store(true)
	time.Sleep(c.shutdownDelay)
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
