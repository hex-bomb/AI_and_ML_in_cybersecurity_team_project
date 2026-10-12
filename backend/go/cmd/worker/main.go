package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/kbsearch/kb/internal/app"
	"github.com/kbsearch/kb/internal/docs/settings"
	"github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/jobs/indexing"
	"github.com/kbsearch/kb/internal/jobs/worker"
	"github.com/kbsearch/kb/internal/platform/config"
	"github.com/kbsearch/kb/internal/ports"
	"github.com/kbsearch/kb/internal/store/pg"
	"github.com/kbsearch/kb/internal/store/s3"
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

	// Lake — ОДИН экземпляр на оба шва четвёртой двери §8.
	//
	// Раньше адаптер собирался прямо в строке сборки стадии удаления, и это
	// работало только потому, что писатель был один. С появлением читателя
	// (internal/jobs/rebuild, пересборка индекса из lake по §9) один и тот же
	// адаптер оказался бы собран дважды: один писателю надгробий, другой —
	// читателю ленты. Само по себе это не поломка, но оба экземпляра делят ОДИН
	// пул, а lake держит идемпотентность на реестре raw_event_ids внутри
	// Publish (§4). Экземпляры, собранные по отдельности, выглядели бы
	// независимыми и перестали бы этим быть при первом же рефакторинге: второе
	// место, где собирается lake, — это ровно то, о котором потом никто не
	// вспомнит.
	//
	// Тот же экземпляр отдаётся и как EventBus (надгробия удаления), и как
	// EventStore (пересборка индекса), и пересборку запускает отдельная команда
	// cmd/rebuild: пересборка — учебная тревога по §9, то есть операционный
	// прогон человека, а не стадия очереди. Стадией worker.Stage она быть не
	// может: worker.Stage отбирается по имени стадии задания, а пересборка
	// заданием не является.
	lake := postgres.NewLake(deps.Pool)

	// Одни и те же порты на обе стадии: стадия convert и стадия index
	// работают с одной версией документа, и разные экземпляры репозитория
	// означали бы разные снимки состояния одной версии.
	//
	// Lake сюда отдаётся ТОТ ЖЕ экземпляр, что и стадии delete ниже. Раньше у
	// стадии index lake просто не было, и это была не «ещё не сделанная
	// оптимизация», а дыра в источнике правды: стадия писала чанки в индекс и
	// молчала, поэтому пересборка индекса из lake (internal/jobs/rebuild,
	// учебная тревога §9) читала ленту без единого события стадии index и
	// рапортовала об успехе на пустом плане. Второй postgres.NewLake рядом
	// выглядел бы безобидно — оба экземпляра делят один пул и один реестр
	// raw_event_ids, но выглядели бы независимыми, и первая же правка одного
	// из них развела бы идемпотентность lake надвое.
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
		Lake:       lake,
		Clock:      clock,
	}

	// Стадия delete обслуживается здесь же и теми же портами. Надгробие едет в
	// lake через тот же адаптер, которым пользуются остальные писатели, потому
	// что идемпотентность lake держится на реестре raw_event_ids внутри
	// Publish, и второй писатель обошёл бы его.
	deleteDeps := indexing.DeleteDeps{
		Index:      deps.Index,
		Lake:       lake,
		Jobs:       indexing.NewDeleteJobs(clock),
		Transactor: deps.Pool,
		Clock:      clock,
	}

	loop := worker.New(worker.Deps{
		Jobs:       jobs,
		Queryer:    deps.Pool,
		Transactor: deps.Pool,
		Clock:      clock,
		Logger:     logger,
		Stage: newStages(
			indexing.NewConverter(stageDeps),
			indexing.NewIndexer(stageDeps),
			indexing.NewDeleter(deleteDeps),
		),
	}, worker.Options{PollInterval: cfg.WorkerPollInterval})

	logger.Info("обработка очереди запущена", "stages", domain.GoStages())
	loop.Run(ctx)
	return nil
}
