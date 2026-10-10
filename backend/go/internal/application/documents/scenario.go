// Сценарии документов (ТЗ 5.4, 5.5).
//
// Порядок операций в Upload повторяет Python _accept: проверки -> файл ->
// транзакция. Обратный порядок не годится: запись оригинала до проверок
// оставила бы в хранилище файлы от отклонённых запросов, а транзакция до
// записи файла оставила бы документ без оригинала.

package documents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kbsearch/backend-go/internal/application/ports"
	"github.com/kbsearch/backend-go/internal/domain"
)

// rfc3339 — формат дат в ответе сценария.
const rfc3339 = time.RFC3339

// Границы пагинации документов. Те же значения, что у списка баз знаний
// (ТЗ 2.3) и в контракте.
const (
	defaultLimit = 20
	maxLimit     = 100
)

// statusAccepted — код ответа, сохраняемый вместе с идемпотентным ответом.
const statusAccepted = http.StatusAccepted

// kbRepo — то, что сценарий документов знает о базах знаний.
//
// Require проверяет принадлежность tenant'у, SetDocumentACL пишет ACL
// документа, ACL нужен для правила доступа. Последнего метода нет в порте
// ports.KBRepository по той же причине, что и в application/kb: структура
// domain.KB поля ACL не несёт. Реализуется существующим
// postgres.KBRepository.
type kbRepo interface {
	ports.KBRepository
	ACL(ctx context.Context, q ports.Queryer, kbID string) (domain.ACL, error)
}

// keyBuilder строит ключ файла оригинала в хранилище.
//
// Передаётся снаружи, а не собирается здесь: правило именования живёт в
// infrastructure/storage, и второй его экземпляр в слое приложения
// разошёлся бы с первым при первой же правке формата ключа. Приложение не
// импортирует инфраструктуру, поэтому единственный способ не дублировать —
// получить функцию при сборке.
type keyBuilder func(tenantID, versionID string) string

// Service — сценарии документов.
type Service struct {
	repo        Repository
	kbs         kbRepo
	jobs        ports.JobRepository
	idem        IdempotencyStore
	storage     ports.FileStorage
	auth        ports.Authorizer
	q           ports.Queryer
	transactor  ports.Transactor
	clock       ports.Clock
	originalKey keyBuilder
}

// New создаёт сервис документов.
func New(
	repo Repository,
	kbs kbRepo,
	jobs ports.JobRepository,
	idem IdempotencyStore,
	storage ports.FileStorage,
	auth ports.Authorizer,
	q ports.Queryer,
	transactor ports.Transactor,
	clock ports.Clock,
	originalKey keyBuilder,
) *Service {
	return &Service{
		repo: repo, kbs: kbs, jobs: jobs, idem: idem, storage: storage, auth: auth,
		q: q, transactor: transactor, clock: clock, originalKey: originalKey,
	}
}

// UploadInput — загрузка документа.
type UploadInput struct {
	KBID      uuid.UUID
	Filename  string
	Title     string
	Content   []byte
	SourceURL string
	Metadata  map[string]any
	// IdempotencyKey заголовка. Пустой ключ означает «без идемпотентности»:
	// контракт объявляет заголовок необязательным и не содержит ответа 400,
	// поэтому требовать его здесь означало бы ответ кодом, которого в
	// контракте нет.
	IdempotencyKey string
}

// ListInput — постраничный список документов базы.
type ListInput struct {
	KBID   uuid.UUID
	Limit  int
	Cursor string
}

// ListOutput — страница документов.
type ListOutput struct {
	Items []View
	// NextCursor пустая строка означает «дальше нечего» — так же, как у
	// KBRepository.List.
	NextCursor string
}

// ReplaceContentInput — замена содержимого существующего документа.
type ReplaceContentInput struct {
	DocumentID     uuid.UUID
	Filename       string
	Title          string
	Content        []byte
	SourceURL      string
	Metadata       map[string]any
	IdempotencyKey string
}

// View — документ в ответе сценария.
type View struct {
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
	ErrorCode string
	ErrorText string
}

// Upload принимает документ и ставит его в очередь на обработку.
//
// Идемпотентность (ТЗ 5.4): повтор с тем же ключом и тем же содержимым
// возвращает исходный ответ и НЕ создаёт вторую версию. Тот же ключ с другим
// содержимым отвергает хранилище ключей, сравнив отпечаток тела: сценарий
// отпечаток только передаёт.
func (s *Service) Upload(ctx context.Context, p domain.Principal, in UploadInput) (CreateResult, error) {
	if err := ctx.Err(); err != nil {
		return CreateResult{}, err
	}
	if err := s.validateFile(ctx, in.Filename, in.Content); err != nil {
		return CreateResult{}, err
	}

	key := s.idempotencyKey(p, in.IdempotencyKey, "POST", uploadPath(in.KBID), in.Content, in.Metadata)
	return s.accept(ctx, p, in.KBID, nil, acceptRequest{
		filename: in.Filename, title: in.Title, sourceURL: in.SourceURL,
		metadata: in.Metadata, content: in.Content,
	}, key)
}

// ReplaceContent создаёт новую версию документа.
//
// Прежняя версия перестаёт быть последней и уйдёт из активных после
// индексации новой: active_version_id переключает ActivateVersion в
// воркере. Сбрасывать его здесь нельзя — на время индексации документ
// исчез бы из поиска, хотя новая версия ещё не проиндексирована и искать
// по ней нечего.
func (s *Service) ReplaceContent(
	ctx context.Context, p domain.Principal, in ReplaceContentInput,
) (uuid.UUID, error) {
	if err := ctx.Err(); err != nil {
		return uuid.Nil, err
	}
	if err := s.validateFile(ctx, in.Filename, in.Content); err != nil {
		return uuid.Nil, err
	}

	record, err := s.repo.Get(ctx, s.q, p, in.DocumentID.String())
	if err != nil {
		return uuid.Nil, err
	}
	kbID, err := uuid.Parse(record.KBID)
	if err != nil {
		return uuid.Nil, internal("У документа некорректный идентификатор базы знаний", err)
	}

	key := s.idempotencyKey(p, in.IdempotencyKey, "PUT", replacePath(in.DocumentID), in.Content, in.Metadata)
	created, err := s.accept(ctx, p, kbID, &in.DocumentID, acceptRequest{
		filename: in.Filename, title: in.Title, sourceURL: in.SourceURL,
		metadata: in.Metadata, content: in.Content,
	}, key)
	if err != nil {
		return uuid.Nil, err
	}
	return created.JobID, nil
}

// acceptRequest — общая часть загрузки и замены содержимого.
//
// Вынесено отдельно, потому что обе операции обязаны выполнять один и тот же
// список шагов в одном и том же порядке; различие только в том, создаётся
// документ или версия существующего.
type acceptRequest struct {
	filename  string
	title     string
	sourceURL string
	metadata  map[string]any
	content   []byte
}

// accept — приём файла: проверки, запись оригинала, транзакция.
//
// Откат файла при неудачной транзакции обязателен: оригинал без версии в
// базе — мусор на диске, который ни один cleanup не найдёт, потому что
// хранилище чистится по ключам версий из таблицы.
func (s *Service) accept(
	ctx context.Context, p domain.Principal, kbID uuid.UUID, documentID *uuid.UUID,
	req acceptRequest, key IdempotencyKey,
) (CreateResult, error) {
	if key.Key != "" {
		saved, err := s.idem.Lookup(ctx, s.q, key)
		if err != nil {
			return CreateResult{}, err
		}
		if saved != nil {
			replayed, err := idsFromBody(saved.Body)
			if err != nil {
				// Сохранённый ответ написан сервисом, поэтому нечитаемые
				// идентификаторы — дефект кода, а не пользовательский ввод.
				return CreateResult{}, internal(
					"Сохранённый ответ идемпотентности не читается", err)
			}
			return replayed, nil
		}
	}

	if err := s.requireAdmin(ctx, p, kbID); err != nil {
		return CreateResult{}, err
	}

	versionID := uuid.New()
	originalKey := s.originalKey(p.TenantID.String(), versionID.String())
	if err := s.storage.Put(ctx, originalKey, req.content); err != nil {
		return CreateResult{}, err
	}

	created, err := s.createVersion(ctx, p, CreateParams{
		KBID:          kbID,
		DocumentID:    derefUUID(documentID),
		VersionID:     versionID,
		Filename:      req.filename,
		Title:         req.title,
		SourceURL:     req.sourceURL,
		Metadata:      req.metadata,
		ContentSHA256: contentDigest(req.content),
		OriginalKey:   originalKey,
	})
	if err != nil {
		// Ошибка удаления файла подавляется: оригинал без версии в базе —
		// мусор на диске, и он опаснее сообщения об отказе. Ключ уборки
		// строится из версий таблицы, поэтому потерянный файл ею не
		// находится; чистка остаётся на дисковом обходе.
		_ = s.storage.Delete(ctx, originalKey)
		return CreateResult{}, err
	}

	if key.Key != "" {
		body := map[string]any{
			"document_id": created.DocumentID.String(),
			"version_id":  created.VersionID.String(),
			"job_id":      created.JobID.String(),
			"status":      domain.JobStatusQueued,
		}
		if err := s.idem.Store(ctx, s.q, key, statusAccepted, body); err != nil {
			// Загрузка состоялась: ошибка сохранения ключа делает повтор
			// неидемпотентным, но отдать её значило бы сообщить клиенту о
			// провале приёма файла, которого не было. Поэтому она
			// возвращается, а приём уже завершён.
			return CreateResult{}, err
		}
	}
	return created, nil
}

// List возвращает страницу доступных документов базы.
func (s *Service) List(ctx context.Context, p domain.Principal, in ListInput) (ListOutput, error) {
	if err := ctx.Err(); err != nil {
		return ListOutput{}, err
	}
	if in.KBID == uuid.Nil {
		return ListOutput{}, domain.InvalidMetadata("Идентификатор базы знаний обязателен")
	}

	records, next, err := s.repo.List(ctx, s.q, p, in.KBID.String(), clampLimit(in.Limit), in.Cursor)
	if err != nil {
		return ListOutput{}, err
	}

	// Пустой срез, а не nil: в JSON поле items обязано быть [], иначе клиент
	// получил бы null вместо массива.
	items := make([]View, 0, len(records))
	for _, record := range records {
		view, err := s.view(ctx, record)
		if err != nil {
			return ListOutput{}, err
		}
		items = append(items, view)
	}
	return ListOutput{Items: items, NextCursor: next}, nil
}

// Get возвращает документ.
//
// Права проверяет адаптер в SQL: чужой, удалённый и недоступный документ
// дают одинаковый NOT_FOUND, иначе перебором по коду ответа вычислялось бы
// существование чужих документов (ТЗ 5.4).
func (s *Service) Get(ctx context.Context, p domain.Principal, id uuid.UUID) (View, error) {
	if err := ctx.Err(); err != nil {
		return View{}, err
	}
	if id == uuid.Nil {
		return View{}, domain.InvalidMetadata("Идентификатор документа обязателен")
	}

	record, err := s.repo.Get(ctx, s.q, p, id.String())
	if err != nil {
		return View{}, err
	}
	return s.view(ctx, record)
}

// Delete помечает документ удалённым.
//
// Повтор идемпотентен: второй DELETE не создаёт вторую задачу удаления, а
// возвращает прежнее её задание адаптеру, поэтому клиент, отправивший
// запрос дважды из-за оборванного ответа, не получит нового job_id.
//
// Задача уборки ставится для версии, по которой документ участвует в поиске:
// у активной в индексе лежат чанки, у последней при отсутствии активной —
// на диске остались оригинал и parsed.json. Стадия delete снимет документ с
// индексации, а EnqueueCleanup освободит место и файлы даже если задание
// удаления исчерпает попытки (ТЗ 4.2 — уборка повторяется).
func (s *Service) Delete(ctx context.Context, p domain.Principal, id uuid.UUID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if id == uuid.Nil {
		return domain.InvalidMetadata("Идентификатор документа обязателен")
	}

	record, err := s.repo.Get(ctx, s.q, p, id.String())
	if err != nil {
		return err
	}
	if err := s.auth.RequireAdmin(p); err != nil {
		return err
	}

	return s.transactor.WithTx(ctx, func(tx ports.Queryer) error {
		if _, err := s.repo.MarkDeleted(ctx, tx, p, id.String()); err != nil {
			return err
		}
		active, ok := record.SearchableVersion()
		if !ok {
			return nil
		}
		_, err := s.jobs.EnqueueCleanup(ctx, tx, p.TenantID.String(), id.String(), active.ID)
		return err
	})
}

// Reindex создаёт новую версию из сохранённого оригинала (ТЗ 4.1).
//
// Отдельная версия, а не перезапуск задания: чанки проиндексированной версии
// уже лежат в индексе, и повтор по той же версии не изменил бы ни текста, ни
// векторов. Профиль остаётся прежним — ActivateVersion отклонит смену, и
// это правильно (ТЗ 1.2).
func (s *Service) Reindex(ctx context.Context, p domain.Principal, id uuid.UUID) (uuid.UUID, error) {
	if err := ctx.Err(); err != nil {
		return uuid.Nil, err
	}
	if id == uuid.Nil {
		return uuid.Nil, domain.InvalidMetadata("Идентификатор документа обязателен")
	}

	record, err := s.repo.Get(ctx, s.q, p, id.String())
	if err != nil {
		return uuid.Nil, err
	}
	// Источник — последняя версия, а не активная: переиндексация чинит
	// последнюю загруженную, и брать старую было бы молчаливым откатом
	// содержимого назад.
	if record.Latest == nil {
		return uuid.Nil, domain.NotFound("У документа нет версии для переиндексации")
	}
	source := *record.Latest
	if source.OriginalKey == "" {
		return uuid.Nil, domain.NotFound("Оригинал версии не сохранён, переиндексация невозможна")
	}

	content, err := s.storage.Read(ctx, source.OriginalKey)
	if err != nil {
		return uuid.Nil, err
	}
	kbID, err := uuid.Parse(record.KBID)
	if err != nil {
		return uuid.Nil, internal("У документа некорректный идентификатор базы знаний", err)
	}

	// Повторный разбор формата нужен: файл в хранилище мог быть подменён
	// сторонним процессом, и тогда версия ушла бы в очередь с заведомо
	// битым содержимым.
	if err := s.validateFile(ctx, source.Filename, content); err != nil {
		return uuid.Nil, err
	}

	created, err := s.createVersion(ctx, p, CreateParams{
		KBID:          kbID,
		DocumentID:    id,
		VersionID:     uuid.New(),
		Filename:      source.Filename,
		Title:         source.Title,
		SourceURL:     source.SourceURL,
		Metadata:      source.Metadata,
		ContentSHA256: source.ContentSHA256,
		// Ключ остаётся прежним: содержимое то же, а новый ключ оставил бы
		// старый оригинал в хранилище навсегда — его уже никто не уберёт.
		OriginalKey: source.OriginalKey,
	})
	if err != nil {
		return uuid.Nil, err
	}
	return created.JobID, nil
}

// Retry возобновляет обработку неуспешной последней версии (ТЗ 4.1).
func (s *Service) Retry(ctx context.Context, p domain.Principal, id uuid.UUID) (uuid.UUID, error) {
	if err := ctx.Err(); err != nil {
		return uuid.Nil, err
	}
	if id == uuid.Nil {
		return uuid.Nil, domain.InvalidMetadata("Идентификатор документа обязателен")
	}
	if err := s.auth.RequireAdmin(p); err != nil {
		return uuid.Nil, err
	}

	var jobID uuid.UUID
	err := s.transactor.WithTx(ctx, func(tx ports.Queryer) error {
		// Get внутри транзакции, а не до неё: версия могла уйти в failed
		// между проверкой и записью, и тогда повтор был бы поставлен для
		// версии, которую уже обрабатывают.
		if _, err := s.repo.Get(ctx, tx, p, id.String()); err != nil {
			return err
		}
		res, err := s.repo.Retry(ctx, tx, p, id.String())
		if err != nil {
			return err
		}
		jobID = res.JobID
		return nil
	})
	if err != nil {
		return uuid.Nil, err
	}
	return jobID, nil
}

// SetACL задаёт собственный ACL документа.
//
// nil означает наследование ACL базы, пустой список — доступ всем
// пользователям tenant'а (domain.AllowedList). Подменять одно другим нельзя:
// NULL в documents.acl_override означает «действует ACL базы», а пустой
// объект — «закрыто для всех, кроме администратора».
func (s *Service) SetACL(
	ctx context.Context, p domain.Principal, id uuid.UUID, allowed domain.AllowedList,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if id == uuid.Nil {
		return domain.InvalidMetadata("Идентификатор документа обязателен")
	}

	// Существование и права проверяются до записи: SetDocumentACL в порте
	// ищет документ по одному идентификатору и не знает tenant'а.
	if _, err := s.repo.Get(ctx, s.q, p, id.String()); err != nil {
		return err
	}
	if err := s.auth.RequireAdmin(p); err != nil {
		return err
	}

	override := domain.ACL{Users: allowed.Users, Groups: allowed.Groups}
	return s.transactor.WithTx(ctx, func(tx ports.Queryer) error {
		return s.kbs.SetDocumentACL(ctx, tx, id.String(), &override)
	})
}

// Source отдаёт байты оригинала и имя файла для Content-Disposition.
//
// Второе значение — имя файла, а не content-type: транспорт подставляет его
// в заголовок вложения и сам выставляет Content-Type. Содержимое отдаётся
// той версией, по которой документ участвует в поиске: активной, а при её
// отсутствии последней.
func (s *Service) Source(ctx context.Context, p domain.Principal, id uuid.UUID) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if id == uuid.Nil {
		return nil, "", domain.InvalidMetadata("Идентификатор документа обязателен")
	}

	record, err := s.repo.Get(ctx, s.q, p, id.String())
	if err != nil {
		return nil, "", err
	}
	version, ok := record.SearchableVersion()
	if !ok {
		return nil, "", domain.NotFound("У документа нет доступной версии")
	}
	if version.OriginalKey == "" {
		return nil, "", domain.NotFound("Оригинал версии не сохранён")
	}

	content, err := s.storage.Read(ctx, version.OriginalKey)
	if err != nil {
		return nil, "", err
	}
	return content, version.Filename, nil
}

// createVersion выполняет создание версии в транзакции.
func (s *Service) createVersion(
	ctx context.Context, p domain.Principal, in CreateParams,
) (CreateResult, error) {
	var created CreateResult
	err := s.transactor.WithTx(ctx, func(tx ports.Queryer) error {
		result, err := s.repo.Create(ctx, tx, p, in)
		if err != nil {
			return err
		}
		created = result
		return nil
	})
	if err != nil {
		return CreateResult{}, err
	}
	return created, nil
}

// requireAdmin проверяет права на изменение содержимого базы знаний.
//
// Существование базы проверяется адаптером и даёт NOT_FOUND для чужого
// tenant'а — по коду ответа нельзя перебором узнать о базах другого
// tenant'а.
func (s *Service) requireAdmin(ctx context.Context, p domain.Principal, kbID uuid.UUID) error {
	if _, err := s.kbs.Require(ctx, s.q, p, kbID.String()); err != nil {
		return err
	}
	return s.auth.RequireAdmin(p)
}

// view собирает документ для ответа.
//
// Состояние и чанки берутся у последней версии: она отражает, что сейчас
// происходит с документом. Активная версия в этот момент может быть
// предыдущей, и показывать её как текущую значило бы показывать устаревшее
// содержимое.
func (s *Service) view(ctx context.Context, record Record) (View, error) {
	docID, err := uuid.Parse(record.DocumentID)
	if err != nil {
		return View{}, internal("У документа некорректный идентификатор", err)
	}
	kbID, err := uuid.Parse(record.KBID)
	if err != nil {
		return View{}, internal("У документа некорректный идентификатор базы знаний", err)
	}

	out := View{
		ID:        docID,
		KBID:      kbID,
		CreatedAt: record.CreatedAt.UTC().Format(rfc3339),
		UpdatedAt: s.clock.Now().UTC().Format(rfc3339),
		ACL:       domain.ACL{},
	}
	if record.ACLOverride != nil {
		out.ACL = *record.ACLOverride
	}

	latest := record.Latest
	if latest == nil {
		return out, nil
	}
	out.Filename = latest.Filename
	out.Title = latest.Title
	out.State = latest.Status
	out.Stage = latest.FailedStage
	out.Chunks = latest.ChunkCount
	out.SourceURL = latest.SourceURL
	out.ErrorCode = latest.ErrorCode
	out.ErrorText = latest.ErrorText
	out.UpdatedAt = latest.CreatedAt.UTC().Format(rfc3339)

	versionID, err := uuid.Parse(latest.ID)
	if err != nil {
		return View{}, internal("У версии документа некорректный идентификатор", err)
	}
	out.VersionID = versionID
	return out, nil
}

// validateFile проверяет формат и размер до постановки в очередь (ТЗ 5.5).
//
// Отдельный вызов, а не Convert: конвертировать файл здесь не нужно, worker
// сделает это по содержимому того же файла, а проверка обязана отклонить
// негодный файл ДО того, как он займёт версию и задание.
func (s *Service) validateFile(ctx context.Context, filename string, content []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(content) > MaxFileBytes {
		return domain.FileTooLarge("Размер файла превышает 20 MiB")
	}
	if _, err := ValidateFormat(ctx, filename, content); err != nil {
		return err
	}
	return nil
}

// idempotencyKey собирает ключ идемпотентности.
//
// Пустой заголовок даёт пустой ключ, и вызывающий пропускает проверку:
// требовать заголовок нельзя, в контракте нет ответа для его отсутствия.
func (s *Service) idempotencyKey(
	p domain.Principal, raw, method, path string, content []byte, metadata map[string]any,
) IdempotencyKey {
	if strings.TrimSpace(raw) == "" {
		return IdempotencyKey{}
	}
	return IdempotencyKey{
		TenantID:    p.TenantID,
		UserID:      p.UserID,
		Method:      method,
		Path:        path,
		Key:         raw,
		RequestHash: requestHash(content, metadata),
	}
}

// requestHash — отпечаток тела запроса для ключа идемпотентности (ТЗ 2.4).
//
// SHA-256 байтов файла плюс канонический JSON метаданных. Байты файла идут
// первыми, чтобы разные содержимое с одинаковыми метаданными давали разные
// хеши. Метаданные сериализуются сортированными ключами: порядок полей в
// JSON объекта не должен менять идентичность запроса.
func requestHash(content []byte, metadata map[string]any) string {
	digest := sha256.New()
	sum := sha256.Sum256(content)
	digest.Write(sum[:])

	if metadata == nil {
		metadata = map[string]any{}
	}
	if encoded, err := json.Marshal(metadata); err == nil {
		digest.Write(encoded)
	} else {
		// Метаданные пришли из JSON клиента, поэтому ошибка сериализации
		// означала бы неподдерживаемый тип значения. Хеш всё равно должен
		// быть определённым: иначе два одинаковых запроса получили бы
		// разные хеши и повтор перестал бы быть идемпотентным.
		digest.Write([]byte("{}"))
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// idsFromBody достаёт идентификаторы из сохранённого тела ответа.
func idsFromBody(body map[string]any) (CreateResult, error) {
	var out CreateResult
	for _, field := range []struct {
		name string
		dst  *uuid.UUID
	}{
		{"document_id", &out.DocumentID},
		{"version_id", &out.VersionID},
		{"job_id", &out.JobID},
	} {
		parsed, err := uuidFromBody(body, field.name)
		if err != nil {
			return CreateResult{}, err
		}
		*field.dst = parsed
	}
	return out, nil
}

func uuidFromBody(body map[string]any, name string) (uuid.UUID, error) {
	raw, ok := body[name].(string)
	if !ok {
		return uuid.Nil, domain.Internal("Сохранённый ответ идемпотентности не содержит " + name)
	}
	parsed, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, domain.Internal("Сохранённый ответ идемпотентности содержит некорректный " + name).WithCause(err)
	}
	return parsed, nil
}

// uploadPath и replacePath — логические пути для ключа идемпотентности.
//
// Не берутся из HTTP-запроса: тот же вызов из другой версии контракта или с
// другим префиксом должен попасть в ту же запись ключа.
func uploadPath(kbID uuid.UUID) string { return "POST /kbs/" + kbID.String() + "/documents" }

func replacePath(docID uuid.UUID) string { return "PUT /documents/" + docID.String() + "/content" }

// contentDigest — hex-отпечаток байтов файла для колонки
// document_versions.content_sha256.
//
// Отдельное имя, а не переиспользование sha256Hex из blocks.go: та
// принимает строку, а отпечаток нужен от байтов оригинала.
func contentDigest(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func derefUUID(value *uuid.UUID) uuid.UUID {
	if value == nil {
		return uuid.Nil
	}
	return *value
}

// clampLimit держит размер страницы в границах контракта (ТЗ 2.3).
func clampLimit(limit int) int {
	if limit <= 0 {
		return defaultLimit
	}
	if limit > maxLimit {
		return maxLimit
	}
	return limit
}

// internal — обёртка неожиданной ошибки.
//
// Конструктора INTERNAL_ERROR в реестре домена нет, а заводить его здесь
// нельзя: реестр принадлежит домену. Наружу уходит только текст, причина
// остаётся в логах через Cause.
func internal(message string, cause error) error {
	return domain.Internal(message).WithCause(cause)
}

// OriginalKey строит путь оригинала версии в хранилище файлов.
//
// Экспортирован, потому что ключ задаёт вызывающий (сборка зависимостей),
// а не сам сценарий: сценарий лишь передаёт его дальше. Формат
// «tenant/версия/имя» различает версии разных tenant при общей файловой
// системе и сохраняет расширение — по нему определяется формат разбора.
func OriginalKey(tenantID, versionID string) string {
	return filepath.Join(tenantID, versionID, "original")
}
