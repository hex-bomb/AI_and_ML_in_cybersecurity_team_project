package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/kbsearch/kb/internal/envelope"
)

// maxMultipartMemory — буфер multipart-парсера в памяти до сброса на диск.
const maxMultipartMemory = 8 << 20

// Лимиты поля metadata формы загрузки: сырой JSON не больше 64 КБ,
// source_url — только http/https не длиннее 2048 символов, title — 500 рун.
const (
	maxMetadataBytes = 64 << 10
	maxSourceURLLen  = 2048
	maxTitleRunes    = 500
)

// readUpload разбирает multipart-загрузку и отклоняет негодный файл до очереди (ТЗ 5.5).
func (h *apiHandler) readUpload(r *http.Request) (UploadInput, error) {
	if err := r.ParseMultipartForm(maxMultipartMemory); err != nil {
		return UploadInput{}, domain.InvalidMetadata("Ожидается multipart/form-data")
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	var in UploadInput
	in.IdempotencyKey = r.Header.Get("Idempotency-Key")

	md, err := readMetadata(r)
	if err != nil {
		return UploadInput{}, err
	}
	if md != nil {
		in.Title = strings.TrimSpace(md.Title)
		if md.SourceURL != nil {
			in.SourceURL = *md.SourceURL
		}
		if md.Metadata != nil {
			in.Metadata = md.Metadata
		}
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		return UploadInput{}, domain.InvalidMetadata("Файл не передан: ожидается поле file")
	}
	defer func() { _ = file.Close() }()

	in.Filename = filepath.Base(header.Filename)
	if in.Filename == "" || in.Filename == "." || in.Filename == "/" {
		return UploadInput{}, domain.InvalidMetadata("Имя файла обязательно")
	}
	if in.Title == "" {
		in.Title = in.Filename
	}
	if utf8.RuneCountInString(in.Title) > maxTitleRunes {
		return UploadInput{}, domain.InvalidMetadata("title превышает 500 символов")
	}

	// Размер по заголовку до чтения, чтобы не грузить 2 ГБ ради 413.
	if header.Size > h.cfg.MaxUploadBytes {
		return UploadInput{}, domain.FileTooLarge(
			"Файл превышает допустимый размер",
		).WithDetails(map[string]any{
			"size":     header.Size,
			"max_size": h.cfg.MaxUploadBytes,
		})
	}
	if !extensionAllowed(in.Filename, h.cfg.AllowedUploadExts) {
		return UploadInput{}, domain.UnsupportedFormat(
			"Формат файла не поддерживается",
		).WithDetails(map[string]any{
			"filename": in.Filename,
			"allowed":  h.cfg.AllowedUploadExts,
		})
	}

	// Байт сверх лимита ловит врущий Content-Length заголовка.
	content, err := io.ReadAll(io.LimitReader(file, h.cfg.MaxUploadBytes+1))
	if err != nil {
		return UploadInput{}, domain.InvalidMetadata("Не удалось прочитать файл")
	}
	if int64(len(content)) > h.cfg.MaxUploadBytes {
		return UploadInput{}, domain.FileTooLarge("Файл превышает допустимый размер").
			WithDetails(map[string]any{"max_size": h.cfg.MaxUploadBytes})
	}
	if len(content) == 0 {
		return UploadInput{}, domain.InvalidMetadata("Файл пуст")
	}
	in.Content = content
	return in, nil
}

// uploadMetadata — поле metadata формы загрузки.
type uploadMetadata struct {
	Title     string         `json:"title"`
	SourceURL *string        `json:"source_url"`
	Metadata  map[string]any `json:"metadata"`
}

// readMetadata читает поле metadata (пустое — не ошибка, битый JSON — ошибка).
func readMetadata(r *http.Request) (*uploadMetadata, error) {
	raw := r.FormValue("metadata")
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	if len(raw) > maxMetadataBytes {
		return nil, domain.InvalidMetadata("Поле metadata превышает 64 КБ")
	}
	var md uploadMetadata
	if err := json.Unmarshal([]byte(raw), &md); err != nil {
		return nil, domain.InvalidMetadata("Поле metadata не является корректным JSON")
	}
	if md.SourceURL != nil {
		if err := validateSourceURL(*md.SourceURL); err != nil {
			return nil, err
		}
	}
	return &md, nil
}

// validateSourceURL — allowlist схем http/https: иначе поле уносит
// произвольные URI (javascript:, file:) в хранимый source_url.
func validateSourceURL(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	if len(raw) > maxSourceURLLen {
		return domain.InvalidMetadata("source_url превышает допустимую длину")
	}
	u, err := url.Parse(trimmed)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return domain.InvalidMetadata("source_url допускает только http/https")
	}
	return nil
}

func extensionAllowed(filename string, allowed []string) bool {
	ext := strings.ToLower(filepath.Ext(filename))
	if ext == "" {
		return false
	}
	for _, a := range allowed {
		if strings.ToLower(a) == ext {
			return true
		}
	}
	return false
}

// urlEscape кодирует имя файла для Content-Disposition.
func urlEscape(name string) string { return url.PathEscape(name) }
