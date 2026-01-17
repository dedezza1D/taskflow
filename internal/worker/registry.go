package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dedezza1D/taskflow/internal/store"
)

type Handler func(ctx context.Context, task *store.Task) error

type Registry struct {
	handlers map[string]Handler
}

func NewRegistry() *Registry {
	return &Registry{handlers: map[string]Handler{}}
}

func (r *Registry) Register(taskType string, h Handler) {
	r.handlers[taskType] = h
}

func (r *Registry) Get(taskType string) (Handler, bool) {
	h, ok := r.handlers[taskType]
	return h, ok
}

// PermanentError marks an error that should NOT be retried.
type PermanentError struct{ Err error }

func (e PermanentError) Error() string { return e.Err.Error() }
func (e PermanentError) Unwrap() error { return e.Err }

func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return PermanentError{Err: err}
}

func IsPermanent(err error) bool {
	var pe PermanentError
	return errors.As(err, &pe)
}

// DefaultHandlers registers a couple example handlers.
// You’ll replace/extend these later.
func DefaultHandlers() *Registry {
	r := NewRegistry()

	// demo: succeed after 200ms
	r.Register("demo", func(ctx context.Context, task *store.Task) error {
		time.Sleep(200 * time.Millisecond)
		return nil
	})

	// fail: permanent failure (never retry)
	r.Register("fail", func(ctx context.Context, task *store.Task) error {
		return Permanent(fmt.Errorf("requested failure for demo"))
	})

	return r
}
