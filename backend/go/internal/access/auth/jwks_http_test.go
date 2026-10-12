package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"

	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Имена всех помощников этого файла начинаются с jwks, чтобы не пересечься
// с хелперами oidc_test.go (testKey, sign, requestWith, bothKeys и прочие)
// в общем пакете internal/access/auth.

// Ключи для тестов. Генерируются один раз: rsa.GenerateKey на 2048 бит стоит
// заметного времени, а набор нужен почти каждому тесту.
var (
	jwksKeysOnce sync.Once
	jwksKeys     []*rsa.PrivateKey
)

func jwksTestKeys(t *testing.T) []*rsa.PrivateKey {
	t.Helper()
	jwksKeysOnce.Do(func() {
		for range 3 {
			key, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				panic(fmt.Sprintf("генерация тестового ключа: %v", err))
			}
			jwksKeys = append(jwksKeys, key)
		}
	})
	return jwksKeys
}

// jwksModulus кодирует модуль ключа в base64url, как это делает RFC 7518.
func jwksModulus(key *rsa.PublicKey) string {
	return base64.RawURLEncoding.EncodeToString(key.N.Bytes())
}

// jwksExponent кодирует показатель ключа в base64url.
func jwksExponent(key *rsa.PublicKey) string {
	return base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())
}

// jwksSigEntry собирает корректный ключ подписи — как его отдаёт живой IdP.
func jwksSigEntry(kid string, key *rsa.PrivateKey) jwkKey {
	return jwkKey{
		Kid: kid,
		Kty: "RSA",
		Use: "sig",
		Alg: "RS256",
		N:   jwksModulus(&key.PublicKey),
		E:   jwksExponent(&key.PublicKey),
	}
}

// jwksBody собирает документ JWKS из набора записей.
func jwksBody(t *testing.T, keys ...jwkKey) []byte {
	t.Helper()
	return jwksMarshal(t, map[string]any{"keys": keys})
}

func jwksMarshal(t *testing.T, doc any) []byte {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("сборка тела JWKS: %v", err)
	}
	return raw
}

// jwksServer поднимает локальный сервер с заданным обработчиком.
//
// Сервер слушает 127.0.0.1, поэтому адрес считается loopback и проходит
// проверку схемы без флага: тесты проверяют разбор и транспорт, а не флаг.
func jwksServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// jwksServeOK отдаёт постоянный документ JWKS.
func jwksServeOK(body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}
}

// jwksFetcherFor собирает читателя под локальный сервер.
func jwksFetcherFor(t *testing.T, url string, opts JWKSOptions) *HTTPJWKS {
	t.Helper()
	j, err := NewHTTPJWKSWithOptions(url, opts)
	if err != nil {
		t.Fatalf("NewHTTPJWKSWithOptions(%q): %v", url, err)
	}
	return j
}

// TestJWKSРазбираетНастоящийНаборИОтдаётКлючПоKID: базовый сценарий —
// в наборе несколько ключей, и нужный находится по kid.
func TestJWKSРазбираетНастоящийНаборИОтдаётКлючПоKID(t *testing.T) {
	keys := jwksTestKeys(t)
	body := jwksBody(t,
		jwksSigEntry("key-1", keys[0]),
		jwksSigEntry("key-2", keys[1]),
		jwksSigEntry("key-3", keys[2]),
	)
	srv := jwksServer(t, jwksServeOK(body))

	got, err := jwksFetcherFor(t, srv.URL, JWKSOptions{}).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	for i, kid := range []string{"key-1", "key-2", "key-3"} {
		key, ok := got[kid]
		if !ok {
			t.Fatalf("ключ %q отсутствует в разобранном наборе", kid)
		}
		// Сравниваем с настоящим ключом, а не только факт наличия: иначе
		// тест прошёл бы и с перепутанными местами модулями.
		if key.N.Cmp(keys[i].N) != 0 || key.E != keys[i].E {
			t.Errorf("ключ %q разобран неверно: E=%d", kid, key.E)
		}
	}
}

// TestJWKSОтбрасываетКлючиНеДляПроверкиПодписи: назначение ключа объявлено
// явно, и ключ не для подписи проверять подпись не должен.
//
// Каждый отказ — самостоятельный подслучай: они закрывают разные угрозы, и
// склеенная проверка «хотя бы один отфильтрован» замаскировала бы, какой
// именно фильтр перестал работать.
func TestJWKSОтбрасываетКлючиНеДляПроверкиПодписи(t *testing.T) {
	keys := jwksTestKeys(t)
	good := jwksSigEntry("good", keys[0])

	cases := []struct {
		name   string
		mutate func(jwkKey) jwkKey
	}{
		{
			name:   "не RSA: симметричный ключ (HMAC)",
			mutate: func(k jwkKey) jwkKey { k.Kty = "oct"; return k },
		},
		{
			name:   "не RSA: эллиптическая кривая",
			mutate: func(k jwkKey) jwkKey { k.Kty = "EC"; return k },
		},
		{
			name:   "назначение — шифрование",
			mutate: func(k jwkKey) jwkKey { k.Use = "enc"; return k },
		},
		{
			name:   "алгоритм ECDSA",
			mutate: func(k jwkKey) jwkKey { k.Alg = "ES256"; return k },
		},
		{
			name:   "алгоритм HMAC",
			mutate: func(k jwkKey) jwkKey { k.Alg = "HS256"; return k },
		},
		{
			name:   "алгоритм не существует",
			mutate: func(k jwkKey) jwkKey { k.Alg = "RS1"; return k },
		},
		{
			name: "key_ops без verify",
			mutate: func(k jwkKey) jwkKey {
				k.KeyOps = []string{"encrypt", "wrapKey"}
				return k
			},
		},
		{
			name:   "без kid — опознать ключ нечем",
			mutate: func(k jwkKey) jwkKey { k.Kid = ""; return k },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := tc.mutate(jwksSigEntry("bad", keys[1]))
			srv := jwksServer(t, jwksServeOK(jwksBody(t, good, bad)))

			got, err := jwksFetcherFor(t, srv.URL, JWKSOptions{}).Fetch(context.Background())
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if _, ok := got["bad"]; ok {
				t.Errorf("ключ %q принят, хотя назначение не проверка подписи", tc.name)
			}
			if _, ok := got["good"]; !ok {
				t.Error("корректный ключ рядом с отброшенным не должен теряться")
			}
		})
	}
}

// TestJWKSПринимаетПустоеНазначение: не все IdP пишут use/alg/key_ops.
// Отсутствие поля не должно выкидывать честный ключ — иначе после смены
// поставщика IdP всех выкинуло бы из системы без внятной причины.
func TestJWKSПринимаетПустоеНазначение(t *testing.T) {
	keys := jwksTestKeys(t)
	bare := jwksSigEntry("bare", keys[0])
	bare.Use = ""
	bare.Alg = ""
	bare.KeyOps = nil

	srv := jwksServer(t, jwksServeOK(jwksBody(t, bare)))

	got, err := jwksFetcherFor(t, srv.URL, JWKSOptions{}).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if _, ok := got["bare"]; !ok {
		t.Error("ключ без use/alg/key_ops отброшен, хотя назначение не объявлено")
	}
}

// TestJWKSПринимаетВсеАлгоритмыRSASHA: RS384 и RS512 — тоже RSA-SHA, и их
// публикуют вместе с RS256. Отбрасывать их значит ломать ротацию ключей у IdP,
// который перешёл на другой хеш.
func TestJWKSПринимаетВсеАлгоритмыRSASHA(t *testing.T) {
	keys := jwksTestKeys(t)

	for _, alg := range []string{"RS256", "RS384", "RS512"} {
		t.Run(alg, func(t *testing.T) {
			entry := jwksSigEntry("k", keys[0])
			entry.Alg = alg
			srv := jwksServer(t, jwksServeOK(jwksBody(t, entry)))

			got, err := jwksFetcherFor(t, srv.URL, JWKSOptions{}).Fetch(context.Background())
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if _, ok := got["k"]; !ok {
				t.Errorf("алгоритм %s отброшен, хотя это RSA-SHA", alg)
			}
		})
	}
}

// TestJWKSНе200ДаётОшибку: любой статус кроме 200 — это отказ. Иначе тело
// ошибки разбиралось бы как пустой набор ключей и приводило к тихому 401 у
// всех, пока на самом деле IdP просто перегружен.
func TestJWKSНе200ДаётОшибку(t *testing.T) {
	for _, status := range []int{
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusServiceUnavailable,
	} {
		t.Run(fmt.Sprintf("статус %d", status), func(t *testing.T) {
			keys := jwksTestKeys(t)
			srv := jwksServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				// Тело с ключами при ошибочном статусе: его нельзя принимать
				// как доверенный набор, иначе статус перестаёт что-либо значить.
				_, _ = w.Write(jwksBody(t, jwksSigEntry("key-1", keys[0])))
			})

			got, err := jwksFetcherFor(t, srv.URL, JWKSOptions{}).Fetch(context.Background())
			if err == nil {
				t.Fatalf("статус %d принят, ключи разобраны: %v", status, got)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("%d", status)) {
				t.Errorf("в ошибке нет статуса: %v", err)
			}
		})
	}
}

// TestJWKSБитыйJSONДаётОшибку: нечитаемый ответ — ошибка разбора, а не пустой
// набор. Разница видна оператору: «IdP вернул мусор» и «у IdP нет ключей» —
// разные причины, и обе лечатся по-разному.
func TestJWKSБитыйJSONДаётОшибку(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "не JSON вовсе", body: "<html>proxy error</html>"},
		{name: "обрезанный объект", body: `{"keys":[{"kid":"key-1"`},
		{name: "ключи не массив", body: `{"keys":{"kid":"key-1"}}`},
		{name: "мусор после объекта", body: `{"keys":[]} и ещё мусор`},
		{name: "пустое тело", body: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := jwksServer(t, jwksServeOK([]byte(tc.body)))

			got, err := jwksFetcherFor(t, srv.URL, JWKSOptions{}).Fetch(context.Background())
			if err == nil {
				t.Fatalf("битый JSON принят, ключи: %v", got)
			}
			if !strings.Contains(err.Error(), "разбор JWKS") {
				t.Errorf("ожидалась ошибка разбора, получено: %v", err)
			}
		})
	}
}

// TestJWKSПустойПригодныйНаборДаётОшибку: пустой результат — это отказ, а не
// «нулевые права» и не успех с пустым набором. Иначе после ротации ключей у
// IdP каждый запрос молча превращался бы в 401 без причины.
func TestJWKSПустойПригодныйНаборДаётОшибку(t *testing.T) {
	keys := jwksTestKeys(t)

	cases := []struct {
		name string
		body []byte
	}{
		{name: "пустой массив ключей", body: jwksBody(t)},
		{name: "нет поля keys вовсе", body: jwksMarshal(t, map[string]any{"other": 1})},
		{
			name: "только непригодные ключи",
			body: jwksBody(t,
				func() jwkKey { k := jwksSigEntry("ec", keys[0]); k.Kty = "EC"; return k }(),
				func() jwkKey { k := jwksSigEntry("enc", keys[1]); k.Use = "enc"; return k }(),
			),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := jwksServer(t, jwksServeOK(tc.body))

			got, err := jwksFetcherFor(t, srv.URL, JWKSOptions{}).Fetch(context.Background())
			if err == nil {
				t.Fatalf("пустой набор принят, ключи: %v", got)
			}
			if !strings.Contains(err.Error(), "нет пригодных RSA-ключей") {
				t.Errorf("ожидался отказ по пустому набору, получено: %v", err)
			}
		})
	}
}

// TestJWKSОдинБитыйКлючНеЛомаетНабор: ротация ключей у IdP не должна ронять
// аутентификацию из-за одного неразбираемого элемента набора.
func TestJWKSОдинБитыйКлючНеЛомаетНабор(t *testing.T) {
	keys := jwksTestKeys(t)

	cases := []struct {
		name string
		mut  func(jwkKey) jwkKey
	}{
		{name: "модуль не в base64url", mut: func(k jwkKey) jwkKey { k.N = "не-base64!!"; return k }},
		{name: "показатель не в base64url", mut: func(k jwkKey) jwkKey { k.E = "не-base64!!"; return k }},
		{name: "модуль пустой", mut: func(k jwkKey) jwkKey { k.N = ""; return k }},
		{name: "показатель пустой", mut: func(k jwkKey) jwkKey { k.E = ""; return k }},
		{name: "модуль слишком мал", mut: func(k jwkKey) jwkKey { k.N = base64.RawURLEncoding.EncodeToString([]byte{1, 2, 3}); return k }},
		{name: "показатель нулевой", mut: func(k jwkKey) jwkKey { k.E = base64.RawURLEncoding.EncodeToString([]byte{0}); return k }},
		{name: "показатель чётный", mut: func(k jwkKey) jwkKey { k.E = base64.RawURLEncoding.EncodeToString([]byte{4}); return k }},
		{name: "показатель равен единице", mut: func(k jwkKey) jwkKey { k.E = base64.RawURLEncoding.EncodeToString([]byte{1}); return k }},
		{name: "модуль неправдоподобно велик", mut: func(k jwkKey) jwkKey {
			huge := new(big.Int).Lsh(big.NewInt(1), maxJWKSModulusBits+64)
			k.N = base64.RawURLEncoding.EncodeToString(huge.Bytes())
			return k
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			broken := tc.mut(jwksSigEntry("broken", keys[1]))
			srv := jwksServer(t, jwksServeOK(jwksBody(t, jwksSigEntry("good", keys[0]), broken)))

			got, err := jwksFetcherFor(t, srv.URL, JWKSOptions{}).Fetch(context.Background())
			if err != nil {
				t.Fatalf("один битый ключ обрушил весь набор: %v", err)
			}
			if _, ok := got["good"]; !ok {
				t.Error("корректный ключ потерялся из-за соседнего битого")
			}
			if _, ok := got["broken"]; ok {
				t.Error("битый ключ попал в набор")
			}
		})
	}
}

// TestJWKSПревышениеПределаРазмераДаётОшибку: враждебный эндпойнт не должен
// залить память процесса. Проверяем именно ошибку, а не панику: паника в
// обработчике запроса уронила бы весь сервер.
func TestJWKSПревышениеПределаРазмераДаётОшибку(t *testing.T) {
	const limit = 4 << 10 // 4 КиБ

	var hits atomic.Int64
	srv := jwksServer(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		chunk := strings.Repeat("A", 64<<10)
		for range 256 { // 16 МиБ при пределе 4 КиБ
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
		}
	})

	done := make(chan struct{})
	defer close(done)

	j := jwksFetcherFor(t, srv.URL, JWKSOptions{MaxBodyBytes: limit})
	var (
		got map[string]*rsa.PublicKey
		err error
	)
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("паника вместо ошибки: %v", r)
			}
		}()
		got, err = j.Fetch(context.Background())
	}()

	if err == nil {
		t.Fatalf("ответ больше предела принят, ключи: %v", got)
	}
	if !strings.Contains(err.Error(), "предел") {
		t.Errorf("ожидалась ошибка о пределе размера, получено: %v", err)
	}
	if hits.Load() == 0 {
		t.Error("сервер не был опрошен — тест не проверил ничего")
	}
}

// TestJWKSПределРазмераНеОтсекаетРовноПредельныйОтвет: проверка на «limit+1
// байт» означает, что ответ ровно в пределе обязан приниматься. Здесь обход
// в другую сторону: урезанный лимит в байт должен давать ошибку, а не
// «успех» на обрезанном JSON.
func TestJWKSПределРазмераНеОтсекаетРовноПредельныйОтвет(t *testing.T) {
	keys := jwksTestKeys(t)
	body := jwksBody(t, jwksSigEntry("key-1", keys[0]), jwksSigEntry("key-2", keys[1]))
	srv := jwksServer(t, jwksServeOK(body))

	t.Run("ровно предел принимается", func(t *testing.T) {
		j := jwksFetcherFor(t, srv.URL, JWKSOptions{MaxBodyBytes: int64(len(body))})
		if _, err := j.Fetch(context.Background()); err != nil {
			t.Fatalf("ответ ровно в пределе отвергнут: %v", err)
		}
	})

	t.Run("на байт меньше предела отвергается", func(t *testing.T) {
		j := jwksFetcherFor(t, srv.URL, JWKSOptions{MaxBodyBytes: int64(len(body)) - 1})
		if _, err := j.Fetch(context.Background()); err == nil {
			t.Fatal("ответ больше предела принят")
		}
	})

	t.Run("предел по умолчанию разумный", func(t *testing.T) {
		if defaultJWKSMaxBodyBytes < 64<<10 || defaultJWKSMaxBodyBytes > 16<<20 {
			t.Errorf("предел по умолчанию %d байт выглядит неосмысленным", defaultJWKSMaxBodyBytes)
		}
	})
}

// TestJWKSРедиректНаДругойХостЗапрещён: адрес из редиректа никто не
// проверял, поэтому им уводят запрос во внутреннюю сеть — вплоть до
// 169.254.169.254. Целевой сервер обязан остаться нетронутым.
func TestJWKSРедиректНаДругойХостЗапрещён(t *testing.T) {
	keys := jwksTestKeys(t)

	var internalHits atomic.Int64
	internal := jwksServer(t, func(w http.ResponseWriter, _ *http.Request) {
		internalHits.Add(1)
		_, _ = w.Write(jwksBody(t, jwksSigEntry("stolen", keys[0])))
	})

	edge := jwksServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL+"/latest/meta-data/", http.StatusFound)
	})

	got, err := jwksFetcherFor(t, edge.URL, JWKSOptions{}).Fetch(context.Background())
	if err == nil {
		t.Fatalf("редирект на другой хост выполнен, ключи: %v", got)
	}
	if !strings.Contains(err.Error(), "редирект JWKS") {
		t.Errorf("ожидался отказ по редиректу, получено: %v", err)
	}
	if internalHits.Load() != 0 {
		t.Error("внутренний адрес был опрошен — SSRF-цепочка не закрыта")
	}
	if _, leaked := got["stolen"]; leaked {
		t.Error("ключи с внутреннего адреса попали в набор доверия")
	}
}

// TestJWKSРедиректВПределахХостаРазрешён: запрет редиректов «вообще» сломал
// бы стенды, где JWKS отдаётся с другого пути. Ограничение по схеме и хосту
// обязано такое пропускать — иначе его чинили бы обходным путём.
func TestJWKSРедиректВПределахХостаРазрешён(t *testing.T) {
	keys := jwksTestKeys(t)

	var hits atomic.Int64
	srv := jwksServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/realms/kb/protocol/openid-connect/certs" {
			hits.Add(1)
			_, _ = w.Write(jwksBody(t, jwksSigEntry("key-1", keys[0])))
			return
		}
		http.Redirect(w, r, "/realms/kb/protocol/openid-connect/certs", http.StatusMovedPermanently)
	})

	got, err := jwksFetcherFor(t, srv.URL+"/jwks", JWKSOptions{}).Fetch(context.Background())
	if err != nil {
		t.Fatalf("редирект в пределах хоста отвергнут: %v", err)
	}
	if _, ok := got["key-1"]; !ok {
		t.Error("после редиректа ключ не получен")
	}
	if hits.Load() == 0 {
		t.Error("целевой путь не был опрошен")
	}
}

// TestJWKSРедиректНеПонижаетСхему: ответ «иди сюда» не должен уводить проверку
// подписи с https на http. Проверяется политика напрямую: поднять https-сервер
// с самоподписанным сертификатом в unit-тесте дороже, чем проверить правило.
func TestJWKSРедиректНеПонижаетСхему(t *testing.T) {
	policy := jwksRedirectPolicy(false)
	origin := jwksRequestFor(t, "https://idp.example.com/jwks")

	err := policy(jwksRequestFor(t, "http://idp.example.com/jwks"), []*http.Request{origin})
	if err == nil {
		t.Fatal("редирект с https на http разрешён")
	}
	if !strings.Contains(err.Error(), "http://") {
		t.Errorf("в ошибке нет указания на понижение схемы: %v", err)
	}

	// Тот же хост по https — обычное дело, оно обязано проходить.
	sameScheme := jwksRequestFor(t, "https://idp.example.com/realms/kb/certs")
	if err := policy(sameScheme, []*http.Request{origin}); err != nil {
		t.Errorf("редирект в пределах https-хоста отвергнут: %v", err)
	}
}

// TestJWKSРедиректыОграниченыПоЧислу: цепочка редиректов — признак
// скомпрометированного эндпойнта, а не нормальной конфигурации.
func TestJWKSРедиректыОграниченыПоЧислу(t *testing.T) {
	srv := jwksServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/jwks", http.StatusFound)
	})

	got, err := jwksFetcherFor(t, srv.URL, JWKSOptions{}).Fetch(context.Background())
	if err == nil {
		t.Fatalf("бесконечный редирект принят, ключи: %v", got)
	}
	if !strings.Contains(err.Error(), "редирект") {
		t.Errorf("ожидался отказ по числу редиректов, получено: %v", err)
	}
}

// TestJWKSHTTPБезРазрешенияОтвергаетсяНаСтарте: главный дефект аудита M-3a.
// По http:// MITM подменяет набор ключей, и подпись начинает проверяться чужим
// ключом — при этом все прочие проверки остаются зелёными, то есть отказ
// невидим.
//
// Отвергать надо на старте, а не на первом запросе: к первому запросу
// процесс уже считает себя работающим.
func TestJWKSHTTPБезРазрешенияОтвергаетсяНаСтарте(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{name: "публичный адрес по http", url: "http://idp.example.com/jwks"},
		{name: "внутреннее имя по http", url: "http://keycloak/jwks"},
		{name: "http с логином и паролем", url: "http://user:pass@idp.example.com/jwks"},
		{name: "http с ложным префиксом loopback", url: "http://127.0.0.1.evil.example/jwks"},
		{name: "http на внешний адрес вместо loopback", url: "http://169.254.169.254/jwks"},
		{name: "схема file", url: "file:///etc/passwd"},
		{name: "схема gopher", url: "gopher://idp.example.com/jwks"},
		{name: "без схемы", url: "idp.example.com/jwks"},
		{name: "без хоста", url: "https:///jwks"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j, err := NewHTTPJWKSWithOptions(tc.url, JWKSOptions{})
			if err == nil {
				t.Fatalf("адрес %q принят на старте: %+v", tc.url, j)
			}
			if !errors.Is(err, ErrInsecureJWKSURL) && !strings.Contains(err.Error(), "JWKS") {
				t.Errorf("ошибка не про JWKS: %v", err)
			}
		})
	}
}

// TestJWKSОтказПоСхемеНеДаётУйтиВСеть: NewHTTPJWKS сохранён ради вызова из
// internal/app, поэтому недоверенный адрес отвергается не ошибкой, а внутри.
// Проверяем главное — до отказа не уходит ни одного байта: иначе «защита»
// оказывается проверкой после отправки запроса.
func TestJWKSОтказПоСхемеНеДаётУйтиВСеть(t *testing.T) {
	// Хост из зарезервированного TLD: если бы запрос был отправлен, ошибка
	// была бы сетевой, а не ошибкой проверки схемы.
	j := NewHTTPJWKS("http://idp.example.invalid/jwks")

	got, err := j.Fetch(context.Background())
	if err == nil {
		t.Fatalf("http:// без флага принят, ключи: %v", got)
	}
	if !errors.Is(err, ErrInsecureJWKSURL) {
		t.Fatalf("ожидался ErrInsecureJWKSURL, получено: %v", err)
	}
	if strings.Contains(err.Error(), "no such host") || strings.Contains(err.Error(), "timeout") {
		t.Errorf("запрос всё же ушёл в сеть: %v", err)
	}
}

// TestJWKSНеСобираетNilНаНедоверенномАдресе: регрессия.
//
// newAuth кладёт NewHTTPJWKS(...) прямо в OIDCOptions.JWKS и полагается на
// проверку «jwks обязателен». Типизированный nil *HTTPJWKS в интерфейсе не
// равен nil, поэтому проверка прошла бы, сервер поднялся бы — и падал бы на
// первом запросе с токеном. Отказ в обслуживании по вине чужого токена здесь
// недопустим, поэтому ридер обязан существовать всегда.
func TestJWKSНеСобираетNilНаНедоверенномАдресе(t *testing.T) {
	for _, raw := range []string{
		"http://idp.example.com/jwks",
		"http://keycloak/jwks",
		"file:///etc/passwd",
		"https:///jwks",
		"://не-разбирается",
	} {
		t.Run(raw, func(t *testing.T) {
			var j *HTTPJWKS
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("паника при сборке ридера: %v", r)
					}
				}()
				j = NewHTTPJWKS(raw)
			}()

			if j == nil {
				t.Fatal("ридер nil — проверка «jwks обязателен» его не поймает")
			}
			// Отказ обязан прийти из Fetch, а не из паники: именно Fetch
			// зовёт ридер на первом запросе с токеном.
			if _, err := j.Fetch(context.Background()); err == nil {
				t.Error("недоверенный адрес дал успех вместо отказа")
			}
		})
	}
}

// TestJWKSNilРидерДаётОшибкуАНеПанику: нулевой ридер обязан отказать, а не
// уронить процесс.
func TestJWKSNilРидерДаётОшибкуАНеПанику(t *testing.T) {
	var j *HTTPJWKS

	var (
		got map[string]*rsa.PublicKey
		err error
	)
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("паника вместо ошибки: %v", r)
			}
		}()
		got, err = j.Fetch(context.Background())
	}()

	if err == nil {
		t.Fatalf("нулевой ридер вернул успех: %v", got)
	}
	if got != nil {
		t.Errorf("нулевой ридер вернул ключи: %v", got)
	}
}

// TestJWKSLoopbackПоHTTPРазрешён: стенды и docker-compose с одним контейнером
// поднимают IdP рядом. Трафик не покидает хост, позиции для MITM нет, а
// требование https сломало бы разработку.
func TestJWKSLoopbackПоHTTPРазрешён(t *testing.T) {
	for _, url := range []string{
		"http://127.0.0.1:8080/jwks",
		"http://127.0.0.2:8080/jwks",
		"http://[::1]:8080/jwks",
		"http://localhost:8080/jwks",
		"http://LOCALHOST:8080/jwks",
	} {
		t.Run(url, func(t *testing.T) {
			if _, err := NewHTTPJWKSWithOptions(url, JWKSOptions{}); err != nil {
				t.Errorf("loopback-адрес отвергнут: %v", err)
			}
		})
	}
}

// TestJWKSФлагСтендаРазрешаетОткрытыйКанал: явное решение оператора должно
// работать, иначе его пришлось бы обходить, а обходы не возвращают доверие.
func TestJWKSФлагСтендаРазрешаетОткрытыйКанал(t *testing.T) {
	url := "http://keycloak:8080/realms/kb/protocol/openid-connect/certs"

	if _, err := NewHTTPJWKSWithOptions(url, JWKSOptions{}); err == nil {
		t.Fatal("без флага адрес принят")
	}
	j, err := NewHTTPJWKSWithOptions(url, JWKSOptions{AllowInsecureJWKS: true})
	if err != nil {
		t.Fatalf("с флагом адрес отвергнут: %v", err)
	}
	if !j.allowInsecureJWKS {
		t.Error("флаг не сохранён в ридере")
	}

	t.Run("флаг из окружения", func(t *testing.T) {
		t.Setenv(allowInsecureJWKSEnv, "true")
		if !allowInsecureFromEnv() {
			t.Error("OIDC_ALLOW_INSECURE_JWKS=true не прочитан")
		}
		if !NewHTTPJWKS(url).allowInsecureJWKS {
			t.Error("NewHTTPJWKS проигнорировал флаг окружения")
		}
	})

	t.Run("опечатка в флаге закрывает канал", func(t *testing.T) {
		// Ошибка в переменной обязана вести к отказу, а не к открытому каналу.
		for _, raw := range []string{"да", "yes please", "1 1", "tru"} {
			t.Setenv(allowInsecureJWKSEnv, raw)
			if allowInsecureFromEnv() {
				t.Errorf("значение %q неверно прочитано как истинное", raw)
			}
		}
	})

	t.Run("флаг не выставлен", func(t *testing.T) {
		t.Setenv(allowInsecureJWKSEnv, "")
		if allowInsecureFromEnv() {
			t.Error("пустая переменная прочитана как истинная")
		}
	})
}

// TestJWKSТаймаутРаботает: недоступный IdP не должен держать соединение до
// отказа балансировщика. Без таймаута каждый запрос с токеном висел бы на
// сети, и API переставал бы отвечать.
func TestJWKSТаймаутРаботает(t *testing.T) {
	const clientTimeout = 150 * time.Millisecond

	release := make(chan struct{})
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	// Сервер ждёт освобождения, а не сна: Close() не зависнет на спящем
	// обработчике, и тест не станет медленнее своей длительности.
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	j := jwksFetcherFor(t, srv.URL, JWKSOptions{Timeout: clientTimeout})

	start := time.Now()
	got, err := j.Fetch(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("зависший сервер вернул ключи: %v", got)
	}
	// Не «прошло N секунд», а «вернулось быстро»: точное время нагруженной
	// машины плавает, а вот отсутствие таймаута дало бы минуты.
	if elapsed > 5*time.Second {
		t.Errorf("Fetch шёл %v — таймаут %v не сработал", elapsed, clientTimeout)
	}
	if !strings.Contains(err.Error(), "запрос JWKS") {
		t.Errorf("ошибка не объясняет, что не удалось получить JWKS: %v", err)
	}
	if hits.Load() == 0 {
		t.Error("сервер не был опрошен — тест не проверил ничего")
	}
}

// TestJWKSОтменаКонтекстаУважается: если запрос отменён (пользователь ушёл,
// shutdown), незачем продолжать ждать IdP.
func TestJWKSОтменаКонтекстаУважается(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := jwksFetcherFor(t, srv.URL, JWKSOptions{}).Fetch(ctx); err == nil {
		t.Fatal("отменённый контекст проигнорирован")
	}
}

// TestJWKSПовторныйkidБерётПервыйКлюч: одинаковый kid с разными модулями —
// это попытка выбора ключа по порядку в ответе. Результат обязан быть
// определённым, иначе подпись проверяется «тем» ключом, который подсунул
// эндпойнт.
func TestJWKSПовторныйkidБерётПервыйКлюч(t *testing.T) {
	keys := jwksTestKeys(t)
	first := jwksSigEntry("dup", keys[0])
	second := jwksSigEntry("dup", keys[1])

	srv := jwksServer(t, jwksServeOK(jwksBody(t, first, second)))

	got, err := jwksFetcherFor(t, srv.URL, JWKSOptions{}).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got["dup"].N.Cmp(keys[0].N) != 0 {
		t.Error("при одинаковом kid побеждает не первый ключ — выбор зависит от порядка в ответе")
	}
}

// TestJWKSЗапросНеНесётТокенов: JWKS — публичный документ, и запрос за ним не
// должен нести заголовков аутентификации. Явный Accept защищает от ответа
// «не-JSON» (captive-портал, прокси), который иначе выглядел бы как битый
// документ и маскировал бы настоящую причину.
func TestJWKSЗапросНеНесётТокенов(t *testing.T) {
	keys := jwksTestKeys(t)

	var seen http.Header
	srv := jwksServer(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		_, _ = w.Write(jwksBody(t, jwksSigEntry("key-1", keys[0])))
	})

	if _, err := jwksFetcherFor(t, srv.URL, JWKSOptions{}).Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if got := seen.Get("Authorization"); got != "" {
		t.Errorf("токен ушёл на JWKS-эндпойнт: %q", got)
	}
	if got := seen.Get("Accept"); !strings.Contains(got, "application/json") {
		t.Errorf("Accept = %q, ожидался запрос JSON-документа", got)
	}
}

// TestJWKSТелоОтветаНеУходитВОшибкуСтатуса: содержимое ответа чужого сервера —
// недоверенные данные. Ошибка статуса не должна тащить их в диагностику, где
// они могут оказаться в логах.
func TestJWKSТелоОтветаНеУходитВОшибкуСтатуса(t *testing.T) {
	const secret = "утечка-не-должна-в-лог"
	srv := jwksServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(secret))
	})

	_, err := jwksFetcherFor(t, srv.URL, JWKSOptions{}).Fetch(context.Background())
	if err == nil {
		t.Fatal("ожидалась ошибка")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("тело чужого ответа попало в ошибку: %v", err)
	}
}

// TestJWKSТаймаутПоУмолчаниюРазумный: и слишком короткий, и слишком длинный
// таймаут ломают работу по-своему, и оба случая незаметны в тестах, которые
// используют явный таймаут.
func TestJWKSТаймаутПоУмолчаниюРазумный(t *testing.T) {
	if defaultJWKSFetchTimeout <= 0 {
		t.Fatal("таймаут по умолчанию не задан")
	}
	if defaultJWKSFetchTimeout > 30*time.Second {
		t.Errorf("таймаут %v слишком велик: недоступный IdP будет держать запросы", defaultJWKSFetchTimeout)
	}
	if defaultJWKSFetchTimeout < time.Second {
		t.Errorf("таймаут %v слишком мал: IdP под нагрузкой будет ронять аутентификацию", defaultJWKSFetchTimeout)
	}
}

// TestJWKSРазобранныйКлючПроверяетНастоящуюПодпись: ключ из набора должен быть
// не просто «разобран», а принят криптографией. Ошибка в параметрах RSA
// (показатель нечётный, модуль осмысленный) всплыла бы позже и уже на живом
// токене, когда причину уже не отличить от отказа IdP.
func TestJWKSРазобранныйКлючПроверяетНастоящуюПодпись(t *testing.T) {
	keys := jwksTestKeys(t)
	srv := jwksServer(t, jwksServeOK(jwksBody(t, jwksSigEntry("key-1", keys[0]))))

	got, err := jwksFetcherFor(t, srv.URL, JWKSOptions{}).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	digest := sha256.Sum256([]byte("проверка"))
	sig, err := rsa.SignPKCS1v15(rand.Reader, keys[0], crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("подпись тестовым ключом: %v", err)
	}
	key, ok := got["key-1"]
	if !ok {
		t.Fatal("ключ key-1 отсутствует в наборе")
	}
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		t.Errorf("разобранный ключ не проверяет настоящую подпись: %v", err)
	}
}

// TestJWKSПоляСобираютсяРуками: у HTTPJWKS экспортируемые поля, поэтому
// структуру можно собрать в обход конструктора — и тогда проверка схемы,
// жившая только в конструкторе, была бы обойдена. Отказ обязан прийти из
// Fetch в обоих случаях.
func TestJWKSПоляСобираютсяРуками(t *testing.T) {
	t.Run("недоверенный адрес без конструктора", func(t *testing.T) {
		handmade := &HTTPJWKS{URL: "http://idp.example.invalid/jwks"}

		var (
			got map[string]*rsa.PublicKey
			err error
		)
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("паника вместо отказа: %v", r)
				}
			}()
			got, err = handmade.Fetch(context.Background())
		}()

		if err == nil {
			t.Fatalf("собранный вручную ридер принял http://, ключи: %v", got)
		}
		if !errors.Is(err, ErrInsecureJWKSURL) {
			t.Errorf("ожидался ErrInsecureJWKSURL, получено: %v", err)
		}
	})

	// Ридер без HTTPClient обязан работать: клиент подставляется по
	// умолчанию, иначе nil-указатель уронил бы процесс на живом запросе.
	t.Run("ридер без клиента", func(t *testing.T) {
		keys := jwksTestKeys(t)
		srv := jwksServer(t, jwksServeOK(jwksBody(t, jwksSigEntry("key-1", keys[0]))))

		handmade := &HTTPJWKS{URL: srv.URL}
		got, err := handmade.Fetch(context.Background())
		if err != nil {
			t.Fatalf("ридер без клиента не отработал: %v", err)
		}
		if _, ok := got["key-1"]; !ok {
			t.Error("ключ не получен")
		}
	})
}

// TestJWKSОшибкаЧтенияТелаНеПутаетсяСБитымJSON: оборванное соединение и
// негодный документ — разные причины. Ошибка чтения обязана оставаться ошибкой
// чтения, иначе в диагностике «IdP отдал мусор» появится там, где IdP просто
// закрыл соединение.
func TestJWKSОшибкаЧтенияТелаНеПутаетсяСБитымJSON(t *testing.T) {
	_, err := readLimitedBody(jwksBrokenReader{}, 4096)
	if err == nil {
		t.Fatal("оборванное тело принято")
	}
	if !strings.Contains(err.Error(), "чтение тела") {
		t.Errorf("ожидалась ошибка чтения, получено: %v", err)
	}
	if strings.Contains(err.Error(), "разбор JWKS") {
		t.Errorf("ошибка чтения выглядит как ошибка разбора: %v", err)
	}
}

// jwksBrokenReader — тело, которое рвётся на середине.
type jwksBrokenReader struct{}

func (jwksBrokenReader) Read([]byte) (int, error) {
	return 0, errors.New("соединение разорвано")
}

// jwksRequestFor собирает запрос на указанный адрес: политике редиректов нужен
// именно *http.Request, а не URL.
func jwksRequestFor(t *testing.T, raw string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, raw, nil)
	if err != nil {
		t.Fatalf("сборка запроса %q: %v", raw, err)
	}
	return req
}
