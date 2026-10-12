package httpapi

import (
	"context"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/kbsearch/kb/api"
	"github.com/kbsearch/kb/internal/envelope"
)

// Сценарии — порты application для транспорта. DTO не совпадают с типами
// контракта: перевод выполняет транспорт, сценарии проверяются без HTTP.

// SearchInput — запрос поиска.
type SearchInput struct {
	KBID  uuid.UUID
	Query string
	TopK  int
}

// SearchOutput — ответ поиска.
type SearchOutput struct {
	Status string
	Hits   []domain.SearchCandidate
}

// AnswerInput — запрос ответа.
type AnswerInput struct {
	KBID     uuid.UUID
	Question string
}

// AnswerOutput — ответ с цитатами.
type AnswerOutput struct {
	Status    string
	Answer    string
	Citations []domain.Citation
}

// UploadInput — загрузка документа.
type UploadInput struct {
	KBID      uuid.UUID
	Filename  string
	Title     string
	Content   []byte
	SourceURL string
	Metadata  map[string]any
	// IdempotencyKey — повтор загрузки не создаёт вторую версию (ТЗ 5.4).
	IdempotencyKey string
}

// ListInput — постраничный список документов.
type ListInput struct {
	KBID   uuid.UUID
	Limit  int
	Cursor string
}

// ListOutput — страница документов.
type ListOutput struct {
	Items      []DocumentView
	NextCursor string
}

// ReplaceContentInput — замена содержимого существующего документа.
type ReplaceContentInput struct {
	DocumentID     uuid.UUID
	Filename       string
	Title          string
	Content        []byte
	Metadata       map[string]any
	IdempotencyKey string
}

// DocumentView — документ в ответе API.
type DocumentView struct {
	ID        uuid.UUID
	KBID      uuid.UUID
	Filename  string
	Title     string
	VersionID uuid.UUID
	State     string
	Stage     string
	CreatedAt string
	UpdatedAt string
	Chunks    int
	SourceURL string
	ACL       domain.ACL
}

// KBView — база знаний в ответе API (время строкой RFC3339 из БД).
type KBView struct {
	ID        uuid.UUID
	Name      string
	CreatedAt string
	ACL       domain.ACL
}

// JobView — задание в ответе API.
type JobView struct {
	ID         uuid.UUID
	DocumentID string
	VersionID  string
	Kind       string
	Stage      string
	Status     string
	Attempts   int
	Chunks     int
	ErrorCode  string
	ErrorText  string
	CreatedAt  string
	UpdatedAt  string
}

// Diagnostics — состояние настроек.
type Diagnostics struct {
	Ready   bool
	Missing []string
	Keys    []KeyStatus
}

// KeyStatus — состояние одного ключа настроек.
type KeyStatus struct {
	Key     string `json:"key"`
	Scope   string `json:"scope"`
	IsSet   bool   `json:"is_set"`
	Sources string `json:"source"`
}

// AuthScenario отвечает, кто обращается. В локальном режиме возвращается
// единственный Principal, в рабочем — разбирается Bearer.
type AuthScenario interface {
	Principal(r *http.Request) (domain.Principal, error)
}

// locatorOut переводит локатор в форму ответа (пустые срезы — [], не null).
func locatorOut(l domain.Locator) contracts.LocatorOut {
	out := contracts.LocatorOut{SectionPath: []string{}}
	for _, id := range l.BlockIDs {
		out.BlockIds = append(out.BlockIds, id.String())
	}
	out.SectionPath = append(out.SectionPath, l.SectionPath...)
	if l.PageStart != nil {
		ps := *l.PageStart
		out.PageStart = &ps
	}
	if l.PageEnd != nil {
		pe := *l.PageEnd
		out.PageEnd = &pe
	}
	return out
}

// sourceURL прячет пустую ссылку из ответа вместо "".
func sourceURL(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// parseLimit читает limit из query с верхней границей.
func parseLimit(raw string, fallback, max int) int {
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return fallback
	}
	if n > max {
		return max
	}
	return n
}

// ── Сценарии ─────────────────────────────────────────────────────────

// SearchScenario отвечает на POST /search.
type SearchScenario interface {
	Search(ctx context.Context, p domain.Principal, in SearchInput) (SearchOutput, error)
}

// AnswerScenario отвечает на POST /answers.
type AnswerScenario interface {
	Answer(ctx context.Context, p domain.Principal, in AnswerInput) (AnswerOutput, error)
}

// KBScenario — базы знаний: создание, список, чтение, ACL.
type KBScenario interface {
	CreateKB(ctx context.Context, p domain.Principal, name string) (uuid.UUID, error)
	ListKBs(ctx context.Context, p domain.Principal, limit int, cursor string) ([]KBView, string, error)
	GetKB(ctx context.Context, p domain.Principal, kbID uuid.UUID) (KBView, error)
	SetKBACL(ctx context.Context, p domain.Principal, kbID uuid.UUID, acl domain.AllowedList) error
}

// UploadResult — идентификаторы созданного документа (структура против перепутанных uuid).
type UploadResult struct {
	DocumentID uuid.UUID
	VersionID  uuid.UUID
	JobID      uuid.UUID
}

// DocumentScenario — документы целиком.
type DocumentScenario interface {
	Upload(ctx context.Context, p domain.Principal, in UploadInput) (UploadResult, error)
	List(ctx context.Context, p domain.Principal, in ListInput) (ListOutput, error)
	Get(ctx context.Context, p domain.Principal, id uuid.UUID) (DocumentView, error)
	Delete(ctx context.Context, p domain.Principal, id uuid.UUID) error
	ReplaceContent(ctx context.Context, p domain.Principal, in ReplaceContentInput) (uuid.UUID, error)
	Reindex(ctx context.Context, p domain.Principal, id uuid.UUID) (uuid.UUID, error)
	Retry(ctx context.Context, p domain.Principal, id uuid.UUID) (uuid.UUID, error)
	SetACL(ctx context.Context, p domain.Principal, id uuid.UUID, acl domain.AllowedList) error
	Source(ctx context.Context, p domain.Principal, id uuid.UUID) ([]byte, string, error)
}

// SettingsScenario — GET /settings/diagnostics.
type SettingsScenario interface {
	Diagnose(ctx context.Context, p domain.Principal, kbID uuid.UUID) (Diagnostics, error)
}

// JobScenario — GET /jobs/{id}.
type JobScenario interface {
	Get(ctx context.Context, p domain.Principal, id uuid.UUID) (JobView, error)
}

// HealthScenario — /health/live и /health/ready.
type HealthScenario interface {
	Live() error
	Ready(ctx context.Context) (map[string]bool, error)
}

// principal спрашивает у AuthScenario, кто обращается.
func (h *apiHandler) principal(r *http.Request) (domain.Principal, error) {
	if h.svc.Auth == nil {
		return domain.Principal{}, domain.ServiceUnavailable("Правовая подсистема не подключена")
	}
	return h.svc.Auth.Principal(r)
}
