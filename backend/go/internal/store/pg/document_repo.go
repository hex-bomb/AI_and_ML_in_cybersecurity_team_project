package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	domain "github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
)

// DocumentRepository — реализация ports.DocumentRepository: чтение для поиска
// и активация версии (запись — у DocumentsRepository).
type DocumentRepository struct {
	clock ports.Clock
}

// Интерфейс проверяется компилятором.
var _ ports.DocumentRepository = (*DocumentRepository)(nil)

// NewDocumentRepository создаёт репозиторий документов (clock — для проверки lease).
func NewDocumentRepository(clock ports.Clock) *DocumentRepository {
	return &DocumentRepository{clock: clock}
}

// GetAllowedActiveVersions возвращает активные версии, доступные principal (ТЗ 3.5).
// Пустой список — no_results без обращения к индексу (ТЗ 2.1).
func (r *DocumentRepository) GetAllowedActiveVersions(
	ctx context.Context, q ports.Queryer, p domain.Principal, kbID string,
) ([]domain.ActiveVersion, error) {
	kbUUID, err := parseUUID("kb_id", kbID)
	if err != nil {
		return nil, err
	}
	// Плейсхолдеры $1..$2 заняты, фильтр доступа — с третьего.
	filter, filterArgs := accessFilter(p, 3)
	query := `
		SELECT d.id, d.active_version_id
		FROM documents d
		JOIN knowledge_bases kb ON kb.id = d.kb_id
		WHERE d.tenant_id = $1
		  AND kb.tenant_id = $1
		  AND d.kb_id = $2
		  AND d.deleted_at IS NULL
		  AND ` + filter

	rows, err := q.Query(ctx, query, append([]any{p.TenantID, kbUUID}, filterArgs...)...)
	if err != nil {
		return nil, err
	}

	items := []domain.ActiveVersion{}
	err = collect(rows, func() error {
		var (
			docID     uuid.UUID
			versionID *uuid.UUID
		)
		if err := rows.Scan(&docID, &versionID); err != nil {
			return err
		}
		if versionID == nil {
			return nil
		}
		items = append(items, domain.ActiveVersion{
			DocumentID:      docID.String(),
			ActiveVersionID: versionID.String(),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return items, nil
}

// VersionSources возвращает source_url версий из PostgreSQL (не из индекса —
// он может отставать), с проверкой доступа в глубину. Без source_url — пропуск.
func (r *DocumentRepository) VersionSources(
	ctx context.Context, q ports.Queryer, p domain.Principal, kbID string, active map[string]string,
) (map[string]string, error) {
	sources := map[string]string{}
	if len(active) == 0 {
		return sources, nil
	}
	kbUUID, err := parseUUID("kb_id", kbID)
	if err != nil {
		return nil, err
	}
	// В active нужны только значения (version_id).
	versionIDs := make([]uuid.UUID, 0, len(active))
	for _, rawVersionID := range active {
		versionID, err := parseUUID("active version id", rawVersionID)
		if err != nil {
			return nil, err
		}
		versionIDs = append(versionIDs, versionID)
	}

	// Плейсхолдеры $1..$3 заняты.
	filter, filterArgs := accessFilter(p, 4)
	query := `
		SELECT v.id, v.source_url
		FROM document_versions v
		JOIN documents d ON d.id = v.document_id
		JOIN knowledge_bases kb ON kb.id = d.kb_id
		WHERE v.tenant_id = $1
		  AND d.tenant_id = $1
		  AND d.kb_id = $2
		  AND d.deleted_at IS NULL
		  AND v.id = ANY($3::uuid[])
		  AND ` + filter

	rows, err := q.Query(ctx, query, append([]any{p.TenantID, kbUUID, versionIDs}, filterArgs...)...)
	if err != nil {
		return nil, err
	}

	err = collect(rows, func() error {
		var (
			versionID uuid.UUID
			sourceURL *string
		)
		if err := rows.Scan(&versionID, &sourceURL); err != nil {
			return err
		}
		if sourceURL != nil {
			sources[versionID.String()] = *sourceURL
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return sources, nil
}

// LoadParsed читает разобранный документ версии из файлового хранилища (ключ — из БД).
func (r *DocumentRepository) LoadParsed(
	ctx context.Context, q ports.Queryer, store ports.FileStorage, tenantID, versionID string,
) (domain.ParsedDocument, error) {
	tenant, err := parseUUID("tenant_id", tenantID)
	if err != nil {
		return domain.ParsedDocument{}, err
	}
	version, err := parseUUID("version_id", versionID)
	if err != nil {
		return domain.ParsedDocument{}, err
	}

	var parsedKey *string
	err = q.QueryRow(ctx,
		"SELECT parsed_key FROM document_versions WHERE id = $1 AND tenant_id = $2",
		version, tenant).Scan(&parsedKey)
	if err != nil {
		return domain.ParsedDocument{}, noRows(err, "Версия документа не найдена")
	}
	if parsedKey == nil || *parsedKey == "" {
		return domain.ParsedDocument{}, domain.NotFound("ParsedDocument для версии отсутствует")
	}

	raw, err := store.Read(ctx, *parsedKey)
	if err != nil {
		return domain.ParsedDocument{}, err
	}
	var parsed domain.ParsedDocument
	if err := json.Unmarshal(raw, &parsed); err != nil {
		// Нечитаемый JSON — дефект конвертера, наружу внутренняя ошибка.
		return domain.ParsedDocument{}, (&domain.AppError{
			Code:    domain.CodeInternalError,
			Status:  500,
			Message: "Файл разобранного документа не читается",
			Cause:   err,
		})
	}
	return parsed, nil
}

// LoadVersion читает версию вместе с байтами оригинала — что разбирает стадия
// convert.
//
// Атрибуты версии и её байты читаются одним вызовом намеренно: два отдельных
// чтения — это два снимка состояния, и между ними Retry успел бы сменить
// статус версии, из-за чего разобранное соответствовало бы не той версии,
// которую воркер держит под lease.
func (r *DocumentRepository) LoadVersion(
	ctx context.Context, q ports.Queryer, store ports.FileStorage, tenantID, versionID string,
) (domain.ConvertVersion, error) {
	tenant, err := parseUUID("tenant_id", tenantID)
	if err != nil {
		return domain.ConvertVersion{}, err
	}
	version, err := parseUUID("version_id", versionID)
	if err != nil {
		return domain.ConvertVersion{}, err
	}

	var (
		out           domain.ConvertVersion
		sourceURL     *string
		originalKey   string
		contentSHA256 string
		metadata      []byte
	)
	err = q.QueryRow(ctx, `
		SELECT filename, title, source_url, content_sha256, original_key, metadata
		FROM document_versions WHERE id = $1 AND tenant_id = $2`,
		version, tenant).Scan(
		&out.Filename, &out.Title, &sourceURL,
		&contentSHA256, &originalKey, &metadata,
	)
	if err != nil {
		return domain.ConvertVersion{}, noRows(err, "Версия документа не найдена")
	}

	out.SourceURL = sourceURL
	out.ContentSHA256 = contentSHA256
	if len(metadata) > 0 {
		if err := json.Unmarshal(metadata, &out.Metadata); err != nil {
			return domain.ConvertVersion{}, fmt.Errorf("metadata версии %s: %w", version, err)
		}
	}

	// Пустой original_key — дефект загрузки: читать нечего, и молча
	// разобрать пустой документ означало бы записать в parsed.json то, что
	// стадия index потом сочтёт содержимым.
	if originalKey == "" {
		return domain.ConvertVersion{}, domain.NotFound("Оригинал версии не сохранён")
	}
	content, err := store.Read(ctx, originalKey)
	if err != nil {
		return domain.ConvertVersion{}, err
	}
	out.Content = content
	return out, nil
}

// ActivateVersion переключает active_version_id (возвращает прежнюю для cleanup).
// Порядок блокировок фиксирован: KB -> document -> job -> version.
// Транзакцию открывает вызывающий (одна с FinishIndex, ТЗ 6.3 п.7).
func (r *DocumentRepository) ActivateVersion(
	ctx context.Context, tx ports.Queryer, p domain.ActivateParams,
) (string, error) {
	tenant, err := parseUUID("tenant_id", p.TenantID)
	if err != nil {
		return "", err
	}
	docUUID, err := parseUUID("document_id", p.DocumentID)
	if err != nil {
		return "", err
	}
	versionUUID, err := parseUUID("version_id", p.VersionID)
	if err != nil {
		return "", err
	}

	// 1. База знаний документа (без блокировки — только для шага 2).
	var kbID uuid.UUID
	if err := tx.QueryRow(ctx,
		"SELECT kb_id FROM documents WHERE id = $1 AND tenant_id = $2",
		docUUID, tenant).Scan(&kbID); err != nil {
		return "", noRows(err, "Документ не найден")
	}

	// 2. Блокировка KB.
	var lockedProfile *string
	if err := tx.QueryRow(ctx,
		"SELECT embedding_profile_id FROM knowledge_bases WHERE id = $1 FOR UPDATE",
		kbID).Scan(&lockedProfile); err != nil {
		return "", noRows(err, "База знаний не найдена")
	}

	// 3. Блокировка документа.
	var (
		activeVersion *uuid.UUID
		latestVersion *uuid.UUID
		deletedAt     *time.Time
	)
	if err := tx.QueryRow(ctx,
		"SELECT active_version_id, latest_version_id, deleted_at FROM documents WHERE id = $1 FOR UPDATE",
		docUUID).Scan(&activeVersion, &latestVersion, &deletedAt); err != nil {
		return "", noRows(err, "Документ не найден")
	}
	if deletedAt != nil {
		return "", domain.NotFound("Документ удалён")
	}

	// 4. Задание обязано быть running, иначе версия видна раньше чанков.
	var jobID uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT id FROM jobs
		WHERE document_id = $1
		  AND kind = 'ingest'
		  AND version_id = $2
		  AND status = 'running'
		LIMIT 1`, docUUID, versionUUID).Scan(&jobID)
	if err != nil {
		return "", noRows(err, "Задание индексации не найдено")
	}

	// 5. Проверка lease (единственная в приложении, ТЗ 3.6).
	if _, err := lockRunningJob(ctx, r.clock, tx, jobID, p.LeaseToken); err != nil {
		return "", err
	}

	// 6. Только последняя версия (иначе молчаливый откат).
	if latestVersion == nil || *latestVersion != versionUUID {
		return "", domain.Conflict("Активируется не последняя версия документа")
	}

	// 7. Блокировка версии.
	if err := lockVersion(ctx, tx, versionUUID, docUUID); err != nil {
		return "", err
	}

	// 8. Профиль фиксируется при первой активации (ТЗ 6.3 п.7).
	switch {
	case lockedProfile == nil:
		if _, err := tx.Exec(ctx,
			"UPDATE knowledge_bases SET embedding_profile_id = $2 WHERE id = $1",
			kbID, p.EmbeddingProfileID); err != nil {
			return "", err
		}
	case *lockedProfile != p.EmbeddingProfileID:
		return "", domain.EmbeddingProfileNeedsRebuild(
			fmt.Sprintf("профиль базы зафиксирован как %s, а версия индексирована профилем %s",
				*lockedProfile, p.EmbeddingProfileID))
	}

	previous := ""
	if activeVersion != nil && *activeVersion != versionUUID {
		previous = activeVersion.String()
	}
	if _, err := tx.Exec(ctx,
		"UPDATE documents SET active_version_id = $2 WHERE id = $1",
		docUUID, versionUUID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE document_versions
		SET status = 'ready', chunk_count = $3, embedding_profile_id = $4
		WHERE id = $1 AND document_id = $2`,
		versionUUID, docUUID, p.ChunkCount, p.EmbeddingProfileID); err != nil {
		return "", err
	}
	return previous, nil
}

// IngestBusy сообщает, идёт ли загрузка документа (повторяет uq_jobs_active_ingest).
func (r *DocumentRepository) IngestBusy(ctx context.Context, q ports.Queryer, documentID string) (bool, error) {
	docUUID, err := parseUUID("document_id", documentID)
	if err != nil {
		return false, err
	}
	var jobID *uuid.UUID
	err = q.QueryRow(ctx, `
		SELECT id FROM jobs
		WHERE document_id = $1 AND kind = 'ingest' AND status IN ('queued', 'running')
		LIMIT 1`, docUUID).Scan(&jobID)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, err
	}
	return jobID != nil, nil
}

// IsVersionActive сообщает, является ли версия активной (для cleanup-задачи).
func (r *DocumentRepository) IsVersionActive(
	ctx context.Context, q ports.Queryer, documentID, versionID string,
) (bool, error) {
	docUUID, err := parseUUID("document_id", documentID)
	if err != nil {
		return false, err
	}
	versionUUID, err := parseUUID("version_id", versionID)
	if err != nil {
		return false, err
	}
	var active bool
	err = q.QueryRow(ctx,
		"SELECT COALESCE(active_version_id = $2, false) FROM documents WHERE id = $1",
		docUUID, versionUUID).Scan(&active)
	if err != nil {
		return false, noRows(err, "Документ не найден")
	}
	return active, nil
}
