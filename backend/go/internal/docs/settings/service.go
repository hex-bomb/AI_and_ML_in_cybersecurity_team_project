package settings

import (
	"context"
	"slices"

	domain "github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
)

// Source is the level a value was resolved from; Unset means no row exists.
type Source string

const (
	SourceKB     Source = "kb"
	SourceTenant Source = "tenant"
	SourceUnset  Source = "unset"
)

// ErrNotOwned means the key is known but outside the requested set (a code bug).
type ErrNotOwned struct {
	Key Key
}

func (e *ErrNotOwned) Error() string {
	return "ключ " + e.Key.Name() + " не входит в запрошенный набор настроек"
}

// Values holds validated, normalized key values with their source each.
type Values struct {
	byKey map[string]item
}

type item struct {
	key    Key
	value  any
	source Source
}

func (r Values) valueOf(key Key) (any, error) {
	found, ok := r.byKey[key.Name()]
	if !ok {
		return nil, &ErrNotOwned{Key: key}
	}
	return found.value, nil
}

// valueAs reads a normalized value; a type mismatch is a code bug, not input.
func valueAs[T any](r Values, key Key) (T, error) {
	var zero T
	value, err := r.valueOf(key)
	if err != nil {
		return zero, err
	}
	typed, ok := value.(T)
	if !ok {
		return zero, &ErrNotOwned{Key: key}
	}
	return typed, nil
}

// String reads a string key.
func (r Values) String(key Key) (string, error) { return valueAs[string](r, key) }

// Int reads an integer key.
func (r Values) Int(key Key) (int, error) {
	number, err := valueAs[int64](r, key)
	return int(number), err
}

// Bool reads a boolean key.
func (r Values) Bool(key Key) (bool, error) { return valueAs[bool](r, key) }

// Strings reads a string-array key.
func (r Values) Strings(key Key) ([]string, error) { return valueAs[[]string](r, key) }

// Service reads and validates knowledge-base settings.
type Service struct {
	repo ports.SettingsRepository
}

// New creates the settings service.
func New(repo ports.SettingsRepository) *Service {
	return &Service{repo: repo}
}

// Resolve validates a key set for a base; KB level wins over tenant.
// Missing keys come back as one CONFIGURATION_MISSING listing all of them.
func (s *Service) Resolve(
	ctx context.Context, q ports.Queryer, tenantID, kbID string, keys []Key,
) (Values, error) {
	if err := ctx.Err(); err != nil {
		return Values{}, err
	}
	kbLevel, tenantLevel, err := s.repo.Levels(ctx, q, tenantID, kbID)
	if err != nil {
		return Values{}, err
	}

	out := Values{byKey: make(map[string]item, len(keys))}
	var missing []string
	for _, key := range keys {
		raw, source, found := lookup(kbLevel, tenantLevel, key)
		if !found {
			missing = append(missing, key.Name())
			continue
		}
		value, err := validate(key, raw)
		if err != nil {
			return Values{}, invalid(key, raw, err)
		}
		entry := item{key: key, value: value, source: source}
		out.byKey[key.Name()] = entry
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return Values{}, domain.ConfigurationMissing("", map[string]any{"keys": missing})
	}
	return out, nil
}

func lookup(kbLevel, tenantLevel map[string]any, key Key) (any, Source, bool) {
	if value, ok := kbLevel[key.Name()]; ok {
		return value, SourceKB, true
	}
	if value, ok := tenantLevel[key.Name()]; ok {
		return value, SourceTenant, true
	}
	return nil, SourceUnset, false
}

func invalid(key Key, raw any, err error) error {
	return domain.ConfigurationInvalid(
		"Неверное значение настройки "+key.Name(),
		map[string]any{
			"key":           key.Name(),
			"reason":        invalidReason(err),
			"expected_type": string(key.ValueType()),
			"value":         raw,
		},
	)
}

// IndexingConfig builds the indexing profile from the nine embedding keys.
func (s *Service) IndexingConfig(
	ctx context.Context, q ports.Queryer, tenantID, kbID string,
) (domain.IndexingConfig, error) {
	values, err := s.Resolve(ctx, q, tenantID, kbID, EmbeddingKeys)
	if err != nil {
		return domain.IndexingConfig{}, err
	}
	return indexingFrom(values)
}

// indexingFrom assembles IndexingConfig from already-resolved values, without
// a second table read (key sets nest, so re-resolving repeats one SELECT).
func indexingFrom(values Values) (domain.IndexingConfig, error) {
	maxTokens, err := values.Int(ChunkingMaxTokens)
	if err != nil {
		return domain.IndexingConfig{}, err
	}
	overlap, err := values.Int(ChunkingOverlapTokens)
	if err != nil {
		return domain.IndexingConfig{}, err
	}
	if overlap >= maxTokens {
		return domain.IndexingConfig{}, domain.ConfigurationInvalid(
			"chunking.overlap_tokens должен быть меньше chunking.max_tokens",
			map[string]any{
				"key":                     ChunkingOverlapTokens.Name(),
				"chunking.max_tokens":     maxTokens,
				"chunking.overlap_tokens": overlap,
			},
		)
	}

	provider, err := values.String(EmbeddingProviderID)
	if err != nil {
		return domain.IndexingConfig{}, err
	}
	model, err := values.String(EmbeddingModel)
	if err != nil {
		return domain.IndexingConfig{}, err
	}
	revision, err := values.String(EmbeddingRevision)
	if err != nil {
		return domain.IndexingConfig{}, err
	}
	dimension, err := values.Int(EmbeddingDimension)
	if err != nil {
		return domain.IndexingConfig{}, err
	}
	normalize, err := values.Bool(EmbeddingNormalize)
	if err != nil {
		return domain.IndexingConfig{}, err
	}
	distance, err := values.String(EmbeddingDistance)
	if err != nil {
		return domain.IndexingConfig{}, err
	}
	tokenizer, err := values.String(ChunkingTokenizerID)
	if err != nil {
		return domain.IndexingConfig{}, err
	}

	cfg := domain.IndexingConfig{
		EmbeddingProviderID: provider,
		EmbeddingModel:      model,
		EmbeddingRevision:   revision,
		EmbeddingDimension:  dimension,
		ChunkMaxTokens:      maxTokens,
		ChunkOverlapTokens:  overlap,
		TokenizerID:         tokenizer,
		NormalizeVectors:    normalize,
		Distance:            distance,
	}
	if err := cfg.Validate(); err != nil {
		return domain.IndexingConfig{}, err
	}
	return cfg, nil
}

// Retrieval is the search-time settings: indexing profile plus result size.
type Retrieval struct {
	IndexingConfig domain.IndexingConfig
	TopK           int
}

// ResolveRetrieval validates the search key set.
func (s *Service) ResolveRetrieval(
	ctx context.Context, q ports.Queryer, tenantID, kbID string,
) (Retrieval, error) {
	values, err := s.Resolve(ctx, q, tenantID, kbID, SearchKeys)
	if err != nil {
		return Retrieval{}, err
	}
	cfg, err := indexingFrom(values)
	if err != nil {
		return Retrieval{}, err
	}
	topK, err := values.Int(SearchTopK)
	if err != nil {
		return Retrieval{}, err
	}
	return Retrieval{IndexingConfig: cfg, TopK: topK}, nil
}

// Generation is the answer-time settings.
type Generation struct {
	IndexingConfig   domain.IndexingConfig
	ProviderID       string
	Model            string
	TokenizerID      string
	MaxContextTokens int
	MaxOutputTokens  int
	EnabledTools     []string
}

// InstructionReserveTokens reserves prompt space inside the context budget.
const InstructionReserveTokens = 512

// ResolveGeneration validates the answer key set, including the kb_search
// tool requirement.
func (s *Service) ResolveGeneration(
	ctx context.Context, q ports.Queryer, tenantID, kbID string,
) (Generation, error) {
	values, err := s.Resolve(ctx, q, tenantID, kbID, AnswerKeys)
	if err != nil {
		return Generation{}, err
	}
	cfg, err := indexingFrom(values)
	if err != nil {
		return Generation{}, err
	}
	provider, err := values.String(AnswerProviderID)
	if err != nil {
		return Generation{}, err
	}
	model, err := values.String(AnswerModel)
	if err != nil {
		return Generation{}, err
	}
	tokenizer, err := values.String(AnswerTokenizerID)
	if err != nil {
		return Generation{}, err
	}
	maxContext, err := values.Int(AnswerMaxContextTokens)
	if err != nil {
		return Generation{}, err
	}
	maxOutput, err := values.Int(AnswerMaxOutputTokens)
	if err != nil {
		return Generation{}, err
	}
	tools, err := values.Strings(ToolsEnabled)
	if err != nil {
		return Generation{}, err
	}

	if !slices.Contains(tools, toolKBSearch) {
		return Generation{}, domain.ConfigurationInvalid(
			"tools.enabled должен содержать kb_search",
			map[string]any{"key": ToolsEnabled.Name(), "value": tools},
		)
	}

	return Generation{
		IndexingConfig:   cfg,
		ProviderID:       provider,
		Model:            model,
		TokenizerID:      tokenizer,
		MaxContextTokens: maxContext,
		MaxOutputTokens:  maxOutput,
		EnabledTools:     tools,
	}, nil
}

// DiagnosticSource is one key's diagnostic state (presence only, no values).
// Plain application shape: delivery owns its own Diagnostics DTO and JSON form.
type DiagnosticSource struct {
	Key    Key
	Source Source
	// HasValue reports presence; values are never dumped in diagnostics.
	HasValue bool
}

// Diagnostics is the settings state for the diagnostics scenario.
type Diagnostics struct {
	Valid                      bool
	MissingKeys                []string
	InvalidKeys                []string
	EmbeddingProfileCompatible bool
	Sources                    []DiagnosticSource
}

// Diagnostics reports settings state and never fails on incomplete setup:
// a missing key is its result, not a reason to 409.
func (s *Service) Diagnostics(
	ctx context.Context, q ports.Queryer, tenantID, kbID, fixedProfileID string,
) (Diagnostics, error) {
	if err := ctx.Err(); err != nil {
		return Diagnostics{}, err
	}
	kbLevel, tenantLevel, err := s.repo.Levels(ctx, q, tenantID, kbID)
	if err != nil {
		return Diagnostics{}, err
	}

	diagnostics := Diagnostics{Sources: make([]DiagnosticSource, 0, len(AllKeys))}
	var missing []string
	var invalidKeys []string

	for _, key := range AllKeys {
		raw, source, found := lookup(kbLevel, tenantLevel, key)
		diagnostics.Sources = append(diagnostics.Sources, DiagnosticSource{
			Key:      key,
			Source:   source,
			HasValue: found,
		})
		if !found {
			missing = append(missing, key.Name())
			continue
		}
		if _, err := validate(key, raw); err != nil {
			invalidKeys = append(invalidKeys, key.Name())
		}
	}

	compatible := false
	if !hasAny(missing, EmbeddingKeys) && !hasAny(invalidKeys, EmbeddingKeys) {
		cfg, err := indexingFrom(pick(kbLevel, tenantLevel, EmbeddingKeys))
		switch {
		case err != nil:
			if key, ok := errorKey(err); ok {
				invalidKeys = append(invalidKeys, key)
			}
		case fixedProfileID == "" || fixedProfileID == cfg.ProfileID():
			compatible = true
		default:
			invalidKeys = append(invalidKeys, EmbeddingModel.Name())
		}
	}

	slices.Sort(missing)
	slices.Sort(invalidKeys)
	return Diagnostics{
		Valid:                      len(missing) == 0 && len(invalidKeys) == 0 && compatible,
		MissingKeys:                missing,
		InvalidKeys:                invalidKeys,
		EmbeddingProfileCompatible: compatible,
		Sources:                    diagnostics.Sources,
	}, nil
}

// pick builds Values from already-read levels, avoiding a second SELECT that
// could race a concurrent settings edit.
func pick(kbLevel, tenantLevel map[string]any, keys []Key) Values {
	out := Values{byKey: make(map[string]item, len(keys))}
	for _, key := range keys {
		raw, source, found := lookup(kbLevel, tenantLevel, key)
		if !found {
			continue
		}
		value, err := validate(key, raw)
		if err != nil {
			continue
		}
		out.byKey[key.Name()] = item{key: key, value: value, source: source}
	}
	return out
}

// errorKey extracts the key name from a CONFIGURATION_INVALID, if it has one.
func errorKey(err error) (string, bool) {
	appErr, ok := domain.AsAppError(err)
	if !ok {
		return "", false
	}
	key, ok := appErr.Details["key"].(string)
	if !ok || key == "" {
		return "", false
	}
	return key, true
}

func hasAny(names []string, keys []Key) bool {
	for _, key := range keys {
		if slices.Contains(names, key.Name()) {
			return true
		}
	}
	return false
}

// SourceOf returns where a value came from, for diagnostics and logs.
func (r Values) SourceOf(key Key) Source {
	found, ok := r.byKey[key.Name()]
	if !ok {
		return SourceUnset
	}
	return found.source
}
