// Command taskflow-desktop is the whole application in one process.
//
// No server to connect to, no broker, no container: SQLite in a file the user
// owns, the queue backed by that same file, and an HTTP API bound to loopback.
// Documents never leave the machine, which is the entire argument for shipping a
// compliance tool this way — the scanner does not become another copy of the
// personal data it finds.
//
// What that costs, deliberately: no user accounts (there is one user) and no
// TLS (loopback, and no certificate authority will vouch for localhost). What
// stands in for authentication is a per-launch token and a strict Host check —
// loopback by itself is reachable from web pages and from other accounts on
// the machine; see guard.go.
// config.Validate refuses that combination whenever ENV=prod, so it cannot
// escape onto a served deployment by accident.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/dedezza1D/taskflow/api/httpapi"
	"github.com/dedezza1D/taskflow/internal/auth"
	"github.com/dedezza1D/taskflow/internal/config"
	"github.com/dedezza1D/taskflow/internal/logging"
	"github.com/dedezza1D/taskflow/internal/maintenance"
	"github.com/dedezza1D/taskflow/internal/objects"
	"github.com/dedezza1D/taskflow/internal/pipeline"
	"github.com/dedezza1D/taskflow/internal/queue"
	"github.com/dedezza1D/taskflow/internal/store"
	workerpkg "github.com/dedezza1D/taskflow/internal/worker"
	"go.uber.org/zap"
)

func main() {
	var (
		dataDir = flag.String("data-dir", "", "where the database and documents live (default: per-user application data)")
		addr    = flag.String("addr", "127.0.0.1:0", "listen address; port 0 picks a free one")
		webDir  = flag.String("web-dir", "", "optional directory of built frontend files to serve")
		tessBin = flag.String("tesseract", "", "path to a bundled tesseract binary (default: look on PATH)")
	)
	flag.Parse()

	logger, err := logging.New(logging.Config{Level: envOr("LOG_LEVEL", "info")})
	if err != nil {
		panic(err)
	}
	defer func() { _ = logger.Sync() }()

	dir, err := resolveDataDir(*dataDir)
	if err != nil {
		logger.Fatal("could not determine data directory", zap.Error(err))
	}
	logger.Info("data directory", zap.String("path", dir))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st, err := store.NewSQLite(ctx, filepath.Join(dir, "taskflow.db"))
	if err != nil {
		logger.Fatal("open local database failed", zap.Error(err))
	}
	defer st.Close()

	// documents.org_id references an organisation even with authentication off,
	// so the local tenant has to exist before the first upload. Creating it here
	// (rather than in the schema) keeps the id in one place: auth.LocalOrgID,
	// the same one the local principal carries.
	if err := ensureLocalOrg(ctx, st); err != nil {
		logger.Fatal("could not prepare the local organisation", zap.Error(err))
	}

	obj, err := objects.NewFS(filepath.Join(dir, "objects"))
	if err != nil {
		logger.Fatal("object store init failed", zap.Error(err))
	}

	cfg := desktopConfig()
	broker := queue.NewLocal(st, logger)

	// A bundled tesseract means a scanned document works on a machine where the
	// user never installed anything. Falling back to PATH keeps `go run` and the
	// served deployment working unchanged.
	tesseract := stripExtendedPrefix(*tessBin)
	if tesseract != "" {
		if _, err := os.Stat(tesseract); err != nil {
			logger.Warn("bundled tesseract not found; falling back to PATH",
				zap.String("path", tesseract), zap.Error(err))
			tesseract = ""
		} else {
			// Point the engine at its own language data. Without this the
			// bundled binary falls back to the path compiled in on whatever
			// machine built it — which on a user's computer does not exist, and
			// which fails only on the one document that actually needed OCR.
			//
			// Tesseract 5 wants the tessdata directory itself here, not its
			// parent.
			tessdata := filepath.Join(filepath.Dir(tesseract), "tessdata")
			if _, err := os.Stat(tessdata); err == nil {
				if err := os.Setenv("TESSDATA_PREFIX", tessdata); err != nil {
					logger.Warn("could not set TESSDATA_PREFIX", zap.Error(err))
				}
			} else {
				logger.Warn("bundled tesseract has no tessdata directory beside it",
					zap.String("expected", tessdata))
			}
			logger.Info("using bundled tesseract", zap.String("path", tesseract))
		}
	}

	registry := workerpkg.DefaultHandlers()
	pl := pipeline.New(st, obj, logger, pipeline.Config{
		OCRTimeout:   cfg.WorkerOCRTimeout,
		OCRLanguages: cfg.OCRLanguages,
		TesseractBin: tesseract,
	})
	pl.Register(registry)

	loop := &workerpkg.Loop{
		Logger:       logger,
		Store:        st,
		Broker:       broker,
		Registry:     registry,
		Config:       cfg,
		OnDeadLetter: pl.MarkDeadLettered,
	}

	server := httpapi.NewServer(httpapi.Config{
		Objects:        obj,
		MaxUploadBytes: cfg.MaxUploadBytes,
		// Every request runs as the local principal. Safe here and nowhere else
		// — see the package comment.
		AuthEnabled:   false,
		SecureCookies: false,
	}, logger, st, nil)

	handler := server.Handler()
	if *webDir != "" {
		handler = withStaticFiles(handler, *webDir)
		logger.Info("serving frontend", zap.String("dir", *webDir))
	}

	// Bind before announcing, so the port in the log is the one actually in use
	// — with :0 the kernel chooses it, and a wrapper needs to read it back.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		logger.Fatal("listen failed", zap.String("addr", *addr), zap.Error(err))
	}
	if err := requireLoopback(ln.Addr()); err != nil {
		logger.Fatal("unsafe listen address", zap.Error(err))
	}

	// Authentication is off, so loopback plus this guard is the whole access
	// control — see guard.go for what loopback alone let through.
	token, err := newLaunchToken()
	if err != nil {
		logger.Fatal("could not generate launch token", zap.Error(err))
	}
	handler = withLaunchGuard(handler, allowedHosts(ln.Addr()), token)

	// The token goes to stdout only, which is the process that started us, and
	// never into the log.
	logger.Info("taskflow desktop ready", zap.String("url", "http://"+ln.Addr().String()))
	fmt.Printf("TaskFlow Compliance is running at http://%s/?%s=%s\n", ln.Addr().String(), launchParam, token)

	httpServer := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		if err := httpServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server stopped", zap.Error(err))
			cancel()
		}
	}()

	// The same background sweeps the served worker runs. Without them a desktop
	// install kept the original of every dead-lettered document forever - the
	// documents nobody revisits - and never rescued a task left 'processing' by a
	// close mid-scan, which is the crash the local queue expects the reconciler to
	// clean up. Both sweep once at startup, because this process often does not
	// live long enough to reach a tick.
	go maintenance.RunReconciler(ctx, logger, st, broker, cfg, pl.MarkDeadLettered)
	go maintenance.RunRawRetention(ctx, logger, st, pl, cfg)

	go loop.Run(ctx)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-stop:
		logger.Info("shutdown signal received")
	case <-ctx.Done():
	}
	cancel()

	shutdownCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()
	_ = httpServer.Shutdown(shutdownCtx)
	logger.Info("taskflow desktop stopped")
}

// desktopConfig is the served defaults with the distributed pieces removed.
func desktopConfig() *config.Config {
	cfg := config.Load()
	cfg.AuthEnabled = false
	cfg.SecureCookies = false
	// One user, one document at a time: concurrency here would only contend for
	// SQLite's single writer.
	cfg.WorkerConcurrency = 1
	// The local broker returns immediately when work exists, so this is only the
	// idle poll interval — short enough that a dropped wake-up is imperceptible.
	cfg.WorkerPollTimeout = 500 * time.Millisecond
	// Portuguese first: this is a Brazilian compliance tool, and without the
	// Portuguese model the engine mangles every accented word. English stays
	// behind it for mixed-language documents. An explicit OCR_LANGS still wins.
	if os.Getenv("OCR_LANGS") == "" {
		cfg.OCRLanguages = "por+eng"
	}
	return cfg
}

func ensureLocalOrg(ctx context.Context, st *store.Store) error {
	_, err := st.GetOrganization(ctx, auth.LocalOrgID)
	if err == nil {
		return nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	_, err = st.CreateOrganization(ctx, auth.LocalOrgID, "local")
	return err
}

// resolveDataDir picks the per-user location the application owns.
func resolveDataDir(override string) (string, error) {
	dir := override
	if dir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(base, "TaskFlow")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// withStaticFiles serves the built SPA for everything the API does not claim.
// Unknown paths fall back to index.html so a deep link survives a reload — the
// same rule nginx applies in the served deployment.
func withStaticFiles(api http.Handler, dir string) http.Handler {
	files := http.FileServer(http.Dir(dir))
	index := filepath.Join(dir, "index.html")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Path) >= 5 && r.URL.Path[:5] == "/api/" || r.URL.Path == "/metrics" {
			api.ServeHTTP(w, r)
			return
		}
		if _, err := os.Stat(filepath.Join(dir, filepath.Clean(r.URL.Path))); err != nil {
			http.ServeFile(w, r, index)
			return
		}
		files.ServeHTTP(w, r)
	})
}

// stripExtendedPrefix removes Windows' `\\?\` extended-length prefix.
//
// Tauri resolves bundled resource paths through Rust's canonicalize, which
// returns them in that form. Go handles it fine; tesseract — a C program using
// plain fopen — does not, and fails with an "Error opening data file" naming a
// path that looks perfectly correct. Normalising at this boundary is the point:
// it is where a path stops being ours and becomes an external tool's.
func stripExtendedPrefix(p string) string {
	return strings.TrimPrefix(p, `\\?\`)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
