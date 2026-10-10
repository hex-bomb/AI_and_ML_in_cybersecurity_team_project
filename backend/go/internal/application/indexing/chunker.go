package indexing

import (
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/kbsearch/backend-go/internal/domain"
)

// Chunk splits a parsed document into deterministic chunks: same document and
// profile always give the same boundaries and chunk_ids, keeping reindex idempotent.
// Blocks never merge across section_path; overlap applies within one section only.
func Chunk(doc domain.ParsedDocument, cfg domain.IndexingConfig) ([]Draft, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	ordered := orderedBlocks(doc.Blocks)
	drafts := make([]Draft, 0, len(ordered))
	for _, section := range sectionsByPath(ordered) {
		header := sectionHeader(section[0].SectionPath)
		budget, err := pieceBudget(header, cfg)
		if err != nil {
			return nil, err
		}
		units := unitsOfSection(section, budget, cfg)
		drafts = append(drafts, packSection(units, header, section[0].SectionPath, cfg)...)
	}
	if len(drafts) == 0 {
		return nil, emptyDocument()
	}

	for i := range drafts {
		drafts[i].Number = i
		drafts[i].ChunkID = domain.ChunkIDFor(domain.ChunkIDParams{
			VersionID:   doc.VersionID.String(),
			ProfileID:   cfg.ProfileID(),
			TokenizerID: cfg.TokenizerID,
			MaxTokens:   cfg.ChunkMaxTokens,
			Overlap:     cfg.ChunkOverlapTokens,
			Number:      i,
		})
	}
	return drafts, nil
}

// Draft is a chunk before embedding; it cannot go to the index without a vector.
type Draft struct {
	// ChunkID is deterministic and survives process restarts.
	ChunkID string
	// Number is the zero-based chunk position in the document.
	Number      int
	Text        string
	SectionPath []string
	BlockIDs    []uuid.UUID
	// PageStart/PageEnd stay nil for formats without pages (MD, DOCX).
	PageStart *int
	PageEnd   *int
}

// unit is a block fragment packed into a chunk whole.
type unit struct {
	blockID uuid.UUID
	text    string
	tokens  int
	page    *int
}

const chunkSeparator = "\n\n"

const sectionSeparator = " > "

// packSection packs one section's units; an over-long block arrives pre-split.
func packSection(units []unit, header string, sectionPath []string, cfg domain.IndexingConfig) []Draft {
	out := make([]Draft, 0, len(units))
	current := make([]unit, 0, len(units))

	for _, u := range units {
		if len(current) > 0 && chunkTokens(current, u, header) > cfg.ChunkMaxTokens {
			out = append(out, buildDraft(current, header, sectionPath))
			current = overlapTail(current, u, header, cfg)
		}
		current = append(current, u)
	}
	if len(current) > 0 {
		out = append(out, buildDraft(current, header, sectionPath))
	}
	return out
}

// overlapTail carries whole trailing units into the next chunk. Nil means the
// overlap does not fit next to the incoming block.
func overlapTail(window []unit, next unit, header string, cfg domain.IndexingConfig) []unit {
	for start := len(window); start > 0; start-- {
		candidate := window[start-1:]
		if unitTokens(candidate) > cfg.ChunkOverlapTokens {
			return nil
		}
		if chunkTokens(candidate, next, header) <= cfg.ChunkMaxTokens {
			return candidate
		}
	}
	return nil
}

const separatorReserve = 1

// pieceBudget is what a block keeps after the repeated section header.
func pieceBudget(header string, cfg domain.IndexingConfig) (int, error) {
	spent := EstimateTokens(header) + separatorReserve
	if spent >= cfg.ChunkMaxTokens {
		return 0, domain.ConfigurationInvalid(
			"Путь раздела не помещается в chunk_max_tokens",
			map[string]any{"section_path": header, "chunk_max_tokens": cfg.ChunkMaxTokens},
		)
	}
	return cfg.ChunkMaxTokens - spent, nil
}

func unitsOfSection(blocks []domain.Block, budget int, cfg domain.IndexingConfig) []unit {
	units := make([]unit, 0, len(blocks))
	for _, b := range blocks {
		units = append(units, splitBlock(b, budget, cfg)...)
	}
	return units
}

// splitBlock splits an over-long block with overlap; boundaries are in runes
// so chunk_ids stay deterministic without the real model tokenizer.
func splitBlock(b domain.Block, budget int, cfg domain.IndexingConfig) []unit {
	tokens := EstimateTokens(b.Text)
	if tokens == 0 {
		return nil
	}
	if tokens <= budget {
		return []unit{{blockID: b.BlockID, text: b.Text, tokens: tokens, page: b.Page}}
	}

	runes := []rune(b.Text)
	limit := budget * CharsPerToken
	advance := max(limit-cfg.ChunkOverlapTokens*CharsPerToken, 1)

	units := make([]unit, 0, len(runes)/advance+1)
	for start := 0; start < len(runes); {
		end := min(start+limit, len(runes))
		if end < len(runes) {
			end = breakAtBoundary(runes, start, end)
		}
		if text := strings.TrimSpace(string(runes[start:end])); text != "" {
			units = append(units, unit{
				blockID: b.BlockID,
				text:    text,
				tokens:  EstimateTokens(text),
				page:    b.Page,
			})
		}
		if end >= len(runes) {
			break
		}
		start = max(min(start+advance, end), start+1)
	}
	return units
}

// breakAtBoundary backs off to the last word boundary inside the window.
func breakAtBoundary(runes []rune, start, end int) int {
	floor := max(start+1, end-(end-start)/2)
	for i := end; i > floor; i-- {
		switch runes[i-1] {
		case ' ', '\n', '\t':
			return i
		}
	}
	return end
}

// sectionsByPath splits by adjacency, not by value: same-path blocks divided
// by another section never share a chunk.
func sectionsByPath(blocks []domain.Block) [][]domain.Block {
	var out [][]domain.Block
	for _, b := range blocks {
		if n := len(out); n > 0 && slices.Equal(out[n-1][0].SectionPath, b.SectionPath) {
			out[n-1] = append(out[n-1], b)
			continue
		}
		out = append(out, []domain.Block{b})
	}
	return out
}

// orderedBlocks sorts by position, stable so equal positions stay file-ordered.
func orderedBlocks(blocks []domain.Block) []domain.Block {
	out := slices.Clone(blocks)
	slices.SortStableFunc(out, func(a, b domain.Block) int { return a.Position - b.Position })
	return out
}

func buildDraft(units []unit, header string, sectionPath []string) Draft {
	parts := make([]string, 0, len(units)+1)
	if header != "" {
		parts = append(parts, header)
	}
	blockIDs := make([]uuid.UUID, 0, len(units))
	for _, u := range units {
		parts = append(parts, u.text)
		if len(blockIDs) == 0 || blockIDs[len(blockIDs)-1] != u.blockID {
			blockIDs = append(blockIDs, u.blockID)
		}
	}

	start, end := pageRange(units)
	return Draft{
		Text:        strings.Join(parts, chunkSeparator),
		SectionPath: slices.Clone(sectionPath),
		BlockIDs:    blockIDs,
		PageStart:   start,
		PageEnd:     end,
	}
}

// pageRange spans pages of units that have one; nil/nil when none do.
func pageRange(units []unit) (*int, *int) {
	var from, to *int
	for _, u := range units {
		if u.page == nil {
			continue
		}
		if from == nil || *u.page < *from {
			value := *u.page
			from = &value
		}
		if to == nil || *u.page > *to {
			value := *u.page
			to = &value
		}
	}
	return from, to
}

// sectionHeader is indexed text: it makes fragments findable by section name.
func sectionHeader(sectionPath []string) string {
	return strings.Join(sectionPath, sectionSeparator)
}

func unitTokens(units []unit) int {
	total := 0
	for _, u := range units {
		total += u.tokens
	}
	return total
}

func chunkTokens(window []unit, next unit, header string) int {
	parts := make([]string, 0, len(window)+1)
	if header != "" {
		parts = append(parts, header)
	}
	for _, u := range window {
		parts = append(parts, u.text)
	}
	parts = append(parts, next.text)
	return EstimateTokens(strings.Join(parts, chunkSeparator))
}

func emptyDocument() *domain.AppError {
	return domain.InvalidMetadata("Разобранный документ не содержит текста для индексации")
}
