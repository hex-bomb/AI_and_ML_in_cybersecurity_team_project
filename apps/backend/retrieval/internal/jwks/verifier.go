// Package jwks — проверка JWT по JWKS-эндпоинту auth-сервиса.
package jwks

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Claims — содержимое токена сессии.
type Claims struct {
	Sub  string   `json:"sub"`
	Name string   `json:"name"`
	Role string   `json:"role"`
	Scp  []string `json:"scp"`
	Grp  []string `json:"grp"`
	Exp  int64    `json:"exp"`
}

// Channels — каналы, в которых principal может читать документы.
// Сотрудник видит и внутренние: auth раскладывает роль в scopes, а не в fields.
func (c Claims) Channels() []string {
	for _, scope := range c.Scp {
		if scope == "employee" {
			return []string{"public", "internal"}
		}
	}
	return []string{"public"}
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// Verifier проверяет подписи RS256, кэшируя ключи и обновляя их по TTL.
type Verifier struct {
	url string
	hc  *http.Client

	// mu защищает только карту ключей и метку времени; сетевой вызов — вне лока.
	mu   sync.RWMutex
	keys map[string]*rsa.PublicKey
	at   time.Time

	// refreshMu сериализует обновления, чтобы не устраивать шторм запросов к auth.
	refreshMu sync.Mutex
	// nextAttempt — пауза после неудачного обновления; читается только под refreshMu.
	nextAttempt time.Time
	// ttl задаёт время жизни кэша: ротация ключей в auth подхватывается с этой
	// задержкой.
	ttl time.Duration
}

const (
	refreshBackoff = 10 * time.Second
	// maxJwksBytes ограничивает тело ответа JWKS: auth — внешняя система, её
	// ответ читаем с потолком.
	maxJwksBytes = 1 << 20
)

func NewVerifier(url string) *Verifier {
	return &Verifier{
		url: url,
		hc:  &http.Client{Timeout: 5 * time.Second},
		ttl: 5 * time.Minute,
	}
}

// Verify возвращает claims либо ошибку. Ошибка всегда — отказ авторизации.
func (v *Verifier) Verify(ctx context.Context, token string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, errors.New("malformed token")
	}

	rawHeader, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Claims{}, err
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(rawHeader, &header); err != nil {
		return Claims{}, err
	}
	if header.Alg != "RS256" {
		return Claims{}, errors.New("unsupported alg")
	}

	key, err := v.key(ctx, header.Kid)
	if err != nil {
		return Claims{}, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		return Claims{}, errors.New("bad signature")
	}

	rawPayload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, err
	}
	var c Claims
	if err := json.Unmarshal(rawPayload, &c); err != nil {
		return Claims{}, err
	}
	if c.Exp == 0 || time.Now().Unix() >= c.Exp {
		return Claims{}, errors.New("expired")
	}
	return c, nil
}

func (v *Verifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	// Быстрый путь: ключ уже в кэше и свежий.
	if k, ok := v.cached(kid); ok {
		return k, nil
	}

	// Медленный путь: обновление под отдельным лочом, карта не блокируется на I/O.
	v.refreshMu.Lock()
	defer v.refreshMu.Unlock()

	// Пока брали refreshMu, другой поток мог уже обновить ключи.
	if k, ok := v.cached(kid); ok {
		return k, nil
	}

	// После сбоя не долбим auth на каждом запросе: без паузы недоступный
	// auth превращается в ожидание таймаута на каждом вызове подряд.
	if time.Now().Before(v.nextAttempt) {
		if k, ok := v.lookup(kid); ok {
			return k, nil
		}
		return nil, errors.New("jwks refresh backed off")
	}

	if err := v.refresh(ctx); err != nil {
		v.nextAttempt = time.Now().Add(refreshBackoff)
		// При живых старых ключах деградируем на них, но логируем сбой.
		if v.hasAny() {
			slog.Warn("jwks refresh failed, using cached keys", "err", err)
		} else {
			return nil, fmt.Errorf("jwks refresh: %w", err)
		}
	} else {
		v.nextAttempt = time.Time{}
	}

	if k, ok := v.lookup(kid); ok {
		return k, nil
	}
	return nil, errors.New("unknown kid")
}

// cached возвращает ключ, если карта свежая по TTL.
func (v *Verifier) cached(kid string) (*rsa.PublicKey, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if time.Since(v.at) >= v.ttl {
		return nil, false
	}
	k, ok := v.keys[kid]
	return k, ok
}

func (v *Verifier) lookup(kid string) (*rsa.PublicKey, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	k, ok := v.keys[kid]
	return k, ok
}

func (v *Verifier) hasAny() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return len(v.keys) > 0
}

// refresh заменяет карту ключей разом: подписчики cached не увидят полусобранный
// набор.
func (v *Verifier) refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.url, nil)
	if err != nil {
		return err
	}
	resp, err := v.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks status %d", resp.StatusCode)
	}

	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJwksBytes)).Decode(&set); err != nil {
		return err
	}

	keys := make(map[string]*rsa.PublicKey, len(set.Keys))
	for _, j := range set.Keys {
		if j.Kty != "RSA" {
			continue
		}
		n, err := base64.RawURLEncoding.DecodeString(j.N)
		if err != nil {
			return err
		}
		e, err := base64.RawURLEncoding.DecodeString(j.E)
		if err != nil {
			return err
		}
		keys[j.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	if len(keys) == 0 {
		return errors.New("empty jwks")
	}

	v.mu.Lock()
	v.keys, v.at = keys, time.Now()
	v.mu.Unlock()
	return nil
}
