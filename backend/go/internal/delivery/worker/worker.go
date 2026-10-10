// Package worker обслуживает очередь заданий под lease (ТЗ 3.6).
// Стадия индексации приходит интерфейсом Stage — цикл проверяется без ES и модели.
package worker

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/kbsearch/backend-go/internal/application/ports"
	"github.com/kbsearch/backend-go/internal/domain"
)

// Stage — обработчик стадии задания (реализация в application, чтобы сценарий не знал о lease).
type Stage interface {
	// Convert выполняет стадию convert: оригинал версии превращается в
	// ParsedDocument, а задание переводится на index.
	Convert(ctx context.Context, lease domain.JobLease) error
	// Index выполняет стадию index (постоянные коды не повторяются, ТЗ 2.4).
	Index(ctx context.Context, lease domain.JobLease) error
}

// Deps — порты цикла.
type Deps struct {
	Jobs       ports.JobRepository
	Queryer    ports.Queryer
	Transactor ports.Transactor
	Stage      Stage
	Clock      ports.Clock
	Logger     *slog.Logger
}

// Options — параметры цикла.
type Options struct {
	// WorkerID пишется в jobs.worker_id для диагностики очереди.
	WorkerID string
	// Stages забирает Claim (пусто — domain.GoStages()).
	Stages []string
	// PollInterval — пауза на пустой очереди.
	PollInterval time.Duration
	// HeartbeatInterval — период продления lease.
	HeartbeatInterval time.Duration
}

// Worker — цикл обработки очереди.
type Worker struct {
	deps   Deps
	opts   Options
	log    *slog.Logger
	stages []string
}

// New собирает цикл (сроки lease/heartbeat — из домена, они связаны).
func New(deps Deps, opts Options) *Worker {
	if opts.Stages == nil {
		opts.Stages = domain.GoStages()
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = DefaultPollInterval
	}
	if opts.HeartbeatInterval <= 0 {
		opts.HeartbeatInterval = domain.HeartbeatInterval
	}
	if opts.WorkerID == "" {
		opts.WorkerID = defaultWorkerID()
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{deps: deps, opts: opts, log: logger, stages: opts.Stages}
}

// DefaultPollInterval — пауза на пустой очереди (очередь общая, частый опрос мешает).
const DefaultPollInterval = 2 * time.Second

// failTimeout — предел записи ошибки (иначе SIGTERM переживёт оркестратор).
const failTimeout = 10 * time.Second

// Run крутит очередь до отмены контекста (выход — штатная остановка без ошибки).
func (w *Worker) Run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}

		lease, err := w.deps.Jobs.Claim(ctx, w.deps.Queryer, w.opts.WorkerID, w.stages)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			// Сбой Claim не роняет воркер.
			w.log.Error("не удалось взять задание", "err", err)
		case lease == nil:
			// Очередь пуста — не сбой (ТЗ 3.6).
		default:
			w.process(ctx, *lease)
			continue
		}

		if err := w.deps.Clock.Sleep(ctx, w.opts.PollInterval); err != nil {
			return
		}
	}
}

// process выполняет одно задание (отмена — перехват lease или остановка).
func (w *Worker) process(ctx context.Context, lease domain.JobLease) {
	jobCtx, cancelJob := context.WithCancel(ctx)
	defer cancelJob()

	beat := w.startHeartbeat(jobCtx, cancelJob, lease)
	err := w.stage(jobCtx, lease)
	beat.stop()

	if err == nil {
		w.log.Info("задание обработано", "job", lease.JobID, "stage", lease.Stage)
		return
	}
	if beat.lost() || errors.Is(err, domain.ErrJobLeaseLost) {
		// Lease чужой — ошибку не пишем (ТЗ 7.7).
		w.log.Warn("lease задания утрачен, работа прекращена", "job", lease.JobID)
		return
	}
	if ctx.Err() != nil {
		// Остановка: задание вернётся само через истёкший lease (ТЗ 4.2).
		w.log.Info("задание прервано остановкой, lease истечёт и вернёт его в очередь",
			"job", lease.JobID)
		return
	}
	w.fail(jobCtx, lease, err)
}

// stage выбирает обработчик по стадии (чужая стадия — ошибка границы, не молчаливый index).
func (w *Worker) stage(ctx context.Context, lease domain.JobLease) error {
	switch lease.Stage {
	case domain.StageConvert:
		return w.deps.Stage.Convert(ctx, lease)
	case domain.StageIndex:
		return w.deps.Stage.Index(ctx, lease)
	default:
		return domain.ConfigurationInvalid(
			"Стадия задания не обслуживается этим воркером",
			map[string]any{"stage": lease.Stage, "stages": w.stages},
		)
	}
}

// fail записывает ошибку этапа (триплет — из AppError, решение о повторе — у репозитория).
func (w *Worker) fail(ctx context.Context, lease domain.JobLease, cause error) {
	code, message, retryable := describe(cause)

	// Предел обязателен: зависшее соединение пережило бы SIGTERM.
	writeCtx, cancel := context.WithTimeout(ctx, failTimeout)
	defer cancel()

	jobID, token := lease.JobID.String(), lease.LeaseToken
	err := w.deps.Transactor.WithTx(writeCtx, func(tx ports.Queryer) error {
		return w.deps.Jobs.Fail(writeCtx, tx, jobID, token, code, message, retryable)
	})
	if err != nil {
		w.log.Error("не удалось записать ошибку задания", "job", lease.JobID, "err", err)
		return
	}
	w.log.Info("ошибка задания записана", "job", lease.JobID, "code", code, "retryable", retryable)
}

// describe переводит ошибку этапа в тройку для очереди (без драйверных деталей).
func describe(err error) (code, message string, retryable bool) {
	var appErr *domain.AppError
	if !errors.As(err, &appErr) {
		return domain.CodeInternalError, "Внутренняя ошибка этапа индексации", true
	}
	return appErr.Code, appErr.MessageOrDefault(), appErr.Retryable()
}

// defaultWorkerID — хост + случайная часть (перезапуски различимы в журнале).
func defaultWorkerID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "worker"
	}
	return host + "/" + uuid.NewString()[:8]
}
