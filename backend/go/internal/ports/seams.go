package ports

import (
	"context"

	domain "github.com/kbsearch/kb/internal/envelope"
)

// This file is the four seams of roadmap §8: «для каждого один Go-интерфейс и
// версионируемая схема событий». Интерфейсы объявляет потребитель, поэтому всё
// объявление живёт здесь, а реализации — в internal/store.
//
// Общий запрет швов: подпись интерфейса не называет транспорт и хранилище.
// Kafka, NATS, ClickHouse, Postgres и S3 меняются за этими типами, и тип,
// названный kafkaMessage, в сигнатуре означал бы, что замена потребует правки
// сценариев — ровно то, ради чего швы и выделены.

// Connector is the single door for every source (roadmap §4): новый источник
// добавляется коннектором и маппингом, ядро при этом не меняется.
//
// Подпись приведена из §4 дословно, вместе с комментариями.
type Connector interface {
	Name() string
	// Pull возвращает порцию и новый курсор; пустой курсор = полный обход.
	Pull(ctx context.Context, cursor string) (items []domain.RawItem, next string, err error)
	// Permissions — кто может читать этот объект в источнике (внешние группы).
	Permissions(ctx context.Context, item domain.RawItem) ([]domain.Principal, error)
}

// EventBus is the write side of the event flow (§8, seam 4): то, во что
// складываются результаты стадий и из чего берут потребители.
//
// Kafka, NATS, простой вызов реализации или шина на тестах — всё это одна и
// та же подпись. Ни тип, ни поле сигнатуры не называют Kafka или ClickHouse,
// иначе «можно заменить, не трогая остальной код» перестаёт быть возможным;
// это проверяется в seams_test.go.
type EventBus interface {
	Publish(ctx context.Context, event domain.Event) error
	PublishBatch(ctx context.Context, events []domain.Event) error
	Close() error
}

// EventStore is the read side of the lake: журнал только на добавление, который
// читается по порядку (§8, seam 4). От него зависят пересборка индекса, графа
// и PageRank (§1, принцип 1), поэтому он намеренно отделён от EventBus:
// писатель и читатель lake живут разными процессами и могут быть разными
// системами (фаза 2 подключает ClickHouse, MVP — таблицу Postgres).
//
// Как и EventBus, подпись ничего не знает о хранилище. Пишет lake реализация
// EventBus, читает — этот интерфейс.
type EventStore interface {
	Read(ctx context.Context, q domain.EventQuery) ([]domain.Event, error)
	Ping(ctx context.Context) error
	Close() error
}

// Ranker reorders the candidates fused by RRF (roadmap §5, п. 4: cross-encoder
// rerank топ-50 за флагом). Без него RRF даёт порядок сам.
type Ranker interface {
	Name() string
	Rank(ctx context.Context, query string, candidates []domain.SearchCandidate, limit int) ([]domain.SearchCandidate, error)
}

// QueryExpander rewrites the question before retrieval. В MVP реализация пустая
// (§5, §8): вернуть исходный запрос и всё. Фаза 3 добавляет сюда Hopfield, и
// оставляет его, только если он поднимает nDCG@10 на своём наборе вопросов.
type QueryExpander interface {
	Name() string
	// Expand returns the variants to retrieve with; the no-op returns [query].
	Expand(ctx context.Context, query string) ([]string, error)
}

// ProfiledEmbedder is the identity part of the Embedder seam (§8, seam 2):
// «версия модели в документе», §4.
//
// Это отдельный узкий интерфейс, а не новый метод в Embedder: у документов
// уже есть EmbeddingProfileID, менять его без миграции нельзя, а спрашивать у
// адаптера, какая модель стоит за векторами, должны уметь и существующие
// реализации, не добавляя им метод задним числом.
type ProfiledEmbedder interface {
	Profile() domain.EmbeddingProfile
}

// SecuredSearchIndex is the access part of the SearchIndex seam (§6): права
// едут внутрь запроса к индексу — term по tenant_id плюс terms по space_id и
// acl, — а не приезжают готовым списком document_id.
//
// Старый путь с AllowedDocumentIDs остаётся в SearchIndex (см. ports.go) и
// помечен там как legacy: им пользуется search/service, и его переезд на
// этот интерфейс — отдельная задача. Смена прав на space после переезда
// применяется пересчётом AccessScope.SpaceIDs, без переиндексации документов.
type SecuredSearchIndex interface {
	KeywordSearchScoped(
		ctx context.Context, query string, scope domain.AccessScope, cfg domain.IndexingConfig, limit int,
	) ([]domain.SearchCandidate, error)
	VectorSearchScoped(
		ctx context.Context, vec []float32, cfg domain.IndexingConfig, scope domain.AccessScope, limit int,
	) ([]domain.SearchCandidate, error)
}
