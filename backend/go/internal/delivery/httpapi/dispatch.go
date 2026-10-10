package httpapi

import (
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/kbsearch/backend-go/internal/contracts"
	"github.com/kbsearch/backend-go/internal/domain"
)

// Лимиты длин на входе транспорта (длиннее — 422, до сценариев не доходит).
const (
	maxQueryRunes = 2000
	maxNameRunes  = 500
)

// dispatch разводит вызовы контракта по обработчикам (switch вместо карты).
func (h *apiHandler) dispatch(name string, w http.ResponseWriter, r *http.Request) {
	switch name {

	case "HealthLiveHealthLiveGet":
		h.healthLive(w, r)
	case "HealthReadyHealthReadyGet":
		h.healthReady(w, r)
	case "MeApiV1MeGet":
		h.me(w, r)

	case "CreateKbApiV1KbsPost":
		h.createKB(w, r)
	case "ListKbsApiV1KbsGet":
		h.listKBs(w, r)
	case "PatchKbAclApiV1KbsKbIdAclPatch":
		h.patchKBACL(w, r)

	case "SearchEndpointApiV1SearchPost":
		h.search(w, r)
	case "AnswersEndpointApiV1AnswersPost":
		h.answers(w, r)
	case "SettingsDiagnosticsApiV1SettingsDiagnosticsGet":
		h.diagnostics(w, r)

	case "UploadDocumentApiV1KbsKbIdDocumentsPost":
		h.uploadDocument(w, r)
	case "ListDocumentsApiV1DocumentsGet":
		h.listDocuments(w, r)
	case "GetDocumentApiV1DocumentsDocumentIdGet":
		h.getDocument(w, r)
	case "DeleteDocumentApiV1DocumentsDocumentIdDelete":
		h.deleteDocument(w, r)
	case "ReplaceContentApiV1DocumentsDocumentIdContentPut":
		h.replaceContent(w, r)
	case "ReindexDocumentApiV1DocumentsDocumentIdReindexPost":
		h.reindexDocument(w, r)
	case "RetryDocumentApiV1DocumentsDocumentIdRetryPost":
		h.retryDocument(w, r)
	case "PatchDocumentAclApiV1DocumentsDocumentIdAclPatch":
		h.patchDocumentACL(w, r)
	case "DownloadSourceApiV1DocumentsDocumentIdSourceGet":
		h.downloadSource(w, r)

	case "GetJobApiV1JobsJobIdGet":
		h.getJob(w, r)

	default:
		WriteError(w, r, domain.NotFound("Маршрут не реализован"))
	}
}

// ── health и principal ───────────────────────────────────────────────

func (h *apiHandler) healthLive(w http.ResponseWriter, r *http.Request) {
	if h.svc.Health == nil {
		WriteJSON(w, http.StatusOK, map[string]string{"status": "alive"})
		return
	}
	if err := h.svc.Health.Live(); err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]string{"status": "alive"})
}

func (h *apiHandler) healthReady(w http.ResponseWriter, r *http.Request) {
	if h.svc.Health == nil {
		WriteJSON(w, http.StatusOK, map[string]any{"status": "ready", "checks": map[string]bool{}})
		return
	}
	checks, err := h.svc.Health.Ready(r.Context())
	ready := err == nil
	status := "ready"
	code := http.StatusOK
	if !ready {
		status, code = "not_ready", http.StatusServiceUnavailable
	}
	WriteJSON(w, code, map[string]any{"status": status, "checks": checks})
}

func (h *apiHandler) me(w http.ResponseWriter, r *http.Request) {
	principal, err := h.principal(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	groups := principal.Groups
	roles := principal.Roles
	WriteJSON(w, http.StatusOK, contracts.Principal{
		TenantId: principal.TenantID,
		UserId:   principal.UserID,
		Groups:   &groups,
		Roles:    &roles,
	})
}

// ── базы знаний ──────────────────────────────────────────────────────

func (h *apiHandler) createKB(w http.ResponseWriter, r *http.Request) {
	principal, err := h.principal(r)
	if _, ok := scenario(w, r, "KB", h.svc.KB); !ok {
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	name := "База знаний"
	var body struct {
		Name *string `json:"name"`
	}
	if r.ContentLength > 0 {
		if err := strictJSON(r, &body); err != nil {
			WriteError(w, r, err)
			return
		}
		if body.Name != nil && strings.TrimSpace(*body.Name) != "" {
			name = strings.TrimSpace(*body.Name)
		}
	}
	if utf8.RuneCountInString(name) > maxNameRunes {
		WriteError(w, r, domain.InvalidMetadata("name превышает 500 символов"))
		return
	}

	id, err := h.svc.KB.CreateKB(r.Context(), principal, name)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, map[string]any{
		"kb_id":      id.String(),
		"name":       name,
		"created_at": time.Now().UTC(),
		"request_id": requestIDFrom(r.Context()),
	})
}

func (h *apiHandler) listKBs(w http.ResponseWriter, r *http.Request) {
	principal, err := h.principal(r)
	if _, ok := scenario(w, r, "KB", h.svc.KB); !ok {
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	limit := parseLimit(r.URL.Query().Get("limit"), 50, 100)
	cursor := r.URL.Query().Get("cursor")

	items, next, err := h.svc.KB.ListKBs(r.Context(), principal, limit, cursor)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out := contracts.KBList{Items: make([]contracts.KBInfo, 0, len(items))}
	for _, kb := range items {
		out.Items = append(out.Items, contracts.KBInfo{
			KbId: kb.ID.String(), Name: kb.Name, CreatedAt: parseTime(kb.CreatedAt),
		})
	}
	if next != "" {
		out.NextCursor = &next
	}
	WriteJSON(w, http.StatusOK, out)
}

func (h *apiHandler) patchKBACL(w http.ResponseWriter, r *http.Request) {
	principal, err := h.principal(r)
	if _, ok := scenario(w, r, "KB", h.svc.KB); !ok {
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	kbID, err := pathUUID(r, "kb_id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var body contracts.ACLRequest
	if err := strictJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if err := h.svc.KB.SetKBACL(r.Context(), principal, kbID, aclFrom(body)); err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"request_id": requestIDFrom(r.Context())})
}

// ── поиск и ответы ───────────────────────────────────────────────────

func (h *apiHandler) search(w http.ResponseWriter, r *http.Request) {
	principal, err := h.principal(r)
	if _, ok := scenario(w, r, "Search", h.svc.Search); !ok {
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var body contracts.SearchBody
	if err := strictJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if body.KbId == openapi_types.UUID(uuid.Nil) {
		WriteError(w, r, domain.InvalidMetadata("kb_id обязателен"))
		return
	}
	if strings.TrimSpace(body.Query) == "" {
		WriteError(w, r, domain.InvalidMetadata("query не может быть пустым"))
		return
	}
	if utf8.RuneCountInString(body.Query) > maxQueryRunes {
		WriteError(w, r, domain.InvalidMetadata("query превышает 2000 символов"))
		return
	}

	// Ноль top_k — «взять search.top_k из настроек базы» (ТЗ 7.3).
	topK := 0
	if body.TopK != nil {
		topK = *body.TopK
		if topK < 1 || topK > 100 {
			WriteError(w, r, domain.InvalidMetadata("top_k должен быть в диапазоне 1..100"))
			return
		}
	}

	out, err := h.svc.Search.Search(r.Context(), principal, SearchInput{
		KBID: body.KbId, Query: body.Query, TopK: topK,
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}

	// Указатель: пустая выдача опускает поле вместо null по схеме.
	hits := make([]contracts.SearchHit, 0, len(out.Hits))
	for _, c := range out.Hits {
		hits = append(hits, contracts.SearchHit{
			ChunkId:     c.ChunkID,
			DocumentId:  c.DocumentID,
			VersionId:   c.VersionID,
			Title:       c.Title,
			Text:        c.Text,
			Score:       float32(c.Score),
			SourceUrl:   c.SourceURL,
			DownloadUrl: c.DownloadURL,
			Locator:     locatorOut(c.Locator),
		})
	}
	WriteJSON(w, http.StatusOK, contracts.SearchResponse{
		Status:    out.Status,
		RequestId: requestIDFrom(r.Context()),
		Hits:      &hits,
	})
}

func (h *apiHandler) answers(w http.ResponseWriter, r *http.Request) {
	principal, err := h.principal(r)
	if _, ok := scenario(w, r, "Answers", h.svc.Answers); !ok {
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var body contracts.AnswerBody
	if err := strictJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if body.KbId == openapi_types.UUID(uuid.Nil) {
		WriteError(w, r, domain.InvalidMetadata("kb_id обязателен"))
		return
	}
	if strings.TrimSpace(body.Question) == "" {
		WriteError(w, r, domain.InvalidMetadata("question не может быть пустым"))
		return
	}
	if utf8.RuneCountInString(body.Question) > maxQueryRunes {
		WriteError(w, r, domain.InvalidMetadata("question превышает 2000 символов"))
		return
	}

	out, err := h.svc.Answers.Answer(r.Context(), principal, AnswerInput{
		KBID: body.KbId, Question: body.Question,
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}

	// Номер цитаты — позиция в срезе сценария.
	citations := make([]contracts.Citation, 0, len(out.Citations))
	for i, c := range out.Citations {
		citations = append(citations, contracts.Citation{
			Number:      i + 1,
			ChunkId:     c.ChunkID,
			DocumentId:  c.DocumentID,
			VersionId:   c.VersionID,
			Title:       c.Title,
			Quote:       c.Quote,
			Locator:     locatorOut(c.Locator),
			SourceUrl:   sourceURL(c.SourceURL),
			DownloadUrl: c.DownloadURL,
		})
	}
	WriteJSON(w, http.StatusOK, contracts.AnswerResponse{
		Status:    out.Status,
		Answer:    out.Answer,
		Citations: &citations,
		RequestId: requestIDFrom(r.Context()),
	})
}

func (h *apiHandler) diagnostics(w http.ResponseWriter, r *http.Request) {
	principal, err := h.principal(r)
	if _, ok := scenario(w, r, "Settings", h.svc.Settings); !ok {
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	kbID, err := uuidParam(r, "kb_id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out, err := h.svc.Settings.Diagnose(r.Context(), principal, kbID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"ready":      out.Ready,
		"missing":    out.Missing,
		"keys":       out.Keys,
		"request_id": requestIDFrom(r.Context()),
	})
}

// ── документы ────────────────────────────────────────────────────────

func (h *apiHandler) uploadDocument(w http.ResponseWriter, r *http.Request) {
	principal, err := h.principal(r)
	if _, ok := scenario(w, r, "Document", h.svc.Document); !ok {
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	kbID, err := pathUUID(r, "kb_id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	in, err := h.readUpload(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	in.KBID = kbID

	created, err := h.svc.Document.Upload(r.Context(), principal, in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	status := "accepted"
	WriteJSON(w, http.StatusAccepted, contracts.DocumentAccepted{
		DocumentId: created.DocumentID.String(),
		JobId:      created.JobID.String(),
		VersionId:  created.VersionID.String(),
		Status:     &status,
		RequestId:  requestIDFrom(r.Context()),
	})
}

func (h *apiHandler) listDocuments(w http.ResponseWriter, r *http.Request) {
	principal, err := h.principal(r)
	if _, ok := scenario(w, r, "Document", h.svc.Document); !ok {
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	kbID, err := uuidParam(r, "kb_id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out, err := h.svc.Document.List(r.Context(), principal, ListInput{
		KBID:   kbID,
		Limit:  parseLimit(r.URL.Query().Get("limit"), 50, 100),
		Cursor: r.URL.Query().Get("cursor"),
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	items := make([]contracts.DocumentInfo, 0, len(out.Items))
	for _, d := range out.Items {
		items = append(items, documentInfo(d))
	}
	resp := contracts.DocumentList{Items: items}
	if out.NextCursor != "" {
		resp.NextCursor = &out.NextCursor
	}
	WriteJSON(w, http.StatusOK, resp)
}

func (h *apiHandler) getDocument(w http.ResponseWriter, r *http.Request) {
	principal, err := h.principal(r)
	if _, ok := scenario(w, r, "Document", h.svc.Document); !ok {
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	id, err := pathUUID(r, "document_id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	d, err := h.svc.Document.Get(r.Context(), principal, id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, documentInfo(d))
}

func (h *apiHandler) deleteDocument(w http.ResponseWriter, r *http.Request) {
	principal, err := h.principal(r)
	if _, ok := scenario(w, r, "Document", h.svc.Document); !ok {
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	id, err := pathUUID(r, "document_id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := h.svc.Document.Delete(r.Context(), principal, id); err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"request_id": requestIDFrom(r.Context())})
}

func (h *apiHandler) replaceContent(w http.ResponseWriter, r *http.Request) {
	principal, err := h.principal(r)
	if _, ok := scenario(w, r, "Document", h.svc.Document); !ok {
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	id, err := pathUUID(r, "document_id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	upload, err := h.readUpload(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	jobID, err := h.svc.Document.ReplaceContent(r.Context(), principal, ReplaceContentInput{
		DocumentID:     id,
		Filename:       upload.Filename,
		Title:          upload.Title,
		Content:        upload.Content,
		Metadata:       upload.Metadata,
		IdempotencyKey: upload.IdempotencyKey,
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusAccepted, map[string]any{
		"document_id": id.String(),
		"job_id":      jobID.String(),
		"request_id":  requestIDFrom(r.Context()),
	})
}

func (h *apiHandler) reindexDocument(w http.ResponseWriter, r *http.Request) {
	principal, err := h.principal(r)
	if _, ok := scenario(w, r, "Document", h.svc.Document); !ok {
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	id, err := pathUUID(r, "document_id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	jobID, err := h.svc.Document.Reindex(r.Context(), principal, id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusAccepted, map[string]any{
		"document_id": id.String(),
		"job_id":      jobID.String(),
		"request_id":  requestIDFrom(r.Context()),
	})
}

func (h *apiHandler) retryDocument(w http.ResponseWriter, r *http.Request) {
	principal, err := h.principal(r)
	if _, ok := scenario(w, r, "Document", h.svc.Document); !ok {
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	id, err := pathUUID(r, "document_id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	jobID, err := h.svc.Document.Retry(r.Context(), principal, id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusAccepted, map[string]any{
		"document_id": id.String(),
		"job_id":      jobID.String(),
		"request_id":  requestIDFrom(r.Context()),
	})
}

func (h *apiHandler) patchDocumentACL(w http.ResponseWriter, r *http.Request) {
	principal, err := h.principal(r)
	if _, ok := scenario(w, r, "Document", h.svc.Document); !ok {
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	id, err := pathUUID(r, "document_id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var body contracts.DocumentACLRequest
	if err := strictJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if body.AclOverride == nil {
		WriteError(w, r, domain.InvalidMetadata("acl_override обязателен"))
		return
	}
	if err := h.svc.Document.SetACL(r.Context(), principal, id, aclFrom(*body.AclOverride)); err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"request_id": requestIDFrom(r.Context())})
}

func (h *apiHandler) downloadSource(w http.ResponseWriter, r *http.Request) {
	principal, err := h.principal(r)
	if _, ok := scenario(w, r, "Document", h.svc.Document); !ok {
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	id, err := pathUUID(r, "document_id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	content, filename, err := h.svc.Document.Source(r.Context(), principal, id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	// Имя файла кодируется: кириллица ломает разбор у части клиентов.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition",
		"attachment; filename*=UTF-8''"+urlEscape(filename))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

// ── задания ──────────────────────────────────────────────────────────

func (h *apiHandler) getJob(w http.ResponseWriter, r *http.Request) {
	principal, err := h.principal(r)
	if _, ok := scenario(w, r, "Job", h.svc.Job); !ok {
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	id, err := pathUUID(r, "job_id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	j, err := h.svc.Job.Get(r.Context(), principal, id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	info := contracts.JobInfo{
		JobId:      j.ID.String(),
		DocumentId: j.DocumentID,
		Kind:       j.Kind,
		Stage:      j.Stage,
		Status:     j.Status,
		Attempt:    j.Attempts,
		UpdatedAt:  parseTime(j.UpdatedAt),
	}
	if j.ErrorCode != "" {
		code := j.ErrorCode
		info.ErrorCode = &code
	}
	if j.ErrorText != "" {
		msg := j.ErrorText
		info.ErrorMessage = &msg
	}
	if j.Chunks > 0 {
		n := j.Chunks
		info.ProcessedChunks = &n
	}
	if j.VersionID != "" {
		v := j.VersionID
		info.VersionId = &v
	}
	WriteJSON(w, http.StatusOK, info)
}

// ── вспомогательное ──────────────────────────────────────────────────

// pathUUID достаёт UUID из пути chi.
func pathUUID(r *http.Request, param string) (uuid.UUID, error) {
	raw := chi.URLParam(r, param)
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, domain.InvalidMetadata("Некорректный идентификатор в пути: " + raw)
	}
	return id, nil
}

// uuidParam достаёт UUID из query (отсутствие — ошибка).
func uuidParam(r *http.Request, name string) (uuid.UUID, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return uuid.Nil, domain.InvalidMetadata(name + " обязателен")
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, domain.InvalidMetadata("Некорректный " + name + ": " + raw)
	}
	return id, nil
}

func aclFrom(a contracts.ACLRequest) domain.AllowedList {
	return domain.AllowedList{Users: a.AllowedUserIds, Groups: a.AllowedGroupIds}
}

func documentInfo(d DocumentView) contracts.DocumentInfo {
	out := contracts.DocumentInfo{
		DocumentId: d.ID.String(),
		KbId:       d.KBID.String(),
		ExternalId: sourceURL(d.SourceURL),
		CreatedAt:  parseTime(d.CreatedAt),
	}
	if d.VersionID != uuid.Nil {
		v := d.VersionID.String()
		out.ActiveVersionId = &v
	}
	out.LatestVersion = &contracts.DocumentVersionInfo{
		VersionId:  d.VersionID.String(),
		Filename:   d.Filename,
		Title:      d.Title,
		Status:     d.State,
		ChunkCount: d.Chunks,
	}
	return out
}

func parseTime(v string) time.Time {
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Now().UTC()
	}
	return t
}

// scenario проверяет подключение сценария (неподключённый — честный 503).
func scenario[T any](w http.ResponseWriter, r *http.Request, name string, s T) (T, bool) {
	var zero T
	if any(s) == nil {
		WriteError(w, r, domain.ServiceUnavailable("Сценарий "+name+" ещё не подключён").
			WithDetails(map[string]any{"scenario": name}))
		return zero, false
	}
	return s, true
}
