package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/dedezza1D/taskflow/internal/observability"
	"github.com/dedezza1D/taskflow/internal/queue"
	"github.com/dedezza1D/taskflow/internal/store"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

type Server struct {
	httpServer *http.Server
	logger     *zap.Logger
	store      *store.Store
	queue      *queue.Queue
}

type Config struct {
	Port string
}

func NewServer(cfg Config, logger *zap.Logger, st *store.Store, q *queue.Queue) *Server {
	r := mux.NewRouter()

routeName := func(r *http.Request) string {
	if rt := mux.CurrentRoute(r); rt != nil {
		if tpl, err := rt.GetPathTemplate(); err == nil && tpl != "" {
			return tpl
		}
	}
	return r.URL.Path
}

// Middlewares (order matters)
r.Use(observability.RequestIDMiddleware)
r.Use(observability.TracingMiddleware(routeName))
r.Use(observability.HTTPMetricsMiddleware(routeName))
r.Use(observability.AccessLogMiddleware(logger, routeName))

	srv := &Server{
		logger: logger,
		store:  st,
		queue:  q,
	}

	// Metrics
	r.Handle("/metrics", promhttp.Handler()).Methods(http.MethodGet)

	// Health
	r.HandleFunc("/api/v1/health", srv.handleHealth).Methods(http.MethodGet)

	// Tasks
	r.HandleFunc("/api/v1/tasks", srv.handleCreateTask).Methods(http.MethodPost)
	r.HandleFunc("/api/v1/tasks", srv.handleListTasks).Methods(http.MethodGet)
	r.HandleFunc("/api/v1/tasks/{id}", srv.handleGetTask).Methods(http.MethodGet)

	// Task executions
	r.HandleFunc("/api/v1/tasks/{id}/executions", srv.handleListExecutions).Methods(http.MethodGet)

	s := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	srv.httpServer = s
	return srv
}

func (s *Server) Start() error {
	s.logger.Info("HTTP server starting", zap.String("addr", s.httpServer.Addr))
	return s.httpServer.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info("HTTP server shutting down")
	return s.httpServer.Shutdown(ctx)
}
