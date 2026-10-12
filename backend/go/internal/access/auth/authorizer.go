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

// TenantRBAC — авторизация для режима OIDC, где tenant приходит из токена.
//
// Local не годится: он сравнивает с одним жёстко заданным tenant'ом и понятия
// роли не имеет. Собрать Local с нулевым tenant — значит выдать права тому, у
// кого tenant тоже нулевой, то есть ровно тому, кто принёс токен с
// tenant_id = 00000000-0000-0000-0000-000000000000.
//
// Здесь обе проверки обязательны всегда: tenant из токена не должен быть пустым,
// а администратором делает роль, а не совпадение tenant с константой.
type TenantRBAC struct{}

// NewTenantRBAC creates role-based authorization for the OIDC mode.
func NewTenantRBAC() *TenantRBAC { return &TenantRBAC{} }

// RequireAdmin passes a principal of a real tenant carrying the admin role.
// Нулевой tenant — всегда отказ: иначе токен без tenant выдаёт полные права.
func (TenantRBAC) RequireAdmin(p domain.Principal) error {
	if p.TenantID == uuid.Nil {
		return forbidden("Действие доступно только администратору tenant'а")
	}
	if p.HasRole(RoleAdmin) {
		return nil
	}
	return forbidden("Действие доступно только администратору tenant'а")
}

// RequireRead checks tenant first (no existence oracle), then the domain ACL rule.
func (TenantRBAC) RequireRead(p domain.Principal, kbACL domain.ACL) error {
	if p.TenantID == uuid.Nil {
		return forbidden("База знаний не найдена")
	}
	if kbACL.Grants(p) {
		return nil
	}
	return forbidden("База знаний не найдена")
}

// RoleAdmin — роль, дающая права администратора tenant'а. Объявлена здесь,
// потому что роль приходит из токена, а Local о ролях не знает.
const RoleAdmin = "admin"
