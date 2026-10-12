//go:build e5

// Тяжёлые тесты с настоящей ONNX-моделью (тег e5; без кэша модель скачивается сама).
//
// Запуск:
//
//	make test-e5
//	EMBEDDING_MODEL_CACHE=/tmp/hugottest/models make test-e5
package embeddings

import (
	"context"
	"math"
	"os"
	"testing"
	"time"
)

func liveConfig(t *testing.T) Config {
	t.Helper()
	model := os.Getenv("EMBEDDING_MODEL")
	if model == "" {
		model = "intfloat/multilingual-e5-small"
	}
	dim := 384
	if model == "sentence-transformers/all-MiniLM-L6-v2" {
		// У MiniLM своя размерность; проверка идёт по факту, но конфиг
		// должен совпадать, иначе проверка не дойдёт до векторов.
		dim = 384
	}
	return Config{Model: model, Dimension: dim, CacheDir: os.Getenv("EMBEDDING_MODEL_CACHE"),
		BatchSize: 32, Concurrency: 2}
}

// корпус — те же шесть пассажей, на которых замерялось решение между
// MiniLM и E5. Тексты взяты из contracts/fixtures/vektor.
var (
	passages = map[string]string{
		"муфта":  "Момент затяжки защитной муфты насоса — 45 Нм.",
		"ТО":     "Плановое техническое обслуживание проводится каждые 500 моточасов.",
		"ковш":   "Глубина копания экскаватора ВЕКТОР-EC-22 составляет 6 720 мм.",
		"оплата": "Оплата производится двумя частями: 40% при подписании, 60% после приёмки.",
		"ОТ":     "Запрещено находиться в зоне работающего шпинделя.",
		"робот":  "Сварочный робот ВЕКТОР-RW-6 повторяет движение с точностью ±0,05 мм.",
	}
	queries = map[string]string{
		"муфта":  "Насколько сильно затягивать муфту насоса?",
		"ТО":     "Как часто нужно обслуживать насос?",
		"ковш":   "На какую глубину копает экскаватор?",
		"оплата": "Как платить за технику?",
		"ОТ":     "Что нельзя делать при работе станка?",
		"робот":  "Насколько точен сварочный робот?",
	}
)

// TestLiveModelProducesConfiguredDimension — базовая гарантия: модель даёт
// столько измерений, сколько заявлено в профиле.
func TestLiveModelProducesConfiguredDimension(t *testing.T) {
	e := New(liveConfig(t))
	cfg := testConfig(liveConfig(t).Dimension)

	got, err := e.Embed(context.Background(), []string{"проверка"}, cfg)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(got[0]) != e.cfg.Dimension {
		t.Fatalf("размерность %d, ожидалось %d", len(got[0]), e.cfg.Dimension)
	}
}

// TestLiveNormalizationGivesUnitLength — при нормализации длина вектора
// равна единице, иначе косинус в Elasticsearch считается от произвольных
func TestLiveNormalizationGivesUnitLength(t *testing.T) {
	cfg := testConfig(liveConfig(t).Dimension)
	cfg.NormalizeVectors = true

	got, err := New(liveConfig(t)).Embed(context.Background(), []string{"проверка нормализации"}, cfg)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if n := norm(got[0]); math.Abs(n-1) > 1e-4 {
		t.Errorf("длина = %v, ожидалось 1", n)
	}
}

// TestLiveBatchEqualsSingle — батчинг не меняет результат. Считается на
// настоящих числах, потому что на синтетике ошибка индекса была бы незаметна.
func TestLiveBatchEqualsSingle(t *testing.T) {
	cfgE := liveConfig(t)
	cfg := testConfig(cfgE.Dimension)

	keys := []string{"муфта", "ТО", "ковш", "оплата", "ОТ", "робот"}
	texts := make([]string, 0, len(keys))
	for _, k := range keys {
		texts = append(texts, passages[k])
	}

	// Один большой батч.
	batchCfg := cfgE
	batchCfg.BatchSize = len(texts)
	batchCfg.Concurrency = 1
	batched, err := New(batchCfg).Embed(context.Background(), texts, cfg)
	if err != nil {
		t.Fatalf("батч: %v", err)
	}

	// По одному тексту за вызов.
	oneCfg := cfgE
	oneCfg.BatchSize = 1
	oneCfg.Concurrency = 1
	single, err := New(oneCfg).Embed(context.Background(), texts, cfg)
	if err != nil {
		t.Fatalf("поштучно: %v", err)
	}

	for i := range texts {
		if c := cosine(batched[i], single[i]); math.Abs(c-1) > 1e-4 {
			t.Errorf("текст %d: косинус батча и поштучного = %v, ожидалось 1", i, c)
		}
	}
}

// TestLiveParaphraseRetrieval — смысловая выдача на русском.
// Это главный критерий выбора E5: перефразированный вопрос должен находить
func TestLiveParaphraseRetrieval(t *testing.T) {
	cfg := testConfig(liveConfig(t).Dimension)
	ctx := context.Background()

	keys := []string{"муфта", "ТО", "ковш", "оплата", "ОТ", "робот"}
	docs := make([]string, 0, len(keys))
	for _, k := range keys {
		docs = append(docs, passages[k])
	}
	docVectors, err := New(liveConfig(t)).EmbedRole(ctx, docs, RoleDocument, cfg)
	if err != nil {
		t.Fatalf("документы: %v", err)
	}

	hit1, hitAny := 0, 0
	for _, key := range keys {
		qVec, err := New(liveConfig(t)).EmbedRole(ctx, []string{queries[key]}, RoleQuery, cfg)
		if err != nil {
			t.Fatalf("запрос %q: %v", key, err)
		}

		best, bestIdx, target := -2.0, 0, -1.0
		targetIdx := -1
		for i, k := range keys {
			c := cosine(qVec[0], docVectors[i])
			if c > best {
				best, bestIdx = c, i
			}
			if k == key {
				target, targetIdx = c, i
			}
		}
		if bestIdx == targetIdx {
			hit1++
		}
		if target > 0.5 {
			hitAny++
		}
		t.Logf("запрос %-7s -> %-7s косинус %.3f (нужный %.3f)", key, keys[bestIdx], best, target)
	}

	t.Logf("Top-1: %d/%d, косинус к нужному > 0.5: %d/%d", hit1, len(keys), hitAny, len(keys))
	if hit1 < 4 {
		t.Errorf("Top-1 %d из %d: перефразированные вопросы ищутся хуже, чем MiniLM давал 3/6", hit1, len(keys))
	}
}

// TestLivePrefixesChangeResult — префиксы E5 влияют на вектор. Без такой
// проверки легко снести их рефакторингом и не заметить.
func TestLivePrefixesChangeResult(t *testing.T) {
	cfg := testConfig(liveConfig(t).Dimension)
	ctx := context.Background()
	const text = "Момент затяжки защитной муфты"

	doc, err := New(liveConfig(t)).EmbedRole(ctx, []string{text}, RoleDocument, cfg)
	if err != nil {
		t.Fatalf("document: %v", err)
	}
	q, err := New(liveConfig(t)).EmbedRole(ctx, []string{text}, RoleQuery, cfg)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if math.Abs(cosine(doc[0], q[0])-1) < 1e-6 {
		t.Error("префиксы не изменили вектор: они не применяются")
	}
}

// TestLiveSpeed — грубая оценка производительности на CPU. Порог намеренно
// щедрый: тест должен ловить регрессию на порядок, а не мерить микросекунды.
func TestLiveSpeed(t *testing.T) {
	cfg := testConfig(liveConfig(t).Dimension)
	texts := make([]string, 12)
	for i := range texts {
		texts[i] = "текст для замера скорости эмбеддера номер " + string(rune('A'+i))
	}

	start := time.Now()
	if _, err := New(liveConfig(t)).Embed(context.Background(), texts, cfg); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	elapsed := time.Since(start)
	per := elapsed / time.Duration(len(texts))
	t.Logf("%d текстов за %v (%.0f мс на текст)", len(texts), elapsed.Round(time.Millisecond),
		float64(elapsed.Milliseconds())/float64(len(texts)))
	if per > 500*time.Millisecond {
		t.Errorf("%.0f мс на текст: на порядок медленнее ожидаемого ~30 мс", float64(per.Milliseconds()))
	}
}
