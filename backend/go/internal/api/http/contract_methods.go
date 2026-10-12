package httpapi

import (
	"net/http"

	openapi_types "github.com/oapi-codegen/runtime/types"

	contracts "github.com/kbsearch/kb/api"
)

// Мост контракта к сценариям. Параметры генератора игнорируются: тело и query
// разбирает транспорт через strictJSON и parseLimit; маршруты сгенерированы.

func (h *apiHandler) AnswersEndpointApiV1AnswersPost(w http.ResponseWriter, r *http.Request, _ contracts.AnswersEndpointApiV1AnswersPostParams) {
	h.dispatch("AnswersEndpointApiV1AnswersPost", w, r)
}

func (h *apiHandler) ListDocumentsApiV1DocumentsGet(w http.ResponseWriter, r *http.Request, _ contracts.ListDocumentsApiV1DocumentsGetParams) {
	h.dispatch("ListDocumentsApiV1DocumentsGet", w, r)
}

func (h *apiHandler) DeleteDocumentApiV1DocumentsDocumentIdDelete(w http.ResponseWriter, r *http.Request, _ openapi_types.UUID, _ contracts.DeleteDocumentApiV1DocumentsDocumentIdDeleteParams) {
	h.dispatch("DeleteDocumentApiV1DocumentsDocumentIdDelete", w, r)
}

func (h *apiHandler) GetDocumentApiV1DocumentsDocumentIdGet(w http.ResponseWriter, r *http.Request, _ openapi_types.UUID, _ contracts.GetDocumentApiV1DocumentsDocumentIdGetParams) {
	h.dispatch("GetDocumentApiV1DocumentsDocumentIdGet", w, r)
}

func (h *apiHandler) PatchDocumentAclApiV1DocumentsDocumentIdAclPatch(w http.ResponseWriter, r *http.Request, _ openapi_types.UUID, _ contracts.PatchDocumentAclApiV1DocumentsDocumentIdAclPatchParams) {
	h.dispatch("PatchDocumentAclApiV1DocumentsDocumentIdAclPatch", w, r)
}

func (h *apiHandler) ReplaceContentApiV1DocumentsDocumentIdContentPut(w http.ResponseWriter, r *http.Request, _ openapi_types.UUID, _ contracts.ReplaceContentApiV1DocumentsDocumentIdContentPutParams) {
	h.dispatch("ReplaceContentApiV1DocumentsDocumentIdContentPut", w, r)
}

func (h *apiHandler) ReindexDocumentApiV1DocumentsDocumentIdReindexPost(w http.ResponseWriter, r *http.Request, _ openapi_types.UUID, _ contracts.ReindexDocumentApiV1DocumentsDocumentIdReindexPostParams) {
	h.dispatch("ReindexDocumentApiV1DocumentsDocumentIdReindexPost", w, r)
}

func (h *apiHandler) RetryDocumentApiV1DocumentsDocumentIdRetryPost(w http.ResponseWriter, r *http.Request, _ openapi_types.UUID, _ contracts.RetryDocumentApiV1DocumentsDocumentIdRetryPostParams) {
	h.dispatch("RetryDocumentApiV1DocumentsDocumentIdRetryPost", w, r)
}

func (h *apiHandler) DownloadSourceApiV1DocumentsDocumentIdSourceGet(w http.ResponseWriter, r *http.Request, _ openapi_types.UUID, _ contracts.DownloadSourceApiV1DocumentsDocumentIdSourceGetParams) {
	h.dispatch("DownloadSourceApiV1DocumentsDocumentIdSourceGet", w, r)
}

func (h *apiHandler) GetJobApiV1JobsJobIdGet(w http.ResponseWriter, r *http.Request, _ openapi_types.UUID, _ contracts.GetJobApiV1JobsJobIdGetParams) {
	h.dispatch("GetJobApiV1JobsJobIdGet", w, r)
}

func (h *apiHandler) ListKbsApiV1KbsGet(w http.ResponseWriter, r *http.Request, _ contracts.ListKbsApiV1KbsGetParams) {
	h.dispatch("ListKbsApiV1KbsGet", w, r)
}

func (h *apiHandler) CreateKbApiV1KbsPost(w http.ResponseWriter, r *http.Request, _ contracts.CreateKbApiV1KbsPostParams) {
	h.dispatch("CreateKbApiV1KbsPost", w, r)
}

func (h *apiHandler) PatchKbAclApiV1KbsKbIdAclPatch(w http.ResponseWriter, r *http.Request, _ openapi_types.UUID, _ contracts.PatchKbAclApiV1KbsKbIdAclPatchParams) {
	h.dispatch("PatchKbAclApiV1KbsKbIdAclPatch", w, r)
}

func (h *apiHandler) UploadDocumentApiV1KbsKbIdDocumentsPost(w http.ResponseWriter, r *http.Request, _ openapi_types.UUID, _ contracts.UploadDocumentApiV1KbsKbIdDocumentsPostParams) {
	h.dispatch("UploadDocumentApiV1KbsKbIdDocumentsPost", w, r)
}

func (h *apiHandler) MeApiV1MeGet(w http.ResponseWriter, r *http.Request, _ contracts.MeApiV1MeGetParams) {
	h.dispatch("MeApiV1MeGet", w, r)
}

func (h *apiHandler) SearchEndpointApiV1SearchPost(w http.ResponseWriter, r *http.Request, _ contracts.SearchEndpointApiV1SearchPostParams) {
	h.dispatch("SearchEndpointApiV1SearchPost", w, r)
}

func (h *apiHandler) SettingsDiagnosticsApiV1SettingsDiagnosticsGet(w http.ResponseWriter, r *http.Request, _ contracts.SettingsDiagnosticsApiV1SettingsDiagnosticsGetParams) {
	h.dispatch("SettingsDiagnosticsApiV1SettingsDiagnosticsGet", w, r)
}

func (h *apiHandler) HealthLiveHealthLiveGet(w http.ResponseWriter, r *http.Request) {
	h.dispatch("HealthLiveHealthLiveGet", w, r)
}

func (h *apiHandler) HealthReadyHealthReadyGet(w http.ResponseWriter, r *http.Request) {
	h.dispatch("HealthReadyHealthReadyGet", w, r)
}
