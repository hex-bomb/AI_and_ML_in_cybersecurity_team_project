// Package jobs reads a single queue job with tenant isolation.
package jobs

import (
	"context"
	"time"

	"github.com/google/uuid"

	domain "github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
)

// Record mirrors the jobs table row, not the HTTP contract.
type Record struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	DocumentID uuid.UUID
	VersionID  *uuid.UUID
	Kind       string
	Stage      string
	Status     string
	Attempt    int
	// TotalChunks stays nil until the chunk count is known.
	ProcessedChunks int
	TotalChunks     *int
	ErrorCode       *string
	ErrorMessage    *string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Repository reads a job scoped by tenant.
type Repository interface {
	GetJob(ctx context.Context, q ports.Queryer, tenantID, jobID string) (Record, error)
}

// View is the job as served to the API.
type View struct {
	ID         uuid.UUID
	DocumentID string
	VersionID  string
	Kind       string
	Stage      string
	Status     string
	Attempts   int
	Chunks     int
	ErrorCode  string
	ErrorText  string
	CreatedAt  string
	UpdatedAt  string
}

// Service reads jobs.
type Service struct {
	repo Repository
}

// New creates the flow.
func New(repo Repository) *Service { return &Service{repo: repo} }

// Get returns a job. A foreign tenant's job reads as 404, same as missing,
// so ids cannot be probed for existence.
func (s *Service) Get(
	ctx context.Context, q ports.Queryer, p domain.Principal, id uuid.UUID,
) (View, error) {
	if err := ctx.Err(); err != nil {
		return View{}, err
	}
	if id == uuid.Nil {
		return View{}, domain.NotFound("Задание не найдено")
	}

	record, err := s.repo.GetJob(ctx, q, p.TenantID.String(), id.String())
	if err != nil {
		return View{}, err
	}
	// Defense in depth: never trust the adapter's tenant filter alone.
	if record.TenantID != p.TenantID {
		return View{}, domain.NotFound("Задание не найдено")
	}

	return view(record), nil
}

func view(record Record) View {
	out := View{
		ID:         record.ID,
		DocumentID: record.DocumentID.String(),
		Kind:       record.Kind,
		Stage:      record.Stage,
		Status:     record.Status,
		Attempts:   record.Attempt,
		Chunks:     record.ProcessedChunks,
		CreatedAt:  record.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:  record.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if record.VersionID != nil {
		out.VersionID = record.VersionID.String()
	}
	if record.ErrorCode != nil {
		out.ErrorCode = *record.ErrorCode
	}
	if record.ErrorMessage != nil {
		out.ErrorText = *record.ErrorMessage
	}
	return out
}
