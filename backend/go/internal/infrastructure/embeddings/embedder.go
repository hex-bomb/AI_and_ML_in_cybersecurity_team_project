// Package embeddings — порт эмбеддера на hugot (локальный ONNX без сети).
// Модель E5 требует префиксов «query: »/«passage: » — их задаёт Role.
package embeddings

import (
	"context"
	"math"
	"os"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/kbsearch/backend-go/internal/application/ports"
	"github.com/kbsearch/backend-go/internal/domain"
)

// Role — назначение текста (определяет префикс E5).
type Role int

const (
	// RoleDocument — текст, который индексируется. Префикс «passage: ».
	RoleDocument Role = iota
	// RoleQuery — поисковый вопрос пользователя. Префикс «query: ».
	RoleQuery
)

// prefix возвращает требуемый E5 префикс.
func (r Role) prefix() string {
	switch r {
	case RoleQuery:
		return "query: "
	case RoleDocument:
		return "passage: "
	default:
		return ""
	}
}

// Config — параметры эмбеддера из окружения.
type Config struct {
	// Model — идентификатор модели в каталоге Hugging Face.
	Model string
	// Dimension — ожидаемая размерность вектора.
	Dimension int
	// CacheDir — каталог, куда скачивается ONNX. Пустой — кэш hugot по умолчанию.
	CacheDir string
	// BatchSize — сколько текстов кодируется за один вызов RunPipeline.
	BatchSize int
	// Concurrency — сколько горутин считают (сессия hugot общая).
	Concurrency int
}

// withDefaults подставляет значения по умолчанию.
func (c Config) withDefaults() Config {
	if c.BatchSize <= 0 {
		c.BatchSize = 32
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 2
	}
	return c
}

// Embedder — реализация ports.Embedder поверх hugot (ленивая загрузка под sync.Once).

var _ ports.Embedder = (*Embedder)(nil)

type Embedder struct {
	cfg Config

	mu       sync.Mutex
	session  Session
	pipeline Session
}

// Session — минимальный срез hugot ради заглушек в тестах без ONNX-рантайма.
type Session interface {
	RunPipeline(ctx context.Context, texts []string) (*Result, error)
}

// Result — ответ пайплайна (локальный тип, чтобы тесты не тянули ONNX-рантайм).
type Result struct {
	Embeddings [][]float32
}

// newSessionFunc создаёт настоящую hugot-сессию. Подменяется в тестах.
var newSessionFunc = newHugotSession

// New создаёт эмбеддер. Модель при этом НЕ загружается.
func New(cfg Config) *Embedder {
	return &Embedder{cfg: cfg.withDefaults()}
}

// Dimensions возвращает заявленную размерность (не блокируется загрузкой модели).
func (e *Embedder) Dimensions() int { return e.cfg.Dimension }

// Embed кодирует тексты как документы.
func (e *Embedder) Embed(ctx context.Context, texts []string, cfg domain.IndexingConfig) ([][]float32, error) {
	return e.EmbedRole(ctx, texts, RoleDocument, cfg)
}

// EmbedQuery кодирует один поисковый вопрос.
func (e *Embedder) EmbedQuery(ctx context.Context, text string, cfg domain.IndexingConfig) ([]float32, error) {
	vectors, err := e.EmbedRole(ctx, []string{text}, RoleQuery, cfg)
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}

// EmbedRole кодирует тексты с явной ролью и проверяет профиль.
func (e *Embedder) EmbedRole(
	ctx context.Context, texts []string, role Role, cfg domain.IndexingConfig,
) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	// Размерность — до загрузки модели (не качать 448 МБ ради неверного ответа).
	if cfg.EmbeddingDimension != 0 && cfg.EmbeddingDimension != e.cfg.Dimension {
		return nil, domain.EmbeddingDimensionMismatch(
			"Размерность настройки не совпала с моделью эмбеддера",
			cfg.EmbeddingDimension, e.cfg.Dimension,
		)
	}

	pipe, err := e.pipelineFor(ctx)
	if err != nil {
		return nil, err
	}

	prefixed := make([]string, len(texts))
	for i, text := range texts {
		prefixed[i] = role.prefix() + text
	}

	vectors, err := e.runBatches(ctx, pipe, prefixed, cfg.NormalizeVectors)
	if err != nil {
		return nil, err
	}

	// Проверка размерности после вычисления — ошибка профиля.
	for i, v := range vectors {
		if len(v) != e.cfg.Dimension {
			return nil, domain.EmbeddingDimensionMismatch(
				"Модель вернула вектор неожиданной размерности",
				len(v), e.cfg.Dimension,
			).WithDetails(map[string]any{"text_index": i})
		}
	}
	return vectors, nil
}

// runBatches считает батчи параллельно (ошибка одного отменяет остальные).
func (e *Embedder) runBatches(
	ctx context.Context, pipe Session, texts []string, normalize bool,
) ([][]float32, error) {
	batches := split(texts, e.cfg.BatchSize)
	out := make([][]float32, len(texts))

	group, gctx := errgroup.WithContext(ctx)
	group.SetLimit(e.cfg.Concurrency)
	results := make([][][]float32, len(batches))

	// Переменные цикла свои на итерацию (Go 1.22+): захват в замыкание безопасен.
	for i, batch := range batches {
		group.Go(func() error {
			vectors, err := encode(gctx, pipe, batch, normalize)
			if err != nil {
				return err
			}
			results[i] = vectors
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}

	for i, vectors := range results {
		copy(out[i*e.cfg.BatchSize:], vectors)
	}
	return out, nil
}

// encode зовёт пайплайн и приводит векторы к нужному виду.
func encode(ctx context.Context, pipe Session, texts []string, normalize bool) ([][]float32, error) {
	// Контекст отвязывается от отмены: бэкенд hugot ломает разделяемую сессию,
	// если переданный в RunPipeline контекст отменяют даже после возврата —
	// первый HTTP-запрос проходил бы, а все следующие получали ошибку.
	res, err := pipe.RunPipeline(context.WithoutCancel(ctx), texts)
	if err != nil {
		return nil, domain.EmbeddingResponseInvalid("Модель не вернула эмбеддинги").WithCause(err)
	}
	if res == nil || len(res.Embeddings) != len(texts) {
		got := 0
		if res != nil {
			got = len(res.Embeddings)
		}
		return nil, domain.EmbeddingResponseInvalid("Число векторов не совпало с числом текстов").
			WithDetails(map[string]any{"texts": len(texts), "embeddings": got})
	}
	if !normalize {
		return res.Embeddings, nil
	}
	for i, v := range res.Embeddings {
		res.Embeddings[i] = normalizeVec(v)
	}
	return res.Embeddings, nil
}

// pipelineFor лениво создаёт сессию. Неудача не кэшируется: временный сбой
// (модель докачивается соседним процессом) лечится следующим вызовом.
func (e *Embedder) pipelineFor(ctx context.Context) (Session, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pipeline != nil {
		return e.pipeline, nil
	}
	if e.cfg.CacheDir != "" {
		if err := os.MkdirAll(e.cfg.CacheDir, 0o755); err != nil {
			return nil, domain.ServiceUnavailable("Эмбеддер недоступен").WithCause(err)
		}
	}
	// Сессия привязывается к переданному контексту: отмена HTTP-запроса,
	// создавшего сессию, ломала бы все последующие вызовы.
	session, err := newSessionFunc(context.WithoutCancel(ctx), e.cfg)
	if err != nil {
		return nil, domain.ServiceUnavailable("Эмбеддер недоступен").WithCause(err)
	}
	e.session, e.pipeline = session, session
	return e.pipeline, nil
}

// Ping поднимает модель.
func (e *Embedder) Ping(ctx context.Context) error {
	_, err := e.pipelineFor(ctx)
	return err
}

// Close освобождает сессию. Повторный вызов безопасен.
func (e *Embedder) Close() error {
	if e.session == nil {
		return nil
	}
	if closer, ok := e.session.(interface{ Destroy() error }); ok {
		return closer.Destroy()
	}
	return nil
}

// split режет список на части не длиннее size.
func split[T any](items []T, size int) [][]T {
	if size <= 0 {
		return [][]T{items}
	}
	var out [][]T
	for start := 0; start < len(items); start += size {
		out = append(out, items[start:min(start+size, len(items))])
	}
	return out
}

// normalizeVec приводит вектор к единичной длине (нулевой остаётся нулём).
func normalizeVec(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return v
	}
	inv := 1.0 / math.Sqrt(sum)
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(float64(x) * inv)
	}
	return out
}
