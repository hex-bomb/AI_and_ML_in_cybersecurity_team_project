package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
)

// Lake — реализация обоих швов четвёртой двери roadmap §8 поверх PostgreSQL.
// Одна таблица, один тип, два интерфейса: пишет lake как EventBus, читает как
// EventStore. Разделение интерфейсов оставлено по §8 — на фазе 2 ClickHouse
// читается другим процессом, и сценарии об этом не знают.
//
// Здесь lake лежит в Postgres, а не в ClickHouse, сознательно (§9: «если
// команда маленькая, на фазе 1 писать raw_events в партиционированную таблицу
// Postgres за интерфейсом EventStore, а ClickHouse подключить на фазе 2»).
// В подписи и в тексте файла нет ни слова про Postgres как обязательное
// хранилище: замена на ClickHouse не должна затронуть ни одного сценария,
// поэтому здесь нет ничего, что пришлось бы переписывать при её выполнении.
type Lake struct {
	db *DB
	// now читается из БД при вставке (DEFAULT now()), а не из приложения:
	// received_at двух писателей обязан совпадать, иначе метрика лага (§4)
	// зависит от того, какой у воркера часы.
}

// Интерфейсы проверяются компилятором: ошибка в сигнатуре видна на сборке.
var (
	_ ports.EventBus   = (*Lake)(nil)
	_ ports.EventStore = (*Lake)(nil)
)

// NewLake создаёт lake поверх пула.
func NewLake(db *DB) *Lake { return &Lake{db: db} }

// ── Запись (EventBus) ───────────────────────────────────────────────────────

// Publish кладёт одно событие в lake. Повтор того же события — не ошибка:
// идемпотентность §4 устроена так, что ретрай стадии не меняет ни одной
// строки.
func (l *Lake) Publish(ctx context.Context, event domain.Event) error {
	return l.PublishBatch(ctx, []domain.Event{event})
}

// PublishBatch кладёт порцию событий одной транзакцией.
//
// Транзакция одна, а не по строке на событие, по двум причинам. Первая:
// порция стадии — это один факт о пачке документов, и половина записанной
// порции означала бы lake, из которого пересборка индекса соберёт неполную
// картину, причём без единого признака, что это случилось. Вторая:
// §4 «любой индекс можно удалить и воссоздать из lake», и этот индекс
// строится по порции; читать его на середине записи нельзя.
//
// Идемпотентность внутри транзакции обеспечивает реестр raw_event_ids,
// а не ON CONFLICT на самой raw_events: у партиционированной по occurred_at
// таблицы нет уникального индекса только по event_id, а составной
// (event_id, occurred_at) не ловит ретрай с новым occurred_at — ровно тот
// случай, ради которого §4 и требует идемпотентности.
func (l *Lake) PublishBatch(ctx context.Context, events []domain.Event) error {
	if len(events) == 0 {
		return nil
	}

	// События, которые не прошли проверку, уходят в очередь мёртвых, а не
	// теряются: §4 требует dead-letter-очередь у каждого топика, и «молча
	// пропущенная битая строка» опаснее, чем упавшая пачка.
	accepted := make([]domain.Event, 0, len(events))
	var deadLettered []error
	for _, event := range events {
		if err := validateLakeEvent(event); err != nil {
			if dlqErr := l.deadLetter(ctx, event, err); dlqErr != nil {
				deadLettered = append(deadLettered, fmt.Errorf(
					"событие %s не записано и не отправлено в очередь мёртвых: %w", event.EventID, dlqErr))
				continue
			}
			deadLettered = append(deadLettered, fmt.Errorf("событие %s: %w", event.EventID, err))
			continue
		}
		accepted = append(accepted, event)
	}

	err := l.db.WithTx(ctx, func(tx ports.Queryer) error {
		for _, event := range accepted {
			if err := l.appendEvent(ctx, tx, event); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Ошибки проверки возвращаются после коммита принятых: успешно записанное
	// не откатывается из-за соседа по порции, а вызывающий всё равно узнаёт,
	// что часть событий не доехала.
	return errors.Join(deadLettered...)
}

// appendEvent пишет одно событие: сначала реестр идемпотентности, и только
// при новом event_id — саму строку lake.
func (l *Lake) appendEvent(ctx context.Context, tx ports.Queryer, event domain.Event) error {
	tag, err := tx.Exec(ctx, `
		INSERT INTO raw_event_ids (event_id, tenant_id, occurred_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (event_id) DO NOTHING`,
		event.EventID, event.TenantID, event.OccurredAt)
	if err != nil {
		return err
	}
	// Ноль затронутых строк — событие уже записано. Это и есть идемпотентность
	// §4: повтор стадии с тем же содержимым не добавляет к lake ничего.
	if tag.RowsAffected() == 0 {
		return nil
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO raw_events (event_id, version, tenant_id, stage, doc_id,
		                        content_hash, occurred_at, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		event.EventID, event.Version, event.TenantID, event.Stage, event.DocID,
		event.ContentHash, event.OccurredAt, event.Payload)
	return err
}

// deadLetter кладёт событие в очередь мёртвых с причиной отказа.
//
// Отдельная транзакция здесь намеренно не нужна: очередь читает человек или
// оператор, и она должна пережить откат порции. Событие, не прошедшее
// проверку, и не попавшее в lake, и не записанное в очередь — это потерянное
// событие, и потерять его молча нельзя, поэтому ошибка записи в очередь
// поднимается наружу.
func (l *Lake) deadLetter(ctx context.Context, event domain.Event, cause error) error {
	// payload кладётся в двух видах сразу: разобранный — для чтения очередь
	// запросами, исходный — потому что битый JSON в jsonb не положил бы, а
	// именно такие события сюда и попадают.
	payloadRaw := string(event.Payload)
	payloadJSON := json.RawMessage(nil)
	if json.Valid(event.Payload) && len(event.Payload) > 0 {
		payloadJSON = event.Payload
	}

	_, err := l.db.Exec(ctx, `
		INSERT INTO dead_letter_events (id, event_id, tenant_id, version, stage, doc_id,
		                                content_hash, occurred_at, payload, payload_raw,
		                                reason_code, reason_message)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (event_id) WHERE event_id IS NOT NULL DO NOTHING`,
		uuid.New(), nullIfNil(event.EventID), nullIfNil(event.TenantID),
		nullIfEmpty(event.Version), nullIfEmpty(string(event.Stage)), nullIfEmpty(event.DocID),
		nullIfEmpty(event.ContentHash), nullIfNilTime(event.OccurredAt),
		payloadJSON, payloadRaw, reasonCode(cause), cause.Error())
	return err
}

// ── Проверка события ────────────────────────────────────────────────────────

// validateLakeEvent — проверка перед записью. Здесь и только здесь решается,
// что событие годно к lake; §4 называет версионирование схем правилом
// безопасности, и правило, которое не проверяется, не защищает.
//
// Проверка составная: инварианты самого события (domain.Event.Validate) плюс
// соответствие payload объявленной версии. Вторая часть и есть то, чего нет в
// Validate: версия может быть syntactically корректной («raw.v7»), а payload
// при этом читаться не будет.
func validateLakeEvent(event domain.Event) error {
	if err := event.Validate(); err != nil {
		return err
	}
	return validatePayloadVersion(event.Version, event.Payload)
}

// payloadValidators — проверка тела по версии схемы.
//
// Карта, а не switch: следующая версия (raw.v2) добавляется сюда строкой, и
// §8 («новая версия — новый потребитель, а не правка чужого кода») требует
// именно такого порядка. Версии, которой здесь нет, не «проходит на всякий
// случай» — она отправляется в очередь мёртвых, потому что читатель не знает,
// как её разбирать, и молча проглотить её значило бы потерять событие.
var payloadValidators = map[string]func(json.RawMessage) error{
	domain.RawEventVersion: validateRawV1Payload,
}

// validatePayloadVersion проверяет payload по его версии.
func validatePayloadVersion(version string, payload json.RawMessage) error {
	validate, ok := payloadValidators[version]
	if !ok {
		return domain.InvalidMetadata(fmt.Sprintf(
			"версия схемы события %q не поддерживается: lake умеет %s",
			version, strings.Join(supportedVersions(), ", ")))
	}
	if len(payload) == 0 {
		return domain.InvalidMetadata("event.payload пуст: событие без тела восстановить нельзя")
	}
	if !json.Valid(payload) {
		// Сюда попадает только мусор, доехавший мимо конвейера: lake получает
		// уже разобранный json.RawMessage, и невалидный JSON означает, что
		// событие собрали в обход конверта.
		return domain.InvalidMetadata("event.payload не является корректным JSON")
	}
	return validate(payload)
}

// supportedVersions перечисляет версии, которые lake умеет разбирать.
func supportedVersions() []string {
	versions := make([]string, 0, len(payloadValidators))
	for version := range payloadValidators {
		versions = append(versions, version)
	}
	sortStrings(versions)
	return versions
}

// validateRawV1Payload проверяет тело версии raw.v1: JSON-объект.
//
// Объект, а не массив и не скаляр, потому что конверт §4 — это набор полей
// (doc_id, tenant_id, source, kind, title, body, ...), и потребитель стадии
// enrich читает их по имени. Тело другой формы нельзя было бы отличить от
// «стадия ещё не наполнила его», и оно ушло бы дальше по конвейеру как
// готовый документ.
func validateRawV1Payload(payload json.RawMessage) error {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(payload, &probe); err != nil {
		return domain.InvalidMetadata("event.payload версии raw.v1 должен быть JSON-объектом: " + err.Error())
	}
	return nil
}

// reasonCode выделяет машинночитаемый код причины из доменной ошибки.
//
// Код берётся из кода AppError, а не из текста: текст меняется вместе с
// формулировкой, и очередь, разбираемая запросами по коду, рассыпалась бы
// при первом же рефакторинге сообщения.
func reasonCode(err error) string {
	var appErr *domain.AppError
	if errors.As(err, &appErr) {
		return appErr.Code
	}
	return "LAKE_REJECTED"
}

// ── Чтение (EventStore) ─────────────────────────────────────────────────────

// Read читает события lake по EventQuery.
//
// Порядок чтения задан в порту и здесь не переопределяется: occurred_at, а при
// равенстве event_id (§8). Детерминизм — не удобство, а условие
// воспроизводимости: §9 требует пересобрать индекс из lake целиком, и два
// прогона пересборки обязаны получить одни и те же события в одном порядке
// при любом параллелизме записи. Порядок по одному occurred_at для этого
// недостаточно — метки времени совпадают у событий одной стадии пачки.
func (l *Lake) Read(ctx context.Context, q domain.EventQuery) ([]domain.Event, error) {
	if q.TenantID == uuid.Nil {
		return nil, domain.InvalidMetadata("EventQuery.tenant_id обязателен: чтение lake без тенанта вернуло бы чужие события")
	}
	if q.Stage != "" && !q.Stage.Valid() {
		return nil, domain.InvalidMetadata(fmt.Sprintf(
			"EventQuery.stage %q не известен: допустимы %v", q.Stage, domain.AllStages()))
	}

	// Курсор разбирается до похода в базу: битый курсор должен падать на
	// вызывающем, а не после того, как база вернёт первую страницу.
	cursor, err := parseLakeCursor(q.Cursor)
	if err != nil {
		return nil, err
	}

	limit := q.Limit
	if limit <= 0 {
		limit = defaultLakeLimit
	}
	if limit > maxLakeLimit {
		// Потолок защищает не от лени, а от восстановления: пересборка индекса
		// читает lake постранично, и запрос «просто всё» без предела утащил бы
		// в память процесса столько событий, сколько накопилось за месяцы.
		limit = maxLakeLimit
	}

	query := `
		SELECT event_id, version, tenant_id, stage, doc_id, content_hash, occurred_at, payload
		FROM raw_events
		WHERE tenant_id = $1`
	args := []any{q.TenantID}
	next := 2

	if q.Stage != "" {
		args = append(args, string(q.Stage))
		query += fmt.Sprintf("\n\t\t  AND stage = $%d", next)
		next++
	}
	if q.DocID != "" {
		args = append(args, q.DocID)
		query += fmt.Sprintf("\n\t\t  AND doc_id = $%d", next)
		next++
	}
	if !q.Since.IsZero() {
		args = append(args, q.Since)
		query += fmt.Sprintf("\n\t\t  AND occurred_at >= $%d", next)
		next++
	}
	if cursor != nil {
		// Сравнение парой значений, а не «occurred_at > X»: по одной метке
		// времени страница теряла бы события, попавшие в неё на той же
		// микросекунде, — то есть молча, без признака пропуска.
		args = append(args, cursor.occurredAt, cursor.eventID)
		query += fmt.Sprintf("\n\t\t  AND (occurred_at, event_id) > ($%d, $%d)", next, next+1)
		next += 2
	}

	args = append(args, limit)
	query += fmt.Sprintf("\n\t\tORDER BY occurred_at, event_id\n\t\tLIMIT $%d", next)

	rows, err := l.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}

	events := make([]domain.Event, 0, limit)
	err = collect(rows, func() error {
		var (
			event       domain.Event
			stage       string
			payload     []byte
			contentHash string
		)
		if err := rows.Scan(&event.EventID, &event.Version, &event.TenantID, &stage,
			&event.DocID, &contentHash, &event.OccurredAt, &payload); err != nil {
			return err
		}
		event.Stage = domain.Stage(stage)
		event.ContentHash = contentHash
		event.Payload = payload
		// Событие, прочитанное из lake, проверяется на выходе так же, как
		// проверялось на входе. Расхождение означает, что строку в обход
		// конвейера кто-то всё же записал, и вернуть её потребителю значило
		// бы отдать битый payload дальше по конвейеру.
		if err := validateLakeEvent(event); err != nil {
			return domain.Internal("Событие lake не проходит проверку версии: пересборка остановлена").WithCause(err)
		}
		events = append(events, event)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return events, nil
}

// Границы постраничного чтения.
const (
	// defaultLakeLimit — страница по умолчанию. Пересборка индекса (§9)
	// читает lake целиком, и читает его постранично: страница должна быть
	// достаточно большой, чтобы не превратить пересборку в миллион запросов,
	// и достаточно маленькой, чтобы уместиться в памяти вместе с собранным
	// индексом.
	defaultLakeLimit = 500
	// maxLakeLimit — потолок для Limit, заданного вызывающим.
	maxLakeLimit = 5000
)

// Ping проверяет связность с базой: §9 требует, чтобы пересборка индекса из
// lake была проверяемой, и первым делом этой проверки является «lake вообще
// доступен».
func (l *Lake) Ping(ctx context.Context) error { return l.db.Ping(ctx) }

// Close закрывает lake. Пул принадлежит приложению, и закрывает его вызывающий
// вместе с остальными адаптерами: закрывать здесь означало бы отнять у API
// соединение, которым тот ещё пользуется.
func (l *Lake) Close() error { return nil }

// ── Курсор постраничного чтения ─────────────────────────────────────────────

// lakeCursor — положение в ленте lake: последнее прочитанное событие.
// Следующая страница начинается строго после него.
type lakeCursor struct {
	occurredAt time.Time
	eventID    uuid.UUID
}

// Формат курсора — «метка|event_id», обе части в базе 64 и 36. Разделитель
// выбран потому, что не встречается ни в RFC3339, ни в UUID.
//
// Курсор непрозрачен для вызывающего: это положение в ленте, а не параметры
// запроса. Отдавать вызывающему offset означало бы две вещи: он смог бы
// подставлять в чтение произвольные значения, и пересборка индекса сломалась
// бы при любой вставке в начало ленты. Keyset-курсор переживает и то и
// другое: вставка в начало не сдвигает уже прочитанное, и подделать его
// нечем — положение проверяется при разборе, а не применяется как есть.
const lakeCursorSeparator = "|"

// encodeLakeCursor кодирует положение ленты в строку курсора.
func encodeLakeCursor(event domain.Event) string {
	return fmt.Sprintf("%s%s%s",
		event.OccurredAt.UTC().Format(time.RFC3339Nano), lakeCursorSeparator, event.EventID)
}

// parseLakeCursor разбирает курсор. Пустая строка — начало ленты, это не ошибка.
func parseLakeCursor(cursor string) (*lakeCursor, error) {
	if cursor == "" {
		return nil, nil
	}
	mark, rawID, ok := strings.Cut(cursor, lakeCursorSeparator)
	if !ok {
		return nil, domain.InvalidMetadata("курсор lake повреждён: ожидался формат «метка|event_id»")
	}
	occurredAt, err := time.Parse(time.RFC3339Nano, mark)
	if err != nil {
		return nil, domain.InvalidMetadata("курсор lake повреждён: метка времени не разбирается")
	}
	eventID, err := uuid.Parse(rawID)
	if err != nil {
		return nil, domain.InvalidMetadata("курсор lake повреждён: event_id не разбирается")
	}
	if eventID == uuid.Nil {
		return nil, domain.InvalidMetadata("курсор lake повреждён: пустой event_id")
	}
	return &lakeCursor{occurredAt: occurredAt.UTC(), eventID: eventID}, nil
}

// ── Мелкие помощники ────────────────────────────────────────────────────────

// nullIfNil превращает нулевой UUID в NULL: колонка события в очереди мёртвых
// nullable, потому что туда попадает и то, что разобрать не удалось.
func nullIfNil(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

// nullIfNilTime — то же для времени. Отдельный помощник, а не общий с
// nullIfNil: у времени «нулевое» значение — это year 1, и подставить его в
// колонку события значило бы записать дату, которой не было.
func nullIfNilTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// sortStrings сортирует строки на месте.
func sortStrings(values []string) { slices.Sort(values) }
