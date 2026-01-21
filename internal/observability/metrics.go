package observability

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	HTTPRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "taskflow",
			Name:      "http_requests_total",
			Help:      "Total number of HTTP requests.",
		},
		[]string{"route", "method", "status"},
	)

	HTTPRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "taskflow",
			Name:      "http_request_duration_seconds",
			Help:      "HTTP request duration in seconds.",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{"route", "method"},
	)

	TasksStartedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "taskflow",
			Name:      "tasks_started_total",
			Help:      "Tasks started by workers.",
		},
		[]string{"type", "priority"},
	)

	TasksCompletedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "taskflow",
			Name:      "tasks_completed_total",
			Help:      "Tasks completed successfully.",
		},
		[]string{"type"},
	)

	TasksFailedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "taskflow",
			Name:      "tasks_failed_total",
			Help:      "Tasks failed.",
		},
		[]string{"type", "reason"},
	)

	TaskDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "taskflow",
			Name:      "task_duration_seconds",
			Help:      "Task execution duration in seconds.",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{"type"},
	)
)

func RegisterMetrics() {
	// MustRegister is safe to call once; if tests call multiple times, use Register and ignore errors.
	prometheus.MustRegister(
		HTTPRequestsTotal,
		HTTPRequestDuration,
		TasksStartedTotal,
		TasksCompletedTotal,
		TasksFailedTotal,
		TaskDuration,
	)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// HTTPMetricsMiddleware records basic HTTP request metrics.
func HTTPMetricsMiddleware(routeName func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: 200}
			next.ServeHTTP(rec, r)

			route := routeName(r)
			method := r.Method
			status := strconv.Itoa(rec.status)

			HTTPRequestsTotal.WithLabelValues(route, method, status).Inc()
			HTTPRequestDuration.WithLabelValues(route, method).Observe(time.Since(start).Seconds())
		})
	}
}
