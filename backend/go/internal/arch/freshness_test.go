package arch

// ВОРОТА «СВЕЖЕСТЬ ДАННЫХ» — §9: «у каждого коннектора есть метрика “последнее
// успешное обновление” и алерт на отставание», §10 (риск «тихое отставание
// коннекторов»): «метрика возраста последнего обновления, алерт, периодическая
// сверка числа документов с источником».
//
// ГЛАВНОЕ ПРО ЭТОТ ФАЙЛ: СВЕЖЕСТЬ СУЩЕСТВУЕТ ТОЛЬКО КАК SQL. Проверяется
// ровно то, что есть:
//
//	таблица connectors с колонками last_success_at, last_attempt_at,
//	stale_after_seconds, consecutive_failures и ограничениями на них;
//	представление connector_freshness, которое считает lag_seconds и is_stale.
//
// Чего в дереве НЕТ и что ворота не могут проверить:
//
//   - Go-репозитория коннекторов: слово connectors не встречается ни в одном
//     нетестовом .go файле модуля;
//   - вычислителя отставания в Go и метрики для мониторинга;
//   - порога алерта, кроме колонки на источник.
//
// Поэтому ворота состоят из трёх частей, и каждая говорит, что именно она
// проверяет:
//
//	TestСхемаКоннекторовНесётМетрикуСвежести   — контракт DDL, без базы;
//	TestПредикатОтставанияВедётСебяПоДанным      — НАСТОЯЩЕЕ исполнение
//	                                              представления на живом
//	                                              PostgreSQL (свежий, отсталый,
//	                                              ни разу не запускался);
//	TestСвежестьКоннекторовНеВыгружаетсяВGo     — маркер долга: метрика не
//	                                              доходит до мониторинга.
//
// Без живой базы средний тест ПРОПУСКАЕТСЯ. Это сказано в его сообщении, а не
// спрятано.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kbsearch/kb/internal/ports"
)

// gateMigration читает файл миграции из migrations/sql.
func gateMigration(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "migrations", "sql", name))
	if err != nil {
		t.Fatalf("чтение миграции %s: %v", name, err)
	}
	return string(raw)
}

// TestСхемаКоннекторовНесётМетрикуСвежести — контракт DDL §9.
//
// Проверяется наличие колонок метрики, ограничений на них и представления с
// предикатом отставания. Структурная проверка, а не поведенческая: запуск
// представления — это TestПредикатОтставанияВедётСебяПоДанным, и ему нужна база.
//
// Отдельно проверяется, что порог отставания берётся ИЗ СТРОКИ, а не из
// константы: у Slack и у файловой папки правдоподобная свежесть разная, и одна
// константа на всех заставила бы либо молчать об отказе Slack, либо поднимать
// тревогу на каждой папке.
func TestСхемаКоннекторовНесётМетрикуСвежести(t *testing.T) {
	text := gateMigration(t, "0002_lake_acl.up.sql")

	for _, column := range []string{
		"last_success_at",      // метрика §9 «последнее успешное обновление»
		"last_attempt_at",      // попытка была, успеха не было — разные вещи
		"last_error_code",      // машиночитаемая причина для разбора очереди
		"consecutive_failures", // подряд идущие отказы
		"stale_after_seconds",  // порог «отстал», задаётся на источник
		"last_success_items",   // сверка числа документов с источником (§10)
	} {
		if !strings.Contains(text, column) {
			t.Errorf("в таблице connectors нет колонки %s: метрика «последнее успешное "+
				"обновление» (§9) не выводится", column)
		}
	}

	// Ограничения: без них порог в ноль или отрицательные счётчики отказов
	// сделали бы предикат отставания бессмысленным.
	for _, constraint := range []string{
		"ck_connectors_stale_after",
		"ck_connectors_failures",
		"ck_connectors_items",
	} {
		if !strings.Contains(text, constraint) {
			t.Errorf("в таблице connectors нет ограничения %s", constraint)
		}
	}
	if !strings.Contains(text, "stale_after_seconds > 0") {
		t.Error("порог отставания не ограничен снизу: stale_after_seconds <= 0 сделал бы " +
			"любой коннектор вечно свежим")
	}

	// Представление метрики.
	if !strings.Contains(text, "CREATE VIEW connector_freshness") {
		t.Fatal("нет представления connector_freshness: метрика свежести нигде не вычисляется")
	}
	for _, element := range []string{
		"lag_seconds",             // величина отставания
		"is_stale",                // сам предикат
		"last_success_at IS NULL", // ни разу не запускался — тоже отставание
		"> c.stale_after_seconds", // порог из строки, а не константа
		"now()",                   // отставание считается от текущего момента
	} {
		if !strings.Contains(text, element) {
			t.Errorf("в представлении connector_freshness нет %q: метрика свежести "+
				"считается не от того", element)
		}
	}
}

// TestСвежестьКоннекторовНеВыгружаетсяВGo — маркер долга.
//
// ПРОХОДИТ СЕГОДНЯ И ДОЛЖЕН УПАСТЬ, когда появится вычислитель свежести,
// экспорт метрики или порог алерта. До этого момента §9 «у каждого коннектора
// есть метрика и алерт на отставание» выполнено наполовину: метрика лежит в
// представлении SQL, но ни один Go-код её не читает и в мониторинг не отдаёт.
//
// Обход ограничен каталогами модуля кроме internal/store/pg (там живёт сама
// миграция lake) и migrations: иначе нашлось бы её собственное упоминание.
func TestСвежестьКоннекторовНеВыгружаетсяВGo(t *testing.T) {
	readers := gateFilesMentioning(t,
		[]string{"internal", "cmd", "api", "eval"}, "connector_freshness")
	readers = append(readers, gateFilesMentioning(t,
		[]string{"internal", "cmd", "api", "eval"}, "last_success_at")...)
	if len(readers) > 0 {
		t.Errorf("свежесть коннекторов начала выгружаться в Go (%v): проверьте, что "+
			"метрика действительно отдаётся в мониторинг, и перепишите этот тест "+
			"под новое поведение", readers)
	}
}

// ── Предикат отставания на живом PostgreSQL ─────────────────────────────────

// TestПредикатОтставанияВедётСебяПоДанным — настоящее исполнение §9.
//
// Три состояния коннектора, которые обязаны различаться:
//
//	свежий    — last_success_at недавно, is_stale = false;
//	отсталый  — last_success_at давно, is_stale = true;
//	ни разу   — last_success_at IS NULL, is_stale = true.
//
// Последнее — то, что молчаливый коннектор отдаёт в первые сутки жизни и что
// обычная проверка «прошло ли N часов с последнего успеха» пропускает как
// «нечего проверять».
//
// БЕЗ БАЗЫ ТЕСТ ПРОПУСКАЕТСЯ: предикат существует только в SQL, и исполнить его
// без PostgreSQL нечем. Структурная часть (наличие колонок и представления) при
// этом проверяется всегда — см. TestСхемаКоннекторовНесётМетрикуСвежести.
func TestПредикатОтставанияВедётСебяПоДанным(t *testing.T) {
	if testing.Short() {
		t.Skip("интеграционный тест: нужен PostgreSQL (запустите без -short)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)

	db := gateOpenLiveSchema(t, ctx, gateFreshnessSchema,
		"0001_initial_schema.up.sql", "0002_lake_acl.up.sql")
	q := ports.Queryer(db)

	seedFreshness(t, ctx, q)

	rows := gateReadFreshness(t, ctx, q)
	byName := map[string]gateFreshnessRow{}
	for _, row := range rows {
		byName[row.name] = row
	}
	if len(byName) != 4 {
		t.Fatalf("представление вернуло %d строк, ожидалось 4: %v", len(byName), rows)
	}

	cases := []struct {
		name      string
		wantStale bool
		wantLag   bool // true — lag_seconds обязан быть непустым
		why       string
	}{
		{"fresh-slack", false, true, "успех был минуту назад, порог час"},
		{"fresh-fast", false, true, "успех был две минуты назад, порог десять минут"},
		{"stale-slack", true, true, "успех был три часа назад, порог час"},
		{"never-run", true, false, "ни одного успеха: отставание не имеет величины, " +
			"но коннектор отстал"},
	}
	for _, want := range cases {
		got, found := byName[want.name]
		if !found {
			t.Errorf("в представлении нет коннектора %s", want.name)
			continue
		}
		if got.stale != want.wantStale {
			t.Errorf("%s: is_stale = %v, ожидалось %v — %s", want.name, got.stale, want.wantStale, want.why)
		}
		if want.wantLag && got.lag == nil {
			t.Errorf("%s: lag_seconds пуст, хотя был успешный проход", want.name)
		}
		if !want.wantLag && got.lag != nil {
			t.Errorf("%s: lag_seconds = %v, хотя ни одного успеха не было: "+
				"отставание коннектора, который не запускался, не вычисляется", want.name, *got.lag)
		}
	}

	// Порог из строки, а не из константы: два коннектора с одинаковым временем
	// последнего успеха обязаны дать РАЗНЫЙ is_stale при разных порогах.
	fresh, stale := byName["fresh-slack"], byName["stale-slack"]
	if fresh.stale == stale.stale {
		t.Errorf("два коннектора с одинаковым последним успехом и разными порогами "+
			"дали одинаковый is_stale (%v): порог отставания не берётся из строки", fresh.stale)
	}
}

// gateFreshnessRow — строка представления connector_freshness.
type gateFreshnessRow struct {
	name     string
	stale    bool
	lag      *int64
	failures int
}

// gateReadFreshness читает представление.
func gateReadFreshness(t *testing.T, ctx context.Context, q ports.Queryer) []gateFreshnessRow {
	t.Helper()
	rows, err := q.Query(ctx, `
		SELECT name, is_stale, lag_seconds, consecutive_failures
		FROM connector_freshness
		ORDER BY name`)
	if err != nil {
		t.Fatalf("чтение connector_freshness: %v", err)
	}
	defer rows.Close()

	var out []gateFreshnessRow
	for rows.Next() {
		var row gateFreshnessRow
		// lag_seconds nullable: у коннектора, который ни разу не запускался,
		// отставания нет, и это разные состояния, а не одно.
		if err := rows.Scan(&row.name, &row.stale, &row.lag, &row.failures); err != nil {
			t.Fatalf("разбор строки connector_freshness: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("обход connector_freshness: %v", err)
	}
	return out
}

// seedFreshness наполняет таблицу коннекторов четырьмя состояниями.
func seedFreshness(t *testing.T, ctx context.Context, q ports.Queryer) {
	t.Helper()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := q.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("наполнение коннекторов: %v", err)
		}
	}
	tenant := gateTenantAcme
	exec(`INSERT INTO tenants (id, name) VALUES ($1, 'acme') ON CONFLICT DO NOTHING`, tenant)

	exec(`INSERT INTO connectors (id, tenant_id, name, kind, last_success_at, stale_after_seconds)
	      VALUES ('11111111-1111-4111-8111-000000000001', $1, 'fresh-slack', 'slack',
	              now() - interval '1 minute', 3600)`, tenant)
	exec(`INSERT INTO connectors (id, tenant_id, name, kind, last_success_at, stale_after_seconds)
	      VALUES ('11111111-1111-4111-8111-000000000002', $1, 'fresh-fast', 'file',
	              now() - interval '2 minutes', 600)`, tenant)
	exec(`INSERT INTO connectors (id, tenant_id, name, kind, last_success_at, stale_after_seconds)
	      VALUES ('11111111-1111-4111-8111-000000000003', $1, 'stale-slack', 'slack',
	              now() - interval '3 hours', 3600)`, tenant)
	exec(`INSERT INTO connectors (id, tenant_id, name, kind, last_success_at, stale_after_seconds)
	      VALUES ('11111111-1111-4111-8111-000000000004', $1, 'never-run', 'gmail',
	              NULL, 3600)`, tenant)

	// Порог отставания в ноль обязан быть отвергнут схемой: иначе «не отстал»
	// превратилось бы в «не отстаёт никогда».
	if _, err := q.Exec(ctx,
		`INSERT INTO connectors (id, tenant_id, name, kind, stale_after_seconds)
		 VALUES ('11111111-1111-4111-8111-000000000005', $1, 'bad-threshold', 'slack', 0)`, tenant); err == nil {
		t.Error("схема приняла stale_after_seconds = 0: порог отставания в ноль " +
			"делает коннектор вечно свежим")
	}
}

// gateFreshnessSchema — отдельная схема ворот свежести.
const gateFreshnessSchema = "kb_gates_freshness"
