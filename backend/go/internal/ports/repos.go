package ports

import (
	"context"

	domain "github.com/kbsearch/kb/internal/envelope"
)

// InheritRights помечает документ, у которого НЕТ собственного ACL на сервере.
//
// Зачем отдельное значение. Ответ оракула — domain.DocumentRights, а этот тип
// различает только два состояния: «контейнер плюс исключения» и «ничего». Решений
// на самом деле три, и все три обязаны быть различимы:
//
//	documents.acl_override IS NULL  → наследование: контейнер из конверта
//	override с принципалами        → контейнер СБРАСЫВАЕТСЯ, acl = принципалы
//	override с пустыми списками     → закрыт всем: ни контейнера, ни acl
//
// Третье состояние обязано отличаться от первого: пустой override по коду
// accessFilterSQL (internal/store/pg/acl.go) означает «закрыто для всех, кроме
// администратора», а NULL — «действует ACL базы». Смешать их — вернуть целому
// контейнеру документ, который администратор закрыл лично, то есть утечка.
//
// Значение выбрано fail-closed: если маркер забудут срезать, документ получит
// space_id, которого нет ни в одном acl_containers, и не совпадёт ни с одним
// термом фильтра — то есть будет невидим НИКОМУ. Обратная ошибка (забытый срез,
// превращённый в наследование) стоила бы выдачи документа всему контейнеру.
//
// NUL внутри не может встретиться в данных: PostgreSQL не принимает нулевой байт
// ни в text, ни в jsonb («unsupported Unicode escape sequence»), а метаданные
// конверта лежат в document_versions.metadata. Объявить такой space_id клиент не
// может даже при желании.
const InheritRights = "\x00kb:inherit-rights"

// DocumentRepository reads documents and switches the active version.
type DocumentRepository interface {
	GetAllowedActiveVersions(ctx context.Context, q Queryer, p domain.Principal, kbID string) ([]domain.ActiveVersion, error)
	// DocumentRights спрашивает у сервера действующие права документа: что
	// администратор записал в documents.acl_override.
	//
	// Права документа приезжают в индекс из двух источников, и только один из
	// них — серверный: конверт (§4, «acl — список принципалов») собирает
	// коннектор, а переопределение ACL документа ставит администратор в Postgres.
	// Раньше конвейер читал только конверт, и ограничение, поставленное
	// администратором, молча терялось: документ с acl_override = {anna},
	// лежащий в контейнере space-hr, индексировался как видимый ВСЕМ в space-hr.
	//
	// Поэтому оракул обязателен для стадии index, а объявленное клиентом может
	// только уступить ему, но не расшириться им.
	//
	// Три состояния ответа (см. InheritRights):
	//
	//   - SpaceID == InheritRights — acl_override IS NULL, серверного мнения нет,
	//     действует объявленный конвертом контейнер;
	//   - иначе ответ авторитетен целиком: SpaceID обязан быть пустым, ACL —
	//     перевод legacy-списков в словарь §6. Контейнер при переопределении не
	//     наследуется: фильтр индекса — это ИЛИ (terms(space_id) либо terms(acl)),
	//     и оставленный контейнер вернул бы документ всему контейнеру вопреки
	//     ограничению;
	//   - ACL пустой при непустом SpaceID — ошибка вызывающего, а не «прав нет».
	//
	// Ошибка (нет документа, чужой тенант, недоступная база) обязана
	// останавливать стадию индексации: версия не активируется, документ не уезжает
	// в индекс, то есть отказ закрытый.
	DocumentRights(ctx context.Context, q Queryer, tenantID, documentID string) (domain.DocumentRights, error)
	LoadParsed(ctx context.Context, q Queryer, store FileStorage, tenantID, versionID string) (domain.ParsedDocument, error)
	// LoadVersion reads the version row together with its original bytes —
	// what the convert stage parses.
	LoadVersion(ctx context.Context, q Queryer, store FileStorage, tenantID, versionID string) (domain.ConvertVersion, error)
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

// JobRepository serves the convert and index stages.
type JobRepository interface {
	Claim(ctx context.Context, q Queryer, workerID string, stages []string) (*domain.JobLease, error)
	Heartbeat(ctx context.Context, q Queryer, jobID, leaseToken string) (bool, error)
	// FinishConvert marks the version parsed and advances the job to the
	// index stage (ТЗ 3.6 advance_to_index).
	FinishConvert(ctx context.Context, tx Queryer, jobID, leaseToken string, res domain.ConversionResult) error
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
