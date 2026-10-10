package documents

import (
	"net/http"

	"github.com/kbsearch/backend-go/internal/domain"
)

// ErrConverterNotImplemented reports that the converter is not written yet.
var ErrConverterNotImplemented = &domain.AppError{
	Code:    "CONVERTER_NOT_IMPLEMENTED",
	Status:  http.StatusNotImplemented,
	Message: "Конвертер документов пока не реализован",
}
