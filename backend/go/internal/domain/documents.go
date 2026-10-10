package domain

import (
	"time"

	"github.com/google/uuid"
)

// DocumentVersion is a document version as served to scenarios.
type DocumentVersion struct {
	ID            string
	Filename      string
	Title         string
	SourceURL     string
	Status        string
	FailedStage   string
	ErrorCode     string
	ErrorText     string
	ChunkCount    int
	OriginalKey   string
	ParsedKey     string
	ContentSHA256 string
	Metadata      map[string]any
	CreatedAt     time.Time
}

// DocumentRecord is a document with its latest and active versions.
type DocumentRecord struct {
	DocumentID  string
	TenantID    uuid.UUID
	KBID        string
	ExternalID  string
	CreatedAt   time.Time
	Latest      *DocumentVersion
	Active      *DocumentVersion
	ACLOverride *ACL

	latestVersion *uuid.UUID
	activeVersion *uuid.UUID
}

// SetVersionIDs stores latest/active version ids.
func (r *DocumentRecord) SetVersionIDs(latest, active *uuid.UUID) {
	r.latestVersion = latest
	r.activeVersion = active
}

// LatestVersionID returns a copy of the latest version id.
func (r DocumentRecord) LatestVersionID() *uuid.UUID {
	if r.latestVersion == nil {
		return nil
	}
	v := *r.latestVersion
	return &v
}

// ActiveVersionID returns a copy of the active version id.
func (r DocumentRecord) ActiveVersionID() *uuid.UUID {
	if r.activeVersion == nil {
		return nil
	}
	v := *r.activeVersion
	return &v
}

// SearchableVersion prefers the active version, then the latest.
func (r DocumentRecord) SearchableVersion() (DocumentVersion, bool) {
	if r.Active != nil {
		return *r.Active, true
	}
	if r.Latest != nil {
		return *r.Latest, true
	}
	return DocumentVersion{}, false
}

// DocumentCreateParams is the document-or-new-version creation input.
type DocumentCreateParams struct {
	KBID          uuid.UUID
	DocumentID    uuid.UUID
	VersionID     uuid.UUID
	Filename      string
	Title         string
	SourceURL     string
	Metadata      map[string]any
	ContentSHA256 string
	OriginalKey   string
}

// DocumentCreateResult holds created identifiers.
type DocumentCreateResult struct {
	DocumentID uuid.UUID
	VersionID  uuid.UUID
	JobID      uuid.UUID
}

// DocumentRetryResult is the requeued version and job.
type DocumentRetryResult struct {
	VersionID uuid.UUID
	JobID     uuid.UUID
}

// DocumentIdempotencyKey identifies a request.
type DocumentIdempotencyKey struct {
	TenantID    uuid.UUID
	UserID      string
	Method      string
	Path        string
	Key         string
	RequestHash string
}

// DocumentIdempotencyRecord is the stored original response.
type DocumentIdempotencyRecord struct {
	Status int
	Body   map[string]any
}

// ActiveVersion is a {document_id, active_version_id} pair.
type ActiveVersion struct {
	DocumentID      string
	ActiveVersionID string
}

// ActivateParams is the ActivateVersion input.
type ActivateParams struct {
	TenantID           string
	DocumentID         string
	VersionID          string
	EmbeddingProfileID string
	ChunkCount         int
	LeaseToken         string
}

// IsActiveVersion reports whether versionID is the active one.
func IsActiveVersion(versionID string, active *string) bool {
	return active != nil && *active == versionID
}
