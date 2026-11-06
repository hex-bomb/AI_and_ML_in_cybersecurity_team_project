package jwks

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"testing"
	"time"
)

// stubRoundTripper отдаёт готовый ответ JWKS без сети.
type stubRoundTripper struct {
	body []byte
	code int
}

func (s stubRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: s.code,
		Body:       io.NopCloser(bytes.NewReader(s.body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// verifierWith возвращает верификатор, отдающий указанный JWKS.
func verifierWith(t *testing.T, key *rsa.PrivateKey, kid string) *Verifier {
	t.Helper()
	pub := key.Public().(*rsa.PublicKey)
	body := mustJSON(map[string]any{"keys": []map[string]string{{
		"kty": "RSA",
		"kid": kid,
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}})
	v := NewVerifier("http://jwks.stub")
	v.hc = &http.Client{Transport: stubRoundTripper{body: body, code: http.StatusOK}}
	return v
}

// sign возвращает готовый RS256-токен.
func sign(t *testing.T, key *rsa.PrivateKey, kid string, exp int64) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString(mustJSON(map[string]string{"alg": "RS256", "kid": kid}))
	payload := base64.RawURLEncoding.EncodeToString(mustJSON(map[string]any{
		"sub": "s", "role": "employee", "scp": []string{"public", "employee"},
		"grp": []string{"legal"}, "exp": exp,
	}))
	signingInput := header + "." + payload
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func newKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestVerifyAcceptsValidToken(t *testing.T) {
	key := newKey(t)
	v := verifierWith(t, key, "k1")
	token := sign(t, key, "k1", time.Now().Add(time.Hour).Unix())

	claims, err := v.Verify(context.Background(), token)
	if err != nil {
		t.Fatalf("валидный токен отклонён: %v", err)
	}
	if claims.Role != "employee" || len(claims.Grp) != 1 || claims.Grp[0] != "legal" {
		t.Errorf("claims разобраны неверно: %+v", claims)
	}
	if len(claims.Channels()) != 2 {
		t.Errorf("сотрудник должен видеть оба канала: %v", claims.Channels())
	}
}

func TestVerifyRejectsExpired(t *testing.T) {
	key := newKey(t)
	v := verifierWith(t, key, "k1")
	token := sign(t, key, "k1", time.Now().Add(-time.Hour).Unix())

	if _, err := v.Verify(context.Background(), token); err == nil {
		t.Fatal("просроченный токен должен отклоняться")
	}
}

func TestVerifyRejectsUnknownKid(t *testing.T) {
	key := newKey(t)
	other := newKey(t)
	v := verifierWith(t, key, "k1")
	// Подписан чужим ключом: kid берётся из заголовка токена и не найден в JWKS.
	token := sign(t, other, "unknown", time.Now().Add(time.Hour).Unix())

	if _, err := v.Verify(context.Background(), token); err == nil {
		t.Fatal("токен с неизвестным kid должен отклоняться")
	}
}

func TestVerifyRejectsMalformed(t *testing.T) {
	v := NewVerifier("http://unused")
	for _, token := range []string{"", "a", "a.b", "a.b.c.d"} {
		if _, err := v.Verify(context.Background(), token); err == nil {
			t.Errorf("мусорный токен %q должен отклоняться", token)
		}
	}
}
