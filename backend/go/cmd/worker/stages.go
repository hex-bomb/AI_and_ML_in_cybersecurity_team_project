package main

import (
	"context"

	"github.com/kbsearch/backend-go/internal/application/indexing"
	"github.com/kbsearch/backend-go/internal/delivery/worker"
	"github.com/kbsearch/backend-go/internal/domain"
)

// stages склеивает стадии convert и index в один worker.Stage.
//
// WHY объединятель, а не общий тип стадии: конвертер и индексатор — разные
// сценарии с разными зависимостями (индексатору нужны эмбеддер и индекс
// поиска, конвертеру — нет). Объединение их в один тип означало бы, что
// стадии convert пришлось бы принимать эмбеддер, который ей не нужен, а
// индексатор — умолчать о том, что не использует. Разделение остаётся, а
// цикл видит единый интерфейс.
type stages struct {
	converter *indexing.Converter
	indexer   *indexing.Indexer
}

// newStages собирает обработчик стадий воркера.
func newStages(converter *indexing.Converter, indexer *indexing.Indexer) worker.Stage {
	return stages{converter: converter, indexer: indexer}
}

var _ worker.Stage = stages{}

// Convert выполняет стадию convert.
func (s stages) Convert(ctx context.Context, lease domain.JobLease) error {
	return s.converter.Convert(ctx, lease)
}

// Index выполняет стадию index.
func (s stages) Index(ctx context.Context, lease domain.JobLease) error {
	return s.indexer.Index(ctx, lease)
}
