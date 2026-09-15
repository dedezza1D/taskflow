// Package objects is the object-storage boundary for document bytes.
//
// C1 by construction: document content lives HERE and only here. Task payloads,
// queue messages, and DLQ entries carry a storage URI (a reference), never bytes.
//
// The v1 implementation is a filesystem store (FS) with one property the
// pipeline's checkpoint invariant leans on: Put is ATOMIC — the object becomes
// visible under its key all-at-once (temp file + fsync + rename), so "object
// exists" always means "object complete". An S3/MinIO implementation slots in
// behind the same interface (single-part S3 PUTs are likewise atomic); that swap
// is the C5 upgrade path (bucket lifecycle rules for retention).
package objects

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Scheme prefixes every URI produced by the FS store.
const Scheme = "fs://"

var (
	ErrNotFound   = errors.New("objects: not found")
	ErrInvalidKey = errors.New("objects: invalid key")
)

// Store is the minimal surface the pipeline and the erasure path need.
type Store interface {
	// Put writes the object atomically under key and returns its URI. A reader
	// error or a crash mid-write leaves no partial object visible.
	Put(ctx context.Context, key string, r io.Reader) (string, error)
	// Get opens the object identified by uri. Returns ErrNotFound if absent.
	Get(ctx context.Context, uri string) (io.ReadCloser, error)
	// Remove deletes a single object. Removing an absent object is not an error
	// (erasure must be idempotent).
	Remove(ctx context.Context, uri string) error
	// RemovePrefix deletes every object under a key prefix — the erasure
	// primitive: all of a document's bytes live under documents/{id}/.
	RemovePrefix(ctx context.Context, prefix string) error
}

// FS is a local-filesystem Store rooted at a directory. In docker-compose the
// API and worker share this root via a volume.
type FS struct {
	root string
}

func NewFS(root string) (*FS, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("objects: root dir is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	return &FS{root: abs}, nil
}

// validKey rejects anything that could escape the root: absolute paths, empty
// segments, and dot-segments. Keys are always program-constructed
// (documents/{uuid}/...), so this is defense in depth, not an input format.
func validKey(key string) error {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "\\") {
		return ErrInvalidKey
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return ErrInvalidKey
		}
	}
	return nil
}

func (f *FS) pathFor(key string) (string, error) {
	if err := validKey(key); err != nil {
		return "", err
	}
	return filepath.Join(f.root, filepath.FromSlash(key)), nil
}

func keyFromURI(uri string) (string, error) {
	if !strings.HasPrefix(uri, Scheme) {
		return "", fmt.Errorf("%w: uri %q", ErrInvalidKey, uri)
	}
	key := strings.TrimPrefix(uri, Scheme)
	if err := validKey(key); err != nil {
		return "", err
	}
	return key, nil
}

// Put implements the atomic write: stream into a temp file in the SAME
// directory, fsync, then rename over the final name. Rename within a directory
// is atomic on POSIX filesystems, which is exactly the checkpoint invariant:
// a reader either sees no object or the complete object, never a partial one.
func (f *FS) Put(ctx context.Context, key string, r io.Reader) (string, error) {
	path, err := f.pathFor(key)
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	tmp, err := os.CreateTemp(dir, ".put-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}

	if _, err := io.Copy(tmp, r); err != nil {
		cleanup()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return "", err
	}
	if err := tmp.Chmod(0o644); err != nil {
		cleanup()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return "", err
	}
	return Scheme + key, nil
}

func (f *FS) Get(ctx context.Context, uri string) (io.ReadCloser, error) {
	key, err := keyFromURI(uri)
	if err != nil {
		return nil, err
	}
	path, err := f.pathFor(key)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return file, nil
}

func (f *FS) Remove(ctx context.Context, uri string) error {
	key, err := keyFromURI(uri)
	if err != nil {
		return err
	}
	path, err := f.pathFor(key)
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (f *FS) RemovePrefix(ctx context.Context, prefix string) error {
	path, err := f.pathFor(prefix)
	if err != nil {
		return err
	}
	return os.RemoveAll(path)
}
