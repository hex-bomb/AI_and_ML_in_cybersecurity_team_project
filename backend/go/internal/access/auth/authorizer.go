// Package auth implements ports.Authorizer for the single-tenant local build.
package auth

import (
	"github.com/google/uuid"

	domain "github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
)

// Local authorizes the single local tenant; roles do not exist in this build.
type Local struct {
	tenantID uuid.UUID
}

// NewLocal creates local-build authorization.
func NewLocal(tenantID uuid.UUID) *Local { return &Local{tenantID: tenantID} }

// RequireAdmin passes the local tenant's user.
func (l *Local) RequireAdmin(p domain.Principal) error {
	if p.TenantID == l.tenantID {
		return nil
	}
	return forbidden("Действие доступно только администратору tenant'а")
}

// RequireRead checks tenant first (no existence oracle), then the domain ACL rule.
func (l *Local) RequireRead(p domain.Principal, kbACL domain.ACL) error {
	if p.TenantID != l.tenantID {
		return forbidden("База знаний не найдена")
	}
	if kbACL.Grants(p) {
		return nil
	}
	return forbidden("База знаний не найдена")
}

// forbidden maps access denial to NOT_FOUND so probing cannot learn what exists.
func forbidden(message string) error {
	return domain.NotFound(message)
}

var _ ports.Authorizer = (*Local)(nil)
