package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/dedezza1D/taskflow/internal/mail"
	"github.com/dedezza1D/taskflow/internal/objects"
	"github.com/dedezza1D/taskflow/internal/observability"
	"github.com/dedezza1D/taskflow/internal/queue"
	"github.com/dedezza1D/taskflow/internal/store"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

type Server struct {
	httpServer     *http.Server
	logger         *zap.Logger
	store          *store.Store
	queue          *queue.Queue
	objects        objects.Store
	maxUploadBytes int64
	authEnabled    bool
	secureCookies  bool
	sessionTTL     time.Duration
	mailer         mail.Sender
	baseURL        string
	resetTTL       time.Duration
}

type Config struct {
	Port string
	// Objects enables the /documents endpoints (nil disables them with a 503,
	// which is also what keeps the existing NewServer call sites and tests
	// source-compatible).
	Objects objects.Store
	// MaxUploadBytes bounds POST /documents uploads; <=0 falls back to 25 MiB.
	MaxUploadBytes int64
	// AuthEnabled turns on session authentication. When false every request runs
	// as the local principal — correct for the single-user desktop build, wide
	// open anywhere else, which is why it must be set deliberately.
	AuthEnabled bool
	// SecureCookies marks the session cookie Secure. Required over HTTPS;
	// leaving it on over plain HTTP means the browser drops the cookie and
	// nobody can stay signed in.
	SecureCookies bool
	// SessionTTL bounds a session's absolute lifetime; <=0 falls back to 12h.
	SessionTTL time.Duration
	// Mailer enables password recovery. Nil disables it — correct for the
	// desktop build, where there is no mail and no server to recover against.
	Mailer mail.Sender
	// BaseURL is the public origin used to build recovery links. It cannot be
	// derived from the request: Host is attacker-controlled, and trusting it
	// would let someone mail a victim a link pointing at their own server.
	BaseURL string
	// ResetTTL bounds a recovery link's life; <=0 falls back to 1h.
	ResetTTL time.Duration
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

	maxUpload := cfg.MaxUploadBytes
	if maxUpload <= 0 {
		maxUpload = 25 << 20
	}
	sessionTTL := cfg.SessionTTL
	if sessionTTL <= 0 {
		sessionTTL = 12 * time.Hour
	}
	resetTTL := cfg.ResetTTL
	if resetTTL <= 0 {
		resetTTL = time.Hour
	}

	srv := &Server{
		logger:         logger,
		store:          st,
		queue:          q,
		objects:        cfg.Objects,
		maxUploadBytes: maxUpload,
		authEnabled:    cfg.AuthEnabled,
		secureCookies:  cfg.SecureCookies,
		sessionTTL:     sessionTTL,
		mailer:         cfg.Mailer,
		baseURL:        cfg.BaseURL,
		resetTTL:       resetTTL,
	}

	// Resolves the caller for every request; rejection is per-route below.
	r.Use(srv.principalMiddleware)

	// Metrics
	r.Handle("/metrics", promhttp.Handler()).Methods(http.MethodGet)

	// Health — unauthenticated on purpose: load balancers and container
	// healthchecks probe it before anyone can sign in.
	r.HandleFunc("/api/v1/health", srv.handleHealth).Methods(http.MethodGet)

	// Auth. Login must stay open (it is how you stop being anonymous); the rest
	// only needs *a* session, hence viewer.
	r.HandleFunc("/api/v1/auth/config", srv.handleAuthConfig).Methods(http.MethodGet)
	r.HandleFunc("/api/v1/auth/login", srv.handleLogin).Methods(http.MethodPost)
	// Both recovery routes are open by necessity: whoever needs them cannot
	// sign in. Holding the emailed token is the authentication.
	r.HandleFunc("/api/v1/auth/forgot-password", srv.handleForgotPassword).Methods(http.MethodPost)
	r.HandleFunc("/api/v1/auth/reset-password", srv.handleResetPassword).Methods(http.MethodPost)
	r.HandleFunc("/api/v1/auth/logout", srv.handleLogout).Methods(http.MethodPost)
	r.HandleFunc("/api/v1/auth/me", srv.handleMe).Methods(http.MethodGet)
	r.HandleFunc("/api/v1/auth/password", srv.requireRole(store.RoleViewer, srv.handleChangePassword)).Methods(http.MethodPost)

	// User administration.
	r.HandleFunc("/api/v1/users", srv.requireRole(store.RoleAdmin, srv.handleListUsers)).Methods(http.MethodGet)
	r.HandleFunc("/api/v1/users", srv.requireRole(store.RoleAdmin, srv.handleCreateUser)).Methods(http.MethodPost)
	r.HandleFunc("/api/v1/users/{id}", srv.requireRole(store.RoleAdmin, srv.handleDeleteUser)).Methods(http.MethodDelete)
	r.HandleFunc("/api/v1/users/{id}/password", srv.requireRole(store.RoleAdmin, srv.handleResetUserPassword)).Methods(http.MethodPost)

	// Tasks — the engine's own surface. Reading is a viewer's right; creating
	// arbitrary tasks is not, so it sits with analyst alongside uploads.
	r.HandleFunc("/api/v1/tasks", srv.requireRole(store.RoleAnalyst, srv.handleCreateTask)).Methods(http.MethodPost)
	r.HandleFunc("/api/v1/tasks", srv.requireRole(store.RoleViewer, srv.handleListTasks)).Methods(http.MethodGet)
	r.HandleFunc("/api/v1/tasks/{id}", srv.requireRole(store.RoleViewer, srv.handleGetTask)).Methods(http.MethodGet)

	// Task executions
	r.HandleFunc("/api/v1/tasks/{id}/executions", srv.requireRole(store.RoleViewer, srv.handleListExecutions)).Methods(http.MethodGet)

	// Documents (compliance pipeline). Erasure is irreversible, so it is the one
	// document action reserved for admin.
	r.HandleFunc("/api/v1/documents", srv.requireRole(store.RoleAnalyst, srv.handleCreateDocument)).Methods(http.MethodPost)
	r.HandleFunc("/api/v1/documents", srv.requireRole(store.RoleViewer, srv.handleListDocuments)).Methods(http.MethodGet)
	r.HandleFunc("/api/v1/documents/{id}", srv.requireRole(store.RoleViewer, srv.handleGetDocument)).Methods(http.MethodGet)
	r.HandleFunc("/api/v1/documents/{id}/report", srv.requireRole(store.RoleViewer, srv.handleGetDocumentReport)).Methods(http.MethodGet)
	r.HandleFunc("/api/v1/documents/{id}", srv.requireRole(store.RoleAdmin, srv.handleDeleteDocument)).Methods(http.MethodDelete)

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

// Handler exposes the routed handler so a caller can serve it on a listener it
// owns — the desktop build binds loopback on an ephemeral port and wraps this
// with static file serving, instead of using Start.
func (s *Server) Handler() http.Handler {
	return s.httpServer.Handler
}

func (s *Server) Start() error {
	s.logger.Info("HTTP server starting", zap.String("addr", s.httpServer.Addr))
	return s.httpServer.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info("HTTP server shutting down")
	return s.httpServer.Shutdown(ctx)
}
