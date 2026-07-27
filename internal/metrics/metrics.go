// Package metrics defines Prometheus metrics for the Runkite control plane.
package metrics

import "github.com/prometheus/client_golang/prometheus"

var (
	// HTTP request metrics
	HTTPRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "runkite_http_requests_total",
			Help: "Total number of HTTP requests",
		},
		[]string{"method", "path", "status"},
	)

	HTTPRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "runkite_http_request_duration_seconds",
			Help:    "HTTP request duration in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "path"},
	)

	// Run metrics
	RunsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "runkite_runs_total",
			Help: "Total number of runs created",
		},
		[]string{"agent_id", "status"},
	)

	RunDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "runkite_run_duration_seconds",
			Help:    "Run execution duration in seconds",
			Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300},
		},
		[]string{"agent_id"},
	)

	ActiveRuns = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "runkite_active_runs",
		Help: "Number of currently active runs",
	})

	// Queue metrics
	QueueDepth = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "runkite_queue_depth",
		Help: "Number of jobs currently in the queue",
	})

	// SSE connections
	ActiveSSEConnections = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "runkite_active_sse_connections",
		Help: "Number of active SSE streaming connections",
	})
)

func init() {
	prometheus.MustRegister(
		HTTPRequestsTotal, HTTPRequestDuration,
		RunsTotal, RunDuration, ActiveRuns,
		QueueDepth, ActiveSSEConnections,
	)
}

// SetQueueDepth updates the queue-depth gauge. Call after enqueue/dequeue.
func SetQueueDepth(n int64) {
	QueueDepth.Set(float64(n))
}
