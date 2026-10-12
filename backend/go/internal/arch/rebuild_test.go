package arch

// ВОРОТА «ВОССТАНОВЛЕНИЕ» — §1 (принцип 1: lake — источник правды, всё
// остальное пересобираемо), §4 («любой индекс можно удалить и воссоздать из
// lake»), §9 («индекс хотя бы раз полностью пересобран из lake по учебной
// тревоге»).
//
// ГЛАВНОЕ ПРО ЭТОТ ФАЙЛ: ПЕРЕСБОРКИ ИНДЕКСА БОЛЬШЕ НЕТ ТОЛЬКО НА СЛОВАХ.
// Файл начинался воротами ПРЕДПОСЫЛОК, потому что production-свертки не
// существовало: в дереве был порт ports.EventStore, его реализация Lake и ни
// одного потребителя. Теперь потребитель есть — internal/jobs/rebuild, сценарий
// пересборки, и он ПРОВЕРЯЕТСЯ ЗДЕСЬ НАСТОЯЩИМ, а не зеркалом:
//
//	rebuild.Fold        — production-свертка ленты в план пересборки;
//	ports.EventStore    — порт lake, объявленный потребителем (§8);
//	pgadapter.NewLake   — настоящая реализация обоих швов над raw_events;
//	indexing.TombstoneEvent — настоящий писатель надгробий.
//
// Зеркало production-кода в этом файле осталось ровно одно, и оно не про
// правило: фикстура событий (gateLakeEvent). Это данные, а не реализация —
// зеркалом была бы свёртка, написанная здесь же, и её больше нет.
//
// ЧТО ЗДЕСЬ ПРОВЕРЯЕТСЯ:
//
//  1. СВЁРТКА ЖИВЁТ В КОДЕ И ПОДКЛЮЧЕНА. Потребитель порта lake есть, он не
//     ходит в базу мимо порта, команда пересборки собрана, а lake собирается в
//     composition root РОВНО ОДИН РАЗ (TestПересборкиИндексаВКодеНет).
//  2. СХЕМА ДОСТАТОЧНА. События lake несут всё, что нужно для восстановления
//     документа, и стадии конвейера в конверте совпадают со стадиями,
//     допущенными ограничением raw_events.
//  3. СОБЫТИЯ ДЕТЕРМИНИРОВАНЫ. event_id — UUIDv5 от (версия, тенант, стадия,
//     doc_id, content_hash): повтор стадии даёт тот же id, поэтому lake не
//     растёт от ретраев и повторный проход пересборки читает то же самое.
//  4. ПОРЯДОК ЧТЕНИЯ ДЕТЕРМИНИРОВАН. Порт обещает порядок по
//     (occurred_at, event_id), и пересборка обязана быть воспроизводимой при
//     любом параллелизме записи.
//  5. ПОВТОР ИДЕМПОТЕНТЕН. Дублирование событий в потоке не меняет результат
//     пересборки.
//  6. УДАЛЕНИЕ ЧИТАЕТСЯ. Надгробие, записанное настоящей стадией delete,
//     убирает документ из пересборки — иначе удалённый в источнике документ
//     вернулся бы в выдачу после восстановления (§9).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	domain "github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/jobs/indexing"
	"github.com/kbsearch/kb/internal/jobs/rebuild"
	"github.com/kbsearch/kb/internal/ports"
	pgadapter "github.com/kbsearch/kb/internal/store/pg"
)

// ── Фикстура lake ───────────────────────────────────────────────────────────

// lakeBaseTime — начало ленты времени фикстуры. Фиксировано, чтобы порядок
// чтения не зависел от now() и прогоны были сопоставимы. Объявлено здесь, а не
// в gates_test.go, потому что используется в воротах удаления (deletion_test.go).
var lakeBaseTime = time.Date(2026, time.November, 3, 9, 0, 0, 0, time.UTC)

// Идентификаторы фикстурного корпуса.
//
// UUID, а не «doc-1»: пересборка разбирает doc_id, kb_id и version_id в UUID
// (ParsedDocument требует их именно такими), и фикстура с не-UUID заставила бы
// врать: ворота проверяли бы отказ на выдуманных данных вместо поведения
// свёртки.
var (
	gateLakeKB       = uuid.MustParse("e0000000-0000-4000-8000-000000000001")
	gateDoc1         = uuid.MustParse("d0000000-0000-4000-8000-000000000001")
	gateDoc2         = uuid.MustParse("d0000000-0000-4000-8000-000000000002")
	gateDoc3         = uuid.MustParse("d0000000-0000-4000-8000-000000000003")
	gateVersionDoc1  = uuid.MustParse("f0000000-0000-4000-8000-000000000001")
	gateVersionDoc2  = uuid.MustParse("f0000000-0000-4000-8000-000000000002")
	gateVersionDoc3  = uuid.MustParse("f0000000-0000-4000-8000-000000000003")
	gateVersionDoc1b = uuid.MustParse("f0000000-0000-4000-8000-00000000000b")
)

// gateLakeEvent собирает настоящее событие lake через domain.NewEvent.
//
// Payload — JSON-объект ровно той формы, которую читает пересборка
// (internal/jobs/rebuild/payload.go): идентификация документа, права и блоки.
// Это ДАННЫЕ фикстуры, а не повтор правила: правило читается в пересборке и
// применяется её кодом, а здесь собирается лишь то, что в lake положит стадия.
func gateLakeEvent(t *testing.T, tenant uuid.UUID, stage domain.Stage,
	docID, versionID, contentHash string, minute int, extra map[string]any) domain.Event {
	t.Helper()
	payload := map[string]any{
		"schema_version": "1.0",
		"doc_id":         docID,
		"tenant_id":      tenant.String(),
		"stage":          stage.String(),
		"content_hash":   contentHash,
		"kb_id":          gateLakeKB.String(),
		"version_id":     versionID,
		"title":          "документ " + docID,
		"space_id":       gateAcmePlatform,
		"acl":            []string{},
		"blocks":         gateLakeBlocks(docID),
		"chunk_count":    3,
	}
	for key, value := range extra {
		payload[key] = value
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("сборка payload %s: %v", stage, err)
	}
	event, err := domain.NewEvent(domain.NewEventParams{
		TenantID:    tenant,
		Stage:       stage,
		DocID:       docID,
		ContentHash: contentHash,
		Payload:     raw,
		OccurredAt:  lakeBaseTime.Add(time.Duration(minute) * time.Minute),
	})
	if err != nil {
		t.Fatalf("NewEvent(%s, %s): %v", stage, docID, err)
	}
	return event
}

// gatePipelineStages — стадии прохода конвейера документа, без надгробия.
//
// Отличие от domain.AllStages обязательно: delete не стадия прохода, а отдельный
// факт, приходящий в любое время (см. комментарий к AllStages). Фикстура,
// которая прошлась бы по всем шести стадиям, положила бы надгробие последним
// событием каждого документа — и пересборка справедливо убрала бы из выдачи
// всё, что фикстура якобы проиндексировала.
func gatePipelineStages() []domain.Stage {
	stages := make([]domain.Stage, 0, len(domain.AllStages()))
	for _, stage := range domain.AllStages() {
		if stage == domain.DeleteStage {
			continue
		}
		stages = append(stages, stage)
	}
	return stages
}

// gateLakeBlocks собирает блоки фикстурного документа.
//
// Идентификаторы блоков детерминированы по содержимому: chunk_id считается от
// версии документа, а повторный прогон обязан получить те же block_id.
func gateLakeBlocks(docID string) []map[string]any {
	return []map[string]any{
		{
			"block_id":     uuid.NewSHA1(uuid.Nil, []byte(docID+"#1")).String(),
			"position":     0,
			"kind":         "paragraph",
			"text":         "учебный текст для пересборки " + docID + ", часть 1",
			"section_path": []string{"Раздел"},
		},
		{
			"block_id":     uuid.NewSHA1(uuid.Nil, []byte(docID+"#2")).String(),
			"position":     1,
			"kind":         "paragraph",
			"text":         "учебный текст для пересборки " + docID + ", часть 2",
			"section_path": []string{"Раздел"},
		},
	}
}

// gateLakeFixture строит ленту: три документа, по стадии на каждую из
// domain.AllStages, плюс вторую версию одного документа (новый content_hash —
// новая глава §7).
func gateLakeFixture(t *testing.T) []domain.Event {
	t.Helper()
	docs := []struct {
		id      string
		version string
		hash    string
		start   int
	}{
		{gateDoc1.String(), gateVersionDoc1.String(), "sha256:aaa", 0},
		{gateDoc2.String(), gateVersionDoc2.String(), "sha256:bbb", 10},
		{gateDoc3.String(), gateVersionDoc3.String(), "sha256:ccc", 20},
	}
	var events []domain.Event
	for _, doc := range docs {
		for i, stage := range gatePipelineStages() {
			events = append(events, gateLakeEvent(t, gateTenantAcme, stage,
				doc.id, doc.version, doc.hash, doc.start+i, nil))
		}
	}
	// Вторая редакция doc-1: тот же документ, новый content_hash. Пересборка
	// обязана взять последнюю по времени версию, а не первую и не обе.
	for i, stage := range gatePipelineStages() {
		events = append(events, gateLakeEvent(t, gateTenantAcme, stage,
			gateDoc1.String(), gateVersionDoc1b.String(), "sha256:ddd", 100+i,
			map[string]any{"chunk_count": 5}))
	}
	return gateSortLake(events)
}

// gateSortLake приводит ленту к каноническому порядку чтения: (occurred_at,
// event_id). Именно его обещает порт EventStore.
func gateSortLake(events []domain.Event) []domain.Event {
	out := make([]domain.Event, len(events))
	copy(out, events)
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].OccurredAt.Equal(out[j].OccurredAt) {
			return out[i].OccurredAt.Before(out[j].OccurredAt)
		}
		return out[i].EventID.String() < out[j].EventID.String()
	})
	return out
}

// ── Печатные формы результата пересборки ────────────────────────────────────

// gateFold — свёртка НАСТОЯЩЕГО сценария пересборки.
//
// Обёртка нужна только для сообщений об ошибках: сам разбор поведения идёт в
// телах тестов. Здесь нет ни строки правила — всё поведение в rebuild.Fold.
func gateFold(t *testing.T, events []domain.Event) rebuild.Plan {
	t.Helper()
	plan, err := rebuild.Fold(events)
	if err != nil {
		t.Fatalf("пересборка индекса из lake: %v", err)
	}
	return plan
}

// gateDocumentIDs — document_id восстановленных документов в порядке плана.
func gateDocumentIDs(plan rebuild.Plan) []string {
	out := make([]string, 0, len(plan.Documents))
	for _, document := range plan.Documents {
		out = append(out, document.ID())
	}
	return out
}

// gatePlanSummary делает план сравнимым в ошибке.
func gatePlanSummary(plan rebuild.Plan) string {
	parts := make([]string, 0, len(plan.Documents))
	for _, document := range plan.Documents {
		parts = append(parts, fmt.Sprintf("%s@%s(chunks=%d)",
			document.ID(), document.ContentHash, document.ChunkCount))
	}
	summary := "[" + strings.Join(parts, ", ") + "] удалены=" +
		"[" + strings.Join(plan.Deleted, ", ") + "]"
	return summary
}

// gateEventIDs — печатная форма набора событий.
func gateEventIDs(events []domain.Event) string {
	out := make([]string, 0, len(events))
	for _, event := range events {
		out = append(out, event.Stage.String()+"/"+event.DocID)
	}
	sort.Strings(out)
	return "[" + strings.Join(out, ", ") + "]"
}

// ── Ворота восстановления ───────────────────────────────────────────────────

// TestПересборкаИзLakeВоспроизводима — ядро ворот «Восстановление».
//
// Четыре утверждения, каждое — отдельный вид поломки:
//
//	две свёртки одного и того же потока дают одно и то же;
//	поток с продублированными событиями даёт то же самое (идемпотентность §4);
//	поток, перетрясенный и приведённый к каноническому порядку, даёт то же
//	самое (порядок чтения детерминирован);
//	в пересборку попадают ровно те документы, что дошли до стадии index, и
//	берётся последняя редакция каждого из них.
func TestПересборкаИзLakeВоспроизводима(t *testing.T) {
	events := gateLakeFixture(t)

	first := gateFold(t, events)
	second := gateFold(t, events)
	if gatePlanSummary(first) != gatePlanSummary(second) {
		t.Fatalf("две пересборки одного потока разошлись:\n %s\n %s",
			gatePlanSummary(first), gatePlanSummary(second))
	}

	// Повтор стадии: lake отдаёт каждое событие дважды. Результат обязан
	// совпасть, иначе повтор стадии меняет индекс.
	doubled := make([]domain.Event, 0, 2*len(events))
	doubled = append(doubled, events...)
	doubled = append(doubled, events...)
	if got := gateFold(t, gateSortLake(doubled)); gatePlanSummary(got) != gatePlanSummary(first) {
		t.Errorf("повтор событий изменил пересборку:\n было %s\n стало %s",
			gatePlanSummary(first), gatePlanSummary(got))
	}

	// Порядок доставки не должен влиять на результат: перетряхиваем поток и
	// приводим его к порядку, который обещает порт.
	shuffled := make([]domain.Event, len(events))
	copy(shuffled, events)
	sort.SliceStable(shuffled, func(i, j int) bool {
		// Обратный порядок по времени: худший случай для потребителя.
		return shuffled[i].OccurredAt.After(shuffled[j].OccurredAt)
	})
	if got := gateFold(t, gateSortLake(shuffled)); gatePlanSummary(got) != gatePlanSummary(first) {
		t.Errorf("пересборка зависит от порядка доставки:\n канон %s\n перетрях %s",
			gatePlanSummary(first), gatePlanSummary(got))
	}

	// В пересборку попадает ровно то, что дошло до стадии index.
	if len(first.Documents) != 3 {
		t.Errorf("пересобрано %d документов, ожидалось 3: %s",
			len(first.Documents), gatePlanSummary(first))
	}
	ids := gateDocumentIDs(first)
	if !slices.IsSorted(ids) {
		t.Errorf("план не отсортирован по doc_id: %v — порядок обхода карты просочился "+
			"в результат, и две пересборки дали бы разный порядок записи в индекс", ids)
	}

	// Последняя редакция побеждает: у doc-1 их две, и пересборка обязана взять
	// вторую (chunk_count 5, а не 3).
	for _, document := range first.Documents {
		if document.ID() != gateDoc1.String() {
			continue
		}
		if document.ContentHash != "sha256:ddd" || document.ChunkCount != 5 {
			t.Errorf("doc-1 восстановлен как %s (чанков %d), ожидалась версия sha256:ddd "+
				"с пятью чанками", document.ContentHash, document.ChunkCount)
		}
	}
}

// TestБезСтадииIndexПересборкаПуста — достаточность набора стадий.
//
// Если из lake вынуть стадию index, поисковый индекс из него не собирается: ни
// одного документа, потому что именно эта стадия и есть утверждение «эта версия
// документа в индексе». Это доказывает, что набор стадий не избыточен и что
// пересборка зависит от lake целиком, а не от части его.
func TestБезСтадииIndexПересборкаПуста(t *testing.T) {
	events := gateLakeFixture(t)
	var withoutIndex []domain.Event
	for _, event := range events {
		if event.Stage == domain.IndexStage {
			continue
		}
		withoutIndex = append(withoutIndex, event)
	}
	partial := gateFold(t, gateSortLake(withoutIndex))
	full := gateFold(t, events)

	if len(partial.Documents) != 0 {
		t.Fatalf("без стадии index пересборка дала %d документов (%v): документов, "+
			"которых не было в индексе, ставить туда нечем",
			len(partial.Documents), gatePlanSummary(partial))
	}
	if len(full.Documents) != 3 {
		t.Fatalf("с полной лентой пересобрано %d документов, ожидалось 3: %s",
			len(full.Documents), gatePlanSummary(full))
	}
}

// TestНадгробиеУбираетДокументИзПересборки — §9 «документ, удалённый в
// источнике, исчезает из выдачи» на ПЕРЕСБОРКЕ.
//
// Надгробие собирается настоящей стадией delete (indexing.TombstoneEvent), а
// читается настоящей пересборкой. Подделок ровно ноль: если стадия delete
// перестанет писать удаление так, как его читает пересборка, ворота упадут
// здесь, а не на учебной тревоге.
func TestНадгробиеУбираетДокументИзПересборки(t *testing.T) {
	events := gateLakeFixture(t)

	// Надгробие для doc-2: он был проиндексирован, а потом удалён в источнике.
	tombstone, err := indexing.TombstoneEvent(domain.JobLease{
		JobID:      uuid.MustParse("77777777-7777-4777-8777-777777777777"),
		TenantID:   gateTenantAcme,
		DocumentID: gateDoc2,
		Kind:       domain.KindDelete,
		Stage:      domain.StageDelete,
	}, lakeBaseTime.Add(200*time.Minute))
	if err != nil {
		t.Fatalf("TombstoneEvent: %v", err)
	}

	plan := gateFold(t, gateSortLake(append(events, tombstone)))
	if len(plan.Documents) != 2 {
		t.Fatalf("пересобрано %d документов, ожидалось 2: %s",
			len(plan.Documents), gatePlanSummary(plan))
	}
	for _, document := range plan.Documents {
		if document.ID() == gateDoc2.String() {
			t.Errorf("удалённый документ %s попал в пересборку: после восстановления "+
				"он вернулся бы в выдачу", gateDoc2)
		}
	}
	if len(plan.Deleted) != 1 || plan.Deleted[0] != gateDoc2.String() {
		t.Fatalf("удалённых документов %v, ожидался [%s]: надгробие обязано быть "+
			"прочитано и учтено", plan.Deleted, gateDoc2)
	}
}

// TestНадгробиеПриходитРаньшеИндексацииДокументаУбираетЕгоТоже.
//
// Порядок событий в lake — это порядок фактов во времени. Удаление может
// прийти раньше последнего события стадии index того же документа (например,
// документ переиндексировали из запоздалой порции после удаления), и тогда
// ПОСЛЕДНЕЕ событие — это index, то есть документ снова в выдаче. Это не ошибка
// свёртки: выдачу определяет последний факт, а не первый.
func TestНадгробиеПриходитРаньшеИндексацииДокументаУбираетЕгоТоже(t *testing.T) {
	doc := gateLakeEvent(t, gateTenantAcme, domain.IndexStage,
		gateDoc3.String(), gateVersionDoc3.String(), "sha256:ccc", 300, nil)
	tombstone, err := indexing.TombstoneEvent(domain.JobLease{
		JobID:      uuid.MustParse("88888888-8888-4888-8888-888888888888"),
		TenantID:   gateTenantAcme,
		DocumentID: gateDoc3,
		Kind:       domain.KindDelete,
		Stage:      domain.StageDelete,
	}, lakeBaseTime.Add(400*time.Minute))
	if err != nil {
		t.Fatalf("TombstoneEvent: %v", err)
	}

	// Надгробие ПОЗЖЕ индексации — документ обязан исчезнуть.
	removed := gateFold(t, []domain.Event{doc, tombstone})
	if len(removed.Documents) != 0 || len(removed.Deleted) != 1 {
		t.Fatalf("надгробие после индексации не убрало документ: %s", gatePlanSummary(removed))
	}

	// Надгробие РАНЬШЕ индексации — документ снова в выдаче: последний факт
	// говорит, что он проиндексирован.
	restored := gateFold(t, []domain.Event{tombstone, doc})
	if len(restored.Documents) != 1 || len(restored.Deleted) != 0 {
		t.Fatalf("индексация после надгробия не вернула документ в выдачу: %s",
			gatePlanSummary(restored))
	}
}

// TestEventIDДетерминированПоСодержимому — идемпотентность стадий §4.
//
// Повтор стадии обязан получить ТОТ ЖЕ event_id: сначала из одних и тех же
// параметров, затем — из тех же параметров с другим временем и другим телом.
// Второе важно отдельно: тело стадии меняется при каждом проходе (метки
// времени, идентификаторы чанков), и если бы оно входило в имя, lake рос бы от
// каждого ретрая и пересборка считала бы одни и те же данные дважды.
func TestEventIDДетерминированПоСодержимому(t *testing.T) {
	base := domain.EventIDParams{
		Version: domain.RawEventVersion, TenantID: gateTenantAcme,
		Stage: domain.IndexStage, DocID: gateDoc1.String(), ContentHash: "sha256:aaa",
	}
	first := domain.EventIDFor(base)

	if again := domain.EventIDFor(base); again != first {
		t.Errorf("повторный расчёт дал другой event_id: %s и %s", first, again)
	}

	// Другая работа — другой факт, значит другой id.
	for name, changed := range map[string]domain.EventIDParams{
		"другая стадия": {
			Version: base.Version, TenantID: base.TenantID, Stage: domain.EnrichStage,
			DocID: base.DocID, ContentHash: base.ContentHash,
		},
		"другая версия": {
			Version: "raw.v2", TenantID: base.TenantID, Stage: base.Stage,
			DocID: base.DocID, ContentHash: base.ContentHash,
		},
		"другой тенант": {
			Version: base.Version, TenantID: gateTenantGlobex, Stage: base.Stage,
			DocID: base.DocID, ContentHash: base.ContentHash,
		},
		"другой документ": {
			Version: base.Version, TenantID: base.TenantID, Stage: base.Stage,
			DocID: gateDoc2.String(), ContentHash: base.ContentHash,
		},
		"другое содержимое": {
			Version: base.Version, TenantID: base.TenantID, Stage: base.Stage,
			DocID: base.DocID, ContentHash: "sha256:bbb",
		},
	} {
		if got := domain.EventIDFor(changed); got == first {
			t.Errorf("%s дал тот же event_id %s: разные факты слились в один, "+
				"и lake не сможет отличить одну редакцию документа от другой", name, got)
		}
	}

	// Время и тело в имя события НЕ входят — это и есть идемпотентность.
	firstEvent := gateLakeEvent(t, gateTenantAcme, domain.IndexStage,
		base.DocID, gateVersionDoc1.String(), base.ContentHash, 0, nil)
	retryEvent := gateLakeEvent(t, gateTenantAcme, domain.IndexStage,
		base.DocID, gateVersionDoc1.String(), base.ContentHash, 999,
		map[string]any{"chunk_count": 42})
	if firstEvent.EventID != retryEvent.EventID {
		t.Errorf("ретрай стадии с другим временем и телом дал другой event_id "+
			"(%s и %s): lake вырастет на каждом ретрае", firstEvent.EventID, retryEvent.EventID)
	}
	if firstEvent.EventID != domain.EventIDFor(base) {
		t.Errorf("NewEvent и EventIDFor разошлись: %s и %s — lake и пересборка "+
			"посчитают разные идентификаторы", firstEvent.EventID, domain.EventIDFor(base))
	}
}

// TestНаборСтадийLakeСовпадаетСНаборомСтадийКонвейера — достаточность схемы.
//
// Набор стадий объявлен дважды: в конверте (domain.AllStages) и в ограничении
// raw_events (CHECK ck_raw_events_stage). Расхождение означало бы, что стадия
// конвейера не пишется в lake (или, наоборот, что-то пишется, чего конверт не
// знает), и пересборка была бы неполной без единого признака.
//
// ЧИТАЕТСЯ ПОСЛЕДНЯЯ РЕДАКЦИЯ ОГРАНИЧЕНИЯ, А НЕ ТОЛЬКО 0002. Перечисление
// стадий меняется миграцией (0003 добавила delete), и применённую 0002 не
// переписывают, поэтому действующая редакция стоит в последней из них. Проверка
// только 0002 видела бы снятое ограничение и ругалась бы на корректно
// работающую схему.
func TestНаборСтадийLakeСовпадаетСНаборомСтадийКонвейера(t *testing.T) {
	line := gateStageConstraint(t, "ck_raw_events_stage")
	for _, stage := range domain.AllStages() {
		quoted := "'" + stage.String() + "'"
		if !strings.Contains(line, quoted) {
			t.Errorf("стадия %s есть в конверте, но её нет в ограничении ck_raw_events_stage: %s",
				stage, line)
		}
	}
	// Лишние стадии в схеме, которых нет в конверте, тоже означали бы
	// расхождение: читатель обязан знать, что писать в lake.
	for _, quoted := range []string{"'tombstone'", "'remove'", "'copy'"} {
		if strings.Contains(line, quoted) {
			t.Errorf("в ограничении ck_raw_events_stage есть стадия %s, которой нет в конверте", quoted)
		}
	}
}

// gateStageConstraint возвращает строку последней РЕДАКЦИИ ограничения стадий
// lake — ту, что создаёт CHECK, а не ту, что на него ссылается.
//
// Отличие не косметическое: COMMENT ON CONSTRAINT упоминает имя ограничения
// последним, и поиск по имени без проверки «здесь и CHECK (stage» поднялся бы
// на комментарий и не увидел бы перечисления вообще.
func gateStageConstraint(t *testing.T, constraint string) string {
	t.Helper()
	text := gateMigration(t, "0001_initial_schema.up.sql") +
		gateMigration(t, "0002_lake_acl.up.sql") +
		gateMigration(t, "0003_delete_stage.up.sql")

	line, found := "", false
	for _, candidate := range strings.Split(text, "\n") {
		if strings.Contains(candidate, constraint) && strings.Contains(candidate, "CHECK (stage IN") {
			line, found = candidate, true
		}
	}
	if !found {
		t.Fatalf("в миграциях lake нет ограничения %s с перечислением стадий", constraint)
	}
	return line
}

// TestПорядокЧтенияLakeЗафиксированВЗапросе — детерминизм порядка на уровне SQL.
//
// Порт EventStore обещает порядок «occurred_at, затем event_id». Проверяется
// текст настоящего запроса Lake.Read: сортировка по одной метке времени теряла
// бы события одной стадии пачки, попавшие в неё на одной микросекунде, то есть
// молча, без признака пропуска.
func TestПорядокЧтенияLakeЗафиксированВЗапросе(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "store", "pg", "lake.go"))
	if err != nil {
		t.Fatalf("чтение lake.go: %v", err)
	}
	text := string(raw)
	if !strings.Contains(text, "ORDER BY occurred_at, event_id") {
		t.Errorf("в Lake.Read нет сортировки по (occurred_at, event_id): " +
			"пересборка индекса перестанет быть воспроизводимой при параллельной записи")
	}
	// Курсор обязан сравниваться парой, а не одной меткой времени — иначе
	// постраничное чтение lake теряет события на границе страницы.
	if !strings.Contains(text, "(occurred_at, event_id) >") {
		t.Errorf("в Lake.Read нет keyset-курсора по паре (occurred_at, event_id): " +
			"чтение постранично пропустит события на границе")
	}
}

// ── Ворота «пересборка есть и подключена» ───────────────────────────────────

// lakeDeclarations — файлы, где порт lake объявлен и описан. Потребителями они
// не считаются: объявление интерфейса (§8), документирование входного типа в
// конверте и реализация в адаптере — это три места, где упоминание законно.
var lakeDeclarations = []string{
	"internal/ports/seams.go",
	"internal/envelope/seams.go",
	"internal/store/pg/lake.go",
}

// rebuildScenarioDir — каталог production-сценария пересборки.
const rebuildScenarioDir = "internal/jobs/rebuild"

// TestПересборкиИндексаВКодеНет — БЫЛ маркером долга, СТАЛ обратным вороно.
//
// ИМЯ ОСТАВЛЕНО СТАРЫМ СО ЗНАКОМ «ВКОДЕ» НЕ СЛУЧАЙНО: удалять ворота нельзя,
// удалять надо его утверждение. Пока пересборки не было, тест проверял, что
// потребителя EventStore в дереве нет, и ПРОХОДИЛ на пустом месте. Теперь он
// проверяет обратное — потребитель ЕСТЬ, он подключён и он ходит в lake через
// порт, — и падает, если пересборку снова вынут.
//
// ЧТО ПРОВЕРЯЕТСЯ, ПО ШАГАМ (каждый шаг — отдельная подпроверка, и каждый сам
// по себе роняет тест):
//
//  1. ПОТРЕБИТЕЛЬ ПОРТА. В дереве есть файл вне объявлений порта, который
//     обращается к ports.EventStore. Пока пересборки не было, таких не было
//     ни одного: lake не читал никто.
//  2. ЧТЕНИЕ ЧЕРЕЗ ПОРТ, А НЕ ЧЕРЕЗ SQL. Сценарий не импортирует internal/store:
//     подменить lake заглушкой в тесте иначе было бы нельзя, и «пересборка из
//     lake» означала бы «пересборка из таблицы, названной raw_events».
//  3. СЦЕНАРИЙ СВЁРТЫВАЕТ ЛЕНТУ. Файл свёртки вызывает Read порта и строит
//     план документов — то есть пересборка не «перезаписывает индекс», а
//     разбирает ленту событий.
//  4. КОМАНДА СОБРАНА. cmd/rebuild существует и собирает сценарий над
//     настоящими адаптерами (lake, индекс, эмбеддер, настройки) и настоящей
//     нарезкой internal/jobs/indexing.
//  5. LAKE СОБИРАЕТСЯ ОДИН РАЗ. В composition root воркера ровно одно место,
//     собирающее адаптер lake, и оно отдаёт его стадии delete. Два экземпляра
//     на одном пуле выглядели бы независимыми и перестали бы быть такими при
//     первом же рефакторинге: идемпотентность lake держится на реестре
//     raw_event_ids внутри Publish.
func TestПересборкиИндексаВКодеНет(t *testing.T) {
	mentioned := gateFilesMentioning(t, []string{"internal", "cmd"}, "EventStore")

	var consumers []string
	for _, file := range mentioned {
		if !slices.Contains(lakeDeclarations, gateTrimModule(file)) {
			consumers = append(consumers, gateTrimModule(file))
		}
	}
	if len(consumers) == 0 {
		t.Fatalf("в дереве нет ни одного потребителя порта lake (упоминания только в %v): "+
			"пересборка индекса снова пропала, и §9 «индекс хотя бы раз полностью "+
			"пересобран из lake по учебной тревоге» снова не выполнен",
			gateTrimModuleAll(mentioned))
	}
	t.Logf("портом lake упоминается в %v", gateTrimModuleAll(mentioned))

	t.Run("СценарийЧитаетLakeЧерезПорт", func(t *testing.T) {
		source := gateReadPackage(t, rebuildScenarioDir)
		if !strings.Contains(source, "ports.EventStore") {
			t.Error("сценарий пересборки не обращается к ports.EventStore: lake читается " +
				"чем-то другим, и подменить его заглушкой нельзя")
		}
		// Прямой запрет: сценарий не знает про хранилище. Обход дерева по
		// содержимому файлов ловит и SQL, собранный руками внутри сценария, —
		// в отличие от проверки импортов, которую делает layers_test.
		for _, forbidden := range []string{
			modulePrefix + "internal/store",
			"raw_events",
			"SELECT ",
		} {
			if strings.Contains(source, forbidden) {
				t.Errorf("в %s есть %q: пересборка обязана идти через порт, "+
					"иначе она не потребитель lake, а ещё один читатель базы",
					rebuildScenarioDir, forbidden)
			}
		}
	})

	t.Run("СценарийСворачиваетЛенту", func(t *testing.T) {
		fold := gateReadFile(t, filepath.Join(rebuildScenarioDir, "fold.go"))
		if !strings.Contains(fold, "func Fold(events []domain.Event)") {
			t.Error("в сценарии нет функции Fold(events): лента не сворачивается в " +
				"план документов, и пересборка не делает ничего")
		}
		if !strings.Contains(fold, "domain.IndexStage") {
			t.Error("свёртка не смотрит на стадию index: непонятно, какое событие " +
				"считается утверждением «документ в индексе»")
		}
		if !strings.Contains(fold, "domain.DeleteStage") {
			t.Error("свёртка не смотрит на стадию delete: удалённый в источнике документ " +
				"вернулся бы в выдачу после пересборки")
		}
	})

	t.Run("СценарийПишетЧерезИндексныйПорт", func(t *testing.T) {
		source := gateReadPackage(t, rebuildScenarioDir)
		if !strings.Contains(source, "ports.SearchIndex") {
			t.Error("сценарий пересборки не обращается к ports.SearchIndex: индекс, " +
				"который он восстанавливает, назван иначе")
		}
		if !strings.Contains(source, "DeleteDocument") {
			t.Error("сценарий пересборки не зовёт DeleteDocument: надгробие будет " +
				"прочитано, но чанки удалённого документа останутся в индексе")
		}
	})

	t.Run("КомандаПересборкиСобрана", func(t *testing.T) {
		entry := filepath.Join(moduleRoot, "cmd", "rebuild", "main.go")
		if _, err := os.Stat(entry); err != nil {
			t.Fatalf("нет точки входа пересборки %s: §9 описывает учебную тревогу как "+
				"прогон оператора, а запускать его нечем", entry)
		}
		source, err := os.ReadFile(entry)
		if err != nil {
			t.Fatalf("чтение точки входа: %v", err)
		}
		text := string(source)
		// Настоящие адаптеры, а не заглушки: lake из Postgres, индекс из
		// Elasticsearch, нарезка из внутренней стадии индексации.
		for _, required := range []string{
			"postgres.NewLake(",
			"rebuild.New(",
			"indexing.Chunk(",
			"os.Exit(",
		} {
			if !strings.Contains(text, required) {
				t.Errorf("в cmd/rebuild нет %q: команда пересборки собрана не над "+
					"настоящими адаптерами или не может сообщить о провале кодом возврата",
					required)
			}
		}
	})

	t.Run("LakeСобираетсяВCompositionRootОдинРаз", func(t *testing.T) {
		entry := filepath.Join(moduleRoot, "cmd", "worker", "main.go")
		source, err := os.ReadFile(entry)
		if err != nil {
			t.Fatalf("чтение состава воркера: %v", err)
		}
		text := string(source)
		if count := strings.Count(text, "NewLake("); count != 1 {
			t.Errorf("в composition root воркера адаптер lake собирается %d раз, "+
				"ожидался ровно один: экземпляры делят один пул, а идемпотентность "+
				"lake держится на реестре raw_event_ids внутри Publish", count)
		}
		// Собранный экземпляр обязан достаться писателю надгробий: lake,
		// собранный «про запас», никто не зовёт.
		if !strings.Contains(text, "Lake:       lake") {
			t.Error("собранный lake не отдан стадии удаления: надгробия удаления не " +
				"попадут в источник правды, и пересборка их не увидит")
		}
	})
}

// gateReadFile читает файл пакета от его каталога в дереве.
func gateReadFile(t *testing.T, relative string) string {
	t.Helper()
	path := filepath.Join(moduleRoot, filepath.FromSlash(relative))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("чтение %s: %v", relative, err)
	}
	return string(raw)
}

// gateReadPackage склеивает все нетестовые .go-файлы каталога.
//
// Именно нетестовые: тест сценария в этом же каталоге проверял бы сам себя, а
// ворота должны говорить о production-коде.
func gateReadPackage(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(moduleRoot, filepath.FromSlash(dir)))
	if err != nil {
		t.Fatalf("чтение каталога %s: %v", dir, err)
	}
	var builder strings.Builder
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		names = append(names, name)
	}
	slices.Sort(names)
	if len(names) == 0 {
		t.Fatalf("в %s нет ни одного нетестового .go-файла: проверять нечего", dir)
	}
	for _, name := range names {
		builder.WriteString(gateReadFile(t, dir+"/"+name))
	}
	return builder.String()
}

// gateTrimModule приводит путь к виду от корня модуля.
func gateTrimModule(path string) string {
	relative, err := filepath.Rel(moduleRoot, filepath.FromSlash(path))
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(relative)
}

func gateTrimModuleAll(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		out = append(out, gateTrimModule(path))
	}
	return out
}

// ── Тот же lake на живой PostgreSQL ─────────────────────────────────────────

// TestПересборкаИзLakeНаНастоящейPostgres — lake как он поедет в прод.
//
// БЕЗ БАЗЫ ТЕСТ ПРОПУСКАЕТСЯ. Что он проверяет сверх бескластерных ворот:
//
//   - повторная публикация той же порции не добавляет ни одной строки
//     (идемпотентность §4 через реестр raw_event_ids);
//   - чтение отдаёт события в порядке (occurred_at, event_id);
//   - постраничное чтение курсором не теряет и не дублирует события;
//   - два полных прохода по lake дают один и тот же набор событий;
//   - ПРОDUCTION-СВЁРТКА по фактическому чтению lake даёт тот же план, что и по
//     фикстуре: lake пригоден для восстановления, а не только для хранения.
func TestПересборкаИзLakeНаНастоящейPostgres(t *testing.T) {
	if testing.Short() {
		t.Skip("интеграционный тест: нужен PostgreSQL (запустите без -short)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)

	// Полная схема lake, включая 0003: фикстура gateLakeFixture строит событие
	// для каждой стадии domain.AllStages, а набор стадий теперь на шесть.
	db := gateOpenLiveSchema(t, ctx, gateLakeSchema,
		"0001_initial_schema.up.sql", "0002_lake_acl.up.sql", "0003_delete_stage.up.sql")
	lake := pgadapter.NewLake(db)
	events := gateLakeFixture(t)

	if err := lake.PublishBatch(ctx, events); err != nil {
		t.Fatalf("PublishBatch: %v", err)
	}
	// Та же порция ещё раз: lake не должен вырасти.
	if err := lake.PublishBatch(ctx, events); err != nil {
		t.Fatalf("повторный PublishBatch: %v", err)
	}

	first := gateReadWholeLake(t, ctx, lake)
	if len(first) != len(events) {
		t.Errorf("в lake %d событий, ожидалось %d: идемпотентность §4 не сработала "+
			"или события слились по event_id", len(first), len(events))
	}
	second := gateReadWholeLake(t, ctx, lake)
	if gateEventIDs(first) != gateEventIDs(second) {
		t.Errorf("два чтения lake разошлись:\n %s\n %s", gateEventIDs(first), gateEventIDs(second))
	}

	// Порядок чтения детерминирован.
	for i := 1; i < len(first); i++ {
		previous, current := first[i-1], first[i]
		if current.OccurredAt.Before(previous.OccurredAt) ||
			(current.OccurredAt.Equal(previous.OccurredAt) &&
				current.EventID.String() < previous.EventID.String()) {
			t.Errorf("нарушен порядок (occurred_at, event_id) на позиции %d: %s идёт раньше %s",
				i, current.EventID, previous.EventID)
		}
	}

	// Постраничное чтение не теряет событий: маленькая страница обязана
	// собрать ту же ленту.
	paged := gateReadLakePaged(t, ctx, lake, 2)
	if gateEventIDs(paged) != gateEventIDs(first) {
		t.Errorf("постраничное чтение отличается от сплошного:\n сплошное %s\n постраничное %s",
			gateEventIDs(first), gateEventIDs(paged))
	}

	// Production-свёртка по фактическому чтению lake даёт тот же план, что и по
	// фикстуре: lake пригоден для восстановления, а не только для хранения.
	if got := gatePlanSummary(gateFold(t, first)); got != gatePlanSummary(gateFold(t, events)) {
		t.Errorf("пересборка по прочитанному lake разошлась с фикстурой:\n lake %s\n фикстура %s",
			got, gatePlanSummary(gateFold(t, events)))
	}

	// Порог страницы защищает восстановление от «просто всё».
	if _, err := lake.Read(ctx, domain.EventQuery{
		TenantID: gateTenantAcme, Limit: 1_000_000,
	}); err != nil {
		t.Errorf("Read с огромным Limit: %v", err)
	}
}

// gateLakeSchema — отдельная схема ворот lake.
const gateLakeSchema = "kb_gates_lake"

// gateReadWholeLake читает всю ленту тенанта.
func gateReadWholeLake(t *testing.T, ctx context.Context, store ports.EventStore) []domain.Event {
	t.Helper()
	var out []domain.Event
	cursor := ""
	for {
		page, err := store.Read(ctx, domain.EventQuery{
			TenantID: gateTenantAcme, Limit: 2, Cursor: cursor,
		})
		if err != nil {
			t.Fatalf("чтение lake: %v", err)
		}
		if len(page) == 0 {
			return out
		}
		out = append(out, page...)
		last := page[len(page)-1]
		cursor = gateLakeCursor(last)
	}
}

// gateReadLakePaged читает ленту заданным размером страницы.
func gateReadLakePaged(t *testing.T, ctx context.Context, store ports.EventStore, limit int) []domain.Event {
	t.Helper()
	var out []domain.Event
	cursor := ""
	for {
		page, err := store.Read(ctx, domain.EventQuery{
			TenantID: gateTenantAcme, Limit: limit, Cursor: cursor,
		})
		if err != nil {
			t.Fatalf("постраничное чтение lake: %v", err)
		}
		if len(page) == 0 {
			return out
		}
		out = append(out, page...)
		cursor = gateLakeCursor(page[len(page)-1])
	}
}

// gateLakeCursor кодирует курсор в формате порта: «метка|event_id».
func gateLakeCursor(event domain.Event) string {
	return event.OccurredAt.UTC().Format(time.RFC3339Nano) + "|" + event.EventID.String()
}
