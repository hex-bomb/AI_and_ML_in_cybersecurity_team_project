package indexing_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	domain "github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
)

// Права от конверта до чанка: последнее звено §6 перед индексом.
//
// Сторона индекса (store/es/index_body.go) и сторона запроса (acl_filter.go)
// уже проверены своими тестами, но обе они принимают chunk.Rights на веру. Если
// сценарий индексации собирает чанк без прав, контракт рвётся посередине: поля
// space_id и acl пишутся, но пустыми, и документ не находит НИКОГО — включая
// тех, кому он принадлежит. Тесты ниже закрывают именно этот разрыв.
//
// Кластера не требуют: проверяется форма чанка, с которой уходит UpsertChunks.

// rightsHarness — сценарий индексации, у которого разобранный документ несёт
// указанные метаданные. Всё остальное — те же подделки, что и в handler_test.go.
//
// Нарезка мелкая (chunksCfg): эталонный документ должен дать больше одного
// чанка. Права разрешаются один раз на документ, и расхождение между чанками
// заметно только там, где чанков больше одного.
var chunksCfg = testConfig(8, 2)

// aclTenant — тенант эталонного документа (document() в chunker_test.go).
// Права документа от тенанта не зависят, но AccessScope — величина осмысленная
// только вместе со своим тенантом, как её строит оракул (ResolveScope).
const aclTenant = "11111111-1111-4111-8111-111111111111"

func rightsHarness(t *testing.T, meta map[string]any) *harness {
	t.Helper()
	h := newHarness(t, chunksCfg)
	h.documents.parsed.Metadata = meta
	return h
}

// indexedChunks прогоняет стадию и возвращает чанки, ушедшие в индекс.
func indexedChunks(t *testing.T, h *harness) []domain.Chunk {
	t.Helper()
	if err := h.indexer.Index(context.Background(), h.lease); err != nil {
		t.Fatalf("Index: %v", err)
	}
	if len(h.index.chunks) == 0 {
		t.Fatal("в индекс не ушло ни одного чанка")
	}
	return h.index.chunks
}

// TestЧанкНесётКонтейнерДокумента — §6, «права висят на контейнере».
//
// Контейнер без исключений — самый частый случай и самая частая ошибка при
// нарезке: список исключений пуст, и кажется, что «прав нет, писать нечего».
// Но поле space_id в таком случае обязано быть заполнено — именно по нему
// документ видят те, кому доступен контейнер.
func TestЧанкНесётКонтейнерДокумента(t *testing.T) {
	h := rightsHarness(t, map[string]any{"space_id": "space-platform"})

	for n, chunk := range indexedChunks(t, h) {
		if chunk.Rights.SpaceID != "space-platform" {
			t.Errorf("чанк %d: контейнер %q вместо space-platform", n, chunk.Rights.SpaceID)
		}
		if terms := chunk.Rights.ExceptionTerms(); len(terms) != 0 {
			t.Errorf("чанк %d: документ без исключений получил acl = %v", n, terms)
		}
	}
}

// TestЧанкНесётКонтейнерИТочечныеИсключения — полная форма §6: контейнер плюс
// список принципалов, которым документ открыт в обход контейнера.
func TestЧанкНесётКонтейнерИТочечныеИсключения(t *testing.T) {
	h := rightsHarness(t, map[string]any{
		"space_id": "space-platform",
		"acl":      []any{"group:team-backend", "user:anna"},
	})

	want := []string{"group:team-backend", "user:anna"}
	for n, chunk := range indexedChunks(t, h) {
		if chunk.Rights.SpaceID != "space-platform" {
			t.Errorf("чанк %d: контейнер %q вместо space-platform", n, chunk.Rights.SpaceID)
		}
		if got := chunk.Rights.ExceptionTerms(); !slices.Equal(got, want) {
			t.Errorf("чанк %d: исключения %#v, ожидалось %#v", n, got, want)
		}
	}
}

// TestУЛичнойЗаметкиНетКонтейнера — §6 дословно: «Права личных заметок:
// acl = [user:<id>] и больше никто».
//
// Контейнер у заметки отсутствует, и это не потеря: у документа без контейнера
// и без исключений прав нет вообще. Проверяется и форма, и то, что заметку
// видит только автор — сосед с правами на весь тенант её не получает.
func TestУЛичнойЗаметкиНетКонтейнера(t *testing.T) {
	h := rightsHarness(t, map[string]any{"acl": []any{"user:anna"}})

	var rights domain.DocumentRights
	for n, chunk := range indexedChunks(t, h) {
		if chunk.Rights.SpaceID != "" {
			t.Errorf("чанк %d: личная заметка получила контейнер %q", n, chunk.Rights.SpaceID)
		}
		if got := chunk.Rights.ExceptionTerms(); !slices.Equal(got, []string{"user:anna"}) {
			t.Errorf("чанк %d: исключения %#v, ожидалось [user:anna]", n, got)
		}
		rights = chunk.Rights
	}

	owner := domain.AccessScope{TenantID: h.lease.TenantID.String(), KBID: "kb-1",
		Principals: []string{"user:anna"}}
	if !owner.Grants(rights) {
		t.Error("автор не видит собственную заметку")
	}
	stranger := domain.AccessScope{TenantID: h.lease.TenantID.String(), KBID: "kb-1",
		SpaceIDs:   []string{"space-platform", "space-hr"},
		Principals: []string{"user:petya", "role:admin", "group:team-backend"}}
	if stranger.Grants(rights) {
		t.Error("личная заметка видна постороннему с полными правами на тенант")
	}
}

// TestИндексацияБезПравДаётЧанкБезПрав — fail-closed на стадии индексации.
//
// Документ, чьи права не доехали, обязан быть невидим для всех, включая
// пользователя с полными правами на тенант: terms по ненаписанному полю не
// совпадает ни с чем. Это не «документ без прав», это «отказ в доступе» —
// и разница между ними в том, что отказ не выглядит утечкой.
//
// Тест зеркалит TestЧанкБезПравНеВиденНиКому со стороны адаптера: тот
// проверяет, что пустой rights не открывается, этот — что сценарий действительно
// отдаёт пустой rights, когда прав нет. Стоит одна из двух половин — и тест
// второй стороны упадёт.
func TestИндексацияБезПравДаётЧанкБезПрав(t *testing.T) {
	for name, meta := range map[string]map[string]any{
		"метаданных нет": nil,
		"без полей прав": {"product": "portal"},
	} {
		t.Run(name, func(t *testing.T) {
			h := rightsHarness(t, meta)

			for n, chunk := range indexedChunks(t, h) {
				if chunk.Rights.SpaceID != "" {
					t.Errorf("чанк %d получил контейнер %q", n, chunk.Rights.SpaceID)
				}
				if len(chunk.Rights.ExceptionTerms()) != 0 {
					t.Errorf("чанк %d получил исключения %v", n, chunk.Rights.ExceptionTerms())
				}
			}
		})
	}
}

// TestПраваНеРасширяютсяНиНаОдномЧанке — главная регрессия этого файла.
//
// Правило одно и проверяется для всех видов документа: пользователь, у которого
// нет НИ контейнера документа, НИ одного из его принципалов, не должен видеть
// ни одного его чанка. Нарушение этого правила — утечка, и оно не падает: чанк
// уезжает в индекс законно, с корректным chunk_id и правильным профилем, просто
// с лишним доступом. Заметить его можно только таким чеком.
//
// Особо опасен случай «почти fail-closed»: права разрешаются один раз на
// документ, и ошибка в этом месте дала бы чанк с ПУСТЫМИ правами посреди
// документа — то есть документ, видимый ровно половиной своего текста.
func TestПраваНеРасширяютсяНиНаОдномЧанке(t *testing.T) {
	cases := map[string]struct {
		meta      map[string]any
		wantSpace string
		wantACL   []string
	}{
		"контейнер": {
			meta:      map[string]any{"space_id": "space-hr"},
			wantSpace: "space-hr",
		},
		"контейнер с исключением": {
			meta:      map[string]any{"space_id": "space-hr", "acl": []any{"user:anna"}},
			wantSpace: "space-hr",
			wantACL:   []string{"user:anna"},
		},
		"личная заметка": {
			meta:    map[string]any{"acl": []any{"user:anna"}},
			wantACL: []string{"user:anna"},
		},
		"прав нет": {},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := rightsHarness(t, tc.meta)

			// Сосед: права максимальные, но ни контейнера документа, ни его
			// принципалов у него нет. Админ и участник чужой группы — чтобы
			// «роль:admin» сама по себе не считалась доступом к документу.
			stranger := domain.AccessScope{
				TenantID:   h.lease.TenantID.String(),
				KBID:       "kb-1",
				SpaceIDs:   []string{"space-platform", "space-finance"},
				Principals: []string{"user:petya", "role:admin", "group:team-sales"},
			}

			chunks := indexedChunks(t, h)
			if len(chunks) < 2 {
				t.Fatalf("документ дал %d чанк(а): расхождение прав невозможно", len(chunks))
			}

			for n, chunk := range chunks {
				if chunk.Rights.SpaceID != tc.wantSpace {
					t.Errorf("чанк %d: контейнер %q вместо %q", n, chunk.Rights.SpaceID, tc.wantSpace)
				}
				if got := chunk.Rights.ExceptionTerms(); !slices.Equal(got, tc.wantACL) {
					t.Errorf("чанк %d: исключения %#v вместо %#v", n, got, tc.wantACL)
				}
				if stranger.Grants(chunk.Rights) {
					t.Errorf("чанк %d виден постороннему: %+v", n, chunk.Rights)
				}
			}
		})
	}
}

// TestНеверныеПраваОстанавливаютИндексацию — форма прав проверяется на входе,
// и ошибка обязана дойти до оператора, а не превратиться в пустой чанк.
//
// Молча выброшенные права означают документ в статусе ready, которого не находит
// поиск: оператор видит «успешно обработанный документ» и не имеет причины
// жаловаться. Индекс при этом не должен быть тронут — версия не активна, и
// удалять нечего.
func TestНеверныеПраваОстанавливаютИндексацию(t *testing.T) {
	for name, meta := range map[string]map[string]any{
		"контейнер не строка":   {"space_id": 42},
		"acl не список":         {"acl": "user:anna"},
		"acl — список не строк": {"acl": []any{"user:anna", 7}},
	} {
		t.Run(name, func(t *testing.T) {
			h := rightsHarness(t, meta)

			err := h.indexer.Index(context.Background(), h.lease)
			if err == nil {
				t.Fatal("ожидался отказ по неверной форме прав")
			}
			appErr, ok := domain.AsAppError(err)
			if !ok || appErr.Code != domain.CodeInvalidMetadata {
				t.Fatalf("получено %v вместо INVALID_METADATA", err)
			}
			if len(h.index.calls) != 0 {
				t.Errorf("индекс затронут при отказе по правам: %v", h.index.calls)
			}
			if h.documents.activated || h.jobs.finishCalls != 0 {
				t.Error("версия активирована при отказе по правам")
			}
			// Права разбираются до эмбеддера: тратить вектора на документ,
			// который не уедет в индекс, незачем.
			if len(h.embedder.texts) != 0 {
				t.Error("эмбеддер вызван до проверки прав")
			}
		})
	}
}

// TestПраваДокументаНеРазъезжаютсяМеждуЧанками — все чанки одной версии делят
// один набор прав.
//
// Регрессия на «почти правильно»: если бы права читались из документа внутри
// цикла нарезки, один из нескольких чанков мог бы собраться с пустым набором.
// Пользователь увидел бы документ наполовину — и ни один тест этого не поймал,
// потому что каждый чанк по отдельности выглядит допустимым.
func TestПраваДокументаНеРазъезжаютсяМеждуЧанками(t *testing.T) {
	h := rightsHarness(t, map[string]any{
		"space_id": "space-platform",
		"acl":      []any{"user:anna"},
	})

	chunks := indexedChunks(t, h)
	want := []string{"user:anna"}
	for n, chunk := range chunks {
		if chunk.Rights.SpaceID != "space-platform" {
			t.Errorf("чанк %d: контейнер %q", n, chunk.Rights.SpaceID)
		}
		if got := chunk.Rights.ExceptionTerms(); !slices.Equal(got, want) {
			t.Errorf("чанк %d: исключения %#v вместо %#v", n, got, want)
		}
	}
}

// ── Серверный авторитет прав: documents.acl_override доезжает до индекса ────────
//
// Утечка, которую закрывают тесты ниже. Права документа приезжали в индекс
// только из конверта: и контейнер, и точечные исключения писал коннектор. Но
// администратор ставит ограничение отдельно, в documents.acl_override
// (SetDocumentACL), и до индекса оно не доезжало вообще. Документ, лично закрытый
// на anna и лежащий в контейнере space-hr, уезжал в индекс с space_id и был
// виден ВСЕМ в space-hr — ограничение администратора просто не участвовало в
// поиске.
//
// Ключевое свойство ES-фильтра, из-за которого это утечка, а не косметика:
// aclAccessClause строит should: [terms(space_id), terms(acl)] с
// minimum_should_match 1, то есть ИЛИ. Контейнер, оставленный рядом с
// ограничением, открывает документ всему контейнеру.

// overrideRights — ответ оракула для документа с ограничением администратора.
// Контейнера в нём нет намеренно: так выглядит серверный авторитет §6.
func overrideRights(terms ...string) domain.DocumentRights {
	return domain.DocumentRights{ACL: terms}
}

// rightsWithOverride прогоняет стадию индексации с заданным ответом оракула.
func rightsWithOverride(t *testing.T, meta map[string]any, oracle domain.DocumentRights) []domain.Chunk {
	t.Helper()
	h := rightsHarness(t, meta)
	h.documents.rights = &oracle
	return indexedChunks(t, h)
}

// TestПереопределениеАдминистратораЗакрываетКонтейнер — ГЛАВНАЯ регрессия.
//
// Клиент объявил контейнер space-hr, администратор ограничил документ до anna.
// Чанк обязан уехать с acl = [user:anna] и БЕЗ space_id: контейнер в ответе
// оракула означал бы, что весь space-hr видит документ.
//
// При старом поведении (конверт как единственный источник прав) тест падает на
// проверке space_id — индекс уезжал с объявленным клиентом контейнером.
func TestПереопределениеАдминистратораЗакрываетКонтейнер(t *testing.T) {
	chunks := rightsWithOverride(t,
		map[string]any{"space_id": "space-hr"},
		overrideRights("user:anna"))

	if len(chunks) < 2 {
		t.Fatalf("документ дал %d чанк(а): расхождение прав невозможно", len(chunks))
	}
	for n, chunk := range chunks {
		if chunk.Rights.SpaceID != "" {
			t.Errorf("чанк %d сохранил контейнер %q: объявленный контейнер обходит "+
				"ограничение администратора (фильтр индекса — ИЛИ)", n, chunk.Rights.SpaceID)
		}
		if got := chunk.Rights.ExceptionTerms(); !slices.Equal(got, []string{"user:anna"}) {
			t.Errorf("чанк %d: исключения %#v вместо [user:anna]", n, got)
		}
	}
}

// TestОбъявленноеНеРасширяетОграничениеАдминистратора — клиент не может ни
// добавить контейнер, ни дописать своих людей к ограничению администратора.
//
// Второе не менее важно первого: если бы объявленные исключения сохранялись, то
// загрузка документа в контейнер, куда у клиента нет прав, дала бы ему доступ к
// audit-документу плюс выглядела бы легитимно.
func TestОбъявленноеНеРасширяетОграничениеАдминистратора(t *testing.T) {
	chunks := rightsWithOverride(t, map[string]any{
		"space_id": "space-hr",
		"acl":      []any{"user:mallory", "group:team-audit"},
	}, overrideRights("user:anna"))

	for n, chunk := range chunks {
		got := chunk.Rights.ExceptionTerms()
		if !slices.Equal(got, []string{"user:anna"}) {
			t.Errorf("чанк %d: исключения %#v — объявленные клиентом люди добавлены "+
				"к ограничению администратора", n, got)
		}
		if chunk.Rights.SpaceID != "" {
			t.Errorf("чанк %d сохранил объявленный контейнер %q", n, chunk.Rights.SpaceID)
		}

		// И правило §6 целиком: сосед с правом на весь контейнер не получает
		// документ, а anna получает.
		neighbour := domain.AccessScope{
			TenantID:   aclTenant,
			SpaceIDs:   []string{"space-hr", "space-finance"},
			Principals: []string{"user:petya", "user:mallory", "group:team-audit", "role:admin"},
		}
		if neighbour.Grants(chunk.Rights) {
			t.Errorf("чанк %d виден соседу по контейнеру: %+v", n, chunk.Rights)
		}
		anna := domain.AccessScope{TenantID: aclTenant, Principals: []string{"user:anna"}}
		if !anna.Grants(chunk.Rights) {
			t.Errorf("чанк %d не виден тому, кому ограничение выдано: %+v", n, chunk.Rights)
		}
	}
}

// TestПустоеПереопределениеЗакрываетДокументВсем — пустой acl_override по коду
// accessFilterSQL означает «закрыто для всех, кроме администратора». Индекс не
// различает администратора и не должен: документ без контейнера и без
// исключений не виден НИКОМУ.
//
// Особенно важно, что объявленный контейнер при этом отбрасывается: иначе
// «закрытый лично» документ вернулся бы всему контейнеру.
func TestПустоеПереопределениеЗакрываетДокументВсем(t *testing.T) {
	chunks := rightsWithOverride(t,
		map[string]any{"space_id": "space-hr"},
		overrideRights())

	for n, chunk := range chunks {
		if chunk.Rights.SpaceID != "" {
			t.Errorf("чанк %d сохранил контейнер %q при пустом переопределении", n, chunk.Rights.SpaceID)
		}
		if len(chunk.Rights.ExceptionTerms()) != 0 {
			t.Errorf("чанк %d получил исключения %v при пустом переопределении",
				n, chunk.Rights.ExceptionTerms())
		}
		everyone := domain.AccessScope{
			TenantID:   aclTenant,
			SpaceIDs:   []string{"space-hr", "space-platform"},
			Principals: []string{"user:anna", "user:petya", "role:admin", "group:team-backend"},
		}
		if everyone.Grants(chunk.Rights) {
			t.Errorf("чанк %d, закрытый пустым переопределением, виден постороннему: %+v",
				n, chunk.Rights)
		}
	}
}

// TestНаследованиеСохраняетОбъявленныйКонтейнер — путь конверта не сломан.
//
// Ответ оракула «наследование» означает, что серверного ограничения нет, и
// контейнер из конверта остаётся единственным источником. Это обычный путь
// бо́льшей части документов, и его поломка была бы не утечкой, а обратной
// ошибкой — потерей прав у всех.
func TestНаследованиеСохраняетОбъявленныйКонтейнер(t *testing.T) {
	for name, tc := range map[string]struct {
		meta      map[string]any
		wantSpace string
		wantACL   []string
	}{
		"контейнер": {
			meta:      map[string]any{"space_id": "space-platform"},
			wantSpace: "space-platform",
		},
		"контейнер с исключением": {
			meta:      map[string]any{"space_id": "space-platform", "acl": []any{"user:anna"}},
			wantSpace: "space-platform",
			wantACL:   []string{"user:anna"},
		},
		"личная заметка": {
			meta:    map[string]any{"acl": []any{"user:anna"}},
			wantACL: []string{"user:anna"},
		},
		"прав нет": {},
	} {
		t.Run(name, func(t *testing.T) {
			chunks := rightsWithOverride(t, tc.meta,
				domain.DocumentRights{SpaceID: ports.InheritRights})
			for n, chunk := range chunks {
				if chunk.Rights.SpaceID != tc.wantSpace {
					t.Errorf("чанк %d: контейнер %q вместо %q", n, chunk.Rights.SpaceID, tc.wantSpace)
				}
				if got := chunk.Rights.ExceptionTerms(); !slices.Equal(got, tc.wantACL) {
					t.Errorf("чанк %d: исключения %#v вместо %#v", n, got, tc.wantACL)
				}
			}
		})
	}
}

// TestНеразрешённыеПраваОстанавливаютИндексацию — fail-closed на отказе оракула.
//
// Оракул не ответил: база недоступна, документа нет, тенант чужой. Молча взять
// объявленные конвертом права здесь нельзя — это ровно тот путь, который привёл к
// утечке. Стадия обязана остановиться: версия не активируется, индекс не
// трогается, и документ остаётся непроиндексированным, то есть невидимым.
func TestНеразрешённыеПраваОстанавливаютИндексацию(t *testing.T) {
	for name, err := range map[string]error{
		"база недоступна":       domain.ServiceUnavailable("PostgreSQL временно недоступен"),
		"документа нет":         domain.NotFound("Документ не найден"),
		"переопределение битое": domain.InvalidMetadata("acl_override документа не разбирается"),
	} {
		t.Run(name, func(t *testing.T) {
			h := rightsHarness(t, map[string]any{"space_id": "space-hr"})
			h.documents.rightsErr = err

			if got := h.indexer.Index(context.Background(), h.lease); !errors.Is(got, err) {
				t.Fatalf("Index = %v, ожидалась та же ошибка оракула", got)
			}
			if len(h.index.calls) != 0 {
				t.Errorf("индекс затронут при отказе оракула прав: %v", h.index.calls)
			}
			if h.documents.activated || h.jobs.finishCalls != 0 {
				t.Error("версия активирована при отказе оракула прав")
			}
			// Права читаются до эмбеддера: вектора на документ, который не
			// уедет в индекс, тратить незачем.
			if len(h.embedder.texts) != 0 {
				t.Error("эмбеддер вызван до разрешения прав")
			}
		})
	}
}

// TestОракулСпрашиваетсяОдинРазНаДокумент — права разрешаются один раз на
// документ, а не по разу на чанк.
//
// Чанков много, и запрос к базе на каждый чанк — это и лишняя нагрузка, и
// окно, в котором соседние чанки одного документа могут получить РАЗНЫЕ права:
// документ стал бы виден наполовину. Тест считает обращения, а не проверяет
// содержимое, потому что содержимое проверяют тесты выше.
func TestОракулСпрашиваетсяОдинРазНаДокумент(t *testing.T) {
	h := rightsHarness(t, map[string]any{"space_id": "space-platform"})

	chunks := indexedChunks(t, h)
	if len(chunks) < 2 {
		t.Fatalf("документ дал %d чанк(а)", len(chunks))
	}
	if h.documents.rightsCalls != 1 {
		t.Errorf("оракул прав спрошен %d раз для %d чанков: права разрешаются один раз на документ",
			h.documents.rightsCalls, len(chunks))
	}
}

// TestМаркерНаследованияНеПопадаетВИндекс — маркер оракула не является
// контейнером и не имеет права им стать.
//
// Если ports.InheritRights уедет в space_id, документ получит несуществующий
// контейнер: он не совпадёт ни с одним термом фильтра и станет невидим —
// fail-closed, но тихо сломанным. Проверяется на всех трёх состояниях оракула.
func TestМаркерНаследованияНеПопадаетВИндекс(t *testing.T) {
	cases := map[string]domain.DocumentRights{
		"наследование": {SpaceID: ports.InheritRights},
		"ограничение":  {ACL: []string{"user:anna"}},
		"закрыто":      {},
	}
	for name, oracle := range cases {
		t.Run(name, func(t *testing.T) {
			chunks := rightsWithOverride(t,
				map[string]any{"space_id": "space-hr", "acl": []any{"user:anna"}}, oracle)
			for n, chunk := range chunks {
				if strings.Contains(chunk.Rights.SpaceID, "\x00") {
					t.Errorf("чанк %d: в контейнер уехал служебный маркер %q",
						n, chunk.Rights.SpaceID)
				}
				for _, term := range chunk.Rights.ExceptionTerms() {
					if strings.Contains(term, "\x00") {
						t.Errorf("чанк %d: в исключения уехал служебный маркер %q", n, term)
					}
				}
			}
		})
	}
}

// TestКонтейнерОракулаНеПереносится — поле SpaceID в ответе оракула не является
// контейнером документа.
//
// Оно — либо маркер наследования, либо ошибка вызывающего, и оба случая значат
// «контейнера у документа нет». Контейнер при переопределении не наследуется
// никогда: иначе ИЛИ-фильтр отдаст документ всему контейнеру.
func TestКонтейнерОракулаНеПереносится(t *testing.T) {
	// Оракул ответил SpaceID, которого быть не должно: стадия всё равно обязана
	// отбросить контейнер и взять только принципалов.
	chunks := rightsWithOverride(t,
		map[string]any{"space_id": "space-hr"},
		domain.DocumentRights{SpaceID: "space-hr", ACL: []string{"user:anna"}})

	for n, chunk := range chunks {
		if chunk.Rights.SpaceID != "" {
			t.Errorf("чанк %d взял контейнер из ответа оракула: %q", n, chunk.Rights.SpaceID)
		}
		if got := chunk.Rights.ExceptionTerms(); !slices.Equal(got, []string{"user:anna"}) {
			t.Errorf("чанк %d: исключения %#v вместо [user:anna]", n, got)
		}
	}
}
