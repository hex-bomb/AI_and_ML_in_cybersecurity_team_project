package domain

import "context"

// SearchToolFunc runs kb_search through the same SearchService.
type SearchToolFunc func(ctx context.Context, query string) ([]SearchCandidate, error)

// GenerateRequest is the Generate input.
type GenerateRequest struct {
	Question         string
	Known            []SearchCandidate
	MaxContextTokens int
	MaxOutputTokens  int
	ProviderID       string
	Model            string
	SearchTool       SearchToolFunc
	MaxToolRounds    int
	ToolResultLimit  int
	ToolRoundTokens  int
}

// GenerateResult is the structured answer with server-collected sources.
type GenerateResult struct {
	Answer           string
	CitationChunkIDs []string
	CitationSources  []SearchCandidate
	ProviderID       string
	Model            string
}
