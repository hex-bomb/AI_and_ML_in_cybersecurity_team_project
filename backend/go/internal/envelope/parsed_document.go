package domain

import (
	"github.com/google/uuid"
)

// Block is one block of a parsed document. Page nil means page: None.
// JSON tags must match the Python model keys exactly; the type travels
// between the Python converter and the Go indexer through a file.
type Block struct {
	BlockID     uuid.UUID `json:"block_id"`
	Position    int       `json:"position"`
	Kind        string    `json:"kind"`
	Text        string    `json:"text"`
	SectionPath []string  `json:"section_path"`
	Page        *int      `json:"page"`
}

// ParsedDocument is the converter output (ТЗ 3.2).
type ParsedDocument struct {
	SchemaVersion string         `json:"schema_version"`
	TenantID      uuid.UUID      `json:"tenant_id"`
	KBID          uuid.UUID      `json:"kb_id"`
	DocumentID    uuid.UUID      `json:"document_id"`
	VersionID     uuid.UUID      `json:"version_id"`
	Title         string         `json:"title"`
	Filename      string         `json:"filename"`
	ContentSHA256 string         `json:"content_sha256"`
	SourceURL     *string        `json:"source_url"`
	Metadata      map[string]any `json:"metadata"`
	Blocks        []Block        `json:"blocks"`
}

func (d ParsedDocument) SourceURLValue() (string, bool) {
	if d.SourceURL == nil {
		return "", false
	}
	return *d.SourceURL, true
}

func (d ParsedDocument) BlockByID(id uuid.UUID) (Block, bool) {
	for _, b := range d.Blocks {
		if b.BlockID == id {
			return b, true
		}
	}
	return Block{}, false
}
