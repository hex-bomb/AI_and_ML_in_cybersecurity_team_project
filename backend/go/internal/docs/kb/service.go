// Package kb implements knowledge-base flows: create, list, read, set ACL.
package kb

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
)

const rfc3339 = time.RFC3339

// View is a knowledge base as served; dates are preformatted strings.
type View struct {
	ID        uuid.UUID
	Name      string
	CreatedAt string
	ACL       domain.ACL
}

type aclReader interface {
	ACL(ctx context.Context, q ports.Queryer, kbID string) (domain.ACL, error)
}

type repo interface {
	ports.KBRepository
	aclReader
}

// Service runs knowledge-base flows.
type Service struct {
	repo    repo
	auth    ports.Authorizer
	q       ports.Queryer
	transac ports.Transactor
}

// New creates the knowledge-base service.
func New(r repo, auth ports.Authorizer, q ports.Queryer, t ports.Transactor) *Service {
	return &Service{repo: r, auth: auth, q: q, transac: t}
}

// CreateKB creates a knowledge base. No settings rows are created here.
func (s *Service) CreateKB(ctx context.Context, p domain.Principal, name string) (uuid.UUID, error) {
	if err := ctx.Err(); err != nil {
		return uuid.Nil, err
	}
	if err := s.auth.RequireAdmin(p); err != nil {
		return uuid.Nil, err
	}
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return uuid.Nil, domain.InvalidMetadata("Имя базы знаний не может быть пустым")
	}

	created, err := s.repo.Create(ctx, s.q, p, domain.CreateKBInput{Name: trimmed})
	if err != nil {
		return uuid.Nil, err
	}
	id, err := uuid.Parse(created.KBID)
	if err != nil {
		return uuid.Nil, internal("Базе знаний присвоен некорректный идентификатор", err)
	}
	return id, nil
}

// ListKBs returns one page and the next cursor. ACLs are not listed per item:
// enumeration is tenant-visible, rights are enforced on content reads.
func (s *Service) ListKBs(
	ctx context.Context, p domain.Principal, limit int, cursor string,
) ([]View, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	items, next, err := s.repo.List(ctx, s.q, p, clampLimit(limit), cursor)
	if err != nil {
		return nil, "", err
	}

	out := make([]View, 0, len(items))
	for _, item := range items {
		id, err := uuid.Parse(item.KBID)
		if err != nil {
			return nil, "", internal("База знаний имеет некорректный идентификатор", err)
		}
		out = append(out, View{
			ID:        id,
			Name:      item.Name,
			CreatedAt: item.CreatedAt.UTC().Format(rfc3339),
		})
	}
	return out, next, nil
}

// GetKB returns one knowledge base with its ACL.
func (s *Service) GetKB(ctx context.Context, p domain.Principal, kbID uuid.UUID) (View, error) {
	if err := ctx.Err(); err != nil {
		return View{}, err
	}
	if kbID == uuid.Nil {
		return View{}, domain.InvalidMetadata("Идентификатор базы знаний обязателен")
	}

	kb, err := s.repo.Require(ctx, s.q, p, kbID.String())
	if err != nil {
		return View{}, err
	}
	acl, err := s.repo.ACL(ctx, s.q, kbID.String())
	if err != nil {
		return View{}, err
	}
	if err := s.auth.RequireRead(p, acl); err != nil {
		return View{}, err
	}
	return View{
		ID:        kbID,
		Name:      kb.Name,
		CreatedAt: kb.CreatedAt.UTC().Format(rfc3339),
		ACL:       acl,
	}, nil
}

// SetKBACL replaces the ACL. Existence is required first so SetACL (which
// locks by id without tenant scope) cannot touch a foreign base.
func (s *Service) SetKBACL(
	ctx context.Context, p domain.Principal, kbID uuid.UUID, allowed domain.AllowedList,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if kbID == uuid.Nil {
		return domain.InvalidMetadata("Идентификатор базы знаний обязателен")
	}
	if err := s.auth.RequireAdmin(p); err != nil {
		return err
	}
	if _, err := s.repo.Require(ctx, s.q, p, kbID.String()); err != nil {
		return err
	}

	acl := domain.ACL{Users: allowed.Users, Groups: allowed.Groups}
	return s.transac.WithTx(ctx, func(tx ports.Queryer) error {
		return s.repo.SetACL(ctx, tx, kbID.String(), acl)
	})
}

const (
	defaultLimit = 20
	maxLimit     = 100
)

func clampLimit(limit int) int {
	if limit <= 0 {
		return defaultLimit
	}
	if limit > maxLimit {
		return maxLimit
	}
	return limit
}

// internal wraps an unexpected adapter failure via the domain constructor.
func internal(message string, cause error) error {
	return domain.Internal(message).WithCause(cause)
}
