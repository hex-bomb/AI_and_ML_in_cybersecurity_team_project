package api

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"

	"vektor/retrieval/internal/search"
)

// HybridSearch — ядро: гибридный поиск с fallback на лексику и слиянием RRF.
//
// Состояние деградации нужно только для того, чтобы не писать warning
// на каждый запрос: при отказе embedding это тысячи строк лога в секунду.
type HybridSearch struct {
	es       *search.ES
	embedder *search.Embedder
	down     atomic.Bool
}

func NewHybridSearch(es *search.ES, embedder *search.Embedder) *HybridSearch {
	return &HybridSearch{es: es, embedder: embedder}
}

// Search возвращает слитые хиты. Ошибка — только когда недоступны обе системы
// поиска; отказ одной — деградация, а не отказ сервиса.
func (s *HybridSearch) Search(ctx context.Context, scope search.Scope, query string, topK int) ([]search.Hit, error) {
	size := search.Size(topK)

	// Лексический поиск не зависит от вектора: запускаем его сразу, чтобы
	// не платить два сетевых RTT последовательно (embed -> ES, ES -> ES).
	type lexResult struct {
		result search.SearchResult
		err    error
	}
	// Буфер на 1: горутина не залипнет, даже если читатель уйдёт раньше.
	lexCh := make(chan lexResult, 1)
	go func() {
		result, err := s.es.Search(ctx, search.LexicalBody(scope, query, size))
		lexCh <- lexResult{result, err}
	}()

	// Отказ embedding не фатален: запрос продолжит лексическая ветка.
	vector, embedErr := s.embedder.Embed(ctx, query)
	s.noteEmbedding(embedErr)

	var vec search.SearchResult
	// Отказ embedding — тоже недоступность векторной ветки: иначе при мёртвом
	// ES и мёртвом embedding сервис отвечал бы пустым успехом.
	vecErr := embedErr
	if embedErr == nil {
		if len(vector) == 0 {
			vecErr = errors.New("embedding returned empty vector")
		} else {
			vec, vecErr = s.es.Search(ctx, search.KnnBody(scope, vector, size))
		}
	}

	lex := <-lexCh
	if lex.err != nil && vecErr != nil {
		return nil, combineErrors(lex.err, vecErr)
	}
	if lex.err != nil {
		slog.Debug("lexical search failed", "err", lex.err)
	}
	if vecErr != nil {
		slog.Debug("vector search skipped", "err", vecErr)
	}

	sources := search.MergeSources(lex.result.Sources, vec.Sources)
	fused := search.RRF(search.Rankings(lex.result.Ranking, vec.Ranking), topK)

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

// noteEmbedding логирует переход состояния, а не каждый запрос.
func (s *HybridSearch) noteEmbedding(err error) {
	if err == nil {
		if s.down.Swap(false) {
			slog.Info("embedding recovered")
		}
		return
	}
	if !s.down.Swap(true) {
		slog.Warn("embedding unavailable, lexical only", "err", err)
	}
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

// Unwrap отдаёт обе причины: errors.Is/As должны находить любую из веток.
func (e *searchUnavailableError) Unwrap() []error {
	return []error{e.lex, e.vec}
}
