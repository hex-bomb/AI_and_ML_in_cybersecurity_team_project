package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/kbsearch/kb/internal/envelope"
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
type countingFetcher struct {
	keys  map[string]*rsa.PublicKey
	calls int
}

func (f *countingFetcher) Fetch(context.Context) (map[string]*rsa.PublicKey, error) {
	f.calls++
	return f.keys, nil
}

func newTestOIDC(t *testing.T, fetch *countingFetcher, clock ports.Clock) *OIDC {
	t.Helper()
	o, err := NewOIDC(OIDCOptions{
		Issuer:      testIssuer,
		Audience:    testAudience,
		JWKS:        fetch,
		Clock:       clock,
		TenantClaim: "tenant_id",
		RolesClaim:  "roles",
		GroupsClaim: "groups",
		RightsTTL:   5 * time.Minute,
	})
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
	o := newTestOIDC(t, fetch, &fixedClock{now: time.Now()})

	known := sign(t, testKey, testKeyID, jwt.SigningMethodRS256, func(c jwt.MapClaims) {
		c["tenant_id"] = testTenant.String()
	})
	if _, err := o.Principal(requestWith(known)); err != nil {
		t.Fatalf("первый запрос: %v", err)
	}
	if fetch.calls != 1 {
		t.Fatalf("первый запрос должен был загрузить ключи ровно один раз, вызовов %d", fetch.calls)
	}

	// IdP выпустил новый ключ. Пока он не появился в JWKS, запрос с ним
	// обязан быть отклонён, а не принят на честном слове.
	rotated := sign(t, testOtherKey, testOtherKID, jwt.SigningMethodRS256, func(c jwt.MapClaims) {
		c["tenant_id"] = testTenant.String()
	})
	if _, err := o.Principal(requestWith(rotated)); err == nil {
		t.Fatal("токен с незнакомым ключом принят — это подделка")
	}

	// Ключ опубликован. Тот же токен обязан заработать: неизвестный kid
	// обязан приводить к перечитыванию JWKS, иначе после ротации ключей
	// всех выкинуло бы из системы.
	fetch.keys = bothKeys()
	if _, err := o.Principal(requestWith(rotated)); err != nil {
		t.Fatalf("после публикации ключа токен обязан приниматься: %v", err)
	}
	if fetch.calls < 2 {
		t.Errorf("JWKS перечитан %d раз, ожидалось >= 2 при неизвестном kid", fetch.calls)
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
	afterFirst := fetch.calls

	if _, err := o.Principal(requestWith(token)); err != nil {
		t.Fatalf("второй запрос: %v", err)
	}
	if fetch.calls != afterFirst {
		t.Errorf("внутри срока кэша JWKS перечитан (%d -> %d)", afterFirst, fetch.calls)
	}

	clock.now = start.Add(6 * time.Minute)
	if _, err := o.Principal(requestWith(token)); err != nil {
		t.Fatalf("запрос после срока кэша: %v", err)
	}
	if fetch.calls == afterFirst {
		t.Errorf("после истечения срока кэша JWKS обязан быть перечитан, вызовов осталось %d", fetch.calls)
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
