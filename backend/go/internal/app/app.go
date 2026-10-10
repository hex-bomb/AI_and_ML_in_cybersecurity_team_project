// Package app — composition root: сборка зависимостей для api, worker и тестов.
package app

import (
	"context"
	"fmt"
	"net/http"

	"github.com/kbsearch/backend-go/internal/application/answers"
	"github.com/kbsearch/backend-go/internal/application/auth"
	"github.com/kbsearch/backend-go/internal/application/documents"
	"github.com/kbsearch/backend-go/internal/application/jobs"
	"github.com/kbsearch/backend-go/internal/application/kb"
	"github.com/kbsearch/backend-go/internal/application/ports"
	"github.com/kbsearch/backend-go/internal/application/search"
	"github.com/kbsearch/backend-go/internal/application/settings"
	"github.com/kbsearch/backend-go/internal/config"
	"github.com/kbsearch/backend-go/internal/delivery/httpapi"
	"github.com/kbsearch/backend-go/internal/infrastructure/elasticsearch"
	"github.com/kbsearch/backend-go/internal/infrastructure/embeddings"
	"github.com/kbsearch/backend-go/internal/infrastructure/ollama"
	"github.com/kbsearch/backend-go/internal/infrastructure/postgres"
	"github.com/kbsearch/backend-go/internal/infrastructure/storage"
)

// Deps — собранные зависимости. Close освобождает модель и пул.
type Deps struct {
	Config   config.Config
	Pool     *postgres.DB
	Storage  ports.FileStorage
	Embedder *embeddings.Embedder
	Index    *elasticsearch.Index
	Close    func()
	Auth     httpapi.AuthScenario
	Health   httpapi.HealthScenario
	Search   httpapi.SearchScenario
	Answers  httpapi.AnswerScenario
	KB       httpapi.KBScenario
	Document httpapi.DocumentScenario
	Settings httpapi.SettingsScenario
	Job      httpapi.JobScenario
}

// New собирает зависимости. Непостроенное остаётся nil — транспорт отвечает 503.
func New(ctx context.Context, cfg config.Config) (*Deps, error) {
	pool, err := postgres.New(ctx, postgres.Config{URL: cfg.DatabaseURL})
	if err != nil {
		return nil, fmt.Errorf("подключение к PostgreSQL: %w", err)
	}

	fileStorage, err := storage.New(cfg.FileStorageRoot)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("хранилище файлов: %w", err)
	}

	// Ленивая инициализация: /health/live отвечает до скачивания модели.
	embedder := embeddings.New(embeddings.Config{
		Model:       cfg.EmbeddingModel,
		Dimension:   cfg.EmbeddingDimension,
		CacheDir:    cfg.EmbeddingModelCache,
		BatchSize:   cfg.EmbeddingBatchSize,
		Concurrency: cfg.EmbeddingConcurrency,
	})

	index, err := elasticsearch.New(elasticsearch.Config{
		Addresses: []string{cfg.ElasticsearchURL},
		Username:  cfg.ElasticsearchUser,
		Password:  cfg.ElasticsearchPassword,
	})
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("хранилище поиска: %w", err)
	}

	d := &Deps{
		Config:   cfg,
		Pool:     pool,
		Storage:  fileStorage,
		Embedder: embedder,
		Index:    index,
	}
	d.Auth = newLocalAuth(cfg)
	d.Health = newHealth(d)
	d.wire(ctx)
	d.Close = func() {
		_ = embedder.Close()
		_ = index.Close(context.Background())
		pool.Close()
	}
	return d, nil
}

// wire связывает сценарии с портами. Неподключённое отвечает 503, а не 500.
func (d *Deps) wire(_ context.Context) {
	clock := ports.SystemClock{}
	kbs := postgres.NewKBRepository()
	docs := postgres.NewDocumentRepository(clock)
	settingsSvc := settings.New(postgres.NewSettingsRepository())
	authorizer := auth.NewLocal(d.tenantID())

	d.KB = kbAdapter{svc: kb.New(kbs, authorizer, d.Pool, d.Pool)}

	searchSvc := search.New(d.Pool, kbs, docs, d.Index, d.Embedder, settingsSvc)
	d.Search = searchAdapter{svc: searchSvc}

	llm := ollama.New(ollama.Config{
		BaseURL:     d.Config.LLMBaseURL,
		Temperature: d.Config.LLMTemperature,
	})
	d.Answers = answerAdapter{svc: answers.New(d.Pool, searchSvc, llm, docs, settingsSvc)}

	jobQueue := postgres.NewJobRepository(clock)
	documentSvc := documents.New(
		postgres.NewDocumentsRepository(),
		kbs,
		jobQueue,
		postgres.NewIdempotencyStoreAdapter(
			postgres.NewIdempotencyRepository(clock, postgres.IdempotencyTTL)),
		d.Storage,
		authorizer,
		d.Pool,
		d.Pool,
		clock,
		documents.OriginalKey,
	)
	d.Document = documentAdapter{svc: documentSvc}

	// Диагностика и задания: без проводки транспорт отвечал 503.
	d.Settings = settingsAdapter{svc: settingsSvc, kbs: kbs, queryer: d.Pool}
	d.Job = jobAdapter{svc: jobs.New(jobRepoAdapter{repo: jobQueue}), queryer: d.Pool}
}

// Handler строит HTTP-обработчик.
func (d *Deps) Handler() http.Handler {
	return httpapi.Handler(d.Config, httpapi.Services{
		Search:   d.Search,
		Answers:  d.Answers,
		KB:       d.KB,
		Document: d.Document,
		Settings: d.Settings,
		Job:      d.Job,
		Health:   d.Health,
		Auth:     d.Auth,
	})
}
