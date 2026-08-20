package httpapi

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
)

// metrics holds the server's instruments. They live in a registry owned by the
// Server rather than prometheus.DefaultRegisterer, because a global registry
// makes two servers in one process — a test binary, `heyarr all` later — a
// duplicate-registration panic, and it silently publishes whatever any
// dependency registered in an init function.
type metrics struct {
	requests  *prometheus.CounterVec
	duration  *prometheus.HistogramVec
	inFlight  prometheus.Gauge
	panics    prometheus.Counter
	authFails *prometheus.CounterVec
}

func newMetrics(reg prometheus.Registerer) (*metrics, error) {
	m := &metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "heyarr", Subsystem: "http", Name: "requests_total",
			Help: "HTTP requests by route pattern, method and status class.",
			// The label is the chi route *pattern*, never the path. A label
			// per blob hash would be an unbounded cardinality explosion that
			// takes the metrics endpoint, and often Prometheus, down with it.
		}, []string{"method", "route", "status_class"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "heyarr", Subsystem: "http", Name: "request_duration_seconds",
			Help: "HTTP request duration by route pattern.",
			// Range-serving a large blob is a normal request that takes
			// minutes, so the buckets run well past the usual web defaults.
			Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 30, 300},
		}, []string{"method", "route", "status_class"}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "heyarr", Subsystem: "http", Name: "requests_in_flight",
			Help: "HTTP requests currently being served.",
		}),
		panics: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "heyarr", Subsystem: "http", Name: "handler_panics_total",
			Help: "Handlers that panicked and were recovered.",
		}),
		authFails: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "heyarr", Subsystem: "http", Name: "auth_failures_total",
			Help: "Rejected credentials by reason.",
		}, []string{"reason"}),
	}
	for _, c := range []prometheus.Collector{m.requests, m.duration, m.inFlight, m.panics, m.authFails} {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("httpapi: registering metrics: %w", err)
		}
	}
	return m, nil
}

// statusClass buckets a status code as 2xx, 4xx and so on. The full code would
// multiply the series count for no operational gain — an alert cares that
// errors are up, and the log says which code.
func statusClass(code int) string {
	if code < 100 || code > 599 {
		return "unknown"
	}
	return strconv.Itoa(code/100) + "xx"
}

// routePattern is the chi pattern a request matched, or "unmatched". It is only
// known after the handler has run, which is why every use of it is deferred.
func routePattern(r *http.Request) string {
	if rc := chi.RouteContext(r.Context()); rc != nil {
		if p := rc.RoutePattern(); p != "" {
			return p
		}
	}
	return "unmatched"
}

// metricsMiddleware records every request. It sits *before* auth so that
// rejected requests are counted too: a spike of 401s is exactly the thing you
// want a graph of.
func (s *Server) metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.metrics.inFlight.Inc()
		defer s.metrics.inFlight.Dec()

		rec := wrapResponse(w)
		start := time.Now()
		defer func() {
			route := routePattern(r)
			class := statusClass(rec.status)
			s.metrics.requests.WithLabelValues(r.Method, route, class).Inc()
			s.metrics.duration.WithLabelValues(r.Method, route, class).
				Observe(time.Since(start).Seconds())
		}()
		next.ServeHTTP(rec, r)
	})
}
