package indexing

import (
	"context"

	"github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ingest/parser"
	"github.com/kbsearch/kb/internal/ports"
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
	// Lake — тот же экземпляр, что отдан стадии delete и пересборке индекса
	// (composition root, cmd/worker/main.go). Второй адаптер lake выглядел бы
	// независимым, а на деле делил бы с первым один пул и один реестр
	// идемпотентности raw_event_ids, и ни один из них об этом не знал бы.
	// Нужен только стадии index, поэтому проверяется на границе стадии — по
	// той же причине, что и ParsedKey.
	Lake ports.EventBus
	// Clock задаёт occurred_at снимка (§4). Часы подставляются по умолчанию в
	// NewIndexer — без них стадия упала бы на первом же задании, ровно как
	// Delete делает для своего надгробия.
	Clock ports.Clock
}

// Indexer runs the index stage from ParsedDocument to active version.
// It knows nothing of queue leases or retries; those live outside.
type Indexer struct {
	deps Deps
}

// NewIndexer assembles the index stage.
func NewIndexer(deps Deps) *Indexer {
	if deps.Clock == nil {
		deps.Clock = ports.SystemClock{}
	}
	return &Indexer{deps: deps}
}

// Index processes one index-stage job. Chunks hit the index before the
// version flip, so a version never becomes searchable before its chunks.
//
// ПОРЯДОК ШАГОВ И ОДИН ОСОБЫЙ ШАГ МЕЖДУ НИМИ:
//
//  1. нарезка и вектора;
//  2. запись чанков (снести версию, потом записать);
//  3. СНИМОК ДОКУМЕНТА В LAKE — утверждение «эта версия в индексе» (§1,
//     принцип 1). Обоснование места шага — в publishSnapshot
//     (index_event.go): после чанков, потому что до них утверждение ещё не
//     истинно, и до закрытия задания, потому что после него отказ lake уже
//     некуда записать;
//  4. переключение активной версии, закрытие задания и уборка прежней версии
//     одной транзакцией.
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

	// Права разрешаются ОДИН раз на документ и уходят в чанки и в снимок lake
	// одними и теми же значениями. Второй запрос к оракулу был бы не лишней
	// нагрузкой, а ОКНОМ: между ним и первым администратор мог бы закрыть
	// документ лично, и индекс получил бы одни права, а источник правды —
	// другие. Пересборка по такому lake восстановила бы документ с правами,
	// которых у него уже нет (§6 — худший из возможных исходов).
	rights, err := i.resolveRights(ctx, parsed)
	if err != nil {
		return err
	}

	chunks, err := i.buildChunks(ctx, parsed, cfg, rights)
	if err != nil {
		return err
	}
	if err := i.writeChunks(ctx, tenantID, documentID, version, cfg, chunks); err != nil {
		return err
	}

	if err := i.publishSnapshot(ctx, lease, parsed, rights, len(chunks)); err != nil {
		return err
	}

	return i.activate(ctx, lease, domain.NewIndexingResult(
		documentID, version, cfg.ProfileID(), len(chunks),
	))
}

// buildChunks нарезает документ и считает вектора.
//
// Права приходят аргументом, а не разрешаются здесь: см. Index — их должно быть
// ровно одно разрешение на документ, и это разрешение обязано попасть и в чанки,
// и в снимок lake одними и теми же значениями.
func (i *Indexer) buildChunks(
	ctx context.Context, parsed domain.ParsedDocument, cfg domain.IndexingConfig,
	rights domain.DocumentRights,
) ([]domain.Chunk, error) {
	// Порядок важен: чанки собираются уже с правами, поэтому неверная форма
	// прав остановит стадию до того, как потрачены вектора.
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
			// Права едут вместе с чанком в индекс (§4, стадия index: «Upsert в
			// Elasticsearch вместе с ACL»). Пустые права здесь безопасны и означают
			// ровно одно: документ не виден никому, потому что terms по пустому полю
			// не совпадает ни с чем. Чинить это в адаптере нельзя — адаптер получает
			// чанк, а не документ, и восстановить права документа уже не из чего.
			Rights:      rights,
			ChunkNumber: d.Number,
			Title:       parsed.Title,
			Text:        d.Text,
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

// resolveRights собирает права документа из ДВУХ источников, и решает, кто из них
// главный.
//
// Конверт (RightsFromMetadata) приезжает от коннектора и в лучшем случае
// описывает контейнер документа. Серверное переопределение (documents.acl_override,
// порт DocumentRights) ставит администратор, и раньше оно до индекса не доезжало
// вообще: документ, лично закрытый на anna, индексировался как видимый ВСЕМ в
// своём контейнере. Это был тихий обход администраторского ограничения, и
// закрывать его обязан оракул, а не объявленное клиентом.
//
// Правило одно и строгое:
//
//   - серверного мнения нет (ports.InheritRights) — действует объявленный
//     контейнер, путь конверта сохранён;
//   - серверное мнение есть — оно авторитетно ЦЕЛИКОМ: ни контейнера, ни
//     объявленных исключений. Объявленное может только уступить, но не
//     расширить.
//
// Почему при переопределении срезается контейнер, а не добавляется acl: фильтр
// индекса — ИЛИ (terms(space_id) либо terms(acl), minimum_should_match 1), и
// оставленный контейнер вернул бы документ всему контейнеру вопреки
// ограничению. По той же причине срезаются и объявленные исключения: иначе
// клиент дописал бы к anna ещё и mallory.
//
// Отказ оракула останавливает стадию: версия не активируется, документ не
// уезжает в индекс и снимок не пишется. Это fail-closed — цена ошибки здесь не
// утечка, а непроиндексированный документ, который оператор видит по коду ошибки.
func (i *Indexer) resolveRights(
	ctx context.Context, parsed domain.ParsedDocument,
) (domain.DocumentRights, error) {
	declared, err := documents.RightsFromMetadata(parsed.Metadata)
	if err != nil {
		return domain.DocumentRights{}, err
	}

	server, err := i.deps.Documents.DocumentRights(
		ctx, i.deps.Queryer, parsed.TenantID.String(), parsed.DocumentID.String(),
	)
	if err != nil {
		return domain.DocumentRights{}, err
	}
	if server.SpaceID == ports.InheritRights {
		// Серверного ограничения нет: конверт остаётся единственным источником.
		return declared, nil
	}

	// SpaceID здесь не переносится НИКОГДА, даже если оракул его вернул: поле
	// контейнера в ответе оракула — это маркер наследования или ошибка вызывающего,
	// и оба случая означают «контейнера у документа нет».
	return domain.DocumentRights{ACL: server.ExceptionTerms()}, nil
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

// activate flips the active version, finishes the job and enqueues previous
// version cleanup in one transaction.
//
// ПОЧЕМУ ОНО НЕ ПОД НАЗВАНИЕМ publish. «Publish» в этой стадии значит «положить
// факт в lake» (publishSnapshot) и «сделать версию видимой» (это). Смешение двух
// разных глагоров в одном имени стоило бы того, чтобы читатель шагами стадии
// ошибся в том, какой из них записан в источник правды.
func (i *Indexer) activate(ctx context.Context, lease domain.JobLease, result domain.IndexingResult) error {
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
