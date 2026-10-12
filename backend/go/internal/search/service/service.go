package search

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/kbsearch/kb/internal/docs/settings"
	domain "github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
)

const (
	// StatusOK marks a non-empty result.
	StatusOK = "ok"
	// StatusNoResults marks an empty result.
	StatusNoResults = "no_results"
)

// DefaultAPIPrefix prefixes download links when the service is built without
// an explicit APIPrefix; it must match GET /documents/{id}/source.
const DefaultAPIPrefix = "/api/v1"

// Request is a search request. TopK <= 0 means "take search.top_k from settings".
type Request struct {
	KBID  uuid.UUID
	Query string
	TopK  int
}

// Response is a search response. Hits is never nil for JSON [] encoding.
type Response struct {
	Status string
	Hits   []domain.SearchCandidate
}

// retrievalSettings is the settings subset search needs; *settings.Service
// implements it, so New keeps taking the concrete service.
type retrievalSettings interface {
	ResolveRetrieval(ctx context.Context, q ports.Queryer, tenantID, kbID string) (settings.Retrieval, error)
}

// AccessScopeResolver — оракул прав §6 в том виде, в котором его видит поиск.
//
// Интерфейс объявлен ПОТРЕБИТЕЛЕМ, а не в internal/ports: правило §8 говорит
// «интерфейсы объявляет потребитель», а реализация лежит в store/pg и уехала бы
// отсюда вместе с внутренним SQL. Поиск — единственный сценарий, которому нужен
// именно такой вызов, и объявление рядом с ним же честно говорит, что правая
// сторона обязана ответить отказом на сбой, а не пустой областью.
//
// Права приходят ОДИН раз на запрос (§5 шаг 1, §6 «пересчёт списка доступных
// space у пользователя»): это контейнеры и принципалы, а не список документов.
// Права не перебираются по одному документу и не запрашиваются на каждый найденный
// чанк.
type AccessScopeResolver interface {
	ResolveScope(
		ctx context.Context, q ports.Queryer, p domain.Principal, kbID string,
	) (domain.AccessScope, error)
}

// ── УСТАРЕВШИЙ ПУТЬ, КОТОРЫЙ ЗДЕСЬ БОЛЬШЕ НЕ СОБИРАЕТСЯ ──────────────────────
//
// ports.SearchIndex с domain.SearchFilter.AllowedDocumentIDs помечен в портах
// как legacy, и после переезда на §6 его нельзя подключать снова. Причины §6
// формулирует прямо:
//
//   - «Права проверяются в самом запросе к индексу», а post-filter по списку
//     document_id запрещён: запрещённый чанк успевает оказаться в памяти
//     процесса и в контексте LLM ДО того, как сработала проверка;
//   - список доступных документов упирается в index.max_terms_count и растёт
//     вместе с тенантом, поэтому запрос перестаёт исполняться;
//   - отзыв права ждал бы переиндексации всех чанков контейнера, а §6 требует
//     применять смену прав сразу.
//
// Замена — ports.SecuredSearchIndex (KeywordSearchScoped / VectorSearchScoped):
// term по tenant_id плюс terms по space_id и acl внутри тела запроса. Правила
// сбора области живут в Search, этот список — напоминание для тех, кто
// захочет «просто вернуть старый фильтр».

// Service is hybrid BM25 + vector search with RRF merging.
type Service struct {
	queryer  ports.Queryer
	kbs      ports.KBRepository
	docs     ports.DocumentRepository
	index    ports.SecuredSearchIndex
	acl      AccessScopeResolver
	embedder ports.Embedder
	settings retrievalSettings
	// APIPrefix overrides DefaultAPIPrefix in download links; empty means default.
	APIPrefix string
}

// New assembles the search flow.
//
// acl обязателен: без него сценарий не знает, что пользователю можно, и
// единственным способом продолжить был бы поиск без прав вообще. Поэтому
// неподключённый оракул — отказ (см. resolveScope), а не «искать для всех».
func New(
	queryer ports.Queryer,
	kbs ports.KBRepository,
	docs ports.DocumentRepository,
	index ports.SecuredSearchIndex,
	acl AccessScopeResolver,
	embedder ports.Embedder,
	set *settings.Service,
) *Service {
	return &Service{
		queryer:  queryer,
		kbs:      kbs,
		docs:     docs,
		index:    index,
		acl:      acl,
		embedder: embedder,
		settings: set,
	}
}

// Search runs the full retrieval flow. Any part failing fails the whole
// search: a silent partial result would look complete.
func (s *Service) Search(
	ctx context.Context, p domain.Principal, in Request,
) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	if strings.TrimSpace(in.Query) == "" {
		return Response{}, domain.InvalidMetadata("query не может быть пустым")
	}

	kbID := in.KBID.String()
	tenantID := p.TenantID.String()

	// KB access first: a foreign KB must read as access-denied, not "unconfigured".
	kb, err := s.kbs.Require(ctx, s.queryer, p, kbID)
	if err != nil {
		return Response{}, err
	}
	retrieval, err := s.settings.ResolveRetrieval(ctx, s.queryer, tenantID, kbID)
	if err != nil {
		return Response{}, err
	}
	// Профиль по-прежнему сверяется с базой: расхождение — это 409 и требование
	// переиндексации, а не пустая выдача. Саму строку профиль теперь считает
	// индекс из IndexingConfig (scopedFilterClauses), поэтому значение нужно
	// только ради проверки.
	if _, err := profileFor(kb, retrieval.IndexingConfig); err != nil {
		return Response{}, err
	}
	topK := effectiveTopK(in.TopK, retrieval.TopK)

	// Права — ОДИН раз на запрос, до сети (§6). Область собирает оракул прав по
	// Postgres; в Elasticsearch она уезжает термами запроса.
	scope, err := s.resolveScope(ctx, p, kbID)
	if err != nil {
		return Response{}, err
	}
	// Пустая область — ноль результатов без обращения к индексу: спрашивать
	// кластер о документах, которых пользователь не увидит, незачем. AllowsNothing
	// означает «нечего не видно», а не «фильтр не задан» (см. envelope.AccessScope).
	if scope.AllowsNothing() {
		return noResults(), nil
	}

	lexical, vector, err := s.branches(
		ctx, in.Query, retrieval.IndexingConfig, scope, CandidateLimit(topK))
	if err != nil {
		return Response{}, err
	}

	merged := RRFMerge(lexical, vector)
	if len(merged) == 0 {
		return noResults(), nil
	}

	// Защита в глубину (§6): «перед тем как отдать top-N в UI или в контекст LLM
	// через MCP, повторно проверяем can_read(user, doc_ids) по Postgres». Индекс
	// отстаёт по правам (пересчёт области занимает минуты), и без этой проверки
	// выдача отдала бы документ контейнера, права на который уже отозвали. Здесь
	// же отсекаются удалённые и неактивные версии.
	//
	// ДОЛГ ЭТОЙ ПРОВЕРКИ (осознанный, не унаследованный): GetAllowedActiveVersions
	// отвечает «прочитай все документы тенанта, доступные principal», поэтому
	// сверка шире, чем вернувшиеся 20–50 кандидатов, хотя условие доступа
	// применяется сервером и в Elasticsearch эти id больше не уезжают. Сужение до
	// списка кандидатов требует нового метода порта вида
	// ReadableActiveVersions(ctx, q, p, kbID, docIDs) — одна база для одной
	// границы, и он не должен отвечать «какие документы мне видны» (§6 запрещает
	// post-filter по id именно в оракуле прав). Само собирание материализованного
	// списка В ФИЛЬТРЕ ЗАПРОСА, которое было раньше, здесь больше не происходит:
	// в Elasticsearch уезжает область.
	active, err := s.docs.GetAllowedActiveVersions(ctx, s.queryer, p, kbID)
	if err != nil {
		return Response{}, err
	}
	activeVersions := activeVersions(active)
	hits := visibleHits(merged, activeVersions, topK)
	if len(hits) == 0 {
		return noResults(), nil
	}
	sources, err := s.docs.VersionSources(ctx, s.queryer, p, kbID, activeVersions)
	if err != nil {
		return Response{}, err
	}
	return Response{Status: StatusOK, Hits: s.withLinks(hits, sources)}, nil
}

// resolveScope спрашивает у оракула права пользователя и закрепляет границы
// запроса (§6) за аутентифицированным субъектом.
//
// Закрепление (tenant и база из токена и запроса) — не косметика: term по
// tenant_id в теле запроса стоит первым и не зависит от области, но полагаться
// на то, что оракул не ошибся, — значило бы оставлять границу организации на
// милость чужого кода. Субъект известен точно, поэтому и граница должна прийти из
// него.
//
// Отказ — в обоих направлениях, и оба закрыты:
//
//   - оракула нет (проводка не задана) — отказ, потому что единственная
//     альтернатива была бы поиском без прав, то есть полным обходом;
//   - оракул ошибся — отказ, потому что «не знаю прав» это не «прав нет».
//     Подставить пустую область здесь можно было бы тихо, и тогда сбой базы
//     выглядел бы как «вам ничего не найдено» вместо 503. Причина уходит в
//     Cause, наружу — текст без внутренних подробностей.
func (s *Service) resolveScope(
	ctx context.Context, p domain.Principal, kbID string,
) (domain.AccessScope, error) {
	if s.acl == nil {
		return domain.AccessScope{}, domain.Internal(
			"Права доступа не настроены: поиск без оракула прав не выполняется")
	}
	scope, err := s.acl.ResolveScope(ctx, s.queryer, p, kbID)
	if err != nil {
		return domain.AccessScope{}, domain.ServiceUnavailable(
			"Права доступа недоступны").WithCause(err)
	}
	scope.TenantID = p.TenantID.String()
	scope.KBID = kbID
	return scope, nil
}

// branches runs both search branches in parallel. The query vector is encoded
// first so a broken embedder never sends a request to the index.
//
// Порядок параметров у двух методов разный — (scope, cfg) у текстовой ветви и
// (cfg, scope) у векторной; это подпись порта, и она перепутана быть не может:
// несовпадение типов не компилируется.
func (s *Service) branches(
	ctx context.Context,
	query string,
	cfg domain.IndexingConfig,
	scope domain.AccessScope,
	limit int,
) ([]domain.SearchCandidate, []domain.SearchCandidate, error) {
	vector, err := s.queryVector(ctx, query, cfg)
	if err != nil {
		return nil, nil, err
	}

	group, gctx := errgroup.WithContext(ctx)
	var lexical, semantic []domain.SearchCandidate
	group.Go(func() error {
		found, err := s.index.KeywordSearchScoped(gctx, query, scope, cfg, limit)
		lexical = found
		return err
	})
	group.Go(func() error {
		found, err := s.index.VectorSearchScoped(gctx, vector, cfg, scope, limit)
		semantic = found
		return err
	})
	if err := group.Wait(); err != nil {
		return nil, nil, err
	}
	return lexical, semantic, nil
}

// queryVector encodes the question with the indexing-time profile.
func (s *Service) queryVector(
	ctx context.Context, query string, cfg domain.IndexingConfig,
) ([]float32, error) {
	vector, err := s.embedder.EmbedQuery(ctx, query, cfg)
	if err != nil {
		return nil, err
	}
	// An empty vector would score the whole corpus equally; surface it instead.
	if len(vector) == 0 {
		return nil, domain.EmbeddingResponseInvalid("Эмбеддер вернул пустой вектор для вопроса")
	}
	return vector, nil
}

// profileFor pins search to the profile the index was built with; a drifted
// profile is a rebuild requirement, not a silent empty result.
func profileFor(kb domain.KB, cfg domain.IndexingConfig) (string, error) {
	if kb.EmbeddingProfileID == "" {
		return cfg.ProfileID(), nil
	}
	if kb.EmbeddingProfileID != cfg.ProfileID() {
		return "", domain.EmbeddingProfileNeedsRebuild(
			"Настройки эмбеддинга изменились после индексации",
		).WithDetails(map[string]any{
			"indexed":    kb.EmbeddingProfileID,
			"configured": cfg.ProfileID(),
		})
	}
	return kb.EmbeddingProfileID, nil
}

func effectiveTopK(requested, fromSettings int) int {
	topK := requested
	if topK <= 0 {
		topK = fromSettings
	}
	if topK < 1 {
		return 1
	}
	if topK > settings.TopKMax {
		return settings.TopKMax
	}
	return topK
}

// visibleHits is the §6 defense in depth before results leave the process.
//
// Кандидат проходит, только если Postgres СЕЙЧАС говорит, что документ читаем
// (тот же фильтр доступа, что у GetAllowedActiveVersions) и версия из индекса —
// активная. Это единственное место, где выдача сверяется с базой после ответа
// индекса, и оно же закрывает окно устаревшего индекса: права отозвали минуту
// назад, область ещё не пересчитана, а документ уже не вернётся.
//
// TopK применяется здесь, а не до фильтра, — иначе top_k набирался бы из
// документов, которые всё равно не покажут. RRF-score ставится поверх score
// ветви (BM25 и косинус несравнимы).
func visibleHits(merged []ScoredCandidate, active map[string]string, topK int) []domain.SearchCandidate {
	out := make([]domain.SearchCandidate, 0, topK)
	for _, item := range merged {
		versionID, ok := active[item.Candidate.DocumentID]
		if !ok || !domain.IsActiveVersion(item.Candidate.VersionID, &versionID) {
			continue
		}
		hit := item.Candidate
		hit.Score = item.Score
		out = append(out, hit)
		if len(out) >= topK {
			break
		}
	}
	return out
}

func (s *Service) withLinks(hits []domain.SearchCandidate, sources map[string]string) []domain.SearchCandidate {
	out := make([]domain.SearchCandidate, len(hits))
	for i, hit := range hits {
		if url, ok := sources[hit.VersionID]; ok {
			link := url
			hit.SourceURL = &link
		}
		hit.DownloadURL = s.downloadURL(hit)
		out[i] = hit
	}
	return out
}

func (s *Service) downloadURL(hit domain.SearchCandidate) string {
	return s.prefix() + "/documents/" + hit.DocumentID +
		"/source?version_id=" + hit.VersionID
}

// prefix is the injectable link prefix; empty keeps the default contract.
func (s *Service) prefix() string {
	if s.APIPrefix != "" {
		return s.APIPrefix
	}
	return DefaultAPIPrefix
}

func activeVersions(active []domain.ActiveVersion) map[string]string {
	out := make(map[string]string, len(active))
	for _, item := range active {
		out[item.DocumentID] = item.ActiveVersionID
	}
	return out
}

func noResults() Response {
	return Response{Status: StatusNoResults, Hits: []domain.SearchCandidate{}}
}
