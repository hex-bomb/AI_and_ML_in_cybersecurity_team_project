package migrations

import (
	"embed"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	_ "github.com/jackc/pgx/v5/stdlib"

	"database/sql"

	// Нормализация DSN общая с пулом (postgres.NormalizeDSN): дубль удалён.
	pginfra "github.com/kbsearch/kb/internal/store/pg"
)

//go:embed sql/*.sql
var files embed.FS

// Up applies pending migrations; a second call on a current DB is a no-op.
func Up(databaseURL string) error {
	db, err := sql.Open("pgx", pginfra.NormalizeDSN(databaseURL))
	if err != nil {
		return fmt.Errorf("подключение к БД: %w", err)
	}
	defer func() { _ = db.Close() }()

	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		return fmt.Errorf("драйвер миграций: %w", err)
	}

	src, err := iofs.New(files, "sql")
	if err != nil {
		return fmt.Errorf("источник миграций: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", src, "postgres", driver)
	if err != nil {
		return fmt.Errorf("сборка мигратора: %w", err)
	}

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("применение миграций: %w", err)
	}
	return nil
}

// Version returns the current schema version for diagnostics.
func Version(databaseURL string) (uint, error) {
	db, err := sql.Open("pgx", pginfra.NormalizeDSN(databaseURL))
	if err != nil {
		return 0, fmt.Errorf("подключение к БД: %w", err)
	}
	defer func() { _ = db.Close() }()

	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		return 0, fmt.Errorf("драйвер миграций: %w", err)
	}
	src, err := iofs.New(files, "sql")
	if err != nil {
		return 0, fmt.Errorf("источник миграций: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "postgres", driver)
	if err != nil {
		return 0, fmt.Errorf("сборка мигратора: %w", err)
	}
	version, dirty, err := m.Version()
	if err != nil {
		return 0, fmt.Errorf("версия схемы: %w", err)
	}
	if dirty {
		return version, errors.New("схема в состоянии dirty: миграция упала посередине")
	}
	return version, nil
}
