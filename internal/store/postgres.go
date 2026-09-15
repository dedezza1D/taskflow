package store

import (
	"context"
	"errors"

	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// postgresDB adapts pgxpool to the DB seam. It is deliberately thin: pgx.Rows
// and pgconn.CommandTag already satisfy Rows and Result, so only QueryRow needs
// wrapping — to translate pgx.ErrNoRows into the neutral sentinel.
type postgresDB struct {
	pool *pgxpool.Pool
}

type postgresRow struct {
	row pgx.Row
}

func (r postgresRow) Scan(dest ...any) error {
	return translate(r.row.Scan(dest...))
}

// translate maps pgx's error vocabulary onto the neutral sentinels, keeping the
// original wrapped so a caller that wants the detail can still reach it.
func translate(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoRows
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return fmt.Errorf("%w: %w", ErrUniqueViolation, err)
	}
	return err
}

func (d *postgresDB) QueryRow(ctx context.Context, sql string, args ...any) Row {
	return postgresRow{row: d.pool.QueryRow(ctx, sql, args...)}
}

func (d *postgresDB) Query(ctx context.Context, sql string, args ...any) (Rows, error) {
	rows, err := d.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, translate(err)
	}
	return rows, nil
}

func (d *postgresDB) Exec(ctx context.Context, sql string, args ...any) (Result, error) {
	tag, err := d.pool.Exec(ctx, sql, args...)
	if err != nil {
		return nil, translate(err)
	}
	return tag, nil
}

func (d *postgresDB) Close() {
	if d.pool != nil {
		d.pool.Close()
	}
}
