package app

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/kbsearch/kb/internal/access/auth"
	"github.com/kbsearch/kb/internal/api/http"
	"github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/platform/config"
	"github.com/kbsearch/kb/internal/ports"
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

// newAuth выбирает реализацию по AUTH_MODE.
//
// Причина, по которой это переключение нельзя спрятать в NewOIDC или сделать
// «умным»: AUTH_MODE=oidc с незаполненным OIDC_* обязан ронять старт, а не
// молча деградировать в локальный режим, где любой запрос — администратор.
func newAuth(cfg config.Config) (httpapi.AuthScenario, error) {
	switch cfg.AuthMode {
	case "", "local":
		return newLocalAuth(cfg), nil
	case "oidc":
		// Проверяем здесь, а не полагаемся на config.Validate: NewHTTPJWKS("")
		// вернёт непустой объект, который будет падать уже на первом
		// запросе — то есть в проде, а не на старте.
		for _, required := range []struct {
			name  string
			value string
		}{
			{"OIDC_ISSUER", cfg.OIDCIssuer},
			{"OIDC_AUDIENCE", cfg.OIDCAudience},
			{"OIDC_JWKS_URL", cfg.OIDCJWKSURL},
		} {
			if strings.TrimSpace(required.value) == "" {
				return nil, domain.ConfigurationInvalid(
					"При AUTH_MODE=oidc обязательны OIDC_ISSUER, OIDC_AUDIENCE и OIDC_JWKS_URL",
					map[string]any{"переменная": required.name},
				)
			}
		}
		verifier, err := auth.NewOIDC(auth.OIDCOptions{
			Issuer:      cfg.OIDCIssuer,
			Audience:    cfg.OIDCAudience,
			JWKS:        auth.NewHTTPJWKS(cfg.OIDCJWKSURL),
			Clock:       ports.SystemClock{},
			TenantClaim: cfg.OIDCTenantClaim,
			RolesClaim:  cfg.OIDCRolesClaim,
			GroupsClaim: cfg.OIDCGroupsClaim,
			RightsTTL:   cfg.OIDCRightsTTL,
		})
		if err != nil {
			return nil, err
		}
		return verifier, nil
	default:
		// config.Validate это уже отвергает; повторяем, чтобы непроверенный
		// конфиг не приводил к «аутентификации отключена».
		return nil, domain.ConfigurationInvalid("Неизвестный AUTH_MODE", map[string]any{"mode": cfg.AuthMode})
	}
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
// В режиме OIDC tenant приходит из токена, и здесь его нет: не подставляем
// пустой tenant, чтобы не выдать права не тому.
func (d *Deps) tenantID() uuid.UUID {
	local, ok := d.Auth.(localAuth)
	if !ok {
		return uuid.Nil
	}
	return local.tenantID
}
