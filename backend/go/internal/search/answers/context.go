package answers

import (
	"github.com/kbsearch/kb/internal/docs/settings"
	domain "github.com/kbsearch/kb/internal/envelope"
)

// buildContext keeps whole hits that fit the answer context budget, in
// search order. Empty selection is a configuration error, not an empty answer.
func buildContext(
	maxContextTokens int, hits []domain.SearchCandidate,
) ([]domain.SearchCandidate, error) {
	budget := maxContextTokens - settings.InstructionReserveTokens
	if budget <= 0 {
		return nil, contextBudgetError(maxContextTokens, 0)
	}

	out := make([]domain.SearchCandidate, 0, len(hits))
	used := 0
	for _, hit := range hits {
		cost := chunkTokens(hit)
		// Skipped, not terminal: a later shorter fragment may still fit.
		if used+cost > budget {
			continue
		}
		used += cost
		out = append(out, hit)
	}
	if len(out) == 0 {
		needed := 0
		if len(hits) > 0 {
			needed = chunkTokens(hits[0])
		}
		return nil, contextBudgetError(maxContextTokens, needed)
	}
	return out, nil
}

// chunkTokens estimates a fragment size including its title.
// Считает тем же domain.EstimateTokens, что и нарезка: цитата должна
// попадать в тот же фрагмент, границы которого посчитал индекс.
func chunkTokens(hit domain.SearchCandidate) int {
	return domain.EstimateTokens(hit.Title) + domain.EstimateTokens(hit.Text)
}

func contextBudgetError(maxContextTokens, needed int) error {
	return domain.ConfigurationInvalid(
		"answer.max_context_tokens не вмещает ни одного найденного фрагмента",
		map[string]any{
			"max_context_tokens":  maxContextTokens,
			"instruction_reserve": settings.InstructionReserveTokens,
			"needed_tokens":       needed,
		},
	)
}
