package ports

import (
	"context"

	"github.com/kbsearch/backend-go/internal/domain"
)

// DocumentRepository reads documents and switches the active version.
type DocumentRepository interface {
	GetAllowedActiveVersions(ctx context.Context, q Queryer, p domain.Principal, kbID string) ([]domain.ActiveVersion, error)
	LoadParsed(ctx context.Context, q Queryer, store FileStorage, tenantID, versionID string) (domain.ParsedDocument, error)
	VersionSources(ctx context.Context, q Queryer, p domain.Principal, kbID string, active map[string]string) (map[string]string, error)
	ActivateVersion(ctx context.Context, tx Queryer, p domain.ActivateParams) (previousVersionID string, err error)
	IngestBusy(ctx context.Context, q Queryer, documentID string) (bool, error)
	IsVersionActive(ctx context.Context, q Queryer, documentID, versionID string) (bool, error)
}

// KBRepository holds knowledge bases and ACLs.
type KBRepository interface {
	Create(ctx context.Context, q Queryer, p domain.Principal, in domain.CreateKBInput) (domain.KB, error)
	List(ctx context.Context, q Queryer, p domain.Principal, limit int, cursor string) ([]domain.KB, string, error)
	Require(ctx context.Context, q Queryer, p domain.Principal, kbID string) (domain.KB, error)
	SetACL(ctx context.Context, tx Queryer, kbID string, in domain.ACL) error
	SetDocumentACL(ctx context.Context, tx Queryer, documentID string, override *domain.ACL) error
	EmbeddingProfileID(ctx context.Context, q Queryer, kbID string) (string, error)
}

// SettingsRepository is read-only; values are managed by an admin.
type SettingsRepository interface {
	Levels(ctx context.Context, q Queryer, tenantID, kbID string) (kbLevel, tenantLevel map[string]any, err error)
}

// JobRepository serves ONLY the index stage.
type JobRepository interface {
	Claim(ctx context.Context, q Queryer, workerID string, stages []string) (*domain.JobLease, error)
	Heartbeat(ctx context.Context, q Queryer, jobID, leaseToken string) (bool, error)
	FinishIndex(ctx context.Context, tx Queryer, jobID, leaseToken string, res domain.IndexingResult) error
	Fail(ctx context.Context, tx Queryer, jobID, leaseToken, code, message string, retryable bool) error
	EnqueueCleanup(ctx context.Context, tx Queryer, tenantID, documentID, versionID string) (string, error)
}

// DocumentsRepository stores documents and versions.
type DocumentsRepository interface {
	Create(ctx context.Context, tx Queryer, p domain.Principal, in domain.DocumentCreateParams) (domain.DocumentCreateResult, error)
	List(ctx context.Context, q Queryer, p domain.Principal, kbID string, limit int, cursor string) ([]domain.DocumentRecord, string, error)
	Get(ctx context.Context, q Queryer, p domain.Principal, documentID string) (domain.DocumentRecord, error)
	MarkDeleted(ctx context.Context, tx Queryer, p domain.Principal, documentID string) (string, error)
	Retry(ctx context.Context, tx Queryer, p domain.Principal, documentID string) (domain.DocumentRetryResult, error)
}

// DocumentIdempotencyStore deduplicates document writes.
type DocumentIdempotencyStore interface {
	Lookup(ctx context.Context, q Queryer, key domain.DocumentIdempotencyKey) (*domain.DocumentIdempotencyRecord, error)
	Store(ctx context.Context, q Queryer, key domain.DocumentIdempotencyKey, status int, body map[string]any) error
}

// Authorizer holds access rules.
type Authorizer interface {
	RequireAdmin(p domain.Principal) error
	RequireRead(p domain.Principal, kbACL domain.ACL) error
}
