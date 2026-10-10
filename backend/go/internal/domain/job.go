package domain

import (
	"time"

	"github.com/google/uuid"
)

const (
	StageConvert = "convert"
	StageIndex   = "index"
	StageDelete  = "delete"
	StageCleanup = "cleanup"
)

const (
	KindIngest         = "ingest"
	KindDelete         = "delete"
	KindCleanupVersion = "cleanup_version"
)

const (
	JobStatusQueued    = "queued"
	JobStatusRunning   = "running"
	JobStatusSucceeded = "succeeded"
	JobStatusFailed    = "failed"
	JobStatusCancelled = "cancelled"
)

const (
	VersionStatusUploaded = "uploaded"
	VersionStatusParsing  = "parsing"
	VersionStatusParsed   = "parsed"
	VersionStatusIndexing = "indexing"
	VersionStatusReady    = "ready"
	VersionStatusFailed   = "failed"
)

// LeaseDuration is the job lease TTL.
const LeaseDuration = 90 * time.Second

// HeartbeatInterval must stay at LeaseDuration/3.
const HeartbeatInterval = 30 * time.Second

// GoStages returns stages served by the Go worker.
//
// Порядок значим для чтения: convert идёт раньше index, и так же выглядит
// цепочка стадий одного документа — загрузка ставит convert, convert ставит
// index. Список открыт для Claim: чужую стадию воркер не заберёт.
func GoStages() []string { return []string{StageConvert, StageIndex} }

// JobLease is a claimed job with its lease token.
type JobLease struct {
	JobID      uuid.UUID
	TenantID   uuid.UUID
	DocumentID uuid.UUID
	VersionID  *uuid.UUID
	// KBID заполняется стадией convert: без него ParsedDocument остался бы
	// без базы знаний, и индекс не смог бы взять профиль этой базы.
	KBID           uuid.UUID
	Kind           string
	Stage          string
	Attempt        int
	LeaseToken     string
	LeaseExpiresAt time.Time
}

// IsIndexStage reports the index stage.
func (l JobLease) IsIndexStage() bool { return l.Stage == StageIndex }

// IsConvertStage reports the convert stage.
func (l JobLease) IsConvertStage() bool { return l.Stage == StageConvert }

// VersionIDValue returns the version id or false.
func (l JobLease) VersionIDValue() (uuid.UUID, bool) {
	if l.VersionID == nil {
		return uuid.UUID{}, false
	}
	return *l.VersionID, true
}

// ExpiredAt treats equality as expired.
func (l JobLease) ExpiredAt(t time.Time) bool {
	return !t.Before(l.LeaseExpiresAt)
}
