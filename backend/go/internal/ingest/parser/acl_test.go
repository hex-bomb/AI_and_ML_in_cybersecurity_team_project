package documents_test

import (
	"context"
	"slices"
	"testing"

	domain "github.com/kbsearch/kb/internal/envelope"
	documents "github.com/kbsearch/kb/internal/ingest/parser"
)

// Права конверта на пути ingest → index (§4, §6).
//
// Проверяется ровно то, что не проверяет ни один другой тест конвейера: что
// объявленные права ДОЕЗЖАЮТ до ParsedDocument в канонической форме, и что
// документ без объявленных прав уезжает невидимым, а не видимым всем.
// Кластера не требуют.

// aclRequest — запрос разбора с указанными метаданными.
func aclRequest(filename string, content []byte, meta map[string]any) documents.ConversionRequest {
	req := conversionRequest(filename)
	req.Metadata = meta
	return req
}

// convertWithMetadata разбирает файл и падает на любой ошибке.
func convertWithMetadata(
	t *testing.T, filename string, content []byte, meta map[string]any,
) domain.ParsedDocument {
	t.Helper()
	parsed, err := documents.Convert(
		context.Background(), content, aclRequest(filename, content, meta))
	if err != nil {
		t.Fatalf("Convert(%s): %v", filename, err)
	}
	return parsed
}

// TestКонвертНесётОбъявленныеПрава — права, объявленные во входе, обязаны
// доехать до ParsedDocument в том же виде, в каком их прочтёт стадия index.
//
// Формат файла значения не имеет: права едут в конверте, а не в тексте, и
// разбирать их заново для каждого MIME-типа значило бы завести четыре копии
// одного правила. Отсюда и одинаковый ожидаемый результат у MD, TXT, DOCX и
// PDF — расхождение было бы признаком того, что какой-то разбор потерял поля.
func TestКонвертНесётОбъявленныеПрава(t *testing.T) {
	files := map[string][]byte{
		"руководство.md":  []byte("# Раздел\n\nТекст документа.\n"),
		"заметка.txt":     []byte("Текст заметки.\n"),
		"инструкция.docx": buildDocx([]string{"<w:p><w:r><w:t>Текст документа.</w:t></w:r></w:p>"}),
		"отчёт.pdf":       buildPDF([]string{"Document body"}),
	}
	for name, content := range files {
		t.Run(name, func(t *testing.T) {
			parsed := convertWithMetadata(t, name, content, map[string]any{
				"space_id": "space-platform",
				"acl":      []any{"user:anna", "group:team-backend"},
			})

			rights, err := documents.RightsFromMetadata(parsed.Metadata)
			if err != nil {
				t.Fatalf("RightsFromMetadata: %v", err)
			}
			if rights.SpaceID != "space-platform" {
				t.Errorf("контейнер %q вместо space-platform", rights.SpaceID)
			}
			want := []string{"group:team-backend", "user:anna"}
			if got := rights.ExceptionTerms(); !slices.Equal(got, want) {
				t.Errorf("исключения %#v, ожидалось %#v", got, want)
			}
		})
	}
}

// TestКонвертБезПравНеВиденНиКому — fail-closed на входе конвейера.
//
// Документ, которому права не объявили, обязан уехать невидимым для всех, и
// в первую очередь для пользователя с полными правами на весь тенант. Если бы
// пустое значение превращалось в «доступ всем», то фаза A закрывалась бы утечкой
// каждого документа, чьи права не успели доехать, — то есть ровно на старте
// конвейера, когда они не доехали ни у одного.
func TestКонвертБезПравНеВиденНиКому(t *testing.T) {
	for name, meta := range map[string]map[string]any{
		"метаданных нет":          nil,
		"метаданные без прав":     {"product": "portal"},
		"только пробельные термы": {"acl": []any{"  ", ""}},
		"контейнер из пробелов":   {"space_id": "   "},
	} {
		t.Run(name, func(t *testing.T) {
			parsed := convertWithMetadata(t, "doc.md", []byte("Текст.\n"), meta)

			rights, err := documents.RightsFromMetadata(parsed.Metadata)
			if err != nil {
				t.Fatalf("RightsFromMetadata: %v", err)
			}
			// Пробельный контейнер и пробельные термы — это отсутствие прав,
			// а не права: иначе чанк получил бы терм, которому ни один запрос
			// не может совпасть по содержанию, и документ просто исчез бы из
			// поиска без причины.
			if rights.SpaceID != "" {
				t.Errorf("документ без прав получил контейнер %q", rights.SpaceID)
			}
			if len(rights.ExceptionTerms()) != 0 {
				t.Errorf("документ без прав получил исключения %v", rights.ExceptionTerms())
			}

			full := domain.AccessScopeFor(
				domain.Principal{
					TenantID: testTenantID,
					UserID:   "anna",
					Roles:    []string{"admin"},
					Groups:   []string{"team-backend"},
				},
				"kb-1", []string{"space-platform", "space-hr"})
			if full.Grants(rights) {
				t.Errorf("документ без прав виден пользователю с полными правами: %+v", rights)
			}
		})
	}
}

// TestЛичнаяЗаметкаВиднаТолькоАвтору — §6 дословно: «Права личных заметок:
// acl = [user:<id>] и больше никто».
//
// Контейнера у заметки нет, поэтому единственный способ её увидеть — собственный
// терм. Проверяется и положительный случай (автор видит), и отрицательный:
// сосед по тенанту с правами на всё остальное не должен её увидеть.
func TestЛичнаяЗаметкаВиднаТолькоАвтору(t *testing.T) {
	parsed := convertWithMetadata(t, "note.md", []byte("Мысли.\n"),
		map[string]any{"acl": []any{"user:anna"}})

	rights, err := documents.RightsFromMetadata(parsed.Metadata)
	if err != nil {
		t.Fatalf("RightsFromMetadata: %v", err)
	}
	if rights.SpaceID != "" {
		t.Errorf("личная заметка получила контейнер %q", rights.SpaceID)
	}
	if got := rights.ExceptionTerms(); !slices.Equal(got, []string{"user:anna"}) {
		t.Fatalf("исключения %#v, ожидалось [user:anna]", got)
	}

	owner := domain.AccessScope{TenantID: testTenantID.String(), KBID: testKBID.String(),
		Principals: []string{"user:anna"}}
	if !owner.Grants(rights) {
		t.Error("автор не видит собственную заметку")
	}

	for name, scope := range map[string]domain.AccessScope{
		"сосед с полными правами": {
			TenantID: testTenantID.String(), KBID: testKBID.String(),
			SpaceIDs:   []string{"space-platform", "space-hr"},
			Principals: []string{"user:petya", "role:admin", "group:team-backend"},
		},
		"сосед с правами на контейнер": {
			TenantID: testTenantID.String(), KBID: testKBID.String(),
			SpaceIDs:   []string{"space-hr"},
			Principals: []string{"user:petya"},
		},
		"аноним": {TenantID: testTenantID.String(), KBID: testKBID.String()},
	} {
		t.Run(name, func(t *testing.T) {
			if scope.Grants(rights) {
				t.Error("чужая личная заметка видна постороннему")
			}
		})
	}
}

// TestПустойAclНеОткрываетДокументВсем — регрессия на подмену «исключений нет».
//
// Права контейнера без исключений означают «виден тем, кому доступен контейнер»,
// и NOTHING ELSE. Если бы пустой список превращался в «прочий доступ всем» (так
// ведёт себя legacy ACL, где пустое значение — AllowsAny), то документ, чьи
// исключения не приехали, утекал бы всему тенанту. Словари DocumentRights и ACL
// различаются именно этим, и смешивать их нельзя.
func TestПустойAclНеОткрываетДокументВсем(t *testing.T) {
	parsed := convertWithMetadata(t, "doc.md", []byte("Текст.\n"),
		map[string]any{"space_id": "space-hr", "acl": []any{}})

	rights, err := documents.RightsFromMetadata(parsed.Metadata)
	if err != nil {
		t.Fatalf("RightsFromMetadata: %v", err)
	}
	if len(rights.ExceptionTerms()) != 0 {
		t.Errorf("документ без исключений получил acl = %v", rights.ExceptionTerms())
	}

	ownContainer := domain.AccessScope{TenantID: testTenantID.String(), KBID: testKBID.String(),
		SpaceIDs: []string{"space-hr"}}
	if !ownContainer.Grants(rights) {
		t.Error("владелец контейнера не видит свой документ — права сломались в другую сторону")
	}

	neighbour := domain.AccessScope{TenantID: testTenantID.String(), KBID: testKBID.String(),
		SpaceIDs: []string{"space-platform"}, Principals: []string{"user:anna"}}
	if neighbour.Grants(rights) {
		t.Error("документ открылся по правам чужого контейнера")
	}
}

// TestПраваНормализуютсяОдинРаз — parsed.json уходит на диск и читается
// повторно (§1: индекс перестраивается из lake), поэтому канон должен быть
// записан, а не вычисляться на каждом чтении по-разному.
//
// Порядок, пробелы и дубли не имеют значения по смыслу §6, но имеют значение
// для файла на диске: без канона два одинаковых по смыслу объявления дали бы
// разные diff'ы parsed.json при пересборке индекса.
func TestПраваНормализуютсяОдинРаз(t *testing.T) {
	meta := map[string]any{
		"space_id": "  space-hr  ",
		"acl":      []any{" user:petya ", "user:anna", "user:anna", "  "},
	}
	parsed := convertWithMetadata(t, "doc.md", []byte("Текст.\n"), meta)

	if parsed.Metadata["space_id"] != "space-hr" {
		t.Errorf("контейнер в конверте %v, ожидалось space-hr", parsed.Metadata["space_id"])
	}
	got, ok := parsed.Metadata["acl"].([]string)
	if !ok {
		t.Fatalf("acl в конверте %T, ожидался []string", parsed.Metadata["acl"])
	}
	want := []string{"user:anna", "user:petya"}
	if !slices.Equal(got, want) {
		t.Errorf("acl в конверте %#v, ожидалось %#v", got, want)
	}

	// Повторное чтение того же конверта обязано дать те же права: читатель и
	// писатель обязаны говорить об одном каноне.
	rights, err := documents.RightsFromMetadata(parsed.Metadata)
	if err != nil {
		t.Fatalf("RightsFromMetadata: %v", err)
	}
	if !slices.Equal(rights.ExceptionTerms(), want) {
		t.Errorf("повторное чтение дало %#v, ожидалось %#v", rights.ExceptionTerms(), want)
	}
}

// TestМетаданныеВызывающегоНеМутируются — разбор возвращает документ, который
// переживает вызывающего: он уезжает в JSON на диск и в индекс. Общая с
// вызывающим карта дала бы позднюю подмену прав уже записанного документа —
// то есть файл на диске и индекс разошлись бы по правам без всякой ошибки.
func TestМетаданныеВызывающегоНеМутируются(t *testing.T) {
	meta := map[string]any{
		"space_id": "  space-hr  ",
		"acl":      []any{" user:petya ", "user:anna"},
	}
	convertWithMetadata(t, "doc.md", []byte("Текст.\n"), meta)

	if meta["space_id"] != "  space-hr  " {
		t.Errorf("контейнер вызывающего изменён на %v", meta["space_id"])
	}
	acl, ok := meta["acl"].([]any)
	if !ok {
		t.Fatalf("acl вызывающего изменил тип: %T", meta["acl"])
	}
	if len(acl) != 2 || acl[0] != " user:petya " {
		t.Errorf("список исключений вызывающего изменён: %v", acl)
	}
}

// TestНевернаяФормаПравОстанавливаетРазбор — молча выбросить неверное поле
// прав нельзя ни в одну сторону.
//
// Потерянный контейнер делает документ невидимым (отказ, оператор не видит
// причины), потерянный список исключений делает его видимым всем, у кого есть
// права на контейнер (утечка). Оба исхода хуже явной ошибки, поэтому форма
// проверяется на входе, пока неверное значение ещё видно как ошибка ввода.
func TestНевернаяФормаПравОстанавливаетРазбор(t *testing.T) {
	for name, meta := range map[string]map[string]any{
		"контейнер не строка":   {"space_id": 42},
		"acl не список":         {"acl": "user:anna"},
		"acl — объект":          {"acl": map[string]any{"user": "anna"}},
		"acl — список не строк": {"acl": []any{"user:anna", 7}},
	} {
		t.Run(name, func(t *testing.T) {
			parsed, err := documents.Convert(context.Background(), []byte("Текст.\n"),
				aclRequest("doc.md", []byte("Текст.\n"), meta))
			if err == nil {
				t.Fatalf("ожидалась ошибка, разобран документ с метаданными %v", parsed.Metadata)
			}
			appErr, ok := domain.AsAppError(err)
			if !ok {
				t.Fatalf("ошибка не AppError: %v", err)
			}
			if appErr.Code != domain.CodeInvalidMetadata {
				t.Errorf("код %s вместо INVALID_METADATA", appErr.Code)
			}
			// Повтор не исправит: файл не станет годным от повторного разбора.
			if appErr.Retryable() {
				t.Errorf("код %s не должен повторяться", appErr.Code)
			}
			if len(parsed.Blocks) != 0 {
				t.Errorf("вместе с ошибкой возвращено %d блоков", len(parsed.Blocks))
			}
		})
	}
}

// TestОшибкаПравНеПовторяетсяИНеЧитаетЗначение — детали ошибки уходят в HTTP
// как есть, а значение поля пришло от клиента. Эхо чужой строки в ответе —
// лишний канал утечки, поэтому в деталях только имя поля и тип.
func TestОшибкаПравНеПовторяетсяИНеЧитаетЗначение(t *testing.T) {
	secret := "токен-секрет"
	_, err := documents.RightsFromMetadata(map[string]any{"space_id": []string{secret}})
	if err == nil {
		t.Fatal("ожидалась ошибка неверной формы")
	}
	appErr, ok := domain.AsAppError(err)
	if !ok {
		t.Fatalf("ошибка не AppError: %v", err)
	}
	if _, leaked := appErr.Details["value"]; leaked {
		t.Error("значение поля попало в детали ошибки")
	}
	if appErr.Details["field"] != "space_id" {
		t.Errorf("в деталях %v, ожидалось имя поля space_id", appErr.Details)
	}
	if got := appErr.Details["got"]; got != "[]string" {
		t.Errorf("в деталях тип %v, ожидался []string", got)
	}
}
