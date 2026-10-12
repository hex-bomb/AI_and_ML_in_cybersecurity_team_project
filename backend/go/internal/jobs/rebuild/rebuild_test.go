package rebuild_test

// Тесты пересборки индекса из lake — §1 (принцип 1), §4 («любой индекс можно
// удалить и воссоздать из lake»), §9 («Восстановление»).
//
// Что здесь проверяется и почему именно это:
//
//	СВЁРТКА — collect→normalize→enrich→index даёт документ, а стадии без index
//	не дают ничего; последняя редакция документа побеждает.
//
//	НАДГРОБИЕ — документ, удалённый в источнике, обязан ОТСУТСТВОВАТЬ после
//	пересборки. Это главное утверждение §9 об удалении, и проверяется оно и на
//	плане (документа нет в Documents), и на индексе (DeleteDocument зван по его
//	паре, чанков не записано).
//
//	ИДЕМПОТЕНТНОСТЬ — две пересборки одного lake дают побайтно одну и ту же
//	последовательность записей в индекс. §4 требует идемпотентности стадий, и
//	пересборка, добавляющая чанки при каждом повторе, была бы утечкой объёма
//	индекса и мусором в выдаче.
//
//	ДЕТЕРМИНИЗМ — свёртка не заимствует ничего у порядка обхода карт. Проверка
//	делает много прогонов и сравнивает результат; обход карты в Go меняется от
//	прогона к прогону, поэтому такой тест ловит ровно то, что надо.
//
//	ЧТЕНИЕ ПОСТРАНИЧНО — лента читается целиком и в порядке порта; подделка lake
//	проверяет, что курсор двигается, а курсор, застрявший на месте, останавливает
//	пересборку ошибкой, а не кружит вечно.
//
//	ПРОЧИЕ ТЕНАНТЫ И БАЗЫ — пересборка одного тенанта не трогает чужое, а
//	пересборка одной базы не выходит за неё; при этом надгробия обрабатываются
//	ВСЕГДА, потому что тело надгробия не знает базы.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	domain "github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/jobs/rebuild"
)

// harness собирает пересборку на подделках.
type harness struct {
	service *rebuild.Service
	lake    *fakeLake
	index   *recordingIndex
	embed   *fakeEmbedder
	chunker *fakeChunker
	steps   []rebuild.Step
}

func newHarness(t *testing.T, events []domain.Event, pageSize int) *harness {
	t.Helper()
	cfg := rebuildConfig()
	h := &harness{
		lake:    &fakeLake{events: sortLake(events), pageSize: pageSize},
		index:   &recordingIndex{},
		embed:   &fakeEmbedder{dimension: cfg.EmbeddingDimension},
		chunker: &fakeChunker{},
	}
	service, err := rebuild.New(rebuild.Deps{
		Lake:     h.lake,
		Settings: &fakeSettings{cfg: cfg},
		Queryer:  stubQueryer{},
		Chunk:    h.chunker,
		Embedder: h.embed,
		Index:    h.index,
		Progress: func(step rebuild.Step) { h.steps = append(h.steps, step) },
	})
	if err != nil {
		t.Fatalf("сборка пересборки: %v", err)
	}
	h.service = service
	return h
}

// run выполняет пересборку с явным таймаутом: тест без срока на висящей
// пересборке висит до внешнего таймаута go test и сообщает об этом бесполезно.
func (h *harness) run(t *testing.T, scope rebuild.Scope) (rebuild.Report, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return h.service.Rebuild(ctx, scope)
}

// docFixture собирает описание документа фикстуры.
func fixtureDoc(docID int, title string) docFixture {
	return docFixture{
		docID:     uuid.MustParse(fmt.Sprintf("aaaaaaaa-0000-4000-8000-%012d", docID)),
		versionID: uuid.MustParse(fmt.Sprintf("bbbbbbbb-0000-4000-8000-%012d", docID)),
		kbID:      rebuildKB,
		title:     title,
		spaceID:   rebuildSpace,
		acl:       []string{},
		hash:      fmt.Sprintf("sha256:%012d", docID),
		blocks:    []string{"первый блок " + title, "второй блок " + title},
	}
}

// fullPipeline собирает полный конвейер документа: collect → normalize → enrich →
// index. Именно такой лентой lake выглядит после прохода документа.
func fullPipeline(t *testing.T, doc docFixture, chunkCount int, baseMinute int) []domain.Event {
	t.Helper()
	stages := []domain.Stage{
		domain.CollectStage, domain.NormalizeStage, domain.EnrichStage, domain.IndexStage,
	}
	events := make([]domain.Event, 0, len(stages))
	for i, stage := range stages {
		events = append(events, lakeEvent(t, doc, stage, baseMinute+i,
			doc.indexPayload(t, stage, chunkCount)))
	}
	return events
}

// ── Свёртка ─────────────────────────────────────────────────────────────────

// TestСвёрткаСобираетДокументИзКонвейера — полный успешный путь.
func TestСвёрткаСобираетДокументИзКонвейера(t *testing.T) {
	doc := fixtureDoc(1, "runbook платформы")
	events := fullPipeline(t, doc, 2, 0)

	plan, err := rebuild.Fold(events)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}

	if len(plan.Documents) != 1 {
		t.Fatalf("восстановлено %d документов, ожидался 1: %v", len(plan.Documents), plan.Documents)
	}
	got := plan.Documents[0]
	cases := []struct {
		name      string
		got, want any
	}{
		{"document_id", got.DocumentID, doc.docID},
		{"tenant_id", got.TenantID, rebuildTenant},
		{"kb_id", got.KBID, doc.kbID},
		{"version_id", got.VersionID, doc.versionID},
		{"title", got.Title, doc.title},
		{"content_hash", got.ContentHash, doc.hash},
		{"space_id", got.Rights.SpaceID, rebuildSpace},
		{"блоков", len(got.Blocks), len(doc.blocks)},
		{"chunk_count", got.ChunkCount, 2},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %v, ожидалось %v", c.name, c.got, c.want)
		}
	}
	if len(plan.Deleted) != 0 {
		t.Errorf("в плане %d удалённых документов, ожидался 0: %v", len(plan.Deleted), plan.Deleted)
	}
}

// TestСвёрткаБерётТолькоДокументыДошедшиеДоИндекса — достаточность стадий.
//
// Документ, не дошедший до стадии index, в пересборку не попадает: в индексе его
// не было, и ставить его туда на основании промежуточных стадий значило бы
// выдумать состояние, которого не было.
func TestСвёрткаБерётТолькоДокументыДошедшиеДоИндекса(t *testing.T) {
	indexed := fixtureDoc(1, "проиндексирован")
	pending := fixtureDoc(2, "застрял на enrich")

	full := fullPipeline(t, indexed, 2, 0)
	full = append(full, pipelineEvent(t, pending, domain.CollectStage, 10))
	full = append(full, pipelineEvent(t, pending, domain.NormalizeStage, 11))
	full = append(full, pipelineEvent(t, pending, domain.EnrichStage, 12))

	plan, err := rebuild.Fold(sortLake(full))
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if len(plan.Documents) != 1 || plan.Documents[0].DocumentID != indexed.docID {
		t.Fatalf("в пересборку попали %v, ожидался только %s (документ без стадии "+
			"index в индексе не было — восстанавливать нечего)",
			documentIDs(plan.Documents), indexed.docID)
	}
}

// TestСвёрткаБерётПоследнююРедакцию — новый content_hash это новая глава (§7).
func TestСвёрткаБерётПоследнююРедакцию(t *testing.T) {
	doc := fixtureDoc(1, "runbook")
	events := fullPipeline(t, doc, 2, 0)

	second := doc
	second.hash = "sha256:999999999999"
	second.title = "runbook, редакция 2"
	events = append(events, fullPipeline(t, second, 5, 20)...)

	plan, err := rebuild.Fold(sortLake(events))
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if len(plan.Documents) != 1 {
		t.Fatalf("восстановлено %d документов, ожидался 1: %v", len(plan.Documents), plan.Documents)
	}
	got := plan.Documents[0]
	if got.ContentHash != "sha256:999999999999" || got.Title != "runbook, редакция 2" ||
		got.ChunkCount != 5 {
		t.Errorf("восстановлена редакция %q (чанков %d, заголовок %q), ожидалась вторая",
			got.ContentHash, got.ChunkCount, got.Title)
	}
}

// ── Надгробие: главное утверждение об удалении ─────────────────────────────

// TestНадгробиеУбираетДокументИзПересборки — §9 «документ, удалённый в
// источнике, исчезает из выдачи» для ПЕРЕСБОРКИ.
//
// Документ прошёл конвейер и был проиндексирован, а потом удалён в источнике:
// lake знает и о стадии index, и о надгробии. Пересборка обязана оставить в
// индексе ровно один документ и убрать второй ПО ПАРЕ (тенант, документ).
func TestНадгробиеУбираетДокументИзПересборки(t *testing.T) {
	kept := fixtureDoc(1, "остаётся в выдаче")
	removed := fixtureDoc(2, "удалён в источнике")

	events := fullPipeline(t, kept, 2, 0)
	events = append(events, fullPipeline(t, removed, 2, 10)...)
	events = append(events, tombstoneEvent(t, removed,
		uuid.MustParse("cccccccc-0000-4000-8000-000000000002"), 30))

	// План: документа нет, и он помечен удалённым.
	plan, err := rebuild.Fold(sortLake(events))
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if len(plan.Documents) != 1 || plan.Documents[0].DocumentID != kept.docID {
		t.Fatalf("в пересборку попали %v, ожидался только %s: удалённый документ "+
			"вернулся бы в выдачу", documentIDs(plan.Documents), kept.docID)
	}
	if len(plan.Deleted) != 1 || plan.Deleted[0] != removed.docID.String() {
		t.Fatalf("удалённых документов %v, ожидался [%s]", plan.Deleted, removed.docID)
	}

	// Индекс: удаление звано по паре, чанков удалённого не записано.
	h := newHarness(t, events, 500)
	report, err := h.run(t, rebuild.Scope{TenantID: rebuildTenant})
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if report.Tombstones != 1 {
		t.Errorf("учтено %d надгробий, ожидалось 1", report.Tombstones)
	}
	if got := h.index.документы(); len(got) != 1 || got[0] != kept.docID.String() {
		t.Errorf("в индексе %v, ожидался только %s: удалённый документ не должен "+
			"вернуться после пересборки", got, kept.docID)
	}
	var sawRemoval bool
	for _, call := range h.index.deleted {
		if call.documentID == removed.docID.String() {
			sawRemoval = true
			if call.tenantID != rebuildTenant.String() {
				t.Errorf("удаление ограничено тенантом %q вместо %q: чужой документ "+
					"с тем же document_id пострадал бы", call.tenantID, rebuildTenant)
			}
		}
	}
	if !sawRemoval {
		t.Errorf("индекс не звали DeleteDocument по паре (%s, %s): чанки удалённого "+
			"документа остались бы в индексе", rebuildTenant, removed.docID)
	}
}

// TestНадгробиеДоИндексацииНеОживает — удалённый документ не восстанавливается
// даже без стадии index.
func TestНадгробиеДоИндексацииНеОживает(t *testing.T) {
	doc := fixtureDoc(3, "не доиндексирован и удалён")
	events := []domain.Event{
		pipelineEvent(t, doc, domain.CollectStage, 0),
		pipelineEvent(t, doc, domain.EnrichStage, 1),
		tombstoneEvent(t, doc, uuid.MustParse("cccccccc-0000-4000-8000-000000000003"), 2),
	}

	plan, err := rebuild.Fold(sortLake(events))
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if len(plan.Documents) != 0 || len(plan.Deleted) != 0 {
		t.Fatalf("восстановлено %v, удалено %v, ожидалось пусто в обоих списках: "+
			"документ, которого не было в индексе, удалять из него тоже нечего",
			documentIDs(plan.Documents), plan.Deleted)
	}
}

// TestПовторноеУдалениеПослеНовойЗагрузки — документ можно загрузить заново и
// удалить снова (delete.go, довод 2 «повторное удаление после новой загрузки»).
//
// Оба порядка обязаны давать правильный ответ, и различаются они только тем,
// какое событие пришло последним:
//
//	загрузили → удалили  → удалён;
//	удалили → загрузили  → снова в выдаче, потому что он снова есть в источнике;
//	удалили → загрузили → удалили → удалён.
func TestПовторноеУдалениеПослеНовойЗагрузки(t *testing.T) {
	doc := fixtureDoc(4, "живучий документ")
	reuploaded := doc
	reuploaded.hash = "sha256:444444444444"
	job1 := uuid.MustParse("cccccccc-0000-4000-8000-000000000004")
	job2 := uuid.MustParse("cccccccc-0000-4000-8000-000000000005")

	cases := []struct {
		name     string
		events   []domain.Event
		wantKept bool
	}{
		{
			name: "загрузили и удалили — документа нет",
			events: append(
				fullPipeline(t, doc, 2, 0),
				tombstoneEvent(t, doc, job1, 30),
			),
			wantKept: false,
		},
		{
			name: "удалили, потом загрузили заново — документ снова в выдаче",
			events: append([]domain.Event{tombstoneEvent(t, doc, job1, 0)},
				fullPipeline(t, reuploaded, 3, 10)...),
			wantKept: true,
		},
		{
			name: "удалили, загрузили заново и удалили ещё раз — документа нет",
			events: append(
				append([]domain.Event{tombstoneEvent(t, doc, job1, 0)},
					fullPipeline(t, reuploaded, 3, 10)...),
				tombstoneEvent(t, doc, job2, 30),
			),
			wantKept: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan, err := rebuild.Fold(sortLake(c.events))
			if err != nil {
				t.Fatalf("Fold: %v", err)
			}
			kept := len(plan.Documents) == 1 && plan.Documents[0].DocumentID == doc.docID
			if kept != c.wantKept {
				t.Fatalf("документ в пересборке = %v, ожидалось %v (восстановлено %v, "+
					"удалено %v)", kept, c.wantKept, documentIDs(plan.Documents), plan.Deleted)
			}
			if !c.wantKept && len(plan.Deleted) != 1 {
				t.Errorf("удалённых документов %v, ожидался [%s]: второе удаление — "+
					"другой факт, и пересборка обязана его увидеть", plan.Deleted, doc.docID)
			}
		})
	}
}

// ── Идемпотентность и детерминизм ───────────────────────────────────────────

// TestПересборкаИдемпотентна — §4 «любой индекс можно удалить и воссоздать из
// lake» плюс §7 (учебная тревога): пересборка обязана быть безопасна для
// повторного запуска.
//
// Сравниваются ПОСЛЕДОВАТЕЛЬНОСТИ записей, а не множества: повторный Upsert с
// тем же chunk_id перезаписывает документ индекса, и сравнение множеств
// сочло бы дубль «тем же самым». Последовательность же показывает и дубль, и
// пропуск.
func TestПересборкаИдемпотентна(t *testing.T) {
	first := fixtureDoc(1, "runbook")
	second := fixtureDoc(2, "postmortem")

	events := fullPipeline(t, first, 2, 0)
	events = append(events, fullPipeline(t, second, 3, 10)...)
	events = append(events, tombstoneEvent(t, second,
		uuid.MustParse("cccccccc-0000-4000-8000-000000000006"), 30))

	var previous []string
	for attempt := 1; attempt <= 3; attempt++ {
		h := newHarness(t, events, 500)
		report, err := h.run(t, rebuild.Scope{TenantID: rebuildTenant})
		if err != nil {
			t.Fatalf("пересборка %d: %v", attempt, err)
		}
		if report.Indexed != 1 || report.Tombstones != 1 {
			t.Errorf("пересборка %d: записано %d документов, учтено %d надгробий, "+
				"ожидалось 1 и 1", attempt, report.Indexed, report.Tombstones)
		}
		got := h.index.подписи()
		if previous != nil && !equalStrings(previous, got) {
			t.Fatalf("пересборка %d записала другой индекс:\n было %v\n стало %v",
				attempt, previous, got)
		}
		previous = got
	}
}

// TestСвёрткаДетерминирована — обход карты не должен просачиваться в результат.
//
// Сортировка плана по doc_id — единственное, что отделяет воспроизводимую
// пересборку от случайной: накопление идёт в map[string]*docState, а обход map в
// Go упорядочен произвольно и меняется от прогона к прогону. Много прогонов подряд
// поймали бы это без всякой подмены: сортировка всё равно нужна, чтобы порядок
// записей в индекс совпадал.
func TestСвёрткаДетерминирована(t *testing.T) {
	docs := make([]docFixture, 0, 12)
	for i := 1; i <= 12; i++ {
		doc := fixtureDoc(i, fmt.Sprintf("документ %d", i))
		docs = append(docs, doc)
	}
	var events []domain.Event
	for i, doc := range docs {
		events = append(events, fullPipeline(t, doc, 2, i*10)...)
	}
	events = append(events, tombstoneEvent(t, docs[3],
		uuid.MustParse("cccccccc-0000-4000-8000-000000000007"), 500))
	events = sortLake(events)

	reference, err := rebuild.Fold(events)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	referenceIDs := documentIDs(reference.Documents)
	if !sortedAscending(referenceIDs) {
		t.Fatalf("план не отсортирован по doc_id: %v", referenceIDs)
	}

	// Прогоны подряд плюс перестановка входа: порядок lake — это порядок порта,
	// и свёртка имеет право на него опираться, но не имеет права зависеть от
	// порядка обхода карты при накоплении.
	for attempt := 0; attempt < 200; attempt++ {
		plan, err := rebuild.Fold(events)
		if err != nil {
			t.Fatalf("Fold (прогон %d): %v", attempt, err)
		}
		if got := documentIDs(plan.Documents); !equalStrings(got, referenceIDs) {
			t.Fatalf("прогон %d дал другой порядок документов:\n канон %v\n получено %v",
				attempt, referenceIDs, got)
		}
		if !equalStrings(plan.Deleted, reference.Deleted) {
			t.Fatalf("прогон %d дал другой список удалённых: %v вместо %v",
				attempt, plan.Deleted, reference.Deleted)
		}
	}
}

// TestДублиСобытийНеМеняютПлан — §4 «все стадии идемпотентны».
//
// Повтор стадии возвращает те же события с теми же event_id (lake отбрасывает
// их реестром raw_event_ids, но потребитель читающий поток в обход реестра
// получить может).
func TestДублиСобытийНеМеняютПлан(t *testing.T) {
	doc := fixtureDoc(1, "runbook")
	events := fullPipeline(t, doc, 2, 0)

	reference, err := rebuild.Fold(events)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}

	doubled := append(slices.Clone(events), events...)
	plan, err := rebuild.Fold(sortLake(doubled))
	if err != nil {
		t.Fatalf("Fold с дублями: %v", err)
	}
	if !equalStrings(documentIDs(plan.Documents), documentIDs(reference.Documents)) {
		t.Errorf("дубли событий изменили пересборку: %v вместо %v",
			documentIDs(plan.Documents), documentIDs(reference.Documents))
	}
}

// ── Чтение lake ─────────────────────────────────────────────────────────────

// TestПересборкаЧитаетЛентуПостранично — страницы не теряются и не дублируются.
//
// Подделка lake реализует keyset-курсор ровно как адаптер: страница начинается
// строго после (occurred_at, event_id) последнего прочитанного. Маленькая
// страница обязана собрать ту же ленту, что и сплошная.
func TestПересборкаЧитаетЛентуПостранично(t *testing.T) {
	var events []domain.Event
	for i := 1; i <= 7; i++ {
		events = append(events, fullPipeline(t, fixtureDoc(i, fmt.Sprintf("док %d", i)), 2, i*10)...)
	}
	events = sortLake(events)

	want := newHarness(t, events, 0)
	if _, err := want.run(t, rebuild.Scope{TenantID: rebuildTenant}); err != nil {
		t.Fatalf("сплошное чтение: %v", err)
	}

	for _, pageSize := range []int{1, 2, 3, 5, 7, 8, 100} {
		h := newHarness(t, events, pageSize)
		report, err := h.run(t, rebuild.Scope{TenantID: rebuildTenant, PageSize: pageSize})
		if err != nil {
			t.Fatalf("страница %d: %v", pageSize, err)
		}
		if report.Events != len(events) {
			t.Errorf("страница %d прочитала %d событий из %d", pageSize, report.Events, len(events))
		}
		if !equalStrings(h.index.подписи(), want.index.подписи()) {
			t.Errorf("страница %d дала другой индекс", pageSize)
		}
		if len(h.lake.calls) < 2 {
			t.Errorf("страница %d: обращений к lake %d, чтение явно не постраничное",
				pageSize, len(h.lake.calls))
		}
	}
}

// TestПересборкаОстанавливаетсяНаЗастрявшемКурсоре — страховка от бесконечного
// цикла.
//
// Адаптер, вернувший повторяющуюся страницу, иначе крутил бы пересборку до
// конца времени и отчитался бы успехом, построив индекс из первой страницы.
func TestПересборкаОстанавливаетсяНаЗастрявшемКурсоре(t *testing.T) {
	doc := fixtureDoc(1, "runbook")
	events := fullPipeline(t, doc, 2, 0)

	h := newHarness(t, events, 500)
	stuck := &stuckLake{events: sortLake(events)}

	service, err := rebuild.New(rebuild.Deps{
		Lake:     stuck,
		Settings: &fakeSettings{cfg: rebuildConfig()},
		Queryer:  stubQueryer{},
		Chunk:    h.chunker,
		Embedder: h.embed,
		Index:    h.index,
	})
	if err != nil {
		t.Fatalf("сборка пересборки: %v", err)
	}

	if _, err := service.Rebuild(context.Background(),
		rebuild.Scope{TenantID: rebuildTenant}); err == nil {
		t.Fatal("пересборка завершилась успешно на застрявшем курсоре: индекс был бы " +
			"собран из первой страницы, а отчёт сказал бы «готово»")
	}
	if stuck.calls < 2 {
		t.Errorf("lake зван %d раз: цикл чтения не был запущен повторно, и защита от "+
			"застревания не проверена", stuck.calls)
	}
}

// stuckLake отдаёт одну и ту же страницу на любой курсор.
type stuckLake struct {
	events []domain.Event
	calls  int
}

func (l *stuckLake) Read(_ context.Context, q domain.EventQuery) ([]domain.Event, error) {
	l.calls++
	return l.events, nil
}

func (l *stuckLake) Ping(context.Context) error { return nil }

func (l *stuckLake) Close() error { return nil }

// TestПересборкаПроверяетЖивостьLake — §9: первым делом проверяется «lake вообще
// доступен».
func TestПересборкаПроверяетЖивостьLake(t *testing.T) {
	doc := fixtureDoc(1, "runbook")
	h := newHarness(t, fullPipeline(t, doc, 2, 0), 500)
	h.lake.pingErr = domain.ServiceUnavailable("PostgreSQL недоступен")

	if _, err := h.run(t, rebuild.Scope{TenantID: rebuildTenant}); err == nil {
		t.Fatal("пересборка пошла на недоступном lake: §9 требует, чтобы это было " +
			"заметно сразу, а не после получаса чтения")
	}
	if len(h.lake.calls) != 0 {
		t.Errorf("при недоступном lake выполнено %d чтений, ожидалось 0", len(h.lake.calls))
	}
}

// TestПересборкаПрерываетсяПоОтмене — §4: пересборка безопасна для прерывания.
//
// Отмена обязана быть громкой: индекс после прерывания неполон, и «успешно
// завершилась» после отмены было бы враньём. Повторный запуск безопасен и
// доводит дело до конца — это проверяется тестом идемпотентности.
func TestПересборкаПрерываетсяПоОтмене(t *testing.T) {
	var events []domain.Event
	for i := 1; i <= 5; i++ {
		events = append(events, fullPipeline(t, fixtureDoc(i, fmt.Sprintf("док %d", i)), 2, i*10)...)
	}
	h := newHarness(t, events, 500)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := h.service.Rebuild(ctx, rebuild.Scope{TenantID: rebuildTenant}); err == nil {
		t.Fatal("пересборка на отменённом контексте отчиталась успехом")
	} else if !errors.Is(err, context.Canceled) {
		t.Errorf("пересборка вернула %v, ожидалась отмена контекста", err)
	}
	if len(h.index.written) != 0 {
		t.Errorf("на отменённом контексте записано %d чанков, ожидался 0", len(h.index.written))
	}
}

// ── Границы области пересборки ──────────────────────────────────────────────

// TestПересборкаНеВыходитЗаПределыОбласти — база знаний и границы области.
//
// Фикстура: «мой» документ лежит в базе rebuildKB и УДАЛЁН надгробием,
// «чужой» — в другой базе и жив. Проверяются три независимых утверждения:
//
//	фильтр по базе пишет только свою базу и не трогает чужую;
//	надгробия обрабатываются ВСЕГДА, даже когда база чужая, потому что тело
//	надгробия базы не знает (delete.go) — пропустить их из-за неизвестной базы
//	значило бы вернуть удалённый документ в выдачу, то есть ровно то нарушение
//	§9, ради которого пересборка и делается;
//	каждый перезаписываемый документ сносится по паре (тенант, документ) ПЕРЕД
//	записью — иначе прежняя редакция осталась бы рядом с новой.
func TestПересборкаНеВыходитЗаПределыОбласти(t *testing.T) {
	mine := fixtureDoc(1, "мой документ")
	foreign := fixtureDoc(2, "чужой документ")
	foreign.kbID = rebuildOther

	events := fullPipeline(t, mine, 2, 0)
	events = append(events, fullPipeline(t, foreign, 2, 10)...)
	events = append(events, tombstoneEvent(t, mine,
		uuid.MustParse("cccccccc-0000-4000-8000-000000000008"), 30))

	cases := []struct {
		name     string
		scope    rebuild.Scope
		wantDocs []string
		wantSkip int
	}{
		{
			name:     "весь тенант",
			scope:    rebuild.Scope{TenantID: rebuildTenant},
			wantDocs: []string{foreign.docID.String()},
		},
		{
			name: "только своя база знаний — удалённого в ней нет",
			scope: rebuild.Scope{
				TenantID: rebuildTenant,
				KBID:     rebuildKB.String(),
			},
			wantDocs: nil,
			wantSkip: 1,
		},
		{
			// Пропущенных нет: удалённый документ ушёл не по фильтру базы, а по
			// своему надгробию — до фильтра он даже не дошёл.
			name: "только чужая база знаний",
			scope: rebuild.Scope{
				TenantID: rebuildTenant,
				KBID:     rebuildOther.String(),
			},
			wantDocs: []string{foreign.docID.String()},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, events, 500)
			report, err := h.run(t, c.scope)
			if err != nil {
				t.Fatalf("Rebuild: %v", err)
			}
			if got := h.index.документы(); !equalStrings(got, c.wantDocs) {
				t.Errorf("в индексе %v, ожидалось %v", got, c.wantDocs)
			}
			if report.Skipped != c.wantSkip {
				t.Errorf("пропущено %d документов, ожидалось %d", report.Skipped, c.wantSkip)
			}
			if report.Indexed != len(c.wantDocs) {
				t.Errorf("отчёт говорит о %d записанных документах, в индексе их %d",
					report.Indexed, len(c.wantDocs))
			}

			// Надгробие обработано при любом фильтре по базе.
			var removed []string
			for _, call := range h.index.deleted {
				if call.tenantID != rebuildTenant.String() {
					t.Errorf("удаление ограничено тенантом %q вместо %q", call.tenantID, rebuildTenant)
				}
				removed = append(removed, call.documentID)
			}
			if !slices.Contains(removed, mine.docID.String()) {
				t.Errorf("надгробие не обработано: удалено %v. Фильтр по базе не должен "+
					"прятать удаление — тело надгробия базу не знает", removed)
			}
		})
	}
}

// TestЧужийТенантНеЧитается — изоляция §3/§4.
func TestЧужийТенантНеЧитается(t *testing.T) {
	doc := fixtureDoc(1, "runbook")
	h := newHarness(t, fullPipeline(t, doc, 2, 0), 500)

	report, err := h.run(t, rebuild.Scope{TenantID: rebuildOther})
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if report.Events != 0 || report.Indexed != 0 || len(h.index.written) != 0 {
		t.Fatalf("пересборка чужого тенанта прочитала %d событий и записала %d чанков: "+
			"lake отдаёт по tenant_id, и чужой тенант обязан вернуть пустоту",
			report.Events, len(h.index.written))
	}
}

// ── Отказы ──────────────────────────────────────────────────────────────────

// TestПересборкаОтвергаетНекорректнуюОбласть — граница сценария.
//
// Про базу знаний отдельно: опечатка в идентификаторе молча ничего не нашла бы
// и отчиталась «готово», оставив индекс прежним. Пустой пересборки, сказанной
// вслух, здесь быть не может.
func TestПересборкаОтвергаетНекорректнуюОбласть(t *testing.T) {
	cases := []struct {
		name  string
		scope rebuild.Scope
	}{
		{"без тенанта", rebuild.Scope{}},
		{"отрицательная страница", rebuild.Scope{TenantID: rebuildTenant, PageSize: -1}},
		{"страница выше потолка адаптера",
			rebuild.Scope{TenantID: rebuildTenant, PageSize: rebuild.PageSizeMax + 1}},
		{"база знаний не идентификатор",
			rebuild.Scope{TenantID: rebuildTenant, KBID: "не-ууид"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, nil, 500)
			if _, err := h.run(t, c.scope); err == nil {
				t.Fatal("пересборка приняла некорректную область")
			}
			if len(h.lake.calls) != 0 {
				t.Errorf("при некорректной области lake зван %d раз, ожидался 0", len(h.lake.calls))
			}
		})
	}
}

// TestСборкаПересборкиПроверяетПорты — частичная сборка обязана падать на старте.
//
// Сценарий без lake не имеет смысла, а сценарий без индекса или эмбеддера
// молча пересобрёт «индекс» в никуда и отчитается об успехе.
func TestСборкаПересборкиПроверяетПорты(t *testing.T) {
	cfg := rebuildConfig()
	full := rebuild.Deps{
		Lake:     &fakeLake{},
		Settings: &fakeSettings{cfg: cfg},
		Queryer:  stubQueryer{},
		Chunk:    &fakeChunker{},
		Embedder: &fakeEmbedder{dimension: cfg.EmbeddingDimension},
		Index:    &recordingIndex{},
	}
	if _, err := rebuild.New(full); err != nil {
		t.Fatalf("полная сборка отвергнута: %v", err)
	}

	for _, missing := range []string{"Lake", "Settings", "Queryer", "Chunk", "Embedder", "Index"} {
		t.Run("без "+missing, func(t *testing.T) {
			deps := full
			switch missing {
			case "Lake":
				deps.Lake = nil
			case "Settings":
				deps.Settings = nil
			case "Queryer":
				deps.Queryer = nil
			case "Chunk":
				deps.Chunk = nil
			case "Embedder":
				deps.Embedder = nil
			case "Index":
				deps.Index = nil
			}
			if _, err := rebuild.New(deps); err == nil {
				t.Fatalf("пересборка собрана без %s", missing)
			}
		})
	}
}

// TestПересборкаОстанавливаетсяНаПервойОшибке — что происходит при отказе на
// каждом шаге.
//
// Правило одно: незаписанный документ не оставляет после себя половину работы.
// Нарезка и эмбеддер считаются ДО удаления чанков из индекса, поэтому отказ на
// них оставляет индекс в прежнем состоянии — документ, который был в выдаче,
// остаётся в выдаче, а не исчезает из неё навсегда.
func TestПересборкаОстанавливаетсяНаПервойОшибке(t *testing.T) {
	doc := fixtureDoc(1, "runbook")
	events := fullPipeline(t, doc, 2, 0)

	cases := []struct {
		name         string
		prepare      func(*harness)
		wantErrCode  string
		wantWritten  int
		wantReported bool
	}{
		{
			name: "нарезка упала — индекс не тронут",
			prepare: func(h *harness) {
				h.chunker.err = domain.ConfigurationInvalid("chunking не настроено", nil)
			},
			wantErrCode: domain.CodeConfigurationInvalid,
		},
		{
			name: "эмбеддер упал — индекс не тронут",
			prepare: func(h *harness) {
				h.embed.err = domain.ServiceUnavailable("модель недоступна")
			},
			wantErrCode: domain.CodeServiceUnavailable,
		},
		{
			name: "индекс недоступен — запись не объявлена успешной",
			prepare: func(h *harness) {
				h.index.upsertErr = domain.IndexUnavailable("Elasticsearch недоступен")
			},
			wantErrCode: domain.CodeIndexUnavailable,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, events, 500)
			c.prepare(h)

			_, err := h.run(t, rebuild.Scope{TenantID: rebuildTenant})
			if err == nil {
				t.Fatal("пересборка отчиталась успехом при отказе")
			}
			appErr, ok := domain.AsAppError(err)
			if !ok || appErr.Code != c.wantErrCode {
				t.Fatalf("вернулось %v, ожидался код %s", err, c.wantErrCode)
			}
			if c.wantReported {
				t.Errorf("в отчёт попал документ, которого в индексе нет")
			}
		})
	}
}

// TestСвёрткаОтвергаетБитоеСобытие — таблица отказов свёртки.
//
// Каждая строка — свой вид поломки lake, и молчаливый пропуск любой из них
// означал бы, что пересборка построила индекс по неполной картине и сказала
// «готово».
func TestСвёрткаОтвергаетБитоеСобытие(t *testing.T) {
	doc := fixtureDoc(1, "runbook")
	good := fullPipeline(t, doc, 2, 0)
	broken := good[0]
	broken.EventID = uuid.Nil

	foreignBody := good[len(good)-1]
	foreignBody.Payload = doc.payloadWith(t, domain.IndexStage, map[string]any{
		"doc_id": rebuildOther.String(),
	})

	noBlocks := good[len(good)-1]
	noBlocks.Payload = doc.payloadWith(t, domain.IndexStage, map[string]any{"blocks": []any{}})

	noKB := good[len(good)-1]
	noKB.Payload = doc.payloadWith(t, domain.IndexStage, map[string]any{"kb_id": ""})

	noIdentity := good[len(good)-1]
	noIdentity.Payload = doc.payloadWith(t, domain.IndexStage, map[string]any{"doc_id": ""})

	badVersion := good[len(good)-1]
	badVersion.Version = "raw.v7"

	notObject := good[len(good)-1]
	notObject.Payload = []byte(`["не объект"]`)

	// Один doc_id у двух тенантов — штатная ситуация по §3/§4 (загруженные
	// идентификаторы исторически не хэшировали тенанта).
	otherTenant := good[len(good)-1]
	otherTenant.TenantID = uuid.MustParse("dddddddd-0000-4000-8000-000000000001")

	cases := []struct {
		name   string
		events []domain.Event
	}{
		{"событие без event_id", []domain.Event{broken}},
		{"тело называет чужой doc_id", []domain.Event{foreignBody}},
		{"стадия index без блоков", []domain.Event{noBlocks}},
		{"стадия index без kb_id", []domain.Event{noKB}},
		{"стадия index не называет себя", []domain.Event{noIdentity}},
		{"неизвестная версия схемы", []domain.Event{badVersion}},
		{"тело не объект", []domain.Event{notObject}},
		{"один doc_id у двух тенантов", []domain.Event{good[0], otherTenant}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := rebuild.Fold(c.events); err == nil {
				t.Fatal("свёртка приняла битое событие и построила бы индекс по " +
					"неполной картине")
			}
		})
	}
}

// TestСвёрткаПропускаетНеподдерживаемыеСтадии — стадии конвейера до index.
//
// Тело collect/enrich может не повторять заголовок целиком (см. checkAgainst),
// и свёртка обязана это пережить: она их всё равно не читает.
func TestСвёрткаПропускаетНеподдерживаемыеСтадии(t *testing.T) {
	doc := fixtureDoc(1, "runbook")
	events := fullPipeline(t, doc, 2, 0)

	for _, stage := range []domain.Stage{
		domain.CollectStage, domain.NormalizeStage, domain.EnrichStage, domain.DeriveStage,
	} {
		t.Run(stage.String(), func(t *testing.T) {
			bare := lakeEvent(t, doc, stage, 40, []byte(`{"schema_version":"1.0"}`))
			plan, err := rebuild.Fold(sortLake(append(slices.Clone(events), bare)))
			if err != nil {
				t.Fatalf("Fold: %v", err)
			}
			if len(plan.Documents) != 1 || plan.Documents[0].ChunkCount != 2 {
				t.Fatalf("стадия %s изменила восстановленный документ: %v", stage, plan.Documents)
			}
		})
	}
}

// ── Вспомогательное ─────────────────────────────────────────────────────────

func documentIDs(documents []rebuild.Document) []string {
	out := make([]string, 0, len(documents))
	for _, document := range documents {
		out = append(out, document.ID())
	}
	return out
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func sortedAscending(values []string) bool {
	for i := 1; i < len(values); i++ {
		if strings.Compare(values[i-1], values[i]) > 0 {
			return false
		}
	}
	return true
}

func slices_Clone[T any](in []T) []T {
	return append([]T(nil), in...)
}
