package indexing

import (
	"context"

	"github.com/kbsearch/backend-go/internal/application/ports"
	"github.com/kbsearch/backend-go/internal/domain"
)

const schemaVersion = "1.0"

// IndexingSettings is the one settings lookup the indexing flow needs.
type IndexingSettings interface {
	IndexingConfig(ctx context.Context, q ports.Queryer, tenantID, kbID string) (domain.IndexingConfig, error)
}

// Deps wires the indexing stage ports.
type Deps struct {
	Documents  ports.DocumentRepository
	Jobs       ports.JobRepository
	Settings   IndexingSettings
	Embedder   ports.Embedder
	Index      ports.SearchIndex
	Storage    ports.FileStorage
	Queryer    ports.Queryer
	Transactor ports.Transactor
	// ParsedKey строит ключ parsed.json. Нужен только стадии convert, поэтому
	// не проверяется на этапе сборки NewIndexer: стадия индексации ключ не
	// строит, и требовать его там означало бы запретить частичную сборку.
	ParsedKey keyBuilder
}

// Indexer runs the index stage from ParsedDocument to active version.
// It knows nothing of queue leases or retries; those live outside.
type Indexer struct {
	deps Deps
}

// NewIndexer assembles the index stage.
func NewIndexer(deps Deps) *Indexer { return &Indexer{deps: deps} }

// Index processes one index-stage job. Chunks hit the index before the
// version flip, so a version never becomes searchable before its chunks.
func (i *Indexer) Index(ctx context.Context, lease domain.JobLease) error {
	versionID, ok := lease.VersionIDValue()
	if !ok {
		return domain.Conflict("Задание стадии index не ссылается на версию документа")
	}

	tenantID := lease.TenantID.String()
	documentID := lease.DocumentID.String()
	version := versionID.String()

	parsed, err := i.deps.Documents.LoadParsed(ctx, i.deps.Queryer, i.deps.Storage, tenantID, version)
	if err != nil {
		return err
	}
	if parsed.TenantID != lease.TenantID || parsed.DocumentID != lease.DocumentID ||
		parsed.VersionID != versionID {
		return domain.Conflict("Разобранный документ не соответствует заданию")
	}

	cfg, err := i.deps.Settings.IndexingConfig(ctx, i.deps.Queryer, tenantID, parsed.KBID.String())
	if err != nil {
		return err
	}

	chunks, err := i.buildChunks(ctx, parsed, cfg)
	if err != nil {
		return err
	}
	if err := i.writeChunks(ctx, tenantID, documentID, version, cfg, chunks); err != nil {
		return err
	}

	return i.publish(ctx, lease, domain.NewIndexingResult(
		documentID, version, cfg.ProfileID(), len(chunks),
	))
}

func (i *Indexer) buildChunks(
	ctx context.Context, parsed domain.ParsedDocument, cfg domain.IndexingConfig,
) ([]domain.Chunk, error) {
	drafts, err := Chunk(parsed, cfg)
	if err != nil {
		return nil, err
	}

	texts := make([]string, len(drafts))
	for n, d := range drafts {
		texts[n] = d.Text
	}
	vectors, err := i.deps.Embedder.Embed(ctx, texts, cfg)
	if err != nil {
		return nil, err
	}
	// A shifted vector order would silently attach wrong embeddings to chunks.
	if len(vectors) != len(drafts) {
		return nil, domain.EmbeddingResponseInvalid("Число векторов не совпало с числом чанков").
			WithDetails(map[string]any{"chunks": len(drafts), "vectors": len(vectors)})
	}

	profileID := cfg.ProfileID()
	chunks := make([]domain.Chunk, len(drafts))
	for n, d := range drafts {
		chunks[n] = domain.Chunk{
			SchemaVersion: schemaVersion,
			ChunkID:       d.ChunkID,
			TenantID:      parsed.TenantID.String(),
			KBID:          parsed.KBID.String(),
			DocumentID:    parsed.DocumentID.String(),
			VersionID:     parsed.VersionID.String(),
			ChunkNumber:   d.Number,
			Title:         parsed.Title,
			Text:          d.Text,
			Locator: domain.Locator{
				BlockIDs:    d.BlockIDs,
				SectionPath: d.SectionPath,
				PageStart:   d.PageStart,
				PageEnd:     d.PageEnd,
			},
			EmbeddingProfileID: profileID,
			Embedding:          vectors[n],
		}
	}
	return chunks, nil
}

// writeChunks rewrites the version's chunks. Delete-before-write is required:
// retry with changed slicing settings must not leave stale chunks behind.
// The gap is invisible since the version is not active yet.
func (i *Indexer) writeChunks(
	ctx context.Context, tenantID, documentID, versionID string,
	cfg domain.IndexingConfig, chunks []domain.Chunk,
) error {
	if _, err := i.deps.Index.DeleteVersion(ctx, tenantID, documentID, versionID); err != nil {
		return err
	}
	written, err := i.deps.Index.UpsertChunks(ctx, cfg, chunks)
	if err != nil {
		return err
	}
	if written != len(chunks) {
		return domain.IndexUnavailable("В индекс записано меньше чанков, чем насчитано").
			WithDetails(map[string]any{"expected": len(chunks), "actual": written})
	}
	return nil
}

// publish flips the active version, finishes the job and enqueues previous
// version cleanup in one transaction.
func (i *Indexer) publish(ctx context.Context, lease domain.JobLease, result domain.IndexingResult) error {
	tenantID := lease.TenantID.String()
	documentID := lease.DocumentID.String()

	return i.deps.Transactor.WithTx(ctx, func(tx ports.Queryer) error {
		previous, err := i.deps.Documents.ActivateVersion(ctx, tx, domain.ActivateParams{
			TenantID:           tenantID,
			DocumentID:         documentID,
			VersionID:          result.VersionID,
			EmbeddingProfileID: result.EmbeddingProfileID,
			ChunkCount:         result.ChunkCount,
			LeaseToken:         lease.LeaseToken,
		})
		if err != nil {
			return err
		}
		if err := i.deps.Jobs.FinishIndex(ctx, tx, lease.JobID.String(), lease.LeaseToken, result); err != nil {
			return err
		}
		// Empty previous means no active version to clean up.
		if previous == "" {
			return nil
		}
		_, err = i.deps.Jobs.EnqueueCleanup(ctx, tx, tenantID, documentID, previous)
		return err
	})
}
