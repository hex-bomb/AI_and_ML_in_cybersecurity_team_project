package indexing

// Снимок документа в lake по стадии index — писатель, которого у стадии не
// было (§1, принцип 1: «источник правды один: сырой слой (lake) + события. Всё
// остальное (поисковый индекс, граф, PageRank, эмбеддинги) — производные
// представления, которые можно пересобрать из lake за часы»; §4: «любой индекс
// можно удалить и воссоздать из lake без обращения к источникам»).
//
// ДО ЧЕГО ЭТО НУЖНО БЫЛО. Писателей lake был ровно один: delete.go клал
// надгробие. Стадия index писала чанки в индекс и молчала, поэтому пересборка
// индекса (internal/jobs/rebuild) читала ленту, в которой нет НИ ОДНОГО события
// стадии index, сворачивала её в пустой план и рапортовала «пересборка прошла».
// Потребитель был корректен, поток был пуст, а учебная тревога §9 при этом
// отчиталась бы об успехе. Lake не был источником правды: он был журналом
// удалений.
//
// ПОЧЕМУ ЗДЕСЬ, А НЕ В ПЕРЕСБОРКЕ. §8 требует, чтобы новое ПОДПИСЫВАЛОСЬ на
// поток событий, а не чтобы каждый старый потребитель начал проверять поле.
// Снимок — это утверждение стадии index («эта версия документа в индексе, вот её
// права и вот её блоки»), и писать его обязана именно та стадия, которая
// индексирует: собирать снимок в пересборке означало бы восстанавливать индекс
// по предположению о том, что было проиндексировано.
//
// ГЛАВНОЕ ПРО ЭТОТ ФАЙЛ: ТЕЛО ЗДЕСЬ — КОНТРАКТ, А НЕ ОПИСАНИЕ. Читатель берёт
// поля поимённо (lakePayload в internal/jobs/rebuild/payload.go), поэтому тело
// типизировано и повторяет эти поля один в один. Расхождение в названии или в
// версии схемы не падает нигде: пересборка просто не соберёт документ и
// рапортует «пересборка прошла», то есть тихая потеря целого класса данных.
// Поэтому тест в index_event_test.go прогоняет НАСТОЯЩУЮ свёртку rebuild по
// событиям этой стадии.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"time"

	"github.com/google/uuid"

	domain "github.com/kbsearch/kb/internal/envelope"
)

// indexContentHashPrefix — алгоритм ключа идемпотентности снимка.
//
// Тот же приём именования, что у tombstoneKeyPrefix («job:») и у ключей конверта
// («<алгоритм>:<hex>»): ограничение ck_raw_events_content_hash проверяет ФОРМУ и
// не перечисляет алгоритмы, потому что смена хэш-функции не должна требовать
// миграции схемы (§8).
const indexContentHashPrefix = "sha256:"

// indexSnapshot — часть снимка, по которой считается ключ идемпотентности.
//
// ПОЧЕМУ ЭТО НЕ ХЭШ ИСХОДНОГО ФАЙЛА (parsed.ContentSHA256). Снимок в lake
// append-only и идемпотентен по event_id (реестр raw_event_ids, §4): lake
// отбрасывает событие, event_id которого уже есть. Ключ по хэшу файла связывает
// снимок с тем СЫРЬЁМ, из которого он когда-то получился, а не с тем, что в
// нём написано, и это расходится в двух местах:
//
//   - права. Администратор закрыл документ лично, стадия переиндексировала его —
//     блоки те же, rights другие. По хэшу файла lake отбросил бы новое событие и
//     оставил прежний снимок с прежними правами: пересборка восстановила бы
//     документ шире, чем он есть. Это утечка §6.
//   - разбор. Тот же файл, разобранный новой версией конвертера, даёт другие
//     блоки. По хэшу файла lake опять молча оставил бы старые блоки, и
//     пересборка вернула бы в индекс документ, которого в нём уже нет.
//
// Идентичность здесь — ровно то, ЧТО событие утверждает: база, версия, заголовок,
// права и блоки. Смена настроек нарезки в неё НЕ входит: chunk_count —
// диагностика (§9), и пересборка специально трактует его смену как правильный
// результат, а не как новый факт.
type indexSnapshot struct {
	KBID      string         `json:"kb_id"`
	VersionID string         `json:"version_id"`
	Title     string         `json:"title"`
	SpaceID   string         `json:"space_id"`
	ACL       []string       `json:"acl"`
	Blocks    []domain.Block `json:"blocks"`
}

// indexPayload — тело события стадии index версии raw.v1.
//
// Порядок и набор полей повторяют lakePayload в internal/jobs/rebuild/payload.go.
// Карта означала бы, что любое поле можно забыть и тихо потерять, а потерянное
// поле документа здесь означает потерянный документ в выдаче.
//
// ПРАВА ПИШУТСЯ ВСЕГДА, ОБА ПОЛЯ. Пустые space_id и acl — законные значения, а не
// «поле отсутствует»: документ, лично закрытый администратором на пустой список,
// обязан восстановиться закрытым. Отличать «нет поля» от «поле пустое» читатель
// не умеет и не обязан.
type indexPayload struct {
	SchemaVersion string         `json:"schema_version"`
	DocID         string         `json:"doc_id"`
	TenantID      string         `json:"tenant_id"`
	Stage         string         `json:"stage"`
	ContentHash   string         `json:"content_hash"`
	KBID          string         `json:"kb_id"`
	VersionID     string         `json:"version_id"`
	Title         string         `json:"title"`
	SpaceID       string         `json:"space_id"`
	ACL           []string       `json:"acl"`
	Blocks        []domain.Block `json:"blocks"`
	ChunkCount    int            `json:"chunk_count"`
}

// IndexEventParams — вход IndexEvent.
type IndexEventParams struct {
	// Lease — задание, ради которого стадия работает. Тенант и документ берутся
	// отсюда же: это единственные значения, за которые отвечает выдача.
	Lease domain.JobLease
	// Parsed — разобранный документ, который ушёл в индекс. Ровно он и есть
	// снимок: блоки, база, версия, заголовок.
	Parsed domain.ParsedDocument
	// Rights — разрешённые права документа. Те же, что ушли в чанки, и по
	// той же причине: индекс и источник правды обязаны описывать документ
	// одинаково, иначе пересборка разойдётся с выдачей (§6).
	Rights domain.DocumentRights
	// ChunkCount — число чанков прошлой индексации. Диагностика, в ключ
	// идемпотентности не входит (см. indexSnapshot).
	ChunkCount int
	// OccurredAt — когда стадия произвела факт.
	OccurredAt time.Time
}

// IndexEvent собирает событие стадии index со снимком документа.
//
// Экспортировано по той же причине, что TombstoneEvent: потребитель lake
// (пересборка индекса) читает тело по полям, и собирать его второй раз где-то
// ещё нельзя — два тела разойдутся, и пересборка соберёт документ из первого.
//
// ПРОВЕРКИ ДО СБОРКИ, А НЕ ПОСЛЕ. Три поля читатель считает обязательными
// (doc_id, kb_id, version_id), и отсутствие любого из них останавливает
// пересборку ВСЕЙ ленты, а не только этого документа: одно битое событие
// роняет учебную тревогу целиком. Поэтому битый вход отвергается здесь, на
// границе стадии, где оператор ещё видит код ошибки задания.
func IndexEvent(p IndexEventParams) (domain.Event, error) {
	versionID, ok := p.Lease.VersionIDValue()
	if !ok {
		return domain.Event{}, domain.Conflict(
			"Событие стадии index у задания без версии документа")
	}
	// Тело обязано подтверждать шапку (lakePayload.checkAgainst). Собирать тело
	// из одного источника, а шапку из другого — значило бы записать в источник
	// правды документ, заголовок и содержимое которого описывают разные вещи.
	if p.Parsed.TenantID != p.Lease.TenantID ||
		p.Parsed.DocumentID != p.Lease.DocumentID ||
		p.Parsed.VersionID != versionID {
		return domain.Event{}, domain.Conflict(
			"Разобранный документ не соответствует заданию индексации")
	}
	if p.Parsed.KBID == uuid.Nil {
		return domain.Event{}, domain.ConfigurationMissing(
			"У версии документа не задана база знаний: снимок в lake был бы невосстановим",
			map[string]any{
				"doc_id":     p.Lease.DocumentID.String(),
				"version_id": versionID.String(),
			})
	}
	if len(p.Parsed.Blocks) == 0 {
		return domain.Event{}, domain.InvalidMetadata(
			"Снимок документа без блоков пересборка индекса не восстановит")
	}

	snapshot := indexSnapshot{
		KBID:      p.Parsed.KBID.String(),
		VersionID: versionID.String(),
		Title:     p.Parsed.Title,
		SpaceID:   p.Rights.SpaceID,
		// ExceptionTerms никогда не nil: пустой список сериализовался бы в
		// null, а пересборка не должна отличать «исключений нет» от
		// «список не задан».
		ACL: p.Rights.ExceptionTerms(),
		// Копия, а не сам срез: тело уходит в lake и переживает стадию, а
		// разобранный документ принадлежит вызывающему.
		Blocks: slices.Clone(p.Parsed.Blocks),
	}
	contentHash, err := snapshotContentHash(snapshot)
	if err != nil {
		return domain.Event{}, err
	}

	// schemaVersion — тот же, что у чанков этой стадии, и это НЕ совпадение
	// строк, а одно поле: снимок и чанк описывают один и тот же документ по
	// одной схеме. Свою вторую строку здесь заводить нельзя — она уехала бы в
	// lake впереди читателя и разошлась бы с ним при первой же правке.
	payload, err := json.Marshal(indexPayload{
		SchemaVersion: schemaVersion,
		DocID:         p.Lease.DocumentID.String(),
		TenantID:      p.Lease.TenantID.String(),
		Stage:         domain.IndexStage.String(),
		ContentHash:   contentHash,
		KBID:          snapshot.KBID,
		VersionID:     snapshot.VersionID,
		Title:         snapshot.Title,
		SpaceID:       snapshot.SpaceID,
		ACL:           snapshot.ACL,
		Blocks:        snapshot.Blocks,
		ChunkCount:    p.ChunkCount,
	})
	if err != nil {
		return domain.Event{}, domain.Internal("Не удалось закодировать снимок документа для lake").
			WithCause(err)
	}

	return domain.NewEvent(domain.NewEventParams{
		TenantID:    p.Lease.TenantID,
		Stage:       domain.IndexStage,
		DocID:       p.Lease.DocumentID.String(),
		ContentHash: contentHash,
		Payload:     payload,
		OccurredAt:  p.OccurredAt,
	})
}

// snapshotContentHash считает ключ идемпотентности §4 по самому снимку.
//
// json.Marshal здесь детерминирован: indexSnapshot состоит из скаляров, среза
// строк и среза Block, а у Block нет ни карт, ни интерфейсов, поэтому порядок
// полей задан объявлением типа и не зависит ни от версии Go, ни от обхода.
func snapshotContentHash(snapshot indexSnapshot) (string, error) {
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return "", domain.Internal("Не удалось закодировать снимок документа для ключа lake").
			WithCause(err)
	}
	sum := sha256.Sum256(encoded)
	return indexContentHashPrefix + hex.EncodeToString(sum[:]), nil
}

// publishSnapshot кладёт в lake снимок только что проиндексированной версии.
//
// ГДЕ ИМЕННО В ЖИЗНЕННОМ ЦИКЛЕ СТАДИИ (см. Index в handler.go): после записи
// чанков и ДО переключения активной версии. Три варианта, и все три были
// рассмотрены:
//
//	ДО записи чанков — нельзя. lake утверждало бы «документ в индексе», пока его
//	там нет, а стадия могла бы упасть позже и оставить это утверждение навсегда.
//	Восстановление по такому lake построило бы индекс из документа, который не
//	индексировался.
//
//	ПОСЛЕ транзакции активации — тоже нельзя, и по причине, которой нет ни в
//	одном комментарии про lake: FinishIndex закрывает задание (status succeeded,
//	lease_token очищен). Отказ lake после этого уходит в worker.fail, а тот
//	пишет ошибку условным UPDATE'ом по ЖИВОМУ lease — по закрытому заданию он
//	не попадёт ни в одну строку. Задание вернётся в очередь по своему обычному
//	пути, то есть НИКОГДА, а документ останется недостижимым для пересборки
//	навсегда. Это ровно та тихая потеря, которую §1 запрещает.
//
//	МЕЖДУ — здесь. Чанки уже записаны и их число сверено, то есть утверждение
//	истинно; задание ещё живо, то есть отказ lake повторяем, а повтор
//	идемпотентен: тот же снимок даёт тот же event_id, и lake отбрасывает дубль
//	реестром raw_event_ids (§4). Ничего «лишнего» при этом не пишется.
//
// ОСТАВШЕЕСЯ ОКНО И ЕГО НАПРАВЛЕНИЕ. Между этой записью и переключением активной
// версии lake знает о версии раньше, чем documents.active_version_id. Это то же
// направление, которое delete.go уже выбрал и обосновал: источник правды говорит
// раньше производного представления, и падение в этом окне даёт «пересборка
// восстановит только что записанную версию». Обратное направление даёт «lake
// молчит, документ потерян».
func (i *Indexer) publishSnapshot(
	ctx context.Context, lease domain.JobLease, parsed domain.ParsedDocument,
	rights domain.DocumentRights, chunkCount int,
) error {
	// Проверка на границе стадии, а не в NewIndexer: Deps общая у стадий convert
	// и index (см. ParsedKey), и требовать lake при сборке означало бы запретить
	// частичную сборку. Стадия, которой lake не нужен, его и не спрашивает.
	if i.deps.Lake == nil {
		return domain.ConfigurationInvalid(
			"Сборка стадии index не задала lake: снимок документа некуда писать, "+
				"и пересборка индекса из lake его не восстановит",
			map[string]any{"job_id": lease.JobID.String()},
		)
	}

	event, err := IndexEvent(IndexEventParams{
		Lease:      lease,
		Parsed:     parsed,
		Rights:     rights,
		ChunkCount: chunkCount,
		OccurredAt: i.deps.Clock.Now(),
	})
	if err != nil {
		return err
	}
	// Ошибка наружу, без обёртки и без проглатывания: lake — источник правды,
	// и молчаливая потеря снимка означала бы документ, которого пересборка не
	// найдёт, при зелёной стадии и живом задании.
	return i.deps.Lake.Publish(ctx, event)
}
