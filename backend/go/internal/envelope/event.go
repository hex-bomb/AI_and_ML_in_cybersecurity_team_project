package domain

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// RawEventVersion is the current lake event schema version: roadmap §4,
// «схемы событий версионируются (raw.v1)». Версия едет в каждом событии,
// потому что потребитель читает payload, не зная, кто его написал.
const RawEventVersion = "raw.v1"

// Stage is one of the lake journal stages: the five pipeline stages of roadmap
// §4 (collect → normalize → enrich → index → derive) and the delete tombstone.
//
// Константы названы суффиксом Stage, а не префиксом, как стадии задания
// (StageConvert, StageIndex в job.go): это другой набор, и префикс Stage у
// него уже занят. Значение строки совпадает с названием стадии в §4.
type Stage string

const (
	CollectStage   Stage = "collect"
	NormalizeStage Stage = "normalize"
	EnrichStage    Stage = "enrich"
	IndexStage     Stage = "index"
	DeriveStage    Stage = "derive"
)

// DeleteStage — надгробие удаления.
//
// §4, стадия index: «удаление — через событие-надгробие (tombstone)», §9:
// «документ, удалённый в источнике, исчезает из выдачи». Стадия lake называется
// так же, как стадия ЗАДАНИЯ job.go (StageDelete), потому что это один и тот же
// факт с двух сторон: задание стадии delete обрабатывается и кладёт в lake
// именно это событие.
//
// ПОЧЕМУ ОТДЕЛЬНАЯ СТАДИЯ, А НЕ ФЛАГ «tombstone: true» В СОБЫТИИ СТАДИИ INDEX.
// Выбор сделан один раз и держится на четырёх доводах; отказ от любого из них
// означал бы правку существующих потребителей.
//
//  1. ИДЕНТИЧНОСТЬ СОБЫТИЯ. event_id — UUIDv5 от
//     (version|tenant|stage|doc_id|content_hash). У удаления нет содержимого, и
//     хэшировать нечего: с флагом в событии index пришлось бы либо выдумать
//     content_hash ради флага (ложь в колонке, названной хэшем), либо столкнуть
//     надгробие с index-событием той же версии по одному event_id. У отдельной
//     стадии своя координата в ключе, и надгробие — самостоятельный факт.
//
//  2. ПОВТОРНОЕ УДАЛЕНИЕ ПОСЛЕ НОВОЙ ЗАГРУЗКИ. Документ можно загрузить заново
//     и удалить снова, и это уже ДРУГОЙ факт: пересборка индекса обязана увидеть
//     второе удаление, иначе после него документ вернулся бы в выдачу. Ключ
//     «стадия delete + идентификатор удаления» различает эти факты; флаг внутри
//     index-события различить их нечем.
//
//  3. НОВЫЙ ПОТРЕБИТЕЛЬ, А НЕ ПРАВКА ВСЕХ. §1 (принцип 6) и §9 («Что не меняется
//     от фазы к фазе … Новое добавляется потребителями событий и флагами»)
//     требуют, чтобы новый потребитель ПОДПИСАЛСЯ, а не чтобы каждый старый
//     начал проверять поле. Стадия — это подписка; флаг — это правка всех.
//
//  4. ЧИТАЕМОСТЬ СХЕМЫ. CHECK ck_raw_events_stage перечисляет, что вообще может
//     лежать в lake. С флагом читатель обязан знать имя поля в теле события и
//     помнить про него; со стадией достаточно перечислить стадии.
//
// Обратная сторона решения и она же цена: шестая стадия — изменение набора
// стадий lake, поэтому ограничение ck_raw_events_stage расширяется миграцией
// 0003, а применённая 0002 не переписывается.
const DeleteStage Stage = "delete"

// AllStages returns the pipeline order followed by the tombstone. Список один:
// им пользуются проверка события, сборка конвейера и тесты, поэтому седьмая
// стадия не появится в одном месте и не пропадёт из другого.
//
// DeleteStage стоит последним не потому, что он «новее», а потому, что он не
// стадия прохода: конвейер одного документа collect → normalize → enrich →
// index → derive проходит один раз, а надгробие приходит отдельным фактом в
// любое время. В CHECK ck_raw_events_stage он при этом обязателен: список
// допустимых стадий lake и порядок конвейера — одна и та же величина, иначе
// событие удаления записать нечем.
func AllStages() []Stage {
	return []Stage{
		CollectStage, NormalizeStage, EnrichStage, IndexStage, DeriveStage, DeleteStage,
	}
}

// String returns the wire name of the stage.
func (s Stage) String() string { return string(s) }

// Valid reports membership in the §4 pipeline. Пустая стадия невалидна: событие
// без стадии не знает, кто его должен обработать.
func (s Stage) Valid() bool { return slices.Contains(AllStages(), s) }

// Event is one record of the raw lake — the unit the single source of truth is
// written in (§1, принцип 1). Всё остальное (поисковый индекс, граф,
// PageRank, эмбеддинги) — производные представления, пересобираемые из этих
// событий за часы.
type Event struct {
	// Version is the payload schema version, "raw.v1" today. Обязательное
	// поле, а не украшение: следующая версия payload читается иначе, и без
	// версии потребитель обязан угадывать.
	Version string `json:"version"`
	// EventID is the deterministic id of the record, см. EventIDFor.
	EventID uuid.UUID `json:"event_id"`
	// TenantID is the organization of the object; also part of the id, so
	// events of two tenants never collide.
	TenantID uuid.UUID `json:"tenant_id"`
	// Stage is the pipeline stage that produced the payload (§4).
	Stage Stage `json:"stage"`
	// DocID is the stable document id: хэш tenant + source + external_id
	// (§4, таблица конверта). Один и тот же объект источника даёт один и тот
	// же DocID при любом числе повторных сборов.
	DocID string `json:"doc_id"`
	// ContentHash is the idempotency material of §4 («ключ doc_id +
	// content_hash»). У события collect он может быть пустым: сырьё ещё не
	// разобрано и не посчитано.
	ContentHash string `json:"content_hash"`
	// OccurredAt is when the stage produced the event. В идентификатор не
	// входит намеренно: повтор той же работы — то же самое событие, а не
	// новое, и lake не должен расти от каждого ретрая.
	OccurredAt time.Time `json:"occurred_at"`
	// Payload is the stage-specific body, versioned together with Version.
	Payload json.RawMessage `json:"payload"`
}

// EventIDParams is the deterministic lake event id input.
type EventIDParams struct {
	Version     string
	TenantID    uuid.UUID
	Stage       Stage
	DocID       string
	ContentHash string
}

// lakeEventNamespace is the fixed UUIDv5 namespace of lake event ids. Он
// постоянен намеренно: смена пространства переименовала бы каждое событие,
// и идемпотентность стадий (§4) перестала бы работать незаметно.
var lakeEventNamespace = uuid.MustParse("5b394a91-0018-4eb6-87d7-e4371cbca47c")

// EventIDFor returns the deterministic lake event id (uuid.NewSHA1 — это UUIDv5).
//
// Детерминизм здесь не удобство, а требование §4: все стадии идемпотентны по
// ключу `doc_id + content_hash`, и повтор стадии обязан получить тот же
// event_id. Со случайным uuid повторный запуск конвейера после сбоя или
// перезапуска воркера дал бы вторую запись о том же факте — lake раздувался
// бы, а пересборка индекса пересчитывала бы одни и те же данные дважды.
//
// OccurredAt и Payload в имя не входят: метка времени и тело стадии не
// меняют факта «этот документ этой версии прошёл эту стадию», поэтому ретрай
// с другим временем и тем же содержимым остаётся тем же событием.
func EventIDFor(p EventIDParams) uuid.UUID {
	name := strings.Join([]string{
		p.Version,
		p.TenantID.String(),
		p.Stage.String(),
		p.DocID,
		p.ContentHash,
	}, "|")
	return uuid.NewSHA1(lakeEventNamespace, []byte(name))
}

// NewEventParams is the Event constructor input.
type NewEventParams struct {
	// Version is optional: пустое значение означает текущую RawEventVersion,
	// и валидное событие получается без ручного проставления версии.
	Version     string
	TenantID    uuid.UUID
	Stage       Stage
	DocID       string
	ContentHash string
	Payload     json.RawMessage
	OccurredAt  time.Time
}

// NewEvent builds a lake event and validates it, so an invalid one is hard to
// put together by hand: версия проставляется сама, event_id считается, а
// ошибка возвращается вместо события без инвариантов.
func NewEvent(p NewEventParams) (Event, error) {
	version := p.Version
	if strings.TrimSpace(version) == "" {
		version = RawEventVersion
	}
	event := Event{
		Version: version,
		EventID: EventIDFor(EventIDParams{
			Version:     version,
			TenantID:    p.TenantID,
			Stage:       p.Stage,
			DocID:       p.DocID,
			ContentHash: p.ContentHash,
		}),
		TenantID:    p.TenantID,
		Stage:       p.Stage,
		DocID:       p.DocID,
		ContentHash: p.ContentHash,
		OccurredAt:  p.OccurredAt,
		Payload:     p.Payload,
	}
	return event, event.Validate()
}

// Validate enforces the §4 rules on a lake event. Здесь же проверяется
// event_id: событие без него не идемпотентно, и обнаружится это только при
// пересборке lake, то есть слишком поздно.
func (e Event) Validate() error {
	if strings.TrimSpace(e.Version) == "" {
		return InvalidMetadata("event.version не может быть пустым: схема события версионируется (" + RawEventVersion + ")")
	}
	if e.TenantID == uuid.Nil {
		return InvalidMetadata("event.tenant_id не может быть пустым")
	}
	if !e.Stage.Valid() {
		return InvalidMetadata(fmt.Sprintf("event.stage %q не известен: допустимы %v", e.Stage, AllStages()))
	}
	if strings.TrimSpace(e.DocID) == "" {
		return InvalidMetadata("event.doc_id не может быть пустым")
	}
	if e.EventID == uuid.Nil {
		return InvalidMetadata("event.event_id не может быть пустым: событие собирается через NewEvent")
	}
	return nil
}
