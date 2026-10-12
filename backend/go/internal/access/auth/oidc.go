package auth

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	domain "github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
)

// JWKSFetcher отдаёт публичные ключи IdP по kid.
//
// Узкий интерфейс рядом с потребителем, а не порт в internal/ports: конкретный
// IdP (Keycloak, Entra, Yandex) приносит свой формат ответа, и протаскивать
// это в общий порт незачем. Правило проекта прямо разрешает такое исключение.
type JWKSFetcher interface {
	Fetch(ctx context.Context) (map[string]*rsa.PublicKey, error)
}

// OIDCOptions — параметры проверки токена.
type OIDCOptions struct {
	Issuer   string
	Audience string
	JWKS     JWKSFetcher
	Clock    ports.Clock

	// Имена клеймов. У разных IdP они различаются, поэтому вынесены в
	// настройку, а не зашиты в код.
	TenantClaim string
	RolesClaim  string
	GroupsClaim string

	// RightsTTL — на сколько кэшируется разобранный Principal. Это и есть
	// окно, в пределах которого отзыв доступа ещё не действует (критерий
	// MVP §9 «Вход»).
	RightsTTL time.Duration
}

// OIDC разбирает Bearer-токен и строит Principal.
//
// Инвариант: любая причина отказа даёт пустой Principal и 401. Ни один путь
// не возвращает «нулевого» пользователя с правами — иначе отсутствие токена
// читалось бы как «гость с полными правами».
type OIDC struct {
	opts OIDCOptions

	mu       sync.RWMutex
	keys     map[string]*rsa.PublicKey
	keysAt   time.Time
	verified map[string]cachedPrincipal
}

type cachedPrincipal struct {
	principal domain.Principal
	expiresAt time.Time
}

// NewOIDC собирает проверяющий токены resolver.
// Ошибки конфигурации возвращаются вызывающему: молчаливый отказ проверки хуже неудачного старта.
func NewOIDC(opts OIDCOptions) (*OIDC, error) {
	if opts.Issuer == "" {
		return nil, domain.ConfigurationInvalid("Некорректная конфигурация OIDC", map[string]any{"issuer": "обязателен"})
	}
	if opts.Audience == "" {
		return nil, domain.ConfigurationInvalid("Некорректная конфигурация OIDC", map[string]any{"audience": "обязателен"})
	}
	if opts.JWKS == nil {
		return nil, domain.ConfigurationInvalid("Некорректная конфигурация OIDC", map[string]any{"jwks": "обязателен"})
	}
	if opts.Clock == nil {
		opts.Clock = ports.SystemClock{}
	}
	if opts.RightsTTL <= 0 {
		opts.RightsTTL = 5 * time.Minute
	}
	if opts.TenantClaim == "" {
		opts.TenantClaim = "tenant_id"
	}
	if opts.RolesClaim == "" {
		opts.RolesClaim = "roles"
	}
	if opts.GroupsClaim == "" {
		opts.GroupsClaim = "groups"
	}
	return &OIDC{opts: opts, verified: map[string]cachedPrincipal{}}, nil
}

// Principal реализует httpapi.AuthScenario структурно: импортировать пакет
// api из feature-слоя не нужно, интерфейс одноиметодный.
func (o *OIDC) Principal(r *http.Request) (domain.Principal, error) {
	token, err := bearerToken(r)
	if err != nil {
		return domain.Principal{}, err
	}

	now := o.opts.Clock.Now()

	// Разбор отложен только на срок кэша прав; сама подпись и срок действия
	// токена проверяются на каждом запросе, иначе отозванный токен жил бы
	// в кэше.
	if p, ok := o.fromCache(token, now); ok {
		return p, nil
	}

	claims, err := o.verify(r.Context(), token, now)
	if err != nil {
		return domain.Principal{}, err
	}

	p, err := o.principalFrom(claims)
	if err != nil {
		return domain.Principal{}, err
	}
	o.toCache(token, p, now)
	return p, nil
}

// bearerToken достаёт токен из заголовка. Отказ намеренно общий на все
// причины: различая «нет заголовка» и «токен битый», мы даём переборщику
// информацию, которой у него нет.
func bearerToken(r *http.Request) (string, error) {
	raw := r.Header.Get("Authorization")
	if raw == "" {
		return "", domain.Unauthorized("")
	}
	scheme, token, found := strings.Cut(raw, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return "", domain.Unauthorized("")
	}
	return strings.TrimSpace(token), nil
}

// verify проверяет подпись, издателя, аудиторию и срок действия токена.
func (o *OIDC) verify(ctx context.Context, token string, now time.Time) (jwt.MapClaims, error) {
	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		return o.keyFor(ctx, t, now)
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}),
		jwt.WithIssuer(o.opts.Issuer),
		jwt.WithAudience(o.opts.Audience),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(func() time.Time { return now }),
	)
	if err != nil {
		// Причина в лог, наружу — единый отказ. Токен в лог не пишем.
		return nil, domain.Unauthorized("").WithCause(fmt.Errorf("токен не прошёл проверку: %w", err))
	}
	return claims, nil
}

// keyFor отдаёт ключ проверки подписи и молча отказывает на всём, кроме
// асимметричного RS256.
//
// WithValidMethods в verify уже отсекает прочие алгоритмы, но проверка
// дублируется здесь: она стоит рядом с выбором ключа, и подмена alg на HS256
// с публичным ключом IdP в качестве секрета — известная атака, которую
// нельзя отсекать в другом слое.
func (o *OIDC) keyFor(ctx context.Context, t *jwt.Token, now time.Time) (any, error) {
	if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
		return nil, fmt.Errorf("алгоритм %q не принимается", t.Method.Alg())
	}
	if t.Method.Alg() != jwt.SigningMethodRS256.Alg() {
		return nil, fmt.Errorf("алгоритм %q не принимается", t.Method.Alg())
	}

	kid, _ := t.Header["kid"].(string)
	if kid == "" {
		return nil, errors.New("в токене нет kid")
	}

	if key, ok := o.key(kid, now); ok {
		return key, nil
	}
	// Неизвестный kid: ключи у IdP могли смениться при ротации. Перечитываем
	// один раз и пробуем снова.
	o.refresh(ctx, now)
	key, ok := o.key(kid, now)
	if !ok {
		return nil, fmt.Errorf("ключ %q не найден", kid)
	}
	return key, nil
}

// key возвращает ключ из кэша, если срок кэша не истёк.
func (o *OIDC) key(kid string, now time.Time) (*rsa.PublicKey, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.keys == nil || now.Sub(o.keysAt) >= o.opts.RightsTTL {
		return nil, false
	}
	key, ok := o.keys[kid]
	return key, ok
}

// refresh перечитывает JWKS. Ошибка здесь не фатальна для вызывающего:
// keyFor попробует ключи, которые уже есть, и откажет, если ключа нет.
func (o *OIDC) refresh(ctx context.Context, now time.Time) {
	keys, err := o.opts.JWKS.Fetch(ctx)
	if err != nil || keys == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.keys = keys
	o.keysAt = now
}

// principalFrom переносит клеймы в Principal. Отсутствие tenant — отказ:
// кому именно выдавать права, из токена не известно, а пустой tenant в
// Principal означал бы «права ни на что».
func (o *OIDC) principalFrom(claims jwt.MapClaims) (domain.Principal, error) {
	rawTenant, ok := claims[o.opts.TenantClaim].(string)
	if !ok || rawTenant == "" {
		return domain.Principal{}, domain.Unauthorized("")
	}
	tenant, err := uuid.Parse(rawTenant)
	if err != nil {
		return domain.Principal{}, domain.Unauthorized("")
	}
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return domain.Principal{}, domain.Unauthorized("")
	}

	return domain.Principal{
		TenantID: tenant,
		UserID:   sub,
		Roles:    stringSlice(claims[o.opts.RolesClaim]),
		Groups:   stringSlice(claims[o.opts.GroupsClaim]),
	}, nil
}

// stringSlice читает клейм как список строк. IdP отдаёт группы как
// список, но отдельные поставщики кладут одиночную строку — такой клейм
// обязан трактоваться так же, как список из одного элемента.
func stringSlice(v any) []string {
	switch typed := v.(type) {
	case []string:
		return append([]string(nil), typed...)
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		if typed == "" {
			return nil
		}
		return []string{typed}
	default:
		return nil
	}
}

func (o *OIDC) fromCache(token string, now time.Time) (domain.Principal, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	entry, ok := o.verified[token]
	if !ok || now.After(entry.expiresAt) {
		return domain.Principal{}, false
	}
	return entry.principal, true
}

func (o *OIDC) toCache(token string, p domain.Principal, now time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.verified[token] = cachedPrincipal{principal: p, expiresAt: now.Add(o.opts.RightsTTL)}
}
