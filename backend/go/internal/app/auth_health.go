package app

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/kbsearch/backend-go/internal/config"
	"github.com/kbsearch/backend-go/internal/delivery/httpapi"
	"github.com/kbsearch/backend-go/internal/domain"
)

// localAuth — локальный режим без токена (ТЗ 9); настоящая авторизация
// подключается новой реализацией httpapi.AuthScenario.
type localAuth struct {
	tenantID uuid.UUID
	userID   string
}

func newLocalAuth(cfg config.Config) httpapi.AuthScenario {
	tenant, err := uuid.Parse(cfg.LocalTenantID)
	if err != nil {
		// config уже проверил, что переменная задана; пустой tenant при
		// нечитаемом значении безопаснее, чем паника в конструкторе.
		tenant = uuid.Nil
	}
	return localAuth{tenantID: tenant, userID: "local"}
}

func (a localAuth) Principal(_ *http.Request) (domain.Principal, error) {
	// Роли — из domain.LocalPrincipal (admin), иначе локальный пользователь теряет права.
	p := domain.LocalPrincipal()
	p.TenantID = a.tenantID
	return p, nil
}

// health отвечает на /health/live и /health/ready. readiness проверяет
// реальные зависимости пингом, а не наличие указателей.
type health struct {
	deps *Deps
}

func newHealth(d *Deps) httpapi.HealthScenario { return health{deps: d} }

func (health) Live() error { return nil }

func (h health) Ready(ctx context.Context) (map[string]bool, error) {
	checks := map[string]bool{}

	if h.deps.Pool != nil {
		checks["postgresql"] = h.deps.Pool.Ping(ctx) == nil
	} else {
		checks["postgresql"] = false
	}
	if h.deps.Index != nil {
		checks["elasticsearch"] = h.deps.Index.Ping(ctx) == nil
	} else {
		checks["elasticsearch"] = false
	}
	// LLM в готовность не входит: без неё поиск остаётся доступен (ТЗ 10.1).
	// В карте она только для диагностики оператора.
	checks["llm"] = h.deps.Embedder != nil

	ready := true
	for name, ok := range checks {
		if name == "llm" {
			continue
		}
		if !ok {
			ready = false
		}
	}
	if !ready {
		return checks, domain.ServiceUnavailable("Не все зависимости доступны")
	}
	return checks, nil
}

// tenantID — tenant локального режима из того же источника, что Principal.
func (d *Deps) tenantID() uuid.UUID {
	local, ok := d.Auth.(localAuth)
	if !ok {
		return uuid.Nil
	}
	return local.tenantID
}
