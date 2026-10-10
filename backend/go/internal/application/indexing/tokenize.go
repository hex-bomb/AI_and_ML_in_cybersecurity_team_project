// Package indexing runs the index stage: chunking, embeddings, activation.
package indexing

import "strings"

// CharsPerToken is the rough estimator shared by chunking and answer context.
// Changing it reshapes chunk boundaries and chunk_ids, forcing full reindex.
const CharsPerToken = 4

// EstimateTokens returns len in runes // 4, minimum 1 for non-blank text.
// Rune-based: byte counting would halve chunks on Cyrillic text.
func EstimateTokens(text string) int {
	stripped := strings.TrimSpace(text)
	if stripped == "" {
		return 0
	}
	n := len([]rune(stripped)) / CharsPerToken
	if n < 1 {
		return 1
	}
	return n
}
