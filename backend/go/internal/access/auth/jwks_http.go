package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"time"
)

// HTTPJWKS читает JWKS IdP по сети.
//
// Ключи с не-RSA алгоритмом молча пропускаются: используются только
// асимметричные, иначе появился бы путь, где подпись проверяется секретом,
// известным проверяющему.
type HTTPJWKS struct {
	URL        string
	HTTPClient *http.Client
}

// NewHTTPJWKS собирает читатель JWKS с таймаутом по умолчанию.
func NewHTTPJWKS(url string) *HTTPJWKS {
	return &HTTPJWKS{
		URL: url,
		// Таймаут обязателен: без него недоступный IdP держит соединение
		// до отказа внешнего балансировщика, и API перестаёт отвечать.
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	}
}

// Fetch загружает и разбирает JWKS.
func (j *HTTPJWKS) Fetch(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("запрос JWKS: %w", err)
	}
	resp, err := j.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("запрос JWKS: %w", err)
	}
	// Ошибку закрытия тела игнорировать можно только после того, как тело
	// прочитано: закрытие не влияет ни на статус, ни на разобранные ключи.
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("JWKS вернул статус %d", resp.StatusCode)
	}

	var payload struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			Alg string `json:"alg"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("разбор JWKS: %w", err)
	}

	out := make(map[string]*rsa.PublicKey, len(payload.Keys))
	for _, k := range payload.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		key, err := rsaFromJWK(k.N, k.E)
		if err != nil {
			// Один битый ключ не должен ломать весь набор: остальные
			// продолжат работать.
			continue
		}
		out[k.Kid] = key
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("в JWKS нет пригодных RSA-ключей")
	}
	return out, nil
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
	e := 0
	for _, b := range eBytes {
		e = e<<8 | int(b)
	}
	if e == 0 {
		return nil, fmt.Errorf("показатель ключа пуст")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: e}, nil
}
