package rebuild

// СВЁРТКА ЛЕНТЫ В ПЛАН ПЕРЕСБОРКИ — §1 (принцип 1: «поисковый индекс …
// производные представления, которые можно пересобрать из lake за часы») и §4
// («любой индекс можно удалить и воссоздать из lake без обращения к
// источникам»).
//
// ГЛАВНОЕ ПРО ЭТОТ ФАЙЛ: СВЁРТКА ОДНОЗНАЧНА И НЕ ЗАВИСИТ ОТ ПОРЯДКА ОБХОДА
// КАРТ. Порядок чтения задан портом EventStore — (occurred_at, event_id), см.
// lake.go, — и этот файл его НЕ переопределяет: правило «последнее событие
// побеждает» осмысленно только на упорядоченном потоке. Итоговый список
// документов сортируется по doc_id перед возвратом, потому что накопление идёт
// в карте, а обход карты в Go НЕ упорядочен: без сортировки два одинаковых
// прогона дали бы разный порядок записи в индекс и разные батчи.
//
// ПРАВИЛО СВЁРТКИ ОДНО И СТРОГОЕ: ПОСЛЕДНЕЕ СОБЫТИЕ ДОКУМЕНТА РЕШАЕТ ВСЁ.
//
//	стадия delete → документ удалён (надгробие, §4 «удаление — через
//	                событие-надгробие»);
//	стадия index  → документ существует в ТОЙ редакции и ТОМ содержании,
//	                которые несёт это событие;
//	остальные      → состояние не меняют.
//
// ПОЧЕМУ ИМЕННО СТАДИЯ INDEX, А НЕ СЛИЯНИЕ ПО СТАДИЯМ. Первая версия этой
// свёртки складывала поля документов из событий enrich и index («последнее
// непустое побеждает»). Она отвергнута, и вот почему:
//
//	 1. ПРАВА. Контейнер документа (space_id) и список исключений (acl) могут
//	    быть СБРОШЕНЫ администратором, и следующее событие обязано нести
//	    пустые права. Правило «последнее НЕПУСТОЕ побеждает» оставило бы прежний
//	    контейнер — то есть документ остался бы видимым всем, кому доступен тот
//	    контейнер, из которого его уже убрали. Это утечка §6, а утечка — худший
//	    из возможных исходов. Обратная ошибка (сбросить права, пришедшие пустыми
//	    по недосмотру) стоила бы невидимости документа, то есть fail-closed.
//
//	 2. ВЛАДЕНИЕ ПОЛЯМИ. У каждой стадии свой набор полей, и объединение «по
//	    последнему событию каждой стадии» означало бы семь правил слияния и семь
//	    мест, где их можно перепутать. Объединения нет вовсе.
//
//	 3. УТВЕРЖДЕНИЕ О САМОМ ИНДЕКСЕ. Стадия index — это и есть утверждение
//	    «эта версия документа в индексе, вот его профиль и вот число чанков».
//	    Строить индекс по стадии, которая индекс ещё не трогала, значило бы
//	    пересобирать выдачу из предположения.
//
// ПОСЛЕДСТВИЕ, КОТОРОЕ НАДО ЗНАТЬ: документ, не дошедший до стадии index, в
// пересборку НЕ попадает. Это не потеря — наоборот, из индекса нечего
// восстанавливать: документа в нём не было, и ставить его туда на основании
// промежуточных стадий означало бы выдумать состояние, которого не было.
//
// ПОРЯДОК «УДАЛЕНИЕ ПОСЛЕ ИНДЕКСАЦИИ» СЛЕДУЕТ ИЗ ПРАВИЛА ПОСЛЕДНЕГО СОБЫТИЯ,
// а не из отдельного флага. Документ можно загрузить заново после удаления
// (delete.go, довод 2 «повторное удаление после новой загрузки»), и тогда
// index-событие новой редакции приходит позже надгробия — документ
// возвращается в выдачу, что и правильно: он снова есть в источнике. Второе
// удаление снова приходит последним и снова побеждает.

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	domain "github.com/kbsearch/kb/internal/envelope"
)

// ChunkSchemaVersion — версия тела документа индекса.
//
// Та же строка, что у стадии индексации (schemaVersion в
// internal/jobs/indexing). Продублирована сознательно: пересборка — отдельный
// потребитель lake, а §8 прямо требует, чтобы новое добавлялось новым
// потребителем, а не правкой чужого кода. Поле обязано быть заполнено всегда:
// пустой schema_version в _source означает документ неизвестной схемы, и такой
// индекс нельзя ни прочитать, ни пересобрать в следующий раз.
const ChunkSchemaVersion = "1.0"

// Document — документ, восстановленный из lake и готовый к переиндексации.
//
// Идентификаторы здесь уже разобраны в uuid.UUID: ParsedDocument требует их
// именно такими, и разбор строки по дороге к индексу означал бы, что неверный
// doc_id обнаружится на записи, а не на свёртке.
type Document struct {
	DocumentID  uuid.UUID
	TenantID    uuid.UUID
	KBID        uuid.UUID
	VersionID   uuid.UUID
	ContentHash string
	Title       string
	// Rights едет вместе с документом (§4, стадия index: «Upsert в
	// Elasticsearch вместе с ACL»). Пустые права законны и означают ровно одно:
	// документ не виден никому.
	Rights domain.DocumentRights
	// Blocks — разобранный документ. Нарезка на чанки делается из них же, и
	// потому block_id в локаторе чанка совпадает с block_id документа.
	Blocks []domain.Block
	// ChunkCount — число чанков, объявленное стадией index. Это ДИАГНОСТИКА
	// (сколько чанков насчитала прошлая индексация), а не ограничение: после
	// смены настроек нарезки пересборка даст другое число, и это правильный
	// результат, а не расхождение.
	ChunkCount int
}

// ID returns the document id as the index stores it.
func (d Document) ID() string { return d.DocumentID.String() }

// Parsed собирает разобранный документ для нарезки на чанки.
//
// Metadata намеренно пустой: права приходят полем Rights (см. Document), и
// повторная сборка их из метаданных означала бы второй, несовпадающий с
// конвертом путь к тем же правам.
func (d Document) Parsed() domain.ParsedDocument {
	return domain.ParsedDocument{
		SchemaVersion: ChunkSchemaVersion,
		TenantID:      d.TenantID,
		KBID:          d.KBID,
		DocumentID:    d.DocumentID,
		VersionID:     d.VersionID,
		Title:         d.Title,
		ContentSHA256: d.ContentHash,
		Blocks:        slices.Clone(d.Blocks),
	}
}

// Plan — результат свёртки: что переиндексировать и что убрать.
//
// Оба среза отсортированы. Карта накопления обходится в случайном порядке, и
// без сортировки отчёт о пересборке, порядок обращений к индексу и содержимое
// батчей менялись бы от прогона к прогону при одном и том же lake.
type Plan struct {
	// Documents — существующие документы в порядке doc_id.
	Documents []Document
	// Deleted — doc_id, удалённые надгробием и обязанные отсутствовать в индексе.
	Deleted []string
}

// docState — накопленное состояние одного документа на момент свёртки.
type docState struct {
	docID    string
	tenantID uuid.UUID
	deleted  bool
	indexed  bool
	// indexEvent — последнее событие стадии index, принятое как снимок.
	// Хранится целиком, а не только разобранное тело: при ошибке пересборки
	// нужны event_id и occurred_at из шапки, а разбирать шапку повторно ради
	// сообщения об ошибке — значит завести ещё одно место, где эти поля живут.
	indexEvent domain.Event
	snapshot   lakePayload
}

// Fold сворачивает ленту lake в план пересборки индекса.
//
// Порядок events — тот, который отдаёт порт EventStore: (occurred_at,
// event_id). Функция его не сортирует заново: переупорядочивание сделало бы
// правило «последнее событие побеждает» зависимым от того, кто именно принёс
// поток, а §9 требует, чтобы пересборка была воспроизводима при любом
// параллелизме записи. Дубли событий (повтор стадии, §4) безвредны: они несут
// те же поля и тот же event_id.
//
// Ошибка возвращается на ПЕРВОМ несогласованном событии и останавливает
// пересборку: наполовину восстановленный индекс хуже отсутствующего, потому
// что выдача после него выглядит правдоподобно.
func Fold(events []domain.Event) (Plan, error) {
	states := make(map[string]*docState, len(events))

	for _, event := range events {
		if err := event.Validate(); err != nil {
			return Plan{}, domain.InvalidMetadata(
				"Событие lake не проходит проверку: пересборка индекса остановлена").
				WithCause(err)
		}
		state, ok := states[event.DocID]
		if !ok {
			state = &docState{docID: event.DocID, tenantID: event.TenantID}
			states[event.DocID] = state
		}
		if state.tenantID != event.TenantID {
			// doc_id хэширует tenant (§4), но загруженные и рукописные
			// идентификаторы исторически не хэшировались. Свёртка обязана
			// отвергнуть чужое событие, а не склеить документы двух
			// организаций в один.
			return Plan{}, domain.Conflict(
				"Один doc_id принадлежит двум тенантам: пересборка склеила бы документы разных организаций").
				WithDetails(map[string]any{
					"doc_id":   event.DocID,
					"first":    state.tenantID.String(),
					"second":   event.TenantID.String(),
					"event_id": event.EventID.String(),
				})
		}

		payload, err := decodePayload(event)
		if err != nil {
			return Plan{}, err
		}

		switch event.Stage {
		case domain.DeleteStage:
			// Надгробие. Тело у него своё (delete.go), и в снимок документа
			// оно НЕ идёт: там нет ни блоков, ни контейнера, и подставив их
			// по памяти, пересборка восстановила бы удалённый документ.
			state.deleted = true
		case domain.IndexStage:
			state.deleted = false
			state.indexed = true
			state.snapshot = payload
			state.indexEvent = event
		default:
			// collect, normalize, enrich, derive: состояние документа не
			// меняют. Считать их прогрессом конвейера здесь незачем — у
			// пересборки своя правда, и она в стадии index.
		}
	}

	return collectPlan(states)
}

// collectPlan превращает накопленные состояния в отсортированный план.
func collectPlan(states map[string]*docState) (Plan, error) {
	docIDs := make([]string, 0, len(states))
	for docID := range states {
		docIDs = append(docIDs, docID)
	}
	slices.Sort(docIDs)

	plan := Plan{
		Documents: make([]Document, 0, len(docIDs)),
		Deleted:   make([]string, 0, len(docIDs)),
	}
	for _, docID := range docIDs {
		state := states[docID]
		if !state.indexed {
			// Ни одной стадии index — восстанавливать нечего, и даже при
			// наличии надгробия убирать из индекса тоже нечего.
			continue
		}
		if state.deleted {
			plan.Deleted = append(plan.Deleted, docID)
			continue
		}
		document, err := state.document()
		if err != nil {
			return Plan{}, err
		}
		plan.Documents = append(plan.Documents, document)
	}
	return plan, nil
}

// document собирает восстановленный документ из снимка последней стадии index.
func (s *docState) document() (Document, error) {
	if err := s.snapshot.requireIdentity(s.indexEvent); err != nil {
		return Document{}, err
	}
	documentID, err := s.parseDocID()
	if err != nil {
		return Document{}, err
	}
	kbID, err := parseEventUUID(s.snapshot.KBID, "kb_id", s)
	if err != nil {
		return Document{}, err
	}
	versionID, err := parseEventUUID(s.snapshot.VersionID, "version_id", s)
	if err != nil {
		return Document{}, err
	}
	if len(s.snapshot.Blocks) == 0 {
		return Document{}, domain.InvalidMetadata(
			"Событие стадии index без блоков: восстановить документ из него нельзя").
			WithDetails(map[string]any{
				"doc_id":      s.docID,
				"event_id":    s.indexEvent.EventID.String(),
				"occurred_at": s.indexEvent.OccurredAt.UTC().Format(time.RFC3339Nano),
			})
	}

	blocks := make([]domain.Block, len(s.snapshot.Blocks))
	copy(blocks, s.snapshot.Blocks)
	return Document{
		DocumentID:  documentID,
		TenantID:    s.tenantID,
		KBID:        kbID,
		VersionID:   versionID,
		ContentHash: s.snapshot.ContentHash,
		Title:       s.snapshot.Title,
		Rights:      domain.DocumentRightsFor(s.snapshot.SpaceID, s.snapshot.ACL),
		Blocks:      blocks,
		ChunkCount:  s.snapshot.ChunkCount,
	}, nil
}

// parseDocID разбирает doc_id события в UUID.
func (s *docState) parseDocID() (uuid.UUID, error) {
	return parseEventUUID(s.docID, "doc_id", s)
}

// parseEventUUID разбирает обязательное поле восстановленного документа.
//
// Строка вместо UUID в документе — не «не страшно», а признак того, что lake
// собран мимо конвейера: наверх определены именно UUID, и молча пропустить
// такое поле означало бы построить индекс с document_id, которого нет в
// documents, то есть с документом, который никто не удалит и никто не найдёт.
func parseEventUUID(raw, field string, state *docState) (uuid.UUID, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return uuid.Nil, domain.InvalidMetadata(
			"В событии стадии index нет поля " + field + ": восстановить документ нельзя").
			WithDetails(map[string]any{
				"doc_id":   state.docID,
				"event_id": state.indexEvent.EventID.String(),
			})
	}
	parsed, err := uuid.Parse(trimmed)
	if err != nil || parsed == uuid.Nil {
		return uuid.Nil, domain.InvalidMetadata(
			"Поле " + field + " события стадии index не является идентификатором").
			WithDetails(map[string]any{
				"doc_id":   state.docID,
				"event_id": state.indexEvent.EventID.String(),
				field:      raw,
			})
	}
	return parsed, nil
}

// payloadReaders — разбор тела события по версии схемы.
//
// Карта, а не switch, по тем же причинам, что и в lake.go: следующая версия
// (raw.v2) добавляется строкой, и §8 («новая версия — новый потребитель»)
// требует именно такого порядка. Версия, которой здесь нет, ОСТАНАВЛИВАЕТ
// пересборку: проигнорировать её значило бы построить индекс по неполной
// картине и отчитаться об успехе.
var payloadReaders = map[string]func(json.RawMessage) (lakePayload, error){
	domain.RawEventVersion: decodeRawV1,
}

// supportedPayloadVersions перечисляет версии, которые умеет читать пересборка.
func supportedPayloadVersions() []string {
	versions := make([]string, 0, len(payloadReaders))
	for version := range payloadReaders {
		versions = append(versions, version)
	}
	slices.Sort(versions)
	return versions
}

// decodePayload разбирает тело события по его версии.
func decodePayload(event domain.Event) (lakePayload, error) {
	read, ok := payloadReaders[event.Version]
	if !ok {
		return lakePayload{}, domain.InvalidMetadata(fmt.Sprintf(
			"Версия схемы события %q не поддерживается: пересборка индекса умеет %v",
			event.Version, strings.Join(supportedPayloadVersions(), ", ")))
	}
	payload, err := read(event.Payload)
	if err != nil {
		return lakePayload{}, err
	}
	if err := payload.checkAgainst(event); err != nil {
		return lakePayload{}, err
	}
	return payload, nil
}

// decodeRawV1 читает тело версии raw.v1.
//
// Объект, а не массив и не скаляр — та же причина, что у lake.go: конверт §4
// состоит из полей, и потребитель читает их поимённо.
func decodeRawV1(raw json.RawMessage) (lakePayload, error) {
	if len(raw) == 0 {
		return lakePayload{}, domain.InvalidMetadata(
			"Тело события lake пусто: восстановить из него нечего")
	}
	if !json.Valid(raw) {
		return lakePayload{}, domain.InvalidMetadata(
			"Тело события lake не является корректным JSON")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return lakePayload{}, domain.InvalidMetadata(
			"Тело события версии " + domain.RawEventVersion +
				" должно быть JSON-объектом: " + err.Error())
	}
	var payload lakePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return lakePayload{}, domain.InvalidMetadata(
			"Тело события версии " + domain.RawEventVersion + " не разбирается").
			WithCause(err)
	}
	return payload, nil
}
