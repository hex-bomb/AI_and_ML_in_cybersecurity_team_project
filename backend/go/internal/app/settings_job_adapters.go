package app

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	httpapi "github.com/kbsearch/kb/internal/api/http"
	"github.com/kbsearch/kb/internal/docs/settings"
	domain "github.com/kbsearch/kb/internal/envelope"
	jobs "github.com/kbsearch/kb/internal/jobs/queue"
	"github.com/kbsearch/kb/internal/ports"
	postgres "github.com/kbsearch/kb/internal/store/pg"
)

// settingsAdapter переводит DTO транспорта в вызов диагностики настроек.
//
// Чужая база даёт 404 через kbs.Require — та же проверка, что стоит первой
// в поиске и в documents.requireAdmin: без неё опечатка в kb_id неотличима
// от «база пустая», а чужой tenant зондируется на существование.
type settingsAdapter struct {
	svc     *settings.Service
	kbs     ports.KBRepository
	queryer ports.Queryer
}

func (a settingsAdapter) Diagnose(
	ctx context.Context, p domain.Principal, kbID uuid.UUID,
) (httpapi.Diagnostics, error) {
	// Профиль фиксируется при первой активации версии; search и answers
	// читают его из той же строки (kbs.Require → profileFor), поэтому
	// диагностика сравнивает настройки именно с ним. Пустой профиль —
	// база ещё не индексировалась, проверка совместимости пропускается.
	kb, err := a.kbs.Require(ctx, a.queryer, p, kbID.String())
	if err != nil {
		return httpapi.Diagnostics{}, err
	}
	out, err := a.svc.Diagnostics(
		ctx, a.queryer, p.TenantID.String(), kbID.String(), kb.EmbeddingProfileID)
	if err != nil {
		return httpapi.Diagnostics{}, err
	}
	return diagnosticsView(out), nil
}

// diagnosticsView переводит состояние настроек в форму ответа. Значения
// ключей наружу не уходят (только наличие и уровень), scope — уровень,
// на котором ключ задан, source — откуда взято значение.
func diagnosticsView(d settings.Diagnostics) httpapi.Diagnostics {
	missing := d.MissingKeys
	if missing == nil {
		missing = []string{}
	}
	keys := make([]httpapi.KeyStatus, 0, len(d.Sources))
	for _, s := range d.Sources {
		keys = append(keys, httpapi.KeyStatus{
			Key:     s.Key.Name(),
			Scope:   string(s.Key.Level()),
			IsSet:   s.HasValue,
			Sources: string(s.Source),
		})
	}
	return httpapi.Diagnostics{Ready: d.Valid, Missing: missing, Keys: keys}
}

// jobRepoAdapter подводит postgres.JobRepository под jobs.Repository.
// Чтение задания идёт по первичному ключу без фильтра tenant, поэтому
// область видимости проверяет код, а не SQL: сначала здесь, затем повторно
// в jobs.Service.Get. Чужое задание неотличимо от отсутствующего, иначе
// перебором id раскрываются чужие очереди. Логики доступа, кроме сверки
// tenant, здесь нет.
type jobRepoAdapter struct{ repo *postgres.JobRepository }

func (a jobRepoAdapter) GetJob(
	ctx context.Context, q ports.Queryer, tenantID, jobID string,
) (jobs.Record, error) {
	if err := ctx.Err(); err != nil {
		return jobs.Record{}, err
	}
	stored, err := a.repo.Get(ctx, q, jobID)
	if err != nil {
		return jobs.Record{}, err
	}
	if stored.TenantID.String() != tenantID {
		return jobs.Record{}, domain.NotFound("Задание не найдено")
	}
	// Метки времени чтение задания не возит (пакет infrastructure вне зоны
	// правок), добираем одним чтением по первичному ключу: без updated_at
	// ответ GET /jobs врал бы «0001-01-01».
	var createdAt, updatedAt time.Time
	err = q.QueryRow(ctx,
		"SELECT created_at, updated_at FROM jobs WHERE id = $1", stored.ID,
	).Scan(&createdAt, &updatedAt)
	if err != nil {
		// Строка исчезла между двумя чтениями (уборка удалённого
		// документа) — это тоже «нет задания», а не 500.
		if errors.Is(err, pgx.ErrNoRows) {
			return jobs.Record{}, domain.NotFound("Задание не найдено")
		}
		return jobs.Record{}, err
	}
	return jobs.Record{
		ID: stored.ID, TenantID: stored.TenantID, DocumentID: stored.DocumentID,
		VersionID: stored.VersionID, Kind: stored.Kind, Stage: stored.Stage,
		Status: stored.Status, Attempt: stored.Attempt,
		ProcessedChunks: stored.ProcessedCount, TotalChunks: stored.TotalChunks,
		ErrorCode: stored.ErrorCode, ErrorMessage: stored.ErrorMessage,
		CreatedAt: createdAt.UTC(), UpdatedAt: updatedAt.UTC(),
	}, nil
}

// jobAdapter переводит DTO транспорта в вызов чтения задания. Пишущих
// операций нет, поэтому нужен только queryer (d.Pool, тот же q, что
// в documents): транзактор остался в сценарии документов.
type jobAdapter struct {
	svc     *jobs.Service
	queryer ports.Queryer
}

func (a jobAdapter) Get(
	ctx context.Context, p domain.Principal, id uuid.UUID,
) (httpapi.JobView, error) {
	v, err := a.svc.Get(ctx, a.queryer, p, id)
	if err != nil {
		return httpapi.JobView{}, err
	}
	return jobView(v), nil
}

// jobView переводит представление сценария в представление ответа.
func jobView(v jobs.View) httpapi.JobView {
	return httpapi.JobView{
		ID: v.ID, DocumentID: v.DocumentID, VersionID: v.VersionID,
		Kind: v.Kind, Stage: v.Stage, Status: v.Status,
		Attempts: v.Attempts, Chunks: v.Chunks,
		ErrorCode: v.ErrorCode, ErrorText: v.ErrorText,
		CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt,
	}
}
