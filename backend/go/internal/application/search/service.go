package search

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/kbsearch/backend-go/internal/application/ports"
	"github.com/kbsearch/backend-go/internal/application/settings"
	"github.com/kbsearch/backend-go/internal/domain"
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

// Service is hybrid BM25 + vector search with RRF merging.
type Service struct {
	queryer  ports.Queryer
	kbs      ports.KBRepository
	docs     ports.DocumentRepository
	index    ports.SearchIndex
	embedder ports.Embedder
	settings retrievalSettings
	// APIPrefix overrides DefaultAPIPrefix in download links; empty means default.
	APIPrefix string
}

// New assembles the search flow.
func New(
	queryer ports.Queryer,
	kbs ports.KBRepository,
	docs ports.DocumentRepository,
	index ports.SearchIndex,
	embedder ports.Embedder,
	set *settings.Service,
) *Service {
	return &Service{
		queryer:  queryer,
		kbs:      kbs,
		docs:     docs,
		index:    index,
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
	profileID, err := profileFor(kb, retrieval.IndexingConfig)
	if err != nil {
		return Response{}, err
	}
	topK := effectiveTopK(in.TopK, retrieval.TopK)

	active, err := s.docs.GetAllowedActiveVersions(ctx, s.queryer, p, kbID)
	if err != nil {
		return Response{}, err
	}
	if len(active) == 0 {
		return noResults(), nil
	}

	filter := domain.SearchFilter{
		TenantID:           tenantID,
		KBID:               kbID,
		EmbeddingProfileID: profileID,
		AllowedDocumentIDs: documentIDs(active),
	}
	lexical, vector, err := s.branches(ctx, in.Query, retrieval.IndexingConfig, filter, CandidateLimit(topK))
	if err != nil {
		return Response{}, err
	}

	activeVersions := activeVersions(active)
	hits := visibleHits(RRFMerge(lexical, vector), activeVersions, topK)
	if len(hits) == 0 {
		return noResults(), nil
	}
	sources, err := s.docs.VersionSources(ctx, s.queryer, p, kbID, activeVersions)
	if err != nil {
		return Response{}, err
	}
	return Response{Status: StatusOK, Hits: s.withLinks(hits, sources)}, nil
}

// branches runs both search branches in parallel. The query vector is encoded
// first so a broken embedder never sends a request to the index.
func (s *Service) branches(
	ctx context.Context,
	query string,
	cfg domain.IndexingConfig,
	filter domain.SearchFilter,
	limit int,
) ([]domain.SearchCandidate, []domain.SearchCandidate, error) {
	vector, err := s.queryVector(ctx, query, cfg)
	if err != nil {
		return nil, nil, err
	}

	group, gctx := errgroup.WithContext(ctx)
	var lexical, semantic []domain.SearchCandidate
	group.Go(func() error {
		found, err := s.index.KeywordSearch(gctx, query, filter, limit)
		lexical = found
		return err
	})
	group.Go(func() error {
		found, err := s.index.VectorSearch(gctx, vector, cfg, filter, limit)
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

// visibleHits keeps active-version candidates and stamps the RRF score over
// the branch score (BM25 and cosine are not mutually comparable).
// The active check is shared with answers.visible via domain.IsActiveVersion.
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

func documentIDs(active []domain.ActiveVersion) []string {
	out := make([]string, 0, len(active))
	for _, item := range active {
		out = append(out, item.DocumentID)
	}
	return out
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
