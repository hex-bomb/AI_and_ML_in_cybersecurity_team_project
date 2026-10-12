package postgres

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
)

// Тесты идемпотентности (ТЗ 2.4). Поведение повторяет
// app/infrastructure/idempotency.py один в один.

// idempotencyKeyFor собирает ключ с фиксированными полями.
func idempotencyKeyFor(key, hash string) IdempotencyKey {
	return IdempotencyKey{
		TenantID:    tenantID,
		UserID:      "local-dev",
		Method:      "POST",
		Path:        "/api/v1/kb/" + kbID.String() + "/documents",
		Key:         key,
		RequestHash: hash,
	}
}

// TestIdempotencyLookupReturnsNilForUnusedKey — порт idempotency.py:37-69.
func TestIdempotencyLookupReturnsNilForUnusedKey(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := NewIdempotencyRepository(ports.SystemClock{}, 0)

	record, err := repo.Lookup(ctx, testDB, idempotencyKeyFor("k-1", "hash-a"))
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if record != nil {
		t.Fatalf("неиспользованный ключ вернул %+v", record)
	}
}

// TestIdempotencyRepeatWithSameBodyReturnsStoredResponse — главный сценарий
// ТЗ 2.4: повтор запроса с тем же ключом и тем же телом возвращает исходный
func TestIdempotencyRepeatWithSameBodyReturnsStoredResponse(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := NewIdempotencyRepository(ports.SystemClock{}, 0)
	key := idempotencyKeyFor("k-1", "hash-a")

	body := map[string]any{"document_id": docID.String(), "status": "queued"}
	if err := repo.Store(ctx, testDB, key, 201, body); err != nil {
		t.Fatalf("Store: %v", err)
	}

	record, err := repo.Lookup(ctx, testDB, idempotencyKeyFor("k-1", "hash-a"))
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if record == nil {
		t.Fatal("повтор с тем же телом не нашёл сохранённый ответ")
	}
	if record.ResponseStatus != 201 {
		t.Errorf("response_status = %d, ожидался 201", record.ResponseStatus)
	}
	if record.ResponseBody["document_id"] != docID.String() {
		t.Errorf("тело ответа искажено: %+v", record.ResponseBody)
	}
	if record.ExpiresAt.Before(record.CreatedAt) {
		t.Errorf("срок хранения короче момента записи: %v < %v", record.ExpiresAt, record.CreatedAt)
	}
}

// TestIdempotencyRepeatWithDifferentBodyIsConflict — порт idempotency.py:67-68.
// Тот же ключ с другим телом — 409 IDEMPOTENCY_CONFLICT, а не обработка
func TestIdempotencyRepeatWithDifferentBodyIsConflict(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := NewIdempotencyRepository(ports.SystemClock{}, 0)

	if err := repo.Store(ctx, testDB, idempotencyKeyFor("k-1", "hash-a"), 201,
		map[string]any{"document_id": docID.String()}); err != nil {
		t.Fatalf("Store: %v", err)
	}

	record, err := repo.Lookup(ctx, testDB, idempotencyKeyFor("k-1", "hash-b"))
	if !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("тот же ключ с другим телом: %v (record=%+v), ожидался ErrIdempotencyConflict", err, record)
	}

	// Сохранённый ответ не тронут: конфликт не должен его перетирать.
	record, err = repo.Lookup(ctx, testDB, idempotencyKeyFor("k-1", "hash-a"))
	if err != nil || record == nil {
		t.Fatalf("исходный ответ после конфликта: %v / %+v", err, record)
	}
}

// TestIdempotencyStoreKeepsFirstResponse — порт idempotency.py:82-100.
// ON CONFLICT DO NOTHING: повторная запись того же ключа не перезаписывает
func TestIdempotencyStoreKeepsFirstResponse(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := NewIdempotencyRepository(ports.SystemClock{}, 0)
	key := idempotencyKeyFor("k-1", "hash-a")

	if err := repo.Store(ctx, testDB, key, 201, map[string]any{"n": 1}); err != nil {
		t.Fatalf("первая запись: %v", err)
	}
	if err := repo.Store(ctx, testDB, key, 500, map[string]any{"n": 2}); err != nil {
		t.Fatalf("повторная запись: %v", err)
	}

	record, err := repo.Lookup(ctx, testDB, key)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if record.ResponseStatus != 201 {
		t.Errorf("response_status = %d, ожидался первый (201)", record.ResponseStatus)
	}
	if record.ResponseBody["n"] != float64(1) {
		t.Errorf("тело = %+v, ожидалось первое", record.ResponseBody)
	}
}

// TestIdempotencyExpiredKeyBehavesAsUnused — порт idempotency.py:63-66.
// Истёкший ключ действует как отсутствующий и удаляется. Без удаления через
func TestIdempotencyExpiredKeyBehavesAsUnused(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := NewIdempotencyRepository(ports.SystemClock{}, time.Hour)
	key := idempotencyKeyFor("k-1", "hash-a")

	if err := repo.Store(ctx, testDB, key, 201, map[string]any{"n": 1}); err != nil {
		t.Fatalf("Store: %v", err)
	}
	execRaw(t, ctx, "UPDATE api_idempotency SET expires_at = now() - interval '1 second'")

	record, err := repo.Lookup(ctx, testDB, key)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if record != nil {
		t.Fatalf("истёкший ключ вернул %+v", record)
	}

	var remaining int
	if err := testDB.QueryRow(ctx,
		"SELECT count(*) FROM api_idempotency WHERE key = $1", "k-1").Scan(&remaining); err != nil {
		t.Fatalf("подсчёт записей: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("истёкшая запись не удалена: осталось %d", remaining)
	}

	// После удаления ключ снова доступен.
	if err := repo.Store(ctx, testDB, idempotencyKeyFor("k-1", "hash-b"), 201,
		map[string]any{"n": 2}); err != nil {
		t.Fatalf("повторная запись: %v", err)
	}
}

// TestIdempotencyKeyIsScopedByTenantUserMethodAndPath — проверка без Python-теста.
// Ключ уникален на пятёрке полей, а не на самом ключе: одинаковая строка
func TestIdempotencyKeyIsScopedByTenantUserMethodAndPath(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := NewIdempotencyRepository(ports.SystemClock{}, 0)

	if err := repo.Store(ctx, testDB, idempotencyKeyFor("k-1", "hash-a"), 201,
		map[string]any{"who": "u1"}); err != nil {
		t.Fatalf("Store: %v", err)
	}

	variants := []func(key IdempotencyKey) IdempotencyKey{
		func(key IdempotencyKey) IdempotencyKey { key.UserID = "u2"; return key },
		func(key IdempotencyKey) IdempotencyKey { key.Method = "PUT"; return key },
		func(key IdempotencyKey) IdempotencyKey { key.Path = "/api/v1/other"; return key },
		func(key IdempotencyKey) IdempotencyKey {
			key.TenantID = uuid.MustParse("99999999-9999-4999-8999-999999999999")
			return key
		},
	}
	for i, mutate := range variants {
		variant := mutate(idempotencyKeyFor("k-1", "hash-a"))
		record, err := repo.Lookup(ctx, testDB, variant)
		if err != nil {
			t.Fatalf("вариант %d: %v", i, err)
		}
		if record != nil {
			t.Errorf("вариант %d нашёл чужую запись: %+v", i, record)
		}
	}
}

// TestIdempotencyRejectsBadKeyLength — порт idempotency.py:114-116.
// Проверка идёт до обращения к базе: пустой ключ означал бы, что идемпотентность
func TestIdempotencyRejectsBadKeyLength(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := NewIdempotencyRepository(ports.SystemClock{}, 0)

	tooLong := strings.Repeat("k", MaxIdempotencyKeyLength+1)
	for _, key := range []string{"", tooLong} {
		identity := idempotencyKeyFor(key, "hash-a")
		if _, err := repo.Lookup(ctx, testDB, identity); err == nil {
			t.Errorf("Lookup принял ключ длины %d", len(key))
		}
		if err := repo.Store(ctx, testDB, identity, 200, map[string]any{}); err == nil {
			t.Errorf("Store принял ключ длины %d", len(key))
		}
	}

	// Граница в 128 символов допустима.
	atLimit := strings.Repeat("k", MaxIdempotencyKeyLength)
	if err := repo.Store(ctx, testDB, idempotencyKeyFor(atLimit, "hash-a"), 200,
		map[string]any{}); err != nil {
		t.Errorf("Store отверг ключ допустимой длины: %v", err)
	}
}

// TestPurgeExpiredRemovesOnlyExpired — порт idempotency.py:107-110.
func TestPurgeExpiredRemovesOnlyExpired(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := NewIdempotencyRepository(ports.SystemClock{}, 0)

	for i, key := range []string{"old", "fresh"} {
		if err := repo.Store(ctx, testDB, idempotencyKeyFor(key, "hash-a"), 200,
			map[string]any{"i": i}); err != nil {
			t.Fatalf("Store: %v", err)
		}
	}
	execRaw(t, ctx, "UPDATE api_idempotency SET expires_at = now() - interval '1 second' WHERE key = $1", "old")

	removed, err := repo.PurgeExpired(ctx, testDB)
	if err != nil {
		t.Fatalf("PurgeExpired: %v", err)
	}
	if removed != 1 {
		t.Fatalf("удалено %d записей, ожидалась 1", removed)
	}
	if record, err := repo.Lookup(ctx, testDB, idempotencyKeyFor("fresh", "hash-a")); err != nil || record == nil {
		t.Fatalf("свежая запись потеряна: %v / %+v", err, record)
	}
}

// TestRequestHashMatchesPythonFormula — эталоны посчитаны Python; второй случай
// с «&» ловит расхождение экранирования Go/Python (иначе повтор не найдёт ответ).
func TestRequestHashMatchesPythonFormula(t *testing.T) {
	cases := []struct {
		name     string
		content  []byte
		metadata map[string]any
		want     string
	}{
		{
			name:     "пустое тело и пустые метаданные",
			content:  nil,
			metadata: nil,
			want:     "f54410417cf7dc264fec6e77bdd662fe056f44011cd7a987ecc4c9cfbf4e6441",
		},
		{
			name:     "URL с HTML-символами в метаданных",
			content:  []byte("файл"),
			metadata: map[string]any{"source_url": "https://example.org/a?x=1&y=2"},
			want:     "ccbaacdd6292bc69d43b90194f60dd528d1c67d638a72aa4145d1d7e709b80b8",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RequestHash(tc.content, tc.metadata)
			if err != nil {
				t.Fatalf("RequestHash: %v", err)
			}
			if got != tc.want {
				t.Errorf("хеш = %s, ожидался %s (расхождение с Python)", got, tc.want)
			}
		})
	}
}

// TestRequestHashIsStableAndSensitive — проверка без Python-теста.
// Порядок ключей и содержимое влияют на хеш; содержимое файла — тоже.
func TestRequestHashIsStableAndSensitive(t *testing.T) {
	first, err := RequestHash([]byte("a"), map[string]any{"b": 1, "a": "x"})
	if err != nil {
		t.Fatalf("RequestHash: %v", err)
	}
	// Тот же запрос в другом порядке ключей Go map даёт тот же хеш.
	second, err := RequestHash([]byte("a"), map[string]any{"a": "x", "b": 1})
	if err != nil {
		t.Fatalf("RequestHash: %v", err)
	}
	if first != second {
		t.Errorf("порядок ключей повлиял на хеш: %s != %s", first, second)
	}

	differentContent, err := RequestHash([]byte("b"), map[string]any{"a": "x", "b": 1})
	if err != nil {
		t.Fatalf("RequestHash: %v", err)
	}
	if differentContent == first {
		t.Error("содержимое файла не повлияло на хеш")
	}

	differentMeta, err := RequestHash([]byte("a"), map[string]any{"a": "y", "b": 1})
	if err != nil {
		t.Fatalf("RequestHash: %v", err)
	}
	if differentMeta == first {
		t.Error("метаданные не повлияли на хеш")
	}
}

// TestRequestHashNormalizesIntegralNumbers — проверка без Python-теста.
// «384» и «384.0» — один и тот же запрос: без нормализации клиент, приславший
func TestRequestHashNormalizesIntegralNumbers(t *testing.T) {
	asInt, err := RequestHash(nil, map[string]any{"n": jsonNumber("384")})
	if err != nil {
		t.Fatalf("RequestHash: %v", err)
	}
	asFloat, err := RequestHash(nil, map[string]any{"n": jsonNumber("384.0")})
	if err != nil {
		t.Fatalf("RequestHash: %v", err)
	}
	if asInt != asFloat {
		t.Errorf("целое и «целое с точкой» дали разные хеши: %s != %s", asInt, asFloat)
	}
}

// jsonNumber создаёт json.Number — так выглядит число, разобранное
// json.Decoder с UseNumber.
func jsonNumber(literal string) json.Number {
	return json.Number(literal)
}
