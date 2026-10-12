package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	domain "github.com/kbsearch/kb/internal/envelope"
)

const (
	testTenant  = "0f9a1c2e-0000-4000-8000-000000000001"
	testVersion = "0f9a1c2e-0000-4000-8000-0000000000ff"
)

func newTestStorage(t *testing.T) *Local {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestKeysHaveExpectedShape(t *testing.T) {
	if got, want := OriginalKey(testTenant, testVersion), testTenant+"/"+testVersion+"/original.bin"; got != want {
		t.Errorf("OriginalKey = %q, want %q", got, want)
	}
	if got, want := ParsedKey(testTenant, testVersion), testTenant+"/"+testVersion+"/parsed.json"; got != want {
		t.Errorf("ParsedKey = %q, want %q", got, want)
	}
}

func TestNewRejectsEmptyRoot(t *testing.T) {
	if _, err := New("  "); err == nil {
		t.Fatal("New с пустым корнем должен вернуть ошибку")
	}
}

func TestPathForRejectsUnsafeKeys(t *testing.T) {
	unsafe := []string{
		"../escape.bin",
		"/etc/passwd",
		"tenant/../../etc/passwd",
		"a/b/../../../x",
		"",
		"tenant//version",
		"tenant/./x",
		"tenant/ ver/x",
		"tenant/ver\x00sion",
		"tenant\\version",
		"..",
		".",
	}
	s := newTestStorage(t)
	for _, key := range unsafe {
		t.Run(key, func(t *testing.T) {
			path, err := s.PathFor(key)
			if err == nil {
				t.Fatalf("ключ %q принят, путь %q", key, path)
			}
			if !errors.Is(err, ErrUnsafeKey) {
				t.Errorf("ключ %q отвергнут не ErrUnsafeKey: %v", key, err)
			}
		})
	}
}

func TestPathForRejectsTooLongKey(t *testing.T) {
	s := newTestStorage(t)
	long := ""
	for i := 0; i < MaxKeyLength+10; i++ {
		long += "a"
	}
	if _, err := s.PathFor(long); !errors.Is(err, ErrUnsafeKey) {
		t.Fatalf("слишком длинный ключ не отвергнут: %v", err)
	}
}

func TestPathForKeepsPathInsideRoot(t *testing.T) {
	s := newTestStorage(t)
	path, err := s.PathFor(OriginalKey(testTenant, testVersion))
	if err != nil {
		t.Fatalf("PathFor: %v", err)
	}
	rel, err := filepath.Rel(s.Root(), path)
	if err != nil || filepath.IsAbs(rel) {
		t.Fatalf("путь %q вне корня %q: %v", path, s.Root(), err)
	}
	if filepath.Base(path) != "original.bin" {
		t.Errorf("базовое имя %q, ожидалось original.bin", filepath.Base(path))
	}
	if filepath.Base(path) == "guide.pdf" {
		t.Error("клиентское имя файла попало в путь")
	}
}

func TestPutReadDeleteRoundtrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	key := OriginalKey(testTenant, testVersion)

	if err := s.Put(ctx, key, []byte("payload")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Read(ctx, key)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != "payload" {
		t.Errorf("Read = %q, ожидалось payload", got)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("повторный Delete: %v", err)
	}
	if _, err := s.Read(ctx, key); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Read отсутствующего ключа = %v, ожидался ErrNotFound", err)
	}
}

func TestPutOverwritesAtomically(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	key := OriginalKey(testTenant, testVersion)

	if err := s.Put(ctx, key, []byte("first")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Put(ctx, key, []byte("second")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Read(ctx, key)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != "second" {
		t.Errorf("Read = %q, ожидалось second", got)
	}
}

func TestPutLeavesNoTemporaryFile(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	key := ParsedKey(testTenant, testVersion)
	if err := s.Put(ctx, key, []byte("{}")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(mustPath(t, s, key)))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	prefix := filepath.Base(key) + ".tmp"
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" || strings.HasPrefix(e.Name(), prefix) {
			t.Errorf("остался временный файл %q", e.Name())
		}
	}
}

func TestPutConcurrentSameKeyNeverPublishesPartialFile(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	key := OriginalKey(testTenant, testVersion)

	blobs := make([][]byte, 6)
	for i := range blobs {
		blobs[i] = bytes.Repeat([]byte{byte('a' + i)}, (i+1)*1024*1024)
	}
	whole := make(map[string]bool, len(blobs))
	for _, blob := range blobs {
		whole[string(blob)] = true
	}

	var (
		reads   atomic.Int64
		badRead = make(chan string, 1)
	)

	for round := range 8 {
		stop := make(chan struct{})
		var reader sync.WaitGroup
		reader.Add(1)
		go func() {
			defer reader.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				got, err := s.Read(ctx, key)
				if err != nil {
					continue // файл ещё не опубликован — это штатно
				}
				reads.Add(1)
				if !whole[string(got)] {
					select {
					case badRead <- fmt.Sprintf(
						"раунд %d: читатель увидел %d байт, которых нет ни у одного писателя",
						round, len(got)):
					default:
					}
					return
				}
			}
		}()

		var writers sync.WaitGroup
		for i := range blobs {
			writers.Add(1)
			go func(data []byte) {
				defer writers.Done()
				_ = s.Put(ctx, key, data)
			}(blobs[i])
		}
		writers.Wait()
		close(stop)
		reader.Wait()
	}

	select {
	case msg := <-badRead:
		t.Fatal(msg)
	default:
	}
	if reads.Load() == 0 {
		t.Fatal("читатель ни разу не увидел файл — гонку проверить не удалось")
	}

	final, err := s.Read(ctx, key)
	if err != nil {
		t.Fatalf("Read после конкурентных Put: %v", err)
	}
	if !whole[string(final)] {
		t.Errorf("после гонки опубликован не целый блоб: %d байт", len(final))
	}
}

func TestPutCreatesDirectoriesWithPrivateMode(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	key := OriginalKey(testTenant, testVersion)
	if err := s.Put(ctx, key, []byte("данные")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	dir, err := os.Stat(filepath.Dir(mustPath(t, s, key)))
	if err != nil {
		t.Fatalf("Stat каталога: %v", err)
	}
	if mode := dir.Mode().Perm(); mode != dirMode {
		t.Errorf("режим каталога = %o, ожидался %o", mode, dirMode)
	}
	file, err := os.Stat(mustPath(t, s, key))
	if err != nil {
		t.Fatalf("Stat файла: %v", err)
	}
	if mode := file.Mode().Perm(); mode != fileMode {
		t.Errorf("режим файла = %o, ожидался %o", mode, fileMode)
	}
}

func TestPutAndReadRespectCancelledContext(t *testing.T) {
	s := newTestStorage(t)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	if err := s.Put(cancelled, OriginalKey(testTenant, testVersion), []byte("x")); err == nil {
		t.Error("Put с отменённым контекстом выполнился")
	}
	if _, err := s.Read(cancelled, OriginalKey(testTenant, testVersion)); err == nil {
		t.Error("Read с отменённым контекстом выполнился")
	}
	if err := s.Delete(cancelled, OriginalKey(testTenant, testVersion)); err == nil {
		t.Error("Delete с отменённым контекстом выполнился")
	}
}

func TestParsedDocumentRoundtrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	key := ParsedKey(testTenant, testVersion)

	parsed := domain.ParsedDocument{
		SchemaVersion: "1",
		TenantID:      uuid.MustParse(testTenant),
		KBID:          uuid.MustParse("0f9a1c2e-0000-4000-8000-0000000000aa"),
		DocumentID:    uuid.MustParse("0f9a1c2e-0000-4000-8000-0000000000bb"),
		VersionID:     uuid.MustParse(testVersion),
		Title:         "Инструкция",
		Filename:      "guide.pdf",
		ContentSHA256: "abc",
		Blocks: []domain.Block{{
			BlockID:     uuid.MustParse("0f9a1c2e-0000-4000-8000-0000000000cc"),
			Position:    0,
			Kind:        "paragraph",
			Text:        "текст",
			SectionPath: []string{"Раздел"},
		}},
	}
	raw, err := json.Marshal(parsed)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := s.Put(ctx, key, raw); err != nil {
		t.Fatalf("Put: %v", err)
	}
	back, err := s.Read(ctx, key)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var decoded domain.ParsedDocument
	if err := json.Unmarshal(back, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.VersionID != parsed.VersionID || decoded.Title != parsed.Title ||
		len(decoded.Blocks) != 1 || decoded.Blocks[0].Text != "текст" {
		t.Errorf("ParsedDocument не пережил цикл: %+v", decoded)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Read(ctx, key); err == nil {
		t.Error("файл остался после Delete")
	}
}

func mustPath(t *testing.T, s *Local, key string) string {
	t.Helper()
	path, err := s.PathFor(key)
	if err != nil {
		t.Fatalf("PathFor(%q): %v", key, err)
	}
	return path
}
