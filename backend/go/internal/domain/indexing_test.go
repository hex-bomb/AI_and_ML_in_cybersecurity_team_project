package domain

import (
	"strings"
	"testing"
	"unicode/utf8"
)

const (
	e5ProfileID = "sha256:80be2d2ac6c67d03e51601add1b2d72c6e4b0564ef616a5025b4e7b933724cd5"
	e5Payload   = `{"distance":"cosine","embedding_dimension":384,` +
		`"embedding_model":"intfloat/multilingual-e5-small",` +
		`"embedding_provider_id":"local-hugot","embedding_revision":"pilot-1",` +
		`"normalize_vectors":true}`

	e5ChunkID0      = "sha256:5ad8c3d2895db14d08e50e9ac1bc387197758687c6f0c8ae435bfe580eb0d56e"
	e5ChunkID7      = "sha256:cb761bd7f18523e9309403926461219afa72f5581d6a3a5c369eb9b263c0b732"
	e5ChunkPayload0 = `{"chunk_max_tokens":600,"chunk_number":0,"chunk_overlap_tokens":100,` +
		`"embedding_profile_id":"` + e5ProfileID + `",` +
		`"tokenizer_id":"hugot/multilingual-e5-small",` +
		`"version_id":"44444444-4444-4444-8444-444444444444"}`

	minilmProfileID = "sha256:771444588ad996325dc6bf1e3442673e01da9bf81abb13f9febd06dcba658b78"
	minilmChunkID7  = "sha256:66c4ca4b8318a978d4297747643a27f82698cc73427bcda657b4e04da111ea23"

	nonasciiProfileID = "sha256:d28d39881e15459bba67724ed91fc85ff4fb6db4a51dcac119987eb1879a340c"
	nonasciiChunkID0  = "sha256:f63681aa1512c6f2bda7e2455b022b3a9bcd43528f51de62dd1295136fcfd0f0"

	nonasciiPayload = `{"distance":"cosine","embedding_dimension":384,` +
		`"embedding_model":"intfloat/multilingual-e5-small",` +
		`"embedding_provider_id":"\u043b\u043e\u043a\u0430\u043b\u044c\u043d\u044b\u0439",` +
		`"embedding_revision":"\u043f\u0438\u043b\u043e\u0442-1","normalize_vectors":true}`
)

func e5Config() IndexingConfig {
	return IndexingConfig{
		EmbeddingProviderID: "local-hugot",
		EmbeddingModel:      "intfloat/multilingual-e5-small",
		EmbeddingRevision:   "pilot-1",
		EmbeddingDimension:  384,
		ChunkMaxTokens:      600,
		ChunkOverlapTokens:  100,
		TokenizerID:         "hugot/multilingual-e5-small",
		NormalizeVectors:    true,
		Distance:            "cosine",
	}
}

func minilmConfig() IndexingConfig {
	return IndexingConfig{
		EmbeddingRevision:   "pilot-2",
		EmbeddingModel:      "sentence-transformers/all-MiniLM-L6-v2",
		EmbeddingProviderID: "local-hugot",
		EmbeddingDimension:  768,
		ChunkMaxTokens:      800,
		ChunkOverlapTokens:  80,
		TokenizerID:         "hugot/all-MiniLM-L6-v2-wordpiece",
		NormalizeVectors:    false,
		Distance:            "dot",
	}
}

func nonasciiConfig() IndexingConfig {
	return IndexingConfig{
		EmbeddingProviderID: "локальный",
		EmbeddingModel:      "intfloat/multilingual-e5-small",
		EmbeddingRevision:   "пилот-1",
		EmbeddingDimension:  384,
		ChunkMaxTokens:      600,
		ChunkOverlapTokens:  100,
		TokenizerID:         "hugot/мультиязычный",
		NormalizeVectors:    true,
		Distance:            "cosine",
	}
}

func profilePayload(c IndexingConfig) string {
	return string(emptyCanonicalObject().
		withString("distance", c.Distance).
		withInt("embedding_dimension", c.EmbeddingDimension).
		withString("embedding_model", c.EmbeddingModel).
		withString("embedding_provider_id", c.EmbeddingProviderID).
		withString("embedding_revision", c.EmbeddingRevision).
		withBool("normalize_vectors", c.NormalizeVectors).
		encode())
}

func chunkPayload(p ChunkIDParams) string {
	return string(emptyCanonicalObject().
		withInt("chunk_max_tokens", p.MaxTokens).
		withInt("chunk_number", p.Number).
		withInt("chunk_overlap_tokens", p.Overlap).
		withString("embedding_profile_id", p.ProfileID).
		withString("tokenizer_id", p.TokenizerID).
		withString("version_id", p.VersionID).
		encode())
}

func TestProfileIDMatchesPython(t *testing.T) {
	cases := []struct {
		name string
		cfg  IndexingConfig
		want string
	}{
		{"e5", e5Config(), e5ProfileID},
		{"minilm", minilmConfig(), minilmProfileID},
		{"nonascii", nonasciiConfig(), nonasciiProfileID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.ProfileID(); got != tc.want {
				t.Errorf("ProfileID()\n получено %s\n ожидалось %s\n payload  %s",
					got, tc.want, profilePayload(tc.cfg))
			}
		})
	}
}

func TestCanonicalPayloadMatchesPython(t *testing.T) {
	if got := profilePayload(e5Config()); got != e5Payload {
		t.Errorf("payload профиля e5\n получено %s\n ожидалось %s", got, e5Payload)
	}
}

func TestChunkIDMatchesPython(t *testing.T) {
	cfg := e5Config()
	cases := []struct {
		name   string
		params ChunkIDParams
		want   string
	}{
		{
			"e5 chunk 0",
			ChunkIDParams{
				VersionID:   "44444444-4444-4444-8444-444444444444",
				ProfileID:   cfg.ProfileID(),
				TokenizerID: cfg.TokenizerID,
				MaxTokens:   600,
				Overlap:     100,
				Number:      0,
			},
			e5ChunkID0,
		},
		{
			"e5 chunk 7",
			ChunkIDParams{
				VersionID:   "55555555-5555-4555-8555-555555555555",
				ProfileID:   cfg.ProfileID(),
				TokenizerID: cfg.TokenizerID,
				MaxTokens:   600,
				Overlap:     100,
				Number:      7,
			},
			e5ChunkID7,
		},
		{
			"minilm chunk 7",
			ChunkIDParams{
				VersionID:   "55555555-5555-4555-8555-555555555555",
				ProfileID:   minilmConfig().ProfileID(),
				TokenizerID: minilmConfig().TokenizerID,
				MaxTokens:   800,
				Overlap:     80,
				Number:      7,
			},
			minilmChunkID7,
		},
		{
			"nonascii chunk 0",
			ChunkIDParams{
				VersionID:   "44444444-4444-4444-8444-444444444444",
				ProfileID:   nonasciiConfig().ProfileID(),
				TokenizerID: nonasciiConfig().TokenizerID,
				MaxTokens:   600,
				Overlap:     100,
				Number:      0,
			},
			nonasciiChunkID0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ChunkIDFor(tc.params); got != tc.want {
				t.Errorf("ChunkIDFor\n получено %s\n ожидалось %s\n payload  %s",
					got, tc.want, chunkPayload(tc.params))
			}
		})
	}
}

func TestChunkPayloadMatchesPython(t *testing.T) {
	p := ChunkIDParams{
		VersionID:   "44444444-4444-4444-8444-444444444444",
		ProfileID:   e5ProfileID,
		TokenizerID: "hugot/multilingual-e5-small",
		MaxTokens:   600,
		Overlap:     100,
		Number:      0,
	}
	if got := chunkPayload(p); got != e5ChunkPayload0 {
		t.Errorf("payload чанка\n получено %s\n ожидалось %s", got, e5ChunkPayload0)
	}
}

func TestProfileIDIsStableAcrossRuns(t *testing.T) {
	cfg := e5Config()
	first := cfg.ProfileID()
	for i := 0; i < 100; i++ {
		if cfg.ProfileID() != first {
			t.Fatalf("ProfileID() изменился на итерации %d", i)
		}
	}
}

func TestProfileIDIgnoresChunkingParameters(t *testing.T) {
	base := e5Config()
	changed := base
	changed.ChunkMaxTokens = 1200
	changed.ChunkOverlapTokens = 300
	changed.TokenizerID = "hugot/other-tokenizer"

	if base.ProfileID() != changed.ProfileID() {
		t.Errorf("параметры чанкинга не должны входить в profile_id: %s != %s",
			base.ProfileID(), changed.ProfileID())
	}
}

func TestProfileIDChangesWithVectorParameters(t *testing.T) {
	base := e5Config()
	cases := map[string]func(c *IndexingConfig){
		"модель":       func(c *IndexingConfig) { c.EmbeddingModel = "other/model" },
		"размерность":  func(c *IndexingConfig) { c.EmbeddingDimension = 768 },
		"ревизия":      func(c *IndexingConfig) { c.EmbeddingRevision = "pilot-2" },
		"провайдер":    func(c *IndexingConfig) { c.EmbeddingProviderID = "other" },
		"нормализация": func(c *IndexingConfig) { c.NormalizeVectors = false },
		"расстояние":   func(c *IndexingConfig) { c.Distance = "dot" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			changed := base
			mutate(&changed)
			if base.ProfileID() == changed.ProfileID() {
				t.Errorf("смена %s обязана менять profile_id, но он остался %s",
					name, base.ProfileID())
			}
		})
	}
}

func TestIndexNameMatchesPython(t *testing.T) {
	const want = "kb-chunks-v1-80be2d2ac6c67d03e51601add1b2d72c6e4b0564ef616a5025b4e7b933724cd5"
	if got := e5Config().IndexName(); got != want {
		t.Errorf("IndexName() = %s, ожидалось %s", got, want)
	}
}

func TestCanonicalJSONMatchesPythonEnsureASCII(t *testing.T) {
	got := profilePayload(nonasciiConfig())
	if got != nonasciiPayload {
		t.Errorf("канонический payload не-ASCII профиля\n получено %s\n ожидалось %s",
			got, nonasciiPayload)
	}
}

func TestCanonicalJSONEscapesNonASCII(t *testing.T) {
	const expected = `{"distance":"cosine","embedding_dimension":384,` +
		`"embedding_model":"intfloat/multilingual-e5-small",` +
		`"embedding_provider_id":"\u043b\u043e\u043a\u0430\u043b\u044c\u043d\u044b\u0439",` +
		`"embedding_revision":"\u043f\u0438\u043b\u043e\u0442-1","normalize_vectors":true}`

	got := profilePayload(nonasciiConfig())
	if got != expected {
		t.Errorf("получено %s, ожидалось %s", got, expected)
	}

	for i := 0; i < len(got); i++ {
		if got[i] >= utf8.RuneSelf {
			t.Fatalf("в каноническом JSON остался не-экранированный байт 0x%02x "+
				"в позиции %d: %s — Python ensure_ascii его экранировал бы", got[i], i, got)
		}
	}
}

func TestCanonicalJSONDiffersFromRawUTF8(t *testing.T) {
	rawUTF8 := `{"distance":"cosine","embedding_dimension":384,` +
		`"embedding_model":"intfloat/multilingual-e5-small",` +
		`"embedding_provider_id":"локальный",` +
		`"embedding_revision":"пилот-1","normalize_vectors":true}`

	escaped := profilePayload(nonasciiConfig())
	if escaped == rawUTF8 {
		t.Fatal("экранированный payload совпал с сырым UTF-8: тест перестал " +
			"различать ensure_ascii и его отсутствие")
	}
	if !strings.Contains(escaped, `\u043b`) {
		t.Errorf("в payload нет escape-последовательности \\u043l: %s", escaped)
	}
	if sha256Prefixed([]byte(escaped)) != nonasciiProfileID {
		t.Errorf("хэш ЭСКРАНИРОВАННОГО payload %s не совпал с эталоном Python %s",
			sha256Prefixed([]byte(escaped)), nonasciiProfileID)
	}
	if sha256Prefixed([]byte(rawUTF8)) == nonasciiProfileID {
		t.Error("хэш СЫРОГО UTF-8 совпал с эталоном Python — эталон перестал " +
			"фиксировать ensure_ascii")
	}
}

func TestEscapeStringMatchesCPython(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"пусто", "", `""`},
		{"ascii", "abcXYZ019", `"abcXYZ019"`},
		{"кавычка и слэш", `a"b`, `"a\"b"`},
		{"обратный слэш", `a\b`, `"a\\b"`},
		{"перевод строки", "a\nb", `"a\nb"`},
		{"таб и возврат", "a\tb\rc", `"a\tb\rc"`},
		{"backspace formfeed", "a\bb\fc", `"a\bb\fc"`},
		{"управляющий 0x01", "a\x01b", `"a\u0001b"`},
		{"кириллица", "Привет", `"\u041f\u0440\u0438\u0432\u0435\u0442"`},
		{"latin1 e-acute", "\u00e9", `"\u00e9"`},
		{"евро", "\u20ac", `"\u20ac"`},
		{"эмодзи вне BMP", "\U0001F600", `"\ud83d\ude00"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(appendEscapedJSONString(nil, tc.in))
			if got != tc.want {
				t.Errorf("экранирование %q\n получено %s\n ожидалось %s", tc.in, got, tc.want)
			}
		})
	}
}

func TestCanonicalJSONSortsKeys(t *testing.T) {
	forward := emptyCanonicalObject().
		withString("alpha", "1").
		withInt("beta", 2).
		withBool("gamma", true)
	reverse := emptyCanonicalObject().
		withBool("gamma", true).
		withInt("beta", 2).
		withString("alpha", "1")

	const want = `{"alpha":"1","beta":2,"gamma":true}`
	if got := string(forward.encode()); got != want {
		t.Errorf("прямой порядок: получено %s, ожидалось %s", got, want)
	}
	if got := string(reverse.encode()); got != want {
		t.Errorf("обратный порядок: получено %s, ожидалось %s", got, want)
	}
}

func TestCanonicalJSONUsesCompactSeparators(t *testing.T) {
	obj := emptyCanonicalObject().withInt("a", 1).withInt("b", 2)
	const want = `{"a":1,"b":2}`
	if got := string(obj.encode()); got != want {
		t.Errorf("получено %s, ожидалось %s", got, want)
	}
}

func TestCanonicalObjectWithDoesNotMutate(t *testing.T) {
	first := emptyCanonicalObject().withString("k", "v1")
	second := first.withString("k", "v2")

	if got := string(first.encode()); got != `{"k":"v1"}` {
		t.Errorf("исходный объект изменился: %s", got)
	}
	if got := string(second.encode()); got != `{"k":"v2"}` {
		t.Errorf("новый объект неверен: %s", got)
	}
}
