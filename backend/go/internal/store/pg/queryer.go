package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kbsearch/kb/internal/ports"
)

// pgxQuerier — общий минимум *pgxpool.Pool и pgx.Tx. Наружу отдаётся ports.Queryer.
type pgxQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// queryer адаптирует pgx-типы к ports.Queryer и заворачивает ошибки в одной точке.
type queryer struct {
	inner pgxQuerier
}

// newQueryer оборачивает пул или транзакцию в ports.Queryer.
func newQueryer(inner pgxQuerier) ports.Queryer {
	return queryer{inner: inner}
}

func (q queryer) Query(ctx context.Context, sql string, args ...any) (ports.Rows, error) {
	raw, err := q.inner.Query(ctx, sql, args...)
	if err != nil {
		return nil, wrapDBError(err)
	}
	return rows{inner: raw}, nil
}

func (q queryer) QueryRow(ctx context.Context, sql string, args ...any) ports.Row {
	return singleRow{inner: q.inner.QueryRow(ctx, sql, args...)}
}

func (q queryer) Exec(ctx context.Context, sql string, args ...any) (ports.CommandTag, error) {
	tag, err := q.inner.Exec(ctx, sql, args...)
	if err != nil {
		return tag, wrapDBError(err)
	}
	return tag, nil
}

// rows оборачивает pgx.Rows под ports.Rows.
type rows struct {
	inner pgx.Rows
}

func (r rows) Next() bool { return r.inner.Next() }

func (r rows) Scan(dest ...any) error {
	if err := r.inner.Scan(dest...); err != nil {
		return wrapDBError(err)
	}
	return nil
}

func (r rows) Err() error {
	if err := r.inner.Err(); err != nil {
		return wrapDBError(err)
	}
	return nil
}

func (r rows) Close() { r.inner.Close() }

// singleRow оборачивает pgx.Row ради единой точки оборачивания ошибок.
type singleRow struct {
	inner pgx.Row
}

func (r singleRow) Scan(dest ...any) error {
	if err := r.inner.Scan(dest...); err != nil {
		return wrapDBError(err)
	}
	return nil
}

// collect обходит строки целиком (Close всегда, Err после прохода).
func collect(r ports.Rows, fn func() error) error {
	defer r.Close()
	for r.Next() {
		if err := fn(); err != nil {
			return err
		}
	}
	return r.Err()
}
