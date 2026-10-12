package indexing_test

// Снимок документа в lake, который кладёт стадия index (§1, принцип 1; §4:
// «любой индекс можно удалить и воссоздать из lake без обращения к источникам»).
//
// ГЛАВНЫЙ ТЕСТ ФАЙЛА — TestСнимокИндексаЧитаетсяНастоящейПересборкой. Он
// прогоняет НАСТОЯЩУЮ свёртку internal/jobs/rebuild по событиям, которые
// произвела стадия, и проверяет не форму тела, а РЕЗУЛЬТАТ: документ
// восстановился и нарезался теми же чанками. Любой разрыв контракта — переименованное
// поле, чужая версия схемы, забытый kb_id — валит этот тест, а не «молча даёт
// пустую пересборку», как было до появления писателя.
//
// Тест на импортирование пересборки здесь законен и обязателен: internal/arch
// проверяет импорты по НЕТЕСТОВЫМ файлам, а контракт «писатель ↔ потребитель»
// существует только на уровне формы JSON, которую видит только потребитель.
// Проверить его иначе нечем.
//
// Остальное закрывает решения, которые не видны из результата свёртки: момент
// публикации, идемпотентность ключа, отказ lake и изоляция тенанта.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	domain "github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/jobs/indexing"
	"github.com/kbsearch/kb/internal/jobs/rebuild"
)

// lakeSnapshot — разобранное тело снимка. Карта нужна для проверки ПРИСУТСТВИЯ
// полей: типизированная структура молча даёт нулевые значения отсутствующих
// полей, и «забытое» поле не отличилось бы от «пустого».
func lakeSnapshot(t *testing.T, event domain.Event) map[string]json.RawMessage {
	t.Helper()
	var body map[string]json.RawMessage
	if err := json.Unmarshal(event.Payload, &body); err != nil {
		t.Fatalf("тело снимка не разбирается: %v", err)
	}
	return body
}

// onlyIndexEvent достаёт единственное событие стадии index.
func onlyIndexEvent(t *testing.T, h *harness) domain.Event {
	t.Helper()
	recorded := h.lake.записанные()
	if len(recorded) != 1 {
		t.Fatalf("в lake записано %d событий, ожидалось 1", len(recorded))
	}
	event := recorded[0]
	if event.Stage != domain.IndexStage {
		t.Fatalf("стадия события %q вместо index", event.Stage)
	}
	return event
}

// TestИндексацияПишетВLakeСнимокДокумента — полный успешный путь писателя.
//
// Проверяется ровно одно событие и то, что шапка адресована заданию: lake
// идемпотентен по event_id, и второе событие того же документа означало бы либо
// дубль в lake, либо два разных утверждения о нём.
func TestИндексацияПишетВLakeСнимокДокумента(t *testing.T) {
	cfg := testConfig(20, 4)
	h := newHarness(t, cfg)

	if err := h.indexer.Index(context.Background(), h.lease); err != nil {
		t.Fatalf("Index: %v", err)
	}
	if h.lake.calls != 1 {
		t.Fatalf("lake вызван %d раз, ожидался 1", h.lake.calls)
	}

	event := onlyIndexEvent(t, h)
	if event.TenantID != h.lease.TenantID {
		t.Errorf("снимок адресован тенанту %s вместо %s", event.TenantID, h.lease.TenantID)
	}
	if event.DocID != h.lease.DocumentID.String() {
		t.Errorf("снимок адресован документу %s вместо %s", event.DocID, h.lease.DocumentID)
	}
	if !lakeKey.MatchString(event.ContentHash) {
		t.Errorf("ключ снимка %q не проходит ck_raw_events_content_hash "+
			"(«<алгоритм>:[0-9a-f]+»): lake его не запишет", event.ContentHash)
	}
	if !event.OccurredAt.Equal(h.clock.now) {
		t.Errorf("occurred_at %v вместо часов стадии %v", event.OccurredAt, h.clock.now)
	}
}

// TestСнимокИндексаЧитаетсяНастоящейПересборкой — ГЛАВНЫЙ ТЕСТ ФАЙЛА.
//
// Стадия индексации отдаёт события настоящей свёртке internal/jobs/rebuild и
// сравнивает восстановленный документ с тем, который ушёл в индекс. Проверяются
// все поля, которые потребитель читает поимённо, и — главное — РЕЗУЛЬТАТ:
// восстановленный документ нарезается теми же чанками с теми же chunk_id.
// Последнее и есть требование §4 «любой индекс можно удалить и воссоздать из
// lake»: если снимок неполон, пересборка построит другой индекс, и это будет
// видно здесь, а не на учебной тревоге.
func TestСнимокИндексаЧитаетсяНастоящейПересборкой(t *testing.T) {
	cfg := testConfig(20, 4)
	h := newHarness(t, cfg)
	h.documents.parsed.Metadata = map[string]any{
		"space_id": "space-platform",
		"acl":      []any{"group:team-backend", "user:anna"},
	}

	if err := h.indexer.Index(context.Background(), h.lease); err != nil {
		t.Fatalf("Index: %v", err)
	}
	plan, err := rebuild.Fold(h.lake.записанные())
	if err != nil {
		t.Fatalf("пересборка не прочитала событие стадии index: %v", err)
	}
	if len(plan.Deleted) != 0 {
		t.Errorf("пересборка насчитала %d удалённых документов: надгробий не было", len(plan.Deleted))
	}
	if len(plan.Documents) != 1 {
		t.Fatalf("пересборка восстановила %d документов, ожидался 1: снимок стадии "+
			"index не пригоден для восстановления", len(plan.Documents))
	}

	parsed := h.documents.parsed
	restored := plan.Documents[0]
	cases := []struct {
		name      string
		got, want any
	}{
		{"document_id", restored.ID(), parsed.DocumentID.String()},
		{"tenant_id", restored.TenantID, parsed.TenantID},
		{"kb_id", restored.KBID, parsed.KBID},
		{"version_id", restored.VersionID, parsed.VersionID},
		{"title", restored.Title, parsed.Title},
		{"chunk_count", restored.ChunkCount, expectedChunks(t, cfg)},
		{"контейнер", restored.Rights.SpaceID, "space-platform"},
		{"число блоков", len(restored.Blocks), len(parsed.Blocks)},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: %#v, ожидалось %#v", c.name, c.got, c.want)
		}
	}
	if want := []string{"group:team-backend", "user:anna"}; !slices.Equal(restored.Rights.ExceptionTerms(), want) {
		t.Errorf("исключения %#v, ожидалось %#v", restored.Rights.ExceptionTerms(), want)
	}
	for n, want := range parsed.Blocks {
		got, ok := domain.Block{}, false
		for _, block := range restored.Blocks {
			if block.BlockID == want.BlockID {
				got, ok = block, true
				break
			}
		}
		if !ok {
			t.Errorf("блок %d (%s) не восстановился", n, want.BlockID)
			continue
		}
		if got.Text != want.Text || got.Kind != want.Kind {
			t.Errorf("блок %d восстановился другим содержимым: %q вместо %q", n, got.Text, want.Text)
		}
	}

	// Последнее и решающее: восстановленный документ обязан дать тот же индекс.
	// Права и заголовок едут полем Document, а не из метаданных, поэтому лишних
	// проверок здесь не нужно — расхождение проявилось бы в чанках.
	original, err := indexing.Chunk(parsed, cfg)
	if err != nil {
		t.Fatalf("Chunk оригинала: %v", err)
	}
	again, err := indexing.Chunk(restored.Parsed(), cfg)
	if err != nil {
		t.Fatalf("Chunk восстановленного документа: %v", err)
	}
	if len(again) != len(original) {
		t.Fatalf("восстановленный документ дал %d чанков вместо %d", len(again), len(original))
	}
	for n := range original {
		if again[n].ChunkID != original[n].ChunkID {
			t.Errorf("чанк %d: chunk_id %s вместо %s — пересборка построила бы другой индекс",
				n, again[n].ChunkID, original[n].ChunkID)
		}
		if !slices.Equal(again[n].BlockIDs, original[n].BlockIDs) {
			t.Errorf("чанк %d: блоки локатора разошлись: %v вместо %v",
				n, again[n].BlockIDs, original[n].BlockIDs)
		}
		if !slices.Equal(again[n].SectionPath, original[n].SectionPath) {
			t.Errorf("чанк %d: путь раздела разошёлся", n)
		}
	}
}

// TestТелоСнимкаСогласуетсяСШапкой — сверка полей тела с шапкой события.
//
// Тело обязано подтверждать шапку: читатель берёт doc_id и tenant_id из тела, и
// тело, собранное из другого источника, собрало бы документ из чужого
// содержимого, а пересборка прошла бы и выдала неверный индекс.
//
// schema_version сверяется с rebuild.ChunkSchemaVersion — единственным способом
// связать два независимых литерала в разных сценариях: импорт между ними
// запрещён архитектурой (§8, новый потребитель — новый шов), и без такой
// сверки «1.0» и «1.0» разъехались бы при первой же правке одного из них.
func TestТелоСнимкаСогласуетсяСШапкой(t *testing.T) {
	cfg := testConfig(20, 4)
	h := newHarness(t, cfg)
	if err := h.indexer.Index(context.Background(), h.lease); err != nil {
		t.Fatalf("Index: %v", err)
	}
	event := onlyIndexEvent(t, h)
	body := lakeSnapshot(t, event)
	parsed := h.documents.parsed

	// Строковые поля сверяются ТОЧНО и с шапкой события, а не с эталоном из
	// теста: сверка с шапкой и есть контракт (lakePayload.checkAgainst).
	strings := []struct{ field, want string }{
		{"schema_version", schemaVersionWant},
		{"doc_id", event.DocID},
		{"tenant_id", event.TenantID.String()},
		{"stage", event.Stage.String()},
		{"content_hash", event.ContentHash},
		{"kb_id", parsed.KBID.String()},
		{"version_id", parsed.VersionID.String()},
		{"title", parsed.Title},
		// Пустой контейнер — законное значение, а не «поле забыли»: документ,
		// лично закрытый на пустой список, обязан восстановиться закрытым.
		{"space_id", ""},
	}
	for _, c := range strings {
		raw, ok := body[c.field]
		if !ok {
			t.Errorf("в теле снимка нет поля %q: пересборка не прочла бы его и молча потеряла",
				c.field)
			continue
		}
		var got string
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Errorf("поле %q не строка: %s", c.field, raw)
			continue
		}
		if got != c.want {
			t.Errorf("в теле снимка %s = %q, ожидалось %q", c.field, got, c.want)
		}
	}

	// Поля не-строки тоже обязаны ПРИСУТСТВОВАТЬ: разбирая тело в карту, читатель
	// не отличает «нет поля» от «пустое значение», и потерянное поле документа
	// означала бы потерянный документ в выдаче.
	var acl []string
	if raw, ok := body["acl"]; !ok {
		t.Error("в теле снимка нет поля acl: пустые права и отсутствие прав неразличимы")
	} else if err := json.Unmarshal(raw, &acl); err != nil {
		t.Errorf("acl не массив строк: %s", raw)
	} else if len(acl) != 0 {
		t.Errorf("у документа без исключений acl = %v", acl)
	}

	var blocks []domain.Block
	if raw, ok := body["blocks"]; !ok {
		t.Error("в теле снимка нет поля blocks: пересборке нечего нарезать")
	} else if err := json.Unmarshal(raw, &blocks); err != nil {
		t.Errorf("blocks не массив блоков: %s", raw)
	} else if len(blocks) != len(parsed.Blocks) {
		t.Errorf("в снимке %d блоков вместо %d", len(blocks), len(parsed.Blocks))
	}

	var chunkCount int
	if raw, ok := body["chunk_count"]; !ok {
		t.Error("в теле снимка нет поля chunk_count: диагностика прошлой индексации потеряна")
	} else if err := json.Unmarshal(raw, &chunkCount); err != nil {
		t.Errorf("chunk_count не число: %s", raw)
	} else if want := expectedChunks(t, cfg); chunkCount != want {
		t.Errorf("chunk_count %d вместо %d", chunkCount, want)
	}

	if schemaVersionWant != rebuild.ChunkSchemaVersion {
		t.Errorf("стадия index пишет schema_version %q, пересборка читает %q: "+
			"тело ушло бы в lake впереди читателя", schemaVersionWant, rebuild.ChunkSchemaVersion)
	}
}

// schemaVersionWant — значение, которое обязано стоять в теле снимка.
//
// Оно НЕ берётся из пакета стадии (константа unexported): тест проверяет равенство
// строк двух независимых литералов в двух сценариях, которые не могут импортировать
// друг друга. Пока строки совпадают, схема одна; первая же правка любой из них
// валит этот тест, а не полевую пересборку на стенде.
const schemaVersionWant = "1.0"

// TestКлючСнимкаДетерминирован — таблица правил ключа идемпотентности §4.
//
// lake отбрасывает событие с уже известным event_id, поэтому от ключа зависит
// ровно одно: станет ли повтор стадии тем же фактом или вторым. Разбираются
// случаи, где это неочевидно.
//
//	chunk_count НЕ входит в ключ сознательно: он зависит от настроек нарезки,
//	а пересборка трактует его смену как правильный результат, а не как новый
//	факт (fold.go, Document.ChunkCount). Включить его в ключ означало бы
//	плодить события при каждой правке настроек.
func TestКлючСнимкаДетерминирован(t *testing.T) {
	base := indexParams()

	cases := []struct {
		name     string
		mutate   func(*indexing.IndexEventParams)
		wantSame bool
	}{
		{"повтор той же индексации — тот же факт", func(*indexing.IndexEventParams) {}, true},
		{"повтор с другим моментом времени — тот же факт", func(p *indexing.IndexEventParams) {
			p.OccurredAt = p.OccurredAt.Add(72 * time.Hour)
		}, true},
		{"тот же документ другой версии — другой факт", func(p *indexing.IndexEventParams) {
			version := uuid.MustParse("77777777-7777-4777-8777-777777777777")
			p.Lease.VersionID = &version
			p.Parsed.VersionID = version
		}, false},
		{"новое содержимое — другой факт", func(p *indexing.IndexEventParams) {
			p.Parsed.Blocks = append(slices.Clone(p.Parsed.Blocks),
				domain.Block{BlockID: uuid.New(), Position: 9, Kind: "paragraph", Text: words(3)})
		}, false},
		{"другая база знаний — другой факт", func(p *indexing.IndexEventParams) {
			p.Parsed.KBID = uuid.MustParse("88888888-8888-4888-8888-888888888888")
		}, false},
		{"другой заголовок — другой факт", func(p *indexing.IndexEventParams) {
			p.Parsed.Title = "Руководство (ред. 2)"
		}, false},
		{"изменившиеся права — другой факт: иначе lake оставил бы прежний ACL",
			func(p *indexing.IndexEventParams) {
				p.Rights = domain.DocumentRights{ACL: []string{"user:anna"}}
			}, false},
		{"смена числа чанков — тот же факт: chunk_count вне ключа",
			func(p *indexing.IndexEventParams) { p.ChunkCount = 99 }, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			first, err := indexing.IndexEvent(base)
			if err != nil {
				t.Fatalf("IndexEvent: %v", err)
			}
			other := base
			c.mutate(&other)
			second, err := indexing.IndexEvent(other)
			if err != nil {
				t.Fatalf("IndexEvent (изменённый): %v", err)
			}
			if same := first.EventID == second.EventID; same != c.wantSame {
				t.Errorf("event_id %s и %s: одинаковость %v, ожидалось %v",
					first.EventID, second.EventID, same, c.wantSame)
			}
		})
	}
}

// indexParams — эталонные входные данные IndexEvent.
func indexParams() indexing.IndexEventParams {
	parsed := document(block(0, []string{"Раздел"}, words(6)), block(1, []string{"Раздел"}, words(5)))
	versionID := parsed.VersionID
	return indexing.IndexEventParams{
		Lease: domain.JobLease{
			JobID:      uuid.MustParse("99999999-9999-4999-8999-999999999991"),
			TenantID:   parsed.TenantID,
			DocumentID: parsed.DocumentID,
			VersionID:  &versionID,
			Stage:      domain.StageIndex,
		},
		Parsed:     parsed,
		Rights:     domain.DocumentRights{SpaceID: "space-platform"},
		ChunkCount: 3,
		OccurredAt: time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC),
	}
}

// TestПовторнаяИндексацияНеПлодитСобытий — идемпотентность на живой стадии.
//
// Тот же документ прогоняется дважды, как это делает повтор задания после
// сбоя. lake вызывается дважды (стадия не знает, что событие уже есть), но
// записывает одно: второй event_id совпал и был отброшен реестром
// raw_event_ids. Проверяется поведение lake-подделки, повторяющей этот реестр,
// а не число вызовов порта.
func TestПовторнаяИндексацияНеПлодитСобытий(t *testing.T) {
	h := newHarness(t, testConfig(20, 4))

	if err := h.indexer.Index(context.Background(), h.lease); err != nil {
		t.Fatalf("первый Index: %v", err)
	}
	// Повтор приходит с НОВОЙ выдачей: очередь не отдаёт задание со старым
	// lease, поэтому токен другой, а факт индексации — тот же самый.
	retry := h.lease
	retry.LeaseToken = "lease-token-retry"
	retry.Attempt = 2
	if err := h.indexer.Index(context.Background(), retry); err != nil {
		t.Fatalf("повтор Index: %v", err)
	}

	if h.lake.calls != 2 {
		t.Fatalf("lake вызван %d раз, ожидалось 2", h.lake.calls)
	}
	if recorded := h.lake.записанные(); len(recorded) != 1 {
		t.Errorf("в lake записано %d событий, ожидалось 1: повтор той же индексации — "+
			"тот же факт, а не второй", len(recorded))
	}
}

// TestНоваяРедакцияДаётДругоеСобытие — документ перезагружен содержимым.
//
// lake отбрасывает дубль по event_id, поэтому если бы новая редакция получила
// тот же ключ, она бы молча не записалась: документ в индексе обновился бы, а в
// источнике правды осталась бы прежняя редакция. Сверка идёт через настоящую
// свёртку: правило «последнее событие документа решает всё» обязано выбрать
// новую редакцию.
func TestНоваяРедакцияДаётДругоеСобытие(t *testing.T) {
	cfg := testConfig(20, 4)
	h := newHarness(t, cfg)
	if err := h.indexer.Index(context.Background(), h.lease); err != nil {
		t.Fatalf("первый Index: %v", err)
	}

	// Новый документ с тем же doc_id и другим содержимым — новая версия.
	newVersion := uuid.MustParse("77777777-7777-4777-8777-777777777777")
	h.documents.parsed.VersionID = newVersion
	h.documents.parsed.Blocks = []domain.Block{
		block(0, []string{"Раздел"}, words(7)),
		block(1, []string{"Раздел"}, words(8)),
		block(2, []string{"Раздел"}, words(9)),
	}
	h.lease.VersionID = &newVersion
	h.lease.LeaseToken = "lease-token-v2"
	if err := h.indexer.Index(context.Background(), h.lease); err != nil {
		t.Fatalf("второй Index: %v", err)
	}

	recorded := h.lake.записанные()
	if len(recorded) != 2 {
		t.Fatalf("в lake %d событий, ожидалось 2: новая редакция обязана быть отдельным фактом",
			len(recorded))
	}
	plan, err := rebuild.Fold(recorded)
	if err != nil {
		t.Fatalf("пересборка: %v", err)
	}
	if len(plan.Documents) != 1 {
		t.Fatalf("пересборка вернула %d документов, ожидался 1", len(plan.Documents))
	}
	if got := plan.Documents[0].VersionID; got != newVersion {
		t.Errorf("пересборка восстановила версию %s вместо новой %s", got, newVersion)
	}
	if got, want := len(plan.Documents[0].Blocks), len(h.documents.parsed.Blocks); got != want {
		t.Errorf("восстановлено %d блоков вместо %d: lake отдал прежнюю редакцию", got, want)
	}
}

// TestИндексацияПриОтказеВLakeНеПишет — ни один отказ не оставляет события.
//
// Каждая строка — обрыв ДО записи чанков или на ней самой. lake на всех них
// обязан молчать: утверждение «документ в индексе» было бы неправдой, а
// пересборка построила бы индекс из документа, которого в нём нет.
func TestИндексацияПриОтказеВLakeНеПишет(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(*harness)
	}{
		{"разобранный документ не прочитан", func(h *harness) {
			h.documents.loadErr = domain.NotFound("Версия документа не найдена")
		}},
		{"разобранный документ принадлежит другому тенанту", func(h *harness) {
			h.documents.parsed.TenantID = otherTenantID
		}},
		{"настройки индексации недоступны", func(h *harness) {
			h.settings.err = domain.ConfigurationMissing("", map[string]any{"keys": []string{"chunking.max_tokens"}})
		}},
		{"оракул прав ответил отказом", func(h *harness) {
			h.documents.rightsErr = domain.Internal("база недоступна")
		}},
		{"эмбеддер вернул неверное число векторов", func(h *harness) {
			h.embedder.shortCount = true
		}},
		{"прежние чанки версии не снеслись", func(h *harness) {
			h.index.delErr = domain.IndexUnavailable("Elasticsearch недоступен")
		}},
		{"в индекс записано меньше чанков, чем насчитано", func(h *harness) {
			h.index.shortWrite = true
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, testConfig(20, 4))
			c.prepare(h)

			if err := h.indexer.Index(context.Background(), h.lease); err == nil {
				t.Fatal("ожидался отказ стадии")
			}
			if h.lake.calls != 0 {
				t.Errorf("lake вызван %d раз при неудачной индексации: в источнике "+
					"правды появилось утверждение о несуществующем факте", h.lake.calls)
			}
			if h.documents.activated || h.jobs.finishCalls != 0 {
				t.Error("версия активирована или задание закрыто при отказе индексации")
			}
		})
	}
}

// TestОтказLakeНеПроглатывается — lake недоступен, и это отказ задания.
//
// Молча проглоченная запись в lake — это документ, которого пересборка не
// найдёт, при зелёной стадии, живом задании и «успешном» отчёте §9. Поэтому:
//
//  1. ошибка lake доходит наружу;
//  2. задание НЕ закрыто и версия НЕ активирована — то есть отказ повторяем;
//  3. повтор после починки lake сходится: снимок один, а не два.
//
// Пункт 2 — тот самый довод, по которому снимок пишется ДО переключения
// активной версии: после FinishIndex ошибку worker'у записать уже некуда.
func TestОтказLakeНеПроглатывается(t *testing.T) {
	lakeErr := domain.Internal("lake недоступен")
	h := newHarness(t, testConfig(20, 4))
	h.lake.err = lakeErr

	err := h.indexer.Index(context.Background(), h.lease)
	if !errors.Is(err, lakeErr) {
		t.Fatalf("Index вернул %v, ожидалась ошибка lake", err)
	}
	if h.jobs.finishCalls != 0 {
		t.Error("задание закрыто при недоступном lake: повтор его уже не вернёт в очередь")
	}
	if h.documents.activated {
		t.Error("версия активирована при недоступном lake")
	}

	// Починка lake и повтор той же стадии: тот же снимок, тот же event_id.
	h.lake.err = nil
	if err := h.indexer.Index(context.Background(), h.lease); err != nil {
		t.Fatalf("повтор Index: %v", err)
	}
	if recorded := h.lake.записанные(); len(recorded) != 1 {
		t.Errorf("после повтора в lake %d событий, ожидалось 1", len(recorded))
	}
	if h.jobs.finishCalls != 1 {
		t.Errorf("задание закрыто %d раз, ожидался 1", h.jobs.finishCalls)
	}
}

// TestСборкаБезLakeОтказывает — частичная сборка обязана быть громкой.
//
// Deps общая у стадий convert и index, поэтому lake нельзя требовать при сборке
// (как и ParsedKey у конверта). Но стадия, которой lake не задан, обязана
// отказать на границе, а не упасть с нулевым указателем или — что хуже —
// отработать вхолостую.
func TestСборкаБезLakeОтказывает(t *testing.T) {
	h := newHarness(t, testConfig(20, 4))
	indexer := indexing.NewIndexer(indexing.Deps{
		Documents:  h.documents,
		Jobs:       h.jobs,
		Settings:   h.settings,
		Embedder:   h.embedder,
		Index:      h.index,
		Storage:    &stubStorage{},
		Queryer:    stubQueryer{},
		Transactor: h.tx,
		Clock:      h.clock,
	})

	err := indexer.Index(context.Background(), h.lease)
	if err == nil {
		t.Fatal("стадия index отработала без lake")
	}
	var appErr *domain.AppError
	if !errors.As(err, &appErr) || appErr.Code != domain.CodeConfigurationInvalid {
		t.Errorf("получено %v вместо CONFIGURATION_INVALID", err)
	}
	if h.documents.activated || h.jobs.finishCalls != 0 {
		t.Error("задание закрыто без lake: снимок документа не записан никуда")
	}
}

// TestПраваДокументаЕдутВСнимок — §6 через настоящую пересборку.
//
// Права проверяются не в теле события, а в восстановленном документе: утечка
// выглядит ровно так, что права разошлись между индексом и источником правды,
// и увидеть это можно только на стороне потребителя.
//
// Четыре состояния, и каждое обязано пережить дорогу:
//
//	контейнер — обычный документ;
//	личная заметка — контейнера нет, исключение одно;
//	личное закрытие администратором — контейнер срезан, остаётся исключение;
//	прав нет вовсе — документ обязан восстановиться невидимым, а не видимым.
func TestПраваДокументаЕдутВСнимок(t *testing.T) {
	cases := []struct {
		name      string
		meta      map[string]any
		override  *domain.DocumentRights
		wantSpace string
		wantACL   []string
	}{
		{
			name:      "контейнер документа",
			meta:      map[string]any{"space_id": "space-platform"},
			wantSpace: "space-platform",
		},
		{
			name:    "личная заметка",
			meta:    map[string]any{"acl": []any{"user:anna"}},
			wantACL: []string{"user:anna"},
		},
		{
			name:      "контейнер с исключениями",
			meta:      map[string]any{"space_id": "space-hr", "acl": []any{"user:anna"}},
			wantSpace: "space-hr",
			wantACL:   []string{"user:anna"},
		},
		{
			name:      "личное закрытие администратором срезает контейнер",
			meta:      map[string]any{"space_id": "space-hr", "acl": []any{"user:petya"}},
			override:  &domain.DocumentRights{ACL: []string{"user:anna"}},
			wantSpace: "",
			wantACL:   []string{"user:anna"},
		},
		{
			name: "прав нет вовсе",
			meta: map[string]any{"product": "portal"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, testConfig(20, 4))
			h.documents.parsed.Metadata = c.meta
			h.documents.rights = c.override

			if err := h.indexer.Index(context.Background(), h.lease); err != nil {
				t.Fatalf("Index: %v", err)
			}
			plan, err := rebuild.Fold(h.lake.записанные())
			if err != nil {
				t.Fatalf("пересборка: %v", err)
			}
			if len(plan.Documents) != 1 {
				t.Fatalf("восстановлено %d документов, ожидался 1", len(plan.Documents))
			}
			rights := plan.Documents[0].Rights
			if rights.SpaceID != c.wantSpace {
				t.Errorf("контейнер %q вместо %q", rights.SpaceID, c.wantSpace)
			}
			if got := rights.ExceptionTerms(); !slices.Equal(got, c.wantACL) {
				t.Errorf("исключения %#v, ожидалось %#v", got, c.wantACL)
			}
			// Права в снимке и права в индексе обязаны совпадать: разошлись бы
			// они — пересборка восстановила бы документ не с теми правами.
			for n, chunk := range h.index.chunks {
				if chunk.Rights.SpaceID != rights.SpaceID {
					t.Errorf("чанк %d: контейнер в индексе %q, в lake %q",
						n, chunk.Rights.SpaceID, rights.SpaceID)
				}
				if !slices.Equal(chunk.Rights.ExceptionTerms(), rights.ExceptionTerms()) {
					t.Errorf("чанк %d: исключения в индексе %v, в lake %v",
						n, chunk.Rights.ExceptionTerms(), rights.ExceptionTerms())
				}
			}
		})
	}
}

// TestТенантДокументаНеСмешивается — изоляция §3/§4 проверяется на потребителе.
//
// Пара «одинаковый document_id у двух тенантов» не экзотика: doc_id §4 хэширует
// tenant, но рукописные и загруженные идентификаторы исторически не хэшировались.
// Снимок обязан называть тенанта и в шапке, и в теле, иначе пересборка собрала бы
// документ одной организации из содержимого другого.
func TestТенантДокументаНеСмешивается(t *testing.T) {
	t.Run("тело снимка называет тенанта задания", func(t *testing.T) {
		cases := []struct {
			name   string
			tenant uuid.UUID
			doc    uuid.UUID
		}{
			{"тенант A", deleteTenantA, uuid.MustParse("33333333-3333-4333-8333-333333333333")},
			{"тенант B", deleteTenantB, uuid.MustParse("44444444-4444-4444-8444-444444444444")},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				params := indexParams()
				params.Lease.TenantID = c.tenant
				params.Lease.DocumentID = c.doc
				params.Parsed.TenantID = c.tenant
				params.Parsed.DocumentID = c.doc

				event, err := indexing.IndexEvent(params)
				if err != nil {
					t.Fatalf("IndexEvent: %v", err)
				}
				body := lakeSnapshot(t, event)
				var tenant string
				if err := json.Unmarshal(body["tenant_id"], &tenant); err != nil {
					t.Fatalf("разбор tenant_id: %v", err)
				}
				if tenant != c.tenant.String() {
					t.Errorf("тело называет тенанта %q вместо %q", tenant, c.tenant)
				}
				if event.TenantID != c.tenant || event.DocID != c.doc.String() {
					t.Errorf("шапка адресована %s/%s вместо %s/%s",
						event.TenantID, event.DocID, c.tenant, c.doc)
				}
			})
		}
	})

	t.Run("снимок чужого документа не собирается", func(t *testing.T) {
		params := indexParams()
		params.Parsed.TenantID = otherTenantID
		if _, err := indexing.IndexEvent(params); err == nil {
			t.Fatal("снимок собран для разобранного документа чужого тенанта")
		}
	})

	t.Run("пересборка разводит документы двух тенантов", func(t *testing.T) {
		var events []domain.Event
		for _, tenant := range []uuid.UUID{deleteTenantA, deleteTenantB} {
			params := indexParams()
			params.Lease.TenantID = tenant
			params.Lease.DocumentID = uuid.MustParse("33333333-3333-4333-8333-333333333333")
			params.Parsed.TenantID = tenant
			params.Parsed.DocumentID = uuid.MustParse("33333333-3333-4333-8333-333333333333")
			params.Rights = domain.DocumentRights{SpaceID: tenant.String()}
			event, err := indexing.IndexEvent(params)
			if err != nil {
				t.Fatalf("IndexEvent: %v", err)
			}
			events = append(events, event)
		}
		// Оба документа известны lake под ОДНИМ document_id: развести их может
		// только тенант в теле события.
		plan, err := rebuild.Fold(events)
		if err == nil {
			t.Fatalf("пересборка не заметила два тенанта под одним document_id: %+v", plan)
		}

		events = events[:1]
		second := indexParams()
		second.Lease.TenantID = deleteTenantB
		second.Lease.DocumentID = uuid.MustParse("55555555-5555-4555-8555-555555555555")
		second.Parsed.TenantID = deleteTenantB
		second.Parsed.DocumentID = uuid.MustParse("55555555-5555-4555-8555-555555555555")
		second.Rights = domain.DocumentRights{SpaceID: "space-b"}
		event, err := indexing.IndexEvent(second)
		if err != nil {
			t.Fatalf("IndexEvent: %v", err)
		}
		events = append(events, event)

		plan, err = rebuild.Fold(events)
		if err != nil {
			t.Fatalf("пересборка: %v", err)
		}
		if len(plan.Documents) != 2 {
			t.Fatalf("восстановлено %d документов, ожидалось 2", len(plan.Documents))
		}
		byTenant := map[uuid.UUID]rebuild.Document{}
		for _, document := range plan.Documents {
			byTenant[document.TenantID] = document
		}
		if byTenant[deleteTenantA].Rights.SpaceID != deleteTenantA.String() {
			t.Errorf("документ тенанта A получил контейнер %q",
				byTenant[deleteTenantA].Rights.SpaceID)
		}
		if byTenant[deleteTenantB].Rights.SpaceID != "space-b" {
			t.Errorf("документ тенанта B получил контейнер %q",
				byTenant[deleteTenantB].Rights.SpaceID)
		}
	})
}

// TestПересборкаОтвергаетНеполныйСнимок — проверка проверки.
//
// Если стадия забудет поле, которое читатель считает обязательным, пересборка
// обязана УПАСТЬ, а не собрать документ без него: молчаливая потеря поля —
// молчаливая потеря документа. Тест вырезает поле из настоящего снимка и
// убеждается, что свёртка это замечает. Без него TestСнимокИндексаЧитается-
// НастоящейПересборкой прошёл бы и на неполном теле, а дыра обнаружилась бы
// только на учебной тревоге §9 — на живом стенде, по факту.
func TestПересборкаОтвергаетНеполныйСнимок(t *testing.T) {
	h := newHarness(t, testConfig(20, 4))
	if err := h.indexer.Index(context.Background(), h.lease); err != nil {
		t.Fatalf("Index: %v", err)
	}
	event := onlyIndexEvent(t, h)

	cases := []struct {
		name string
		drop string
	}{
		{"без kb_id", "kb_id"},
		{"без version_id", "version_id"},
		{"без doc_id", "doc_id"},
		{"без блоков", "blocks"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := lakeSnapshot(t, event)
			delete(body, c.drop)
			cut, err := json.Marshal(body)
			if err != nil {
				t.Fatalf("перекодирование тела: %v", err)
			}
			cutEvent := event
			cutEvent.Payload = cut

			if _, err := rebuild.Fold([]domain.Event{cutEvent}); err == nil {
				t.Errorf("пересборка приняла снимок %s и потеряла бы документ молча", c.name)
			}
		})
	}
}

// TestИндексНеВидитЛожныйСнимокЧужогоСодержимого — тело подтверждает шапку.
//
// checkAgainst сверяет doc_id, tenant_id, stage и content_hash. Подмена
// содержимого одного документа телом другого прошла бы мимо всех остальных
// проверок и собрала бы индекс из чужого документа.
func TestИндексНеВидитЛожныйСнимокЧужогоСодержимого(t *testing.T) {
	h := newHarness(t, testConfig(20, 4))
	if err := h.indexer.Index(context.Background(), h.lease); err != nil {
		t.Fatalf("Index: %v", err)
	}
	event := onlyIndexEvent(t, h)

	body := lakeSnapshot(t, event)
	foreign, err := json.Marshal(
		uuid.MustParse("55555555-5555-4555-8555-555555555555").String())
	if err != nil {
		t.Fatalf("кодирование чужого doc_id: %v", err)
	}
	body["doc_id"] = foreign
	swapped, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("перекодирование тела: %v", err)
	}
	event.Payload = swapped

	if _, err := rebuild.Fold([]domain.Event{event}); err == nil {
		t.Error("пересборка приняла тело с чужим doc_id: документ собрался бы из чужого содержимого")
	}
}
