package documents

import (
	"bytes"
	"context"
	"io"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/kbsearch/backend-go/internal/domain"
)

// MaxFileBytes caps the original file size.
const MaxFileBytes = 20 * 1024 * 1024

// SupportedFormats lists the extensions accepted on intake.
var SupportedFormats = []string{".md", ".docx", ".pdf"}

// IsSupported reports whether the filename has an accepted extension.
func IsSupported(filename string) bool {
	suffix := strings.ToLower(path.Ext(filepathToSlash(filename)))
	for _, format := range SupportedFormats {
		if suffix == format {
			return true
		}
	}
	return false
}

// readChunk bounds how much is buffered while reading a stream.
const readChunk = 64 * 1024

// ReadOriginal reads a version original without exceeding the size limit.
func ReadOriginal(ctx context.Context, reader io.Reader) ([]byte, error) {
	buffer := make([]byte, 0, readChunk)
	chunk := make([]byte, readChunk)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		read, err := reader.Read(chunk)
		if read > 0 {
			if len(buffer)+read > MaxFileBytes {
				return nil, domain.FileTooLarge("Размер файла превышает 20 MiB")
			}
			buffer = append(buffer, chunk[:read]...)
		}
		if err != nil {
			if err == io.EOF {
				return buffer, nil
			}
			return nil, err
		}
	}
}

// ValidateFormat checks the filename extension and file signature.
func ValidateFormat(ctx context.Context, filename string, content []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	suffix := strings.ToLower(path.Ext(filepathToSlash(filename)))
	switch suffix {
	case ".md", ".docx", ".pdf":
	default:
		return "", domain.UnsupportedFormat("Поддерживаются MD, DOCX и текстовые PDF")
	}
	switch suffix {
	case ".pdf":
		if !bytesHavePrefixTrimmed(content, 1024, "%PDF-") {
			return "", domain.InvalidMetadata("Файл не содержит корректный PDF")
		}
	case ".docx":
		if !bytes.HasPrefix(content, []byte("PK")) {
			return "", domain.InvalidMetadata("Файл не содержит корректный DOCX")
		}
	case ".md":
		if !utf8.Valid(content) {
			return "", domain.InvalidMetadata("Markdown должен использовать UTF-8")
		}
	}
	return suffix, nil
}

func filepathToSlash(filename string) string {
	return strings.ReplaceAll(filename, `\`, "/")
}

func bytesHavePrefixTrimmed(content []byte, window int, prefix string) bool {
	if len(content) > window {
		content = content[:window]
	}
	return bytes.HasPrefix(bytes.TrimLeft(content, " \t\r\n\v\f"), []byte(prefix))
}
