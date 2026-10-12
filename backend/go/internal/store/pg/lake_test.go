package postgres

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
)

// Тесты lake и оракула прав §6 против живого PostgreSQL, тем же стендом, что и
// остальной пакет (адрес — KB_GO_TEST_DATABASE_URL). Без БД и в -short — пропуск,
// не падение: см. requireDB в schema_test.go.
//
// Схема для этих тестов НЕ копируется, как testSchemaSQL в schema_test.go, а
// читается из настоящего файла миграции. Причина практическая: lake — источник
// правды, и тест, который проверял бы не ту схему, что поедет в прод, хуже
// отсутствия теста. Такой тест ловит и опечатку в DDL, и расхождение миграции
// с кодом.

// lakeMigrationOnce поднимает схему 0002 один раз на процесс.
var lakeMigrationOnce sync.Once

// lakeTables — таблицы 0002, которые очищаются перед каждым тестом.
//
// Порядок обратный появлению, хотя CASCADE делает его необязательным: список
// читается человеком при разборе падения, и в нём должен быть смысл.
//
// lakeTables не входит в testTables из schema_test.go: тот файл не наш, и
// дописывать его — значит пересечься с чужим потоком правок.
const lakeTables = "dead_letter_events, doc_edges, connectors, acl_permissions, " +
	"acl_resource_group_members, acl_resource_groups, acl_containers, " +
	"acl_global_group_members, acl_global_groups, acl_users, " +
	"raw_event_ids, raw_events"

// requireLakeDB готовит тест к работе со схемой 0002: поднимает миграцию (один
// раз на процесс) и очищает её таблицы.
func requireLakeDB(t *testing.T) {
	t.Helper()
	requireDB(t)
	applyLakeMigration(t)
	resetLakeTables(t)
}

// applyLakeMigration применяет настоящую миграцию 0002.
//
// Перед применением выполняется откат: база тестов переживает прогоны, а миграция
// состоит из CREATE TABLE без IF NOT EXISTS (так пишут все миграции этого
// проекта), и второй прогон подряд упал бы на «relation already exists».
// Откат перед применением заодно проверяет down-миграцию — на каждом прогоне
// тестов, а не разово при написании.
func applyLakeMigration(t *testing.T) {
	t.Helper()
	lakeMigrationOnce.Do(func() {
		ctx := context.Background()
		down := readMigrationFile(t, "0002_lake_acl.down.sql")
		if _, err := testDB.Exec(ctx, down); err != nil {
			t.Fatalf("откат 0002 перед применением: %v", err)
		}
		up := readMigrationFile(t, "0002_lake_acl.up.sql")
		if _, err := testDB.Exec(ctx, up); err != nil {
			t.Fatalf("применение миграции 0002: %v", err)
		}
	})
}

// readMigrationFile читает файл миграции из каталога migrations/sql.
//
// Путь считается от каталога пакета (internal/store/pg), поэтому до корня
// модуля три уровня, а не два: migrations лежит рядом с internal.
func readMigrationFile(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "..", "..", "migrations", "sql", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("чтение миграции %s: %v", path, err)
	}
	return string(raw)
}

// resetLakeTables очищает таблицы lake и прав.
func resetLakeTables(t *testing.T) {
	t.Helper()
	if _, err := testDB.Exec(testContext(t), "TRUNCATE "+lakeTables+" CASCADE"); err != nil {
		t.Fatalf("очистка таблиц lake: %v", err)
	}
}

// ── Подготовка данных lake ──────────────────────────────────────────────────

// lakeEvent собирает корректное событие lake с заданными параметрами.
func lakeEvent(t *testing.T, tenant uuid.UUID, stage domain.Stage, docID, hash string, occurredAt time.Time) domain.Event {
	t.Helper()
	event, err := domain.NewEvent(domain.NewEventParams{
		TenantID:    tenant,
		Stage:       stage,
		DocID:       docID,
		ContentHash: hash,
		Payload:     json.RawMessage(`{"doc_id":"` + docID + `","title":"заголовок"}`),
		OccurredAt:  occurredAt,
	})
	if err != nil {
		t.Fatalf("сборка события lake: %v", err)
	}
	return event
}

// newLake создаёт lake над тестовым пулом.
func newLake() *Lake { return NewLake(testDB) }

// countRawEvents считает события lake напрямую: проверять количество через
// Read нельзя, Read ограничен по размеру страницы и обрезает результат.
func countRawEvents(t *testing.T) int {
	t.Helper()
	var count int
	if err := testDB.QueryRow(testContext(t), "SELECT count(*) FROM raw_events").Scan(&count); err != nil {
		t.Fatalf("подсчёт событий lake: %v", err)
	}
	return count
}

// baseTime — фиксированная точка отсчёта тестовых меток времени.
var baseTime = time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)

// ── Идемпотентность (§4) ────────────────────────────────────────────────────

// TestPublishIsIdempotentOnRetry — главное требование §4: «все стадии
// идемпотентны по ключу doc_id + content_hash». Повтор той же стадии обязан не
// изменить ни одной строки lake.
//
// Особенность теста: повтор публикуется с ДРУГИМ occurred_at. Именно этот
// случай и ломает наивную реализацию: у raw_events нет уникального индекса
// только по event_id (Postgres не даёт такой на партиционированной по
// occurred_at таблице), поэтому ON CONFLICT (event_id, occurred_at) второй раз
// не срабатывает и lake получает две записи об одном факте. Идемпотентность
// держит реестр raw_event_ids.
func TestPublishIsIdempotentOnRetry(t *testing.T) {
	requireLakeDB(t)
	ctx := testContext(t)
	lake := newLake()

	event := lakeEvent(t, tenantID, domain.CollectStage, "doc-1", "sha256:"+repeatA64(), baseTime)
	if err := lake.Publish(ctx, event); err != nil {
		t.Fatalf("первая публикация: %v", err)
	}

	// Ретрай: тот же факт (те же version/tenant/stage/doc_id/content_hash ⇒
	// тот же event_id), но случившийся позже — так выглядит повтор стадии
	// после перезапуска воркера.
	retry := event
	retry.OccurredAt = baseTime.Add(90 * time.Second)
	if err := lake.Publish(ctx, retry); err != nil {
		t.Fatalf("повторная публикация: %v", err)
	}

	if got := countRawEvents(t); got != 1 {
		t.Errorf("в lake %d событий после повтора, ожидалась 1: ретрай стадии добавил запись", got)
	}
}

// TestPublishBatchIsIdempotentAcrossBatchBoundary — порция, у которой половина
// событий уже записана, не должна дублировать ни их, ни остальных.
func TestPublishBatchIsIdempotentAcrossBatchBoundary(t *testing.T) {
	requireLakeDB(t)
	ctx := testContext(t)
	lake := newLake()

	first := lakeEvent(t, tenantID, domain.CollectStage, "doc-1", "sha256:"+repeatA64(), baseTime)
	second := lakeEvent(t, tenantID, domain.CollectStage, "doc-2", "sha256:"+repeatA64(), baseTime.Add(time.Second))
	if err := lake.PublishBatch(ctx, []domain.Event{first, second}); err != nil {
		t.Fatalf("первая порция: %v", err)
	}

	// Повтор всей порции с другими метками времени.
	if err := lake.PublishBatch(ctx, []domain.Event{
		{OccurredAt: baseTime.Add(time.Hour), Stage: first.Stage, DocID: first.DocID,
			ContentHash: first.ContentHash, Version: first.Version, EventID: first.EventID,
			TenantID: first.TenantID, Payload: first.Payload},
		{OccurredAt: baseTime.Add(time.Hour), Stage: second.Stage, DocID: second.DocID,
			ContentHash: second.ContentHash, Version: second.Version, EventID: second.EventID,
			TenantID: second.TenantID, Payload: second.Payload},
	}); err != nil {
		t.Fatalf("повтор порции: %v", err)
	}

	if got := countRawEvents(t); got != 2 {
		t.Errorf("в lake %d событий после повтора порции, ожидалось 2", got)
	}
}

// TestPublishKeepsFirstOccurredAtOnRetry — повтор не переписывает исходную
// метку времени. Lake только на добавление: пересборка индекса обязана видеть
// время, когда событие произошло, а не время последней попытки его записать.
func TestPublishKeepsFirstOccurredAtOnRetry(t *testing.T) {
	requireLakeDB(t)
	ctx := testContext(t)
	lake := newLake()

	event := lakeEvent(t, tenantID, domain.CollectStage, "doc-1", "sha256:"+repeatA64(), baseTime)
	if err := lake.Publish(ctx, event); err != nil {
		t.Fatalf("первая публикация: %v", err)
	}
	retry := event
	retry.OccurredAt = baseTime.Add(90 * time.Second)
	if err := lake.Publish(ctx, retry); err != nil {
		t.Fatalf("повтор: %v", err)
	}

	var occurredAt time.Time
	err := testDB.QueryRow(ctx,
		"SELECT occurred_at FROM raw_events WHERE event_id = $1", event.EventID).Scan(&occurredAt)
	if err != nil {
		t.Fatalf("чтение occurred_at: %v", err)
	}
	if !occurredAt.UTC().Equal(baseTime) {
		t.Errorf("occurred_at = %s, ожидался исходный %s", occurredAt.UTC(), baseTime)
	}
}

// TestRawEventsRejectsUpdateAndDelete — lake только на добавление (§1,
// принцип 1). Изменение события означает баг пересборки, и оно обязано падать,
// а не проходить молча.
func TestRawEventsRejectsUpdateAndDelete(t *testing.T) {
	requireLakeDB(t)
	ctx := testContext(t)
	lake := newLake()

	event := lakeEvent(t, tenantID, domain.CollectStage, "doc-1", "sha256:"+repeatA64(), baseTime)
	if err := lake.Publish(ctx, event); err != nil {
		t.Fatalf("публикация: %v", err)
	}

	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"UPDATE", "UPDATE raw_events SET payload = '{}'::jsonb WHERE event_id = $1"},
		{"DELETE", "DELETE FROM raw_events WHERE event_id = $1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := testDB.Exec(ctx, tc.sql, event.EventID); err == nil {
				t.Errorf("%s прошёл: lake только на добавление, событие не переписывается", tc.name)
			}
		})
	}
}

// ── Dead letter (§4) ────────────────────────────────────────────────────────

// TestPublishRoutesUnknownVersionToDeadLetter — §4 требует «у каждого топика
// есть dead-letter-очередь», и отдельно §4 называет версионирование схем
// правилом безопасности. Событие неизвестной версии нельзя ни записать (его
// payload никто не умеет читать), ни выбросить (пересборка индекса потом не
// заметит пробела). Оно обязано осесть в очереди с причиной.
func TestPublishRoutesUnknownVersionToDeadLetter(t *testing.T) {
	requireLakeDB(t)
	ctx := testContext(t)
	lake := newLake()

	// Версия будущего: синтаксически корректна («raw.v2»), но lake её не знает.
	event := lakeEvent(t, tenantID, domain.CollectStage, "doc-1", "sha256:"+repeatA64(), baseTime)
	event.Version = "raw.v2"

	err := lake.Publish(ctx, event)
	if err == nil {
		t.Fatal("ожидалась ошибка публикации события неизвестной версии")
	}
	if !strings.Contains(err.Error(), "raw.v2") {
		t.Errorf("ошибка не называет версию: %v", err)
	}

	if got := countRawEvents(t); got != 0 {
		t.Errorf("в lake %d событий, ожидалось 0: неизвестная версия не пишется", got)
	}

	var (
		reasonCode, reasonMessage string
		payloadRaw                string
	)
	err = testDB.QueryRow(ctx, `
		SELECT reason_code, reason_message, payload_raw
		FROM dead_letter_events WHERE event_id = $1`, event.EventID,
	).Scan(&reasonCode, &reasonMessage, &payloadRaw)
	if err != nil {
		t.Fatalf("событие не попало в очередь мёртвых: %v", err)
	}
	if reasonCode != domain.CodeInvalidMetadata {
		t.Errorf("reason_code = %q, ожидался %q", reasonCode, domain.CodeInvalidMetadata)
	}
	if reasonMessage == "" {
		t.Error("reason_message пуст: очередь без причины бесполезна")
	}
	// Исходное тело обязано сохраниться: очередь читают, когда чинят конвейер.
	if !strings.Contains(payloadRaw, "doc-1") {
		t.Errorf("payload_raw = %q, ожидалось исходное тело события", payloadRaw)
	}
}

// TestPublishRoutesBrokenPayloadToDeadLetter — битое тело не должно ни
// пройти, ни исчезнуть. Событие с невалидным JSON собирается в обход
// конвейера; молча проглотить его означало бы, что lake считает полным.
func TestPublishRoutesBrokenPayloadToDeadLetter(t *testing.T) {
	requireLakeDB(t)
	ctx := testContext(t)
	lake := newLake()

	event := lakeEvent(t, tenantID, domain.CollectStage, "doc-1", "sha256:"+repeatA64(), baseTime)
	event.Payload = json.RawMessage(`{"оборвано":`)

	if err := lake.Publish(ctx, event); err == nil {
		t.Fatal("ожидалась ошибка публикации события с битым payload")
	}
	if got := countRawEvents(t); got != 0 {
		t.Errorf("в lake %d событий, ожидалось 0", got)
	}

	var reasonCode string
	if err := testDB.QueryRow(ctx,
		"SELECT reason_code FROM dead_letter_events WHERE event_id = $1", event.EventID,
	).Scan(&reasonCode); err != nil {
		t.Fatalf("событие не попало в очередь мёртвых: %v", err)
	}
	if reasonCode != domain.CodeInvalidMetadata {
		t.Errorf("reason_code = %q, ожидался %q", reasonCode, domain.CodeInvalidMetadata)
	}
}

// TestPublishBatchKeepsValidEventsWhenOneIsBroken — порция не теряется целиком
// из-за одного битого события: остальные обязаны записаться, а вызывающий —
// узнать о проблеме. Обратное поведение (откат всей порции) означало бы, что
// один сбойный коннектор блокирует приём от всех остальных.
func TestPublishBatchKeepsValidEventsWhenOneIsBroken(t *testing.T) {
	requireLakeDB(t)
	ctx := testContext(t)
	lake := newLake()

	good := lakeEvent(t, tenantID, domain.CollectStage, "doc-ok", "sha256:"+repeatA64(), baseTime)
	broken := lakeEvent(t, tenantID, domain.CollectStage, "doc-bad", "sha256:"+repeatA64(), baseTime)
	broken.Version = "raw.v9"

	err := lake.PublishBatch(ctx, []domain.Event{good, broken})
	if err == nil {
		t.Fatal("ожидалась ошибка порции с одним битым событием")
	}
	if got := countRawEvents(t); got != 1 {
		t.Errorf("в lake %d событий, ожидалось 1: корректное событие порции не должно теряться из-за соседа", got)
	}
}

// TestDeadLetterKeepsSingleRowPerEvent — повторная доставка битого события не
// должна забивать очередь дубликатами: за ними перестают быть видны новые
// отказы.
func TestDeadLetterKeepsSingleRowPerEvent(t *testing.T) {
	requireLakeDB(t)
	ctx := testContext(t)
	lake := newLake()

	event := lakeEvent(t, tenantID, domain.CollectStage, "doc-1", "sha256:"+repeatA64(), baseTime)
	event.Version = "raw.v2"
	for range 3 {
		if err := lake.Publish(ctx, event); err == nil {
			t.Fatal("ожидалась ошибка публикации события неизвестной версии")
		}
	}

	var count int
	if err := testDB.QueryRow(ctx,
		"SELECT count(*) FROM dead_letter_events WHERE event_id = $1", event.EventID).Scan(&count); err != nil {
		t.Fatalf("подсчёт строк очереди мёртвых: %v", err)
	}
	if count != 1 {
		t.Errorf("в очереди мёртвых %d строк на одно событие, ожидалась 1", count)
	}
}

// ── Чтение: порядок и фильтры ──────────────────────────────────────────────

// TestReadIsDeterministicallyOrdered — §9 требует пересборки индекса из lake,
// и два прогона обязаны получить одни и те же события в одном порядке при любом
// параллелизме записи. Порядок (occurred_at, event_id) детерминирован только
// целиком: по одной occurred_at события пачки неразличимы, и порядок зависел бы
// от того, в каком порядке воркеры сдали транзакции.
func TestReadIsDeterministicallyOrdered(t *testing.T) {
	requireLakeDB(t)
	ctx := testContext(t)
	lake := newLake()

	// Три события с ОДИНАКОВОЙ меткой времени и разными doc_id: порядок между
	// ними определяется только event_id.
	for _, docID := range []string{"doc-b", "doc-a", "doc-c"} {
		event := lakeEvent(t, tenantID, domain.CollectStage, docID, "sha256:"+repeatA64(), baseTime)
		if err := lake.Publish(ctx, event); err != nil {
			t.Fatalf("публикация %s: %v", docID, err)
		}
	}
	later := lakeEvent(t, tenantID, domain.CollectStage, "doc-d", "sha256:"+repeatA64(), baseTime.Add(time.Minute))
	if err := lake.Publish(ctx, later); err != nil {
		t.Fatalf("публикация doc-d: %v", err)
	}

	first, err := lake.Read(ctx, domain.EventQuery{TenantID: tenantID})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(first) != 4 {
		t.Fatalf("прочитано %d событий, ожидалось 4", len(first))
	}

	// Порядок должен совпадать с порядком по (occurred_at, event_id).
	for i := 1; i < len(first); i++ {
		prev, cur := first[i-1], first[i]
		switch {
		case prev.OccurredAt.After(cur.OccurredAt):
			t.Errorf("порядок нарушен: %s идёт раньше %s", prev.OccurredAt, cur.OccurredAt)
		case prev.OccurredAt.Equal(cur.OccurredAt) && prev.EventID.String() > cur.EventID.String():
			t.Errorf("при равных occurred_at нарушен порядок event_id: %s раньше %s", prev.EventID, cur.EventID)
		}
	}

	// Повторное чтение обязано дать тот же порядок — это и есть требование
	// воспроизводимости.
	second, err := lake.Read(ctx, domain.EventQuery{TenantID: tenantID})
	if err != nil {
		t.Fatalf("повторное Read: %v", err)
	}
	for i := range first {
		if first[i].EventID != second[i].EventID {
			t.Errorf("повторное чтение вернуло другой порядок на позиции %d", i)
		}
	}
}

// TestReadFiltersByStageDocAndTime — фильтры порта обязаны реально отсекать:
// пересборка индекса читает lake по стадиям и по одному документу, и фильтр,
// который ничего не отсекает, молча удваивает работу.
func TestReadFiltersByStageDocAndTime(t *testing.T) {
	requireLakeDB(t)
	ctx := testContext(t)
	lake := newLake()

	collect := lakeEvent(t, tenantID, domain.CollectStage, "doc-1", "sha256:"+repeatA64(), baseTime)
	index := lakeEvent(t, tenantID, domain.IndexStage, "doc-1", "sha256:"+repeatA64(), baseTime.Add(time.Minute))
	other := lakeEvent(t, tenantID, domain.CollectStage, "doc-2", "sha256:"+repeatA64(), baseTime.Add(2*time.Minute))
	for _, event := range []domain.Event{collect, index, other} {
		if err := lake.Publish(ctx, event); err != nil {
			t.Fatalf("публикация %s: %v", event.Stage, err)
		}
	}

	cases := []struct {
		name  string
		query domain.EventQuery
		want  []uuid.UUID
	}{
		{
			name:  "по стадии",
			query: domain.EventQuery{TenantID: tenantID, Stage: domain.IndexStage},
			want:  []uuid.UUID{index.EventID},
		},
		{
			name:  "по документу",
			query: domain.EventQuery{TenantID: tenantID, DocID: "doc-1"},
			want:  []uuid.UUID{collect.EventID, index.EventID},
		},
		{
			name: "по стадии и документу",
			query: domain.EventQuery{
				TenantID: tenantID, Stage: domain.CollectStage, DocID: "doc-2",
			},
			want: []uuid.UUID{other.EventID},
		},
		{
			name:  "по времени Since",
			query: domain.EventQuery{TenantID: tenantID, Since: baseTime.Add(time.Minute)},
			want:  []uuid.UUID{index.EventID, other.EventID},
		},
		{
			name:  "без фильтров",
			query: domain.EventQuery{TenantID: tenantID},
			want:  []uuid.UUID{collect.EventID, index.EventID, other.EventID},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := lake.Read(ctx, tc.query)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("прочитано %d событий, ожидалось %d", len(got), len(tc.want))
			}
			for i, want := range tc.want {
				if got[i].EventID != want {
					t.Errorf("позиция %d: событие %s, ожидалось %s", i, got[i].EventID, want)
				}
			}
		})
	}
}

// TestReadRejectsEmptyTenant — чтение lake без тенанта вернуло бы события всех
// организаций. Отказ обязателен, а не «пустой результат»: пустой результат
// выглядел бы как «у пользователя нет событий» и тихо уронил бы пересборку.
func TestReadRejectsEmptyTenant(t *testing.T) {
	requireLakeDB(t)
	ctx := testContext(t)
	lake := newLake()

	if _, err := lake.Read(ctx, domain.EventQuery{}); err == nil {
		t.Fatal("ожидался отказ на чтение lake без tenant_id")
	}
}

// TestReadIsolatesTenants — события одного тенанта не должны попадать в выдачу
// другого: lake хранит все организации вперемешку, и единственное, что их
// разделяет, — этот фильтр.
func TestReadIsolatesTenants(t *testing.T) {
	requireLakeDB(t)
	ctx := testContext(t)
	lake := newLake()
	seedTenant(t, ctx)

	mine := lakeEvent(t, tenantID, domain.CollectStage, "doc-1", "sha256:"+repeatA64(), baseTime)
	theirs := lakeEvent(t, otherTenant, domain.CollectStage, "doc-2", "sha256:"+repeatA64(), baseTime)
	for _, event := range []domain.Event{mine, theirs} {
		if err := lake.Publish(ctx, event); err != nil {
			t.Fatalf("публикация: %v", err)
		}
	}

	got, err := lake.Read(ctx, domain.EventQuery{TenantID: tenantID})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 1 || got[0].EventID != mine.EventID {
		t.Errorf("прочитано %d событий, ожидалось только своё: %v", len(got), got)
	}
}

// ── Постраничное чтение ─────────────────────────────────────────────────────

// TestReadPaginatesByCursorWithoutGapsOrRepeats — пересборка индекса читает
// lake целиком и постранично. Курсор обязан продолжать страницу ровно с того
// места, где кончилась предыдущая: пропуск события означает документ,
// выпавший из индекса навсегда, а повтор — задвоенный.
func TestReadPaginatesByCursorWithoutGapsOrRepeats(t *testing.T) {
	requireLakeDB(t)
	ctx := testContext(t)
	lake := newLake()

	const total = 7
	all := make([]uuid.UUID, 0, total)
	for i := range total {
		event := lakeEvent(t, tenantID, domain.CollectStage,
			"doc-"+strings.Repeat("x", i), "sha256:"+repeatA64(),
			baseTime.Add(time.Duration(i)*time.Second))
		if err := lake.Publish(ctx, event); err != nil {
			t.Fatalf("публикация события %d: %v", i, err)
		}
		all = append(all, event.EventID)
	}

	var (
		seen  []uuid.UUID
		query = domain.EventQuery{TenantID: tenantID, Limit: 2}
	)
	// Ограничение числа итераций: зациклившийся курсор обнаружится как
	// падение по лимиту, а не как бесконечный тест.
	for range 10 {
		page, err := lake.Read(ctx, query)
		if err != nil {
			t.Fatalf("чтение страницы: %v", err)
		}
		if len(page) == 0 {
			break
		}
		for _, event := range page {
			seen = append(seen, event.EventID)
		}
		query.Cursor = encodeLakeCursor(page[len(page)-1])
	}

	if len(seen) != total {
		t.Fatalf("постранично прочитано %d событий из %d", len(seen), total)
	}
	for i, got := range seen {
		if got != all[i] {
			t.Errorf("позиция %d: событие %s, ожидалось %s", i, got, all[i])
		}
	}
}

// TestReadCursorKeepsEqualTimestamps — граница страницы проходит между
// событиями с одинаковой меткой времени. Сравнение только по occurred_at
// теряло бы всё, что влезло в предыдущую страницу на той же микросекунде, —
// молча, без единого признака пропуска.
func TestReadCursorKeepsEqualTimestamps(t *testing.T) {
	requireLakeDB(t)
	ctx := testContext(t)
	lake := newLake()

	const total = 4
	all := make([]uuid.UUID, 0, total)
	for i := range total {
		// Метка времени одна на все события: различает их только event_id.
		event := lakeEvent(t, tenantID, domain.CollectStage,
			"doc-"+strings.Repeat("y", i), "sha256:"+repeatA64(), baseTime)
		if err := lake.Publish(ctx, event); err != nil {
			t.Fatalf("публикация события %d: %v", i, err)
		}
		all = append(all, event.EventID)
	}
	// Ожидаемый порядок — тот же (occurred_at, event_id), что и у Read, а не
	// порядок вставки: при равных метках времени именно порядок event_id
	// определяет чтение, и сверять страницы с порядком вставки here означало бы
	// сверять с чем-то другим.
	slices.SortFunc(all, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })

	var (
		seen  []uuid.UUID
		query = domain.EventQuery{TenantID: tenantID, Limit: 1}
	)
	for range 10 {
		page, err := lake.Read(ctx, query)
		if err != nil {
			t.Fatalf("чтение страницы: %v", err)
		}
		if len(page) == 0 {
			break
		}
		seen = append(seen, page[0].EventID)
		query.Cursor = encodeLakeCursor(page[0])
	}

	if len(seen) != total {
		t.Fatalf("прочитано %d событий из %d: граница страницы потеряла события с одинаковой меткой", len(seen), total)
	}
	for i, got := range seen {
		if got != all[i] {
			t.Errorf("позиция %d: событие %s, ожидалось %s", i, got, all[i])
		}
	}
}

// TestReadCapsRequestedLimit — Limit от вызывающего ограничивается. Пересборка
// индекса идёт постранично, и запрос без предела утащил бы в память процесса
// столько событий, сколько накопилось за месяцы.
func TestReadCapsRequestedLimit(t *testing.T) {
	requireLakeDB(t)
	ctx := testContext(t)
	lake := newLake()

	event := lakeEvent(t, tenantID, domain.CollectStage, "doc-1", "sha256:"+repeatA64(), baseTime)
	if err := lake.Publish(ctx, event); err != nil {
		t.Fatalf("публикация: %v", err)
	}

	// Запрос за миллионом событий обязан отработать, а не упасть и не утянуть
	// всё: проверяем, что путь исполняется и ограничение не ломает выборку.
	got, err := lake.Read(ctx, domain.EventQuery{TenantID: tenantID, Limit: 1_000_000})
	if err != nil {
		t.Fatalf("Read с огромным Limit: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("прочитано %d событий, ожидалось 1", len(got))
	}
}

// ── Lake: чистые проверки без БД ────────────────────────────────────────────

// TestValidatePayloadVersionRejectsUnknownVersion — проверка версии обязана
// быть чистой функцией и проверяться без базы: она стоит на пути записи и
// обязана быть дешёвой в вызове и ясной в чтении.
func TestValidatePayloadVersionRejectsUnknownVersion(t *testing.T) {
	cases := []struct {
		name    string
		version string
		payload string
		wantErr bool
	}{
		{
			name:    "текущая версия с объектом",
			version: domain.RawEventVersion,
			payload: `{"doc_id":"d1"}`,
			wantErr: false,
		},
		{
			name:    "текущая версия с массивом",
			version: domain.RawEventVersion,
			payload: `[1,2,3]`,
			wantErr: true,
		},
		{
			name:    "текущая версия со скаляром",
			version: domain.RawEventVersion,
			payload: `"строка"`,
			wantErr: true,
		},
		{
			name:    "неизвестная версия",
			version: "raw.v2",
			payload: `{"doc_id":"d1"}`,
			wantErr: true,
		},
		{
			name:    "пустая версия",
			version: "",
			payload: `{"doc_id":"d1"}`,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePayloadVersion(tc.version, json.RawMessage(tc.payload))
			if tc.wantErr && err == nil {
				t.Error("ожидалась ошибка, получено nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("ожидалась приёмка, получено: %v", err)
			}
		})
	}
}

// TestParseLakeCursorRejectsBrokenValue — битый курсор обязан падать на
// вызывающем, а не после того, как база вернёт первую страницу: иначе
// пересборка индекса молча начинается не с того места.
func TestParseLakeCursorRejectsBrokenValue(t *testing.T) {
	cases := []struct {
		name    string
		cursor  string
		wantErr bool
		wantNil bool
	}{
		// Пустой курсор — начало ленты, и это НЕ ошибка: так начинается чтение.
		{name: "пустой курсор — начало ленты", cursor: "", wantNil: true},
		{
			name:    "корректный курсор",
			cursor:  encodeLakeCursor(domain.Event{OccurredAt: baseTime, EventID: docID}),
			wantNil: false,
		},
		{name: "без разделителя", cursor: "2026-11-05T10:00:00Z", wantErr: true},
		{name: "битая метка времени", cursor: "не-дата|" + docID.String(), wantErr: true},
		{name: "битый event_id", cursor: "2026-11-05T10:00:00Z|не-uuid", wantErr: true},
		{name: "нулевой event_id", cursor: "2026-11-05T10:00:00Z|" + uuid.Nil.String(), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := parseLakeCursor(tc.cursor)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("курсор %q принят, ожидался отказ", tc.cursor)
				}
				return
			}
			if err != nil {
				t.Fatalf("ожидался разбор курсора, получена ошибка: %v", err)
			}
			if tc.wantNil && parsed != nil {
				t.Errorf("разобран курсор %+v, ожидалось nil", parsed)
			}
			if !tc.wantNil && parsed == nil {
				t.Error("ожидался разобранный курсор, получен nil")
			}
		})
	}
}

// TestEncodeLakeCursorRoundTrip — метка времени идёт в курсоре в UTC и с
// наносекундами: потеря точности сдвинула бы границу страницы и оставила бы
// событие одновременно на двух страницах.
func TestEncodeLakeCursorRoundTrip(t *testing.T) {
	event := domain.Event{
		OccurredAt: time.Date(2026, 11, 5, 10, 0, 0, 123456789, time.FixedZone("МСК", 3*3600)),
		EventID:    docID,
	}
	cursor := encodeLakeCursor(event)

	parsed, err := parseLakeCursor(cursor)
	if err != nil {
		t.Fatalf("разбор курсора: %v", err)
	}
	if parsed == nil {
		t.Fatal("курсор не разобран")
	}
	if !parsed.occurredAt.Equal(event.OccurredAt) {
		t.Errorf("метка времени %s не совпала с исходной %s", parsed.occurredAt, event.OccurredAt)
	}
	if parsed.eventID != event.EventID {
		t.Errorf("event_id %s не совпал с исходным %s", parsed.eventID, event.EventID)
	}
}

// TestReasonCodeIsStableForAppErrors — очередь мёртвых разбирается запросами по
// reason_code, а текст причины меняется вместе с формулировкой. Код обязан
// браться из кода доменной ошибки, а не выводиться из сообщения.
func TestReasonCodeIsStableForAppErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{name: "доменная ошибка", err: domain.InvalidMetadata("что-то"), want: domain.CodeInvalidMetadata},
		{name: "обёрнутая доменная ошибка", err: domain.NotFound("нет"), want: domain.CodeNotFound},
		{name: "обычная ошибка", err: context.Canceled, want: "LAKE_REJECTED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reasonCode(tc.err); got != tc.want {
				t.Errorf("reasonCode = %q, ожидался %q", got, tc.want)
			}
		})
	}
}

// TestLakeImplementsBothSeams — lake закрывает оба интерфейса четвёртой двери
// §8. Тест не требует базы: проверяется соответствие типов, а не данные, и
// пустой пул здесь не используется — ни один метод не вызывается.
func TestLakeImplementsBothSeams(t *testing.T) {
	var lake any = NewLake(nil)
	if _, ok := lake.(ports.EventBus); !ok {
		t.Error("lake не реализует ports.EventBus")
	}
	if _, ok := lake.(ports.EventStore); !ok {
		t.Error("lake не реализует ports.EventStore")
	}
}
