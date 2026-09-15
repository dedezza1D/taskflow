package objects

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func newFS(t *testing.T) *FS {
	t.Helper()
	f, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	return f
}

func TestPutGetRoundtrip(t *testing.T) {
	f := newFS(t)
	ctx := context.Background()

	uri, err := f.Put(ctx, "documents/abc/original", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !strings.HasPrefix(uri, Scheme) {
		t.Fatalf("uri should carry scheme: %s", uri)
	}

	rc, err := f.Get(ctx, uri)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	if string(data) != "hello" {
		t.Fatalf("roundtrip mismatch: %q", data)
	}
}

func TestPutIsAtomicOverwrite(t *testing.T) {
	f := newFS(t)
	ctx := context.Background()

	if _, err := f.Put(ctx, "k/v", bytes.NewReader([]byte("v1"))); err != nil {
		t.Fatal(err)
	}
	uri, err := f.Put(ctx, "k/v", bytes.NewReader([]byte("v2-longer")))
	if err != nil {
		t.Fatal(err)
	}
	rc, err := f.Get(ctx, uri)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	// A reader sees either the old complete object or the new complete object —
	// after Put returns, it must be the new one, whole.
	if string(data) != "v2-longer" {
		t.Fatalf("expected complete new object, got %q", data)
	}
}

func TestGetMissingIsErrNotFound(t *testing.T) {
	f := newFS(t)
	if _, err := f.Get(context.Background(), Scheme+"nope/none"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestRemoveIsIdempotent(t *testing.T) {
	f := newFS(t)
	ctx := context.Background()
	uri, _ := f.Put(ctx, "a/b", strings.NewReader("x"))
	if err := f.Remove(ctx, uri); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := f.Remove(ctx, uri); err != nil {
		t.Fatalf("second Remove must be a no-op, got %v", err)
	}
}

func TestRemovePrefixErasesEverything(t *testing.T) {
	f := newFS(t)
	ctx := context.Background()

	u1, _ := f.Put(ctx, "documents/d1/original", strings.NewReader("o"))
	u2, _ := f.Put(ctx, "documents/d1/report.json", strings.NewReader("r"))
	u3, _ := f.Put(ctx, "documents/d2/original", strings.NewReader("keep"))

	if err := f.RemovePrefix(ctx, "documents/d1"); err != nil {
		t.Fatalf("RemovePrefix: %v", err)
	}
	for _, gone := range []string{u1, u2} {
		if _, err := f.Get(ctx, gone); !errors.Is(err, ErrNotFound) {
			t.Fatalf("expected %s erased, got %v", gone, err)
		}
	}
	if rc, err := f.Get(ctx, u3); err != nil {
		t.Fatalf("other document must survive: %v", err)
	} else {
		rc.Close()
	}
}

func TestInvalidKeysRejected(t *testing.T) {
	f := newFS(t)
	ctx := context.Background()
	for _, key := range []string{"", "/abs", "../escape", "a/../b", "a//b", `a\b`} {
		if _, err := f.Put(ctx, key, strings.NewReader("x")); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("key %q should be rejected, got %v", key, err)
		}
	}
	if _, err := f.Get(ctx, "no-scheme"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("uri without scheme should be rejected, got %v", err)
	}
}
