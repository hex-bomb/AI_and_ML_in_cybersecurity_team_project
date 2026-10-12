package auth

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	domain "github.com/kbsearch/kb/internal/envelope"
)

// Локальная сборка: один tenant, один пользователь. Проверять тут нечего,
// кроме одного — что чужие отказы остаются отказами, а не проходят по
// случаю. Именно этот случай ломает безопасность молча: локальный principal
// единственный, и ошибка в сравнении tenant'ов выглядит как «всё работает».

func localPrincipal() domain.Principal {
	return domain.Principal{
		TenantID: domain.LocalTenantID,
		UserID:   domain.LocalUserID,
		Roles:    []string{},
		Groups:   []string{},
	}
}

func TestRequireAdminПроПускаетЛокальногоПользователя(t *testing.T) {
	a := NewLocal(domain.LocalTenantID)

	if err := a.RequireAdmin(localPrincipal()); err != nil {
		t.Fatalf("локальный пользователь должен проходить RequireAdmin, получено: %v", err)
	}
}

func TestRequireAdminОтказываетЧужомуTenant(t *testing.T) {
	a := NewLocal(domain.LocalTenantID)
	stranger := localPrincipal()
	stranger.TenantID = uuid.MustParse("22222222-2222-4222-8222-222222222222")

	err := a.RequireAdmin(stranger)
	if err == nil {
		t.Fatal("чужой tenant должен получать отказ, получено nil")
	}
	// NOT_FOUND, а не 403: код 403 в реестре домена нет, и по коду ответа
	// нельзя дать перебором узнать, какие базы вообще есть.
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("ожидался NOT_FOUND, получено: %v", err)
	}
}

func TestRequireReadПускаетПоПустомуACL(t *testing.T) {
	a := NewLocal(domain.LocalTenantID)

	// Пустой ACL означает «доступ всем пользователям tenant'а»
	// (domain.ACL.AllowsAny), и это закреплённое поведение, а не недочёт.
	if err := a.RequireRead(localPrincipal(), domain.ACL{}); err != nil {
		t.Fatalf("пустой ACL должен пускать, получено: %v", err)
	}
}

func TestRequireReadПускаетСовпавшимПользователем(t *testing.T) {
	a := NewLocal(domain.LocalTenantID)
	acl := domain.ACL{Users: []string{domain.LocalUserID}}

	if err := a.RequireRead(localPrincipal(), acl); err != nil {
		t.Fatalf("пользователь из ACL должен проходить, получено: %v", err)
	}
}

func TestRequireReadПускаетСовпавшейГруппой(t *testing.T) {
	a := NewLocal(domain.LocalTenantID)
	principal := localPrincipal()
	principal.Groups = []string{"аналитики"}
	acl := domain.ACL{Groups: []string{"аналитики"}}

	if err := a.RequireRead(principal, acl); err != nil {
		t.Fatalf("член группы из ACL должен проходить, получено: %v", err)
	}
}

func TestRequireReadОтказываетПоНеподходящемуACL(t *testing.T) {
	a := NewLocal(domain.LocalTenantID)
	acl := domain.ACL{Users: []string{"someone-else"}, Groups: []string{"другие"}}

	err := a.RequireRead(localPrincipal(), acl)
	if err == nil {
		t.Fatal("ACL, в котором пользователя нет, должен давать отказ")
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("ожидался NOT_FOUND, получено: %v", err)
	}
}

func TestRequireReadОтказываетЧужомуTenantДажеПриРазрешающемACL(t *testing.T) {
	a := NewLocal(domain.LocalTenantID)
	stranger := localPrincipal()
	stranger.TenantID = uuid.MustParse("22222222-2222-4222-8222-222222222222")

	// ACL проверяется после tenant'а, а не вместо него: ACL чужой базы не
	// должен открывать её пользователю другого tenant'а.
	err := a.RequireRead(stranger, domain.ACL{})
	if err == nil {
		t.Fatal("чужой tenant должен получать отказ даже с пустым ACL")
	}
}
