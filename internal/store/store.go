package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the repository. It holds a DB rather than a concrete pool, so the
// same 42 methods serve the served deployment (Postgres) and the desktop build
// (a local file) without being written twice.
type Store struct {
	db DB
}

// New opens the Postgres-backed store.
func New(ctx context.Context, databaseURL string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}

	// sensible defaults
	cfg.MaxConns = 10
	cfg.MinConns = 1
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.MaxConnLifetime = 30 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}

	// verify connection
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}

	return &Store{db: &postgresDB{pool: pool}}, nil
}

// NewWithDB builds a store on an already-constructed driver — the entry point a
// non-Postgres backend uses.
func NewWithDB(db DB) *Store {
	return &Store{db: db}
}

func (s *Store) Close() {
	if s.db != nil {
		s.db.Close()
	}
}
