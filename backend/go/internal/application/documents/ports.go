package documents

import (
	"github.com/kbsearch/backend-go/internal/application/ports"
	"github.com/kbsearch/backend-go/internal/domain"
)

// Aliases keep scenario names short; ports live in application/ports.
type (
	Version           = domain.DocumentVersion
	Record            = domain.DocumentRecord
	CreateParams      = domain.DocumentCreateParams
	CreateResult      = domain.DocumentCreateResult
	RetryResult       = domain.DocumentRetryResult
	IdempotencyKey    = domain.DocumentIdempotencyKey
	IdempotencyRecord = domain.DocumentIdempotencyRecord
	Repository        = ports.DocumentsRepository
	IdempotencyStore  = ports.DocumentIdempotencyStore
)
