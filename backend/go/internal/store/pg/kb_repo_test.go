package postgres

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
)

// Тесты баз знаний и ACL.

// TestCreateKBStoresACLAndDoesNotCreateSettings — порт test_infrastructure.py
// для KBRepository.create (search/service.py:276-289) плюс требование
func TestCreateKBStoresACLAndDoesNotCreateSettings(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedTenant(t, ctx)
	repo := NewKBRepository()

	kb, err := repo.Create(ctx, testDB, domain.Principal{
		TenantID: tenantID, UserID: "local-dev", Roles: []string{"admin"},
	}, domain.CreateKBInput{
		Name: "Новая база",
		ACL:  domain.ACL{Users: []string{"u1", "u2"}, Groups: []string{"analysts"}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := uuid.Parse(kb.KBID); err != nil {
		t.Fatalf("kb_id = %q не UUID: %v", kb.KBID, err)
	}
	if kb.Name != "Новая база" {
		t.Errorf("name = %q", kb.Name)
	}
	// Профиль не зафиксирован до первой активации версии.
	if kb.EmbeddingProfileID != "" {
		t.Errorf("embedding_profile_id = %q, ожидалась пустая строка", kb.EmbeddingProfileID)
	}
	if kb.CreatedAt.IsZero() {
		t.Error("created_at не заполнен")
	}

	// ACL читается ровно таким, каким был записан.
	stored, err := repo.ACL(ctx, testDB, kb.KBID)
	if err != nil {
		t.Fatalf("ACL: %v", err)
	}
	if strings.Join(stored.Users, ",") != "u1,u2" {
		t.Errorf("allowed_user_ids = %v, ожидались u1,u2", stored.Users)
	}
	if strings.Join(stored.Groups, ",") != "analysts" {
		t.Errorf("allowed_group_ids = %v, ожидался analysts", stored.Groups)
	}

	// Настройки не созданы: их заводит администратор (ТЗ 2.2, 7.3).
	var settingsCount int
	if err := testDB.QueryRow(ctx,
		"SELECT count(*) FROM settings WHERE kb_id = $1", kb.KBID).Scan(&settingsCount); err != nil {
		t.Fatalf("подсчёт настроек: %v", err)
	}
	if settingsCount != 0 {
		t.Fatalf("создание базы создало %d настроек, ожидался 0", settingsCount)
	}
}

// TestCreateKBRejectsRolesInACL — проверка без Python-теста.
// domain.ACL умеет роли, а в схеме нет поля allowed_role_ids. Молча выбросить
func TestCreateKBRejectsRolesInACL(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedTenant(t, ctx)
	repo := NewKBRepository()

	_, err := repo.Create(ctx, testDB, domain.Principal{TenantID: tenantID}, domain.CreateKBInput{
		Name: "С ролями",
		ACL:  domain.ACL{Users: []string{"u1"}, Roles: []string{"admin"}},
	})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("ACL с ролями: %v, ожидался ErrConflict", err)
	}
}

// TestRequireReturnsNotFoundForForeignTenant — проверка без Python-теста.
// База другого tenant'а не существует для вызывающего, а не «запрещена»:
func TestRequireReturnsNotFoundForForeignTenant(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := NewKBRepository()

	if _, err := repo.Require(ctx, testDB, domain.Principal{TenantID: tenantID}, kbID.String()); err != nil {
		t.Fatalf("Require для своего tenant'а: %v", err)
	}

	foreign := uuid.MustParse("99999999-9999-4999-8999-999999999999")
	_, err := repo.Require(ctx, testDB, domain.Principal{TenantID: foreign}, kbID.String())
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Require для чужого tenant'а: %v, ожидался ErrNotFound", err)
	}

	_, err = repo.Require(ctx, testDB, domain.Principal{TenantID: tenantID}, uuid.New().String())
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Require несуществующей базы: %v, ожидался ErrNotFound", err)
	}
}

// TestListPaginatesWithoutOverlap — порт test_infrastructure.py:602-616
// для списка документов и search/service.py:291-313 для списка баз.
func TestListPaginatesWithoutOverlap(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedTenant(t, ctx)
	repo := NewKBRepository()
	principal := domain.Principal{TenantID: tenantID, UserID: "local-dev", Roles: []string{"admin"}}

	// created_at у всех совпадает: страницы пересеклись бы без вторичного
	// ключа сортировки по id.
	for i := 0; i < 4; i++ {
		if _, err := repo.Create(ctx, testDB, principal, domain.CreateKBInput{Name: "kb"}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	execRaw(t, ctx, "UPDATE knowledge_bases SET created_at = now()")

	first, cursor, err := repo.List(ctx, testDB, principal, 2, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("первая страница: %d записей, ожидалось 2", len(first))
	}
	if cursor == "" {
		t.Fatal("курсор следующей страницы не задан")
	}

	second, cursor2, err := repo.List(ctx, testDB, principal, 2, cursor)
	if err != nil {
		t.Fatalf("List со курсором: %v", err)
	}
	if len(second) != 2 {
		t.Fatalf("вторая страница: %d записей, ожидалось 2", len(second))
	}
	if cursor2 != "" {
		t.Errorf("курсор после последней страницы = %q, ожидалась пустая строка", cursor2)
	}

	seen := map[string]int{}
	for _, kb := range append(first, second...) {
		seen[kb.KBID]++
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("база %s встретилась %d раз, страницы пересеклись", id, count)
		}
	}
	if len(seen) != 4 {
		t.Errorf("получено %d уникальных баз, ожидалось 4", len(seen))
	}
}

// TestListReturnsEmptyCursorWhenNothingLeft — проверка без Python-теста.
func TestListReturnsEmptyCursorWhenNothingLeft(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedTenant(t, ctx)
	repo := NewKBRepository()

	items, cursor, err := repo.List(ctx, testDB, domain.Principal{TenantID: tenantID}, 20, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("ожидался пустой список, получено %d", len(items))
	}
	if cursor != "" {
		t.Errorf("курсор = %q, ожидалась пустая строка", cursor)
	}
}

// TestListRejectsBrokenCursor — битый курсор есть 422 INVALID_METADATA, не первая
// страница и не 500 (поведение сознательно отличается от Python — вопрос к приёмке).
func TestListRejectsBrokenCursor(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedTenant(t, ctx)
	repo := NewKBRepository()

	_, _, err := repo.List(ctx, testDB, domain.Principal{TenantID: tenantID}, 20, "не-base64-@@")
	appErr, ok := domain.AsAppError(err)
	if !ok || appErr.Code != domain.CodeInvalidMetadata {
		t.Fatalf("испорченный курсор: %v, ожидался INVALID_METADATA", err)
	}
	if appErr.Status != http.StatusUnprocessableEntity {
		t.Errorf("статус = %d, ожидался 422", appErr.Status)
	}
}

// TestSetACLReplacesBothLists — порт search/service.py:329-344.
func TestSetACLReplacesBothLists(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{kbUsers: []string{"u1"}})
	repo := NewKBRepository()

	err := testDB.WithTx(ctx, func(tx ports.Queryer) error {
		return repo.SetACL(ctx, tx, kbID.String(),
			domain.ACL{Users: []string{"u2", "u3"}, Groups: []string{"sales"}})
	})
	if err != nil {
		t.Fatalf("SetACL: %v", err)
	}

	stored, err := repo.ACL(ctx, testDB, kbID.String())
	if err != nil {
		t.Fatalf("ACL: %v", err)
	}
	if strings.Join(stored.Users, ",") != "u2,u3" {
		t.Errorf("allowed_user_ids = %v, ожидались u2,u3", stored.Users)
	}
	if strings.Join(stored.Groups, ",") != "sales" {
		t.Errorf("allowed_group_ids = %v, ожидался sales", stored.Groups)
	}
}

// TestSetACLRollsBackOnMissingKB — проверка без Python-теста.
func TestSetACLRollsBackOnMissingKB(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedTenant(t, ctx)
	repo := NewKBRepository()

	err := testDB.WithTx(ctx, func(tx ports.Queryer) error {
		return repo.SetACL(ctx, tx, uuid.New().String(), domain.ACL{Users: []string{"u1"}})
	})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("SetACL несуществующей базы: %v, ожидался ErrNotFound", err)
	}
}

// TestSetDocumentACLStoresAndClearsOverride — порт search/service.py:346-371.
// nil означает наследование ACL базы, объект — собственный ACL. Подменять
func TestSetDocumentACLStoresAndClearsOverride(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := NewKBRepository()

	err := testDB.WithTx(ctx, func(tx ports.Queryer) error {
		return repo.SetDocumentACL(ctx, tx, docID.String(),
			&domain.ACL{Users: []string{"u9"}, Groups: []string{"squad"}})
	})
	if err != nil {
		t.Fatalf("SetDocumentACL: %v", err)
	}

	stored := readDocumentACL(t, ctx, docID)
	if len(stored.Users) != 1 || stored.Users[0] != "u9" {
		t.Errorf("acl_override.users = %v, ожидался [u9]", stored.Users)
	}
	if len(stored.Groups) != 1 || stored.Groups[0] != "squad" {
		t.Errorf("acl_override.groups = %v, ожидался [squad]", stored.Groups)
	}

	// Снятие override возвращает NULL, а не пустой объект.
	err = testDB.WithTx(ctx, func(tx ports.Queryer) error {
		return repo.SetDocumentACL(ctx, tx, docID.String(), nil)
	})
	if err != nil {
		t.Fatalf("снятие ACL: %v", err)
	}
	var isNull bool
	if err := testDB.QueryRow(ctx,
		"SELECT acl_override IS NULL FROM documents WHERE id = $1", docID).Scan(&isNull); err != nil {
		t.Fatalf("проверка NULL: %v", err)
	}
	if !isNull {
		t.Fatal("acl_override не вернулся в NULL — документ закрылся бы для всех")
	}
}

// TestSetDocumentACLRejectsDeletedDocument — проверка без Python-теста.
func TestSetDocumentACLRejectsDeletedDocument(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := NewKBRepository()
	execRaw(t, ctx, "UPDATE documents SET deleted_at = now() WHERE id = $1", docID)

	err := testDB.WithTx(ctx, func(tx ports.Queryer) error {
		return repo.SetDocumentACL(ctx, tx, docID.String(), &domain.ACL{Users: []string{"u9"}})
	})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("ACL удалённого документа: %v, ожидался ErrNotFound", err)
	}
}

// TestEmbeddingProfileIDReflectsFirstActivation — проверка без Python-теста.
// Пустая строка означает «ещё не зафиксирован» и НЕ ошибку: база без
func TestEmbeddingProfileIDReflectsFirstActivation(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := NewKBRepository()

	profile, err := repo.EmbeddingProfileID(ctx, testDB, kbID.String())
	if err != nil {
		t.Fatalf("EmbeddingProfileID: %v", err)
	}
	if profile != "" {
		t.Fatalf("профиль = %q, ожидалась пустая строка до первой активации", profile)
	}

	execRaw(t, ctx, "UPDATE knowledge_bases SET embedding_profile_id = $2 WHERE id = $1", kbID, profileID)
	profile, err = repo.EmbeddingProfileID(ctx, testDB, kbID.String())
	if err != nil {
		t.Fatalf("EmbeddingProfileID: %v", err)
	}
	if profile != profileID {
		t.Fatalf("профиль = %q, ожидался %q", profile, profileID)
	}

	_, err = repo.EmbeddingProfileID(ctx, testDB, uuid.New().String())
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("профиль несуществующей базы: %v, ожидался ErrNotFound", err)
	}
}

// TestRepositoriesRejectMalformedUUIDs — проверка без Python-теста.
// Идентификаторы в портах строковые, поэтому неверный UUID — дефект
func TestRepositoriesRejectMalformedUUIDs(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	principal := domain.Principal{TenantID: tenantID}

	kbRepo := NewKBRepository()
	docRepo := newTestDocuments()
	jobRepo := newTestJobs()

	if _, err := kbRepo.Require(ctx, testDB, principal, "не-uuid"); err == nil {
		t.Error("Require принял неверный kb_id")
	}
	if _, err := kbRepo.EmbeddingProfileID(ctx, testDB, "не-uuid"); err == nil {
		t.Error("EmbeddingProfileID принял неверный kb_id")
	}
	if _, err := docRepo.GetAllowedActiveVersions(ctx, testDB, principal, "не-uuid"); err == nil {
		t.Error("GetAllowedActiveVersions принял неверный kb_id")
	}
	if _, err := docRepo.IngestBusy(ctx, testDB, "не-uuid"); err == nil {
		t.Error("IngestBusy принял неверный document_id")
	}
	if _, err := jobRepo.Claim(ctx, testDB, "worker", nil); !errors.Is(err, domain.ErrConfigurationInvalid) {
		t.Errorf("Claim без стадий: %v, ожидался ErrConfigurationInvalid", err)
	}
}
