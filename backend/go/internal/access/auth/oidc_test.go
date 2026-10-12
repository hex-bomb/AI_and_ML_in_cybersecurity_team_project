package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	domain "github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
)

// Ключи и издатель для тестов. Подпись настоящая: тесты, которые проходят
// с поддельной верификацией, ничего не проверяют.
var (
	testIssuer   = "https://idp.example.com"
	testAudience = "kbsearch"
	testTenant   = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	testKey      *rsa.PrivateKey
	testKeyID    = "key-1"
	testOtherKey *rsa.PrivateKey
	testOtherKID = "key-2"
)

func init() {
	var err error
	testKey, err = rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	testOtherKey, err = rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
}

// fixedClock — часы, которые двигает тест, чтобы проверить границы TTL.
type fixedClock struct{ now time.Time }

func (c *fixedClock) Now() time.Time                             { return c.now }
func (c *fixedClock) Sleep(context.Context, time.Duration) error { return nil }

// countingFetcher считает обращения к JWKS: по ним видно, что ключи
// перечитываются при неизвестном kid, а не на каждый запрос.
//
// Счётчик и набор ключей под мьютексом: тесты на параллельные запросы
// гоняют Fetch из нескольких горутин, и без синхронизации гонка была бы
// в самом тесте, а не в проверяемом коде.
type countingFetcher struct {
	mu    sync.Mutex
	keys  map[string]*rsa.PublicKey
	calls int
}

func (f *countingFetcher) Fetch(context.Context) (map[string]*rsa.PublicKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.keys, nil
}

// Calls — сколько раз обращались к JWKS за всё время теста.
func (f *countingFetcher) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// SetKeys имитирует ротацию ключей у IdP.
func (f *countingFetcher) SetKeys(keys map[string]*rsa.PublicKey) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = keys
}

// failingFetcher — недоступный IdP: сеть есть, ответа нет.
type failingFetcher struct {
	mu    sync.Mutex
	calls int
	err   error

	// succeedFirst — сколько первых обращений ещё отдают ключи. Нужно, чтобы
	// воспроизвести «ключи уже в кэше, IdP лёг»: при succeedFirst=0 ключи не
	// загрузятся никогда и проверить работу с кэшем невозможно.
	succeedFirst int
	keys         map[string]*rsa.PublicKey
}

func (f *failingFetcher) Fetch(context.Context) (map[string]*rsa.PublicKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= f.succeedFirst {
		return f.keys, nil
	}
	return nil, f.err
}

func (f *failingFetcher) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// newTestOIDC собирает OIDC с настройками по умолчанию для тестов.
func newTestOIDC(t *testing.T, fetch JWKSFetcher, clock ports.Clock) *OIDC {
	t.Helper()
	return newTestOIDCWith(t, fetch, clock, nil)
}

// newTestOIDCWith собирает OIDC, позволяя переопределить параметры:
// защиты от DoS (H-2) и раздельные TTL (H-3) проверяются с нестандартными
// значениями, которые иначе недостижимы из теста.
func newTestOIDCWith(t *testing.T, fetch JWKSFetcher, clock ports.Clock, tune func(*OIDCOptions)) *OIDC {
	t.Helper()
	opts := OIDCOptions{
		Issuer:      testIssuer,
		Audience:    testAudience,
		JWKS:        fetch,
		Clock:       clock,
		TenantClaim: "tenant_id",
		RolesClaim:  "roles",
		GroupsClaim: "groups",
		RightsTTL:   5 * time.Minute,
	}
	if tune != nil {
		tune(&opts)
	}
	o, err := NewOIDC(opts)
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	return o
}

// sign собирает токен с произвольными изменениями перед подписью.
func sign(t *testing.T, key *rsa.PrivateKey, kid string, method jwt.SigningMethod, mutate func(jwt.MapClaims)) string {
	t.Helper()
	claims := jwt.MapClaims{
		"sub": "anna",
		"iss": testIssuer,
		"aud": testAudience,
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Add(-time.Minute).Unix(),
	}
	if mutate != nil {
		mutate(claims)
	}
	tok := jwt.NewWithClaims(method, claims)
	tok.Header["kid"] = kid
	signed, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("подпись токена: %v", err)
	}
	return signed
}

func requestWith(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/kbs", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func bothKeys() map[string]*rsa.PublicKey {
	return map[string]*rsa.PublicKey{
		testKeyID:    &testKey.PublicKey,
		testOtherKID: &testOtherKey.PublicKey,
	}
}

// TestOIDCPrincipalHappyPath: корректный токен даёт Principal с правами из клеймов.
func TestOIDCPrincipalHappyPath(t *testing.T) {
	fetch := &countingFetcher{keys: bothKeys()}
	clock := &fixedClock{now: time.Now()}
	o := newTestOIDC(t, fetch, clock)

	token := sign(t, testKey, testKeyID, jwt.SigningMethodRS256, func(c jwt.MapClaims) {
		c["tenant_id"] = testTenant.String()
		c["roles"] = []string{"admin", "reader"}
		c["groups"] = []string{"team-a"}
	})

	p, err := o.Principal(requestWith(token))
	if err != nil {
		t.Fatalf("Principal вернул ошибку: %v", err)
	}
	if p.TenantID != testTenant {
		t.Errorf("TenantID = %v, ожидался %v", p.TenantID, testTenant)
	}
	if p.UserID != "anna" {
		t.Errorf("UserID = %q, ожидался %q", p.UserID, "anna")
	}
	if !p.HasRole("admin") {
		t.Errorf("роль admin потерялась: %v", p.Roles)
	}
	if len(p.Groups) != 1 || p.Groups[0] != "team-a" {
		t.Errorf("Groups = %v, ожидались [team-a]", p.Groups)
	}
}

// TestOIDCОтказЗакрытПоУмолчанию: любая причина не проходит молча. Это
// главный инвариант — незнакомая причина отказа обязана давать отказ.
func TestOIDCОтказЗакрытПоУмолчанию(t *testing.T) {
	valid := func(c jwt.MapClaims) {
		c["tenant_id"] = testTenant.String()
	}

	cases := []struct {
		name  string
		token string
		setup func(*OIDC)
	}{
		{
			name:  "заголовка Authorization нет",
			token: "",
		},
		{
			name:  "схема не Bearer",
			token: "Basic " + sign(t, testKey, testKeyID, jwt.SigningMethodRS256, valid),
		},
		{
			name:  "токен не подписан тем ключом, которым объявлен kid",
			token: sign(t, testOtherKey, testKeyID, jwt.SigningMethodRS256, valid),
		},
		{
			name: "токен просрочен",
			token: sign(t, testKey, testKeyID, jwt.SigningMethodRS256, func(c jwt.MapClaims) {
				valid(c)
				c["exp"] = time.Now().Add(-time.Hour).Unix()
			}),
		},
		{
			name: "чужой издатель",
			token: sign(t, testKey, testKeyID, jwt.SigningMethodRS256, func(c jwt.MapClaims) {
				valid(c)
				c["iss"] = "https://злой.example.com"
			}),
		},
		{
			name: "чужая аудитория",
			token: sign(t, testKey, testKeyID, jwt.SigningMethodRS256, func(c jwt.MapClaims) {
				valid(c)
				c["aud"] = "другое-приложение"
			}),
		},
		{
			name:  "alg=none — подпись не проверяется вовсе",
			token: unsignedNoneToken(t),
		},
		{
			name:  "alg=HS256 вместо RS256 — подпись чужим симметричным ключом",
			token: hs256Token(t),
		},
		{
			name:  "неизвестный kid",
			token: sign(t, testKey, "kid-которого-нет", jwt.SigningMethodRS256, valid),
		},
		{
			name:  "в токене нет tenant — кому именно выдавать права, не известно",
			token: sign(t, testKey, testKeyID, jwt.SigningMethodRS256, nil),
		},
		{
			name: "tenant в токене нечитаем",
			token: sign(t, testKey, testKeyID, jwt.SigningMethodRS256, func(c jwt.MapClaims) {
				c["tenant_id"] = "не-uuid"
			}),
		},
		{
			name:  "мусор вместо токена",
			token: "не-токен-вовсе",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fetch := &countingFetcher{keys: bothKeys()}
			o := newTestOIDC(t, fetch, &fixedClock{now: time.Now()})

			p, err := o.Principal(requestWith(tc.token))
			if err == nil {
				t.Fatalf("ожидался отказ, а Principal = %+v", p)
			}
			// Отказ обязан быть 401, а не «пустым успехом».
			appErr, ok := domain.AsAppError(err)
			if !ok {
				t.Fatalf("ошибка не доменная: %#v", err)
			}
			if appErr.HTTPStatus() != http.StatusUnauthorized {
				t.Errorf("статус = %d, ожидался 401", appErr.HTTPStatus())
			}
			if p.TenantID != uuid.Nil || p.UserID != "" {
				t.Errorf("при отказе Principal не должен быть заполнен: %+v", p)
			}
		})
	}
}

// TestOIDCНеизвестныйKidПеречитываетJWKS: смена ключа у IdP должна
// подхватываться, иначе после ротации ключей всех выкинет из системы.
func TestOIDCНеизвестныйKidПеречитываетJWKS(t *testing.T) {
	// На старте у IdP только один ключ — как до ротации.
	fetch := &countingFetcher{keys: map[string]*rsa.PublicKey{testKeyID: &testKey.PublicKey}}
	start := time.Now()
	clock := &fixedClock{now: start}
	o := newTestOIDC(t, fetch, clock)

	known := sign(t, testKey, testKeyID, jwt.SigningMethodRS256, func(c jwt.MapClaims) {
		c["tenant_id"] = testTenant.String()
	})
	if _, err := o.Principal(requestWith(known)); err != nil {
		t.Fatalf("первый запрос: %v", err)
	}
	if fetch.Calls() != 1 {
		t.Fatalf("первый запрос должен был загрузить ключи ровно один раз, вызовов %d", fetch.Calls())
	}

	// IdP выпустил новый ключ. Пока он не появился в JWKS, запрос с ним
	// обязан быть отклонён, а не принят на честном слове.
	rotated := sign(t, testOtherKey, testOtherKID, jwt.SigningMethodRS256, func(c jwt.MapClaims) {
		c["tenant_id"] = testTenant.String()
	})
	if _, err := o.Principal(requestWith(rotated)); err == nil {
		t.Fatal("токен с незнакомым ключом принят — это подделка")
	}

	// Ключ опубликован, и токен обязан заработать — но не мгновенно: kid,
	// которого не нашлось, лежит в отрицательном кэше на JWKSNegativeTTL.
	// Без него перебор неизвестных kid превращается в походы к IdP, что и
	// проверяется отдельно в TestOIDCОтрицательныйКэшKid.
	//
	// Тест изначально ждал успеха немедленно: он писался до появления
	// отрицательного кэша и противоречил более позднему контракту. Контракт
	// новее и строже, поэтому сдвигаем часы, а не ослабляем проверку.
	clock.now = start.Add(2 * time.Minute)
	fetch.SetKeys(bothKeys())
	if _, err := o.Principal(requestWith(rotated)); err != nil {
		t.Fatalf("после публикации ключа токен обязан приниматься: %v", err)
	}
	if fetch.Calls() < 2 {
		t.Errorf("JWKS перечитан %d раз, ожидалось >= 2 при неизвестном kid", fetch.Calls())
	}
}

// TestOIDCC rightsКэшОграниченСроком: срок кэша прав (критерий MVP §9)
// задаёт окно, в течение которого разбор токена не переспрашивает JWKS.
// После истечения срока ключи перечитываются заново.
func TestOIDCCКэшОграниченСроком(t *testing.T) {
	fetch := &countingFetcher{keys: bothKeys()}
	start := time.Now()
	clock := &fixedClock{now: start}
	o := newTestOIDC(t, fetch, clock)

	token := sign(t, testKey, testKeyID, jwt.SigningMethodRS256, func(c jwt.MapClaims) {
		c["tenant_id"] = testTenant.String()
		c["roles"] = []string{"admin"}
	})

	if _, err := o.Principal(requestWith(token)); err != nil {
		t.Fatalf("первый запрос: %v", err)
	}
	afterFirst := fetch.Calls()

	if _, err := o.Principal(requestWith(token)); err != nil {
		t.Fatalf("второй запрос: %v", err)
	}
	if fetch.Calls() != afterFirst {
		t.Errorf("внутри срока кэша JWKS перечитан (%d -> %d)", afterFirst, fetch.Calls())
	}

	clock.now = start.Add(6 * time.Minute)
	if _, err := o.Principal(requestWith(token)); err != nil {
		t.Fatalf("запрос после срока кэша: %v", err)
	}
	if fetch.Calls() == afterFirst {
		t.Errorf("после истечения срока кэша JWKS обязан быть перечитан, вызовов осталось %d", fetch.Calls())
	}
}

// TestOIDCCДругойТокенНеБерётсяИзКэша: два разных токена не должны
// получать права друг друга из одного кэша.
func TestOIDCCДругойТокенНеБерётсяИзКэша(t *testing.T) {
	fetch := &countingFetcher{keys: bothKeys()}
	o := newTestOIDC(t, fetch, &fixedClock{now: time.Now()})

	tokenFor := func(sub string, roles []string) string {
		return sign(t, testKey, testKeyID, jwt.SigningMethodRS256, func(c jwt.MapClaims) {
			c["sub"] = sub
			c["tenant_id"] = testTenant.String()
			c["roles"] = roles
		})
	}
	if _, err := o.Principal(requestWith(tokenFor("anna", []string{"admin"}))); err != nil {
		t.Fatalf("anna: %v", err)
	}
	// У boris свои права; кэш не должен отдать ему права anna.
	p, err := o.Principal(requestWith(tokenFor("boris", nil)))
	if err != nil {
		t.Fatalf("boris: %v", err)
	}
	if p.UserID != "boris" {
		t.Errorf("UserID = %q, ожидался boris", p.UserID)
	}
	if p.HasRole("admin") {
		t.Errorf("boris получил права anna из кэша: %v", p.Roles)
	}
}

// unsignedNoneToken собирает alg=none — подпись не проверяется вовсе.
func unsignedNoneToken(t *testing.T) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT","kid":"` + testKeyID + `"}`))
	claims := jwt.MapClaims{
		"sub":       "anna",
		"iss":       testIssuer,
		"aud":       testAudience,
		"exp":       time.Now().Add(time.Hour).Unix(),
		"tenant_id": testTenant.String(),
		"roles":     []string{"admin"},
	}
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("claims: %v", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(body)
	return header + "." + payload + "."
}

// hs256Token — классическая атака подмены алгоритма: токен подписан
// симметричным ключом вместо асимметричного. Настоящая атака использует
// публичный ключ IdP как секрет, но проверяется ровно то же самое —
// несовпадение алгоритма обязано отбиваться до подбора ключа.
func hs256Token(t *testing.T) string {
	t.Helper()
	claims := jwt.MapClaims{
		"sub":       "anna",
		"iss":       testIssuer,
		"aud":       testAudience,
		"exp":       time.Now().Add(time.Hour).Unix(),
		"tenant_id": testTenant.String(),
		"roles":     []string{"admin"},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tok.Header["kid"] = testKeyID
	signed, err := tok.SignedString([]byte("подставной-секрет"))
	if err != nil {
		t.Fatalf("HS256: %v", err)
	}
	return signed
}

// TestOIDCНулевойTenantНеПроходитАутентификацию: нулевой UUID разбирается
// uuid.Parse без ошибки, поэтому проверка «tenant читается» его не ловит.
//
// Это не формальность: tenant авторизатора в режиме OIDC тоже был нулевым,
// и токен без tenant сравнивался с ним как равный, получая администратора на
// всю базу знаний. Дефект найден аудитом безопасности как C-1.
func TestOIDCНулевойTenantНеПроходитАутентификацию(t *testing.T) {
	fetch := &countingFetcher{keys: bothKeys()}
	o := newTestOIDC(t, fetch, &fixedClock{now: time.Now()})

	token := sign(t, testKey, testKeyID, jwt.SigningMethodRS256, func(c jwt.MapClaims) {
		c["tenant_id"] = uuid.Nil.String()
		c["roles"] = []string{"admin"}
	})
	p, err := o.Principal(requestWith(token))
	if err == nil {
		t.Fatalf("токен с нулевым tenant принят, Principal = %+v", p)
	}
	appErr, ok := domain.AsAppError(err)
	if !ok || appErr.HTTPStatus() != http.StatusUnauthorized {
		t.Errorf("ожидался отказ 401, получено %#v", err)
	}
	if p.TenantID != uuid.Nil || p.UserID != "" {
		t.Errorf("при отказе Principal не должен быть заполнен: %+v", p)
	}
}

// TestTenantRBACНеПутаетНулевойTenantСАдмином: авторизатор режима OIDC
// обязан отличать «tenant неизвестен» от «tenant мой и я администратор».
func TestTenantRBACНеПутаетНулевойTenantСАдмином(t *testing.T) {
	authorizer := NewTenantRBAC()
	realTenant := uuid.MustParse("22222222-2222-2222-2222-222222222222")

	cases := []struct {
		name       string
		principal  domain.Principal
		wantDenied bool
	}{
		{
			name:       "нулевой tenant с ролью администратора",
			principal:  domain.Principal{TenantID: uuid.Nil, UserID: "anna", Roles: []string{"admin"}},
			wantDenied: true,
		},
		{
			name:      "свой tenant и роль администратора",
			principal: domain.Principal{TenantID: realTenant, UserID: "bob", Roles: []string{"admin"}},
		},
		{
			name:       "свой tenant без роли",
			principal:  domain.Principal{TenantID: realTenant, UserID: "carol", Roles: []string{"reader"}},
			wantDenied: true,
		},
		{
			name:       "свой tenant вообще без ролей",
			principal:  domain.Principal{TenantID: realTenant, UserID: "dave"},
			wantDenied: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := authorizer.RequireAdmin(tc.principal)
			if tc.wantDenied && err == nil {
				t.Error("ожидался отказ, а права администратора выданы")
			}
			if !tc.wantDenied && err != nil {
				t.Errorf("ожидался пропуск, получено: %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// H-1. Кэш прав не должен продлевать жизнь токена и обходить проверки.
// ---------------------------------------------------------------------------

// TestOIDCИстёкшийТокенНеПереживаетКэшПрав: токен с exp = T+60s, пришедший
// повторно в T+90s, обязан быть отвергнут.
//
// Дефект H-1: кэш прав продлевал срок токена до конца RightsTTL, то есть на
// 5 минут дольше, чем IdP разрешил. Слова «сам срок действия проверяется на
// каждом запросе» в комментарии описывали защиту, которой не было.
func TestOIDCИстёкшийТокенНеПереживаетКэшПрав(t *testing.T) {
	fetch := &countingFetcher{keys: bothKeys()}
	start := time.Now()
	clock := &fixedClock{now: start}
	o := newTestOIDC(t, fetch, clock)

	// TTL кэша прав — 5 минут, срок токена — 60 секунд.
	token := sign(t, testKey, testKeyID, jwt.SigningMethodRS256, func(c jwt.MapClaims) {
		c["tenant_id"] = testTenant.String()
		c["exp"] = start.Add(time.Minute).Unix()
	})

	if _, err := o.Principal(requestWith(token)); err != nil {
		t.Fatalf("на свежем токене ожидался пропуск: %v", err)
	}

	clock.now = start.Add(90 * time.Second)
	p, err := o.Principal(requestWith(token))
	if err == nil {
		t.Fatalf("истёкший токен принят через 90с при сроке 60с, Principal = %+v", p)
	}
	appErr, ok := domain.AsAppError(err)
	if !ok || appErr.HTTPStatus() != http.StatusUnauthorized {
		t.Errorf("ожидался отказ 401, получено %#v", err)
	}
}

// TestOIDCКэшПравНеДлиннееТокена: таблица границ. Срок записи в кэше — это
// min(now+RightsTTL, exp), поэтому ни один токен не переживает собственный срок
// и ни один не кэшируется дольше RightsTTL.
func TestOIDCКэшПравНеДлиннееТокена(t *testing.T) {
	const rightsTTL = 5 * time.Minute

	cases := []struct {
		name string
		// сколько живёт токен относительно момента первого запроса
		tokenLifetime time.Duration
		// на сколько позже приходит второй запрос
		laterBy time.Duration
		wantErr bool
	}{
		{
			name:          "токен живёт дольше срока кэша — на границе кэша ещё жив",
			tokenLifetime: time.Hour,
			laterBy:       rightsTTL - time.Second,
		},
		{
			name:          "токен живёт дольше срока кэша — за границей кэша rights ещё действует",
			tokenLifetime: time.Hour,
			laterBy:       rightsTTL + time.Second,
		},
		{
			name:          "токен живёт меньше срока кэша — на своей границе ещё жив",
			tokenLifetime: 2 * time.Minute,
			laterBy:       2*time.Minute - time.Second,
		},
		{
			// Ключевой случай дефекта: TTL кэша в 2.5 раза больше срока
			// токена, но токен всё равно обязан умереть вовремя.
			name:          "токен короче срока кэша — на своей границе уже мёртв",
			tokenLifetime: 2 * time.Minute,
			laterBy:       2*time.Minute + time.Second,
			wantErr:       true,
		},
		{
			name:          "токен живёт ровно до RightsTTL",
			tokenLifetime: rightsTTL,
			laterBy:       rightsTTL + time.Second,
			wantErr:       true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fetch := &countingFetcher{keys: bothKeys()}
			start := time.Now()
			clock := &fixedClock{now: start}
			o := newTestOIDC(t, fetch, clock)

			token := sign(t, testKey, testKeyID, jwt.SigningMethodRS256, func(c jwt.MapClaims) {
				c["tenant_id"] = testTenant.String()
				c["exp"] = start.Add(tc.tokenLifetime).Unix()
			})
			if _, err := o.Principal(requestWith(token)); err != nil {
				t.Fatalf("на свежем токене ожидался пропуск: %v", err)
			}

			clock.now = start.Add(tc.laterBy)
			p, err := o.Principal(requestWith(token))
			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("ожидался отказ, а Principal = %+v", p)
			case !tc.wantErr && err != nil:
				t.Fatalf("ожидался пропуск, получено: %v", err)
			case err != nil:
				appErr, ok := domain.AsAppError(err)
				if !ok || appErr.HTTPStatus() != http.StatusUnauthorized {
					t.Errorf("ожидался отказ 401, получено %#v", err)
				}
			}
		})
	}
}

// TestOIDCОтозванныйКлючПрекращаетОбслуживатьКэш: если ключ, которым подписан
// токен, исчез из JWKS, запись кэша прав обязана перестать выдаваться.
//
// Это окно инцидента «ключ утёк»: пока запись жива, подпись вообще не
// проверяется, и отозвать подпись перечитыванием JWKS невозможно.
func TestOIDCОтозванныйКлючПрекращаетОбслуживатьКэш(t *testing.T) {
	fetch := &countingFetcher{keys: bothKeys()}
	start := time.Now()
	clock := &fixedClock{now: start}
	o := newTestOIDCWith(t, fetch, clock, func(opts *OIDCOptions) {
		// Права в кэше живут час, а JWKS обновляется раз в минуту: так
		// проверяется именно ключ, а не истечение срока прав.
		opts.RightsTTL = time.Hour
		opts.KeysTTL = time.Minute
	})

	token := sign(t, testKey, testKeyID, jwt.SigningMethodRS256, func(c jwt.MapClaims) {
		c["tenant_id"] = testTenant.String()
		c["exp"] = start.Add(30 * time.Minute).Unix()
	})
	if _, err := o.Principal(requestWith(token)); err != nil {
		t.Fatalf("первый запрос: %v", err)
	}

	// IdP отозвал ключ: в JWKS остался только второй.
	fetch.SetKeys(map[string]*rsa.PublicKey{testOtherKID: &testOtherKey.PublicKey})
	clock.now = start.Add(2 * time.Minute)

	p, err := o.Principal(requestWith(token))
	if err == nil {
		t.Fatalf("токен отозванного ключа принят из кэша прав, Principal = %+v", p)
	}
	if p.TenantID != uuid.Nil {
		t.Errorf("при отказе Principal не должен быть заполнен: %+v", p)
	}
}

// ---------------------------------------------------------------------------
// H-2. Неизвестный kid не должен превращаться в DoS против IdP.
// ---------------------------------------------------------------------------

// TestOIDCНеизвестныйKidНеАмплифицируетJWKS — главный тест дефекта H-2.
//
// keyfunc в golang-jwt вызывается ДО проверки подписи, поэтому неизвестный
// kid раньше приводил к полному HTTP GET к JWKS на каждый запрос. Валидный
// токен для этого не нужен: достаточно alg=RS256 и случайного kid.
func TestOIDCНеизвестныйKidНеАмплифицируетJWKS(t *testing.T) {
	fetch := &countingFetcher{keys: bothKeys()}
	clock := &fixedClock{now: time.Now()}
	o := newTestOIDC(t, fetch, clock)

	const requests = 100
	for i := range requests {
		token := sign(t, testKey, fmt.Sprintf("kid-%d", i), jwt.SigningMethodRS256, func(c jwt.MapClaims) {
			c["tenant_id"] = testTenant.String()
		})
		if _, err := o.Principal(requestWith(token)); err == nil {
			t.Fatalf("токен с неизвестным kid %d принят — это подделка", i)
		}
	}

	if got := fetch.Calls(); got > 2 {
		t.Errorf("JWKS запрошен %d раз на %d запросов с неизвестными kid: один незнакомый kid "+
			"раздувается в походы к IdP (ожидались единицы). Предел — не зависящий от запроса "+
			"интервал перечитывания", got, requests)
	} else {
		t.Logf("JWKS запрошен %d раз на %d запросов с неизвестными kid", got, requests)
	}
}

// TestOIDCПараллельныеЗапросыНеДолбятJWKS: защита от stampede. N одновременных
// запросов с неизвестным kid обязаны схлопнуться в единичное обращение к IdP.
func TestOIDCПараллельныеЗапросыНеДолбятJWKS(t *testing.T) {
	fetch := &countingFetcher{keys: bothKeys()}
	clock := &fixedClock{now: time.Now()}
	o := newTestOIDC(t, fetch, clock)

	unknown := sign(t, testKey, "kid-которого-нет", jwt.SigningMethodRS256, func(c jwt.MapClaims) {
		c["tenant_id"] = testTenant.String()
	})

	const workers = 64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := o.Principal(requestWith(unknown)); err == nil {
				t.Error("токен с неизвестным kid принят")
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := fetch.Calls(); got > 2 {
		t.Errorf("JWKS запрошен %d раз при %d параллельных запросах: параллельный "+
			"перебор низает IdP ровно столько, сколько его послали", got, workers)
	} else {
		t.Logf("JWKS запрошен %d раз при %d параллельных запросах", got, workers)
	}
}

// TestOIDCОтрицательныйКэшKid: ключ, который только что не нашёлся, не должен
// снова инициировать поход в IdP. Интервал перечитывания здесь осознанно
// мал, а отрицательный кэш — большой, чтобы проверить именно его.
func TestOIDCОтрицательныйКэшKid(t *testing.T) {
	fetch := &countingFetcher{keys: bothKeys()}
	start := time.Now()
	clock := &fixedClock{now: start}
	o := newTestOIDCWith(t, fetch, clock, func(opts *OIDCOptions) {
		opts.JWKSRefreshInterval = time.Second
		opts.JWKSNegativeTTL = time.Minute
	})

	unknown := sign(t, testKey, "kid-которого-нет", jwt.SigningMethodRS256, func(c jwt.MapClaims) {
		c["tenant_id"] = testTenant.String()
	})
	if _, err := o.Principal(requestWith(unknown)); err == nil {
		t.Fatal("токен с неизвестным kid принят")
	}
	afterFirst := fetch.Calls()

	// Лимит частоты уже не мешает (прошло 5с при интервале 1с), но
	// отрицательный кэш живёт минуту.
	clock.now = start.Add(5 * time.Second)
	if _, err := o.Principal(requestWith(unknown)); err == nil {
		t.Fatal("токен с неизвестным kid принят")
	}
	if got := fetch.Calls(); got != afterFirst {
		t.Errorf("отрицательный кэш kid не сработал: JWKS запрошен %d раз вместо %d", got, afterFirst)
	}

	// Отрицательный кэш истёк — перечитывание снова разрешено.
	clock.now = start.Add(2 * time.Minute)
	if _, err := o.Principal(requestWith(unknown)); err == nil {
		t.Fatal("токен с неизвестным kid принят")
	}
	if got := fetch.Calls(); got <= afterFirst {
		t.Errorf("после истечения отрицательного кэша JWKS обязан быть перечитан, вызовов %d", got)
	}
}

// TestOIDCЛимитЧастотыJWKSНеЗависитОтЗапроса: интервал перечитывания — главная
// защита, и она измеряется временем, а не числом запросов.
func TestOIDCЛимитЧастотыJWKSНеЗависитОтЗапроса(t *testing.T) {
	fetch := &countingFetcher{keys: bothKeys()}
	start := time.Now()
	clock := &fixedClock{now: start}
	o := newTestOIDCWith(t, fetch, clock, func(opts *OIDCOptions) {
		opts.JWKSRefreshInterval = time.Minute
		// Отрицательный кэш выключен, чтобы измерялся именно интервал.
		opts.JWKSNegativeTTL = time.Nanosecond
	})

	tokenFor := func(kid string) string {
		return sign(t, testKey, kid, jwt.SigningMethodRS256, func(c jwt.MapClaims) {
			c["tenant_id"] = testTenant.String()
		})
	}
	if _, err := o.Principal(requestWith(tokenFor("kid-a"))); err == nil {
		t.Fatal("токен с неизвестным kid принят")
	}
	afterFirst := fetch.Calls()
	if afterFirst != 1 {
		t.Fatalf("первое обращение к JWKS не состоялось, вызовов %d", afterFirst)
	}

	// Десять разных неизвестных kid за половину интервала.
	clock.now = start.Add(30 * time.Second)
	for i := range 10 {
		if _, err := o.Principal(requestWith(tokenFor(fmt.Sprintf("kid-b-%d", i)))); err == nil {
			t.Fatalf("токен с неизвестным kid %d принят", i)
		}
	}
	if got := fetch.Calls(); got != afterFirst {
		t.Errorf("лимит частоты не сработал: JWKS запрошен %d раз, ожидался %d", got, afterFirst)
	}

	// Интервал истёк — одно перечитывание, не десять.
	clock.now = start.Add(2 * time.Minute)
	if _, err := o.Principal(requestWith(tokenFor("kid-c"))); err == nil {
		t.Fatal("токен с неизвестным kid принят")
	}
	if got := fetch.Calls(); got != afterFirst+1 {
		t.Errorf("после интервала ожидалось ровно одно перечитывание, вызовов %d (было %d)", got, afterFirst)
	}
}

// TestOIDCНедоступныйJWKSДаёт503 — автомат против битья по мёртвому IdP.
//
// Проверяется и вторая половина требования: 503 — честный отказ в доступе
// (fail-closed), а не 500 «что-то сломалось» и не 401 «ваш токен неверен».
func TestOIDCНедоступныйJWKSДаёт503(t *testing.T) {
	fetch := &failingFetcher{err: errors.New("connection refused")}
	start := time.Now()
	clock := &fixedClock{now: start}
	o := newTestOIDCWith(t, fetch, clock, func(opts *OIDCOptions) {
		opts.JWKSRefreshInterval = time.Second
		opts.JWKSFailuresBeforeOpen = 3
	})

	unknown := sign(t, testKey, "kid-которого-нет", jwt.SigningMethodRS256, func(c jwt.MapClaims) {
		c["tenant_id"] = testTenant.String()
	})

	// Первые попытки честно не находят ключ: 401 «токен не прошёл проверку».
	for i := range 3 {
		clock.now = start.Add(time.Duration(i+1) * 10 * time.Second)
		if _, err := o.Principal(requestWith(unknown)); err == nil {
			t.Fatalf("попытка %d: токен с неизвестным kid принят", i)
		}
	}
	if got := fetch.Calls(); got != 3 {
		t.Fatalf("до размыкания автомата ожидались 3 обращения к IdP, было %d", got)
	}

	// Автомат разомкнут: к IdP больше не идём, отвечаем 503.
	clock.now = start.Add(40 * time.Second)
	p, err := o.Principal(requestWith(unknown))
	if err == nil {
		t.Fatalf("при недоступном IdP ожидался отказ, а Principal = %+v", p)
	}
	appErr, ok := domain.AsAppError(err)
	if !ok {
		t.Fatalf("ошибка не доменная: %#v", err)
	}
	if appErr.HTTPStatus() != http.StatusServiceUnavailable {
		t.Errorf("статус = %d, ожидался 503 Service Unavailable", appErr.HTTPStatus())
	}
	if p.TenantID != uuid.Nil || p.UserID != "" {
		t.Errorf("fail-closed нарушен: при отказе Principal заполнен: %+v", p)
	}
	callsAtOpen := fetch.Calls()

	// Пока автомат разомкнут, походов к IdP быть не должно.
	for i := range 5 {
		clock.now = start.Add(time.Duration(41+i) * time.Second)
		if _, err := o.Principal(requestWith(unknown)); err == nil {
			t.Fatal("при недоступном IdP ожидался отказ")
		}
	}
	if got := fetch.Calls(); got != callsAtOpen {
		t.Errorf("при разомкнутом автомате IdP продолжал получать запросы: %d -> %d", callsAtOpen, got)
	}
}

// TestOIDCНедоступныйJWKSНеЛомаетВалидныеТокены: автомат не должен выкидывать
// пользователей, чей ключ у нас уже есть в кэше.
func TestOIDCНедоступныйJWKSНеЛомаетВалидныеТокены(t *testing.T) {
	fetch := &failingFetcher{
		err:          errors.New("connection refused"),
		succeedFirst: 1,
		keys:         bothKeys(),
	}
	start := time.Now()
	clock := &fixedClock{now: start}
	o := newTestOIDCWith(t, fetch, clock, func(opts *OIDCOptions) {
		opts.JWKSRefreshInterval = time.Second
		opts.JWKSFailuresBeforeOpen = 2
	})

	token := sign(t, testKey, testKeyID, jwt.SigningMethodRS256, func(c jwt.MapClaims) {
		c["tenant_id"] = testTenant.String()
		c["exp"] = start.Add(time.Hour).Unix()
	})
	if _, err := o.Principal(requestWith(token)); err != nil {
		t.Fatalf("первый запрос: %v", err)
	}

	// Разгоняем автомат неудачами по неизвестным kid.
	for i := range 3 {
		clock.now = start.Add(time.Duration(i+1) * 10 * time.Second)
		unknown := sign(t, testKey, fmt.Sprintf("kid-%d", i), jwt.SigningMethodRS256, func(c jwt.MapClaims) {
			c["tenant_id"] = testTenant.String()
		})
		_, _ = o.Principal(requestWith(unknown))
	}

	// Ключ валидного токена у нас в кэше — работать он обязан.
	clock.now = start.Add(31 * time.Second)
	p, err := o.Principal(requestWith(token))
	if err != nil {
		t.Fatalf("валидный токен отвергнут из-за недоступности IdP: %v", err)
	}
	if p.TenantID != testTenant {
		t.Errorf("TenantID = %v, ожидался %v", p.TenantID, testTenant)
	}
}

// ---------------------------------------------------------------------------
// H-3. Срок ключей JWKS и срок прав — разные параметры.
// ---------------------------------------------------------------------------

// TestOIDCTTLКлючейОтделенОтTTLПрав: два окна настраиваются независимо.
// Старое поведение (один RightsTTL на оба) делало невозможной настройку:
// уменьшение окна отзыва подписи требовало бы увеличить частоту разбора токенов.
func TestOIDCTTLКлючейОтделенОтTTLПрав(t *testing.T) {
	cases := []struct {
		name      string
		rightsTTL time.Duration
		keysTTL   time.Duration
		// момент второго запроса
		laterBy time.Duration
		// сколько обращений к JWKS ожидается к этому моменту
		wantCalls int
	}{
		{
			// Права в кэше ещё живы, но срок ключей истёк: ключи обязаны
			// перечитаться. Срок прав на это не влияет.
			name:      "истёк KeysTTL, RightsTTL ещё жив",
			rightsTTL: time.Hour,
			keysTTL:   time.Minute,
			laterBy:   2 * time.Minute,
			wantCalls: 2,
		},
		{
			// Истёк срок прав, ключи ещё свежие: перечитывания быть не должно.
			// Старый код здесь ходил в IdP, потому что оба окна были одним
			// параметром.
			name:      "истёк RightsTTL, KeysTTL ещё жив",
			rightsTTL: time.Minute,
			keysTTL:   time.Hour,
			laterBy:   2 * time.Minute,
			wantCalls: 1,
		},
		{
			name:      "оба окна живы",
			rightsTTL: time.Hour,
			keysTTL:   time.Hour,
			laterBy:   2 * time.Minute,
			wantCalls: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fetch := &countingFetcher{keys: bothKeys()}
			start := time.Now()
			clock := &fixedClock{now: start}
			o := newTestOIDCWith(t, fetch, clock, func(opts *OIDCOptions) {
				opts.RightsTTL = tc.rightsTTL
				opts.KeysTTL = tc.keysTTL
			})

			token := sign(t, testKey, testKeyID, jwt.SigningMethodRS256, func(c jwt.MapClaims) {
				c["tenant_id"] = testTenant.String()
				c["exp"] = start.Add(24 * time.Hour).Unix()
			})
			if _, err := o.Principal(requestWith(token)); err != nil {
				t.Fatalf("первый запрос: %v", err)
			}
			clock.now = start.Add(tc.laterBy)
			if _, err := o.Principal(requestWith(token)); err != nil {
				t.Fatalf("второй запрос: %v", err)
			}
			if got := fetch.Calls(); got != tc.wantCalls {
				t.Errorf("обращений к JWKS = %d, ожидалось %d (RightsTTL=%s, KeysTTL=%s)",
					got, tc.wantCalls, tc.rightsTTL, tc.keysTTL)
			}
		})
	}
}

// TestOIDCДефолтыTTLOтдельны: KeysTTL обязан иметь собственный дефолт, иначе
// вызывающий (app/auth_health.go) задаёт только RightsTTL и получает обратно
// старую связку «одно окно на оба».
func TestOIDCДефолтыTTLOтдельны(t *testing.T) {
	o := newTestOIDCWith(t, &countingFetcher{keys: bothKeys()}, &fixedClock{now: time.Now()}, nil)
	if o.opts.KeysTTL <= 0 {
		t.Error("KeysTTL без заданного значения обязан получить разумный дефолт")
	}
	if o.opts.JWKSRefreshInterval <= 0 {
		t.Error("JWKSRefreshInterval без заданного значения обязан получить разумный дефолт")
	}
	if o.opts.JWKSNegativeTTL <= 0 {
		t.Error("JWKSNegativeTTL без заданного значения обязан получить разумный дефолт")
	}
	if o.opts.JWKSFailuresBeforeOpen <= 0 {
		t.Error("JWKSFailuresBeforeOpen без заданного значения обязан получить разумный дефолт")
	}
	if o.opts.RightsCacheSize <= 0 {
		t.Error("RightsCacheSize без заданного значения обязан получить разумный дефолт")
	}
}

// ---------------------------------------------------------------------------
// M-1. Кэш прав обязан быть ограничен и не хранить сырые токены.
// ---------------------------------------------------------------------------

// TestOIDCКэшПравОграниченПоРазмеру: карта не должна расти бесконечно.
// Ключом служит sha256 токена, но это не мешает карте расти по числу записей,
// если никто не вытесняет.
func TestOIDCКэшПравОграниченПоРазмеру(t *testing.T) {
	const cacheSize = 8
	fetch := &countingFetcher{keys: bothKeys()}
	clock := &fixedClock{now: time.Now()}
	o := newTestOIDCWith(t, fetch, clock, func(opts *OIDCOptions) {
		opts.RightsCacheSize = cacheSize
	})

	for i := range 50 {
		token := sign(t, testKey, testKeyID, jwt.SigningMethodRS256, func(c jwt.MapClaims) {
			c["sub"] = fmt.Sprintf("user-%d", i)
			c["tenant_id"] = testTenant.String()
		})
		if _, err := o.Principal(requestWith(token)); err != nil {
			t.Fatalf("токен %d: %v", i, err)
		}
	}

	if got := o.rightsLen(); got > cacheSize {
		t.Errorf("в кэше прав %d записей при лимите %d: карта растёт без границ", got, cacheSize)
	} else {
		t.Logf("после 50 различных токенов в кэше прав %d записей при лимите %d", got, cacheSize)
	}

	// И на длинной жизни процесса размер не должен уехать.
	clock.now = clock.now.Add(24 * time.Hour)
	for i := range 50 {
		token := sign(t, testKey, testKeyID, jwt.SigningMethodRS256, func(c jwt.MapClaims) {
			c["sub"] = fmt.Sprintf("user-later-%d", i)
			c["tenant_id"] = testTenant.String()
			// exp обязан считаться от сдвинутых часов проверки: подпись ниже
			// ставит настоящий exp, а через 24 часа он уже в прошлом и токен
			// просрочен по-настоящему. Тест проверяет размер кэша, а не срок.
			c["exp"] = clock.now.Add(time.Hour).Unix()
		})
		if _, err := o.Principal(requestWith(token)); err != nil {
			t.Fatalf("токен later-%d: %v", i, err)
		}
	}
	if got := o.rightsLen(); got > cacheSize {
		t.Errorf("через сутки в кэше прав %d записей при лимите %d", got, cacheSize)
	}
}

// TestOIDCКэшПравХранитХешАНеТокен: в куче не должно лежать сырых токенов —
// они попадут в core dump и в heap profile, а это готовый словарь bearer-токенов.
func TestOIDCКэшПравХранитХешАНеТокен(t *testing.T) {
	fetch := &countingFetcher{keys: bothKeys()}
	clock := &fixedClock{now: time.Now()}
	o := newTestOIDC(t, fetch, clock)

	token := sign(t, testKey, testKeyID, jwt.SigningMethodRS256, func(c jwt.MapClaims) {
		c["tenant_id"] = testTenant.String()
	})
	if _, err := o.Principal(requestWith(token)); err != nil {
		t.Fatalf("первый запрос: %v", err)
	}

	want := sha256.Sum256([]byte(token))
	got := o.rightsKeys()
	if len(got) != 1 {
		t.Fatalf("в кэше прав %d записей, ожидалась 1", len(got))
	}
	if got[0] != want {
		t.Errorf("ключ кэша прав = %x, ожидался sha256 токена %x", got[0], want)
	}
	if fmt.Sprintf("%T", got[0]) != fmt.Sprintf("%T", want) {
		t.Errorf("тип ключа кэша прав %s, ожидался %s: строковый ключ означал бы сырой токен в куче",
			fmt.Sprintf("%T", got[0]), fmt.Sprintf("%T", want))
	}
}

// TestOIDCКэшПравЧиститИстёкшие: при превышении порога сначала выбрасываются
// мёртвые записи — они всё равно нечитаемы, и это дешевле, чем вытеснять живые.
func TestOIDCКэшПравЧиститИстёкшие(t *testing.T) {
	const cacheSize = 64
	fetch := &countingFetcher{keys: bothKeys()}
	start := time.Now()
	clock := &fixedClock{now: start}
	o := newTestOIDCWith(t, fetch, clock, func(opts *OIDCOptions) {
		opts.RightsCacheSize = cacheSize
		opts.RightsTTL = time.Minute
	})

	for i := range 50 {
		token := sign(t, testKey, testKeyID, jwt.SigningMethodRS256, func(c jwt.MapClaims) {
			c["sub"] = fmt.Sprintf("user-%d", i)
			c["tenant_id"] = testTenant.String()
			c["exp"] = start.Add(24 * time.Hour).Unix()
		})
		if _, err := o.Principal(requestWith(token)); err != nil {
			t.Fatalf("токен %d: %v", i, err)
		}
	}
	if got := o.rightsLen(); got != 50 {
		t.Fatalf("до сдвига часов в кэше %d записей, ожидалось 50", got)
	}

	// Все 50 записей истёкли по RightsTTL, превысить лимит нечем.
	clock.now = start.Add(2 * time.Minute)
	token := sign(t, testKey, testKeyID, jwt.SigningMethodRS256, func(c jwt.MapClaims) {
		c["sub"] = "свежий"
		c["tenant_id"] = testTenant.String()
		c["exp"] = start.Add(24 * time.Hour).Unix()
	})
	if _, err := o.Principal(requestWith(token)); err != nil {
		t.Fatalf("свежий токен: %v", err)
	}
	if got := o.rightsLen(); got != 1 {
		t.Errorf("после чистки истёкших в кэше %d записей, ожидалась 1 — вытеснять надо было мёртвые", got)
	}
}
