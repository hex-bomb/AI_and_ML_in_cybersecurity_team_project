package ollama

import (
	"encoding/json"
	"strings"

	"github.com/kbsearch/backend-go/internal/domain"
)

// structuredAnswer — ответ модели (ТЗ 7.5 п.4).
type structuredAnswer struct {
	Answer           string   `json:"answer"`
	CitationChunkIDs []string `json:"citation_chunk_ids"`
}

// parseAnswer разбирает ответ модели (пустой — 502, не no_evidence).
func parseAnswer(raw string) (structuredAnswer, error) {
	body, ok := stripCodeFence(strings.TrimSpace(raw))
	if !ok {
		return structuredAnswer{}, domain.LLMResponseInvalid("Ответ модели не является JSON-объектом")
	}
	var out structuredAnswer
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		return structuredAnswer{}, domain.LLMResponseInvalid("Ответ модели не разобран").WithCause(err)
	}
	if strings.TrimSpace(out.Answer) == "" {
		return structuredAnswer{}, domain.LLMResponseInvalid("Модель вернула пустой ответ")
	}
	return out, nil
}

// stripCodeFence снимает обёртку ```json (модели добавляют её при отказе парсера).
func stripCodeFence(raw string) (string, bool) {
	if !strings.HasPrefix(raw, "```") {
		return raw, true
	}
	body, _, closed := strings.Cut(strings.TrimPrefix(raw, "```"), "```")
	if !closed {
		return "", false
	}
	// Первая строка внутри fence — язык разметки, а не данные.
	newline := strings.IndexByte(body, '\n')
	if newline < 0 {
		return "", false
	}
	return strings.TrimSpace(body[newline+1:]), true
}
