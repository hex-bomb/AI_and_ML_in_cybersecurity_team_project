package arch

// ВОРОТА «УДАЛЕНИЕ» — §9: «документ, удалённый в источнике, исчезает из выдачи
// (проверяется тестом)», и §4: «удаление — через событие-надгробие
// (tombstone)».
//
// ГЛАВНОЕ ПРО ЭТОТ ФАЙЛ: НАДГРОБИЕ ЕСТЬ, И ОНО ДОХОДИТ ДО ИНДЕКСА. Путь от
// MarkDeleted до исчезновения документа из выдачи:
//
//	DocumentsRepository.MarkDeleted  → documents.deleted_at + задание стадии
//	                                   "delete" в очередь (это было и раньше);
//	Claim (GoStages)                 → берёт задание стадии delete;
//	worker.Stage.Delete              → стадия удаления (её не было: см. ниже);
//	indexing.Deleter.Delete          → надгробие в lake, затем Index.DeleteDocument
//	                                   по паре (tenant_id, document_id), затем
//	                                   закрытие задания под своим lease.
//
// Раньше обрывалось на третьем шаге: cmd/worker собирал только Convert и Index,
// и задание стадии delete доходило до default в worker.stage. Lake об удалении
// тоже молчал — CHECK ck_raw_events_stage допускал ровно пять стадий §4.
// TestНадгробиеУдаленияВКонвейере был маркером этого долга и обязан был
// упасть вместе с ним; теперь он проверяет, что надгробие ЕСТЬ и ПОДКЛЮЧЕНО, и
// падает, если его снова вынут.
//
// Честность вычисления «что исчезнет». Ворот не выбирает удаляемые документы
// сам: он берёт ТЕЛО ушедшего _delete_by_query и применяет его к корпусу тем
// же матчером. То есть «удалённый документ исчезает из выдачи» здесь означает
// «документ, выбранный НАСТОЯЩИМ запросом удаления, больше не матчит
// настоящий фильтр прав». Настоящий кластер проверяет то же самое отдельно
// (TestУдалениеДокументаИсчезаетИзВыдачиНаКластере), а настоящий lake —
// TestНадгробиеУдаленияПроходитLake.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/elastic/go-elasticsearch/v9"
	"github.com/google/uuid"

	domain "github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/jobs/indexing"
	esadapter "github.com/kbsearch/kb/internal/store/es"
	pgadapter "github.com/kbsearch/kb/internal/store/pg"
)

// gateDeletionSchema — отдельная схема для ворот удаления: тесты
// internal/store/pg чистят свои таблицы, и общая база сделала бы два пакета
// зависящими от порядка запуска.
const gateDeletionSchema = "kb_gate_delete"

// gateDeleteQuery вынимает query.bool.filter из тела _delete_by_query.
func gateDeleteQuery(t *testing.T, body string) []any {
	t.Helper()
	var parsed map[string]any
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("разбор тела удаления: %v\n%s", err, body)
	}
	query, ok := parsed["query"].(map[string]any)
	if !ok {
		t.Fatalf("в теле удаления нет query: %s", body)
	}
	boolean, ok := query["bool"].(map[string]any)
	if !ok {
		t.Fatalf("в теле удаления нет query.bool: %s", body)
	}
	clauses, ok := boolean["filter"].([]any)
	if !ok || len(clauses) == 0 {
		t.Fatalf("в теле удаления нет query.bool.filter: %s", body)
	}
	return clauses
}

// gateSelectedBy — документы корпуса, выбранные клозой.
//
// Это тот же исполнитель клоз, что и у фильтра прав: одинаковые формы,
// одинаковый разбор, одно и то же правило «отсутствующее поле не матчит».
func gateSelectedBy(t *testing.T, clauses []any, corpus []gateDoc) []string {
	t.Helper()
	var out []string
	for _, doc := range corpus {
		if doc.Source != nil && gateESMatch(t, clauses, doc.Source) {
			out = append(out, doc.ID)
		}
	}
	slices.Sort(out)
	return out
}

// withoutDocs возвращает корпус без перечисленных документов.
//
// Это НЕ симуляция удаления: удаляемые документы выбраны настоящим запросом
// удаления (gateSelectedBy), и здесь лишь собирается «индекс после
// delete_by_query». Настоящий кластер делает ровно это же — и проверяется
// отдельно, на нём.
func withoutDocs(corpus []gateDoc, removed []string) []gateDoc {
	out := make([]gateDoc, 0, len(corpus))
	for _, doc := range corpus {
		if !slices.Contains(removed, doc.ID) {
			out = append(out, doc)
		}
	}
	return out
}

// TestУдалениеДокументаИсчезаетИзВыдачи — ворота §9 «Удаление», без кластера.
//
// ЧТО ПРОВЕРЯЕТСЯ, ПО ШАГАМ:
//
//  1. Документ виден своему читателю до удаления (иначе шаг 4 нечего проверять).
//  2. Index.DeleteDocument уходит в Elasticsearch с запросом, ограниченным
//     тенантом И документом.
//  3. Этот запрос выбирает РОВНО удаляемый документ и ничего больше: сосед по
//     контейнеру, личная заметка и документы другого тенанта остаются.
//  4. После удаления документ не виден НИКОМУ — ни владельцу контейнера, ни
//     админу, ни автору заметки.
//  5. Сосед по контейнеру остаётся видимым: удаление не должно уносить
//     лишнего.
func TestУдалениеДокументаИсчезаетИзВыдачи(t *testing.T) {
	fixture := gateFixtureWorld()
	cfg := gateConfig()
	wire := newGateWire(t)
	index := gateAdapter(t, wire)
	corpus := gateIndexCorpus(t, index, wire, cfg, fixture.corpus)

	target := "acme-platform-runbook"
	sibling := "acme-platform-draft"
	// Корпус нужен ради document_id удаляемого документа; сам состав корпуса
	// здесь не важен, поэтому результат записи не переиспользуется.
	visibleBefore := gateDeletionVisible(t, fixture, index, wire, cfg, corpus, "anna")

	// Шаг 1: до удаления документ обязан быть виден. Иначе «исчез из выдачи»
	// можно было бы выполнить, ничего не сделав.
	if !visibleBefore[target] {
		t.Fatalf("документ %s не виден до удаления: тест удаления ничего не доказывает", target)
	}
	if !visibleBefore[sibling] {
		t.Fatalf("сосед по контейнеру %s не виден до удаления: сравнение не с чем", sibling)
	}

	// Шаг 2: настоящее удаление.
	deleted, err := index.DeleteDocument(context.Background(),
		gateTenantAcme.String(), gateDocumentID(corpus, target))
	if err != nil {
		t.Fatalf("DeleteDocument: %s", gateErrText(err))
	}
	if deleted <= 0 {
		t.Errorf("DeleteDocument вернул %d: адаптер не сообщил об удалении ни одного чанка", deleted)
	}
	bodies := wire.deletes()
	if len(bodies) != 1 {
		t.Fatalf("в кластер ушло %d запросов удаления, ожидался 1", len(bodies))
	}

	// Шаг 3: запрос выбирает ровно целевой документ.
	selected := gateSelectedBy(t, gateDeleteQuery(t, bodies[0]), corpus)
	if !slices.Equal(selected, []string{target}) {
		t.Errorf("запрос удаления выбрал %v, ожидался ровно [%s]: удаление либо не нашло "+
			"цель, либо задело лишнее", selected, target)
	}

	// Шаги 4 и 5: выдача после удаления.
	after := withoutDocs(corpus, selected)
	for _, reader := range []string{"anna", "boris", "elena", "dmitry", "ghost", "zoe"} {
		visible := gateDeletionVisible(t, fixture, index, wire, cfg, after, reader)
		if visible[target] {
			t.Errorf("утечка после удаления: документ %s всё ещё виден пользователю %s", target, reader)
		}
	}
	if visible := gateDeletionVisible(t, fixture, index, wire, cfg, after, "anna"); !visible[sibling] {
		t.Errorf("после удаления %s исчез и сосед по контейнеру %s: delete_by_query задел лишнее",
			target, sibling)
	}
}

// TestУдалениеНеТрогаетДокументыДругогоТенанта — tenant_id в удалении.
//
// Один и тот же document_id у двух тенантов — штатная ситуация (doc_id §4
// хэширует tenant, но рукописные и загруженные id исторически не хэшировались).
// Запрос удаления ограничен ТОЛЬКО тенантом: без term по tenant_id удаление
// документа Acme снесло бы одноимённый документ Globex.
func TestУдалениеНеТрогаетДокументыДругогоТенанта(t *testing.T) {
	cfg := gateConfig()
	wire := newGateWire(t)
	index := gateAdapter(t, wire)

	// Два документа с одинаковым document_id в разных тенантах.
	const sharedID = "doc-shared"
	twin := gateDocFixture("twin-in-globex", gateTenantGlobex, sharedID,
		domain.ContainerRights(gateGlobexForeign), "одноимённый документ другого тенанта")
	corpus := gateIndexCorpus(t, index, wire, cfg, []gateDoc{
		gateDocFixture("target-in-acme", gateTenantAcme, sharedID,
			domain.ContainerRights(gateAcmePlatform), "удаляемый документ"),
		twin,
	})

	if _, err := index.DeleteDocument(context.Background(),
		gateTenantAcme.String(), sharedID); err != nil {
		t.Fatalf("DeleteDocument: %s", gateErrText(err))
	}
	bodies := wire.deletes()
	if len(bodies) != 1 {
		t.Fatalf("в кластер ушло %d запросов удаления, ожидался 1", len(bodies))
	}
	selected := gateSelectedBy(t, gateDeleteQuery(t, bodies[0]), corpus)
	if !slices.Equal(selected, []string{"target-in-acme"}) {
		t.Errorf("запрос удаления выбрал %v: документ другого тенанта с тем же "+
			"document_id не должен попадать под удаление", selected)
	}
}

// TestМатрицаПравОстаётсяЧистойПослеУдаления — права не должны «разъехаться»
// после того, как часть корпуса исчезла.
//
// Тот же полный перебор «пользователь × документ», но уже на индексе после
// удаления. Права строятся из событий и могут пересчитываться независимо от
// удалений, поэтому совпадение после удаления — не следствие, а отдельное
// утверждение.
func TestМатрицаПравОстаётсяЧистойПослеУдаления(t *testing.T) {
	fixture := gateFixtureWorld()
	cfg := gateConfig()
	wire := newGateWire(t)
	index := gateAdapter(t, wire)
	corpus := gateIndexCorpus(t, index, wire, cfg, fixture.corpus)

	removed := []string{"acme-platform-runbook", "acme-note-anna", "globex-foreign"}
	after := withoutDocs(corpus, removed)
	if len(after) != len(corpus)-len(removed) {
		t.Fatalf("после удаления осталось %d документов, ожидалось %d", len(after), len(corpus)-len(removed))
	}

	matrix := runACLMatrix(t, &gateQueryer{world: fixture.world},
		fixture.users, after, index, wire, cfg)
	for _, cell := range matrix.Cells {
		if slices.Contains(removed, cell.Document) {
			t.Errorf("удалённый документ %s попал в матрицу: %s", cell.Document, cell.User)
		}
	}
	if leaks := matrix.Leaks(); len(leaks) > 0 {
		t.Errorf("УТЕЧКА ПРАВ ПОСЛЕ УДАЛЕНИЯ (§9): %d документов.\n%s", len(leaks), gateReport(leaks))
	}
	if misses := matrix.Misses(); len(misses) > 0 {
		t.Errorf("пропущенные результаты после удаления: %d документов.\n%s",
			len(misses), gateReport(misses))
	}
}

// gateDocumentID достаёт document_id документа корпуса.
func gateDocumentID(corpus []gateDoc, id string) string {
	for _, doc := range corpus {
		if doc.ID == id {
			return doc.Document
		}
	}
	return ""
}

// gateDeletionVisible — краткая видимость одного пользователя по корпусу.
func gateDeletionVisible(t *testing.T, fixture *gateFixture, index *esadapter.Index,
	wire *gateWire, cfg domain.IndexingConfig, corpus []gateDoc, name string) map[string]bool {
	t.Helper()
	var principal domain.Principal
	for _, user := range fixture.users {
		if user.Name == name {
			principal = user.Principal
		}
	}
	scope, _ := gateResolveScope(t, fixture.world, principal)
	return gateVisible(t, gateFilterOf(t, index, wire, scope, cfg), corpus)
}

// ── Надгробие удаления: оно есть и оно подключено ─────────────────────────

// TestНадгробиеУдаленияВКонвейере — ворота §4 «удаление — через
// событие-надгробие» и §9 «документ, удалённый в источнике, исчезает из выдачи».
//
// БЫЛО: TestНадгробиеУдаленияНетВКонвейере — маркер долга, который ПРОХОДИЛ,
// пока надгробия нет, и обязан был упасть вместе с долгом. Теперь долг закрыт,
// и на его месте стоит обратное вороно: оно падает, если надгробие вынут, из
// lake, из конверта или из воркера. Удаление старого теста без замены означало бы
// стереть ворота, поэтому здесь стоит проверка противоположного утверждения, а не
// её отсутствие.
//
// ЧТО ПРОВЕРЯЕТСЯ, ПО ШАГАМ (каждый шаг — отдельная подпроверка, и каждый сам
// по себе роняет тест):
//
//  1. КОНВЕРТ. Набор стадий lake содержит delete, и событие этой стадии
//     проходит Validate. Пока стадии не было, lake был непроницаем для удаления.
//  2. СХЕМА. Последняя редакция ck_raw_events_stage (0002, а затем 0003)
//     допускает delete, и допускает РОВНО domain.AllStages() — ни больше, ни
//     меньше.
//  3. ВОРКЕР. domain.GoStages() содержит стадию delete, worker.Stage объявляет
//     Delete, а worker.stage вызывает её по имени стадии.
//  4. СБОРКА. cmd/worker собирает удалитель и отдаёт его циклу: без этого
//     пункты 1–3 были бы правдой на словах, а чанки удалённого документа снова
//     остались бы в индексе.
//  5. ВЫЗОВ ИНДЕКСА. Стадия удаления зовёт Index.DeleteDocument по паре
//     (tenant_id, document_id) — единственный путь, которым чанки покидают
//     индекс, и он же изоляция тенанта.
func TestНадгробиеУдаленияВКонвейере(t *testing.T) {
	t.Run("КонвертДопускаетСтадиюУдаления", func(t *testing.T) {
		if !domain.DeleteStage.Valid() {
			t.Fatal("стадия delete не входит в domain.AllStages: lake её не примет, " +
				"и удаление снова останется без следа в источнике правды")
		}
		if err := (domain.Event{
			Version:  domain.RawEventVersion,
			EventID:  uuid.New(),
			TenantID: gateTenantAcme,
			Stage:    domain.DeleteStage,
			DocID:    "doc-1",
		}).Validate(); err != nil {
			t.Errorf("событие стадии delete не проходит Validate: %v", err)
		}
	})

	t.Run("ОграничениеLakeДопускаетУдаление", func(t *testing.T) {
		// Последняя РЕДАКЦИЯ ограничения, а не текст 0002: набор стадий
		// меняется миграцией (0003), и применённую 0002 не переписывают.
		// Читать надо ту редакцию, которая действует, иначе ворота проверяли бы
		// снятое ограничение.
		line := gateStageConstraint(t, "ck_raw_events_stage")
		for _, stage := range domain.AllStages() {
			if !strings.Contains(line, "'"+stage.String()+"'") {
				t.Errorf("стадия %s есть в конверте, но не в ограничении "+
					"ck_raw_events_stage: %s", stage, line)
			}
		}
		for _, quoted := range []string{"'tombstone'", "'remove'"} {
			if strings.Contains(line, quoted) {
				t.Errorf("в ограничении lake появилась стадия-синоним %s: надгробие "+
					"объявлено второй раз под другим именем", quoted)
			}
		}
	})

	t.Run("ВоркерОбслуживаетСтадиюУдаления", func(t *testing.T) {
		if !slices.Contains(domain.GoStages(), domain.StageDelete) {
			t.Errorf("в domain.GoStages() нет стадии delete: Claim её не заберёт, "+
				"и задание удаления останется в очереди навсегда (%v)", domain.GoStages())
		}
		// Интерфейс стадии и его разборчик по имени обязаны знать про delete
		// обе стороны: иначе Claim отдаст задание, которое цикл выполнить не
		// умеет, и оно уйдёт в ошибку границы.
		stage := gateFilesMentioning(t, []string{"internal/jobs/worker"}, "Delete(ctx context.Context, lease domain.JobLease) error")
		if len(stage) == 0 {
			t.Error("worker.Stage больше не объявляет Delete: цикл не сможет " +
				"обслужить задание стадии delete")
		}
		routed := gateFilesMentioning(t, []string{"internal/jobs/worker"}, "domain.StageDelete:")
		if len(routed) == 0 {
			t.Error("worker.stage не вызывает Delete по имени стадии: задание " +
				"удаления попадёт в ветку «стадия не обслуживается этим воркером»")
		}
	})

	t.Run("СборкаВоркераОтдаётУдалениеЦиклу", func(t *testing.T) {
		wired := gateFilesMentioning(t, []string{"cmd/worker"}, "NewDeleter")
		if len(wired) == 0 {
			t.Error("cmd/worker не собирает стадию удаления: задание стадии delete " +
				"будет забрано очередью и не выполнено")
		}
	})

	t.Run("СтадияУдаленияЗовётИндексПоПарам", func(t *testing.T) {
		consumers := gateFilesMentioning(t, []string{"internal/jobs/indexing"}, "DeleteDocument")
		if len(consumers) == 0 {
			t.Error("стадия удаления не зовёт Index.DeleteDocument: чанки " +
				"удалённого документа останутся в индексе навсегда")
		}
		// Индекс удаляет по паре (tenant_id, document_id). Удаление только по
		// document_id снесло бы одноимённый документ другого тенанта — тот
		// случай, который запрещает §3/§4.
		source, err := os.ReadFile(filepath.Join("..", "jobs", "indexing", "delete.go"))
		if err != nil {
			t.Fatalf("чтение стадии удаления: %v", err)
		}
		text := string(source)
		for _, need := range []string{
			"lease.TenantID.String()", "lease.DocumentID.String()",
		} {
			if !strings.Contains(text, need) {
				t.Errorf("в стадии удаления нет %s: ограничение по тенанту или по "+
					"документу ослаблено", need)
			}
		}
	})
}

// TestНадгробиеУдаленияПроходитLake — lake ПРИНИМАЕТ и возвращает надгробие.
//
// Проверяется на настоящей PostgreSQL, потому что без неё утверждение «lake
// берёт удаление» остаётся словами: CHECK ck_raw_events_stage живёт в схеме, а
// не в Go-коде, и написанное здесь ограничение (миграция 0003) — единственное
// место, где стадия delete может быть отвергнута.
//
// Отдельно проверяется идемпотентность lake: повтор того же надгробия не
// добавляет ни строки в raw_events, ни строки в реестр raw_event_ids. Это и
// есть «ключ doc_id + content_hash» (§4) на настоящих данных.
//
// БЕЗ БАЗЫ ТЕСТ ПРОПУСКАЕТСЯ С ЯВНЫМ СООБЩЕНИЕМ — молча пропущенная проверка
// читалась бы как «надгробие lake принимает».
func TestНадгробиеУдаленияПроходитLake(t *testing.T) {
	if testing.Short() {
		t.Skip("интеграционный тест: нужна PostgreSQL (запустите без -short)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)

	db := gateOpenLiveSchema(t, ctx, gateDeletionSchema,
		"0001_initial_schema.up.sql", "0002_lake_acl.up.sql", "0003_delete_stage.up.sql")
	lake := pgadapter.NewLake(db)

	lease := domain.JobLease{
		JobID:      uuid.MustParse("77777777-7777-4777-8777-777777777777"),
		TenantID:   gateTenantAcme,
		DocumentID: uuid.MustParse("33333333-3333-4333-8333-333333333333"),
		Kind:       domain.KindDelete,
		Stage:      domain.StageDelete,
	}
	occurredAt := lakeBaseTime
	event, err := indexing.TombstoneEvent(lease, occurredAt)
	if err != nil {
		t.Fatalf("TombstoneEvent: %v", err)
	}

	// Первая запись. Ошибка здесь означала бы ровно то, что было до миграции
	// 0003: lake отверг удаление.
	if err := lake.Publish(ctx, event); err != nil {
		t.Fatalf("lake отверг надгробие удаления: %v", err)
	}
	// Повтор того же надгробия — no-op.
	if err := lake.Publish(ctx, event); err != nil {
		t.Fatalf("повтор надгробия вернул ошибку: %v", err)
	}

	if got := gateCountLakeEvents(ctx, db, event.EventID); got != 1 {
		t.Errorf("в raw_events %d записей надгробия, ожидалась 1: повтор стадии "+
			"не должен растить lake", got)
	}

	// Круг обратно: то, что записано, обязано читаться портом EventStore.
	events, err := lake.Read(ctx, domain.EventQuery{TenantID: lease.TenantID, Limit: 16})
	if err != nil {
		t.Fatalf("Lake.Read: %v", err)
	}
	found := false
	for _, read := range events {
		if read.EventID != event.EventID {
			continue
		}
		found = true
		if read.Stage != domain.DeleteStage {
			t.Errorf("прочитанное событие имеет стадию %q вместо delete", read.Stage)
		}
		if read.DocID != event.DocID || read.ContentHash != event.ContentHash {
			t.Errorf("прочитанное надгробие адресовано %s/%s вместо %s/%s",
				read.DocID, read.ContentHash, event.DocID, event.ContentHash)
		}
	}
	if !found {
		t.Fatalf("надгробие записано, но lake его не отдал: %d событий прочитано", len(events))
	}
}

// gateCountLakeEvents считает записи lake по event_id — напрямую, а не через
// порт, потому что порт отдаёт страницу, а вопрос здесь «сколько строк»,
// то есть свойство идемпотентности, а не выдачи.
func gateCountLakeEvents(ctx context.Context, db *pgadapter.DB, eventID uuid.UUID) int {
	rows, err := db.Query(ctx, "SELECT count(*) FROM raw_events WHERE event_id = $1", eventID)
	if err != nil {
		return -1
	}
	defer rows.Close()
	if !rows.Next() {
		return -1
	}
	var count int
	if err := rows.Scan(&count); err != nil {
		return -1
	}
	return count
}

// gateFilesMentioning ищет упоминания идентификатора в нетестовых .go файлах
// указанных КОРНЕВЫХ каталогов модуля (internal, cmd, api, eval).
//
// Пути разрешаются от корня модуля (../../), потому что go test запускает
// пакет из его каталога: путь «internal» отсюда не существует, Walk тихо
// ничего не находит, и проверка проходит вхолостую. Отсутствующий корень —
// ошибка, а не пустой результат: иначе отказ проверки неотличим от «долга
// закрыли».
func gateFilesMentioning(t *testing.T, roots []string, needle string) []string {
	t.Helper()
	var found []string
	for _, root := range roots {
		dir := filepath.Join(moduleRoot, root)
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("каталог %s не найден (%v): обход дерева молча ничего не проверяет",
				dir, err)
		}
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			if strings.Contains(string(raw), needle) {
				found = append(found, filepath.ToSlash(path))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("обход %s: %v", dir, err)
		}
	}
	slices.Sort(found)
	return found
}

// ── Тот же сценарий на настоящем кластере ───────────────────────────────────

// TestУдалениеДокументаИсчезаетИзВыдачиНаКластере — настоящее подтверждение
// ворот «Удаление» на настоящем Elasticsearch.
//
// БЕЗ КЛАСТЕРА ТЕСТ ПРОПУСКАЕТСЯ, И БЕЗ ЯВНОГО РАЗРЕШЕНИЯ — ТОЖЕ.
// Это единственный тест, который проверяет, что _delete_by_query на самом деле
// убирает чанки, а подставной кластер только повторяет его форму.
//
// ПОЧЕМУ НУЖЕН ЯВНЫЙ РАЗРЕШЕНИЙ (KB_GO_GATES_CLUSTER), А НЕ ОДНОГО
// ELASTICSEARCH_TEST_URL. Интеграционные тесты internal/store/es в своём
// cleanup вызывают dropProfileIndices, который УДАЛЯЕТ ВСЕ индексы kb-chunks-*
// без разбора профилей. Пакеты при `go test ./...` идут параллельно, и живой
// прогон этих ворот в одном прогоне с ними теряет свой индекс между записью и
// поиском. Наблюдалось ровно это: «до удаления найдено 0 документов».
//
// Запуск на отдельном стенде:
//
//	KB_GO_GATES_CLUSTER=1 ELASTICSEARCH_TEST_URL=http://127.0.0.1:59200 \
//	  go test -count=1 -run TestУдалениеДокументаИсчезаетИзВыдачиНаКластере ./internal/arch/
//
// Без этой переменной ворота удаления на настоящем индексе НЕ ВЫПОЛНЯЮТСЯ.
func TestУдалениеДокументаИсчезаетИзВыдачиНаКластере(t *testing.T) {
	if testing.Short() {
		t.Skip("интеграционный тест: нужен Elasticsearch (запустите без -short)")
	}
	if os.Getenv("KB_GO_GATES_CLUSTER") == "" {
		t.Skip("ворота удаления на живом кластере требуют явного разрешения " +
			"KB_GO_GATES_CLUSTER и ОТДЕЛЬНОГО стенда: интеграционные тесты " +
			"internal/store/es удаляют в своём cleanup все индексы kb-chunks-*, " +
			"и в общем прогоне индекс ворот пропадает между записью и поиском. " +
			"ПРОПУЩЕНО")
	}
	address := gateClusterURL()
	client, err := elasticsearch.New(elasticsearch.WithAddresses(address))
	if err != nil {
		t.Skipf("клиент Elasticsearch не собрался для %s: %v", address, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	info, err := client.Info(client.Info.WithContext(ctx))
	if err != nil {
		t.Skipf("Elasticsearch недоступен на %s: %v. Ворота «Удаление» на настоящем "+
			"индексе ПРОПУЩЕНЫ", address, err)
	}
	if info.IsError() {
		status := info.Status()
		_ = info.Body.Close()
		t.Skipf("Elasticsearch на %s ответил %s. Ворота «Удаление» на настоящем "+
			"индексе ПРОПУЩЕНЫ", address, status)
	}
	_ = info.Body.Close()

	cfg := gateConfig()
	index, err := esadapter.NewWithClient(client)
	if err != nil {
		t.Fatalf("адаптер индекса: %v", err)
	}
	t.Cleanup(func() { _ = index.Close(context.Background()) })

	// Индекс профиля ворот удаляется после теста: он общий с другими
	// пакетами по имени, если совпадёт профиль.
	t.Cleanup(func() {
		_, _ = client.Indices.Delete([]string{cfg.IndexName()},
			client.Indices.Delete.WithContext(context.Background()),
			client.Indices.Delete.WithIgnoreUnavailable(true))
	})

	// Уникальный маркер текста: запрос и документ несут одно слово, поэтому
	// находятся только друг друга и не цепляют чужие чанки профиля.
	const marker = "пробниковыймаркерворотудаления"
	docs := []gateDoc{
		gateDocFixture("live-target", gateTenantAcme, "doc-live-1",
			domain.ContainerRights(gateAcmePlatform), marker),
		gateDocFixture("live-sibling", gateTenantAcme, "doc-live-2",
			domain.ContainerRights(gateAcmePlatform), marker),
	}
	corpus := gateIndexCorpusLive(t, index, cfg, docs)
	if corpus[0].Source == nil {
		t.Fatal("корпус не записан")
	}

	scope := domain.AccessScope{
		TenantID:   gateTenantAcme.String(),
		KBID:       gateKBID,
		SpaceIDs:   []string{gateAcmePlatform},
		Principals: []string{domain.UserPrincipal("anna")},
	}
	before := gateLiveSearch(t, index, cfg, scope, marker)
	if len(before) != 2 {
		t.Fatalf("до удаления найдено %d документов, ожидалось 2: %v", len(before), before)
	}

	if _, err := index.DeleteDocument(ctx, gateTenantAcme.String(), "doc-live-1"); err != nil {
		t.Fatalf("DeleteDocument: %s", gateErrText(err))
	}

	after := gateLiveSearch(t, index, cfg, scope, marker)
	for _, id := range after {
		if id == "doc-live-1" {
			t.Errorf("удалённый документ всё ещё в выдаче: %v", after)
		}
	}
	if len(after) != 1 || after[0] != "doc-live-2" {
		t.Errorf("после удаления выдача %v, ожидался только сосед doc-live-2", after)
	}
}

// gateClusterURL — адрес кластера для живых ворот.
func gateClusterURL() string {
	if custom := strings.TrimSpace(os.Getenv("ELASTICSEARCH_TEST_URL")); custom != "" {
		return custom
	}
	// Порт 59200 публикует compose.yaml, чтобы не занимать стандартный 9200.
	return "http://127.0.0.1:59200"
}

// gateLiveSearch возвращает отсортированные document_id найденного.
func gateLiveSearch(t *testing.T, index *esadapter.Index, cfg domain.IndexingConfig,
	scope domain.AccessScope, query string) []string {
	t.Helper()
	hits, err := index.KeywordSearchScoped(context.Background(), query, scope, cfg, 10)
	if err != nil {
		t.Fatalf("KeywordSearchScoped: %s", gateErrText(err))
	}
	out := make([]string, 0, len(hits))
	for _, hit := range hits {
		out = append(out, hit.DocumentID)
	}
	slices.Sort(out)
	return out
}

// gateIndexCorpusLive пишет корпус настоящим UpsertChunks в живой кластер.
func gateIndexCorpusLive(t *testing.T, index *esadapter.Index, cfg domain.IndexingConfig,
	docs []gateDoc) []gateDoc {
	t.Helper()
	ctx := context.Background()
	out := make([]gateDoc, 0, len(docs))
	for _, doc := range docs {
		written, err := index.UpsertChunks(ctx, cfg, []domain.Chunk{gateChunk(cfg, doc)})
		if err != nil {
			t.Fatalf("UpsertChunks(%s) в живой кластер: %s", doc.ID, gateErrText(err))
		}
		if written != 1 {
			t.Fatalf("UpsertChunks(%s) записал %d чанков, ожидался 1", doc.ID, written)
		}
		indexed := doc
		indexed.Source = map[string]any{
			"document_id": doc.Document,
			"tenant_id":   doc.TenantID,
		}
		out = append(out, indexed)
	}
	return out
}
