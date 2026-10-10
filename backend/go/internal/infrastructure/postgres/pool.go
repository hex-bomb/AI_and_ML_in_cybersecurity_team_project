package postgres

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kbsearch/backend-go/internal/application/ports"
)

// Значения пула по умолчанию под короткие запросы MVP.
const (
	defaultMaxConns          int32 = 16
	defaultConnectTimeout          = 5 * time.Second
	defaultMaxConnLifetime         = time.Hour
	defaultMaxConnIdleTime         = 30 * time.Minute
	defaultHealthCheckPeriod       = time.Minute
	// envDatabaseURL — переменная окружения с DSN (общая с compose/.env).
	envDatabaseURL = "DATABASE_URL"
)

// Config — параметры подключения к PostgreSQL.
type Config struct {
	// URL — DSN (пусто — из переменной DATABASE_URL).
	URL string

	MaxConns          int32
	MinConns          int32
	MaxConnLifetime   time.Duration
	MaxConnIdleTime   time.Duration
	ConnectTimeout    time.Duration
	HealthCheckPeriod time.Duration
}

// DB — пул соединений: одновременно ports.Queryer и ports.Transactor.
type DB struct {
	pool *pgxpool.Pool
}

// Проверка соответствия портам на этапе компиляции.
var (
	_ ports.Queryer    = (*DB)(nil)
	_ ports.Transactor = (*DB)(nil)
)

// New создаёт пул и проверяет связность пингом (падение на старте, не на запросе).
func New(ctx context.Context, cfg Config) (*DB, error) {
	dsn, err := resolveDSN(cfg.URL)
	if err != nil {
		return nil, err
	}

	poolCfg, err := pgxpool.ParseConfig(NormalizeDSN(dsn))
	if err != nil {
		return nil, fmt.Errorf("разбор DSN PostgreSQL: %w", err)
	}
	applyDefaults(poolCfg, cfg)
	// Часовой пояс сессии UTC ради воспроизводимости курсоров и логов.
	if poolCfg.ConnConfig.RuntimeParams == nil {
		poolCfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	poolCfg.ConnConfig.RuntimeParams["timezone"] = "UTC"

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("создание пула PostgreSQL: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, wrapDBError(err)
	}
	return &DB{pool: pool}, nil
}

// applyDefaults заполняет незаданные поля (ноль — «по умолчанию»).
func applyDefaults(cfg *pgxpool.Config, in Config) {
	if in.MaxConns > 0 {
		cfg.MaxConns = in.MaxConns
	} else if cfg.MaxConns == 0 {
		cfg.MaxConns = defaultMaxConns
	}
	if in.MinConns > 0 {
		cfg.MinConns = in.MinConns
	}
	if in.MaxConnLifetime > 0 {
		cfg.MaxConnLifetime = in.MaxConnLifetime
	} else if cfg.MaxConnLifetime == 0 {
		cfg.MaxConnLifetime = defaultMaxConnLifetime
	}
	if in.MaxConnIdleTime > 0 {
		cfg.MaxConnIdleTime = in.MaxConnIdleTime
	} else if cfg.MaxConnIdleTime == 0 {
		cfg.MaxConnIdleTime = defaultMaxConnIdleTime
	}
	if in.ConnectTimeout > 0 {
		cfg.ConnConfig.ConnectTimeout = in.ConnectTimeout
	} else if cfg.ConnConfig.ConnectTimeout == 0 {
		cfg.ConnConfig.ConnectTimeout = defaultConnectTimeout
	}
	if in.HealthCheckPeriod > 0 {
		cfg.HealthCheckPeriod = in.HealthCheckPeriod
	} else if cfg.HealthCheckPeriod == 0 {
		cfg.HealthCheckPeriod = defaultHealthCheckPeriod
	}
	if cfg.MaxConnIdleTime > 0 && cfg.MaxConnIdleTime < cfg.HealthCheckPeriod {
		cfg.HealthCheckPeriod = cfg.MaxConnIdleTime
	}
}

// resolveDSN берёт адрес из аргумента или из окружения.
func resolveDSN(url string) (string, error) {
	if url != "" {
		return url, nil
	}
	fromEnv := strings.TrimSpace(os.Getenv(envDatabaseURL))
	if fromEnv == "" {
		return "", fmt.Errorf("не задан DSN PostgreSQL: ни аргумент, ни переменная %s", envDatabaseURL)
	}
	return fromEnv, nil
}

// NormalizeDSN отбрасывает драйверную приписку схемы ради pgx (общий .env).
// Экспорт ради migrations: второй экземпляр той же таблицы жил там же.
func NormalizeDSN(dsn string) string {
	for _, scheme := range []string{"postgresql+asyncpg://", "postgresql+psycopg://"} {
		if rest, ok := strings.CutPrefix(dsn, scheme); ok {
			return "postgres://" + rest
		}
	}
	return dsn
}

// Ping проверяет связность с базой. Используется в health/ready (ТЗ 10.1).
func (db *DB) Ping(ctx context.Context) error {
	if err := db.pool.Ping(ctx); err != nil {
		return wrapDBError(err)
	}
	return nil
}

// Close закрывает пул. Вызывается один раз при остановке процесса.
func (db *DB) Close() {
	db.pool.Close()
}

// Query, QueryRow и Exec — доступ без транзакции (пул и есть Queryer).
func (db *DB) Query(ctx context.Context, sql string, args ...any) (ports.Rows, error) {
	return newQueryer(db.pool).Query(ctx, sql, args...)
}

func (db *DB) QueryRow(ctx context.Context, sql string, args ...any) ports.Row {
	return newQueryer(db.pool).QueryRow(ctx, sql, args...)
}

func (db *DB) Exec(ctx context.Context, sql string, args ...any) (ports.CommandTag, error) {
	return newQueryer(db.pool).Exec(ctx, sql, args...)
}

// WithTx выполняет fn в транзакции: коммит при nil, иначе откат (и при панике).
func (db *DB) WithTx(ctx context.Context, fn func(tx ports.Queryer) error) (err error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return wrapDBError(err)
	}

	// Флаг committed вместо проверки err: при панике err ещё nil и откат пропал бы.
	// Откат идёт с неотменяемым контекстом: обязан дойти до сервера.
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()

	if err = fn(newQueryer(tx)); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		err = wrapDBError(err)
		return err
	}
	committed = true
	return nil
}

// Компиляционная проверка: пул и транзакция годятся как pgxQuerier.
var (
	pgxTx pgx.Tx

	_ pgxQuerier       = (*pgxpool.Pool)(nil)
	_ pgxQuerier       = pgxTx
	_ ports.CommandTag = pgconn.CommandTag{}
)
