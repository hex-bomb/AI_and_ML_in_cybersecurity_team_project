package domain

import "github.com/google/uuid"

// Locator is a chunk position in the source document.
type Locator struct {
	BlockIDs    []uuid.UUID `json:"block_ids"`
	SectionPath []string    `json:"section_path"`
	PageStart   *int        `json:"page_start"`
	PageEnd     *int        `json:"page_end"`
}

// Chunk is an immutable document fragment ready for indexing.
// IDs are strings (chunk_id is a sha256 hex string, not a UUID).
type Chunk struct {
	SchemaVersion string
	ChunkID       string
	TenantID      string
	KBID          string
	DocumentID    string
	VersionID     string
	// Rights rides with the chunk into the index (§6): контейнер документа и
	// точечные исключения. Права едут вместе с документом на каждом шаге, а
	// чанк без них бесполезен поиску — terms по ненаписанному полю не совпадёт
	// ни с чем, и фильтр прав вернёт пустоту всем подряд.
	Rights             DocumentRights
	ChunkNumber        int
	Title              string
	Text               string
	Locator            Locator
	EmbeddingProfileID string
	Embedding          []float32
}

// ChunkIDParams is the deterministic chunk identifier input.
type ChunkIDParams struct {
	VersionID   string
	ProfileID   string
	TokenizerID string
	MaxTokens   int
	Overlap     int
	Number      int
}

// ChunkIDFor returns the deterministic chunk id: same inputs give the same
// id across restarts, which reindex idempotency relies on.
func ChunkIDFor(p ChunkIDParams) string {
	payload := emptyCanonicalObject().
		withInt("chunk_max_tokens", p.MaxTokens).
		withInt("chunk_number", p.Number).
		withInt("chunk_overlap_tokens", p.Overlap).
		withString("embedding_profile_id", p.ProfileID).
		withString("tokenizer_id", p.TokenizerID).
		withString("version_id", p.VersionID).
		encode()
	return sha256Prefixed(payload)
}
