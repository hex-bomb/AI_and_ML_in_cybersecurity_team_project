package domain

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

const (
	eventTenantA = "22222222-2222-4222-8222-222222222222"
	eventTenantB = "33333333-3333-4333-8333-333333333333"
	eventDocID   = "sha256:2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae"
	// lakeEventNamespaceFixture repeats the namespace constant of event.go.
	// Он нужен как золотое значение: смена пространства имён тихо переименовала
	// бы каждое событие lake и сломала бы идемпотентность на живых данных.
	lakeEventNamespaceFixture = "5b394a91-0018-4eb6-87d7-e4371cbca47c"
)

func eventParams() NewEventParams {
	return NewEventParams{
		TenantID:    uuid.MustParse(eventTenantA),
		Stage:       EnrichStage,
		DocID:       eventDocID,
		ContentHash: "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		Payload:     json.RawMessage(`{"chunked":true}`),
		OccurredAt:  time.Date(2026, 10, 11, 9, 30, 0, 0, time.UTC),
	}
}

func mustNewEvent(t *testing.T, p NewEventParams) Event {
	t.Helper()
	event, err := NewEvent(p)
	if err != nil {
		t.Fatalf("NewEvent() вернул ошибку на корректных данных: %v", err)
	}
	return event
}

func TestEventValidate(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(e *Event)
		want   bool
	}{
		{"корректное событие", func(*Event) {}, false},
		{"пустая версия схемы", func(e *Event) { e.Version = "" }, true},
		{"версия из пробелов", func(e *Event) { e.Version = "   " }, true},
		{"пустой тенант", func(e *Event) { e.TenantID = uuid.Nil }, true},
		{"неизвестная стадия", func(e *Event) { e.Stage = Stage("publish") }, true},
		{"пустая стадия", func(e *Event) { e.Stage = "" }, true},
		// Стадия задания из job.go не стадия lake: одноимённая строка не
		// должна молча проходить проверку.
		{"стадия задания вместо стадии lake", func(e *Event) { e.Stage = Stage(StageConvert) }, true},
		{"стадия с регистром", func(e *Event) { e.Stage = Stage("Enrich") }, true},
		{"пустой doc_id", func(e *Event) { e.DocID = "" }, true},
		{"doc_id из пробелов", func(e *Event) { e.DocID = "  " }, true},
		{"пустой event_id", func(e *Event) { e.EventID = uuid.Nil }, true},
		// Хэш содержимого на стадии collect ещё не посчитан, и пустое
		// значение там законно: ключ идемпотентности на этом шаге пустой.
		{"пустой content_hash", func(e *Event) { e.ContentHash = "" }, false},
		{"пустой payload", func(e *Event) { e.Payload = nil }, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			event := mustNewEvent(t, eventParams())
			tc.mutate(&event)
			err := event.Validate()
			if tc.want && err == nil {
				t.Fatal("ожидалась ошибка, получено nil")
			}
			if !tc.want && err != nil {
				t.Fatalf("ожидался nil, получено %v", err)
			}
			if !tc.want {
				return
			}
			var appErr *AppError
			if !errors.As(err, &appErr) {
				t.Fatalf("ожидалась AppError, получено %T: %v", err, err)
			}
			if appErr.Code != CodeInvalidMetadata || appErr.Status != 422 {
				t.Errorf("код %s / статус %d, ожидались %s / 422",
					appErr.Code, appErr.Status, CodeInvalidMetadata)
			}
			if appErr.Message == "" {
				t.Error("сообщение об ошибке пустое: непонятно, какое поле отвергли")
			}
		})
	}
}

func TestEventValidateRejectsEventBuiltByHand(t *testing.T) {
	// Событие, собранное литералом без конструктора, обязано ломаться
	// проверкой: иначе невалидная запись дошла бы до lake молча.
	var zero Event
	if err := zero.Validate(); err == nil {
		t.Fatal("нулевое событие прошло Validate")
	}
}

// TestNewEventFillsVersionAndID: конструктор закрывает два самых частых
// способа сделать событие негодным — забытую версию схемы и случайный
// идентификатор вместо детерминированного.
func TestNewEventFillsVersionAndID(t *testing.T) {
	event := mustNewEvent(t, eventParams())

	if event.Version != RawEventVersion {
		t.Errorf("Version = %q, ожидалась %q", event.Version, RawEventVersion)
	}
	if event.EventID != EventIDFor(EventIDParams{
		Version:     RawEventVersion,
		TenantID:    uuid.MustParse(eventTenantA),
		Stage:       EnrichStage,
		DocID:       eventDocID,
		ContentHash: event.ContentHash,
	}) {
		t.Errorf("EventID = %s, ожидался детерминированный EventIDFor", event.EventID)
	}
	if err := event.Validate(); err != nil {
		t.Errorf("Validate() на собранном событии: %v", err)
	}
}

func TestNewEventAcceptsExplicitVersion(t *testing.T) {
	params := eventParams()
	params.Version = "normalized.v2"
	event := mustNewEvent(t, params)
	if event.Version != "normalized.v2" {
		t.Errorf("Version = %q, ожидалась явная normalized.v2", event.Version)
	}
	// Версия входит в имя события: тот же факт в новой схеме — другое
	// событие, иначе потребитель прочитал бы payload по чужой схеме.
	if event.EventID == mustNewEvent(t, eventParams()).EventID {
		t.Error("смена версии схемы не изменила event_id")
	}
}

func TestNewEventRejectsBadInput(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(p *NewEventParams)
	}{
		{"пустой тенант", func(p *NewEventParams) { p.TenantID = uuid.Nil }},
		{"неизвестная стадия", func(p *NewEventParams) { p.Stage = Stage("ship") }},
		{"пустой doc_id", func(p *NewEventParams) { p.DocID = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := eventParams()
			tc.mutate(&params)
			if _, err := NewEvent(params); err == nil {
				t.Fatal("ожидалась ошибка, получено nil")
			}
		})
	}
}

// TestEventIDIsDeterministic pins the idempotency rule of roadmap §4: повтор
// стадии по тем же входным данным обязан дать тот же event_id. Иначе lake
// получает две записи об одном факте при каждом ретрае, и пересборка индекса
// считает одни и те же данные дважды.
func TestEventIDIsDeterministic(t *testing.T) {
	base := EventIDParams{
		Version:     RawEventVersion,
		TenantID:    uuid.MustParse(eventTenantA),
		Stage:       IndexStage,
		DocID:       eventDocID,
		ContentHash: "sha256:aaaa",
	}

	if got, again := EventIDFor(base), EventIDFor(base); got != again {
		t.Fatalf("повторный расчёт дал другой id: %s != %s", got, again)
	}

	// Золотой id собран вручную из пространства и состава полей: так тест
	// ловит и смену пространства имён, и случайную перестановку полей в имени.
	want := uuid.NewSHA1(uuid.MustParse(lakeEventNamespaceFixture), []byte(
		RawEventVersion+"|"+eventTenantA+"|"+IndexStage+"|"+eventDocID+"|sha256:aaaa"))
	if got := EventIDFor(base); got != want {
		t.Errorf("EventIDFor() = %s, ожидался %s", got, want)
	}

	cases := []struct {
		name   string
		mutate func(p *EventIDParams)
	}{
		{"другая версия схемы", func(p *EventIDParams) { p.Version = "raw.v2" }},
		{"другой тенант", func(p *EventIDParams) { p.TenantID = uuid.MustParse(eventTenantB) }},
		{"другая стадия", func(p *EventIDParams) { p.Stage = DeriveStage }},
		{"другой документ", func(p *EventIDParams) { p.DocID = eventDocID + "9" }},
		{"другой хэш содержимого", func(p *EventIDParams) { p.ContentHash = "sha256:bbbb" }},
		{"пустой хэш содержимого", func(p *EventIDParams) { p.ContentHash = "" }},
		{"пустая версия", func(p *EventIDParams) { p.Version = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			other := base
			tc.mutate(&other)
			if EventIDFor(base) == EventIDFor(other) {
				t.Errorf("EventIDFor не изменился: %s", EventIDFor(base))
			}
		})
	}
}

// TestEventIDIgnoresOccurredAtAndPayload: метка времени и тело стадии не
// меняют факта «этот документ этой версии прошёл эту стадию». Если бы входили,
// каждый ретрай с другим временем рождал бы новую запись lake.
func TestEventIDIgnoresOccurredAtAndPayload(t *testing.T) {
	first := mustNewEvent(t, eventParams())
	params := eventParams()
	params.OccurredAt = params.OccurredAt.Add(72 * time.Hour)
	params.Payload = json.RawMessage(`{"chunked":false,"retry":3}`)
	second := mustNewEvent(t, params)

	if first.EventID != second.EventID {
		t.Errorf("повтор с другим временем и payload дал другой id: %s != %s",
			first.EventID, second.EventID)
	}
	if first.OccurredAt.Equal(second.OccurredAt) {
		t.Error("OccurredAt не изменился, а должен был: тест не проверяет то, что думает")
	}
}

// TestEventJSONRoundTrip: версия схемы переживает пересылку события. Потерянная
// версия ломает читателя payload, поэтому это проверяется явно, а не «на
// глаз» по совпадению остальных полей.
func TestEventJSONRoundTrip(t *testing.T) {
	event := mustNewEvent(t, eventParams())

	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("Marshal() вернул ошибку: %v", err)
	}

	var got Event
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal() вернул ошибку: %v", err)
	}

	if got.Version != RawEventVersion {
		t.Errorf("Version после круга = %q, ожидалась %q", got.Version, RawEventVersion)
	}
	if got.EventID != event.EventID || got.TenantID != event.TenantID {
		t.Errorf("идентификаторы не пережили JSON: %s/%s", got.EventID, got.TenantID)
	}
	if got.Stage != event.Stage || got.DocID != event.DocID || got.ContentHash != event.ContentHash {
		t.Errorf("поля события не совпали после круга: %+v", got)
	}
	if !got.OccurredAt.Equal(event.OccurredAt) {
		t.Errorf("OccurredAt = %s, ожидалось %s", got.OccurredAt, event.OccurredAt)
	}
	if string(got.Payload) != string(event.Payload) {
		t.Errorf("Payload = %s, ожидалось %s", got.Payload, event.Payload)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("событие после круга не проходит Validate: %v", err)
	}
}

func TestStageValid(t *testing.T) {
	for _, stage := range AllStages() {
		if !stage.Valid() {
			t.Errorf("стадия %q невалидна, хотя входит в конвейер", stage)
		}
		if stage.String() == "" {
			t.Errorf("стадия %q не имеет строкового имени", stage)
		}
	}

	// convert — стадия задания из job.go, а не стадия lake: одинаковое
	// название в двух наборах не делает стадии общей.
	invalid := []Stage{"", " ", "Collect", "collect ", Stage(StageConvert), "index_v2"}
	for _, stage := range invalid {
		if stage.Valid() {
			t.Errorf("стадия %q прошла проверку", stage)
		}
	}
}

// TestAllStagesMatchRoadmap: пять стадий §4 в порядке конвейера плюс
// надгробие удаления. Список открытый только на добавление новой стадии, но не
// на перестановку старых: перестановка тихо меняет смысл lake, собранного по
// старому порядку.
//
// Надгробие стоит последним не по времени появления, а потому, что оно не
// стадия прохода (разбор выбора стадии вместо флага — в event.go, у
// DeleteStage).
func TestAllStagesMatchRoadmap(t *testing.T) {
	want := []Stage{
		CollectStage, NormalizeStage, EnrichStage, IndexStage, DeriveStage, DeleteStage,
	}
	got := AllStages()
	if len(got) != len(want) {
		t.Fatalf("AllStages() = %v, ожидалось %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("AllStages()[%d] = %q, ожидалось %q", i, got[i], want[i])
		}
	}
	if CollectStage.String() != "collect" || IndexStage.String() != "index" {
		t.Errorf("строковые имена стадий не совпали с §4: %q / %q",
			CollectStage, IndexStage)
	}
}

// TestНадгробиеУдаленияДопустимоВКонверте — стадия delete проходит проверку
// события.
//
// Пока DeleteStage не был в AllStages, lake был непроницаем для удаления: CHECK
// ck_raw_events_stage допускал ровно пять стадий §4, и удаление не помещалось ни
// в одну, то есть пересборка индекса из lake была невозможна в принципе.
func TestНадгробиеУдаленияДопустимоВКонверте(t *testing.T) {
	if !DeleteStage.Valid() {
		t.Fatal("стадия delete невалидна: lake не примет надгробие удаления")
	}
	if DeleteStage.String() != "delete" {
		t.Errorf("строковое имя стадии delete = %q, ожидалось \"delete\"", DeleteStage)
	}

	event, err := NewEvent(NewEventParams{
		TenantID:    uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		Stage:       DeleteStage,
		DocID:       "doc-1",
		ContentHash: "job:0123456789abcdef0123456789abcdef",
		Payload:     json.RawMessage(`{"doc_id":"doc-1"}`),
		OccurredAt:  time.Now(),
	})
	if err != nil {
		t.Fatalf("NewEvent стадии delete: %v", err)
	}
	if err := event.Validate(); err != nil {
		t.Errorf("событие стадии delete не проходит Validate: %v", err)
	}

	// Надгробие — самостоятельный факт: его event_id обязан отличаться от
	// event_id события index той же версии, иначе lake не смогла бы отличить
	// «проиндексирован» от «удалён».
	indexed, err := NewEvent(NewEventParams{
		TenantID:    event.TenantID,
		Stage:       IndexStage,
		DocID:       "doc-1",
		ContentHash: event.ContentHash,
		Payload:     event.Payload,
		OccurredAt:  event.OccurredAt,
	})
	if err != nil {
		t.Fatalf("NewEvent стадии index: %v", err)
	}
	if indexed.EventID == event.EventID {
		t.Error("надгробие и событие index получили один event_id: lake не различит их")
	}
}

// TestСтадияЗаданияDeleteСовпадаетСоСтадиейLakeDelete — две системы называют
// один факт одинаково.
//
// Задание стадии delete (job.go) и событие стадии delete (event.go) — две
// стороны одного удаления. Расхождение имён означало бы, что задание пишет в
// lake событие под другим именем, и читатель lake не узнает свой факт.
func TestСтадияЗаданияDeleteСовпадаетСоСтадиейLakeDelete(t *testing.T) {
	if StageDelete != DeleteStage.String() {
		t.Errorf("стадия задания %q и стадия lake %q разошлись", StageDelete, DeleteStage)
	}
	if !(JobLease{Stage: StageDelete}).IsDeleteStage() {
		t.Error("JobLease не распознаёт стадию delete")
	}
	if (JobLease{Stage: StageIndex}).IsDeleteStage() {
		t.Error("стадия index ошибочно распознана как delete")
	}
}

// TestAllStagesReturnsFreshSlice: список стадий не должен делиться между
// вызовами, иначе добавление шестой стадии одному потребителю изменило бы
// конвейер всем остальным.
func TestAllStagesReturnsFreshSlice(t *testing.T) {
	first := AllStages()
	first[0] = "мусор"

	if second := AllStages(); second[0] != CollectStage {
		t.Errorf("стадии делятся между вызовами: %q", second[0])
	}
}
