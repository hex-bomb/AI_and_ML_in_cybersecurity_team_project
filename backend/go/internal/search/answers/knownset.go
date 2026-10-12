package answers

import (
	"strings"

	"github.com/kbsearch/kb/internal/envelope"
)

// knownSet keeps fragments in the order the model saw them; the position
// is the citation number, so reordering would misdirect references.
type knownSet struct {
	items []domain.SearchCandidate
	byID  map[string]int
}

func newKnownSet(initial []domain.SearchCandidate) *knownSet {
	set := &knownSet{byID: make(map[string]int, len(initial))}
	set.add(initial...)
	return set
}

// add appends unseen fragments; duplicates keep their original position.
func (s *knownSet) add(items ...domain.SearchCandidate) {
	for _, item := range items {
		if _, dup := s.byID[item.ChunkID]; dup {
			continue
		}
		s.byID[item.ChunkID] = len(s.items)
		s.items = append(s.items, item)
	}
}

// resolveCitations keeps only ids the model actually saw, in first-mention order.
func resolveCitations(ids []string, known *knownSet) []domain.SearchCandidate {
	seen := make(map[string]struct{}, len(ids))
	out := make([]domain.SearchCandidate, 0, len(ids))
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		item, ok := known.get(id)
		if !ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, item)
	}
	return out
}

// buildCitations assembles server-side quotes from the real chunk text.
func buildCitations(sources []domain.SearchCandidate) []domain.Citation {
	out := make([]domain.Citation, 0, len(sources))
	for i, source := range sources {
		citation := domain.Citation{
			Number:      i + 1,
			ChunkID:     source.ChunkID,
			DocumentID:  source.DocumentID,
			VersionID:   source.VersionID,
			Title:       source.Title,
			Quote:       strings.TrimSpace(source.Text),
			Locator:     source.Locator,
			DownloadURL: source.DownloadURL,
		}
		if source.SourceURL != nil {
			citation.SourceURL = *source.SourceURL
		}
		out = append(out, citation)
	}
	return out
}

func (s *knownSet) get(chunkID string) (domain.SearchCandidate, bool) {
	n, ok := s.byID[chunkID]
	if !ok {
		return domain.SearchCandidate{}, false
	}
	return s.items[n], true
}
