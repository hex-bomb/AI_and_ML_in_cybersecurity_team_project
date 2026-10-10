package documents_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/kbsearch/backend-go/internal/application/documents"
	"github.com/kbsearch/backend-go/internal/domain"
)

// TestConvertIsNotImplemented locks the stub: no silent empty parse.
func TestConvertIsNotImplemented(t *testing.T) {
	_, err := documents.Convert(context.Background(), []byte("# stub\n"), documents.ConversionRequest{Filename: "stub.md"})
	if !errors.Is(err, documents.ErrConverterNotImplemented) {
		t.Fatalf("ожидалась заглушка ErrConverterNotImplemented, получено: %v", err)
	}
	appErr, ok := domain.AsAppError(err)
	if !ok || appErr.Status != http.StatusNotImplemented {
		t.Fatalf("заглушка должна маппиться в 501, получено: %v", err)
	}
}
