package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestTasksAPI_CreateThenGet(t *testing.T) {
	st := mustStore(t)
	defer st.Close()

	logger := zap.NewNop()
	srv := NewServer(Config{Port: "0"}, logger, st, nil)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		_ = srv.httpServer.Serve(ln)
	}()

	baseURL := fmt.Sprintf("http://%s", ln.Addr().String())
	client := &http.Client{Timeout: 3 * time.Second}

	// ---- Create ----
	createBody := []byte(`{"type":"demo","payload":{"hello":"world"},"priority":"normal"}`)
	resp, err := client.Post(baseURL+"/api/v1/tasks", "application/json", bytes.NewReader(createBody))
	if err != nil {
		t.Fatalf("POST /tasks: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d body=%s", resp.StatusCode, string(b))
	}

	var created struct {
		Task struct {
			ID string `json:"id"`
		} `json:"task"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.Task.ID == "" {
		t.Fatalf("expected non-empty task.id")
	}

	// ---- Get ----
	getResp, err := client.Get(baseURL + "/api/v1/tasks/" + created.Task.ID)
	if err != nil {
		t.Fatalf("GET /tasks/{id}: %v", err)
	}
	defer getResp.Body.Close()

	if getResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(getResp.Body)
		t.Fatalf("expected 200, got %d body=%s", getResp.StatusCode, string(b))
	}

	var got struct {
		Task struct {
			ID       string          `json:"id"`
			Type     string          `json:"type"`
			Status   string          `json:"status"`
			Priority string          `json:"priority"`
			Payload  json.RawMessage `json:"payload"`
			Version  int             `json:"version"`
		} `json:"task"`
	}
	if err := json.NewDecoder(getResp.Body).Decode(&got); err != nil {
		t.Fatalf("decode get response: %v", err)
	}

	if got.Task.ID != created.Task.ID {
		t.Fatalf("expected same id %s got %s", created.Task.ID, got.Task.ID)
	}
	if got.Task.Type != "demo" {
		t.Fatalf("expected type demo got %q", got.Task.Type)
	}
	if got.Task.Status != "queued" {
		t.Fatalf("expected status queued got %q", got.Task.Status)
	}
	if got.Task.Priority != "normal" {
		t.Fatalf("expected priority normal got %q", got.Task.Priority)
	}
	if got.Task.Version != 1 {
		t.Fatalf("expected version 1 got %d", got.Task.Version)
	}

	// semantic payload check
	var want map[string]any
	var gotPayload map[string]any
	_ = json.Unmarshal([]byte(`{"hello":"world"}`), &want)
	if err := json.Unmarshal(got.Task.Payload, &gotPayload); err != nil {
		t.Fatalf("unmarshal got payload: %v", err)
	}
	if fmt.Sprint(want["hello"]) != fmt.Sprint(gotPayload["hello"]) {
		t.Fatalf("payload mismatch want=%v got=%v", want, gotPayload)
	}
}
