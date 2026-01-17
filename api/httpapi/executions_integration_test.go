package httpapi

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/dedezza1D/taskflow/internal/store"
	"go.uber.org/zap"
)

func TestExecutionsAPI_EmptyList(t *testing.T) {
	dsn := "postgres://taskflow:taskflow@localhost:5432/taskflow?sslmode=disable"
	st, err := store.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()

	created, err := st.CreateTask(context.Background(), store.CreateTaskParams{
		Type:     "demo",
		Payload:  []byte(`{"x":1}`),
		Priority: store.PriorityNormal,
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	logger := zap.NewNop()
	srv := NewServer(Config{Port: "0"}, logger, st, nil)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() { _ = srv.httpServer.Serve(ln) }()

	client := &http.Client{Timeout: 3 * time.Second}
	url := fmt.Sprintf("http://%s/api/v1/tasks/%s/executions?limit=10", ln.Addr().String(), created.ID.String())

	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET executions: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 got %d", resp.StatusCode)
	}
}
