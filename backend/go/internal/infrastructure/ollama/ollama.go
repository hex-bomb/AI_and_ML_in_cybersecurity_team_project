// Package ollama — порт ports.LLMProvider поверх HTTP /api/chat Ollama
// (напрямую: в langchaingo нет tool_calls для kb_search, ТЗ 7.5 п.4).
package ollama

import (
	"context"
	"net/http"
	"time"

	"github.com/kbsearch/backend-go/internal/application/ports"
	"github.com/kbsearch/backend-go/internal/domain"
)

// Значения по умолчанию (смена молча меняет поведение ответа).
const (
	// DefaultTimeout — потолок одного запроса к провайдеру (ТЗ 2.4).
	DefaultTimeout = 60 * time.Second
	// DefaultMaxToolRounds — сколько раундов с инструментом до финального
	// ответа.
	DefaultMaxToolRounds = 2
	// DefaultToolResultLimit — сколько фрагментов возвращает один вызов
	// инструмента.
	DefaultToolResultLimit = 5
	// DefaultToolRoundTokens — короткий бюджет раунда с инструментом: пока
	// инструмент доступен, от модели нужен только вызов.
	DefaultToolRoundTokens = 256
	// DefaultMaxOutputTokens — предел ответа, когда сценарий не задал свой.
	DefaultMaxOutputTokens = 1000
)

// ToolNameKBSearch — единственный инструмент первого релиза (ТЗ 1.2).
const ToolNameKBSearch = "kb_search"

// noFindings — ответ инструменту (текстом: сообщение tool обязано нести результат).
const noFindings = "Ничего не найдено."

// Config — параметры подключения к Ollama (адрес и ключ из окружения, ТЗ 6.1).
type Config struct {
	// BaseURL — корень Ollama, без пути /api/chat.
	BaseURL string
	// APIKey необязателен: локальный Ollama его не требует.
	APIKey string
	// Temperature без подстановки: ноль — допустимое значение.
	Temperature float64
	// Timeout на запрос (ноль — DefaultTimeout).
	Timeout time.Duration
	// HTTPClient переопределяет клиент целиком.
	HTTPClient *http.Client
}

// Provider — реализация ports.LLMProvider поверх Ollama (без состояния).
type Provider struct {
	baseURL     string
	apiKey      string
	temperature float64
	httpClient  *http.Client
}

// New собирает провайдера (пустой адрес — позже честный 503, поиск работает, ТЗ 10.1).
func New(cfg Config) *Provider {
	client := cfg.HTTPClient
	if client == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = DefaultTimeout
		}
		client = &http.Client{Timeout: timeout}
	}
	return &Provider{
		baseURL:     cfg.BaseURL,
		apiKey:      cfg.APIKey,
		temperature: cfg.Temperature,
		httpClient:  client,
	}
}

// Generate спрашивает модель с потолком раундов kb_search и финальным
// запросом без инструментов (ТЗ 7.5 п.3-5).
func (p *Provider) Generate(
	ctx context.Context, req domain.GenerateRequest,
) (domain.GenerateResult, error) {
	if p.baseURL == "" {
		return domain.GenerateResult{}, domain.LLMUnavailable("Не задан адрес LLM (LLM_PRIMARY_BASE_URL)")
	}
	if err := ctx.Err(); err != nil {
		return domain.GenerateResult{}, err
	}

	known := newKnownSet(req.Known)
	messages := initialMessages(req.Question, known)
	rounds := positive(req.MaxToolRounds, DefaultMaxToolRounds)

	for round := 0; round < rounds; round++ {
		answer, err := p.chat(ctx, chatRequest{
			Model:    req.Model,
			Messages: messages,
			Tools:    []toolSpec{kbSearchSpec()},
			Stream:   false,
			Think:    false,
			Options: chatOptions{
				Temperature: p.temperature,
				NumPredict:  positive(req.ToolRoundTokens, DefaultToolRoundTokens),
			},
		})
		if err != nil {
			return domain.GenerateResult{}, err
		}
		if len(answer.ToolCalls) == 0 {
			return p.answer(ctx, req, known, messages, answer)
		}
		messages = appendToolResults(ctx, append(messages, answer), answer, req, known)
	}

	// Раунды исчерпаны, а модель всё зовёт инструмент: последний запрос уходит
	// без инструментов, и ответить обязан текстом.
	final, err := p.chat(ctx, p.finalRequest(req, messages))
	if err != nil {
		return domain.GenerateResult{}, err
	}
	structured, err := parseAnswer(final.Content)
	if err != nil {
		return domain.GenerateResult{}, err
	}
	return result(req, known, structured), nil
}

// answer разбирает ответ раунда с инструментом (неразобранный — один повтор со схемой).
func (p *Provider) answer(
	ctx context.Context,
	req domain.GenerateRequest,
	known *knownSet,
	messages []chatMessage,
	answer chatMessage,
) (domain.GenerateResult, error) {
	structured, err := parseAnswer(answer.Content)
	if err == nil {
		return result(req, known, structured), nil
	}

	final, chatErr := p.chat(ctx, p.finalRequest(req, messages))
	if chatErr != nil {
		return domain.GenerateResult{}, chatErr
	}
	structured, parseErr := parseAnswer(final.Content)
	if parseErr != nil {
		return domain.GenerateResult{}, parseErr
	}
	return result(req, known, structured), nil
}

// finalRequest — запрос финального ответа: без инструментов и с JSON-схемой.
func (p *Provider) finalRequest(req domain.GenerateRequest, messages []chatMessage) chatRequest {
	return chatRequest{
		Model:    req.Model,
		Messages: messages,
		Stream:   false,
		Think:    false,
		Format:   answerFormat(),
		Options: chatOptions{
			Temperature: p.temperature,
			NumPredict:  positive(req.MaxOutputTokens, DefaultMaxOutputTokens),
		},
	}
}

// appendToolResults исполняет вызовы инструмента и дописывает их в переписку.
func appendToolResults(
	ctx context.Context,
	messages []chatMessage,
	answer chatMessage,
	req domain.GenerateRequest,
	known *knownSet,
) []chatMessage {
	for _, call := range answer.ToolCalls {
		messages = append(messages, chatMessage{
			Role:     roleTool,
			ToolName: call.Function.Name,
			Content:  runSearch(ctx, call, req, known),
		})
	}
	return messages
}

// runSearch выполняет kb_search (ошибка подавляется; нумерация сквозная).
func runSearch(
	ctx context.Context,
	call toolCall,
	req domain.GenerateRequest,
	known *knownSet,
) string {
	if call.Function.Name != ToolNameKBSearch || req.SearchTool == nil {
		return noFindings
	}
	query := parseToolQuery(call.Function.Arguments)
	if query == "" {
		return noFindings
	}
	found, err := req.SearchTool(ctx, query)
	if err != nil {
		return noFindings
	}
	limit := positive(req.ToolResultLimit, DefaultToolResultLimit)
	if len(found) > limit {
		found = found[:limit]
	}
	if len(found) == 0 {
		return noFindings
	}

	offset := known.len()
	known.add(found...)
	return formatChunks(found, offset)
}

// result собирает ответ порта (источники — из known, не из ответа модели).
func result(
	req domain.GenerateRequest, known *knownSet, structured structuredAnswer,
) domain.GenerateResult {
	ids := resolveCitations(structured.CitationChunkIDs, structured.Answer, known)
	sources := make([]domain.SearchCandidate, 0, len(ids))
	for _, id := range ids {
		if source, ok := known.get(id); ok {
			sources = append(sources, source)
		}
	}
	return domain.GenerateResult{
		Answer:           structured.Answer,
		CitationChunkIDs: ids,
		CitationSources:  sources,
		ProviderID:       req.ProviderID,
		Model:            req.Model,
	}
}

// positive возвращает значение, если оно задано, и запасное иначе.
func positive(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}

var _ ports.LLMProvider = (*Provider)(nil)
