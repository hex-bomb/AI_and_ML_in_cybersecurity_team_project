// Package answers builds quoted answers: gather material, ask the model,
// verify citations.
package answers

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/kbsearch/kb/internal/docs/settings"
	"github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
	"github.com/kbsearch/kb/internal/search/service"
)

const (
	// StatusOK marks an answer confirmed by citations.
	StatusOK = "ok"
	// StatusNoEvidence marks an answer with no supporting material.
	StatusNoEvidence = "no_evidence"
)

// NoEvidenceAnswer is the neutral reply when nothing confirms an answer.
const NoEvidenceAnswer = "В доступной базе знаний не найдено подтверждающих материалов."

const (
	// MaxToolRounds caps kb_search calls per answer.
	MaxToolRounds = 2
	// ToolResultLimit caps fragments per tool call.
	ToolResultLimit = 5
	// ToolRoundTokens is the token budget of a tool round.
	ToolRoundTokens = 256
)

// Request is an answer request.
type Request struct {
	KBID     uuid.UUID
	Question string
}

// Response is a quoted answer. Citations is never nil for JSON [] encoding.
type Response struct {
	Status    string
	Answer    string
	Citations []domain.Citation
}

// Searcher is the subset of search the answers flow needs.
type Searcher interface {
	Search(ctx context.Context, p domain.Principal, in search.Request) (search.Response, error)
}

// generationSettings is the settings subset answers needs; *settings.Service
// implements it, so New keeps taking the concrete service.
type generationSettings interface {
	ResolveGeneration(ctx context.Context, q ports.Queryer, tenantID, kbID string) (settings.Generation, error)
}

// Service answers with citations.
type Service struct {
	queryer  ports.Queryer
	searcher Searcher
	llm      ports.LLMProvider
	docs     ports.DocumentRepository
	settings generationSettings
}

// New assembles the answers flow.
func New(
	queryer ports.Queryer,
	searcher Searcher,
	llm ports.LLMProvider,
	docs ports.DocumentRepository,
	set *settings.Service,
) *Service {
	return &Service{
		queryer:  queryer,
		searcher: searcher,
		llm:      llm,
		docs:     docs,
		settings: set,
	}
}

// Answer answers a question from the knowledge base.
func (s *Service) Answer(
	ctx context.Context, p domain.Principal, in Request,
) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	if strings.TrimSpace(in.Question) == "" {
		return Response{}, domain.InvalidMetadata("question не может быть пустым")
	}

	generation, err := s.settings.ResolveGeneration(ctx, s.queryer, p.TenantID.String(), in.KBID.String())
	if err != nil {
		return Response{}, err
	}

	found, err := s.searcher.Search(ctx, p, search.Request{KBID: in.KBID, Query: in.Question})
	if err != nil {
		return Response{}, err
	}
	if len(found.Hits) == 0 {
		// No material: calling the model could only hallucinate.
		return noEvidence(), nil
	}

	known, err := buildContext(generation.MaxContextTokens, found.Hits)
	if err != nil {
		return Response{}, err
	}
	sources := newKnownSet(known)

	result, err := s.llm.Generate(ctx, domain.GenerateRequest{
		Question:         in.Question,
		Known:            known,
		MaxContextTokens: generation.MaxContextTokens,
		MaxOutputTokens:  generation.MaxOutputTokens,
		ProviderID:       generation.ProviderID,
		Model:            generation.Model,
		SearchTool:       s.searchTool(p, in.KBID, sources),
		MaxToolRounds:    MaxToolRounds,
		ToolResultLimit:  ToolResultLimit,
		ToolRoundTokens:  ToolRoundTokens,
	})
	if err != nil {
		return Response{}, err
	}

	cited := resolveCitations(result.CitationChunkIDs, sources)
	if len(cited) == 0 {
		return noEvidence(), nil
	}
	cited, err = s.visible(ctx, p, in.KBID.String(), cited)
	if err != nil {
		return Response{}, err
	}
	if len(cited) == 0 {
		return noEvidence(), nil
	}

	return Response{
		Status:    StatusOK,
		Answer:    result.Answer,
		Citations: buildCitations(cited),
	}, nil
}

// searchTool runs kb_search through the same search, so permissions are
// rechecked on tool results too. Matches extend the known citation set.
func (s *Service) searchTool(
	p domain.Principal, kbID uuid.UUID, known *knownSet,
) domain.SearchToolFunc {
	return func(ctx context.Context, query string) ([]domain.SearchCandidate, error) {
		found, err := s.searcher.Search(ctx, p, search.Request{KBID: kbID, Query: query})
		if err != nil {
			return nil, err
		}
		known.add(found.Hits...)
		return found.Hits, nil
	}
}

// visible rechecks cited chunks against the freshly read active versions:
// access may have been revoked between search and answer.
func (s *Service) visible(
	ctx context.Context, p domain.Principal, kbID string, cited []domain.SearchCandidate,
) ([]domain.SearchCandidate, error) {
	active, err := s.docs.GetAllowedActiveVersions(ctx, s.queryer, p, kbID)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]string, len(active))
	for _, item := range active {
		allowed[item.DocumentID] = item.ActiveVersionID
	}

	out := make([]domain.SearchCandidate, 0, len(cited))
	for _, source := range cited {
		versionID, ok := allowed[source.DocumentID]
		if !ok || !domain.IsActiveVersion(source.VersionID, &versionID) {
			continue
		}
		out = append(out, source)
	}
	return out, nil
}

func noEvidence() Response {
	return Response{
		Status:    StatusNoEvidence,
		Answer:    NoEvidenceAnswer,
		Citations: []domain.Citation{},
	}
}
