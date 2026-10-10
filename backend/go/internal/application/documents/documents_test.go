package documents_test

import (
	"context"
	"errors"
	"testing"

	"github.com/kbsearch/backend-go/internal/application/documents"
	"github.com/kbsearch/backend-go/internal/domain"
)

// TestConvertReturnsErrorInsteadOfEmptyDocument — конвертер больше не
// заглушка, но контракт остался: негодный файл обязан дать ошибку домена, а
// не пустой ParsedDocument. Пустой документ, ушедший в индекс, выглядел бы
// для оператора как успешно обработанный документ, который не ищется.
func TestConvertReturnsErrorInsteadOfEmptyDocument(t *testing.T) {
	for _, test := range []struct {
		name     string
		filename string
		content  []byte
	}{
		{name: "пустой markdown", filename: "empty.md", content: []byte("\n\n")},
		{name: "пустой текст", filename: "empty.txt", content: []byte("   ")},
		{name: "пустой docx", filename: "empty.docx", content: buildDocxRaw(nil)},
	} {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := documents.Convert(
				context.Background(), test.content, conversionRequest(test.filename))

			if err == nil {
				t.Fatalf("ожидалась ошибка, получен ParsedDocument с %d блоками",
					len(parsed.Blocks))
			}
			appErr, ok := domain.AsAppError(err)
			if !ok {
				t.Fatalf("ошибка не AppError: %v", err)
			}
			if appErr.Retryable() {
				t.Errorf("код %s не должен повторяться: файл не станет годным от повтора",
					appErr.Code)
			}
			if len(parsed.Blocks) != 0 {
				t.Errorf("вместе с ошибкой возвращено %d блоков", len(parsed.Blocks))
			}
		})
	}
}

// TestUnsupportedExtensionIsNotConverted — файл чужого вида отклоняется до
// разбора: разбирать нечего, и «пустой успех» списал бы документ в ноль.
func TestUnsupportedExtensionIsNotConverted(t *testing.T) {
	_, err := documents.Convert(context.Background(), []byte("данные"), conversionRequest("data.zip"))
	if !errors.Is(err, domain.UnsupportedFormat("")) {
		t.Fatalf("получено %v вместо UNSUPPORTED_FORMAT", err)
	}
}
