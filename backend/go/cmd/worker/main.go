package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/kbsearch/backend-go/internal/app"
	"github.com/kbsearch/backend-go/internal/application/indexing"
	"github.com/kbsearch/backend-go/internal/application/ports"
	"github.com/kbsearch/backend-go/internal/application/settings"
	"github.com/kbsearch/backend-go/internal/config"
	"github.com/kbsearch/backend-go/internal/delivery/worker"
	"github.com/kbsearch/backend-go/internal/domain"
	"github.com/kbsearch/backend-go/internal/infrastructure/postgres"
	"github.com/kbsearch/backend-go/internal/infrastructure/storage"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{}))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("воркер остановлен с ошибкой", "err", err)
		os.Exit(1)
	}
	logger.Info("воркер остановлен")
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	deps, err := app.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer deps.Close()

	clock := ports.SystemClock{}
	jobs := postgres.NewJobRepository(clock)

	// Одни и те же порты на обе стадии: стадия convert и стадия index
	// работают с одной версией документа, и разные экземпляры репозитория
	// означали бы разные снимки состояния одной версии.
	stageDeps := indexing.Deps{
		Documents:  postgres.NewDocumentRepository(clock),
		Jobs:       jobs,
		Settings:   settings.New(postgres.NewSettingsRepository()),
		Embedder:   deps.Embedder,
		Index:      deps.Index,
		Storage:    deps.Storage,
		Queryer:    deps.Pool,
		Transactor: deps.Pool,
		ParsedKey:  storage.ParsedKey,
	}

	loop := worker.New(worker.Deps{
		Jobs:       jobs,
		Queryer:    deps.Pool,
		Transactor: deps.Pool,
		Clock:      clock,
		Logger:     logger,
		Stage:      newStages(indexing.NewConverter(stageDeps), indexing.NewIndexer(stageDeps)),
	}, worker.Options{PollInterval: cfg.WorkerPollInterval})

	logger.Info("обработка очереди запущена", "stages", domain.GoStages())
	loop.Run(ctx)
	return nil
}
