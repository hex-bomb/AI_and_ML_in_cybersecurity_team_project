package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kbsearch/kb/internal/envelope"
)

// Тесты идут на httptest.Server: протокол /api/chat проверяется целиком,
// включая разбор tool_calls, а живая Ollama не нужна и не должна быть условием

// ── Заглушка сервера ────────────────────────────────────────────────

// fakeOllama отвечает заранее заданными сообщениями по очереди и записывает
// все запросы.
type fakeOllama struct {
	// replies — ответы по очереди; последний повторяется, когда очередь
	// кончилась (модель, так и не переставшая звать инструмент).
	replies  []chatResponse
	statuses []int
	requests []chatRequest
	// answerBody используется, когда replies пуст.
	answerBody string
}

func (f *fakeOllama) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != chatPath {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.requests = append(f.requests, req)

	status := http.StatusOK
	if len(f.statuses) > len(f.requests)-1 {
		status = f.statuses[len(f.requests)-1]
	}
	if status != http.StatusOK {
		w.WriteHeader(status)
		return
	}

	reply := chatResponse{}
	switch {
	case len(f.replies) == 0:
		reply.Message = chatMessage{Role: roleAssistant, Content: f.answerBody}
	case len(f.requests) <= len(f.replies):
		reply = f.replies[len(f.requests)-1]
	default:
		// Последняя заготовка повторяется: так модели ведут себя, когда
		// ответ всё ещё не получен.
		reply = f.replies[len(f.replies)-1]
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(reply)
}

// toolReply — ответ с вызовом инструмента. arguments обязаны быть корректным
// JSON: ответ модели Ollama тоже валиден, а битый аргумент приходит внутри
func toolReply(name string, arguments json.RawMessage) chatResponse {
	return chatResponse{Message: chatMessage{
		Role:    roleAssistant,
		Content: "",
		ToolCalls: []toolCall{{
			Function: toolFunction{Name: name, Arguments: arguments},
		}},
	}}
}

// toolArgs собирает аргументы вызова: значения подставляются как JSON, а не
// склеиваются строкой.
func toolArgs(query string) json.RawMessage {
	payload, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		// Недостижимо: карта из одной строки всегда сериализуется.
		panic(err)
	}
	return payload
}

// answerReply — ответ с готовым JSON.
func answerReply(body string) chatResponse {
	return chatResponse{Message: chatMessage{Role: roleAssistant, Content: body}}
}

func newProvider(t *testing.T, srv *httptest.Server) *Provider {
	t.Helper()
	return New(Config{BaseURL: srv.URL, Temperature: 0.75})
}

func testRequest() domain.GenerateRequest {
	return domain.GenerateRequest{
		Question: "Как настроить выгрузку?",
		Known: []domain.SearchCandidate{
			{ChunkID: "sha256:aaa", Title: "Инструкция", Text: "Откройте раздел Интеграции."},
			{ChunkID: "sha256:bbb", Title: "Регламент", Text: "Выгрузка выполняется по расписанию."},
		},
		MaxContextTokens: 6000,
		MaxOutputTokens:  1000,
		ProviderID:       "ollama",
		Model:            "qwen3:4b",
		MaxToolRounds:    2,
		ToolResultLimit:  5,
		ToolRoundTokens:  256,
	}
}

func candidate(chunkID, text string) domain.SearchCandidate {
	return domain.SearchCandidate{ChunkID: chunkID, Title: "Найдено", Text: text}
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}

// ── Обычная генерация ───────────────────────────────────────────────

// TestGenerateWithoutToolCalls: модель отвечает сразу — один запрос, цитаты по
// номерам фрагментов приводятся к реальным chunk_id.
func TestGenerateWithoutToolCalls(t *testing.T) {
	fake := &fakeOllama{replies: []chatResponse{
		answerReply(`{"answer": "Откройте раздел Интеграции [1].", "citation_chunk_ids": ["1"]}`),
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	res, err := newProvider(t, srv).Generate(testCtx(t), testRequest())
	if err != nil {
		t.Fatalf("генерация вернула ошибку: %v", err)
	}
	if len(fake.requests) != 1 {
		t.Errorf("запросов %d, ожидался 1", len(fake.requests))
	}
	if res.Answer != "Откройте раздел Интеграции [1]." {
		t.Errorf("ответ = %q", res.Answer)
	}
	if len(res.CitationChunkIDs) != 1 || res.CitationChunkIDs[0] != "sha256:aaa" {
		t.Errorf("цитаты %v, ожидался sha256:aaa", res.CitationChunkIDs)
	}
	if len(res.CitationSources) != 1 || res.CitationSources[0].Text != "Откройте раздел Интеграции." {
		t.Errorf("источники цитат = %+v", res.CitationSources)
	}
	if res.ProviderID != "ollama" || res.Model != "qwen3:4b" {
		t.Errorf("провайдер/модель = %q/%q", res.ProviderID, res.Model)
	}
}

// TestGenerateSendsSystemPromptAndChunks: модель получает инструкцию, вопрос и
// пронумерованные фрагменты.
func TestGenerateSendsSystemPromptAndChunks(t *testing.T) {
	fake := &fakeOllama{replies: []chatResponse{
		answerReply(`{"answer": "По расписанию [2].", "citation_chunk_ids": ["2"]}`),
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	if _, err := newProvider(t, srv).Generate(testCtx(t), testRequest()); err != nil {
		t.Fatalf("генерация вернула ошибку: %v", err)
	}
	if len(fake.requests[0].Messages) != 2 {
		t.Fatalf("сообщений %d, ожидалось 2", len(fake.requests[0].Messages))
	}
	system := fake.requests[0].Messages[0]
	if system.Role != roleSystem || !strings.Contains(system.Content, "kb_search") {
		t.Errorf("инструкция модели = %+v", system)
	}
	user := fake.requests[0].Messages[1]
	if !strings.Contains(user.Content, "Как настроить выгрузку?") {
		t.Errorf("вопрос не передан: %q", user.Content)
	}
	if !strings.Contains(user.Content, "[1] chunk_id: sha256:aaa") {
		t.Errorf("фрагменты не пронумерованы: %q", user.Content)
	}
	if !strings.Contains(user.Content, "[2] chunk_id: sha256:bbb") {
		t.Errorf("второй фрагмент не показан: %q", user.Content)
	}
}

// TestGenerateRetriesWithSchemaWhenAnswerIsNotJSON: принудительная JSON-схема
// применяется при разборе ответа без схемы.
func TestGenerateRetriesWithSchemaWhenAnswerIsNotJSON(t *testing.T) {
	fake := &fakeOllama{replies: []chatResponse{
		answerReply("Откройте раздел Интеграции."),
		answerReply(`{"answer": "Откройте раздел Интеграции [1].", "citation_chunk_ids": ["1"]}`),
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	res, err := newProvider(t, srv).Generate(testCtx(t), testRequest())
	if err != nil {
		t.Fatalf("генерация вернула ошибку: %v", err)
	}
	if len(fake.requests) != 2 {
		t.Fatalf("запросов %d, ожидалось 2", len(fake.requests))
	}
	if fake.requests[1].Format == nil {
		t.Error("повтор сделан без принудительной JSON-схемы")
	}
	if len(fake.requests[1].Tools) != 0 {
		t.Error("в повторе инструмент остался доступен: модель могла бы звать его снова")
	}
	if len(res.CitationChunkIDs) != 1 {
		t.Errorf("цитаты %v", res.CitationChunkIDs)
	}
}

// TestGenerateStripsCodeFence: ответ, обёрнутый в ```json, разбирается.
func TestGenerateStripsCodeFence(t *testing.T) {
	fake := &fakeOllama{replies: []chatResponse{
		answerReply("```json\n{\"answer\": \"По расписанию [2].\", \"citation_chunk_ids\": [\"2\"]}\n```"),
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	res, err := newProvider(t, srv).Generate(testCtx(t), testRequest())
	if err != nil {
		t.Fatalf("генерация вернула ошибку: %v", err)
	}
	if res.CitationChunkIDs[0] != "sha256:bbb" {
		t.Errorf("цитаты %v, ожидался sha256:bbb", res.CitationChunkIDs)
	}
}

// ── Цикл инструмента ───────────────────────────────────────────────

// TestGenerateRunsSearchTool: вызов kb_search исполняется сценарием, его
// результат уходит модели, и цитата может ссылаться на найденное инструментом.
func TestGenerateRunsSearchTool(t *testing.T) {
	fake := &fakeOllama{replies: []chatResponse{
		toolReply(ToolNameKBSearch, toolArgs("расписание выгрузки")),
		answerReply(`{"answer": "По расписанию [3].", "citation_chunk_ids": ["3"]}`),
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	var queries []string
	req := testRequest()
	req.SearchTool = func(_ context.Context, query string) ([]domain.SearchCandidate, error) {
		queries = append(queries, query)
		return []domain.SearchCandidate{candidate("sha256:ccc", "Выгрузка по вторникам.")}, nil
	}

	res, err := newProvider(t, srv).Generate(testCtx(t), req)
	if err != nil {
		t.Fatalf("генерация вернула ошибку: %v", err)
	}
	if len(queries) != 1 || queries[0] != "расписание выгрузки" {
		t.Errorf("запросы инструмента = %v", queries)
	}
	if len(fake.requests) != 2 {
		t.Fatalf("запросов %d, ожидалось 2", len(fake.requests))
	}
	if len(fake.requests[0].Tools) != 1 {
		t.Errorf("инструмент не объявлен: %+v", fake.requests[0].Tools)
	}
	// Нумерация найденного инструментом продолжает сквозную: [3] — третий
	// фрагмент, а не первый в новом блоке.
	toolMessage := fake.requests[1].Messages[len(fake.requests[1].Messages)-1]
	if toolMessage.Role != roleTool || toolMessage.ToolName != ToolNameKBSearch {
		t.Errorf("сообщение инструмента = %+v", toolMessage)
	}
	if !strings.Contains(toolMessage.Content, "[3] chunk_id: sha256:ccc") {
		t.Errorf("нумерация результата инструмента сбилась: %q", toolMessage.Content)
	}
	if len(res.CitationChunkIDs) != 1 || res.CitationChunkIDs[0] != "sha256:ccc" {
		t.Errorf("цитаты %v, ожидался sha256:ccc", res.CitationChunkIDs)
	}
	if len(res.CitationSources) != 1 || res.CitationSources[0].Text != "Выгрузка по вторникам." {
		t.Errorf("источник цитаты = %+v", res.CitationSources)
	}
}

// TestGenerateSurvivesToolCallWithoutQuery: вызов инструмента без аргумента
// query не роняет ответ — модели возвращается «ничего не найдено», и цикл
func TestGenerateSurvivesToolCallWithoutQuery(t *testing.T) {
	fake := &fakeOllama{replies: []chatResponse{
		toolReply(ToolNameKBSearch, json.RawMessage(`{"q": "расписание"}`)),
		answerReply(`{"answer": "По найденному [1].", "citation_chunk_ids": ["1"]}`),
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	called := false
	req := testRequest()
	req.SearchTool = func(context.Context, string) ([]domain.SearchCandidate, error) {
		called = true
		return nil, nil
	}

	res, err := newProvider(t, srv).Generate(testCtx(t), req)
	if err != nil {
		t.Fatalf("генерация вернула ошибку: %v", err)
	}
	if called {
		t.Error("поиск выполнен без аргумента query")
	}
	toolMessage := fake.requests[1].Messages[len(fake.requests[1].Messages)-1]
	if toolMessage.Content != noFindings {
		t.Errorf("ответ инструмента = %q, ожидалось %q", toolMessage.Content, noFindings)
	}
	if res.CitationChunkIDs[0] != "sha256:aaa" {
		t.Errorf("цитаты %v", res.CitationChunkIDs)
	}
}

// TestGenerateIgnoresUnknownTool: вызов неизвестного инструмента получает
// отказ, а не падение.
func TestGenerateIgnoresUnknownTool(t *testing.T) {
	fake := &fakeOllama{replies: []chatResponse{
		toolReply("send_email", json.RawMessage(`{"to": "a@b.c"}`)),
		answerReply(`{"answer": "По найденному [1].", "citation_chunk_ids": ["1"]}`),
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	req := testRequest()
	req.SearchTool = func(context.Context, string) ([]domain.SearchCandidate, error) {
		t.Error("вызван инструмент, которого нет в настройках")
		return nil, nil
	}

	if _, err := newProvider(t, srv).Generate(testCtx(t), req); err != nil {
		t.Fatalf("генерация вернула ошибку: %v", err)
	}
	toolMessage := fake.requests[1].Messages[len(fake.requests[1].Messages)-1]
	if toolMessage.Content != noFindings {
		t.Errorf("ответ неизвестному инструменту = %q", toolMessage.Content)
	}
}

// TestGenerateSuppressesSearchError: сбой инструмента не роняет ответ — модель
// получает «ничего не найдено» и отвечает по имеющемуся материалу.
func TestGenerateSuppressesSearchError(t *testing.T) {
	searchErr := domain.IndexUnavailable("Поисковый запрос не выполнен")
	fake := &fakeOllama{replies: []chatResponse{
		toolReply(ToolNameKBSearch, toolArgs("расписание")),
		answerReply(`{"answer": "По найденному [1].", "citation_chunk_ids": ["1"]}`),
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	req := testRequest()
	req.SearchTool = func(context.Context, string) ([]domain.SearchCandidate, error) {
		return nil, searchErr
	}

	res, err := newProvider(t, srv).Generate(testCtx(t), req)
	if err != nil {
		t.Fatalf("генерация вернула ошибку: %v", err)
	}
	if res.Answer == "" {
		t.Error("ответ пуст")
	}
}

// TestGenerateLimitsToolResults: один вызов инструмента возвращает не больше
// ToolResultLimit фрагментов (ТЗ 7.5 п.3).
func TestGenerateLimitsToolResults(t *testing.T) {
	fake := &fakeOllama{replies: []chatResponse{
		toolReply(ToolNameKBSearch, toolArgs("выгрузка")),
		answerReply(`{"answer": "По найденному [1].", "citation_chunk_ids": ["1"]}`),
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	req := testRequest()
	req.ToolResultLimit = 2
	req.SearchTool = func(context.Context, string) ([]domain.SearchCandidate, error) {
		return []domain.SearchCandidate{
			candidate("sha256:c1", "Первый"),
			candidate("sha256:c2", "Второй"),
			candidate("sha256:c3", "Третий"),
			candidate("sha256:c4", "Четвёртый"),
		}, nil
	}

	if _, err := newProvider(t, srv).Generate(testCtx(t), req); err != nil {
		t.Fatalf("генерация вернула ошибку: %v", err)
	}
	toolMessage := fake.requests[1].Messages[len(fake.requests[1].Messages)-1]
	if strings.Contains(toolMessage.Content, "sha256:c3") {
		t.Errorf("инструмент вернул больше результатов, чем ToolResultLimit: %q", toolMessage.Content)
	}
}

// TestGenerateStopsAfterMaxToolRounds: зацикливание модели ограничено. Запросов
// ровно MaxToolRounds + 1, последний — без инструмента.
func TestGenerateStopsAfterMaxToolRounds(t *testing.T) {
	fake := &fakeOllama{replies: []chatResponse{
		toolReply(ToolNameKBSearch, toolArgs("раз")),
		toolReply(ToolNameKBSearch, toolArgs("два")),
		answerReply(`{"answer": "По найденному [1].", "citation_chunk_ids": ["1"]}`),
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	calls := 0
	req := testRequest()
	req.SearchTool = func(context.Context, string) ([]domain.SearchCandidate, error) {
		calls++
		return nil, nil
	}

	res, err := newProvider(t, srv).Generate(testCtx(t), req)
	if err != nil {
		t.Fatalf("генерация вернула ошибку: %v", err)
	}
	if calls != req.MaxToolRounds {
		t.Errorf("вызовов инструмента %d, ожидалось %d", calls, req.MaxToolRounds)
	}
	if len(fake.requests) != req.MaxToolRounds+1 {
		t.Errorf("запросов %d, ожидалось %d", len(fake.requests), req.MaxToolRounds+1)
	}
	last := fake.requests[len(fake.requests)-1]
	if len(last.Tools) != 0 {
		t.Error("последний запрос сделан с доступным инструментом: цикл мог бы не закончиться")
	}
	if last.Format == nil {
		t.Error("последний запрос без принудительной JSON-схемы")
	}
	if res.Answer == "" {
		t.Error("ответ пуст")
	}
}

// TestGenerateDoesNotRetryFinalRequest: ответ финального раунда разбирается
// один раз — иначе нечитаемый JSON превращался бы в бесконечный повтор.
func TestGenerateDoesNotRetryFinalRequest(t *testing.T) {
	fake := &fakeOllama{replies: []chatResponse{
		toolReply(ToolNameKBSearch, toolArgs("раз")),
		toolReply(ToolNameKBSearch, toolArgs("два")),
		answerReply("всё равно не JSON"),
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	req := testRequest()
	req.SearchTool = func(context.Context, string) ([]domain.SearchCandidate, error) {
		return nil, nil
	}

	if _, err := newProvider(t, srv).Generate(testCtx(t), req); err == nil {
		t.Fatal("ожидалась ошибка разбора ответа")
	}
	if len(fake.requests) != req.MaxToolRounds+1 {
		t.Errorf("запросов %d, ожидалось %d", len(fake.requests), req.MaxToolRounds+1)
	}
}

// ── Отказы ──────────────────────────────────────────────────────────

// TestGenerateFailsWhenOllamaUnavailable: недоступный Ollama даёт 503
// LLM_UNAVAILABLE, а не 500.
func TestGenerateFailsWhenOllamaUnavailable(t *testing.T) {
	srv := httptest.NewServer(&fakeOllama{})
	provider := newProvider(t, srv)
	srv.Close() // порт закрыт до запроса

	_, err := provider.Generate(testCtx(t), testRequest())
	assertAppError(t, err, domain.CodeLLMUnavailable, 503)
}

// TestGenerateFailsOnServerError: 5xx — тоже недоступность.
func TestGenerateFailsOnServerError(t *testing.T) {
	fake := &fakeOllama{statuses: []int{http.StatusBadGateway}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	_, err := newProvider(t, srv).Generate(testCtx(t), testRequest())
	assertAppError(t, err, domain.CodeLLMUnavailable, 503)
}

// TestGenerateFailsOnRejectedRequest: 4xx — некорректный ответ модели, повтор
// не поможет.
func TestGenerateFailsOnRejectedRequest(t *testing.T) {
	fake := &fakeOllama{statuses: []int{http.StatusNotFound}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	_, err := newProvider(t, srv).Generate(testCtx(t), testRequest())
	assertAppError(t, err, domain.CodeLLMResponseInvalid, 502)
}

// TestGenerateFailsOnBrokenResponse: нечитаемое тело ответа — 502.
func TestGenerateFailsOnBrokenResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{не json"))
	}))
	defer srv.Close()

	_, err := newProvider(t, srv).Generate(testCtx(t), testRequest())
	assertAppError(t, err, domain.CodeLLMResponseInvalid, 502)
}

// TestGenerateFailsOnEmptyAnswer: пустой ответ модели — 502, а no_evidence
// должен решать сценарий, получив разобранный результат.
func TestGenerateFailsOnEmptyAnswer(t *testing.T) {
	fake := &fakeOllama{answerBody: `{"answer": "   ", "citation_chunk_ids": []}`}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	_, err := newProvider(t, srv).Generate(testCtx(t), testRequest())
	assertAppError(t, err, domain.CodeLLMResponseInvalid, 502)
}

// TestGenerateFailsWithoutBaseURL: незаданный адрес LLM — 503 с внятным
// текстом, а не nil-указатель.
func TestGenerateFailsWithoutBaseURL(t *testing.T) {
	_, err := New(Config{}).Generate(testCtx(t), testRequest())
	assertAppError(t, err, domain.CodeLLMUnavailable, 503)
	if !strings.Contains(err.Error(), "LLM_PRIMARY_BASE_URL") {
		t.Errorf("сообщение не называет переменную: %v", err)
	}
}

// TestGenerateStopsOnCancelledContext: отменённый запрос не уходит к модели.
func TestGenerateStopsOnCancelledContext(t *testing.T) {
	fake := &fakeOllama{answerBody: `{"answer": "ok", "citation_chunk_ids": []}`}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := newProvider(t, srv).Generate(ctx, testRequest())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ошибка = %v, ожидался context.Canceled", err)
	}
	if len(fake.requests) != 0 {
		t.Errorf("запросов %d, ожидалось 0", len(fake.requests))
	}
}

// ── Цитаты ──────────────────────────────────────────────────────────

// TestResolveCitationsAcceptsChunkIDsAndNumbers: допускаются и полный
// идентификатор, и номер фрагмента, но только из увиденного.
func TestResolveCitationsAcceptsChunkIDsAndNumbers(t *testing.T) {
	known := newKnownSet([]domain.SearchCandidate{
		{ChunkID: "sha256:aaa"},
		{ChunkID: "sha256:bbb"},
	})

	cases := []struct {
		name   string
		ids    []string
		answer string
		want   []string
	}{
		{
			name: "полные идентификаторы",
			ids:  []string{"sha256:bbb", "sha256:aaa"},
			want: []string{"sha256:bbb", "sha256:aaa"},
		},
		{
			name: "номера фрагментов",
			ids:  []string{"2", "1"},
			want: []string{"sha256:bbb", "sha256:aaa"},
		},
		{
			name: "номер выдуманного фрагмента отбрасывается",
			ids:  []string{"1", "9"},
			want: []string{"sha256:aaa"},
		},
		{
			name: "чужой идентификатор отбрасывается",
			ids:  []string{"sha256:чужой"},
			want: nil,
		},
		{
			name:   "ссылки из текста, когда список пуст",
			ids:    nil,
			answer: "Смотри [2] и снова [2], а вот [1].",
			want:   []string{"sha256:bbb", "sha256:aaa"},
		},
		{
			name:   "ссылка из текста на чужой номер игнорируется",
			ids:    nil,
			answer: "Смотри [7].",
			want:   nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveCitations(tc.ids, tc.answer, known)
			if len(got) != len(tc.want) {
				t.Fatalf("цитаты %v, ожидались %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("цитаты %v, ожидались %v", got, tc.want)
				}
			}
		})
	}
}

// TestResolveCitationsIsDeterministic: порядок задаёт модель, повторы
// схлопываются — одинаковый вход даёт одинаковый выход.
func TestResolveCitationsIsDeterministic(t *testing.T) {
	known := newKnownSet([]domain.SearchCandidate{
		{ChunkID: "sha256:aaa"},
		{ChunkID: "sha256:bbb"},
		{ChunkID: "sha256:ccc"},
	})
	ids := []string{"3", "1", "3", "2", "1"}

	first := resolveCitations(ids, "", known)
	for i := 0; i < 5; i++ {
		again := resolveCitations(ids, "", known)
		if len(again) != len(first) {
			t.Fatalf("длина изменилась на повторе %d: %v", i, again)
		}
		for j := range first {
			if again[j] != first[j] {
				t.Fatalf("порядок изменился на повторе %d:\n%v\n%v", i, again, first)
			}
		}
	}
	want := []string{"sha256:ccc", "sha256:aaa", "sha256:bbb"}
	for i := range want {
		if first[i] != want[i] {
			t.Fatalf("цитаты %v, ожидались %v", first, want)
		}
	}
}

// TestParseToolQueryAcceptsBothArgumentForms: аргументы приходят и объектом,
// и строкой с JSON внутри. Всё остальное — пустой запрос, а не текст запроса:
func TestParseToolQueryAcceptsBothArgumentForms(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"объект", `{"query": "расписание выгрузки"}`, "расписание выгрузки"},
		{"объект с лишними полями", `{"query": "выгрузка", "top_k": 5}`, "выгрузка"},
		{"строка с JSON внутри", `"{\"query\": \"выгрузка\"}"`, "выгрузка"},
		{"пустой запрос", `{"query": ""}`, ""},
		{"нет поля query", `{"q": "выгрузка"}`, ""},
		{"голая строка", `"выгрузка"`, ""},
		{"не JSON", `{`, ""},
		{"пусто", ``, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseToolQuery(json.RawMessage(tc.raw)); got != tc.want {
				t.Errorf("parseToolQuery(%s) = %q, ожидалось %q", tc.raw, got, tc.want)
			}
		})
	}
}

// ── Разбор ответа ───────────────────────────────────────────────────

func TestParseAnswerRejectsGarbage(t *testing.T) {
	cases := []string{
		"",
		"просто текст",
		"{\"answer\": \"\", \"citation_chunk_ids\": []}",
		"```json\nне json\n```",
		"```незакрытая ограда",
	}
	for _, raw := range cases {
		if _, err := parseAnswer(raw); err == nil {
			t.Errorf("parseAnswer(%q) вернул nil-ошибку", raw)
		}
	}
}

func TestParseAnswerAcceptsCodeFence(t *testing.T) {
	got, err := parseAnswer("```json\n{\"answer\": \"Текст [1].\", \"citation_chunk_ids\": [\"1\"]}\n```")
	if err != nil {
		t.Fatalf("разбор обёрнутого ответа вернул ошибку: %v", err)
	}
	if got.Answer != "Текст [1]." || len(got.CitationChunkIDs) != 1 {
		t.Errorf("разобранный ответ = %+v", got)
	}
}

// ── Помощники ───────────────────────────────────────────────────────

// assertAppError проверяет код и HTTP-статус доменной ошибки.
func assertAppError(t *testing.T, err error, code string, status int) {
	t.Helper()
	if err == nil {
		t.Fatalf("ожидалась ошибка с кодом %s", code)
	}
	appErr, ok := domain.AsAppError(err)
	if !ok {
		t.Fatalf("ошибка не доменная: %v", err)
	}
	if appErr.Code != code {
		t.Errorf("код = %s, ожидался %s", appErr.Code, code)
	}
	if appErr.Status != status {
		t.Errorf("статус = %d, ожидался %d", appErr.Status, status)
	}
	if appErr.Cause == nil {
		t.Log("исходная ошибка не сохранена в Cause — диагностика в логе будет беднее")
	}
}
