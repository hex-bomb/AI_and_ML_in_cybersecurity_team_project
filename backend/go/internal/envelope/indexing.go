package domain

import "strings"

// IndexingConfig is the immutable indexing profile.
type IndexingConfig struct {
	EmbeddingProviderID string
	EmbeddingModel      string
	EmbeddingRevision   string
	EmbeddingDimension  int
	ChunkMaxTokens      int
	ChunkOverlapTokens  int
	TokenizerID         string
	NormalizeVectors    bool
	Distance            string
}

// ProfileID hashes six vector-profile fields; chunking params are excluded
// so retuning chunking does not invalidate the index, while a model or
// dimension change does. It also feeds ChunkIDFor, so a profile change
// renames every chunk id.
func (c IndexingConfig) ProfileID() string {
	payload := emptyCanonicalObject().
		withString("distance", c.Distance).
		withInt("embedding_dimension", c.EmbeddingDimension).
		withString("embedding_model", c.EmbeddingModel).
		withString("embedding_provider_id", c.EmbeddingProviderID).
		withString("embedding_revision", c.EmbeddingRevision).
		withBool("normalize_vectors", c.NormalizeVectors).
		encode()
	return sha256Prefixed(payload)
}

// IndexName keeps the Python `kb-chunks-v1-<hex>` format so existing
// indexed data stays addressable after the vendor change.
func (c IndexingConfig) IndexName() string {
	return IndexNameForProfile(c.ProfileID())
}

// IndexPrefix is the physical index prefix shared by all profiles.
const IndexPrefix = "kb-chunks-v1"

func IndexNameForProfile(profileID string) string {
	return IndexPrefix + "-" + strings.TrimPrefix(profileID, "sha256:")
}

func (c IndexingConfig) Validate() error {
	if c.EmbeddingDimension <= 0 {
		return newConfigurationInvalid("embedding_dimension должен быть больше нуля")
	}
	if c.ChunkMaxTokens <= 0 {
		return newConfigurationInvalid("chunk_max_tokens должен быть больше нуля")
	}
	if c.ChunkOverlapTokens < 0 {
		return newConfigurationInvalid("chunk_overlap_tokens не может быть отрицательным")
	}
	if c.ChunkOverlapTokens >= c.ChunkMaxTokens {
		return newConfigurationInvalid("overlap должен быть меньше max_tokens")
	}
	if strings.TrimSpace(c.Distance) != "cosine" {
		return newConfigurationInvalid("в MVP поддерживается только cosine")
	}
	for _, required := range []struct{ name, value string }{
		{"embedding_provider_id", c.EmbeddingProviderID},
		{"embedding_model", c.EmbeddingModel},
		{"embedding_revision", c.EmbeddingRevision},
		{"tokenizer_id", c.TokenizerID},
	} {
		if strings.TrimSpace(required.value) == "" {
			return newConfigurationInvalid(required.name + " не может быть пустым")
		}
	}
	return nil
}
