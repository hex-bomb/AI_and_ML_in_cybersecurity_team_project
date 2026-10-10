package postgres

import (
	"context"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kbsearch/backend-go/internal/application/ports"
	"github.com/kbsearch/backend-go/internal/domain"
)

// Границы пагинации списка баз знаний (ТЗ 2.3, те же что в контракте).
const (
	defaultListLimit = 20
	maxListLimit     = 100
)

// kbColumns — состав выборки базы знаний во всех чтениях.
const kbColumns = "id, name, embedding_profile_id, created_at"

// KBRepository — реализация ports.KBRepository поверх PostgreSQL (только чтение настроек).
type KBRepository struct{}

var _ ports.KBRepository = (*KBRepository)(nil)

// NewKBRepository создаёт репозиторий баз знаний.
func NewKBRepository() *KBRepository { return &KBRepository{} }

// Create создаёт базу знаний (без настроек — их заводит администратор, ТЗ 7.3).
func (r *KBRepository) Create(
	ctx context.Context, q ports.Queryer, p domain.Principal, in domain.CreateKBInput,
) (domain.KB, error) {
	if err := checkACLEncodable(in.ACL); err != nil {
		return domain.KB{}, err
	}
	users, groups, err := aclJSON(in.ACL)
	if err != nil {
		return domain.KB{}, err
	}
	const query = `
		INSERT INTO knowledge_bases (id, tenant_id, name, allowed_user_ids, allowed_group_ids, created_at)
		VALUES (gen_random_uuid(), $1, $2, $3::jsonb, $4::jsonb, now())
		RETURNING ` + kbColumns
	return scanKB(q.QueryRow(ctx, query, p.TenantID, in.Name, users, groups))
}

// List возвращает страницу баз знаний и курсор (порядок created_at DESC, id DESC).
// ACL не применяется: список видит весь tenant, доступ решает чтение (ТЗ 2.1).
func (r *KBRepository) List(
	ctx context.Context, q ports.Queryer, p domain.Principal, limit int, cursor string,
) ([]domain.KB, string, error) {
	limit = clampLimit(limit)

	// Плейсхолдеры нумеруются по факту добавления аргументов.
	var args []any
	next := func(value any) string {
		args = append(args, value)
		return "$" + strconv.Itoa(len(args))
	}

	where := "WHERE tenant_id = " + next(p.TenantID)
	if cursor != "" {
		createdAt, kbID, err := decodeCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		// Построчное сравнение пары (created_at, id).
		where += " AND (created_at, id) < (" + next(createdAt) + ", " + next(kbID) + ")"
	}

	limitArg := next(limit + 1)
	query := "SELECT " + kbColumns + " FROM knowledge_bases " + where +
		" ORDER BY created_at DESC, id DESC LIMIT " + limitArg

	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, "", err
	}

	var (
		items []domain.KB
		total int
	)
	err = collect(rows, func() error {
		kb, scanErr := scanKB(rows)
		if scanErr != nil {
			return scanErr
		}
		items = append(items, kb)
		total++
		return nil
	})
	if err != nil {
		return nil, "", err
	}

	nextCursor := ""
	if total > limit {
		// Курсор — последняя отданная строка.
		nextCursor = encodeCursor(items[limit-1].CreatedAt, items[limit-1].KBID)
		items = items[:limit]
	}
	return items, nextCursor, nil
}

// Require возвращает базу знаний или ErrNotFound (чужой tenant — тоже 404).
func (r *KBRepository) Require(
	ctx context.Context, q ports.Queryer, p domain.Principal, kbID string,
) (domain.KB, error) {
	kbUUID, err := parseUUID("kb_id", kbID)
	if err != nil {
		return domain.KB{}, err
	}
	row := q.QueryRow(ctx, "SELECT "+kbColumns+" FROM knowledge_bases WHERE id = $1 AND tenant_id = $2",
		kbUUID, p.TenantID)
	kb, err := scanKB(row)
	if err != nil {
		return domain.KB{}, noRows(err, "База знаний не найдена")
	}
	return kb, nil
}

// SetACL заменяет ACL базы знаний (строка блокируется FOR UPDATE).
func (r *KBRepository) SetACL(ctx context.Context, tx ports.Queryer, kbID string, in domain.ACL) error {
	if err := checkACLEncodable(in); err != nil {
		return err
	}
	kbUUID, err := parseUUID("kb_id", kbID)
	if err != nil {
		return err
	}
	users, groups, err := aclJSON(in)
	if err != nil {
		return err
	}

	lock := `SELECT id FROM knowledge_bases WHERE id = $1 FOR UPDATE`
	var locked uuid.UUID
	if err := tx.QueryRow(ctx, lock, kbUUID).Scan(&locked); err != nil {
		return noRows(err, "База знаний не найдена")
	}

	const update = `
		UPDATE knowledge_bases
		SET allowed_user_ids = $2::jsonb, allowed_group_ids = $3::jsonb
		WHERE id = $1`
	if _, err := tx.Exec(ctx, update, kbUUID, users, groups); err != nil {
		return err
	}
	return nil
}

// SetDocumentACL ставит собственный ACL документа (nil — наследование ACL базы).
func (r *KBRepository) SetDocumentACL(
	ctx context.Context, tx ports.Queryer, documentID string, override *domain.ACL,
) error {
	docUUID, err := parseUUID("document_id", documentID)
	if err != nil {
		return err
	}

	// Удалённые документы не редактируются.
	const lock = `
		SELECT id FROM documents
		WHERE id = $1 AND deleted_at IS NULL
		FOR UPDATE`
	var locked uuid.UUID
	if err := tx.QueryRow(ctx, lock, docUUID).Scan(&locked); err != nil {
		return noRows(err, "Документ не найден")
	}

	var payload []byte
	if override != nil {
		if err := checkACLEncodable(*override); err != nil {
			return err
		}
		if payload, err = aclOverrideJSON(*override); err != nil {
			return err
		}
	}
	// nil — в SQL NULL (наследование), не в JSON 'null'.
	const update = `UPDATE documents SET acl_override = $2::jsonb WHERE id = $1`
	if _, err := tx.Exec(ctx, update, docUUID, payload); err != nil {
		return err
	}
	return nil
}

// EmbeddingProfileID возвращает зафиксированный профиль (пусто — ещё не фиксировался).
func (r *KBRepository) EmbeddingProfileID(ctx context.Context, q ports.Queryer, kbID string) (string, error) {
	kbUUID, err := parseUUID("kb_id", kbID)
	if err != nil {
		return "", err
	}
	var profile *string
	err = q.QueryRow(ctx, "SELECT embedding_profile_id FROM knowledge_bases WHERE id = $1", kbUUID).Scan(&profile)
	if err != nil {
		return "", noRows(err, "База знаний не найдена")
	}
	if profile == nil {
		return "", nil
	}
	return *profile, nil
}

// ACL возвращает ACL базы знаний (нужен Authorizer.RequireRead; в порте его нет).
func (r *KBRepository) ACL(ctx context.Context, q ports.Queryer, kbID string) (domain.ACL, error) {
	kbUUID, err := parseUUID("kb_id", kbID)
	if err != nil {
		return domain.ACL{}, err
	}
	var users, groups []string
	err = q.QueryRow(ctx,
		"SELECT allowed_user_ids, allowed_group_ids FROM knowledge_bases WHERE id = $1",
		kbUUID).Scan(&users, &groups)
	if err != nil {
		return domain.ACL{}, noRows(err, "База знаний не найдена")
	}
	return domain.ACL{Users: nonNil(users), Groups: nonNil(groups)}, nil
}

// scanKB читает базу знаний из строки результата (и Row, и Rows).
func scanKB(row ports.Row) (domain.KB, error) {
	var id uuid.UUID
	var profile *string
	var kb domain.KB
	if err := row.Scan(&id, &kb.Name, &profile, &kb.CreatedAt); err != nil {
		return domain.KB{}, err
	}
	kb.KBID = id.String()
	kb.CreatedAt = kb.CreatedAt.UTC()
	if profile != nil {
		kb.EmbeddingProfileID = *profile
	}
	return kb, nil
}

// clampLimit ограничивает размер страницы диапазоном контракта.
func clampLimit(limit int) int {
	if limit <= 0 {
		return defaultListLimit
	}
	if limit > maxListLimit {
		return maxListLimit
	}
	return limit
}

// encodeCursor кодирует позицию пагинации (base64 URL без padding для query-строки).
func encodeCursor(createdAt time.Time, id string) string {
	raw := createdAt.UTC().Format(time.RFC3339Nano) + "|" + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor разбирает курсор пагинации (битый — 422, не первая страница).
func decodeCursor(cursor string) (time.Time, uuid.UUID, error) {
	broken := &domain.AppError{
		Code:    domain.CodeInvalidMetadata,
		Status:  http.StatusUnprocessableEntity,
		Message: "Курсор пагинации повреждён",
	}

	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(cursor, "="))
	if err != nil {
		return time.Time{}, uuid.Nil, broken
	}
	createdAtRaw, idRaw, found := strings.Cut(string(raw), "|")
	if !found {
		return time.Time{}, uuid.Nil, broken
	}
	createdAt, err := time.Parse(time.RFC3339Nano, createdAtRaw)
	if err != nil {
		return time.Time{}, uuid.Nil, broken
	}
	id, err := uuid.Parse(idRaw)
	if err != nil {
		return time.Time{}, uuid.Nil, broken
	}
	return createdAt.UTC(), id, nil
}
