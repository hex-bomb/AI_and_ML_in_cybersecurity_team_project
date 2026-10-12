package ollama

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/kbsearch/kb/internal/envelope"
)

// knownSet хранит фрагменты в порядке показа модели (список, не карта — номера стабильны).
type knownSet struct {
	items []domain.SearchCandidate
	seen  map[string]struct{}
}

func newKnownSet(initial []domain.SearchCandidate) *knownSet {
	set := &knownSet{seen: make(map[string]struct{}, len(initial))}
	set.add(initial...)
	return set
}

// add дополняет набор, не меняя порядок уже добавленного.
func (s *knownSet) add(items ...domain.SearchCandidate) {
	for _, item := range items {
		if _, dup := s.seen[item.ChunkID]; dup {
			continue
		}
		s.seen[item.ChunkID] = struct{}{}
		s.items = append(s.items, item)
	}
}

func (s *knownSet) len() int { return len(s.items) }

func (s *knownSet) get(chunkID string) (domain.SearchCandidate, bool) {
	for _, item := range s.items {
		if item.ChunkID == chunkID {
			return item, true
		}
	}
	return domain.SearchCandidate{}, false
}

// byNumber строит карту «номер фрагмента -> chunk_id».
func (s *knownSet) byNumber() map[string]string {
	out := make(map[string]string, len(s.items))
	for i, item := range s.items {
		out[strconv.Itoa(i+1)] = item.ChunkID
	}
	return out
}

// citeMarkers ловит ссылки вида [1] в тексте ответа.
var citeMarkers = regexp.MustCompile(`\[(\d+)\]`)

// resolveCitations приводит цитаты к chunk_id: годятся и номера, и идентификаторы
// (порядок модели, повторы отбрасываются, ТЗ 7.5 п.5).
func resolveCitations(ids []string, answer string, known *knownSet) []string {
	byNumber := known.byNumber()
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))

	appendID := func(raw string) {
		if _, dup := seen[raw]; dup {
			return
		}
		if _, ok := known.seen[raw]; !ok {
			return
		}
		seen[raw] = struct{}{}
		out = append(out, raw)
	}

	for _, raw := range ids {
		value := strings.TrimSpace(raw)
		if resolved, ok := byNumber[value]; ok {
			appendID(resolved)
			continue
		}
		appendID(value)
	}

	if len(out) == 0 {
		// Фолбэк: номера ссылок прямо из текста ответа.
		for _, match := range citeMarkers.FindAllStringSubmatch(answer, -1) {
			appendID(byNumber[match[1]])
		}
	}
	return out
}
