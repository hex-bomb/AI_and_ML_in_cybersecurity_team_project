package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	// defaultJWKSFetchTimeout — потолок на одну загрузку JWKS.
	//
	// Без него недоступный IdP держит соединение до отказа внешнего
	// балансировщика, и API перестаёт отвечать: каждый запрос с токеном ждёт
	// сеть. Обратная сторона — слишком короткий таймаут отсекает IdP под
	// нагрузкой; 5 секунд переживает всплеск и всё ещё осмысленно быстро.
	defaultJWKSFetchTimeout = 5 * time.Second

	// defaultJWKSMaxBodyBytes — потолок на тело ответа JWKS.
	//
	// Набор ключей — это единицы килобайт: даже IdP с сотней активных ключей
	// укладывается в десятки килобайт. МиБ берётся с большим запасом, чтобы
	// не ломать необычный, но честный ответ, и одновременно обрезает поток,
	// который враждебный или скомпрометированный эндпойнт шлёт в память
	// процесса. Без предела Decoder читал тело до упора: это DoS, а не
	// защита от чего-либо.
	defaultJWKSMaxBodyBytes int64 = 1 << 20 // 1 МиБ

	// maxJWKSRedirects — больше одного прыжка JWKS-эндпойнту не нужно.
	// Ограничение осмысленно само по себе: каждый прыжок стоит сети, а
	// цепочка редиректов — это уже признак скомпрометированного эндпойнта.
	maxJWKSRedirects = 3

	// allowInsecureJWKSEnv — явный флаг стенда.
	//
	// Значение читается напрямую из окружения, а не через
	// internal/platform/config: единственный вызов NewHTTPJWKS живёт в
	// internal/app, который этот агент не правит. Читать конфиг из слоя
	// use-case нельзя, поэтому это осознанный временный долг — см. отчёт.
	allowInsecureJWKSEnv = "OIDC_ALLOW_INSECURE_JWKS"
)

// Границы параметров ключа RSA. Не политика «настоящей стойкости» (минимальная
// длина модуля — решение владельца, здесь это не место для неё), а отсечение
// мусора: битый JWK обязан молча выпасть из набора, а не попасть в проверку
// подписи.
const (
	minJWKSModulusBits = 512
	maxJWKSModulusBits = 16384
	maxJWKSExponent    = 1<<32 - 1
)

// rsaSigningAlgs — алгоритмы подписи, для которых ключ RSA вообще годится.
//
// Ужесточение сверх «kty == RSA» нужно потому, что `alg` в JWK объявляет
// назначение ключа. Ключ с `alg: "RS1"` или `alg: "ES256"` — опечатка или
// подмена; принимать его для проверки подписи значит проверять подпись
// алгоритмом, который никто не собирался применять. Пустое значение
// допустимо: часть IdP его не пишет вовсе.
var rsaSigningAlgs = map[string]bool{
	"RS256": true,
	"RS384": true,
	"RS512": true,
}

// ErrInsecureJWKSURL — JWKS задан по открытому каналу и не разрешён
// ни флагом стенда, ни адресом loopback.
//
// Ошибка именованная, а не строка: вызывающий обязан отличать «сбитая
// конфигурация, которую надо исправить» от «IdP сейчас недоступен».
var ErrInsecureJWKSURL = errors.New("JWKS по http:// без разрешающего флага")

// JWKSOptions — настройки защищённого чтения JWKS.
type JWKSOptions struct {
	// AllowInsecureJWKS снимает требование https для любого адреса.
	//
	// Это осознанно грубый инструмент: он годится стенду, где IdP поднят
	// рядом в docker-compose и доступен по http://keycloak:8080. Он НЕ
	// годится продy, потому что снимает ровно ту защиту, ради которой
	// проверка существует.
	AllowInsecureJWKS bool

	// Timeout — потолок одной загрузки. Ноль или отрицательное значение
	// означает defaultJWKSFetchTimeout.
	Timeout time.Duration

	// MaxBodyBytes — потолок тела ответа. Ноль или отрицательное значение
	// означает defaultJWKSMaxBodyBytes.
	MaxBodyBytes int64
}

// HTTPJWKS читает JWKS IdP по сети.
//
// Набор проходит три фильтра, и каждый из них закрывает свою угрозу:
//
//   - схема адреса: http:// без явного разрешения отвергается, потому что
//     MITM подменяет набор ключей, и подпись начинает проверяться чужим
//     ключом — при этом все прочие проверки остаются зелёными;
//   - назначение ключа (`use`/`alg`/`key_ops`): ключ, предназначенный для
//     шифрования, не должен проверять подпись;
//   - целостность ключа: RSA только, показатель нечётный и осмысленный.
//
// Таймаут, запрет редиректов и предел размера ответа — не про доверие к IdP,
// а про то, чтобы враждебный или скомпрометированный эндпойнт не смог
// повесить процесс по памяти, времени или через внутренние адреса.
type HTTPJWKS struct {
	URL        string
	HTTPClient *http.Client

	// allowInsecureJWKS переносится в структуру, потому что схема адреса
	// проверяется на каждом Fetch, а не только в конструкторе: структура с
	// экспортируемыми полями собирается и руками, минуя NewHTTPJWKS.
	allowInsecureJWKS bool
	maxBodyBytes      int64

	// configErr — отказ, вычисленный при сборке.
	//
	// Читатель обязан существовать даже тогда, когда адрес недоверенный.
	// nil здесь проскочил бы мимо проверки «jwks обязателен»: типизированный
	// nil внутри интерфейса не равен nil, проверка прошла бы, и паника ждала бы
	// первый запрос с токеном.
	configErr error
}

// NewHTTPJWKS собирает читателя JWKS с таймаутом по умолчанию.
//
// Сигнатура сохранена прежней, потому что вызов живёт в internal/app. Поэтому
// недоверенный адрес здесь НЕ отвергается ошибкой — вместо этого читатель
// остаётся рабочим объектом, а Fetch отказывается до единого сетевого байта.
// Возвращать nil нельзя: см. поле configErr. Строгий вариант, который роняет
// старт, — NewHTTPJWKSWithOptions.
//
// Разрешение на открытый канал берётся из OIDC_ALLOW_INSECURE_JWKS, иначе
// стенд с IdP на http нельзя было бы поднять вовсе.
func NewHTTPJWKS(url string) *HTTPJWKS {
	return newHTTPJWKS(url, JWKSOptions{AllowInsecureJWKS: allowInsecureFromEnv()})
}

// NewHTTPJWKSWithOptions — строгий конструктор: недоверенный адрес отвергается
// сразу, на старте, а не на первом запросе.
//
// Именно его должен звать newAuth: пока конфигурация проверяется вручную в
// internal/app, ошибка схемы всплывёт там же, где уже проверяется непустота
// переменных, и сервер не поднимется с ключами по открытому каналу.
func NewHTTPJWKSWithOptions(rawURL string, opts JWKSOptions) (*HTTPJWKS, error) {
	if _, err := parseJWKSURL(rawURL, opts.AllowInsecureJWKS); err != nil {
		return nil, err
	}
	return newHTTPJWKS(rawURL, opts), nil
}

// newHTTPJWKS собирает читателя и записывает отказ по адресу в configErr.
//
// Отказ не превращается в nil: вызывающий (internal/app) кладёт результат в
// OIDCOptions.JWKS без проверки, и типизированный nil проскочил бы мимо
// проверки «jwks обязателен», отложив панику до первого запроса с токеном.
func newHTTPJWKS(rawURL string, opts JWKSOptions) *HTTPJWKS {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultJWKSFetchTimeout
	}
	maxBody := opts.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = defaultJWKSMaxBodyBytes
	}

	j := &HTTPJWKS{
		URL:        rawURL,
		HTTPClient: &http.Client{Timeout: timeout, CheckRedirect: jwksRedirectPolicy(opts.AllowInsecureJWKS)},
		// allowInsecureJWKS и maxBodyBytes нужны и в Fetch: ридер можно
		// собрать руками по экспортируемым полям, минуя конструктор.
		allowInsecureJWKS: opts.AllowInsecureJWKS,
		maxBodyBytes:      maxBody,
	}
	if _, err := parseJWKSURL(rawURL, opts.AllowInsecureJWKS); err != nil {
		j.configErr = err
	}
	return j
}

// allowInsecureFromEnv разбирает флаг стенда.
//
// Неразбираемое значение считается «нет»: опечатка в переменной обязана
// закрыть канал, а не открыть его.
func allowInsecureFromEnv() bool {
	raw := strings.TrimSpace(os.Getenv(allowInsecureJWKSEnv))
	if raw == "" {
		return false
	}
	allowed, err := strconv.ParseBool(raw)
	return err == nil && allowed
}

// parseJWKSURL проверяет адрес JWKS и возвращает разобранный URL.
//
// Проверка повторяется на каждом Fetch, а не только в конструкторе: поля
// HTTPJWKS экспортируемые, и структуру можно собрать в обход проверки. Если
// дыра останется только в конструкторе, её откроет первый же рефакторинг.
func parseJWKSURL(rawURL string, allowInsecure bool) (*url.URL, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("адрес JWKS не разбирается: %w", err)
	}

	// Hostname(), а не Host: во втором остаются логин, пароль и порт. Разбор
	// вида «http://idp.example.com@127.0.0.1/jwks» обязан смотреть на
	// настоящий адрес соединения, а не на подставной перед «@».
	host := parsed.Hostname()
	if host == "" {
		return nil, fmt.Errorf("адрес JWKS %q не содержит хоста", rawURL)
	}

	switch parsed.Scheme {
	case "https":
		return parsed, nil
	case "http":
		if allowInsecure {
			return parsed, nil
		}
		// Loopback — исключение не по флагу, а по свойству адреса. Трафик
		// не покидает хост, поэтому позиции для MITM нет: атакующий, который
		// уже выполняет код локально, не выиграет от подмены ключей ничего.
		// Стенды и docker-compose с одним контейнером работают как раньше.
		if isLoopbackHost(host) {
			return parsed, nil
		}
		return nil, fmt.Errorf("%w: %q", ErrInsecureJWKSURL, safeURLForError(parsed))
	default:
		return nil, fmt.Errorf("схема %q адреса JWKS не поддерживается, ждём https", parsed.Scheme)
	}
}

// isLoopbackHost сообщает, что хост — это loopback.
//
// Сравнение с localhost сделано по строке, а не по резолву: иначе проверка
// зависела бы от того, что вернул резолвер, и «localhost» из чужого
// /etc/hosts протащил бы в адрес недоверенный хост. IP разбирается
// библиотекой, а не префиксом строки: «127.0.0.1.evil.example» не должен
// проходить под видом «127.0.0.1».
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// jwksRedirectPolicy собирает правило редиректов.
//
// Редирект — это выданный IdP ответ «иди за мной», а адрес, полученный по
// редиректу, никто не проверял: им можно увести запрос на 169.254.169.254 и
// вытащить метаданные облака, или на любой внутренний сервис. Поэтому
// разрешён только прыжок в пределах того же схемы и хоста, что исходный адрес.
//
// Наивный запрет редиректов был бы безопаснее, но сломал бы стенды, где
// JWKS отдаётся с другого пути после переезда конфигурации. Ограничение по
// схеме и хосту закрывает SSRF и не закрывает ничего нужного.
func jwksRedirectPolicy(allowInsecure bool) func(req *http.Request, via []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxJWKSRedirects {
			return fmt.Errorf("JWKS: больше %d редиректов подряд", maxJWKSRedirects)
		}
		if len(via) == 0 {
			return nil
		}
		origin := via[0].URL

		// Схему проверяем заново на каждом прыжке: иначе https молча скатился
		// бы в http, и проверка подписи поехала бы уже по открытому каналу.
		if _, err := parseJWKSURL(req.URL.String(), allowInsecure); err != nil {
			return fmt.Errorf("редирект JWKS: %w", err)
		}
		if !strings.EqualFold(req.URL.Host, origin.Host) {
			return fmt.Errorf("редирект JWKS уводит с %s на %s", safeURLForError(origin), safeURLForError(req.URL))
		}
		return nil
	}
}

// safeURLForError убирает из URL логин и пароль, оставляя адрес соединения.
//
// Ошибка уходит в лог на старте и в диагностику. Пароль в userinfo там
// оказаться не должен — по той же причине, по которой содержимое токена
// не пишется в лог.
func safeURLForError(u *url.URL) string {
	return u.Scheme + "://" + u.Host + u.EscapedPath()
}

// Fetch загружает и разбирает JWKS.
//
// Возвращает ошибку на любой причине, включая пустой пригодный набор: вверх
// по стеку ошибка превращается в отказ проверки токена, а молчаливый пустой
// набор означал бы, что после ротации ключей все запросы просто перестали бы
// проходить без внятной причины.
func (j *HTTPJWKS) Fetch(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	// nil-ридер — это паника в обработчике запроса, то есть отказ в
	// обслуживании по вине чужого токена. Проверка обязана стоять здесь: ридер
	// кладут в OIDCOptions без проверки, и nil-интерфейс там не отлавливается.
	if j == nil {
		return nil, errors.New("ридер JWKS не собран")
	}
	// Отказ, вычисленный при сборке: он не зависит от запроса и обязан быть
	// возвращён до сетевого вызова.
	if j.configErr != nil {
		return nil, j.configErr
	}

	// Адрес проверяется здесь, а не только в конструкторе: см. parseJWKSURL.
	// Проверка до сетевого вызова — при отказе не уходит ни одного байта.
	parsed, err := parseJWKSURL(j.URL, j.allowInsecureJWKS)
	if err != nil {
		return nil, err
	}

	maxBody := j.maxBodyBytes
	if maxBody <= 0 {
		maxBody = defaultJWKSMaxBodyBytes
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("запрос JWKS: %w", err)
	}
	// Явный Accept: без него captive-портал или прокси отдаст HTML, который
	// разберётся как «битый JSON» и замаскирует настоящую причину.
	req.Header.Set("Accept", "application/json")

	client := j.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultJWKSFetchTimeout, CheckRedirect: jwksRedirectPolicy(j.allowInsecureJWKS)}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("запрос JWKS %s: %w", safeURLForError(parsed), err)
	}
	// Ошибку закрытия тела игнорировать можно только после того, как тело
	// прочитано: закрытие не влияет ни на статус, ни на разобранные ключи.
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Тело не читается намеренно: оно не нужно для решения, а ответ
		// чужого эндпойнта — это недоверенные данные, которые нечего ни
		// логировать, ни тащить в память.
		return nil, fmt.Errorf("JWKS %s вернул статус %d", safeURLForError(parsed), resp.StatusCode)
	}

	body, err := readLimitedBody(resp.Body, maxBody)
	if err != nil {
		return nil, fmt.Errorf("чтение JWKS %s: %w", safeURLForError(parsed), err)
	}

	var payload struct {
		Keys []jwkKey `json:"keys"`
	}
	// Unmarshal, а не Decoder по телу: уже прочитанный ограниченный срез
	// разбирается целиком, и мусор после объекта — тоже ошибка, а не тишина.
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("разбор JWKS %s: %w", safeURLForError(parsed), err)
	}

	out := make(map[string]*rsa.PublicKey, len(payload.Keys))
	for _, k := range payload.Keys {
		if !usableForSignature(k) {
			continue
		}
		key, err := rsaFromJWK(k.N, k.E)
		if err != nil {
			// Один битый ключ не должен ломать весь набор: остальные
			// продолжат работать.
			continue
		}
		// Первый выигрывает, повторы игнорируются. Поведение определено
		// явно: при одинаковом kid с разными модулями результат не должен
		// зависеть от порядка, который задал чужой эндпойнт.
		if _, exists := out[k.Kid]; !exists {
			out[k.Kid] = key
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("в JWKS %s нет пригодных RSA-ключей проверки подписи", safeURLForError(parsed))
	}
	return out, nil
}

// readLimitedBody читает тело целиком, но не больше limit байт.
//
// Один лишний байт — обязателен: по нему отличается «ответ ровно предельного
// размера» от «ответ больше предела». Превышение даёт внятную ошибку, а не
// обрезанный JSON с сообщением про неожиданный конец ввода.
func readLimitedBody(body io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("чтение тела: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("ответ превышает предел в %d байт", limit)
	}
	return data, nil
}

// jwkKey — запись набора JWKS.
//
// Поля use, alg и key_ops разбираются не «на всякий случай», а потому что
// объявляют назначение ключа, и проверка подписи обязана с ним считаться.
// Всё лишнее намеренно не описываем: неизвестные поля JSON игнорируются, а
// лишние поля в структуре тратят память на каждый ключ в наборе.
type jwkKey struct {
	Kid    string   `json:"kid"`
	Kty    string   `json:"kty"`
	Use    string   `json:"use"`
	Alg    string   `json:"alg"`
	N      string   `json:"n"`
	E      string   `json:"e"`
	KeyOps []string `json:"key_ops"`
}

// usableForSignature решает, годится ли ключ для проверки подписи.
//
// Три независимых отсева:
//
//   - kty: только асимметричный RSA. HMAC-ключ (kty: "oct") нельзя принять
//     как материал проверки: проверяющий знает публичный ключ IdP, и если
//     это он же «секрет», подпись подделывается без приватного ключа;
//   - use: ключ для шифрования (use: "enc") не должен проверять подпись.
//     Такие ключи в JWKS лежат рядом с ключами подписи, и их подстановка
//     выглядит неотличимо от корректной работы;
//   - alg и key_ops: объявленное назначение должно совпадать с тем, чем
//     ключ собираются пользоваться. Пустое значение допустимо — не все IdP
//     пишут эти поля.
func usableForSignature(k jwkKey) bool {
	if k.Kty != "RSA" || k.Kid == "" {
		return false
	}
	switch k.Use {
	case "", "sig":
	default:
		return false
	}
	if k.Alg != "" && !rsaSigningAlgs[k.Alg] {
		return false
	}
	// key_ops — то же назначение, что и use, но точнее: список операций.
	// Ключ, которому запрещена verify, не годится для подписи даже при
	// подходящем use.
	if len(k.KeyOps) > 0 && !slices.Contains(k.KeyOps, "verify") {
		return false
	}
	return true
}

// rsaFromJWK собирает публичный ключ из модуля и показателя JWK.
func rsaFromJWK(nB64, eB64 string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nB64)
	if err != nil {
		return nil, fmt.Errorf("модуль ключа: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(eB64)
	if err != nil {
		return nil, fmt.Errorf("показатель ключа: %w", err)
	}
	// Базовый размер RFC 7518 не проверяется: запись с ведущими нулями в
	// модуле допустима, и строгая проверка длины отбросила бы честный ключ.
	// Пустой модуль — уже не ключ.
	n := new(big.Int).SetBytes(nBytes)
	if n.Sign() == 0 {
		return nil, fmt.Errorf("модуль ключа пуст")
	}
	switch bits := n.BitLen(); {
	case bits < minJWKSModulusBits:
		return nil, fmt.Errorf("модуль ключа слишком мал: %d бит", bits)
	case bits > maxJWKSModulusBits:
		return nil, fmt.Errorf("модуль ключа неправдоподобно велик: %d бит", bits)
	}

	e := 0
	for _, b := range eBytes {
		e = e<<8 | int(b)
	}
	// Показатель обязан быть нечётным и >= 3: чётный показатель — это
	// не RSA, и rsa.VerifyPKCS1v15 такой ключ отвергнет, а проверить
	// поведение заранее дешевле, чем разбираться с отказом на живом токене.
	switch {
	case e == 0:
		return nil, fmt.Errorf("показатель ключа пуст")
	case e%2 == 0 || e < 3:
		return nil, fmt.Errorf("показатель ключа %d не похож на RSA", e)
	case e > maxJWKSExponent:
		return nil, fmt.Errorf("показатель ключа %d вне допустимого диапазона", e)
	}
	return &rsa.PublicKey{N: n, E: e}, nil
}
