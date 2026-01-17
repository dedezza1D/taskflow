package httpapi

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/dedezza1D/taskflow/internal/store"
	"go.uber.org/zap"
)

func TestHealthEndpoint_Integration(t *testing.T) {
	logger := zap.NewNop()
	var st *store.Store = nil

	s := NewServer(Config{Port: "0"}, logger, st, nil)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		_ = s.httpServer.Serve(ln)
	}()

	client := &http.Client{Timeout: 2 * time.Second}
	url := fmt.Sprintf("http://%s/api/v1/health", ln.Addr().String())

	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET /health failed: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if string(body) != "ok" {
		t.Fatalf("expected body 'ok', got %q", string(body))
	}
}
