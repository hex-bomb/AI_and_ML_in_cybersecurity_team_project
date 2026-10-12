package ports

import (
	"context"

	domain "github.com/kbsearch/kb/internal/envelope"
)

// FileStorage stores originals and parsed documents.
type FileStorage interface {
	Put(ctx context.Context, key string, data []byte) error
	Read(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, key string) error
}

// Embedder encodes texts with the configured profile.
type Embedder interface {
	Embed(ctx context.Context, texts []string, cfg domain.IndexingConfig) ([][]float32, error)
	EmbedQuery(ctx context.Context, text string, cfg domain.IndexingConfig) ([]float32, error)
	Dimensions() int
	Ping(ctx context.Context) error
	Close() error
}

// SearchIndex persists chunks and serves retrieval.
//
// Legacy access path: KeywordSearch/VectorSearch take a domain.SearchFilter
// whose AllowedDocumentIDs is a pre-materialized list of what the principal may
// read (roadmap §6 forbids post-filter — the allowed chunk would already be
// in memory and in the LLM context). The seam that carries rights inside the
// query is SecuredSearchIndex in seams.go; search/service still builds the old
// filter and owns its migration, so nothing here is deleted and callers on
// either side keep compiling.
type SearchIndex interface {
	EnsureIndex(ctx context.Context, cfg domain.IndexingConfig) (string, error)
	UpsertChunks(ctx context.Context, cfg domain.IndexingConfig, chunks []domain.Chunk) (int, error)
	DeleteVersion(ctx context.Context, tenantID, documentID, versionID string) (int, error)
	DeleteDocument(ctx context.Context, tenantID, documentID string) (int, error)
	KeywordSearch(ctx context.Context, text string, f domain.SearchFilter, limit int) ([]domain.SearchCandidate, error)
	VectorSearch(ctx context.Context, vec []float32, cfg domain.IndexingConfig, f domain.SearchFilter, limit int) ([]domain.SearchCandidate, error)
	Ping(ctx context.Context) error
}

// LLMProvider generates answers from retrieved context.
type LLMProvider interface {
	Generate(ctx context.Context, req domain.GenerateRequest) (domain.GenerateResult, error)
}
