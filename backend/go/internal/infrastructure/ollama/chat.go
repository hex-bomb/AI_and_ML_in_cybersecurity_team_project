package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/kbsearch/backend-go/internal/domain"
)

// Протокол /api/chat Ollama (терять поля нельзя — отключатся инструмент или схема).
const (
	chatPath = "/api/chat"

	roleSystem    = "system"
	roleUser      = "user"
	roleAssistant = "assistant"
	roleTool      = "tool"
)

// maxResponseBytes — предел чтения тела ответа.
const maxResponseBytes = 4 << 20

type chatMessage struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	ToolName  string     `json:"tool_name,omitempty"`
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
}

type toolCall struct {
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Name string `json:"name"`
	// Arguments приходит объектом JSON (RawMessage — чтение у вызывающего).
	Arguments json.RawMessage `json:"arguments"`
}

type toolSpec struct {
	Type     string           `json:"type"`
	Function toolFunctionSpec `json:"function"`
}

type toolFunctionSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type chatOptions struct {
	Temperature float64 `json:"temperature"`
	// NumPredict — предел токенов генерации (имя поля Ollama).
	NumPredict int `json:"num_predict"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Tools    []toolSpec    `json:"tools,omitempty"`
	// Format — принудительная схема (nil на раундах с инструментом).
	Format  *answerSchema `json:"format,omitempty"`
	Stream  bool          `json:"stream"`
	Think   bool          `json:"think"`
	Options chatOptions   `json:"options"`
}

type answerSchema struct {
	Type       string                    `json:"type"`
	Properties map[string]answerProperty `json:"properties"`
	Required   []string                  `json:"required"`
}

type answerProperty struct {
	Type  string          `json:"type"`
	Items *answerProperty `json:"items,omitempty"`
}

type chatResponse struct {
	Message chatMessage `json:"message"`
}

// chat делает один запрос /api/chat (503 LLM_UNAVAILABLE vs 502 LLM_RESPONSE_INVALID).
func (p *Provider) chat(ctx context.Context, body chatRequest) (chatMessage, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return chatMessage{}, domain.LLMResponseInvalid("Запрос к LLM не сериализуется").WithCause(err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+chatPath, bytes.NewReader(payload))
	if err != nil {
		return chatMessage{}, domain.LLMUnavailable("Запрос к LLM не собран").WithCause(err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	res, err := p.httpClient.Do(httpReq)
	if err != nil {
		return chatMessage{}, domain.LLMUnavailable("LLM недоступна").WithCause(err)
	}
	defer func() { _ = res.Body.Close() }()

	switch {
	case res.StatusCode >= 500:
		return chatMessage{}, domain.LLMUnavailable(fmt.Sprintf("LLM вернула %d", res.StatusCode))
	case res.StatusCode >= 400:
		// 4xx — отказ принять запрос, повтором не лечится.
		return chatMessage{}, domain.LLMResponseInvalid(fmt.Sprintf("LLM отклонила запрос: %d", res.StatusCode))
	}

	var out chatResponse
	if err := json.NewDecoder(io.LimitReader(res.Body, maxResponseBytes)).Decode(&out); err != nil {
		return chatMessage{}, domain.LLMResponseInvalid("Ответ LLM не разобран").WithCause(err)
	}
	return out.Message, nil
}

// kbSearchSpec объявляет единственный инструмент (ТЗ 1.2).
func kbSearchSpec() toolSpec {
	return toolSpec{
		Type: "function",
		Function: toolFunctionSpec{
			Name: ToolNameKBSearch,
			Description: "Поиск по корпоративной базе знаний. Вызывай, когда в уже " +
				"полученном материале нет ответа или нужен другой ракурс вопроса.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Поисковый запрос на русском языке, 1–2000 символов",
					},
				},
				"required": []string{"query"},
			},
		},
	}
}

// answerFormat — принудительная схема {answer, citation_chunk_ids} (ТЗ 7.5 п.4).
func answerFormat() *answerSchema {
	return &answerSchema{
		Type: "object",
		Properties: map[string]answerProperty{
			"answer":             {Type: "string"},
			"citation_chunk_ids": {Type: "array", Items: &answerProperty{Type: "string"}},
		},
		Required: []string{"answer", "citation_chunk_ids"},
	}
}

// parseToolQuery достаёт query из вызова (строка с JSON внутри разбирается ещё раз).
func parseToolQuery(raw json.RawMessage) string {
	var args struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(raw, &args); err == nil {
		return args.Query
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if err := json.Unmarshal([]byte(text), &args); err == nil {
			return args.Query
		}
	}
	return ""
}
