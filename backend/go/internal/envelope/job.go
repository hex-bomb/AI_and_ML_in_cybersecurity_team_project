package domain

import (
	"time"

	"github.com/google/uuid"
)

// Стадии ЗАДАНИЯ. Это другой набор, чем стадии lake (Stage в event.go), и
// названия у них совпадают не случайно: задание стадии delete обслуживается и
// кладёт в lake событие стадии DeleteStage.
//
// StageCleanup остаётся за пределами GoStages: уборку версии обслуживает не
// этот воркер, и забирать её оттуда нельзя (задание уборки удаляет версию из
// индекса, и забрал бы её под работающий с ней поиск).
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
//
// StageDelete в списке с тех пор, как у надгробия появился потребитель
// (§4, «удаление — через событие-надгробие»; §9, «документ, удалённый в
// источнике, исчезает из выдачи»). MarkDeleted и раньше клал такое задание в
// очередь, но обслуживать его было некому, и чанки удалённого документа
// оставались в индексе навсегда. Удаление стоит последним: оно не продолжает
// цепочку загрузки, а приходит отдельным заданием от MarkDeleted.
func GoStages() []string { return []string{StageConvert, StageIndex, StageDelete} }

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

// IsDeleteStage reports the delete stage.
func (l JobLease) IsDeleteStage() bool { return l.Stage == StageDelete }

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
