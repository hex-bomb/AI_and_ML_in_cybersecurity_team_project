package postgres

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	domain "github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
)

// Тесты пула, транзакций и адаптера строк.

// TestWithTxCommitsOnNilError — базовое поведение ports.Transactor.
func TestWithTxCommitsOnNilError(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedTenant(t, ctx)

	err := testDB.WithTx(ctx, func(tx ports.Queryer) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ($1, 'second')`, otherTenant)
		return err
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}

	var count int
	if err := testDB.QueryRow(ctx,
		"SELECT count(*) FROM tenants WHERE id = $1", otherTenant).Scan(&count); err != nil {
		t.Fatalf("проверка коммита: %v", err)
	}
	if count != 1 {
		t.Fatalf("транзакция не закоммичена: найдено %d строк", count)
	}
}

// TestWithTxRollsBackOnError — откат при возврате ошибки из fn.
func TestWithTxRollsBackOnError(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedTenant(t, ctx)

	sentinel := errors.New("сбой в середине")
	err := testDB.WithTx(ctx, func(tx ports.Queryer) error {
		if _, err := tx.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ($1, 'second')`, otherTenant); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("WithTx вернул %v, ожидалась исходная ошибка", err)
	}

	var count int
	if err := testDB.QueryRow(ctx,
		"SELECT count(*) FROM tenants WHERE id = $1", otherTenant).Scan(&count); err != nil {
		t.Fatalf("проверка отката: %v", err)
	}
	if count != 0 {
		t.Fatalf("транзакция не откатилась: найдено %d строк", count)
	}
}

// TestWithTxRollsBackOnPanic — откат при панике.
// Требование домена: WithTx откатывает и при панике. Без отложенного
func TestWithTxRollsBackOnPanic(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedTenant(t, ctx)

	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Fatal("паника не была восстановлена — тест некорректен")
			}
		}()
		_ = testDB.WithTx(ctx, func(tx ports.Queryer) error {
			if _, err := tx.Exec(ctx,
				`INSERT INTO tenants (id, name) VALUES ($1, 'second')`, otherTenant); err != nil {
				return err
			}
			panic("сбой после записи")
		})
	}()

	var count int
	if err := testDB.QueryRow(ctx,
		"SELECT count(*) FROM tenants WHERE id = $1", otherTenant).Scan(&count); err != nil {
		t.Fatalf("проверка отката после паники: %v", err)
	}
	if count != 0 {
		t.Fatalf("после паники транзакция не откатилась: найдено %d строк", count)
	}
}

// TestPoolReleasesConnectionsAfterQueries — проверка без Python-теста.
// Утечка соединений проявилась бы не сразу, а под нагрузкой: закрытые Rows
func TestPoolReleasesConnectionsAfterQueries(t *testing.T) {
	if testing.Short() {
		t.Skip("интеграционный тест: нужен PostgreSQL")
	}
	shortCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	db, err := New(shortCtx, Config{URL: testDSN(), MaxConns: 2})
	if err != nil {
		t.Skipf("PostgreSQL недоступен: %v", err)
	}
	defer db.Close()

	for i := 0; i < 25; i++ {
		rows, err := db.Query(shortCtx, "SELECT generate_series(1, 10)")
		if err != nil {
			t.Fatalf("запрос %d: %v", i, err)
		}
		if err := collect(rows, func() error { return nil }); err != nil {
			t.Fatalf("обход %d: %v", i, err)
		}
		var value int
		if err := db.QueryRow(shortCtx, "SELECT 1").Scan(&value); err != nil {
			t.Fatalf("QueryRow %d: %v", i, err)
		}
	}
}

// TestQueryerPropagatesContextCancellation — проверка без Python-теста.
// Контекст пробрасывается везде: отмена запроса клиентом обязана отменять и
func TestQueryerPropagatesContextCancellation(t *testing.T) {
	requireDB(t)
	base := testContext(t)
	seedChain(t, base, seedOptions{})

	cancelCtx, cancel := context.WithCancel(base)
	cancel()
	// Отменённый контекент не должен превращаться в 503: это отмена работы
	// клиентом, а не недоступность сервиса.
	if _, err := testDB.Exec(cancelCtx, "SELECT 1"); err == nil {
		t.Fatal("запрос с отменённым контекстом выполнился")
	} else if errors.Is(err, domain.ErrServiceUnavailable) {
		t.Fatalf("отмена контекста превратилась в ServiceUnavailable: %v", err)
	}
}

// TestNewResolvesDSNFromEnvironment — порт app/infrastructure/db.py:33-35.
// Пустой DSN берётся из DATABASE_URL, и SQLAlchemy-приписка asyncpg
func TestNewResolvesDSNFromEnvironment(t *testing.T) {
	if testing.Short() {
		t.Skip("интеграционный тест: нужен PostgreSQL")
	}
	t.Setenv(envDatabaseURL, "postgresql+asyncpg://kb:dev@127.0.0.1:55432/kb_go_test?sslmode=disable")

	shortCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	db, err := New(shortCtx, Config{})
	if err != nil {
		t.Skipf("PostgreSQL недоступен: %v", err)
	}
	defer db.Close()

	if err := db.Ping(shortCtx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

// TestNewFailsWithoutDSN — проверка без Python-теста.
func TestNewFailsWithoutDSN(t *testing.T) {
	t.Setenv(envDatabaseURL, "")
	ctx := testContext(t)
	if _, err := New(ctx, Config{}); err == nil {
		t.Fatal("New без DATABASE_URL должен отказать")
	}
}

// TestNewFailsOnUnreachableDatabase — проверка без Python-теста.
// Сервис без базы обязан падать на старте, а не отвечать 503 на первый запрос.
func TestNewFailsOnUnreachableDatabase(t *testing.T) {
	if testing.Short() {
		t.Skip("интеграционный тест: нужен PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Порт 1 заведомо закрыт.
	_, err := New(ctx, Config{
		URL:            "postgres://kb:dev@127.0.0.1:1/kb_go_test?sslmode=disable",
		ConnectTimeout: 2 * time.Second,
	})
	if err == nil {
		t.Fatal("New на недоступную базу должен отказать")
	}
}

// TestNormalizeDSNKeepsPostgresScheme — проверка без Python-теста.
func TestNormalizeDSNKeepsPostgresScheme(t *testing.T) {
	cases := map[string]string{
		"postgres://kb:dev@localhost:5432/kb":            "postgres://kb:dev@localhost:5432/kb",
		"postgresql+asyncpg://kb:dev@localhost:55432/kb": "postgres://kb:dev@localhost:55432/kb",
		"postgresql+psycopg://kb:dev@localhost:55432/kb": "postgres://kb:dev@localhost:55432/kb",
		"postgresql://kb:dev@localhost:5432/kb":          "postgresql://kb:dev@localhost:5432/kb",
	}
	for input, want := range cases {
		if got := NormalizeDSN(input); got != want {
			t.Errorf("NormalizeDSN(%q) = %q, ожидалось %q", input, got, want)
		}
	}
}

// TestTestDatabaseURLEnvOverride — проверка без Python-теста.
// Адрес тестовой базы переопределяется переменной окружения: иначе тесты
func TestTestDatabaseURLEnvOverride(t *testing.T) {
	t.Setenv("KB_GO_TEST_DATABASE_URL", "postgres://example/db")
	if got := testDSN(); got != "postgres://example/db" {
		t.Errorf("testDSN() = %q, ожидался адрес из переменной", got)
	}
	if err := os.Unsetenv("KB_GO_TEST_DATABASE_URL"); err != nil {
		t.Fatalf("снятие переменной: %v", err)
	}
	if got := testDSN(); got != defaultTestDSN {
		t.Errorf("testDSN() = %q, ожидался %q", got, defaultTestDSN)
	}
}
