package ports

import "context"

// Rows is a multi-row query result.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close()
}

// Row is a single result row.
type Row interface {
	Scan(dest ...any) error
}

// CommandTag reports affected rows.
type CommandTag interface {
	RowsAffected() int64
}

// Queryer is implemented by both *pgxpool.Pool and pgx.Tx.
type Queryer interface {
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) Row
	Exec(ctx context.Context, sql string, args ...any) (CommandTag, error)
}

// Transactor runs fn in a transaction.
type Transactor interface {
	WithTx(ctx context.Context, fn func(tx Queryer) error) error
}
