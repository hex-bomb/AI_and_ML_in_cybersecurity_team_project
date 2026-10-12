package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/google/uuid"

	"github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
)

// DocumentsRepository — запись и чтение документов (загрузочный порт; писатель один — Go).

var _ ports.DocumentsRepository = (*DocumentsRepository)(nil)

// Алиасы доменных типов для сигнатур порта.
type (
	CreateParams = domain.DocumentCreateParams
	CreateResult = domain.DocumentCreateResult
	RetryResult  = domain.DocumentRetryResult
	Record       = domain.DocumentRecord
	Version      = domain.DocumentVersion
)

// DocumentsRepository — реализация операций над документами.
type DocumentsRepository struct{}

// NewDocumentsRepository создаёт репозиторий документов.
func NewDocumentsRepository() *DocumentsRepository { return &DocumentsRepository{} }

// Create создаёт документ с версией и заданием конвертации — всё в одной транзакции.
func (r *DocumentsRepository) Create(
	ctx context.Context, tx ports.Queryer, p domain.Principal, in CreateParams,
) (CreateResult, error) {
	metadata, err := marshalJSONB("metadata", in.Metadata)
	if err != nil {
		return CreateResult{}, err
	}

	documentID := in.DocumentID
	if documentID == uuid.Nil {
		documentID = uuid.New()
		if err := r.insertDocument(ctx, tx, p, documentID, in); err != nil {
			return CreateResult{}, err
		}
	} else if err := r.requireDocument(ctx, tx, p, documentID); err != nil {
		return CreateResult{}, err
	}

	if err := r.insertVersion(ctx, tx, p, documentID, in, metadata); err != nil {
		return CreateResult{}, err
	}

	// latest_version_id — отдельным UPDATE после вставки версии.
	if err := r.setLatestVersion(ctx, tx, p, documentID, in.VersionID); err != nil {
		return CreateResult{}, err
	}

	jobID, err := r.enqueue(ctx, tx, p, documentID, in.VersionID, domain.KindIngest, domain.StageConvert)
	if err != nil {
		return CreateResult{}, err
	}
	return CreateResult{
		DocumentID: documentID, VersionID: in.VersionID, JobID: jobID,
	}, nil
}

func (r *DocumentsRepository) insertDocument(
	ctx context.Context, tx ports.Queryer, p domain.Principal,
	documentID uuid.UUID, in CreateParams,
) error {
	// external_id уникален в базе: повтор падает в DOCUMENT_EXISTS.
	_, err := tx.Exec(ctx, `
		INSERT INTO documents (id, tenant_id, kb_id, external_id, created_at)
		VALUES ($1, $2, $3, $4, now())`,
		documentID, p.TenantID, in.KBID, nullIfEmpty(in.Filename),
	)
	return wrapDBError(err)
}

func (r *DocumentsRepository) insertVersion(
	ctx context.Context, tx ports.Queryer, p domain.Principal,
	documentID uuid.UUID, in CreateParams, metadata []byte,
) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO document_versions (
			id, tenant_id, document_id, filename, title, source_url,
			metadata, content_sha256, original_key, status, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now())`,
		in.VersionID, p.TenantID, documentID, in.Filename, in.Title,
		nullIfEmpty(in.SourceURL), metadata, in.ContentSHA256, in.OriginalKey,
		domain.VersionStatusUploaded,
	)
	if err != nil {
		return wrapDBError(err)
	}
	return nil
}

// enqueue ставит задание в очередь.
func (r *DocumentsRepository) enqueue(
	ctx context.Context, tx ports.Queryer, p domain.Principal,
	documentID, versionID uuid.UUID, kind, stage string,
) (uuid.UUID, error) {
	jobID := uuid.New()
	_, err := tx.Exec(ctx, `
		INSERT INTO jobs (
			id, tenant_id, document_id, version_id, kind, stage,
			status, attempt, next_attempt_at, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, 0, now(), now(), now())`,
		jobID, p.TenantID, documentID, versionID, kind, stage, domain.JobStatusQueued,
	)
	if err != nil {
		return uuid.Nil, wrapDBError(err)
	}
	return jobID, nil
}

// setLatestVersion проставляет документу последнюю версию.
func (r *DocumentsRepository) setLatestVersion(
	ctx context.Context, tx ports.Queryer, p domain.Principal,
	documentID, versionID uuid.UUID,
) error {
	if _, err := tx.Exec(ctx, `
		UPDATE documents SET latest_version_id = $1
		WHERE id = $2 AND tenant_id = $3`,
		versionID, documentID, p.TenantID,
	); err != nil {
		return wrapDBError(err)
	}
	return nil
}

// requireDocument проверяет, что документ принадлежит principal.
func (r *DocumentsRepository) requireDocument(
	ctx context.Context, tx ports.Queryer, p domain.Principal, documentID uuid.UUID,
) error {
	var found bool
	row := tx.QueryRow(ctx, `
		SELECT true FROM documents
		WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL`,
		documentID, p.TenantID,
	)
	// Ошибка скана здесь — отсутствие строки (прочее уже обернул queryer).
	if err := row.Scan(&found); err != nil || !found {
		return errDocumentNotFound()
	}
	return nil
}

// List возвращает страницу документов базы и курсор следующей.
func (r *DocumentsRepository) List(
	ctx context.Context, q ports.Queryer, p domain.Principal,
	kbID string, limit int, cursor string,
) ([]Record, string, error) {
	kbUUID, err := parseUUID("kb_id", kbID)
	if err != nil {
		return nil, "", err
	}
	limit = clampLimit(limit)

	var args []any
	next := func(value any) string {
		args = append(args, value)
		return "$" + strconv.Itoa(len(args))
	}

	where := "WHERE d.tenant_id = " + next(p.TenantID) + " AND d.kb_id = " + next(kbUUID) +
		" AND d.deleted_at IS NULL"
	aclClause, aclArgs := accessFilter(p, len(args)+1)
	where += " AND " + aclClause
	args = append(args, aclArgs...)

	if cursor != "" {
		createdAt, lastID, err := decodeCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		where += " AND (d.created_at, d.id) < (" + next(createdAt) + ", " + next(lastID) + ")"
	}
	limitArg := next(limit + 1)

	query := `
		SELECT d.id, d.kb_id, d.external_id, d.created_at, d.tenant_id,
		       d.acl_override, d.latest_version_id, d.active_version_id
		FROM documents d
		JOIN knowledge_bases kb ON kb.id = d.kb_id AND kb.tenant_id = d.tenant_id
		` + where + `
		ORDER BY d.created_at DESC, d.id DESC LIMIT ` + limitArg

	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, "", wrapDBError(err)
	}
	records, total, err := r.collectRecords(rows, limit)
	if err != nil {
		return nil, "", err
	}
	if err := r.loadVersions(ctx, q, records); err != nil {
		return nil, "", err
	}
	return records, nextRecordCursor(records, limit, total), nil
}

// Get возвращает документ с версиями (чужой/удалённый/нет — один NOT_FOUND).
func (r *DocumentsRepository) Get(
	ctx context.Context, q ports.Queryer, p domain.Principal, documentID string,
) (Record, error) {
	docUUID, err := parseUUID("document_id", documentID)
	if err != nil {
		return Record{}, errDocumentNotFound()
	}
	aclClause, aclArgs := accessFilter(p, 3)
	rows, err := q.Query(ctx, `
		SELECT d.id, d.kb_id, d.external_id, d.created_at, d.tenant_id,
		       d.acl_override, d.latest_version_id, d.active_version_id
		FROM documents d
		JOIN knowledge_bases kb ON kb.id = d.kb_id AND kb.tenant_id = d.tenant_id
		WHERE d.tenant_id = $1 AND d.id = $2 AND d.deleted_at IS NULL
		  AND `+aclClause+`
		ORDER BY d.created_at DESC, d.id DESC LIMIT 1`,
		append([]any{p.TenantID, docUUID}, aclArgs...)...)
	if err != nil {
		return Record{}, wrapDBError(err)
	}
	records, _, err := r.collectRecords(rows, 1)
	if err != nil {
		return Record{}, err
	}
	if len(records) == 0 {
		return Record{}, errDocumentNotFound()
	}
	if err := r.loadVersions(ctx, q, records); err != nil {
		return Record{}, err
	}
	return records[0], nil
}

// MarkDeleted помечает документ удалённым и ставит задание удаления.
func (r *DocumentsRepository) MarkDeleted(
	ctx context.Context, tx ports.Queryer, p domain.Principal, documentID string,
) (string, error) {
	docUUID, err := parseUUID("document_id", documentID)
	if err != nil {
		return "", errDocumentNotFound()
	}
	if err := r.requireDocument(ctx, tx, p, docUUID); err != nil {
		return "", err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE documents SET deleted_at = now()
		WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL`,
		docUUID, p.TenantID,
	)
	if err != nil {
		return "", wrapDBError(err)
	}
	if tag.RowsAffected() == 0 {
		return "", errDocumentNotFound()
	}
	jobID, err := r.enqueue(
		ctx, tx, p, docUUID, uuid.Nil, domain.KindDelete, domain.StageDelete,
	)
	if err != nil {
		return "", err
	}
	return jobID.String(), nil
}

// Retry снимает failed-статус с последней версии и ставит её заново.
func (r *DocumentsRepository) Retry(
	ctx context.Context, tx ports.Queryer, p domain.Principal, documentID string,
) (RetryResult, error) {
	docUUID, err := parseUUID("document_id", documentID)
	if err != nil {
		return RetryResult{}, errDocumentNotFound()
	}
	var versionID uuid.UUID
	row := tx.QueryRow(ctx, `
		SELECT v.id FROM document_versions v
		JOIN documents d ON d.id = v.document_id AND d.tenant_id = v.tenant_id
		WHERE v.document_id = $1 AND v.tenant_id = $2
		  AND v.status = $3 AND d.deleted_at IS NULL
		ORDER BY v.created_at DESC LIMIT 1`,
		docUUID, p.TenantID, domain.VersionStatusFailed,
	)
	if err := row.Scan(&versionID); err != nil {
		return RetryResult{}, domain.Conflict("Неуспешной версии нет — повторять нечего")
	}
	if _, err := tx.Exec(ctx, `
		UPDATE document_versions
		SET status = $1, failed_stage = NULL, error_code = NULL, error_message = NULL
		WHERE id = $2 AND tenant_id = $3`,
		domain.VersionStatusUploaded, versionID, p.TenantID,
	); err != nil {
		return RetryResult{}, wrapDBError(err)
	}
	jobID, err := r.enqueue(
		ctx, tx, p, docUUID, versionID, domain.KindIngest, domain.StageConvert,
	)
	if err != nil {
		return RetryResult{}, err
	}
	return RetryResult{VersionID: versionID, JobID: jobID}, nil
}

// collectRecords читает страницу (версии — отдельным запросом, иначе JOIN ломает пагинацию).
func (r *DocumentsRepository) collectRecords(rows ports.Rows, limit int) ([]Record, int, error) {
	var (
		items []Record
		total int
	)
	err := collect(rows, func() error {
		var (
			record      Record
			externalID  *string
			aclOverride []byte
			latestID    *uuid.UUID
			activeID    *uuid.UUID
		)
		if err := rows.Scan(
			&record.DocumentID, &record.KBID, &externalID, &record.CreatedAt, &record.TenantID,
			&aclOverride, &latestID, &activeID,
		); err != nil {
			return err
		}
		if externalID != nil {
			record.ExternalID = *externalID
		}
		if len(aclOverride) > 0 {
			var acl domain.ACL
			if err := json.Unmarshal(aclOverride, &acl); err != nil {
				return fmt.Errorf("acl_override документа %s: %w", record.DocumentID, err)
			}
			record.ACLOverride = &acl
		}
		record.SetVersionIDs(latestID, activeID)
		items = append(items, record)
		total++
		return nil
	})
	if err != nil {
		return nil, 0, wrapDBError(err)
	}
	if total > limit {
		items = items[:limit]
	}
	return items, total, nil
}

// nextRecordCursor строит курсор на последнюю отданную строку.
func nextRecordCursor(items []Record, limit, total int) string {
	if total <= limit || len(items) == 0 {
		return ""
	}
	last := items[len(items)-1]
	return encodeCursor(last.CreatedAt, last.DocumentID)
}

// loadVersions добирает версии одним запросом на всю страницу.
func (r *DocumentsRepository) loadVersions(
	ctx context.Context, q ports.Queryer, records []Record,
) error {
	if len(records) == 0 {
		return nil
	}
	byID := make(map[string]*Record, len(records))
	ids := make([]uuid.UUID, 0, len(records)*2)
	for i := range records {
		record := &records[i]
		byID[record.DocumentID] = record
		if record.LatestVersionID() != nil {
			ids = append(ids, *record.LatestVersionID())
		}
		if record.ActiveVersionID() != nil {
			ids = append(ids, *record.ActiveVersionID())
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := q.Query(ctx, `
		SELECT id, filename, title, source_url, status, failed_stage,
		       error_code, error_message, chunk_count, original_key,
		       parsed_key, content_sha256, metadata, created_at
		FROM document_versions
		WHERE tenant_id = $1 AND id = ANY($2)`,
		records[0].TenantID, ids)
	if err != nil {
		return wrapDBError(err)
	}
	return collect(rows, func() error {
		version, scanErr := scanVersion(rows)
		if scanErr != nil {
			return scanErr
		}
		// Версия принадлежит ровно одному документу страницы.
		for _, record := range byID {
			if record.LatestVersionID() != nil && version.ID == record.LatestVersionID().String() {
				copied := version
				record.Latest = &copied
				return nil
			}
			if record.ActiveVersionID() != nil && version.ID == record.ActiveVersionID().String() {
				copied := version
				record.Active = &copied
				return nil
			}
		}
		return nil
	})
}

// scanVersion читает версию документа.
func scanVersion(row ports.Row) (Version, error) {
	var (
		version                           Version
		sourceURL, failedStage, errorCode *string
		errorText                         *string
		parsedKey                         *string
		metadata                          []byte
	)
	err := row.Scan(
		&version.ID, &version.Filename, &version.Title, &sourceURL,
		&version.Status, &failedStage, &errorCode, &errorText,
		&version.ChunkCount, &version.OriginalKey, &parsedKey,
		&version.ContentSHA256, &metadata, &version.CreatedAt,
	)
	if err != nil {
		return Version{}, err
	}
	version.SourceURL = deref(sourceURL)
	version.FailedStage = deref(failedStage)
	version.ErrorCode = deref(errorCode)
	version.ErrorText = deref(errorText)
	version.ParsedKey = deref(parsedKey)
	if len(metadata) > 0 {
		if err := json.Unmarshal(metadata, &version.Metadata); err != nil {
			return Version{}, fmt.Errorf("metadata версии %s: %w", version.ID, err)
		}
	}
	return version, nil
}

// marshalJSONB кодирует объект в jsonb (nil — в {}, колонка NOT NULL).
func marshalJSONB(name string, value map[string]any) ([]byte, error) {
	if value == nil {
		return []byte(`{}`), nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("кодирование %s: %w", name, err)
	}
	return raw, nil
}

// nullIfEmpty превращает пустую строку в NULL.
func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
