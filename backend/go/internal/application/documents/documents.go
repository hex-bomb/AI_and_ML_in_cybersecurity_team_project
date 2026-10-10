// Package documents — приём документов; конвертер пока заглушка.
//
// WHY: разбор MD/DOCX/PDF в ParsedDocument не написан — Convert явно
// отвечает 501, вместо того чтобы молча отдавать пустой документ.
package documents

import (
	"context"

	"github.com/google/uuid"
	"github.com/kbsearch/backend-go/internal/domain"
)

// ConversionRequest identifies the version being converted.
type ConversionRequest struct {
	TenantID   uuid.UUID
	KBID       uuid.UUID
	DocumentID uuid.UUID
	VersionID  uuid.UUID
	Title      string
	Filename   string
	SourceURL  *string
	Metadata   map[string]any
}

// Convert is a stub that always reports ErrConverterNotImplemented.
func Convert(ctx context.Context, _ []byte, _ ConversionRequest) (domain.ParsedDocument, error) {
	if err := ctx.Err(); err != nil {
		return domain.ParsedDocument{}, err
	}
	return domain.ParsedDocument{}, ErrConverterNotImplemented
}
