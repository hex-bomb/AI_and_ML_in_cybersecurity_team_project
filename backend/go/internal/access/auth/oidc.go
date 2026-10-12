package auth

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
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

	// KeysTTL — на сколько кэшируется набор ключей JWKS.
	KeysTTL time.Duration

	JWKSRefreshInterval    time.Duration
	JWKSNegativeTTL        time.Duration
	JWKSFailuresBeforeOpen int
	RightsCacheSize        int
}

// OIDC разбирает Bearer-токен и строит Principal.
//
// Инвариант: любая причина отказа даёт пустой Principal и 401. Ни один путь
// не возвращает «нулевого» пользователя с правами — иначе отсутствие токена
// читалось бы как «гость с полными правами».
type OIDC struct {
	opts OIDCOptions

	mu         sync.RWMutex
	keys       map[string]*rsa.PublicKey
	keysAt     time.Time
	lastTryAt  time.Time
	failures   int
	unknownKid map[string]time.Time
	verified   map[[sha256.Size]byte]cachedPrincipal

	// fetchMu сериализует походы к IdP. Без него N параллельных запросов с
	// неизвестным kid дают N параллельных обращений (stampede): предел
	// частоты проверяется до захвата замка, но саму загрузку он не отменяет.
	fetchMu sync.Mutex
}

type cachedPrincipal struct {
	addedAt   time.Time
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
	if opts.KeysTTL <= 0 {
		opts.KeysTTL = 5 * time.Minute
	}
	// Предел частоты: не чаще раза в минуту при неизвестном kid. Отдельная
	// настройка от KeysTTL нужна, потому что это разные решения: как часто
	// мы замечаем ротацию ключей и как часто нам можно ходить в IdP.
	if opts.JWKSRefreshInterval <= 0 {
		opts.JWKSRefreshInterval = time.Minute
	}
	if opts.JWKSNegativeTTL <= 0 {
		opts.JWKSNegativeTTL = time.Minute
	}
	if opts.JWKSFailuresBeforeOpen <= 0 {
		opts.JWKSFailuresBeforeOpen = 3
	}
	if opts.RightsCacheSize <= 0 {
		opts.RightsCacheSize = 4096
	}
	if opts.JWKSRefreshInterval <= 0 {
		opts.JWKSRefreshInterval = 30 * time.Second
	}
	if opts.JWKSNegativeTTL <= 0 {
		opts.JWKSNegativeTTL = time.Minute
	}
	if opts.JWKSFailuresBeforeOpen <= 0 {
		opts.JWKSFailuresBeforeOpen = 5
	}
	if opts.RightsCacheSize <= 0 {
		opts.RightsCacheSize = 1024
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
	return &OIDC{opts: opts, verified: map[[sha256.Size]byte]cachedPrincipal{}}, nil
}

// Principal реализует httpapi.AuthScenario структурно: импортировать пакет
// api из feature-слоя не нужно, интерфейс одноиметодный.
func (o *OIDC) Principal(r *http.Request) (domain.Principal, error) {
	token, err := bearerToken(r)
	if err != nil {
		return domain.Principal{}, err
	}

	now := o.opts.Clock.Now()

	// Подпись, издатель, аудитория и срок токена проверяются на КАЖДЫЙ запрос,
	// кэшем это не обходится: иначе отозванный ключ или истёкший токен жил бы
	// в кэше вплоть до конца RightsTTL, и отзыв подписи стал бы невозможен.
	// Кэш ниже экономит повторный разбор уже проверенного токена — это чистый
	// CPU, а не сеть.
	claims, err := o.verify(r.Context(), token, now)
	if err != nil {
		return domain.Principal{}, err
	}

	sum := tokenDigest(token)
	if p, ok := o.fromCache(sum, now); ok {
		return p, nil
	}

	p, err := o.principalFrom(claims)
	if err != nil {
		return domain.Principal{}, err
	}
	o.toCache(sum, p, now, claims)
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
		// Отказ инфраструктуры нельзя выдавать за «плохой токен»: клиент
		// начнёт менять токен вместо того, чтобы звать администратора, а
		// мониторинг покажет всплеск неверных токенов вместо недоступности
		// IdP. Токен в лог не пишем.
		// golang-jwt оборачивает ошибку keyfunc в errors.Join, поэтому
		// Unwrap() на верхнем уровне пуст — обходить надо всю цепочку.
		var appErr *domain.AppError
		if errors.As(err, &appErr) && appErr.Code == domain.CodeServiceUnavailable {
			return nil, appErr
		}
		// Причина в лог, наружу — единый отказ.
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

	// kid уже проверялся и в JWKS его не было. Повторный поход к IdP по
	// тому же kid ничего не даст: ключ либо появился, либо нет, а ждать
	// ничего не предпринимал этот запрос.
	if o.knownUnknown(kid, now) {
		return nil, fmt.Errorf("ключ %q не найден", kid)
	}

	// Ключи у IdP могли смениться при ротации: пробуем перечитать один раз.
	if err := o.refresh(ctx, now); err != nil {
		// Отрицательный кэш здесь НЕ ставится: поход не удался из-за
		// недоступности IdP, а не из-за отсутствия kid. Иначе после
		// восстановления IdP прежние отказы держались бы и дальше.
		return nil, err
	}
	key, ok := o.key(kid, now)
	if !ok {
		o.markUnknown(kid, now)
		return nil, fmt.Errorf("ключ %q не найден", kid)
	}
	return key, nil
}

// knownUnknown сообщает, что kid уже опрашивался и не найден.
func (o *OIDC) knownUnknown(kid string, now time.Time) bool {
	o.mu.RLock()
	defer o.mu.RUnlock()
	at, seen := o.unknownKid[kid]
	return seen && now.Sub(at) < o.opts.JWKSNegativeTTL
}

func (o *OIDC) markUnknown(kid string, now time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.unknownKid == nil {
		o.unknownKid = map[string]time.Time{}
	}
	o.unknownKid[kid] = now
}

// key возвращает ключ из кэша, если срок кэша не истёк.
func (o *OIDC) key(kid string, now time.Time) (*rsa.PublicKey, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.keys == nil || now.Sub(o.keysAt) >= o.opts.KeysTTL {
		return nil, false
	}
	key, ok := o.keys[kid]
	return key, ok
}

// refresh перечитывает JWKS под тремя ограничениями.
//
// Первое — предел частоты, не зависящий от запроса: без него неизвестный kid
// в каждом запросе давал поход к IdP (амплификация 1:1, проверено тестом на
// 100 запросов). Второе — сериализация, иначе N параллельных запросов дают N
// параллельных обращений. Третье — автомат: после серии неудач IdP больше не
// трогаем и отвечаем 503, потому что бить по недоступной системе бессмысленно.
func (o *OIDC) refresh(ctx context.Context, now time.Time) error {
	o.fetchMu.Lock()
	defer o.fetchMu.Unlock()

	o.mu.RLock()
	open := o.failures >= o.opts.JWKSFailuresBeforeOpen
	tooSoon := !o.lastTryAt.IsZero() && now.Sub(o.lastTryAt) < o.opts.JWKSRefreshInterval
	o.mu.RUnlock()

	if open {
		return domain.ServiceUnavailable("Служба аутентификации временно недоступна")
	}
	if tooSoon {
		// IdP спрашивали недавно; сколько бы раз ни пришёл запрос с этим kid,
		// повторный поход ничего не изменит.
		return domain.ServiceUnavailable("Служба аутентификации временно недоступна")
	}

	o.mu.Lock()
	o.lastTryAt = now
	o.mu.Unlock()

	keys, err := o.opts.JWKS.Fetch(ctx)
	if err != nil || keys == nil {
		o.mu.Lock()
		o.failures++
		o.mu.Unlock()
		if err == nil {
			err = errors.New("JWKS вернул пустой набор ключей")
		}
		return fmt.Errorf("перечитывание JWKS: %w", err)
	}

	o.mu.Lock()
	o.keys = keys
	o.keysAt = now
	o.failures = 0
	// Набор ключей обновился: прежние отрицательные ответы по kid больше не
	// действительны — возможно, именно из-за ротации их и искали.
	o.unknownKid = map[string]time.Time{}
	o.mu.Unlock()
	return nil
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
	// Нулевой UUID разбирается без ошибки, поэтому проверка выше его не ловит.
	// Он опасен не сам по себе, а тем, что сравним с tenant'ом авторизатора:
	// токен без tenant получил бы права наравне с «своим».
	if tenant == uuid.Nil {
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

// tokenDigest — ключ кэша. Хранится хеш, а не сам токен: сырой bearer в куче
// попал бы в core dump и heap profile.
func tokenDigest(token string) [sha256.Size]byte { return sha256.Sum256([]byte(token)) }

// fromCache отдаёт разобранный Principal для уже проверенного токена.
func (o *OIDC) fromCache(sum [sha256.Size]byte, now time.Time) (domain.Principal, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	entry, ok := o.verified[sum]
	if !ok || now.After(entry.expiresAt) {
		return domain.Principal{}, false
	}
	return entry.principal, true
}

// toCache кладёт Principal с предельным сроком и следит за размером кэша.
//
// Срок записи — min(now+RightsTTL, exp): иначе RightsTTL продлевал бы жизнь
// токена за пределы, которые IdP задал его сроком.
func (o *OIDC) toCache(sum [sha256.Size]byte, p domain.Principal, now time.Time, claims jwt.MapClaims) {
	o.mu.Lock()
	defer o.mu.Unlock()

	expiresAt := now.Add(o.opts.RightsTTL)
	if exp, err := claims.GetExpirationTime(); err == nil && exp != nil {
		if expTime := exp.Time; expTime.Before(expiresAt) {
			expiresAt = expTime
		}
	}
	if now.After(expiresAt) {
		return
	}
	o.verified[sum] = cachedPrincipal{principal: p, expiresAt: expiresAt, addedAt: now}
	o.evictLocked(now)
}

// evictLocked вытесняет мёртвые записи, а если этого мало — самые старые.
// Без этого карта растёт со скоростью «сколько токенов успело смениться» и
// никогда не сжимается: утечка памяти на долгоживущем процессе.
func (o *OIDC) evictLocked(now time.Time) {
	for sum, entry := range o.verified {
		if now.After(entry.expiresAt) {
			delete(o.verified, sum)
		}
	}
	limit := o.opts.RightsCacheSize
	if limit <= 0 {
		return
	}
	for len(o.verified) > limit {
		// Записи вытесняются по возрасту добавления: набор small, порядок
		// по времени достаточен и не тянет за собой структуру с узлами.
		var oldest [sha256.Size]byte
		var oldestAt time.Time
		first := true
		for sum, entry := range o.verified {
			if first || entry.addedAt.Before(oldestAt) {
				oldest, oldestAt, first = sum, entry.addedAt, false
			}
		}
		if first {
			return
		}
		delete(o.verified, oldest)
	}
}

// rightsLen и rightsKeys — наблюдатели кэша прав для тестов.
func (o *OIDC) rightsLen() int {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return len(o.verified)
}

func (o *OIDC) rightsKeys() [][sha256.Size]byte {
	o.mu.RLock()
	defer o.mu.RUnlock()
	out := make([][sha256.Size]byte, 0, len(o.verified))
	for k := range o.verified {
		out = append(out, k)
	}
	return out
}
