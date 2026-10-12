package answers_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kbsearch/kb/internal/docs/settings"
	domain "github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
	"github.com/kbsearch/kb/internal/search/answers"
	search "github.com/kbsearch/kb/internal/search/service"
)

// Тесты ответа с цитатами идут на заглушках поиска и LLM: правила цитат и
// отказов проверяются на всех сочетаниях, а не на тех, что повторились в тестовой
// базе.

// ── Заглушки ────────────────────────────────────────────────────────

// fakeSearch отдаёт заранее заданную выдачу по любому запросу и помнит, какие
// запросы к нему приходили.
type fakeSearch struct {
	hits    []domain.SearchCandidate
	err     error
	queries []string
	// toolHits — выдача для вызовов инструмента. Пустая карта означает, что
	// инструмент возвращает ту же выдачу, что и первый поиск.
	toolHits map[string][]domain.SearchCandidate
}

func (f *fakeSearch) Search(
	_ context.Context, _ domain.Principal, in search.Request,
) (search.Response, error) {
	if f.err != nil {
		return search.Response{}, f.err
	}
	f.queries = append(f.queries, in.Query)
	if found, ok := f.toolHits[in.Query]; ok {
		return search.Response{Status: search.StatusOK, Hits: found}, nil
	}
	return search.Response{Status: search.StatusOK, Hits: f.hits}, nil
}

// fakeLLM отвечает заранее заданным результатом и записывает запрос.
type fakeLLM struct {
	result domain.GenerateResult
	err    error
	calls  int
	reqs   []domain.GenerateRequest
}

func (f *fakeLLM) Generate(
	_ context.Context, req domain.GenerateRequest,
) (domain.GenerateResult, error) {
	f.calls++
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return domain.GenerateResult{}, f.err
	}
	return f.result, nil
}

// fakeDocs отдаёт список активных версий на повторной проверке прав.
type fakeDocs struct {
	active []domain.ActiveVersion
	err    error
	calls  int
}

// DocumentRights — оракул прав. Поиск права не разрешает: он спрашивает у
// оракула права СВОЕГО пользователя (ACLResolver), а права документа приходят в
// индекс на стадии индексации. Заглушка отвечает «наследование», и документ
// виден по своему контейнеру.
func (f *fakeDocs) DocumentRights(
	_ context.Context, _ ports.Queryer, _, _ string,
) (domain.DocumentRights, error) {
	return domain.DocumentRights{SpaceID: ports.InheritRights}, nil
}

func (f *fakeDocs) GetAllowedActiveVersions(
	context.Context, ports.Queryer, domain.Principal, string,
) ([]domain.ActiveVersion, error) {
	f.calls++
	return f.active, f.err
}

func (f *fakeDocs) VersionSources(
	context.Context, ports.Queryer, domain.Principal, string, map[string]string,
) (map[string]string, error) {
	return map[string]string{}, nil
}
func (f *fakeDocs) LoadParsed(
	context.Context, ports.Queryer, ports.FileStorage, string, string,
) (domain.ParsedDocument, error) {
	return domain.ParsedDocument{}, errUnusedPort
}

// LoadVersion стадии convert: поиск и ответы версию не читают.
func (f *fakeDocs) LoadVersion(
	context.Context, ports.Queryer, ports.FileStorage, string, string,
) (domain.ConvertVersion, error) {
	return domain.ConvertVersion{}, errUnusedPort
}
func (f *fakeDocs) ActivateVersion(
	context.Context, ports.Queryer, domain.ActivateParams,
) (string, error) {
	return "", errUnusedPort
}
func (f *fakeDocs) IngestBusy(context.Context, ports.Queryer, string) (bool, error) {
	return false, errUnusedPort
}
func (f *fakeDocs) IsVersionActive(context.Context, ports.Queryer, string, string) (bool, error) {
	return false, errUnusedPort
}

var errUnusedPort = errors.New("метод порта не используется сценарием")

// fakeQueryer не используется: он нужен только для сигнатур репозиториев.
type fakeQueryer struct{}

func (fakeQueryer) Query(context.Context, string, ...any) (ports.Rows, error) {
	return nil, errUnusedPort
}
func (fakeQueryer) QueryRow(context.Context, string, ...any) ports.Row {
	return nil
}
func (fakeQueryer) Exec(context.Context, string, ...any) (ports.CommandTag, error) {
	return nil, errUnusedPort
}

// fakeSettings отдаёт уровни настроек стенда.
type fakeSettings struct {
	tenantLevel map[string]any
	kbLevel     map[string]any
}

func (f *fakeSettings) Levels(
	context.Context, ports.Queryer, string, string,
) (map[string]any, map[string]any, error) {
	return f.kbLevel, f.tenantLevel, nil
}

// ── Общее ───────────────────────────────────────────────────────────

var (
	testKBID       = uuid.MustParse("22222222-2222-4222-8222-222222222222")
	testDocumentID = "33333333-3333-4333-8333-333333333333"
	testVersionID  = "44444444-4444-4444-8444-444444444444"
)

// generationKeys повторяет значения локального стенда (docs/settings.md).
func generationKeys() map[string]any {
	return map[string]any{
		settings.EmbeddingProviderID.Name():    "primary",
		settings.EmbeddingModel.Name():         "multilingual-e5-small",
		settings.EmbeddingRevision.Name():      "pilot-1",
		settings.EmbeddingDimension.Name():     int64(384),
		settings.EmbeddingNormalize.Name():     true,
		settings.EmbeddingDistance.Name():      "cosine",
		settings.ChunkingTokenizerID.Name():    "e5",
		settings.ChunkingMaxTokens.Name():      int64(600),
		settings.ChunkingOverlapTokens.Name():  int64(100),
		settings.SearchTopK.Name():             int64(10),
		settings.AnswerProviderID.Name():       "ollama",
		settings.AnswerModel.Name():            "qwen3:4b",
		settings.AnswerTokenizerID.Name():      "e5",
		settings.AnswerMaxContextTokens.Name(): int64(6000),
		settings.AnswerMaxOutputTokens.Name():  int64(1000),
		settings.ToolsEnabled.Name():           []any{"kb_search"},
	}
}

// chunkWithLinks — найденный фрагмент со ссылками на источник.
func chunkWithLinks(chunkID string, number int) domain.SearchCandidate {
	sourceURL := "https://portal.example/guide/export"
	return domain.SearchCandidate{
		ChunkID:     chunkID,
		DocumentID:  testDocumentID,
		VersionID:   testVersionID,
		ChunkNumber: number,
		Title:       "Инструкция по выгрузке",
		Text:        "Для настройки выгрузки откройте раздел Интеграции.",
		Locator: domain.Locator{
			BlockIDs:    []uuid.UUID{uuid.MustParse("55555555-5555-4555-8555-555555555555")},
			SectionPath: []string{"Настройка", "Выгрузка"},
		},
		SourceURL:   &sourceURL,
		DownloadURL: "/api/v1/documents/" + testDocumentID + "/source?version_id=" + testVersionID,
	}
}

func allowedDocs() []domain.ActiveVersion {
	return []domain.ActiveVersion{{
		DocumentID: testDocumentID, ActiveVersionID: testVersionID,
	}}
}

func newService(
	searcher *fakeSearch, llm *fakeLLM, docs *fakeDocs, tenantLevel map[string]any,
) *answers.Service {
	return answers.New(
		fakeQueryer{},
		searcher,
		llm,
		docs,
		settings.New(&fakeSettings{tenantLevel: tenantLevel}),
	)
}

func newOKService() (*answers.Service, *fakeSearch, *fakeLLM, *fakeDocs) {
	searcher := &fakeSearch{hits: []domain.SearchCandidate{
		chunkWithLinks("sha256:aaa", 1),
		chunkWithLinks("sha256:bbb", 2),
	}}
	llm := &fakeLLM{result: domain.GenerateResult{
		Answer:           "Откройте раздел Интеграции и настройте параметры выгрузки [1].",
		CitationChunkIDs: []string{"sha256:aaa"},
		ProviderID:       "ollama",
		Model:            "qwen3:4b",
	}}
	docs := &fakeDocs{active: allowedDocs()}
	return newService(searcher, llm, docs, generationKeys()), searcher, llm, docs
}

func testRequest() answers.Request {
	return answers.Request{KBID: testKBID, Question: "Как настроить выгрузку?"}
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}

// ── Успешный ответ ──────────────────────────────────────────────────

// TestAnswerBuildsCitationsFromRealChunks: цитата собирается сервером из
// найденного чанка, а номер совпадает с номером источника в ответе модели.
func TestAnswerBuildsCitationsFromRealChunks(t *testing.T) {
	svc, _, llm, _ := newOKService()

	out, err := svc.Answer(testCtx(t), domain.LocalPrincipal(), testRequest())
	if err != nil {
		t.Fatalf("ответ вернул ошибку: %v", err)
	}
	if out.Status != answers.StatusOK {
		t.Fatalf("status = %q, ожидался %q", out.Status, answers.StatusOK)
	}
	if len(out.Citations) != 1 {
		t.Fatalf("цитат %d, ожидалась 1", len(out.Citations))
	}
	citation := out.Citations[0]
	if citation.Number != 1 {
		t.Errorf("номер цитаты = %d, ожидался 1", citation.Number)
	}
	if citation.ChunkID != "sha256:aaa" {
		t.Errorf("chunk_id = %q, ожидался sha256:aaa", citation.ChunkID)
	}
	if citation.DocumentID != testDocumentID || citation.VersionID != testVersionID {
		t.Errorf("документ/версия = %q/%q", citation.DocumentID, citation.VersionID)
	}
	// Quote — реальный текст чанка, а не выдержка из ответа модели.
	if citation.Quote != "Для настройки выгрузки откройте раздел Интеграции." {
		t.Errorf("quote = %q, ожидался текст чанка", citation.Quote)
	}
	if citation.SourceURL != "https://portal.example/guide/export" {
		t.Errorf("source_url = %q", citation.SourceURL)
	}
	if citation.DownloadURL != "/api/v1/documents/"+testDocumentID+"/source?version_id="+testVersionID {
		t.Errorf("download_url = %q", citation.DownloadURL)
	}
	if len(citation.Locator.BlockIDs) != 1 ||
		citation.Locator.BlockIDs[0].String() != "55555555-5555-4555-8555-555555555555" {
		t.Errorf("локатор цитаты = %+v", citation.Locator)
	}
	if len(llm.reqs) != 1 {
		t.Fatalf("вызовов LLM %d, ожидался 1", len(llm.reqs))
	}
}

// TestAnswerPassesSettingsToLLM: провайдеру уходят provider_id, модель и
// лимиты из настроек базы, а не значения по умолчанию адаптера.
func TestAnswerPassesSettingsToLLM(t *testing.T) {
	svc, _, llm, _ := newOKService()

	if _, err := svc.Answer(testCtx(t), domain.LocalPrincipal(), testRequest()); err != nil {
		t.Fatalf("ответ вернул ошибку: %v", err)
	}
	req := llm.reqs[0]
	if req.ProviderID != "ollama" || req.Model != "qwen3:4b" {
		t.Errorf("провайдер/модель = %q/%q, ожидались ollama/qwen3:4b", req.ProviderID, req.Model)
	}
	if req.MaxContextTokens != 6000 || req.MaxOutputTokens != 1000 {
		t.Errorf("лимиты = %d/%d, ожидались 6000/1000", req.MaxContextTokens, req.MaxOutputTokens)
	}
	if req.SearchTool == nil {
		t.Fatal("SearchTool = nil: инструмент kb_search обязан быть доступен")
	}
	if req.MaxToolRounds != answers.MaxToolRounds || req.ToolResultLimit != answers.ToolResultLimit {
		t.Errorf("параметры цикла = %d/%d", req.MaxToolRounds, req.ToolResultLimit)
	}
	if len(req.Known) != 2 {
		t.Errorf("фрагментов в контексте %d, ожидалось 2", len(req.Known))
	}
}

// TestAnswerDropsFabricatedCitations: ссылка на чанк, которого не было в
// выдаче, отбрасывается (ТЗ 7.5 п.5).
func TestAnswerDropsFabricatedCitations(t *testing.T) {
	svc, _, llm, _ := newOKService()
	llm.result.CitationChunkIDs = []string{"sha256:aaa", "sha256:выдуманный"}

	out, err := svc.Answer(testCtx(t), domain.LocalPrincipal(), testRequest())
	if err != nil {
		t.Fatalf("ответ вернул ошибку: %v", err)
	}
	if len(out.Citations) != 1 || out.Citations[0].ChunkID != "sha256:aaa" {
		t.Fatalf("цитаты %v, ожидалась только настоящая", citationIDs(out.Citations))
	}
}

// TestAnswerKeepsCitationOrderAndDropsDuplicates: порядок цитат задаёт модель,
// повторы схлопываются.
func TestAnswerKeepsCitationOrderAndDropsDuplicates(t *testing.T) {
	svc, _, llm, _ := newOKService()
	llm.result.CitationChunkIDs = []string{"sha256:bbb", "sha256:aaa", "sha256:bbb"}

	out, err := svc.Answer(testCtx(t), domain.LocalPrincipal(), testRequest())
	if err != nil {
		t.Fatalf("ответ вернул ошибку: %v", err)
	}
	if len(out.Citations) != 2 {
		t.Fatalf("цитат %d, ожидалось 2", len(out.Citations))
	}
	if out.Citations[0].ChunkID != "sha256:bbb" || out.Citations[1].ChunkID != "sha256:aaa" {
		t.Errorf("порядок цитат нарушен: %v", citationIDs(out.Citations))
	}
	if out.Citations[1].Number != 2 {
		t.Errorf("второй цитате присвоен номер %d", out.Citations[1].Number)
	}
}

// ── Пустой материал ─────────────────────────────────────────────────

// TestAnswerWithoutEvidenceSkipsLLM: при пустой выдаче модель не вызывается, а
// ответ нейтральный и без цитат (ТЗ 7.5 п.2).
func TestAnswerWithoutEvidenceSkipsLLM(t *testing.T) {
	searcher := &fakeSearch{}
	llm := &fakeLLM{result: domain.GenerateResult{Answer: "что-то придумано"}}
	docs := &fakeDocs{active: allowedDocs()}
	svc := newService(searcher, llm, docs, generationKeys())

	out, err := svc.Answer(testCtx(t), domain.LocalPrincipal(), testRequest())
	if err != nil {
		t.Fatalf("ответ вернул ошибку: %v", err)
	}
	if out.Status != answers.StatusNoEvidence {
		t.Errorf("status = %q, ожидался %q", out.Status, answers.StatusNoEvidence)
	}
	if out.Answer != answers.NoEvidenceAnswer {
		t.Errorf("answer = %q, ожидался нейтральный текст", out.Answer)
	}
	if out.Citations == nil {
		t.Error("citations = nil: в JSON поле должно быть []")
	}
	if len(out.Citations) != 0 {
		t.Errorf("цитат %d, ожидалось 0", len(out.Citations))
	}
	if llm.calls != 0 {
		t.Errorf("LLM вызван без материала: %d раз", llm.calls)
	}
}

// TestAnswerWithoutKnownCitationsIsNoEvidence: ответ без единой подтверждённой
// ссылки не публикуется вместе с текстом модели (ТЗ 7.5 п.5).
func TestAnswerWithoutKnownCitationsIsNoEvidence(t *testing.T) {
	svc, _, llm, _ := newOKService()
	llm.result.Answer = "Откройте раздел Интеграции."
	llm.result.CitationChunkIDs = nil

	out, err := svc.Answer(testCtx(t), domain.LocalPrincipal(), testRequest())
	if err != nil {
		t.Fatalf("ответ вернул ошибку: %v", err)
	}
	if out.Status != answers.StatusNoEvidence {
		t.Errorf("status = %q, ожидался %q", out.Status, answers.StatusNoEvidence)
	}
	if strings.Contains(out.Answer, "Интеграции") {
		t.Errorf("в ответе остался неподтверждённый текст модели: %q", out.Answer)
	}
	if len(out.Citations) != 0 {
		t.Errorf("цитат %d, ожидалось 0", len(out.Citations))
	}
}

// ── Повторная проверка прав ─────────────────────────────────────────

// TestAnswerRechecksVersionActivity: если между поиском и ответом версия
// сменилась, цитата на прежнюю версию не публикуется (ТЗ 7.5 п.6).
func TestAnswerRechecksVersionActivity(t *testing.T) {
	svc, _, _, docs := newOKService()
	docs.active = []domain.ActiveVersion{{
		DocumentID: testDocumentID,
		// Версия обновилась после поиска.
		ActiveVersionID: "99999999-9999-4999-8999-999999999999",
	}}

	out, err := svc.Answer(testCtx(t), domain.LocalPrincipal(), testRequest())
	if err != nil {
		t.Fatalf("ответ вернул ошибку: %v", err)
	}
	if out.Status != answers.StatusNoEvidence {
		t.Errorf("status = %q, ожидался %q", out.Status, answers.StatusNoEvidence)
	}
	if len(out.Citations) != 0 {
		t.Errorf("цитат %d, ожидалось 0", len(out.Citations))
	}
}

// TestAnswerRechecksDocumentAccess: отзыв доступа после поиска убирает цитату
// так же, как и смена версии.
func TestAnswerRechecksDocumentAccess(t *testing.T) {
	svc, _, _, docs := newOKService()
	docs.active = nil

	out, err := svc.Answer(testCtx(t), domain.LocalPrincipal(), testRequest())
	if err != nil {
		t.Fatalf("ответ вернул ошибку: %v", err)
	}
	if out.Status != answers.StatusNoEvidence {
		t.Errorf("status = %q, ожидался %q", out.Status, answers.StatusNoEvidence)
	}
	if docs.calls == 0 {
		t.Error("список активных версий не перечитывался перед сборкой цитат")
	}
}

// ── Отказ LLM ───────────────────────────────────────────────────────

// TestAnswerPropagatesLLMError: сбой модели — доменная ошибка 503, а не
// no_evidence: клиент должен отличить «нет материала» от «сервис лежит».
func TestAnswerPropagatesLLMError(t *testing.T) {
	svc, _, llm, _ := newOKService()
	llmErr := domain.LLMUnavailable("LLM недоступна")
	llm.err = llmErr

	out, err := svc.Answer(testCtx(t), domain.LocalPrincipal(), testRequest())
	if !errors.Is(err, llmErr) {
		t.Fatalf("ошибка = %v, ожидалась %v", err, llmErr)
	}
	appErr, ok := domain.AsAppError(err)
	if !ok || appErr.Status != 503 {
		t.Fatalf("статус ошибки = %v, ожидался 503", appErr)
	}
	if out.Status != "" {
		t.Errorf("при ошибке отдаётся частичный ответ: status %q", out.Status)
	}
}

// TestAnswerPropagatesInvalidResponse: нечитаемый ответ модели остаётся 502.
func TestAnswerPropagatesInvalidResponse(t *testing.T) {
	svc, _, llm, _ := newOKService()
	llmErr := domain.LLMResponseInvalid("Ответ модели не разобран")
	llm.err = llmErr

	_, err := svc.Answer(testCtx(t), domain.LocalPrincipal(), testRequest())
	if !errors.Is(err, llmErr) {
		t.Fatalf("ошибка = %v, ожидалась %v", err, llmErr)
	}
}

// TestAnswerFailsWhenSearchFails: отказ поиска не превращается в «нет
// материалов»: иначе пользователь получил бы уверенный ответ о пустой базе.
func TestAnswerFailsWhenSearchFails(t *testing.T) {
	searchErr := domain.IndexUnavailable("Поисковый запрос не выполнен")
	searcher := &fakeSearch{err: searchErr}
	llm := &fakeLLM{}
	svc := newService(searcher, llm, &fakeDocs{active: allowedDocs()}, generationKeys())

	if _, err := svc.Answer(testCtx(t), domain.LocalPrincipal(), testRequest()); !errors.Is(err, searchErr) {
		t.Fatalf("ошибка = %v, ожидалась %v", err, searchErr)
	}
	if llm.calls != 0 {
		t.Errorf("LLM вызван при отказе поиска: %d раз", llm.calls)
	}
}

// ── Конфигурация и отмена ───────────────────────────────────────────

// TestAnswerFailsWhenNoChunkFitsContext: фрагменты не влезают в
// answer.max_context_tokens — 409, а не ответ без цитат.
func TestAnswerFailsWhenNoChunkFitsContext(t *testing.T) {
	keys := generationKeys()
	keys[settings.AnswerMaxContextTokens.Name()] = int64(settings.InstructionReserveTokens + 10)
	llm := &fakeLLM{result: domain.GenerateResult{Answer: "что-то придумано"}}
	svc := newService(&fakeSearch{hits: []domain.SearchCandidate{chunkWithLinks("sha256:aaa", 1)}},
		llm, &fakeDocs{active: allowedDocs()}, keys)

	_, err := svc.Answer(testCtx(t), domain.LocalPrincipal(), testRequest())
	appErr, ok := domain.AsAppError(err)
	if !ok || appErr.Code != domain.CodeConfigurationInvalid {
		t.Fatalf("ошибка = %v, ожидался код %s", err, domain.CodeConfigurationInvalid)
	}
	if llm.calls != 0 {
		t.Errorf("LLM вызван без контекста: %d раз", llm.calls)
	}
}

// TestAnswerSkipsChunksOverBudget: фрагменты, не поместившиеся в бюджет,
// отбрасываются, а ответ строится на оставшихся.
func TestAnswerSkipsChunksOverBudget(t *testing.T) {
	keys := generationKeys()
	// Бюджет вмещает примерно два фрагмента из трёх.
	keys[settings.AnswerMaxContextTokens.Name()] = int64(settings.InstructionReserveTokens + 40)
	searcher := &fakeSearch{hits: []domain.SearchCandidate{
		chunkWithLinks("sha256:aaa", 1),
		chunkWithLinks("sha256:bbb", 2),
		chunkWithLinks("sha256:ccc", 3),
	}}
	llm := &fakeLLM{result: domain.GenerateResult{
		Answer:           "Откройте раздел Интеграции [1].",
		CitationChunkIDs: []string{"sha256:aaa"},
	}}
	svc := newService(searcher, llm, &fakeDocs{active: allowedDocs()}, keys)

	out, err := svc.Answer(testCtx(t), domain.LocalPrincipal(), testRequest())
	if err != nil {
		t.Fatalf("ответ вернул ошибку: %v", err)
	}
	if out.Status != answers.StatusOK {
		t.Errorf("status = %q, ожидался %q", out.Status, answers.StatusOK)
	}
	if len(llm.reqs[0].Known) >= 3 {
		t.Errorf("в контекст ушли все фрагменты: %d", len(llm.reqs[0].Known))
	}
}

// TestAnswerFailsWhenToolsDisabled: без kb_search в tools.enabled настройка
// некорректна, и отвечать нечем.
func TestAnswerFailsWhenToolsDisabled(t *testing.T) {
	keys := generationKeys()
	keys[settings.ToolsEnabled.Name()] = []any{}
	llm := &fakeLLM{result: domain.GenerateResult{Answer: "что-то придумано"}}
	svc := newService(&fakeSearch{hits: []domain.SearchCandidate{chunkWithLinks("sha256:aaa", 1)}},
		llm, &fakeDocs{active: allowedDocs()}, keys)

	_, err := svc.Answer(testCtx(t), domain.LocalPrincipal(), testRequest())
	appErr, ok := domain.AsAppError(err)
	if !ok || appErr.Code != domain.CodeConfigurationInvalid {
		t.Fatalf("ошибка = %v, ожидался код %s", err, domain.CodeConfigurationInvalid)
	}
}

// TestAnswerStopsOnCancelledContext: отменённый запрос не вызывает модель.
func TestAnswerStopsOnCancelledContext(t *testing.T) {
	svc, searcher, llm, _ := newOKService()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := svc.Answer(ctx, domain.LocalPrincipal(), testRequest()); !errors.Is(err, context.Canceled) {
		t.Fatalf("ошибка = %v, ожидался context.Canceled", err)
	}
	if searcher.queries != nil {
		t.Errorf("поиск выполнен после отмены: %v", searcher.queries)
	}
	if llm.calls != 0 {
		t.Errorf("LLM вызван после отмены: %d раз", llm.calls)
	}
}

// ── Помощники ───────────────────────────────────────────────────────

func citationIDs(citations []domain.Citation) []string {
	out := make([]string, 0, len(citations))
	for _, item := range citations {
		out = append(out, item.ChunkID)
	}
	return out
}
