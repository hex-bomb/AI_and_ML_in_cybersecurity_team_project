package app

import (
	"context"
	"crypto/rsa"
	"errors"
	"net/http"
	"net/http/httptest"

	"github.com/google/uuid"

	"strings"
	"testing"

	"github.com/kbsearch/kb/internal/access/auth"
	domain "github.com/kbsearch/kb/internal/envelope"

	"github.com/kbsearch/kb/internal/platform/config"
)

// TestNewAuthРежимOIDCНеДеградируетВЛокальный: главная опасность этой
// настройки — AUTH_MODE=oidc с неполными OIDC_* молча откатывается в
// локальный режим, где любой запрос считается администратором. Такой отказ
// обязан ронять старт.
func TestNewAuthРежимOIDCНеДеградируетВЛокальный(t *testing.T) {
	cases := []struct {
		name    string
		cfg     config.Config
		wantErr bool
	}{
		{
			name:    "пустой режим — локальный",
			cfg:     config.Config{AuthMode: "", LocalTenantID: "11111111-1111-4111-8111-111111111111"},
			wantErr: false,
		},
		{
			name:    "локальный режим",
			cfg:     config.Config{AuthMode: "local", LocalTenantID: "11111111-1111-4111-8111-111111111111"},
			wantErr: false,
		},
		{
			name:    "oidc без издателя",
			cfg:     config.Config{AuthMode: "oidc", OIDCAudience: "kb", OIDCJWKSURL: "https://idp/jwks"},
			wantErr: true,
		},
		{
			name:    "oidc без аудитории",
			cfg:     config.Config{AuthMode: "oidc", OIDCIssuer: "https://idp", OIDCJWKSURL: "https://idp/jwks"},
			wantErr: true,
		},
		{
			name:    "oidc без JWKS",
			cfg:     config.Config{AuthMode: "oidc", OIDCIssuer: "https://idp", OIDCAudience: "kb"},
			wantErr: true,
		},
		{
			name: "oidc полностью задан",
			cfg: config.Config{
				AuthMode:     "oidc",
				OIDCIssuer:   "https://idp",
				OIDCAudience: "kb",
				OIDCJWKSURL:  "https://idp/jwks",
			},
			wantErr: false,
		},
		{
			name:    "неизвестный режим",
			cfg:     config.Config{AuthMode: "что-то ещё", LocalTenantID: "11111111-1111-4111-8111-111111111111"},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scenario, err := newAuth(tc.cfg)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ожидался отказ, получено %T", scenario)
				}
				return
			}
			if err != nil {
				t.Fatalf("ожидался успех, получено: %v", err)
			}
			if scenario == nil {
				t.Fatal("сценарий аутентификации не собран")
			}
		})
	}
}

// TestNewAuthЛокальныйРежимНеТребуетТокена: в локальном режиме запрос без
// заголовка Authorization обязан работать — иначе нельзя ни запустить
// стенд, ни разрабатывать.
func TestNewAuthЛокальныйРежимНеТребуетТокена(t *testing.T) {
	scenario, err := newAuth(config.Config{
		AuthMode:      "local",
		LocalTenantID: "11111111-1111-4111-8111-111111111111",
	})
	if err != nil {
		t.Fatalf("newAuth: %v", err)
	}
	p, err := scenario.Principal(httptest.NewRequest(http.MethodGet, "/api/v1/kbs", nil))
	if err != nil {
		t.Fatalf("локальный запрос без токена должен проходить: %v", err)
	}
	if p.UserID == "" {
		t.Error("пустой UserID в локальном режиме")
	}
}

// TestNewAuthOIDCОтвергаетЗапросБезТокена: в режиме oidc отсутствие токена —
// отказ, а не тихий локальный администратор.
func TestNewAuthOIDCОтвергаетЗапросБезТокена(t *testing.T) {
	scenario, err := newAuth(config.Config{
		AuthMode:     "oidc",
		OIDCIssuer:   "https://idp",
		OIDCAudience: "kb",
		OIDCJWKSURL:  "http://127.0.0.1:1/jwks",
	})
	if err != nil {
		t.Fatalf("newAuth: %v", err)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/kbs", nil)
	if _, err := scenario.Principal(r); err == nil {
		t.Fatal("запрос без токена в режиме oidc обязан отклоняться")
	}

	// И с пустым заголовком Bearer — тоже.
	r = httptest.NewRequest(http.MethodGet, "/api/v1/kbs", nil)
	r.Header.Set("Authorization", "Bearer   ")
	if _, err := scenario.Principal(r); err == nil {
		t.Fatal("пустой Bearer обязан отклоняться")
	}

	// Ответ не должен намекать, что именно сломалось: одинаковый текст на
	// все причины не даёт переборщику оракул.
	r = httptest.NewRequest(http.MethodGet, "/api/v1/kbs", nil)
	_, err = scenario.Principal(r)
	r2 := httptest.NewRequest(http.MethodGet, "/api/v1/kbs", nil)
	r2.Header.Set("Authorization", "Basic abc")
	_, err2 := scenario.Principal(r2)
	if err == nil || err2 == nil {
		t.Fatal("ожидались отказы")
	}
	if !strings.Contains(err.Error(), domainUnauthorizedCode) {
		t.Errorf("отказ без токена должен быть UNAUTHORIZED, получено %q", err.Error())
	}
	if err.Error() != err2.Error() {
		t.Errorf("тексты отказа различаются и дают оракул: %q против %q", err.Error(), err2.Error())
	}
}

const domainUnauthorizedCode = "UNAUTHORIZED"

// TestOIDCНеДаётАдминаНулевомуTenant: регрессия на дефект C-1 из аудита
// безопасности.
//
// Цепочка дефекта: в режиме oidc Deps.tenantID() возвращал uuid.Nil, потому что
// d.Auth — это *auth.OIDC, а не localAuth. Авторизатор собирался как
// NewLocal(uuid.Nil), а его RequireAdmin пропускает любого, у кого TenantID
// равен нулевому UUID. Токен с tenant_id = 00000000-... проходил
// аутентификацию (principalFrom нулевой UUID не отвергал) и получал права
// администратора на всю базу знаний. Второй исход той же строки: нормальный
// администратор своего tenant отклонялся.
//
// Проверяется через реальную сборку Deps.authorizer, а не ручным созданием
// Local — иначе тест зафиксировал бы старую, сломанную конструкцию.
func TestOIDCНеДаётАдминаНулевомуTenant(t *testing.T) {
	oidc, err := auth.NewOIDC(auth.OIDCOptions{
		Issuer:   "https://idp.example.com",
		Audience: "kbsearch",
		JWKS:     neverKeys{},
	})
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	d := &Deps{Config: config.Config{AuthMode: "oidc"}, Auth: oidc}
	authorizer := d.authorizer()

	// Принципал с нулевым tenant — ровно тот, что даёт такой токен.
	attacker := domain.Principal{TenantID: uuid.Nil, UserID: "anna", Roles: []string{"reader"}}
	if err := authorizer.RequireAdmin(attacker); err == nil {
		t.Error("Принципал с нулевым tenant получил права администратора — эскалация")
	}

	// Администратор своего tenant обязан проходить: режим OIDC не должен
	// быть сломан для легитимных пользователей.
	real := domain.Principal{
		TenantID: uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		UserID:   "bob",
		Roles:    []string{auth.RoleAdmin},
	}
	if err := authorizer.RequireAdmin(real); err != nil {
		t.Errorf("Администратор своего tenant отклонён: %v", err)
	}

	// Читатель своего tenant администратором не является.
	reader := domain.Principal{TenantID: real.TenantID, UserID: "carol", Roles: []string{"reader"}}
	if err := authorizer.RequireAdmin(reader); err == nil {
		t.Error("Читатель получил права администратора")
	}
}

// neverKeys — JWKS-заглушка для тестов выбора авторизатора: ключи тут не
// нужны, проверяется не разбор токена, а то, какой авторизатор собран.
type neverKeys struct{}

func (neverKeys) Fetch(context.Context) (map[string]*rsa.PublicKey, error) {
	return nil, errors.New("в этом тесте ключи не загружаются")
}
