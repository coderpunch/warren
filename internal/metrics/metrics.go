// Package metrics has the Prometheus metrics of warren.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics is the set of metrics. All methods are safe on a nil pointer, thus
// a component can operate with no metrics.
type Metrics struct {
	registry    *prometheus.Registry
	coldStart   prometheus.Histogram
	reaps       *prometheus.CounterVec
	creates     *prometheus.CounterVec
	proxyErrors *prometheus.CounterVec
	requests    *prometheus.CounterVec
	websockets  prometheus.Gauge
}

// New makes the metrics and their registry.
func New() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		coldStart: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "warren_sandbox_cold_start_seconds",
			Help:    "Time from the start request of a sandbox to its first use.",
			Buckets: []float64{1, 2, 3, 5, 8, 13, 21, 34, 55, 89, 144},
		}),
		reaps: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "warren_sandbox_reaps_total",
			Help: "Sandboxes that warren stopped, by reason.",
		}, []string{"reason"}),
		creates: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "warren_sandbox_creates_total",
			Help: "Start requests for a sandbox, by result.",
		}, []string{"result"}),
		proxyErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "warren_proxy_errors_total",
			Help: "Requests that warren could not send to a sandbox, by kind.",
		}, []string{"kind"}),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "warren_http_requests_total",
			Help: "HTTP requests from Open WebUI, by status class.",
		}, []string{"code"}),
		websockets: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "warren_websockets_open",
			Help: "WebSockets that are open at this time.",
		}),
	}
	m.registry.MustRegister(m.coldStart, m.reaps, m.creates, m.proxyErrors, m.requests, m.websockets,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

// SetSandboxCounts registers the function that counts the sandboxes.
func (m *Metrics) SetSandboxCounts(count func() (ready, starting int)) {
	if m == nil {
		return
	}
	desc := prometheus.GaugeOpts{
		Name: "warren_sandboxes",
		Help: "Sandboxes that exist at this time, by state.",
	}
	ready := desc
	ready.ConstLabels = prometheus.Labels{"state": "ready"}
	starting := desc
	starting.ConstLabels = prometheus.Labels{"state": "starting"}
	m.registry.MustRegister(
		prometheus.NewGaugeFunc(ready, func() float64 { r, _ := count(); return float64(r) }),
		prometheus.NewGaugeFunc(starting, func() float64 { _, s := count(); return float64(s) }),
	)
}

// Handler serves the metrics in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// ColdStart records the start time of one sandbox.
func (m *Metrics) ColdStart(d time.Duration) {
	if m != nil {
		m.coldStart.Observe(d.Seconds())
	}
}

// Reap counts one sandbox that warren stopped.
func (m *Metrics) Reap(reason string) {
	if m != nil {
		m.reaps.WithLabelValues(reason).Inc()
	}
}

// Create counts one start request.
func (m *Metrics) Create(result string) {
	if m != nil {
		m.creates.WithLabelValues(result).Inc()
	}
}

// ProxyError counts one request that did not get to a sandbox.
func (m *Metrics) ProxyError(kind string) {
	if m != nil {
		m.proxyErrors.WithLabelValues(kind).Inc()
	}
}

// Request counts one HTTP request by its status class, for example "2xx".
func (m *Metrics) Request(status int) {
	if m != nil {
		m.requests.WithLabelValues(strconv.Itoa(status/100) + "xx").Inc()
	}
}

// WebSocketOpened counts a WebSocket that is now open.
func (m *Metrics) WebSocketOpened() {
	if m != nil {
		m.websockets.Inc()
	}
}

// WebSocketClosed counts a WebSocket that is now closed.
func (m *Metrics) WebSocketClosed() {
	if m != nil {
		m.websockets.Dec()
	}
}
