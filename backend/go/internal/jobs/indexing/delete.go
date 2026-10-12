package indexing

// Стадия delete — надгробие удаления (§4, стадия index: «удаление — через
// событие-надгробие (tombstone)»; §9: «документ, удалённый в источнике,
// исчезает из выдачи»).
//
// ДО ЧЕГО ЭТО НУЖНО БЫЛО. MarkDeleted ставит documents.deleted_at и кладёт
// задание стадии delete в очередь, но обслуживать его было некому: cmd/worker
// собирал только convert и index. Чанки удалённого документа оставались в
// Elasticsearch навсегда — документ, удалённый в источнике, продолжал
// выдаваться, то есть прямое нарушение §9. Lake об удалении тоже молчал:
// CHECK ck_raw_events_stage допускал ровно пять стадий конвейера, и удаление не
// помещалось ни в одну — пересборка индекса из lake (§1, принцип 1) была не
// возможна в принципе.

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
)

// tombstoneSchemaVersion — версия тела надгробия. Та же, что у конверта
// (schemaVersion в handler.go): тело надгробия читает потребитель lake так же,
// как тело любой другой стадии, и версионируется по тем же правилам (§4,
// «схемы событий версионируются»).
const tombstoneSchemaVersion = "1.0"

// TombstoneReasonSourceDeleted — причина надгробия по умолчанию: объект убран
// в источнике. Причина едет в теле события, потому что lake append-only и
// исправить событие в нём нельзя (см. триггер raw_events_append_only в
// миграции 0002): если понадобится объяснить удаление, его можно будет только
// дописать.
const TombstoneReasonSourceDeleted = "source_deleted"

// tombstoneKeyPrefix — пространство ключа идемпотентности надгробия.
//
// ПОЧЕМУ «job:», А НЕ «sha256:». Ограничение ck_raw_events_content_hash
// требует форму «<алгоритм>:[0-9a-f]+» и проверяет форму, а не значение:
// значение колонки — ИДЕНТИФИКАТОР ФАКТА УДАЛЕНИЯ, а не хэш текста, и называть
// его sha256 было бы ложью, которую потом не разобрать. Префикс здесь тот же
// приём именования, что у chunk_id и block_id конверта («<алгоритм>:<hex>»).
const tombstoneKeyPrefix = "job:"

// TombstoneKey возвращает ключ идемпотентности надгробия для задания удаления.
//
// §4 требует идемпотентности всех стадий по ключу `doc_id + content_hash`, и у
// надгробия здесь ровно один содержательный выбор.
//
//   - Ключ по ДОКУМЕНТУ («удаление документа — один факт») проще, но неверен:
//     документ можно загрузить заново и удалить снова, а это уже ДРУГОЙ факт.
//     С ключом по документу второе удаление получило бы тот же event_id, lake
//     его отбросил бы, и пересборка индекса вернула бы документ в выдачу.
//
//   - Ключ по ЗАДАНИЮ (здесь) верен: у одного удаления одно задание
//     (MarkDeleted создаёт его вместе с documents.deleted_at), повтор той же
//     стадии — то же задание и тот же ключ, то есть no-op, а второе удаление
//     получает новое задание и новое событие.
//
// Разделители uuid убираются: ограничение content_hash требует сплошной
// [0-9a-f], а дефис ей не соответствует.
func TombstoneKey(jobID uuid.UUID) string {
	return tombstoneKeyPrefix + strings.ReplaceAll(jobID.String(), "-", "")
}

// tombstonePayload — тело события надгробия версии raw.v1.
//
// validateRawV1Payload требует JSON-объект, и читатель пересборки узнаёт
// документ по полям тела, поэтому doc_id, tenant_id и stage обязаны быть здесь
// теми же значениями, что в шапке события: одно тело, собранное из разных
// источников, — это ровно тот случай, когда пересборка собрала бы документ из
// чужого содержимого (ср. lakePayload в internal/arch/rebuild_test.go).
type tombstonePayload struct {
	SchemaVersion string `json:"schema_version"`
	DocID         string `json:"doc_id"`
	TenantID      string `json:"tenant_id"`
	Stage         string `json:"stage"`
	Reason        string `json:"reason"`
	DeleteJobID   string `json:"delete_job_id"`
	DeletedAt     string `json:"deleted_at"`
}

// TombstoneEvent собирает событие-надгробие для задания стадии delete.
//
// Экспортировано потому, что потребитель lake (пересборка индекса) обязан уметь
// отличить надгробие от обычного события, и собирать его второй раз в своём
// коде нельзя: тело и ключ обязаны совпадать байт в байт, иначе lake получит
// два разных события об одном удалении.
func TombstoneEvent(lease domain.JobLease, occurredAt time.Time) (domain.Event, error) {
	payload, err := json.Marshal(tombstonePayload{
		SchemaVersion: tombstoneSchemaVersion,
		DocID:         lease.DocumentID.String(),
		TenantID:      lease.TenantID.String(),
		Stage:         domain.DeleteStage.String(),
		Reason:        TombstoneReasonSourceDeleted,
		DeleteJobID:   lease.JobID.String(),
		DeletedAt:     occurredAt.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return domain.Event{}, domain.Internal("Не удалось закодировать надгробие удаления").
			WithCause(err)
	}
	return domain.NewEvent(domain.NewEventParams{
		TenantID:    lease.TenantID,
		Stage:       domain.DeleteStage,
		DocID:       lease.DocumentID.String(),
		ContentHash: TombstoneKey(lease.JobID),
		Payload:     payload,
		OccurredAt:  occurredAt,
	})
}

// DeleteJobs — единственный порт, которого стадии delete не хватает в
// ports.JobRepository.
//
// ПОЧЕМУ УЗКИЙ ИНТЕРФЕЙС, А НЕ НОВЫЙ МЕТОД В ports. ports — замороженный шов
// (§8), и добавление FinishDelete в общий JobRepository заставило бы
// переписать ВСЕ подделки очереди в дереве (jobs/worker, jobs/indexing,
// ingest/parser) ради одного потребителя. Узкий интерфейс объявляет
// потребляющая сторона — тот же приём, что у IndexingSettings в handler.go.
//
// ДОЛГ, ЗАФИКСИРОВАННЫЙ ЗДЕСЬ. Реализация deleteJobs ниже пишет SQL сам,
// потому что слой адаптеров (internal/store) этой задаче не принадлежит. Когда
// шов ports откроют следующим касанием, FinishDelete переезжает в
// postgres.JobRepository, deleteJobs удаляется, а сигнатура DeleteJobs
// остаётся прежней — вызывающий код не меняется.
type DeleteJobs interface {
	// FinishDelete закрывает задание стадии delete.
	FinishDelete(ctx context.Context, tx ports.Queryer, lease domain.JobLease, removed int) error
}

// DeleteDeps — порты стадии delete.
//
// Отдельная структура, а не Deps стадии индексации: у стадии удаления другой
// набор зависимостей (ей не нужны эмбеддер, настройки и файловое хранилище), и
// требовать их означало бы запретить частичную сборку — ровно то, чего избегает
// Deps.ParsedKey в handler.go.
type DeleteDeps struct {
	Index      ports.SearchIndex
	Lake       ports.EventBus
	Jobs       DeleteJobs
	Transactor ports.Transactor
	Clock      ports.Clock
}

// Deleter выполняет стадию delete: надгробие в lake, удаление чанков из индекса
// и закрытие задания. Как и Indexer, ничего не знает о lease, попытках и
// повторах: это работа цикла (internal/jobs/worker).
type Deleter struct {
	deps DeleteDeps
}

// NewDeleter собирает стадию удаления.
//
// Часы подставляются по умолчанию (см. NewDeleteJobs): occurred_at надгробия
// пишется ими, и без них стадия упала бы на первом же задании.
func NewDeleter(deps DeleteDeps) *Deleter {
	if deps.Clock == nil {
		deps.Clock = ports.SystemClock{}
	}
	return &Deleter{deps: deps}
}

// Delete обрабатывает одно задание стадии delete.
//
// ПОРЯДОК ШАГОВ ОБЯЗАТЕЛЕН, и он обратный привычному «сначала внешний мир,
// потом своё»:
//
//  1. НАДГРОБИЕ В LAKE. Источник правды — lake (§1, принцип 1), и удаление
//     обязано быть в нём ДО того, как изменится индекс. Обратный порядок оставил
//     бы окно, в котором документа нет в индексе, но нет и в lake: падение
//     процесса в этом окне означало бы, что пересборка вернёт документ в выдачу,
//     то есть ровно то нарушение §9, которое стадия и чинит. В этом порядке
//     падение даёт обратное — документ пропадает из индекса позже, при пересборке,
//     и lake уже знает, что он должен отсутствовать.
//
//  2. УДАЛЕНИЕ ЧАНКОВ ИЗ ИНДЕКСА. Индекс производен, поэтому его состояние в
//     любой момент можно восстановить из lake — но только если lake уже сказал
//     про удаление.
//
//  3. ЗАКРЫТИЕ ЗАДАНИЯ. Последним: пока задание живо, повтор вернёт его в
//     очередь и повторит первые два шага, а они идемпотентны.
//
// НИ ОДИН ШАГ НЕ ТРЕБУЕТ СУЩЕСТВОВАНИЯ ДОКУМЕНТА. Документа может уже не
// быть в выдаче, в lake может уже быть надгробие — и это нормальный успех:
// §4 требует идемпотентности стадий, а не «обязательно что-то изменилось».
func (d *Deleter) Delete(ctx context.Context, lease domain.JobLease) error {
	if !lease.IsDeleteStage() {
		return domain.ConfigurationInvalid(
			"Задание не на стадии delete",
			map[string]any{"job_id": lease.JobID.String(), "stage": lease.Stage},
		)
	}

	event, err := TombstoneEvent(lease, d.deps.Clock.Now())
	if err != nil {
		return err
	}
	if err := d.deps.Lake.Publish(ctx, event); err != nil {
		return err
	}

	// Тенант и документ берутся из lease и идут в индекс ВМЕСТЕ. Это и есть
	// изоляция (§3/§4): doc_id §4 хэширует tenant, но исторические и
	// загруженные id не хэшировались, и один term по document_id снёс бы
	// одноимённый документ другого тенанта.
	removed, err := d.deps.Index.DeleteDocument(
		ctx, lease.TenantID.String(), lease.DocumentID.String(),
	)
	if err != nil {
		return err
	}

	return d.deps.Transactor.WithTx(ctx, func(tx ports.Queryer) error {
		return d.deps.Jobs.FinishDelete(ctx, tx, lease, removed)
	})
}

// deleteJobs — реализация DeleteJobs поверх ports.Queryer.
//
// Время берётся из порта, а не из time.Now: updated_at задания пишется тем же
// часами, что и lease, иначе в журнале заданий появляются записи из двух
// разных часов (тот же довод, по которому lake пишет received_at из БД).
type deleteJobs struct {
	clock ports.Clock
}

// NewDeleteJobs собирает реализацию DeleteJobs по умолчанию.
//
// Часы без запасного варианта означали бы падение внутри UPDATE там, где
// причину видно только по стековому следу. Подстановка системных часов здесь
// повторяет приём worker.New для Logger и PollInterval.
func NewDeleteJobs(clock ports.Clock) DeleteJobs {
	if clock == nil {
		clock = ports.SystemClock{}
	}
	return deleteJobs{clock: clock}
}

// finishDeleteSQL закрывает задание стадии delete.
//
// Один UPDATE, а не SELECT ... FOR UPDATE плюс UPDATE, как в lockRunningJob:
// проверка lease здесь УСЛОВИЕ САМОГО UPDATE, поэтому она атомарна с записью и
// гонки не возникает — второй UPDATE после освобождения блокировки увидит уже
// не тот статус и не тронет строку.
//
// Условия повторяют lockRunningJob и добавляют к нему tenant_id, stage и kind:
// задание может быть чужим, выданным по подделанному токену, или просто не тем,
// что закрывают. Ноль затронутых строк означает ровно одно — работа не наша, и
// это тот же JobLeaseLost, которым FinishConvert и FinishIndex отвечают на
// чужую стадию.
const finishDeleteSQL = `
	UPDATE jobs
	SET status          = $8,
		processed_chunks = $6,
		total_chunks     = $6,
		lease_token      = NULL,
		lease_expires_at = NULL,
		next_attempt_at  = NULL,
		error_code       = NULL,
		error_message    = NULL,
		updated_at       = $7
	WHERE id = $1
	  AND tenant_id = $2
	  AND lease_token = $3
	  AND stage = $4
	  AND kind = $5
	  AND status = 'running'
	  AND lease_expires_at > $7`

// FinishDelete ставит заданию статус succeeded и снимает lease.
//
// removed — число снятых чанков: в jobs это processed_chunks/total_chunks,
// то есть диагностика «сколько чанков документа ушло из индекса». Ноль здесь
// законен (документ мог не быть проиндексирован), и ошибкой не считается.
func (d deleteJobs) FinishDelete(
	ctx context.Context, tx ports.Queryer, lease domain.JobLease, removed int,
) error {
	if removed < 0 {
		return domain.ConfigurationInvalid(
			"Число удалённых чанков отрицательно",
			map[string]any{"job_id": lease.JobID.String(), "removed": removed},
		)
	}
	if tx == nil {
		return domain.ConfigurationInvalid(
			"Стадия delete не получила транзакцию для закрытия задания",
			map[string]any{"job_id": lease.JobID.String()},
		)
	}

	tag, err := tx.Exec(ctx, finishDeleteSQL,
		lease.JobID, lease.TenantID, lease.LeaseToken,
		lease.Stage, lease.Kind, removed,
		d.clock.Now(), domain.JobStatusSucceeded,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.JobLeaseLost("Задание удаления не найдено по своему lease или уже разобрано")
	}
	return nil
}
