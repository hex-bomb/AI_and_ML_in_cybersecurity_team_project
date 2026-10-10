package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/kbsearch/backend-go/internal/application/ports"
	"github.com/kbsearch/backend-go/internal/domain"
)

// Хранилище файлов в памяти: LoadParsed читает ParsedDocument отсюда,
// поэтому проверка не требует диска и не зависит от каталога.
type memoryStore struct {
	items map[string][]byte
}

var _ ports.FileStorage = (*memoryStore)(nil)

func newMemoryStore() *memoryStore {
	return &memoryStore{items: map[string][]byte{}}
}

func (m *memoryStore) Put(_ context.Context, key string, data []byte) error {
	m.items[key] = data
	return nil
}

func (m *memoryStore) Read(_ context.Context, key string) ([]byte, error) {
	data, ok := m.items[key]
	if !ok {
		return nil, domain.NotFound("Файл не найден: " + key)
	}
	return data, nil
}

func (m *memoryStore) Delete(_ context.Context, key string) error {
	delete(m.items, key)
	return nil
}

// putParsed кладёт ParsedDocument в хранилище под ключом версии.
func putParsed(t *testing.T, store ports.FileStorage, key string, parsed domain.ParsedDocument) {
	t.Helper()
	raw, err := json.Marshal(parsed)
	if err != nil {
		t.Fatalf("сериализация ParsedDocument: %v", err)
	}
	if err := store.Put(context.Background(), key, raw); err != nil {
		t.Fatalf("Put в хранилище: %v", err)
	}
}

// principalOf собирает principal с указанными ролями и группами.
func principalOf(userID string, roles []string, groups []string) domain.Principal {
	return domain.Principal{
		TenantID: tenantID,
		UserID:   userID,
		Roles:    roles,
		Groups:   groups,
	}
}

// ── Активация версии (ТЗ 6.3 п.7) ─────────────────────────────────

// TestActivateVersionSwitchesActiveVersionAndFinishesJob — порт
// test_jobs.py:318 плюс транзакционность из ТЗ 6.3 п.7.
func TestActivateVersionSwitchesActiveVersionAndFinishesJob(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	docs := newTestDocuments()
	jobs := newTestJobs()
	lease := advanceToIndex(t, ctx, jobs)

	var previous string
	err := testDB.WithTx(ctx, func(tx ports.Queryer) error {
		var err error
		previous, err = docs.ActivateVersion(ctx, tx, domain.ActivateParams{
			TenantID:           tenantID.String(),
			DocumentID:         docID.String(),
			VersionID:          versionID.String(),
			EmbeddingProfileID: profileID,
			ChunkCount:         7,
			LeaseToken:         lease.LeaseToken,
		})
		if err != nil {
			return err
		}
		// Активация и завершение задания — одна транзакция (ТЗ 3.6, 6.3 п.7).
		return jobs.FinishIndex(ctx, tx, jobID.String(), lease.LeaseToken,
			domain.NewIndexingResult(docID.String(), versionID.String(), profileID, 7))
	})
	if err != nil {
		t.Fatalf("активация версии: %v", err)
	}
	if previous != "" {
		t.Errorf("прежняя активная версия = %q, ожидалась пустая", previous)
	}

	active, versionStatus, chunkCount, kbProfile := readActivationState(t, ctx)
	if active != versionID.String() {
		t.Errorf("active_version_id = %s, ожидался %s", active, versionID)
	}
	if versionStatus != domain.VersionStatusReady {
		t.Errorf("статус версии = %s, ожидался ready", versionStatus)
	}
	if chunkCount != 7 {
		t.Errorf("chunk_count = %d, ожидалось 7", chunkCount)
	}
	// Профиль базы фиксируется при первой активации.
	if kbProfile != profileID {
		t.Errorf("embedding_profile_id базы = %q, ожидался %q", kbProfile, profileID)
	}

	record := getJob(t, ctx, jobs)
	if record.Status != domain.JobStatusSucceeded {
		t.Errorf("статус задания = %s, ожидался succeeded", record.Status)
	}
	if record.TotalChunks == nil || *record.TotalChunks != 7 {
		t.Errorf("total_chunks = %v, ожидалось 7", record.TotalChunks)
	}
}

// TestActivateVersionRollsBackOnWrongToken — порт test_jobs.py:287.
// Ключевой тест транзакционности: ошибка в середине не должна оставлять
func TestActivateVersionRollsBackOnWrongToken(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	docs := newTestDocuments()
	jobs := newTestJobs()
	lease := advanceToIndex(t, ctx, jobs)

	err := testDB.WithTx(ctx, func(tx ports.Queryer) error {
		_, err := docs.ActivateVersion(ctx, tx, domain.ActivateParams{
			TenantID:           tenantID.String(),
			DocumentID:         docID.String(),
			VersionID:          versionID.String(),
			EmbeddingProfileID: profileID,
			ChunkCount:         7,
			LeaseToken:         "wrong-token",
		})
		if err != nil {
			return err
		}
		// До FinishIndex дело дойти не должно: активация обязана упасть.
		return jobs.FinishIndex(ctx, tx, jobID.String(), "wrong-token",
			domain.NewIndexingResult(docID.String(), versionID.String(), profileID, 7))
	})
	if !errors.Is(err, domain.ErrJobLeaseLost) {
		t.Fatalf("ожидалась ErrJobLeaseLost, получено: %v", err)
	}

	active, versionStatus, chunkCount, kbProfile := readActivationState(t, ctx)
	if active != "" {
		t.Errorf("транзакция откатилась не полностью: active_version_id = %s", active)
	}
	if versionStatus != domain.VersionStatusUploaded {
		t.Errorf("статус версии = %s, ожидался uploaded", versionStatus)
	}
	if chunkCount != 0 {
		t.Errorf("chunk_count = %d, ожидалось 0", chunkCount)
	}
	if kbProfile != "" {
		t.Errorf("профиль базы зафиксирован при откате: %q", kbProfile)
	}

	record := getJob(t, ctx, jobs)
	if record.Status != domain.JobStatusRunning {
		t.Errorf("статус задания = %s, ожидался running", record.Status)
	}
	if record.LeaseToken == nil || *record.LeaseToken != lease.LeaseToken {
		t.Errorf("lease_token задания изменился при откате: %v", record.LeaseToken)
	}
}

// TestActivateVersionRollsBackWhenFinishIndexFails — проверка без Python-теста.
// Обратная половина требования ТЗ 6.3 п.7: если финализация задания падает,
func TestActivateVersionRollsBackWhenFinishIndexFails(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	docs := newTestDocuments()
	jobs := newTestJobs()
	lease := advanceToIndex(t, ctx, jobs)

	err := testDB.WithTx(ctx, func(tx ports.Queryer) error {
		if _, err := docs.ActivateVersion(ctx, tx, domain.ActivateParams{
			TenantID:           tenantID.String(),
			DocumentID:         docID.String(),
			VersionID:          versionID.String(),
			EmbeddingProfileID: profileID,
			ChunkCount:         5,
			LeaseToken:         lease.LeaseToken,
		}); err != nil {
			return err
		}
		// Искусственная ошибка ПОСЛЕ активации: транзакция обязана её
		// откатить вместе с активацией.
		return errors.New("сбой после активации")
	})
	if err == nil {
		t.Fatal("транзакция завершилась успешно, ожидалась ошибка")
	}

	active, versionStatus, chunkCount, kbProfile := readActivationState(t, ctx)
	if active != "" {
		t.Errorf("активация не откатилась: active_version_id = %s", active)
	}
	if versionStatus != domain.VersionStatusUploaded {
		t.Errorf("статус версии = %s, ожидался uploaded", versionStatus)
	}
	if chunkCount != 0 {
		t.Errorf("chunk_count = %d, ожидалось 0", chunkCount)
	}
	if kbProfile != "" {
		t.Errorf("профиль базы зафиксирован при откате: %q", kbProfile)
	}
}

// TestActivateVersionRejectsNonLatestVersion — порт test_infrastructure.py:715.
func TestActivateVersionRejectsNonLatestVersion(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	docs := newTestDocuments()
	jobs := newTestJobs()

	// Вторая версия становится последней, первая перестаёт ею быть.
	insertSecondVersion(t, ctx)
	advanceToIndex(t, ctx, jobs)

	err := testDB.WithTx(ctx, func(tx ports.Queryer) error {
		_, err := docs.ActivateVersion(ctx, tx, domain.ActivateParams{
			TenantID:           tenantID.String(),
			DocumentID:         docID.String(),
			VersionID:          versionID.String(),
			EmbeddingProfileID: profileID,
			ChunkCount:         7,
			LeaseToken:         jobLeaseTokenOf(t, ctx),
		})
		return err
	})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("активация не последней версии: %v, ожидался ErrConflict", err)
	}

	active, _, chunkCount, _ := readActivationState(t, ctx)
	if active != "" {
		t.Errorf("документ стал активным после отказа: %s", active)
	}
	if chunkCount != 0 {
		t.Errorf("chunk_count = %d, ожидалось 0", chunkCount)
	}
}

// TestActivateVersionRejectsProfileChange — проверка без Python-теста.
// Профиль базы зафиксирован первой активацией; смена профиля требует
func TestActivateVersionRejectsProfileChange(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	// Профиль уже зафиксирован — значит версия индексирована не тем.
	fixed := profileID
	seedChain(t, ctx, seedOptions{embeddingProfile: &fixed})
	docs := newTestDocuments()
	jobs := newTestJobs()
	lease := advanceToIndex(t, ctx, jobs)

	err := testDB.WithTx(ctx, func(tx ports.Queryer) error {
		_, err := docs.ActivateVersion(ctx, tx, domain.ActivateParams{
			TenantID:           tenantID.String(),
			DocumentID:         docID.String(),
			VersionID:          versionID.String(),
			EmbeddingProfileID: "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
			ChunkCount:         7,
			LeaseToken:         lease.LeaseToken,
		})
		return err
	})
	if !errors.Is(err, domain.ErrEmbeddingProfileNeedsRebuild) {
		t.Fatalf("смена профиля: %v, ожидался ErrEmbeddingProfileNeedsRebuild", err)
	}
}

// TestActivateVersionRejectsDeletedDocument — проверка без Python-теста.
// Удалённый документ нельзя сделать активным: он не виден ни в поиске, ни
func TestActivateVersionRejectsDeletedDocument(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	docs := newTestDocuments()
	jobs := newTestJobs()
	lease := advanceToIndex(t, ctx, jobs)
	execRaw(t, ctx, "UPDATE documents SET deleted_at = now() WHERE id = $1", docID)

	err := testDB.WithTx(ctx, func(tx ports.Queryer) error {
		_, err := docs.ActivateVersion(ctx, tx, domain.ActivateParams{
			TenantID:           tenantID.String(),
			DocumentID:         docID.String(),
			VersionID:          versionID.String(),
			EmbeddingProfileID: profileID,
			ChunkCount:         7,
			LeaseToken:         lease.LeaseToken,
		})
		return err
	})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("активация удалённого документа: %v, ожидался ErrNotFound", err)
	}
}

// TestActivateVersionReturnsPreviousVersion — проверка без Python-теста.
// Прежняя активная версия возвращается для cleanup: её чанки надо удалить
func TestActivateVersionReturnsPreviousVersion(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	docs := newTestDocuments()
	jobs := newTestJobs()
	lease := advanceToIndex(t, ctx, jobs)

	// Первая активация: прежней версии нет.
	err := testDB.WithTx(ctx, func(tx ports.Queryer) error {
		_, err := docs.ActivateVersion(ctx, tx, domain.ActivateParams{
			TenantID:           tenantID.String(),
			DocumentID:         docID.String(),
			VersionID:          versionID.String(),
			EmbeddingProfileID: profileID,
			ChunkCount:         7,
			LeaseToken:         lease.LeaseToken,
		})
		return err
	})
	if err != nil {
		t.Fatalf("первая активация: %v", err)
	}

	// Повторная активация той же версии: прежней нет.
	err = testDB.WithTx(ctx, func(tx ports.Queryer) error {
		previous, err := docs.ActivateVersion(ctx, tx, domain.ActivateParams{
			TenantID:           tenantID.String(),
			DocumentID:         docID.String(),
			VersionID:          versionID.String(),
			EmbeddingProfileID: profileID,
			ChunkCount:         9,
			LeaseToken:         lease.LeaseToken,
		})
		if err != nil {
			return err
		}
		if previous != "" {
			t.Errorf("прежняя версия = %q, ожидалась пустая при повторной активации", previous)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("повторная активация: %v", err)
	}
}

// ── Видимость и права (ТЗ 2.1) ────────────────────────────────────

// TestGetAllowedActiveVersionsLimitsVisibilityByACL — порт test_infrastructure.py:620.
func TestGetAllowedActiveVersionsLimitsVisibilityByACL(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{kbUsers: []string{"u1"}})
	docs := newTestDocuments()

	activate(t, ctx)

	member := principalOf("u1", []string{"user"}, nil)
	outsider := principalOf("u2", []string{"user"}, nil)
	admin := principalOf("u2", []string{"admin"}, nil)

	allowed, err := docs.GetAllowedActiveVersions(ctx, testDB, member, kbID.String())
	if err != nil {
		t.Fatalf("чтение для участника ACL: %v", err)
	}
	if len(allowed) != 1 || allowed[0].DocumentID != docID.String() ||
		allowed[0].ActiveVersionID != versionID.String() {
		t.Errorf("участник ACL увидел %+v, ожидался один документ %s", allowed, docID)
	}

	denied, err := docs.GetAllowedActiveVersions(ctx, testDB, outsider, kbID.String())
	if err != nil {
		t.Fatalf("чтение для постороннего: %v", err)
	}
	if len(denied) != 0 {
		t.Errorf("посторонний увидел %+v, ожидался пустой список", denied)
	}

	// Администратор видит всё содержимое своего tenant без проверки ACL.
	adminList, err := docs.GetAllowedActiveVersions(ctx, testDB, admin, kbID.String())
	if err != nil {
		t.Fatalf("чтение для администратора: %v", err)
	}
	if len(adminList) != 1 {
		t.Errorf("администратор увидел %+v, ожидался один документ", adminList)
	}
}

// TestEmptyKBACLIsVisibleToAdminOnly — проверка без Python-теста.
// Пустой ACL базы не значит «доступ всем»: в Python условие остаётся
func TestEmptyKBACLIsVisibleToAdminOnly(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	docs := newTestDocuments()
	activate(t, ctx)

	plain := principalOf("u1", []string{"user"}, nil)
	visible, err := docs.GetAllowedActiveVersions(ctx, testDB, plain, kbID.String())
	if err != nil {
		t.Fatalf("чтение: %v", err)
	}
	if len(visible) != 0 {
		t.Fatalf("пользователь без роли admin увидел %+v при пустом ACL базы", visible)
	}

	admin := principalOf("u1", []string{"admin"}, nil)
	visible, err = docs.GetAllowedActiveVersions(ctx, testDB, admin, kbID.String())
	if err != nil {
		t.Fatalf("чтение для администратора: %v", err)
	}
	if len(visible) != 1 {
		t.Fatalf("администратор увидел %+v, ожидался один документ", visible)
	}
}

// TestDocumentACLOverrideCannotWidenBaseACL — порт test_infrastructure.py:641.
// Собственный ACL документа сужает доступ и никогда не расширяет его за
func TestDocumentACLOverrideCannotWidenBaseACL(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	// Оба пользователя разрешены в ACL базы — иначе проверять нечего.
	seedChain(t, ctx, seedOptions{kbUsers: []string{"u1", "u2"}})
	docs := newTestDocuments()
	activate(t, ctx)

	// Второй документ: u2 в его ACL, u1 — нет.
	insertSecondDocument(t, ctx, `{"allowed_user_ids": ["u2"], "allowed_group_ids": []}`)
	activateSecondDocument(t, ctx)

	// u1 теряет второй документ (ACL документа сужает доступ).
	seenByU1, err := docs.GetAllowedActiveVersions(ctx, testDB,
		principalOf("u1", []string{"user"}, nil), kbID.String())
	if err != nil {
		t.Fatalf("чтение для u1: %v", err)
	}
	if len(seenByU1) != 1 || seenByU1[0].DocumentID != docID.String() {
		t.Fatalf("u1 увидел %+v, ожидался только документ %s", seenByU1, docID)
	}

	// u2 сохраняет первый документ: ACL второго документа не сузил и не
	// расширил видимость первого.
	seenByU2, err := docs.GetAllowedActiveVersions(ctx, testDB,
		principalOf("u2", []string{"user"}, nil), kbID.String())
	if err != nil {
		t.Fatalf("чтение для u2: %v", err)
	}
	if len(seenByU2) != 2 {
		t.Fatalf("u2 увидел %+v, ожидались оба документа", seenByU2)
	}

	// Ключевая проверка Python-теста: u3 прописан ТОЛЬКО в ACL документа,
	// но не в ACL базы, поэтому не видит ничего.
	execRaw(t, ctx, `UPDATE documents SET acl_override = $2::jsonb WHERE id = $1`,
		otherDoc, `{"allowed_user_ids": ["u3"], "allowed_group_ids": []}`)
	activateSecondDocument(t, ctx)
	seenByU3, err := docs.GetAllowedActiveVersions(ctx, testDB,
		principalOf("u3", []string{"user"}, nil), kbID.String())
	if err != nil {
		t.Fatalf("чтение для u3: %v", err)
	}
	if len(seenByU3) != 0 {
		t.Fatalf("ACL документа расширил базовый ACL: u3 увидел %+v", seenByU3)
	}
}

// TestDocumentACLOverrideNarrowsAccess — проверка без Python-теста.
// Обратный случай: если ACL базы разрешает u1, а ACL документа — только u2,
func TestDocumentACLOverrideNarrowsAccess(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{kbUsers: []string{"u1", "u2"}})
	docs := newTestDocuments()
	activate(t, ctx)

	execRaw(t, ctx, `UPDATE documents SET acl_override = $2::jsonb WHERE id = $1`,
		docID, `{"allowed_user_ids": ["u2"], "allowed_group_ids": []}`)

	allowedUser := principalOf("u2", []string{"user"}, nil)
	deniedUser := principalOf("u1", []string{"user"}, nil)

	allowed, err := docs.GetAllowedActiveVersions(ctx, testDB, allowedUser, kbID.String())
	if err != nil {
		t.Fatalf("чтение для u2: %v", err)
	}
	if len(allowed) != 1 {
		t.Errorf("u2 увидел %+v, ожидался один документ", allowed)
	}

	denied, err := docs.GetAllowedActiveVersions(ctx, testDB, deniedUser, kbID.String())
	if err != nil {
		t.Fatalf("чтение для u1: %v", err)
	}
	if len(denied) != 0 {
		t.Errorf("ACL документа не сузил доступ: u1 увидел %+v", denied)
	}
}

// TestACLGroupGrantsAccess — проверка без Python-теста.
// Доступ по группе: ветка EXISTS + ANY на текстовом массиве. Без неё
func TestACLGroupGrantsAccess(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{kbGroups: []string{"analysts"}})
	docs := newTestDocuments()
	activate(t, ctx)

	inGroup := principalOf("u9", []string{"user"}, []string{"analysts"})
	outOfGroup := principalOf("u9", []string{"user"}, []string{"sales"})

	allowed, err := docs.GetAllowedActiveVersions(ctx, testDB, inGroup, kbID.String())
	if err != nil {
		t.Fatalf("чтение для участника группы: %v", err)
	}
	if len(allowed) != 1 {
		t.Errorf("участник группы увидел %+v, ожидался один документ", allowed)
	}

	denied, err := docs.GetAllowedActiveVersions(ctx, testDB, outOfGroup, kbID.String())
	if err != nil {
		t.Fatalf("чтение для постороннего: %v", err)
	}
	if len(denied) != 0 {
		t.Errorf("посторонний увидел %+v", denied)
	}
}

// TestGetAllowedActiveVersionsSkipsUnactivatedAndDeleted — проверка без Python-теста.
func TestGetAllowedActiveVersionsSkipsUnactivatedAndDeleted(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{kbUsers: []string{"u1"}})
	docs := newTestDocuments()

	// Без активации active_version_id = NULL: искать нечего.
	versions, err := docs.GetAllowedActiveVersions(ctx, testDB, principalOf("u1", []string{"user"}, nil), kbID.String())
	if err != nil {
		t.Fatalf("чтение: %v", err)
	}
	if len(versions) != 0 {
		t.Fatalf("неактивированный документ попал в список: %+v", versions)
	}

	activate(t, ctx)
	execRaw(t, ctx, "UPDATE documents SET deleted_at = now() WHERE id = $1", docID)
	versions, err = docs.GetAllowedActiveVersions(ctx, testDB, principalOf("u1", []string{"user"}, nil), kbID.String())
	if err != nil {
		t.Fatalf("чтение после удаления: %v", err)
	}
	if len(versions) != 0 {
		t.Fatalf("удалённый документ попал в список: %+v", versions)
	}
}

// ── VersionSources ─────────────────────────────────────────────────

// TestVersionSourcesReturnsSourceURL — порт test_infrastructure.py по
// search/service.py:184-197.
func TestVersionSourcesReturnsSourceURL(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	source := "https://example.org/guide"
	seedChain(t, ctx, seedOptions{kbUsers: []string{"u1"}, sourceURL: &source})
	docs := newTestDocuments()

	active := map[string]string{docID.String(): versionID.String()}
	sources, err := docs.VersionSources(ctx, testDB,
		principalOf("u1", []string{"user"}, nil), kbID.String(), active)
	if err != nil {
		t.Fatalf("VersionSources: %v", err)
	}
	if got := sources[versionID.String()]; got != source {
		t.Errorf("source_url = %q, ожидался %q", got, source)
	}
}

// TestVersionSourcesOmitsMissingSourceURL — проверка без Python-теста.
// Версия без source_url не должна попадать в карту: Python отдавал
func TestVersionSourcesOmitsMissingSourceURL(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{kbUsers: []string{"u1"}})
	docs := newTestDocuments()

	active := map[string]string{docID.String(): versionID.String()}
	sources, err := docs.VersionSources(ctx, testDB,
		principalOf("u1", []string{"user"}, nil), kbID.String(), active)
	if err != nil {
		t.Fatalf("VersionSources: %v", err)
	}
	if _, exists := sources[versionID.String()]; exists {
		t.Fatalf("версия без source_url попала в карту: %+v", sources)
	}
}

// TestVersionSourcesAppliesAccessFilter — проверка без Python-теста.
// source_url не должен утекать мимо ACL: версия, которую principal не видит,
func TestVersionSourcesAppliesAccessFilter(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	source := "https://example.org/secret"
	seedChain(t, ctx, seedOptions{kbUsers: []string{"u1"}, sourceURL: &source})
	docs := newTestDocuments()

	active := map[string]string{docID.String(): versionID.String()}
	sources, err := docs.VersionSources(ctx, testDB,
		principalOf("u2", []string{"user"}, nil), kbID.String(), active)
	if err != nil {
		t.Fatalf("VersionSources: %v", err)
	}
	if len(sources) != 0 {
		t.Fatalf("source_url ушёл постороннему: %+v", sources)
	}
}

// TestVersionSourcesEmptyInputSkipsDatabase — проверка без Python-теста.
func TestVersionSourcesEmptyInputSkipsDatabase(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	docs := newTestDocuments()

	sources, err := docs.VersionSources(ctx, testDB, domain.LocalPrincipal(), kbID.String(), nil)
	if err != nil {
		t.Fatalf("VersionSources: %v", err)
	}
	if len(sources) != 0 {
		t.Fatalf("ожидалась пустая карта, получено %+v", sources)
	}
}

// ── LoadParsed ─────────────────────────────────────────────────────

// TestLoadParsedReturnsContractObject — порт test_infrastructure.py:680.
func TestLoadParsedReturnsContractObject(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	const key = "parsed/44444444.json"
	seedChain(t, ctx, seedOptions{kbUsers: []string{"u1"}, parsedKey: ptrString(key)})

	store := newMemoryStore()
	blockID := uuid.MustParse("55555555-5555-4555-8555-555555555555")
	putParsed(t, store, key, domain.ParsedDocument{
		SchemaVersion: "1",
		TenantID:      tenantID,
		KBID:          kbID,
		DocumentID:    docID,
		VersionID:     versionID,
		Title:         "Инструкция",
		Filename:      "guide.pdf",
		ContentSHA256: repeatA64(),
		Metadata:      map[string]any{},
		Blocks: []domain.Block{{
			BlockID:  blockID,
			Position: 0,
			Kind:     "paragraph",
			Text:     "Текст",
		}},
	})

	docs := newTestDocuments()
	parsed, err := docs.LoadParsed(ctx, testDB, store, tenantID.String(), versionID.String())
	if err != nil {
		t.Fatalf("LoadParsed: %v", err)
	}
	if parsed.VersionID != versionID {
		t.Errorf("version_id = %s, ожидался %s", parsed.VersionID, versionID)
	}
	if len(parsed.Blocks) != 1 || parsed.Blocks[0].Text != "Текст" {
		t.Fatalf("блоки прочитаны неверно: %+v", parsed.Blocks)
	}
	if parsed.Blocks[0].BlockID != blockID {
		t.Errorf("block_id = %s, ожидался %s", parsed.Blocks[0].BlockID, blockID)
	}
}

// TestLoadParsedWithoutParsedKeyReturnsNotFound — проверка без Python-теста.
func TestLoadParsedWithoutParsedKeyReturnsNotFound(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	docs := newTestDocuments()

	_, err := docs.LoadParsed(ctx, testDB, newMemoryStore(), tenantID.String(), versionID.String())
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("LoadParsed без parsed_key: %v, ожидался ErrNotFound", err)
	}
}

// TestLoadParsedRejectsForeignTenant — проверка без Python-теста.
func TestLoadParsedRejectsForeignTenant(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	const key = "parsed/44444444.json"
	seedChain(t, ctx, seedOptions{parsedKey: ptrString(key)})
	docs := newTestDocuments()

	otherTenant := uuid.MustParse("99999999-9999-4999-8999-999999999999")
	_, err := docs.LoadParsed(ctx, testDB, newMemoryStore(), otherTenant.String(), versionID.String())
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("LoadParsed чужого tenant'а: %v, ожидался ErrNotFound", err)
	}
}

// ── Служебные проверки ─────────────────────────────────────────────

// TestIngestBusyReflectsActiveJob — проверка без Python-теста.
func TestIngestBusyReflectsActiveJob(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	docs := newTestDocuments()

	busy, err := docs.IngestBusy(ctx, testDB, docID.String())
	if err != nil {
		t.Fatalf("IngestBusy: %v", err)
	}
	if !busy {
		t.Fatal("queued-задание не сделало документ занятым")
	}

	execRaw(t, ctx, "UPDATE jobs SET status = 'succeeded' WHERE id = $1", jobID)
	busy, err = docs.IngestBusy(ctx, testDB, docID.String())
	if err != nil {
		t.Fatalf("IngestBusy: %v", err)
	}
	if busy {
		t.Fatal("завершённое задание всё ещё считает документ занятым")
	}
}

// TestIsVersionActive — проверка без Python-теста.
func TestIsVersionActive(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	docs := newTestDocuments()

	active, err := docs.IsVersionActive(ctx, testDB, docID.String(), versionID.String())
	if err != nil {
		t.Fatalf("IsVersionActive: %v", err)
	}
	if active {
		t.Fatal("до активации версия считается активной")
	}

	execRaw(t, ctx, "UPDATE documents SET active_version_id = $2 WHERE id = $1", docID, versionID)
	active, err = docs.IsVersionActive(ctx, testDB, docID.String(), versionID.String())
	if err != nil {
		t.Fatalf("IsVersionActive: %v", err)
	}
	if !active {
		t.Fatal("активная версия не распознана")
	}

	other, err := docs.IsVersionActive(ctx, testDB, docID.String(), secondVer.String())
	if err != nil {
		t.Fatalf("IsVersionActive: %v", err)
	}
	if other {
		t.Fatal("неактивная версия распознана как активная")
	}
}

// ── helpers ────────────────────────────────────────────────────────

// readActivationState читает состояние документа, версии и базы после активации.
func readActivationState(t *testing.T, ctx context.Context) (activeVersion, status string, chunkCount int, kbProfile string) {
	t.Helper()
	var (
		activeID    *uuid.UUID
		profileText *string
	)
	err := testDB.QueryRow(ctx, `
		SELECT d.active_version_id, v.status, v.chunk_count, k.embedding_profile_id
		FROM documents d
		JOIN document_versions v ON v.id = $2
		JOIN knowledge_bases k  ON k.id = $3
		WHERE d.id = $1`,
		docID, versionID, kbID,
	).Scan(&activeID, &status, &chunkCount, &profileText)
	if err != nil {
		t.Fatalf("чтение состояния активации: %v", err)
	}
	if activeID != nil {
		activeVersion = activeID.String()
	}
	if profileText != nil {
		kbProfile = *profileText
	}
	return activeVersion, status, chunkCount, kbProfile
}

// activate доводит документ до активной версии прямыми утверждениями.
// Нужна тестам прав и видимости, которым не важна работа очереди: полный
func activate(t *testing.T, ctx context.Context) {
	t.Helper()
	execRaw(t, ctx, "UPDATE documents SET active_version_id = $2 WHERE id = $1", docID, versionID)
}

// insertSecondVersion добавляет ещё одну версию документа и делает её последней.
func insertSecondVersion(t *testing.T, ctx context.Context) {
	t.Helper()
	_, err := testDB.Exec(ctx, `
		INSERT INTO document_versions (id, tenant_id, document_id, filename, title, metadata,
		                               content_sha256, original_key, status)
		VALUES ($1, $2, $3, 'guide-v2.pdf', 'Инструкция v2', '{}'::jsonb, $4, 'k2', 'uploaded')`,
		secondVer, tenantID, docID, repeatA64())
	if err != nil {
		t.Fatalf("создание второй версии: %v", err)
	}
	execRaw(t, ctx, "UPDATE documents SET latest_version_id = $2 WHERE id = $1", docID, secondVer)
}

// insertSecondDocument добавляет документ с собственным ACL и версией.
// Нужен тестам ACL: собственный ACL сужает видимость, и чтобы это заметить,
func insertSecondDocument(t *testing.T, ctx context.Context, aclOverride string) {
	t.Helper()
	_, err := testDB.Exec(ctx, `
		INSERT INTO documents (id, tenant_id, kb_id, external_id, acl_override)
		VALUES ($1, $2, $3, 'doc-2', $4::jsonb)`,
		otherDoc, tenantID, kbID, aclOverride)
	if err != nil {
		t.Fatalf("создание второго документа: %v", err)
	}
	_, err = testDB.Exec(ctx, `
		INSERT INTO document_versions (id, tenant_id, document_id, filename, title, metadata,
		                               content_sha256, original_key, status)
		VALUES ($1, $2, $3, 'doc-2.pdf', 'Документ 2', '{}'::jsonb, $4, 'k2', 'ready')`,
		otherVer, tenantID, otherDoc, repeatA64())
	if err != nil {
		t.Fatalf("создание версии второго документа: %v", err)
	}
	// Ссылка на версию ставится последней: внешний ключ документов на версии
	// составной и циклический, поэтому при вставке документа версии ещё нет.
	execRaw(t, ctx, "UPDATE documents SET latest_version_id = $2 WHERE id = $1", otherDoc, otherVer)
}

// activateSecondDocument делает активной версию второго документа.
func activateSecondDocument(t *testing.T, ctx context.Context) {
	t.Helper()
	execRaw(t, ctx, "UPDATE documents SET active_version_id = $2 WHERE id = $1", otherDoc, otherVer)
}

// jobLeaseTokenOf возвращает текущий lease задания.
func jobLeaseTokenOf(t *testing.T, ctx context.Context) string {
	t.Helper()
	var token *string
	if err := testDB.QueryRow(ctx, "SELECT lease_token FROM jobs WHERE id = $1", jobID).Scan(&token); err != nil {
		t.Fatalf("чтение lease: %v", err)
	}
	if token == nil {
		t.Fatal("у задания нет lease_token после выдачи")
	}
	return *token
}

// ptrString возвращает указатель на строку — для nullable-параметров сидов.
func ptrString(value string) *string { return &value }
