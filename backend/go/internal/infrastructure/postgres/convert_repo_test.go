package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/kbsearch/backend-go/internal/application/ports"
	"github.com/kbsearch/backend-go/internal/domain"
)

// Тесты стадии convert на живом PostgreSQL: LoadVersion читает версию с
// оригиналом, FinishConvert переводит задание на стадию index.

const convertParsedKey = "parsed/convert.json"

// ── LoadVersion ────────────────────────────────────────────────────

// TestLoadVersionReturnsRowWithOriginal — стадия convert получает атрибуты
// версии и байты оригинала одним вызовом: разбор без оригинала невозможен,
// а расхождение атрибутов и байтов означало бы разбор не той версии.
func TestLoadVersionReturnsRowWithOriginal(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	sourceURL := "https://portal.example/guide"
	seedChain(t, ctx, seedOptions{sourceURL: &sourceURL})

	store := newMemoryStore()
	key := versionID.String() + "/original.bin"
	original := []byte("# Раздел\n\nтекст\n")
	if err := store.Put(ctx, key, original); err != nil {
		t.Fatalf("Put: %v", err)
	}

	docs := newTestDocuments()
	version, err := docs.LoadVersion(ctx, testDB, store, tenantID.String(), versionID.String())
	if err != nil {
		t.Fatalf("LoadVersion: %v", err)
	}

	if version.Filename != "guide.pdf" || version.Title != "Инструкция" {
		t.Errorf("filename/title %q/%q", version.Filename, version.Title)
	}
	if version.SourceURL == nil || *version.SourceURL != sourceURL {
		t.Errorf("source_url %v, ожидался %q", version.SourceURL, sourceURL)
	}
	if version.ContentSHA256 != repeatA64() {
		t.Errorf("content_sha256 %q вместо эталона", version.ContentSHA256)
	}
	if string(version.Content) != string(original) {
		t.Errorf("оригинал %q вместо %q", version.Content, original)
	}
}

// TestLoadVersionFailsWhenOriginalMissing — версия есть, оригинала нет:
// стадия обязана упасть, а не разобрать пустой документ и записать его как
// успешный результат.
func TestLoadVersionFailsWhenOriginalMissing(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})

	docs := newTestDocuments()
	if _, err := docs.LoadVersion(
		ctx, testDB, newMemoryStore(), tenantID.String(), versionID.String()); err == nil {
		t.Fatal("ожидалась ошибка чтения отсутствующего оригинала")
	}
}

// TestLoadVersionRejectsForeignTenant — чужой tenant не должен прочитать
// оригинал версии: идентификатор версии известен по URL-ответам, и без
// проверки tenant'а это была бы утечка содержимого документов.
func TestLoadVersionRejectsForeignTenant(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})

	store := newMemoryStore()
	key := versionID.String() + "/original.bin"
	if err := store.Put(ctx, key, []byte("текст")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	docs := newTestDocuments()
	_, err := docs.LoadVersion(ctx, testDB, store, otherTenant.String(), versionID.String())
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("получено %v вместо NOT_FOUND для чужого tenant'а", err)
	}
}

// TestLoadVersionReturnsMetadata — метаданные версии доезжают до
// ParsedDocument: по ним клиент фильтрует и отображает источник.
func TestLoadVersionReturnsMetadata(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})

	if _, err := testDB.Exec(ctx, `
		UPDATE document_versions SET metadata = '{"product":"portal"}'::jsonb WHERE id = $1`,
		versionID); err != nil {
		t.Fatalf("обновление metadata: %v", err)
	}

	store := newMemoryStore()
	key := versionID.String() + "/original.bin"
	if err := store.Put(ctx, key, []byte("текст")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	docs := newTestDocuments()
	version, err := docs.LoadVersion(ctx, testDB, store, tenantID.String(), versionID.String())
	if err != nil {
		t.Fatalf("LoadVersion: %v", err)
	}
	if version.Metadata["product"] != "portal" {
		t.Errorf("metadata = %v, ожидалось product=portal", version.Metadata)
	}
}

// ── FinishConvert ──────────────────────────────────────────────────

// convertLease забирает задание стадии convert под lease.
func convertLease(t *testing.T, ctx context.Context, repo *JobRepository) *domain.JobLease {
	t.Helper()
	lease, err := repo.Claim(ctx, testDB, "worker-1", []string{domain.StageConvert})
	if err != nil || lease == nil {
		t.Fatalf("Claim стадии convert: %v (lease=%v)", err, lease)
	}
	return lease
}

// finishConvert выполняет FinishConvert в транзакции, как это делает стадия.
func finishConvert(
	t *testing.T, ctx context.Context, repo *JobRepository, lease *domain.JobLease,
) error {
	t.Helper()
	return testDB.WithTx(ctx, func(tx ports.Queryer) error {
		return repo.FinishConvert(ctx, tx, lease.JobID.String(), lease.LeaseToken,
			domain.ConversionResult{
				VersionID:  versionID.String(),
				ParsedKey:  convertParsedKey,
				BlockCount: 7,
			})
	})
}

// TestFinishConvertMarksVersionParsedAndAdvancesJob — успешная стадия convert:
// версия получает parsed_key и статус parsed, задание переводится на стадию
// index и снова берётся воркером. Без этого документ навсегда остался бы в
// uploaded, и index никогда бы не был поставлен.
func TestFinishConvertMarksVersionParsedAndAdvancesJob(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := newTestJobs()

	lease := convertLease(t, ctx, repo)
	if err := finishConvert(t, ctx, repo, lease); err != nil {
		t.Fatalf("FinishConvert: %v", err)
	}

	var (
		parsedKey *string
		status    string
	)
	if err := testDB.QueryRow(ctx,
		"SELECT parsed_key, status FROM document_versions WHERE id = $1", versionID,
	).Scan(&parsedKey, &status); err != nil {
		t.Fatalf("чтение версии: %v", err)
	}
	if parsedKey == nil || *parsedKey != convertParsedKey {
		t.Errorf("parsed_key %v вместо %q", parsedKey, convertParsedKey)
	}
	if status != domain.VersionStatusParsed {
		t.Errorf("статус версии %q вместо parsed", status)
	}

	record := getJob(t, ctx, repo)
	if record.Stage != domain.StageIndex {
		t.Errorf("стадия задания %q вместо index", record.Stage)
	}
	if record.Status != domain.JobStatusQueued {
		t.Errorf("статус задания %q вместо queued", record.Status)
	}
	// Счётчик попыток обнуляется: стадия сменилась, и её попытки считаются
	// заново, иначе index получил бы ноль повторов после неудач на convert.
	if record.Attempt != 0 {
		t.Errorf("attempt = %d вместо 0", record.Attempt)
	}
	if record.ProcessedCount != 7 {
		t.Errorf("processed_chunks = %d вместо 7", record.ProcessedCount)
	}
}

// TestFinishConvertReleasesLease — после перевода задание обязано быть
// выдано заново: живой lease на старой стадии оставил бы задание в вечной
// обработке, и документ завис бы.
func TestFinishConvertReleasesLease(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := newTestJobs()

	lease := convertLease(t, ctx, repo)
	if err := finishConvert(t, ctx, repo, lease); err != nil {
		t.Fatalf("FinishConvert: %v", err)
	}

	next, err := repo.Claim(ctx, testDB, "worker-2", []string{domain.StageIndex})
	if err != nil {
		t.Fatalf("Claim стадии index: %v", err)
	}
	if next == nil {
		t.Fatal("задание не выдалось на стадии index после convert")
	}
	if next.Stage != domain.StageIndex {
		t.Errorf("выдана стадия %q вместо index", next.Stage)
	}
}

// TestFinishConvertIsIdempotentThroughSingleRow — перевод задания делается
// UPDATE-ом одной строки: второе задание на индексацию появиться не может.
// Проверяется тем, что у документа остаётся ровно одно ingest-задание.
func TestFinishConvertKeepsSingleIngestJob(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := newTestJobs()

	lease := convertLease(t, ctx, repo)
	if err := finishConvert(t, ctx, repo, lease); err != nil {
		t.Fatalf("FinishConvert: %v", err)
	}

	var count int
	if err := testDB.QueryRow(ctx, `
		SELECT count(*) FROM jobs WHERE document_id = $1 AND kind = 'ingest'`,
		docID).Scan(&count); err != nil {
		t.Fatalf("подсчёт заданий: %v", err)
	}
	if count != 1 {
		t.Errorf("ingest-заданий %d вместо 1", count)
	}
}

// TestFinishConvertRejectsSpentLease — повтор с израсходованным lease
// отвергается. Без проверки второй воркер, взявший задание после таймаута
// первого, дописал бы parsed_key поверх чужого результата.
func TestFinishConvertRejectsSpentLease(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := newTestJobs()

	lease := convertLease(t, ctx, repo)
	if err := finishConvert(t, ctx, repo, lease); err != nil {
		t.Fatalf("первый FinishConvert: %v", err)
	}

	err := testDB.WithTx(ctx, func(tx ports.Queryer) error {
		return repo.FinishConvert(ctx, tx, lease.JobID.String(), lease.LeaseToken,
			domain.ConversionResult{VersionID: versionID.String(), ParsedKey: convertParsedKey})
	})
	if !errors.Is(err, domain.ErrJobLeaseLost) {
		t.Fatalf("получено %v вместо JOB_LEASE_LOST на израсходованном lease", err)
	}
}

// TestFinishConvertRejectsForeignLeaseToken — чужой токен отвергается: lease
// выдаётся под конкретного воркера, и запись под чужим токеном означала бы,
// что отработал не тот, кто держал задание.
func TestFinishConvertRejectsForeignLeaseToken(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := newTestJobs()

	lease := convertLease(t, ctx, repo)
	err := testDB.WithTx(ctx, func(tx ports.Queryer) error {
		return repo.FinishConvert(ctx, tx, lease.JobID.String(), "чужой-токен",
			domain.ConversionResult{VersionID: versionID.String(), ParsedKey: convertParsedKey})
	})
	if !errors.Is(err, domain.ErrJobLeaseLost) {
		t.Fatalf("получено %v вместо JOB_LEASE_LOST на чужом токене", err)
	}

	var status string
	if err := testDB.QueryRow(ctx,
		"SELECT status FROM document_versions WHERE id = $1", versionID).Scan(&status); err != nil {
		t.Fatalf("чтение версии: %v", err)
	}
	if status == domain.VersionStatusParsed {
		t.Error("версия помечена parsed по чужому lease")
	}
}

// TestFinishConvertRejectsIndexStageJob — задание на стадии index нельзя
// завершить через FinishConvert: это молча закрыло бы чужую стадию.
func TestFinishConvertRejectsIndexStageJob(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{stage: domain.StageIndex, status: domain.JobStatusQueued})
	repo := newTestJobs()

	lease, err := repo.Claim(ctx, testDB, "worker-1", []string{domain.StageIndex})
	if err != nil || lease == nil {
		t.Fatalf("Claim стадии index: %v (lease=%v)", err, lease)
	}

	err = testDB.WithTx(ctx, func(tx ports.Queryer) error {
		return repo.FinishConvert(ctx, tx, lease.JobID.String(), lease.LeaseToken,
			domain.ConversionResult{VersionID: versionID.String(), ParsedKey: convertParsedKey})
	})
	if !errors.Is(err, domain.ErrJobLeaseLost) {
		t.Fatalf("получено %v вместо JOB_LEASE_LOST для чужой стадии", err)
	}
}

// TestFinishConvertRejectsAnotherVersion — результат не должен относиться к
// другой версии: parsed_key чужой версии у этой версии сделал бы разобранным
// не тот документ.
func TestFinishConvertRejectsAnotherVersion(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := newTestJobs()

	lease := convertLease(t, ctx, repo)
	err := testDB.WithTx(ctx, func(tx ports.Queryer) error {
		return repo.FinishConvert(ctx, tx, lease.JobID.String(), lease.LeaseToken,
			domain.ConversionResult{
				VersionID:  uuid.NewString(),
				ParsedKey:  convertParsedKey,
				BlockCount: 1,
			})
	})
	if err == nil {
		t.Fatal("ожидался отказ по чужой версии")
	}
	if errors.Is(err, domain.ErrNotFound) {
		t.Errorf("получено %v вместо CONFLICT: версии документа нет", err)
	}
}

// TestFinishConvertClearsPreviousError — версия, дошедшая до failed на
// предыдущей попытке, обязана очистить ошибку вместе со сменой статуса:
// иначе документ числился бы обработанным и с видимым кодом ошибки.
func TestFinishConvertClearsPreviousError(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{status: domain.JobStatusQueued})
	if _, err := testDB.Exec(ctx, `
		UPDATE document_versions
		SET status = 'failed', failed_stage = 'convert',
		    error_code = 'INVALID_METADATA', error_message = 'битый файл'
		WHERE id = $1`, versionID); err != nil {
		t.Fatalf("подготовка версии: %v", err)
	}

	repo := newTestJobs()
	lease := convertLease(t, ctx, repo)
	if err := finishConvert(t, ctx, repo, lease); err != nil {
		t.Fatalf("FinishConvert: %v", err)
	}

	var (
		status       string
		failedStage  *string
		errorCode    *string
		errorMessage *string
	)
	if err := testDB.QueryRow(ctx, `
		SELECT status, failed_stage, error_code, error_message
		FROM document_versions WHERE id = $1`, versionID,
	).Scan(&status, &failedStage, &errorCode, &errorMessage); err != nil {
		t.Fatalf("чтение версии: %v", err)
	}
	if status != domain.VersionStatusParsed {
		t.Errorf("статус %q вместо parsed", status)
	}
	if failedStage != nil || errorCode != nil || errorMessage != nil {
		t.Errorf("ошибка прежней попытки не очищена: stage=%v code=%v text=%v",
			failedStage, errorCode, errorMessage)
	}
}

// TestConvertThenIndexRunsEndToEnd — цепочка целиком: convert кладёт
// parsed_key, index читает разобранный документ по этому ключу и доводит
// версию до ready. Это и есть путь «загрузка → поиск», ради которого стадия
// convert существует.
func TestConvertThenIndexRunsEndToEnd(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{
		embeddingProfile: ptrString(profileID),
	})

	store := newMemoryStore()
	originalKey := versionID.String() + "/original.bin"
	if err := store.Put(ctx, originalKey, []byte("# Раздел\n\nтекст документа\n")); err != nil {
		t.Fatalf("Put оригинала: %v", err)
	}

	repo := newTestJobs()
	docs := newTestDocuments()

	// Стадия convert.
	version, err := docs.LoadVersion(ctx, testDB, store, tenantID.String(), versionID.String())
	if err != nil {
		t.Fatalf("LoadVersion: %v", err)
	}
	if len(version.Content) == 0 {
		t.Fatal("оригинал версии пуст")
	}
	putParsed(t, store, convertParsedKey, domain.ParsedDocument{
		SchemaVersion: "1.0",
		TenantID:      tenantID, KBID: kbID, DocumentID: docID, VersionID: versionID,
		Title: "Инструкция", Filename: "guide.pdf",
		Blocks: []domain.Block{{BlockID: uuid.New(), Position: 0, Kind: "paragraph",
			Text: "текст документа"}},
	})

	convertJob := convertLease(t, ctx, repo)
	if err := finishConvert(t, ctx, repo, convertJob); err != nil {
		t.Fatalf("FinishConvert: %v", err)
	}

	// Стадия index читает разобранный документ по записанному ключу.
	indexLease, err := repo.Claim(ctx, testDB, "worker-2", []string{domain.StageIndex})
	if err != nil || indexLease == nil {
		t.Fatalf("Claim стадии index: %v (lease=%v)", err, indexLease)
	}
	parsed, err := docs.LoadParsed(ctx, testDB, store, tenantID.String(), versionID.String())
	if err != nil {
		t.Fatalf("LoadParsed после convert: %v", err)
	}
	if len(parsed.Blocks) != 1 {
		t.Errorf("прочитано %d блоков вместо 1", len(parsed.Blocks))
	}

	previous, err := docs.ActivateVersion(ctx, testDB, domain.ActivateParams{
		TenantID:           tenantID.String(),
		DocumentID:         docID.String(),
		VersionID:          versionID.String(),
		EmbeddingProfileID: profileID,
		ChunkCount:         len(parsed.Blocks),
		LeaseToken:         indexLease.LeaseToken,
	})
	if err != nil {
		t.Fatalf("ActivateVersion: %v", err)
	}
	if previous != "" {
		t.Errorf("прежняя активная версия %q у первой версии документа", previous)
	}
	if err := repo.FinishIndex(ctx, testDB, indexLease.JobID.String(), indexLease.LeaseToken,
		domain.NewIndexingResult(
			docID.String(), versionID.String(), profileID, len(parsed.Blocks))); err != nil {
		t.Fatalf("FinishIndex: %v", err)
	}

	var versionStatus string
	if err := testDB.QueryRow(ctx,
		"SELECT status FROM document_versions WHERE id = $1", versionID).Scan(&versionStatus); err != nil {
		t.Fatalf("чтение версии: %v", err)
	}
	if versionStatus != domain.VersionStatusReady {
		t.Errorf("статус версии %q вместо ready", versionStatus)
	}
	if record := getJob(t, ctx, repo); record.Status != domain.JobStatusSucceeded {
		t.Errorf("статус задания %q вместо succeeded", record.Status)
	}
}
