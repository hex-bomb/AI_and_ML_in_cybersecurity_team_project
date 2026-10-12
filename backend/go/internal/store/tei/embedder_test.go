package embeddings

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"

	domain "github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
)

// fakeSession кодирует текст детерминированно, без ONNX.
// Код вектора зависит от текста так, чтобы похожие тексты давали похожие
type fakeSession struct {
	dim int
	// calls считает обращения к пайплайну: по нему видно, что пустой
	// allowed-список не должен долетать до вычислений.
	calls int
	// err подставляется для проверки обработки сбоя.
	err error
	// seen копит тексты, дошедшие до пайплайна: по ним видно префикс роли,
	// который иначе нигде не проявляется снаружи.
	seen []string
	mu   sync.Mutex
}

func (f *fakeSession) RunPipeline(ctx context.Context, texts []string) (*Result, error) {
	f.mu.Lock()
	f.calls++
	f.seen = append(f.seen, texts...)
	f.mu.Unlock()
	if ctx.Err() != nil {
		return nil, context.Canceled
	}
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for i, text := range texts {
		out[i] = fakeVector(text, f.dim)
	}
	return &Result{Embeddings: out}, nil
}

// lastSeen отдаёт последний ушедший в пайплайн текст.
func (f *fakeSession) lastSeen() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.seen) == 0 {
		return ""
	}
	return f.seen[len(f.seen)-1]
}

func (f *fakeSession) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func fakeVector(text string, dim int) []float32 {
	v := make([]float32, dim)
	sum := 0
	for _, r := range text {
		sum += int(r)
	}
	for i := range v {
		// Плавная функция от суммы символов: похожие тексты сближаются,
		// несвязанные расходятся.
		v[i] = float32(math.Sin(float64(sum+i) * 0.01))
	}
	return v
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func norm(v []float32) float64 {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	return math.Sqrt(s)
}

// withFake подменяет создание сессии на время теста.
func withFake(t *testing.T, f *fakeSession) {
	t.Helper()
	prev := newSessionFunc
	newSessionFunc = func(context.Context, Config) (Session, error) { return f, nil }
	t.Cleanup(func() { newSessionFunc = prev })
}

// TestPipelineContextDetachedFromCancel — отмена входящего контекста не должна
// доходить до пайплайна: бэкенд ломает разделяемую сессию, если контекст
// отменили даже после возврата, и чинился бы только рестартом процесса.
func TestPipelineContextDetachedFromCancel(t *testing.T) {
	f := &fakeSession{dim: 8}
	withFake(t, f)
	e := New(Config{Dimension: 8, BatchSize: 2, Concurrency: 1})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Embed(ctx, []string{"раз", "два"}, testConfig(8)); err != nil {
		t.Fatalf("Embed с отменённым контекстом: %v", err)
	}
}

// testConfig собирает профиль под размерность эмбеддера под тестом: в бою
// это 384 измерения E5, в юнит-тестах — 8, и расхождение должно падать, а не
func testConfig(dim int) domain.IndexingConfig {
	return domain.IndexingConfig{
		EmbeddingProviderID: "local-hugot",
		EmbeddingModel:      "intfloat/multilingual-e5-small",
		EmbeddingRevision:   "pilot-1",
		EmbeddingDimension:  dim,
		ChunkMaxTokens:      600,
		ChunkOverlapTokens:  100,
		TokenizerID:         "hugot/multilingual-e5-small",
		NormalizeVectors:    true,
		Distance:            "cosine",
	}
}

// TestRolePrefixesAreApplied — главное свойство E5. Без префиксов вектор
// считается, но вектор запроса и вектор документа оказываются в разных
func TestRolePrefixesAreApplied(t *testing.T) {
	const text = "Момент затяжки защитной муфты"
	cases := []struct {
		role Role
		want string
	}{
		{RoleDocument, "passage: " + text},
		{RoleQuery, "query: " + text},
	}
	for _, tc := range cases {
		if got := tc.role.prefix() + text; got != tc.want {
			t.Errorf("prefix(%v) дал %q, ожидалось %q", tc.role, got, tc.want)
		}
	}
}

// TestQueryAndDocumentOfSameTextDiffer доказывает, что роль влияет на
// результат, а не существует только для красоты.
func TestQueryAndDocumentOfSameTextDiffer(t *testing.T) {
	f := &fakeSession{dim: 8}
	withFake(t, f)
	e := New(Config{Dimension: 8, BatchSize: 4, Concurrency: 1})
	ctx := context.Background()

	doc, err := e.EmbedRole(ctx, []string{"муфта"}, RoleDocument, testConfig(8))
	if err != nil {
		t.Fatalf("EmbedRole(document): %v", err)
	}
	q, err := e.EmbedRole(ctx, []string{"муфта"}, RoleQuery, testConfig(8))
	if err != nil {
		t.Fatalf("EmbedRole(query): %v", err)
	}
	if cosine(doc[0], q[0]) == 1 {
		t.Error("вектор запроса совпал с вектором документа: префикс не применился")
	}
}

// TestEmbedQueryUsesQueryPrefix — EmbedQuery кодирует как запрос (проверяется
// текст, ушедший в сессию: сравнение векторов молча прошло бы и с неверным префиксом).
func TestEmbedQueryUsesQueryPrefix(t *testing.T) {
	f := &fakeSession{dim: 8}
	withFake(t, f)
	e := New(Config{Dimension: 8, BatchSize: 4, Concurrency: 1})

	if _, err := e.EmbedQuery(context.Background(), "момент затяжки", testConfig(8)); err != nil {
		t.Fatalf("EmbedQuery: %v", err)
	}
	if len(f.seen) == 0 {
		t.Fatal("сессия не получила текст")
	}
	last := f.lastSeen()
	if !strings.HasPrefix(last, "query: ") {
		t.Errorf("вопрос ушёл без префикса query: %q", last)
	}
}

func TestEmbedMatchesPortSignature(t *testing.T) {
	f := &fakeSession{dim: 8}
	withFake(t, f)
	e := New(Config{Dimension: 8, BatchSize: 4, Concurrency: 1})

	viaPort, err := e.Embed(context.Background(), []string{"муфта"}, testConfig(8))
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	viaRole, err := e.EmbedRole(context.Background(), []string{"муфта"}, RoleDocument, testConfig(8))
	if err != nil {
		t.Fatalf("EmbedRole: %v", err)
	}
	if c := cosine(viaPort[0], viaRole[0]); math.Abs(c-1) > 1e-9 {
		t.Errorf("Embed и EmbedRole(RoleDocument) разошлись, косинус %v", c)
	}
}

// TestBatchingPreservesOrderAndResult — батчинг не должен переставлять и
// не должен менять сами векторы.
func TestBatchingPreservesOrderAndResult(t *testing.T) {
	const dim = 8
	f := &fakeSession{dim: dim}
	withFake(t, f)
	ctx := context.Background()

	texts := make([]string, 10)
	for i := range texts {
		texts[i] = fmt.Sprintf("чанк номер %d про разное", i)
	}

	// Разные размеры батча: 1, 3, 100. При 100 весь набор уходит одним
	// вызовом, и это тоже должно давать тот же результат.
	for _, size := range []int{1, 3, 100} {
		e := New(Config{Dimension: dim, BatchSize: size, Concurrency: 2})
		got, err := e.Embed(ctx, texts, testConfig(dim))
		if err != nil {
			t.Fatalf("размер батча %d: %v", size, err)
		}
		if len(got) != len(texts) {
			t.Fatalf("размер батча %d: получено %d векторов, ожидалось %d", size, len(got), len(texts))
		}
		want := fakeVector("passage: "+texts[4], dim)
		// Допуск вместо точного равенства: вектор проходит через
		// нормализацию, и после деления на длину косинус отличается от 1
		if c := cosine(got[4], want); math.Abs(c-1) > 1e-9 {
			t.Errorf("размер батча %d: вектор[%d] не соответствует своему тексту, косинус %v", size, 4, c)
		}
	}
}

// TestBatchingSplitsByConfiguredSize — проверка самого разбиения.
func TestBatchingSplitsByConfiguredSize(t *testing.T) {
	cases := []struct {
		in       int
		size     int
		wantB    int
		wantLast int
	}{
		{in: 0, size: 3, wantB: 0, wantLast: 0},
		{in: 7, size: 3, wantB: 3, wantLast: 1},
		{in: 6, size: 3, wantB: 2, wantLast: 3},
		{in: 5, size: 32, wantB: 1, wantLast: 5},
	}
	for _, tc := range cases {
		items := make([]int, tc.in)
		got := split(items, tc.size)
		if tc.in == 0 {
			if len(got) != 0 {
				t.Errorf("split(%d, %d) = %d батчей, ожидалось 0", tc.in, tc.size, len(got))
			}
			continue
		}
		if len(got) != tc.wantB {
			t.Errorf("split(%d, %d): %d батчей, ожидалось %d", tc.in, tc.size, len(got), tc.wantB)
		}
		if len(got[len(got)-1]) != tc.wantLast {
			t.Errorf("split(%d, %d): последний батч %d, ожидалось %d",
				tc.in, tc.size, len(got[len(got)-1]), tc.wantLast)
		}
	}
}

// TestNormalizationProducesUnitLength — при NormalizeVectors вектор должен
// давать единичную длину, иначе косинус в Elasticsearch считается от
func TestNormalizationProducesUnitLength(t *testing.T) {
	f := &fakeSession{dim: 16}
	withFake(t, f)
	e := New(Config{Dimension: 16, BatchSize: 4, Concurrency: 1})

	cfg := testConfig(16)
	cfg.NormalizeVectors = true
	got, err := e.Embed(context.Background(), []string{"нормируем"}, cfg)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if n := norm(got[0]); math.Abs(n-1) > 1e-5 {
		t.Errorf("длина нормализованного вектора = %v, ожидалось 1", n)
	}
}

// TestZeroVectorSurvivesNormalization — нулевой вектор делить на ноль
// нельзя: получится NaN, который тихо портит сходство всех соседей.
func TestZeroVectorSurvivesNormalization(t *testing.T) {
	got := normalizeVec([]float32{0, 0, 0})
	for i, v := range got {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			t.Fatalf("normalizeVec вернул %v на месте %d", v, i)
		}
	}
	if norm(got) != 0 {
		t.Errorf("нулевой вектор изменился: %v", got)
	}
}

// TestDimensionMismatchRejectedBeforeModelLoad — несовпадение размерности
// ловится без обращения к модели. Иначе ради заведомо неверного ответа
func TestDimensionMismatchRejectedBeforeModelLoad(t *testing.T) {
	f := &fakeSession{dim: 8}
	withFake(t, f)
	e := New(Config{Dimension: 384})

	cfg := testConfig(8)
	cfg.EmbeddingDimension = 768
	_, err := e.Embed(context.Background(), []string{"текст"}, cfg)
	if err == nil {
		t.Fatal("несовпадение размерности не отвергнуто")
	}
	appErr, ok := domain.AsAppError(err)
	if !ok || appErr.Code != domain.CodeEmbeddingDimensionMismatch {
		t.Fatalf("код ошибки: %v, ожидался EMBEDDING_DIMENSION_MISMATCH", err)
	}
	if appErr.Details["want"] != 384 || appErr.Details["got"] != 768 {
		t.Errorf("details = %v, ожидалось actual=768 expected=384", appErr.Details)
	}
	if f.callCount() != 0 {
		t.Error("модель вызывалась, хотя размерность уже не совпала")
	}
}

// TestWrongVectorLengthIsProfileError — если модель всё же вернёт вектор
// не той длины, это ошибка профиля, а не молчаливая запись в индекс.
func TestWrongVectorLengthIsProfileError(t *testing.T) {
	f := &fakeSession{dim: 7} // конфиг обещает 8
	withFake(t, f)
	e := New(Config{Dimension: 8, BatchSize: 4, Concurrency: 1})

	_, err := e.Embed(context.Background(), []string{"текст"}, testConfig(8))
	if err == nil {
		t.Fatal("вектор неверной длины принят")
	}
	if !strings.Contains(err.Error(), "размерност") {
		t.Errorf("ошибка не о размерности: %v", err)
	}
}

// TestEmptyInputDoesNotTouchModel — пустой вход не должен поднимать модель.
func TestEmptyInputDoesNotTouchModel(t *testing.T) {
	f := &fakeSession{dim: 8}
	withFake(t, f)
	e := New(Config{Dimension: 8})

	got, err := e.Embed(context.Background(), nil, testConfig(8))
	if err != nil {
		t.Fatalf("Embed(nil): %v", err)
	}
	if got != nil {
		t.Errorf("Embed(nil) = %v, ожидалось nil", got)
	}
	if f.callCount() != 0 {
		t.Error("модель вызвана на пустом входе")
	}
}

// TestModelLoadedLazilyAndOnce — Ping до первого Embed поднимает модель, а
// параллельные обращения не запускают вторую загрузку.
func TestModelLoadedLazilyAndOnce(t *testing.T) {
	f := &fakeSession{dim: 8}
	withFake(t, f)
	e := New(Config{Dimension: 8, BatchSize: 2, Concurrency: 2})

	if f.callCount() != 0 {
		t.Fatal("модель вызвана до первого обращения")
	}
	if err := e.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := e.Ping(context.Background()); err != nil {
		t.Fatalf("повторный Ping: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.Embed(context.Background(), []string{"текст"}, testConfig(8)); err != nil {
				t.Errorf("параллельный Embed: %v", err)
			}
		}()
	}
	wg.Wait()
}

// TestLoadFailureIsRetried — упавшая загрузка перепробуется следующим
// вызовом: иначе временный сбой (модель докачивается соседним процессом)
// отравлял бы процесс навсегда.
func TestLoadFailureIsRetried(t *testing.T) {
	attempts := 0
	prev := newSessionFunc
	newSessionFunc = func(context.Context, Config) (Session, error) {
		attempts++
		return nil, fmt.Errorf("нет сети")
	}
	t.Cleanup(func() { newSessionFunc = prev })

	e := New(Config{Dimension: 8})
	for i := 0; i < 3; i++ {
		if err := e.Ping(context.Background()); err == nil {
			t.Fatal("Ping без модели вернул успех")
		}
	}
	if attempts != 3 {
		t.Errorf("попыток загрузки %d, ожидалось 3: ошибка не перепробуется", attempts)
	}
}

// TestPipelineErrorBecomesEmbeddingResponseInvalid — сбой модели не должен
// выглядеть как «индекс недоступен»: чинить нужно эмбеддер.
func TestPipelineErrorBecomesEmbeddingResponseInvalid(t *testing.T) {
	f := &fakeSession{dim: 8, err: fmt.Errorf("ONNX упал")}
	withFake(t, f)
	e := New(Config{Dimension: 8, BatchSize: 2, Concurrency: 1})

	_, err := e.Embed(context.Background(), []string{"текст"}, testConfig(8))
	if err == nil {
		t.Fatal("сбой пайплайна не превращён в ошибку")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "эмбеддинг") {
		t.Errorf("ошибка не про эмбеддинги: %v", err)
	}
}

// TestCountMismatchRejected — модель, вернувшая не столько векторов, сколько
// было текстов, ломает соответствие индексов. Ловим сразу.
func TestCountMismatchRejected(t *testing.T) {
	prev := newSessionFunc
	newSessionFunc = func(context.Context, Config) (Session, error) {
		return &truncatingSession{}, nil
	}
	t.Cleanup(func() { newSessionFunc = prev })

	e := New(Config{Dimension: 4, BatchSize: 4, Concurrency: 1})
	if _, err := e.Embed(context.Background(), []string{"а", "б", "в"}, testConfig(4)); err == nil {
		t.Fatal("несовпадение числа векторов принято")
	}
}

type truncatingSession struct{}

func (truncatingSession) RunPipeline(context.Context, []string) (*Result, error) {
	return &Result{Embeddings: [][]float32{{1, 2, 3, 4}}}, nil
}

// TestCloseIsIdempotent — закрытие без загрузки и повторное закрытие
// не должны паниковать: процесс останавливается в любой момент.
func TestCloseIsIdempotent(t *testing.T) {
	f := &fakeSession{dim: 8}
	withFake(t, f)
	e := New(Config{Dimension: 8})
	if err := e.Close(); err != nil {
		t.Errorf("Close до загрузки: %v", err)
	}
	if err := e.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Errorf("повторный Close: %v", err)
	}
}

// TestDefaultsApplied — неполный конфиг из окружения не должен приводить к
// делению на ноль.
func TestDefaultsApplied(t *testing.T) {
	got := Config{}.withDefaults()
	if got.BatchSize != 32 {
		t.Errorf("BatchSize = %d, ожидалось 32", got.BatchSize)
	}
	if got.Concurrency != 2 {
		t.Errorf("Concurrency = %d, ожидалось 2", got.Concurrency)
	}
	if got := (Config{BatchSize: -1}).withDefaults(); got.BatchSize != 32 {
		t.Errorf("отрицательный BatchSize не заменён: %d", got.BatchSize)
	}
}

// TestEmbedderSatisfiesPort — компиляционная гарантия, что адаптер
// реализует объявленный порт целиком.
func TestEmbedderSatisfiesPort(t *testing.T) {
	var _ ports.Embedder = New(Config{})
}
