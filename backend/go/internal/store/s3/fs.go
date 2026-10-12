package storage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	domain "github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
)

// Path is built ONLY from the server key; the client filename never enters
// it. Unsafe keys are rejected, never silently cleaned.
var segmentRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

const MaxKeyLength = 512

const (
	dirMode  fs.FileMode = 0o700
	fileMode fs.FileMode = 0o600
)

// Local implements ports.FileStorage on the local filesystem.
type Local struct {
	root string
}

var ErrUnsafeKey = errors.New("недопустимый ключ хранилища")

func New(root string) (*Local, error) {
	trimmed := strings.TrimSpace(root)
	if trimmed == "" {
		return nil, domain.ServiceUnavailable("FILE_STORAGE_ROOT не задан").
			WithCause(errors.New("корень хранилища обязателен"))
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		return nil, domain.ServiceUnavailable("не удалось разобрать корень хранилища").
			WithCause(err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, domain.ServiceUnavailable("корень хранилища недоступен").WithCause(err)
		}
		resolved = abs
	}
	if err := os.MkdirAll(resolved, dirMode); err != nil {
		return nil, domain.ServiceUnavailable("не удалось создать корень хранилища").WithCause(err)
	}
	return &Local{root: resolved}, nil
}

func (s *Local) Root() string { return s.root }

func OriginalKey(tenantID, versionID string) string {
	return tenantID + "/" + versionID + "/" + "original.bin"
}

func ParsedKey(tenantID, versionID string) string {
	return tenantID + "/" + versionID + "/" + "parsed.json"
}

func (s *Local) PathFor(key string) (string, error) {
	segments, err := splitKey(key)
	if err != nil {
		return "", err
	}
	path := filepath.Join(append([]string{s.root}, segments...)...)
	rel, err := filepath.Rel(s.root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", domain.ServiceUnavailable("ключ выходит за пределы хранилища").
			WithCause(fmt.Errorf("%w: %q", ErrUnsafeKey, key))
	}
	return path, nil
}

func splitKey(key string) ([]string, error) {
	if key == "" || len(key) > MaxKeyLength {
		return nil, domain.ServiceUnavailable("недопустимая длина ключа хранилища").
			WithCause(fmt.Errorf("%w: длина %d", ErrUnsafeKey, len(key)))
	}
	parts := strings.Split(key, "/")
	for _, part := range parts {
		if !segmentRE.MatchString(part) {
			return nil, domain.ServiceUnavailable("недопустимый ключ хранилища").
				WithCause(fmt.Errorf("%w: %q", ErrUnsafeKey, key))
		}
	}
	return parts, nil
}

// Put writes atomically via temp file + rename so readers never see a half.
func (s *Local) Put(ctx context.Context, key string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return domain.ServiceUnavailable("операция отменена").WithCause(err)
	}
	path, err := s.PathFor(key)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return domain.ServiceUnavailable("не удалось создать каталог хранилища").WithCause(err)
	}
	// Unique temp name: a shared name let two writers publish partial data
	// through the same inode after the first rename.
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return domain.ServiceUnavailable("не удалось создать временный файл хранилища").WithCause(err)
	}
	tmpName := tmp.Name()
	if err := writeTmp(tmp, data); err != nil {
		_ = os.Remove(tmpName)
		return domain.ServiceUnavailable("не удалось записать файл хранилища").WithCause(err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return domain.ServiceUnavailable("не удалось опубликовать файл хранилища").WithCause(err)
	}
	return nil
}

func writeTmp(f *os.File, data []byte) error {
	if err := f.Chmod(fileMode); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func (s *Local) Read(ctx context.Context, key string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, domain.ServiceUnavailable("операция отменена").WithCause(err)
	}
	path, err := s.PathFor(key)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, domain.NotFound("файл не найден в хранилище").
				WithCause(fmt.Errorf("%w: %q", fs.ErrNotExist, key))
		}
		return nil, domain.ServiceUnavailable("не удалось прочитать файл хранилища").WithCause(err)
	}
	return data, nil
}

func (s *Local) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return domain.ServiceUnavailable("операция отменена").WithCause(err)
	}
	path, err := s.PathFor(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return domain.ServiceUnavailable("не удалось удалить файл хранилища").WithCause(err)
	}
	return nil
}

var _ ports.FileStorage = (*Local)(nil)
