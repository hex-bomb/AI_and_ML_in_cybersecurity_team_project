package documents

import (
	domain "github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
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
