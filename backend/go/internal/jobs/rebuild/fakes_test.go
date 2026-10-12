package rebuild_test

// Подделки портов пересборки.
//
// ЧИСТО ПРО МАТЧЕР. Каждая подделка повторяет ПОВЕДЕНИЕ настоящего адаптера в
// той части, ради которой она подставлена, и ни одной строчки правила:
//
//   - fakeLake повторяет постраничное чтение keyset-курсором и ПРОВЕРЯЕТ, что
//     следующая страница начинается строго после последнего события (как
//     (occurred_at, event_id) > в lake.go). Подделка, отдающая страницы «как
//     пришло», пропустила бы дыру в чтении;
//   - fakeEmbedder отдаёт вектор нужной размерности — иначе проверка размерности
//     в Es-адаптере падала бы по причине, не связанной с пересборкой;
//   - recordingIndex помнит КАЖДЫЙ записанный чанк в срезе, а не в карте по
//     chunk_id: повторная запись того же chunk_id обязана быть видна в отчёте
//     как два чанка, иначе тест на идемпотентность подтверждал бы сам себя.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	domain "github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/jobs/indexing"
	"github.com/kbsearch/kb/internal/jobs/rebuild"
	"github.com/kbsearch/kb/internal/ports"
)

// rebuildTenant и rebuildKB — постоянные идентификаторы фикстуры.
var (
	rebuildTenant = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	rebuildKB     = uuid.MustParse("22222222-2222-4222-8222-222222222222")
	rebuildOther  = uuid.MustParse("33333333-3333-4333-8333-333333333333")
	rebuildSpace  = "space-platform"
)

// lakeStart — начало ленты времени фикстуры. Фиксировано, чтобы порядок чтения
// не зависел от now() и прогоны были сопоставимы между собой.
var lakeStart = time.Date(2026, time.November, 3, 9, 0, 0, 0, time.UTC)

// rebuildConfig — профиль индекса фикстуры. Восемь измерений: при подставном
// эмбеддере вектор не считается, а проверка размерности в адаптере смотрит
// только на длину.
func rebuildConfig() domain.IndexingConfig {
	return domain.IndexingConfig{
		EmbeddingProviderID: "local",
		EmbeddingModel:      "intfloat/multilingual-e5-small",
		EmbeddingRevision:   "rebuild-1",
		EmbeddingDimension:  8,
		ChunkMaxTokens:      320,
		ChunkOverlapTokens:  64,
		TokenizerID:         "e5",
		NormalizeVectors:    true,
		Distance:            "cosine",
	}
}

// ── Фикстура lake ───────────────────────────────────────────────────────────

// docFixture — описание документа, из которого собираются события lake.
type docFixture struct {
	docID     uuid.UUID
	versionID uuid.UUID
	kbID      uuid.UUID
	title     string
	spaceID   string
	acl       []string
	hash      string
	// blocks — текст документа по блокам. Пустой блок не пишется вовсе.
	blocks []string
}

// indexPayload собирает тело события стадии index.
//
// Пишется обычная карта, а не вызовом экспортированного конструктора
// пересборки: контракт тела — это JSON, и проверять его надо тем, что реально
// лежит в lake. Экспортировать сборщик ради тестов значило бы проверять
// компилятором то, что проверяется разбором JSON.
func (f docFixture) indexPayload(t *testing.T, stage domain.Stage, chunkCount int) []byte {
	t.Helper()
	extra := map[string]any{}
	if stage == domain.IndexStage {
		extra["chunk_count"] = chunkCount
	}
	return f.payloadWith(t, stage, extra)
}

// payloadWith собирает тело события стадии с произвольными добавками.
func (f docFixture) payloadWith(t *testing.T, stage domain.Stage, extra map[string]any) []byte {
	t.Helper()
	payload := map[string]any{
		"schema_version": "1.0",
		"doc_id":         f.docID.String(),
		"tenant_id":      rebuildTenant.String(),
		"stage":          stage.String(),
		"content_hash":   f.hash,
		"kb_id":          f.kbID.String(),
		"version_id":     f.versionID.String(),
		"title":          f.title,
		"space_id":       f.spaceID,
		"acl":            f.acl,
		"blocks":         f.blocksPayload(),
	}
	for key, value := range extra {
		payload[key] = value
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("кодирование фикстуры %s/%s: %v", stage, f.docID, err)
	}
	return raw
}

// blocksPayload собирает блоки документа. Идентификаторы блоков детерминированы
// по содержимому: повторный прогон обязан получить те же block_id, иначе
// chunk_id плавал бы от запуска к запуску.
func (f docFixture) blocksPayload() []map[string]any {
	blocks := make([]map[string]any, 0, len(f.blocks))
	for i, text := range f.blocks {
		blocks = append(blocks, map[string]any{
			"block_id":     uuid.NewSHA1(uuid.Nil, []byte(f.docID.String()+text)).String(),
			"position":     i,
			"kind":         "paragraph",
			"text":         text,
			"section_path": []string{"Раздел"},
		})
	}
	return blocks
}

// lakeEvent собирает настоящее событие lake через domain.NewEvent.
func lakeEvent(t *testing.T, doc docFixture, stage domain.Stage, minute int, payload []byte) domain.Event {
	t.Helper()
	event, err := domain.NewEvent(domain.NewEventParams{
		TenantID:    rebuildTenant,
		Stage:       stage,
		DocID:       doc.docID.String(),
		ContentHash: doc.hash,
		Payload:     payload,
		OccurredAt:  lakeStart.Add(time.Duration(minute) * time.Minute),
	})
	if err != nil {
		t.Fatalf("сборка события %s/%s: %v", stage, doc.docID, err)
	}
	return event
}

// pipelineEvent собирает событие промежуточной стадии конвейера.
func pipelineEvent(t *testing.T, doc docFixture, stage domain.Stage, minute int) domain.Event {
	t.Helper()
	return lakeEvent(t, doc, stage, minute, doc.payloadWith(t, stage, nil))
}

// tombstoneEvent собирает настоящее надгробие удаления.
//
// Событие берётся у стадии delete (indexing.TombstoneEvent) — это тот самый
// писатель, который окажется в lake, и собирать его здесь второй раз означало
// бы проверить не его форму, а свою.
func tombstoneEvent(t *testing.T, doc docFixture, jobID uuid.UUID, minute int) domain.Event {
	t.Helper()
	occurredAt := lakeStart.Add(time.Duration(minute) * time.Minute)
	event, err := indexing.TombstoneEvent(domain.JobLease{
		JobID:      jobID,
		TenantID:   rebuildTenant,
		DocumentID: doc.docID,
		Kind:       domain.KindDelete,
		Stage:      domain.StageDelete,
	}, occurredAt)
	if err != nil {
		t.Fatalf("TombstoneEvent: %v", err)
	}
	return event
}

// sortLake приводит ленту к каноническому порядку чтения: (occurred_at,
// event_id). Именно его обещает порт EventStore.
func sortLake(events []domain.Event) []domain.Event {
	out := slices.Clone(events)
	slices.SortStableFunc(out, func(a, b domain.Event) int {
		if !a.OccurredAt.Equal(b.OccurredAt) {
			return a.OccurredAt.Compare(b.OccurredAt)
		}
		return strings.Compare(a.EventID.String(), b.EventID.String())
	})
	return out
}

// ── Подделки портов ─────────────────────────────────────────────────────────

// fakeLake — EventStore, который отдаёт заранее заданную ленту постранично.
type fakeLake struct {
	events []domain.Event
	// pageSize — сколько событий отдавать за раз. Ноль — отдать всё сразу.
	pageSize int
	calls    []domain.EventQuery
	pingErr  error
	readErr  error
}

func (l *fakeLake) Read(_ context.Context, q domain.EventQuery) ([]domain.Event, error) {
	l.calls = append(l.calls, q)
	if l.readErr != nil {
		return nil, l.readErr
	}
	if q.TenantID == uuid.Nil {
		return nil, domain.InvalidMetadata("EventQuery.tenant_id обязателен")
	}
	if q.TenantID != rebuildTenant {
		return nil, nil
	}

	// keyset-курсор: страница начинается строго после (occurred_at, event_id)
	// последнего прочитанного — так же, как (occurred_at, event_id) > в lake.go.
	after := false
	out := make([]domain.Event, 0, len(l.events))
	for _, event := range l.events {
		if after {
			out = append(out, event)
			if l.pageSize > 0 && len(out) == l.pageSize {
				break
			}
			continue
		}
		if q.Cursor == "" {
			after = true
			out = append(out, event)
			if l.pageSize > 0 && len(out) == l.pageSize {
				break
			}
			continue
		}
		mark, rawID, ok := strings.Cut(q.Cursor, "|")
		if !ok {
			return nil, domain.InvalidMetadata("курсор lake повреждён")
		}
		occurredAt, err := time.Parse(time.RFC3339Nano, mark)
		if err != nil {
			return nil, domain.InvalidMetadata("курсор lake повреждён")
		}
		eventID, err := uuid.Parse(rawID)
		if err != nil {
			return nil, domain.InvalidMetadata("курсор lake повреждён")
		}
		if event.OccurredAt.Equal(occurredAt) && event.EventID == eventID {
			after = true
		}
	}
	return out, nil
}

func (l *fakeLake) Ping(context.Context) error { return l.pingErr }

func (l *fakeLake) Close() error { return nil }

var _ ports.EventStore = (*fakeLake)(nil)

// fakeSettings — IndexingSettings, отдающий профиль фикстуры.
type fakeSettings struct {
	cfg   domain.IndexingConfig
	err   error
	calls []string
}

func (s *fakeSettings) IndexingConfig(
	_ context.Context, _ ports.Queryer, _, kbID string,
) (domain.IndexingConfig, error) {
	s.calls = append(s.calls, kbID)
	return s.cfg, s.err
}

var _ rebuild.IndexingSettings = (*fakeSettings)(nil)

// fakeChunker — нарезка, детерминированная по документу и профилю.
//
// Чанк на блок, идентификатор — настоящий domain.ChunkIDFor от того же номера и
// версии. Это НЕ копия настоящей нарезки (она живёт в indexing и подключается
// в cmd/rebuild), но её контракт соблюдён: chunk_id детерминирован по
// (версия, профиль, номер), и повтор даёт тот же идентификатор.
type fakeChunker struct {
	err error
	// texts — порядок нарезки проверяется тестом: пересборка обязана писать в
	// эмбеддер тексты чанков в том же порядке, в каком вернула нарезка.
	texts []string
}

func (c *fakeChunker) Chunk(
	doc domain.ParsedDocument, cfg domain.IndexingConfig,
) ([]domain.Chunk, error) {
	if c.err != nil {
		return nil, c.err
	}
	blocks := slices.Clone(doc.Blocks)
	slices.SortStableFunc(blocks, func(a, b domain.Block) int { return a.Position - b.Position })

	out := make([]domain.Chunk, 0, len(blocks))
	for i, block := range blocks {
		c.texts = append(c.texts, block.Text)
		out = append(out, domain.Chunk{
			ChunkID: domain.ChunkIDFor(domain.ChunkIDParams{
				VersionID:   doc.VersionID.String(),
				ProfileID:   cfg.ProfileID(),
				TokenizerID: cfg.TokenizerID,
				MaxTokens:   cfg.ChunkMaxTokens,
				Overlap:     cfg.ChunkOverlapTokens,
				Number:      i,
			}),
			ChunkNumber: i,
			Text:        block.Text,
			Locator:     domain.Locator{BlockIDs: []uuid.UUID{block.BlockID}, SectionPath: block.SectionPath},
		})
	}
	return out, nil
}

var _ rebuild.Chunker = (*fakeChunker)(nil)

// fakeEmbedder — векторы нужной размерности, разные для разных текстов.
//
// Разные, а не одинаковые: идентичный вектор у всех чанков скрыл бы ошибку
// «вектор от чанка N пришёл от чанка M», потому что подмена не была бы видна.
type fakeEmbedder struct {
	dimension int
	err       error
	inputs    []string
}

func (e *fakeEmbedder) Embed(_ context.Context, texts []string, cfg domain.IndexingConfig) ([][]float32, error) {
	if e.err != nil {
		return nil, e.err
	}
	e.inputs = append(e.inputs, texts...)
	out := make([][]float32, 0, len(texts))
	for _, text := range texts {
		vector := make([]float32, cfg.EmbeddingDimension)
		for i := range vector {
			vector[i] = float32(len(text)+i) / 7
		}
		out = append(out, vector)
	}
	return out, nil
}

func (e *fakeEmbedder) EmbedQuery(
	_ context.Context, text string, cfg domain.IndexingConfig,
) ([]float32, error) {
	vectors, err := e.Embed(context.Background(), []string{text}, cfg)
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}

func (e *fakeEmbedder) Dimensions() int { return e.dimension }

func (e *fakeEmbedder) Ping(context.Context) error { return nil }

func (e *fakeEmbedder) Close() error { return nil }

var _ ports.Embedder = (*fakeEmbedder)(nil)

// recordingIndex — индекс, помнящий каждую запись.
//
// Чанки складываются в СРЕЗ, а не в карту по chunk_id: повторная запись того же
// идентификатора обязана быть видна двумя элементами, иначе тест на
// идемпотентность подтверждал бы сам себя (карта скрыла бы дубль).
type recordingIndex struct {
	mu        sync.Mutex
	written   []domain.Chunk
	deleted   []deleteCall
	ensured   []string
	upsertErr error
	deleteErr error
}

type deleteCall struct {
	tenantID   string
	documentID string
}

func (i *recordingIndex) EnsureIndex(_ context.Context, cfg domain.IndexingConfig) (string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.ensured = append(i.ensured, cfg.ProfileID())
	return cfg.IndexName(), nil
}

func (i *recordingIndex) UpsertChunks(
	_ context.Context, _ domain.IndexingConfig, chunks []domain.Chunk,
) (int, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.upsertErr != nil {
		return 0, i.upsertErr
	}
	i.written = append(i.written, chunks...)
	return len(chunks), nil
}

func (i *recordingIndex) DeleteVersion(context.Context, string, string, string) (int, error) {
	return 0, nil
}

func (i *recordingIndex) DeleteDocument(_ context.Context, tenantID, documentID string) (int, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.deleted = append(i.deleted, deleteCall{tenantID: tenantID, documentID: documentID})
	if i.deleteErr != nil {
		return 0, i.deleteErr
	}
	removed := 0
	kept := i.written[:0]
	for _, chunk := range i.written {
		if chunk.TenantID == tenantID && chunk.DocumentID == documentID {
			removed++
			continue
		}
		kept = append(kept, chunk)
	}
	i.written = kept
	return removed, nil
}

func (i *recordingIndex) KeywordSearch(
	context.Context, string, domain.SearchFilter, int,
) ([]domain.SearchCandidate, error) {
	return nil, nil
}

func (i *recordingIndex) VectorSearch(
	context.Context, []float32, domain.IndexingConfig, domain.SearchFilter, int,
) ([]domain.SearchCandidate, error) {
	return nil, nil
}

func (i *recordingIndex) Ping(context.Context) error { return nil }

var _ ports.SearchIndex = (*recordingIndex)(nil)

// документы — уникальные document_id среди записанных чанков.
func (i *recordingIndex) документы() []string {
	seen := map[string]bool{}
	var out []string
	for _, chunk := range i.written {
		if seen[chunk.DocumentID] {
			continue
		}
		seen[chunk.DocumentID] = true
		out = append(out, chunk.DocumentID)
	}
	slices.Sort(out)
	return out
}

// подписи чанков — сравнимая форма записанного индекса.
//
// Подпись включает chunk_id: именно он ключ _id в Elasticsearch, и две записи с
// одним _id — это перезапись, а не дубль. Чтобы поймать дубль, нужны ВСЕ
// записи подряд, поэтому подпись не сворачивается в множество.
func (i *recordingIndex) подписи() []string {
	out := make([]string, 0, len(i.written))
	for _, chunk := range i.written {
		out = append(out, strings.Join([]string{
			chunk.TenantID, chunk.KBID, chunk.DocumentID, chunk.VersionID,
			chunk.ChunkID, chunk.Title, chunk.Rights.SpaceID,
			strings.Join(chunk.Rights.ExceptionTerms(), "+"), chunk.Text,
		}, "|"))
	}
	return out
}

// stubQueryer — заглушка порта Queryer.
//
// Пересборка отдаёт его сценарию настроек и ни сама ни разу к базе не
// обращается: lake читается через ports.EventStore, а не через SQL. Заглушка
// поэтому не «отвечает на запросы», а честно отказывает на любом вызове — иначе
// тихо подставленный SQL-слой прошёл бы там, где его быть не должно.
type stubQueryer struct{}

func (stubQueryer) Query(context.Context, string, ...any) (ports.Rows, error) {
	return nil, errors.New("пересборка не ходит в базу: lake читается портом EventStore")
}

func (stubQueryer) QueryRow(context.Context, string, ...any) ports.Row {
	return stubRow{err: errors.New("пересборка не ходит в базу: lake читается портом EventStore")}
}

func (stubQueryer) Exec(context.Context, string, ...any) (ports.CommandTag, error) {
	return nil, errors.New("пересборка не ходит в базу: lake читается портом EventStore")
}

type stubRow struct{ err error }

func (r stubRow) Scan(...any) error { return r.err }

var _ ports.Queryer = stubQueryer{}
