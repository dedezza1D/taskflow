package store

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

func TestCreateAndGetTask(t *testing.T) {
	dsn := "postgres://taskflow:taskflow@localhost:5432/taskflow?sslmode=disable"

	st, err := New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("New store: %v", err)
	}
	defer st.Close()

	created, err := st.CreateTask(context.Background(), CreateTaskParams{
		Type:     "demo",
		Payload:  []byte(`{"hello":"world"}`),
		Priority: PriorityNormal,
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	got, err := st.GetTask(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}

	if got.ID != created.ID {
		t.Fatalf("expected id %s got %s", created.ID, got.ID)
	}
	if got.Type != "demo" {
		t.Fatalf("expected type %q got %q", "demo", got.Type)
	}
	if got.Priority != PriorityNormal {
		t.Fatalf("expected priority %q got %q", PriorityNormal, got.Priority)
	}
	if got.Status != StatusQueued {
		t.Fatalf("expected status %q got %q", StatusQueued, got.Status)
	}
	if got.Version != 1 {
		t.Fatalf("expected version 1 got %d", got.Version)
	}

	var want map[string]any
	var gotPayload map[string]any

	if err := json.Unmarshal([]byte(`{"hello":"world"}`), &want); err != nil {
		t.Fatalf("unmarshal want: %v", err)
	}
	if err := json.Unmarshal(got.Payload, &gotPayload); err != nil {
		t.Fatalf("unmarshal got: %v", err)
	}
	if !reflect.DeepEqual(want, gotPayload) {
		t.Fatalf("payload mismatch: want=%v got=%v", want, gotPayload)
	}
}
