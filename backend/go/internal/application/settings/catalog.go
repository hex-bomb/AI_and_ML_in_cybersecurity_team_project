// Package settings reads knowledge-base settings with KB-over-tenant priority.
package settings

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ValueType is the logical value type, matching the settings.value_type column.
type ValueType string

const (
	TypeString      ValueType = "string"
	TypeInteger     ValueType = "integer"
	TypeBoolean     ValueType = "boolean"
	TypeStringArray ValueType = "array[string]"
)

const (
	SubtypeModel      = "model"
	SubtypeIngestion  = "ingestion"
	SubtypeRetrieval  = "retrieval"
	SubtypeGeneration = "generation"
	SubtypeTools      = "tools"
)

// Level is the level a key is set at; tenant rows have NULL kb_id.
type Level string

const (
	LevelTenant Level = "tenant"
	LevelKB     Level = "kb"
)

// Key is a typed catalog descriptor. Instances exist only here, so Resolve
// cannot receive an unknown name: a typo fails to compile.
type Key struct {
	name      string
	valueType ValueType
	subtype   string
	level     Level
	required  bool
	def       any
}

// Name is the analyst-visible key name.
func (k Key) Name() string { return k.name }

// ValueType is the expected logical type.
func (k Key) ValueType() ValueType { return k.valueType }

// Subtype is the settings group.
func (k Key) Subtype() string { return k.subtype }

// Level is the level the key is set at.
func (k Key) Level() Level { return k.level }

// Required reports whether the key is mandatory. All catalog keys are.
func (k Key) Required() bool { return k.required }

// HasDefault reports a silent default. Always false by contract.
func (k Key) HasDefault() bool { return k.def != nil }

// String prints the key name for errors and logs.
func (k Key) String() string { return k.name }

// Key catalog: the first nine build IndexingConfig, then search.top_k,
// then the six answer-generation keys.
var (
	EmbeddingProviderID = Key{
		name: "search.embedding_provider_id", valueType: TypeString, subtype: SubtypeModel,
		level: LevelTenant, required: true,
	}
	EmbeddingModel = Key{
		name: "search.embedding_model", valueType: TypeString, subtype: SubtypeModel,
		level: LevelTenant, required: true,
	}
	EmbeddingRevision = Key{
		name: "search.embedding_revision", valueType: TypeString, subtype: SubtypeModel,
		level: LevelTenant, required: true,
	}
	EmbeddingDimension = Key{
		name: "search.embedding_dimension", valueType: TypeInteger, subtype: SubtypeModel,
		level: LevelTenant, required: true,
	}
	EmbeddingNormalize = Key{
		name: "search.embedding_normalize", valueType: TypeBoolean, subtype: SubtypeModel,
		level: LevelTenant, required: true,
	}
	EmbeddingDistance = Key{
		name: "search.embedding_distance", valueType: TypeString, subtype: SubtypeModel,
		level: LevelTenant, required: true,
	}
	ChunkingTokenizerID = Key{
		name: "chunking.tokenizer_id", valueType: TypeString, subtype: SubtypeIngestion,
		level: LevelTenant, required: true,
	}
	ChunkingMaxTokens = Key{
		name: "chunking.max_tokens", valueType: TypeInteger, subtype: SubtypeIngestion,
		level: LevelTenant, required: true,
	}
	ChunkingOverlapTokens = Key{
		name: "chunking.overlap_tokens", valueType: TypeInteger, subtype: SubtypeIngestion,
		level: LevelTenant, required: true,
	}
	SearchTopK = Key{
		name: "search.top_k", valueType: TypeInteger, subtype: SubtypeRetrieval,
		level: LevelTenant, required: true,
	}
	AnswerProviderID = Key{
		name: "answer.provider_id", valueType: TypeString, subtype: SubtypeModel,
		level: LevelTenant, required: true,
	}
	AnswerModel = Key{
		name: "answer.model", valueType: TypeString, subtype: SubtypeModel,
		level: LevelTenant, required: true,
	}
	AnswerTokenizerID = Key{
		name: "answer.tokenizer_id", valueType: TypeString, subtype: SubtypeGeneration,
		level: LevelTenant, required: true,
	}
	AnswerMaxContextTokens = Key{
		name: "answer.max_context_tokens", valueType: TypeInteger, subtype: SubtypeGeneration,
		level: LevelTenant, required: true,
	}
	AnswerMaxOutputTokens = Key{
		name: "answer.max_output_tokens", valueType: TypeInteger, subtype: SubtypeGeneration,
		level: LevelTenant, required: true,
	}
	ToolsEnabled = Key{
		name: "tools.enabled", valueType: TypeStringArray, subtype: SubtypeTools,
		level: LevelTenant, required: true,
	}
)

// Operation key sets, declared explicitly as part of the contract.
var (
	// EmbeddingKeys builds IndexingConfig.
	EmbeddingKeys = []Key{
		EmbeddingProviderID,
		EmbeddingModel,
		EmbeddingRevision,
		EmbeddingDimension,
		EmbeddingNormalize,
		EmbeddingDistance,
		ChunkingTokenizerID,
		ChunkingMaxTokens,
		ChunkingOverlapTokens,
	}

	// SearchKeys is EmbeddingKeys plus search.top_k.
	SearchKeys = withKeys(EmbeddingKeys, SearchTopK)

	// AnswerKeys is SearchKeys plus answer-generation keys.
	AnswerKeys = withKeys(SearchKeys,
		AnswerProviderID,
		AnswerModel,
		AnswerTokenizerID,
		AnswerMaxContextTokens,
		AnswerMaxOutputTokens,
		ToolsEnabled,
	)

	// AllKeys is the whole catalog for diagnostics.
	AllKeys = withKeys(AnswerKeys)
)

// withKeys copies: appending into a shared backing array would leak keys
// across sets.
func withKeys(base []Key, extra ...Key) []Key {
	out := make([]Key, 0, len(base)+len(extra))
	out = append(out, base...)
	return append(out, extra...)
}

// toolKBSearch is the only MVP tool.
const toolKBSearch = "kb_search"

// TopKMax caps search.top_k; search clamps to the same value.
const TopKMax = 50

// invalidValue carries the check reason; the caller formats the full message.
type invalidValue struct {
	reason string
}

func (e *invalidValue) Error() string { return e.reason }

func invalidReason(err error) string {
	var typed *invalidValue
	if errors.As(err, &typed) {
		return typed.reason
	}
	return err.Error()
}

// validate checks and normalizes a value to its canonical Go type.
func validate(key Key, raw any) (any, error) {
	switch key.valueType {
	case TypeString:
		return textValue(key, raw)
	case TypeInteger:
		return intValue(key, raw)
	case TypeBoolean:
		return boolValue(key, raw)
	case TypeStringArray:
		return stringArrayValue(key, raw)
	default:
		return nil, &invalidValue{reason: fmt.Sprintf("неизвестный тип ключа %s", key.valueType)}
	}
}

func textValue(key Key, raw any) (any, error) {
	text, ok := raw.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return nil, &invalidValue{reason: "ожидается непустая строка"}
	}
	if key == EmbeddingDistance && text != "cosine" {
		return nil, &invalidValue{reason: "в MVP поддерживается только cosine"}
	}
	return text, nil
}

// intValue takes int64 (jsonb) or int (Go callers); float64/json.Number are
// rejected so 384.0 never passes as 384.
func intValue(key Key, raw any) (any, error) {
	var number int64
	switch typed := raw.(type) {
	case int64:
		number = typed
	case int:
		number = int64(typed)
	default:
		return nil, &invalidValue{reason: "ожидается целое число"}
	}
	if number <= 0 {
		return nil, &invalidValue{reason: "ожидается положительное целое"}
	}
	if key == SearchTopK && number > TopKMax {
		return nil, &invalidValue{
			reason: fmt.Sprintf("ожидается целое от 1 до %d", TopKMax),
		}
	}
	return number, nil
}

func boolValue(_ Key, raw any) (any, error) {
	value, ok := raw.(bool)
	if !ok {
		return nil, &invalidValue{reason: "ожидается true или false"}
	}
	return value, nil
}

func stringArrayValue(key Key, raw any) (any, error) {
	items, ok := raw.([]any)
	if !ok {
		return nil, &invalidValue{reason: "ожидается массив строк"}
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil, &invalidValue{reason: "ожидается массив строк"}
		}
		out = append(out, text)
	}
	if key == ToolsEnabled {
		for _, tool := range out {
			if tool != toolKBSearch {
				return nil, &invalidValue{reason: "допустимые значения: [kb_search]"}
			}
		}
	}
	return out, nil
}

// Catalog returns the keys in fixed slice order; map order would flap.
// The slice is a copy.
func Catalog() []Key {
	return slices.Clone(AllKeys)
}
