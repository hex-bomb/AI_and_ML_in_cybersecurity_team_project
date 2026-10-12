package main

import (
	"context"

	"github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/jobs/indexing"
	"github.com/kbsearch/kb/internal/jobs/worker"
)

// stages склеивает стадии convert, index и delete в один worker.Stage.
//
// WHY объединятель, а не общий тип стадии: конвертер, индексатор и удалитель —
// разные сценарии с разными зависимостями (индексатору нужны эмбеддер и индекс
// поиска, конвертеру — нет, удалителю не нужны ни эмбеддер, ни настройки).
// Объединение их в один тип означало бы, что стадии convert пришлось бы
// принимать эмбеддер, который ей не нужен, а индексатор — умолчать о том, что не
// использует. Разделение остаётся, а цикл видит единый интерфейс.
type stages struct {
	converter *indexing.Converter
	indexer   *indexing.Indexer
	deleter   *indexing.Deleter
}

// newStages собирает обработчик стадий воркера.
func newStages(
	converter *indexing.Converter, indexer *indexing.Indexer, deleter *indexing.Deleter,
) worker.Stage {
	return stages{converter: converter, indexer: indexer, deleter: deleter}
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

// Delete выполняет стадию delete.
func (s stages) Delete(ctx context.Context, lease domain.JobLease) error {
	return s.deleter.Delete(ctx, lease)
}
