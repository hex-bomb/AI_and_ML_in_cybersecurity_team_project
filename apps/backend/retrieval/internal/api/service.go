package api

import (
	"context"
	"log/slog"

	"vektor/retrieval/internal/search"
)

// HybridSearch — ядро: гибридный поиск с fallback на лексику и слиянием RRF.
type HybridSearch struct {
	es       *search.ES
	embedder *search.Embedder
}

func NewHybridSearch(es *search.ES, embedder *search.Embedder) *HybridSearch {
	return &HybridSearch{es: es, embedder: embedder}
}

// Search возвращает слитые хиты. Ошибка — только когда недоступны обе системы
// поиска; отказ одной — деградация, а не отказ сервиса.
func (s *HybridSearch) Search(ctx context.Context, scope search.Scope, query string, topK int) ([]search.Hit, error) {
	size := search.Size(topK)

	// Вектор недоступен — работаем на лексике, это не ошибка.
	vector, err := s.embedder.Embed(ctx, query)
	if err != nil {
		slog.Warn("embedding unavailable, lexical only", "err", err)
	}

	lex, lexErr := s.es.Search(ctx, search.LexicalBody(scope, query, size))

	var vec search.SearchResult
	var vecErr error
	if vector != nil {
		vec, vecErr = s.es.Search(ctx, search.KnnBody(scope, vector, size))
	}

	if lexErr != nil && vecErr != nil {
		return nil, combineErrors(lexErr, vecErr)
	}
	if lexErr != nil {
		slog.Warn("lexical search failed", "err", lexErr)
	}

	sources := search.MergeSources(lex.Sources, vec.Sources)
	fused := search.RRF(search.Rankings(lex.Ranking, vec.Ranking), topK)

	hits := make([]search.Hit, 0, len(fused))
	for _, f := range fused {
		src, ok := sources[f.ID]
		if !ok {
			continue
		}
		hits = append(hits, search.ToHit(src, f.Score))
	}
	return hits, nil
}

func combineErrors(lex, vec error) error {
	return &searchUnavailableError{lex: lex, vec: vec}
}

type searchUnavailableError struct {
	lex, vec error
}

func (e *searchUnavailableError) Error() string {
	return "both lexical and knn search failed"
}

func (e *searchUnavailableError) Unwrap() []error {
	return []error{e.lex, e.vec}
}
