package domain

import (
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// RawItem is one object of a source as the connector saw it, before
// normalization (roadmap §4, конверт документа). Это второй набор типов-значений
// швов: Connector возвращает его, стадия normalize превращает его в ParsedDocument.
//
// DocID здесь не вычисляется: он требует tenant, а коннектор тенанта не знает —
// его ставит конвейер, когда ставит событие lake.
type RawItem struct {
	// ExternalID is the object id inside the source ("C123", "t-42").
	ExternalID string `json:"external_id"`
	// Source is the connector name: slack, gmail, note, file, ...
	Source string `json:"source"`
	// Kind is the object type: message, thread, page, file, ticket (§4).
	Kind string `json:"kind"`
	// Title and Body are the source text; Body is normalized to Markdown by
	// the normalize stage, not by the connector.
	Title string `json:"title"`
	Body  string `json:"body"`
	// URL is the link to the original object.
	URL string `json:"url"`
	// ParentID is the container inside the source: thread, folder, channel.
	ParentID string `json:"parent_id"`
	// Links are outgoing links; основа для PageRank и графа (§5).
	Links []string `json:"links"`
	// SpaceID is the resource group container (§6): права висят на нём, а
	// точечный список исключений — в ACL документа.
	SpaceID string `json:"space_id"`
	// Owner drives notifications, Author is who wrote the object (§4).
	Owner  string `json:"owner"`
	Author string `json:"author"`
	// CreatedAt and UpdatedAt are the source dates; freshness counts from
	// UpdatedAt, ingest lag — from IngestedAt (§4).
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	IngestedAt time.Time `json:"ingested_at"`
	// RawKey is the content-addressed object storage key of the original
	// blob (§4, collect кладёт сырьё в S3). Пусто, если объект пришёл
	// телом и отдельного файла у него нет.
	RawKey string `json:"raw_key"`
	// Tags are manual and automatic labels (§4).
	Tags []string `json:"tags"`
	// Extra is everything source-specific (реакции Slack, статус тикета).
	// Новое поле конверта сначала едет сюда и лишь потом, у двух
	// потребителей, переезжает в первый класс (§8, правило процесса).
	Extra map[string]any `json:"extra"`
}

// EventQuery is the lake read input of EventStore: один тенант, одна стадия
// или все, один документ или все. Пустая стадия и пустой DocID означают «всё»;
// Limit и Cursor задают постраничное чтение.
//
// Порядок чтения — по OccurredAt, а при равенстве по EventID: он детерминирован,
// и пересборка индекса получает одни и те же события в одной и той же
// последовательности при любом параллелизме записи.
type EventQuery struct {
	TenantID uuid.UUID
	Stage    Stage
	DocID    string
	Since    time.Time
	Limit    int
	Cursor   string
}

// AccessScope is what one request may see, expressed as terms of the index
// query rather than as a materialized list of document ids (roadmap §6).
//
// Так права попадают внутрь запроса к Elasticsearch: смена прав на space
// применяется пересчётом списка space, без переиндексации документов, а
// post-filter (список разрешённых id) запрещён — разрешённый чанк уже
// оказался бы в памяти и в контексте LLM.
type AccessScope struct {
	TenantID string
	KBID     string
	// SpaceIDs are the containers the principal may read (§6).
	SpaceIDs []string
	// Principals are the expanded subject terms: user:, role:, group:.
	Principals []string
}

// AllowsNothing reports an empty scope. Такой запрос обязан вернуть пустое
// без обращения к индексу: список прав — это фильтр «разрешено», а не
// «не задано», и трактовка «не задано» означала бы утечку всех документов
// тенанта.
func (s AccessScope) AllowsNothing() bool {
	return len(s.SpaceIDs) == 0 && len(s.Principals) == 0
}

// AccessScopeFor expands a principal into index terms (§5 шаг 1 «разворачиваем
// пользователя в список принципалов», §6 «acl = [user:<id>]»). Контейнеры
// приходят отдельно: право на ресурсную группу выдаётся пользователю, а не
// хранится в его профиле.
func AccessScopeFor(p Principal, kbID string, spaceIDs []string) AccessScope {
	terms := make([]string, 0, 1+len(p.Roles)+len(p.Groups))
	terms = append(terms, "user:"+p.UserID)
	for _, role := range p.Roles {
		terms = append(terms, "role:"+role)
	}
	for _, group := range p.Groups {
		terms = append(terms, "group:"+group)
	}
	// Порядок термов не влияет на запрос, но повторяемость нужна тестам и
	// логам, поэтому дубликаты убираются, а список сортируется.
	slices.Sort(terms)
	terms = slices.Compact(terms)
	return AccessScope{
		TenantID:   p.TenantID.String(),
		KBID:       kbID,
		SpaceIDs:   slices.Clone(spaceIDs),
		Principals: terms,
	}
}

// EmbeddingProfile is the identity of the vector model: roadmap §4, «версия
// модели в документе». Документ должен помнить, какая модель посчитала его
// векторы, иначе переиндексация не знает, что пересчитывать.
//
// Это IndexingConfig без параметров чанкинга: ровно те шесть полей, из которых
// считается profile_id. Смена размерности, модели или расстояния меняет
// profile_id, а значит и имя индекса, поэтому профиль и индекс не могут
// разойтись.
type EmbeddingProfile struct {
	ProviderID       string `json:"provider_id"`
	Model            string `json:"model"`
	Revision         string `json:"revision"`
	Dimension        int    `json:"dimension"`
	Distance         string `json:"distance"`
	NormalizeVectors bool   `json:"normalize_vectors"`
}

// NewEmbeddingProfileFrom takes the model identity out of an indexing config.
func NewEmbeddingProfileFrom(cfg IndexingConfig) EmbeddingProfile {
	return EmbeddingProfile{
		ProviderID:       cfg.EmbeddingProviderID,
		Model:            cfg.EmbeddingModel,
		Revision:         cfg.EmbeddingRevision,
		Dimension:        cfg.EmbeddingDimension,
		Distance:         cfg.Distance,
		NormalizeVectors: cfg.NormalizeVectors,
	}
}

// ProfileID returns the id of the index profile these six fields describe. Он
// обязан совпадать с IndexingConfig.ProfileID() для тех же полей: документ
// хранит profile_id, и расхождение означало бы поиск по чужому индексу.
func (p EmbeddingProfile) ProfileID() string {
	return IndexingConfig{
		EmbeddingProviderID: p.ProviderID,
		EmbeddingModel:      p.Model,
		EmbeddingRevision:   p.Revision,
		EmbeddingDimension:  p.Dimension,
		Distance:            p.Distance,
		NormalizeVectors:    p.NormalizeVectors,
	}.ProfileID()
}

// Valid reports whether the profile names a model that can be recorded.
func (p EmbeddingProfile) Valid() bool {
	if p.Dimension <= 0 {
		return false
	}
	for _, required := range []string{p.ProviderID, p.Model, p.Revision, p.Distance} {
		if strings.TrimSpace(required) == "" {
			return false
		}
	}
	return true
}
