package collector

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/goodtune/amber_exporter/amber"
)

// APIMetrics reports how the exporter uses the Amber API.
type APIMetrics struct {
	requests *prometheus.CounterVec
	duration *prometheus.CounterVec

	rateLimit func() amber.RateLimit

	limit     *prometheus.Desc
	remaining *prometheus.Desc
	reset     *prometheus.Desc
}

// NewAPIMetrics makes the API metrics. Pass Observe to the client as its
// request observer, then call SetRateLimitSource with the client.
func NewAPIMetrics() *APIMetrics {
	return &APIMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "api",
			Name:      "requests_total",
			Help:      "Requests sent to the Amber API, by endpoint and HTTP status code. The code is \"error\" when there was no response.",
		}, []string{"endpoint", "code"}),
		duration: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "api",
			Name:      "request_duration_seconds_total",
			Help:      "Total time spent on requests to the Amber API, by endpoint.",
		}, []string{"endpoint"}),
		limit: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "api", "ratelimit_limit"),
			"Number of requests the Amber API allows in one rate limit window.",
			nil, nil,
		),
		remaining: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "api", "ratelimit_remaining"),
			"Number of requests left in the rate limit window, as of the last API response.",
			nil, nil,
		),
		reset: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "api", "ratelimit_reset_timestamp_seconds"),
			"End of the rate limit window as a Unix timestamp, as of the last API response.",
			nil, nil,
		),
	}
}

// Observe records one API request. It has the signature of
// amber.RequestObserver.
func (m *APIMetrics) Observe(endpoint string, status int, d time.Duration) {
	code := "error"
	if status != 0 {
		code = strconv.Itoa(status)
	}
	m.requests.WithLabelValues(endpoint, code).Inc()
	m.duration.WithLabelValues(endpoint).Add(d.Seconds())
}

// SetRateLimitSource sets where the rate limit state is read from.
func (m *APIMetrics) SetRateLimitSource(source func() amber.RateLimit) {
	m.rateLimit = source
}

// Describe implements prometheus.Collector.
func (m *APIMetrics) Describe(ch chan<- *prometheus.Desc) {
	m.requests.Describe(ch)
	m.duration.Describe(ch)
	ch <- m.limit
	ch <- m.remaining
	ch <- m.reset
}

// Collect implements prometheus.Collector.
func (m *APIMetrics) Collect(ch chan<- prometheus.Metric) {
	m.requests.Collect(ch)
	m.duration.Collect(ch)
	if m.rateLimit == nil {
		return
	}
	rl := m.rateLimit()
	if !rl.Known {
		return
	}
	gauge(ch, m.limit, float64(rl.Limit))
	gauge(ch, m.remaining, float64(rl.Remaining))
	if !rl.Reset.IsZero() {
		gauge(ch, m.reset, unixSeconds(rl.Reset))
	}
}
