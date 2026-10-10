package documents_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kbsearch/backend-go/internal/application/documents"
	"github.com/kbsearch/backend-go/internal/domain"
)

// Виды блоков из ТЗ 3.2. Список закрытый: конвертер не придумывает новых,
// иначе потребитель ParsedDocument не сможет на них опираться.
const (
	kindHeading   = "heading"
	kindParagraph = "paragraph"
	kindList      = "list"
	kindCode      = "code"
)

var (
	testTenantID   = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	testKBID       = uuid.MustParse("22222222-2222-4222-8222-222222222222")
	testDocumentID = uuid.MustParse("33333333-3333-4333-8333-333333333333")
	testVersionID  = uuid.MustParse("44444444-4444-4444-8444-444444444444")
	testSourceURL  = "https://portal.example/guide/export"
)

func conversionRequest(filename string) documents.ConversionRequest {
	url := testSourceURL
	return documents.ConversionRequest{
		TenantID:   testTenantID,
		KBID:       testKBID,
		DocumentID: testDocumentID,
		VersionID:  testVersionID,
		Title:      "Инструкция по выгрузке",
		Filename:   filename,
		SourceURL:  &url,
		Metadata:   map[string]any{"product": "portal", "product_version": "4.2"},
	}
}

// convertOk — успешный разбор с падением теста на любой ошибке.
func convertOk(t *testing.T, filename string, content []byte) domain.ParsedDocument {
	t.Helper()
	parsed, err := documents.Convert(context.Background(), content, conversionRequest(filename))
	if err != nil {
		t.Fatalf("Convert(%s): %v", filename, err)
	}
	return parsed
}

// kinds собирает виды блоков по порядку — порядок и есть контракт разбора.
func kinds(blocks []domain.Block) []string {
	out := make([]string, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, b.Kind)
	}
	return out
}

// positionsAreSequential — position начинается с 0 и идёт подряд (ТЗ 3.2).
func positionsAreSequential(blocks []domain.Block) bool {
	for i, b := range blocks {
		if b.Position != i {
			return false
		}
	}
	return true
}

// ── Markdown ───────────────────────────────────────────────────────

// TestConvertMarkdownBuildsNestedSectionPath — вложенность заголовков задаёт
// section_path: без неё чанк нарезался бы по разделам, которых в документе нет.
func TestConvertMarkdownBuildsNestedSectionPath(t *testing.T) {
	source := `# Настройка

Вводный текст настроек.

## Выгрузка

Текст про выгрузку.

### Формат CSV

Текст про формат.
`
	parsed := convertOk(t, "export-guide.md", []byte(source))

	if len(parsed.Blocks) != 6 {
		t.Fatalf("получено %d блоков, ожидалось 6: %v", len(parsed.Blocks), kinds(parsed.Blocks))
	}
	want := [][]string{
		{"Настройка"}, {"Настройка"},
		{"Настройка", "Выгрузка"}, {"Настройка", "Выгрузка"},
		{"Настройка", "Выгрузка", "Формат CSV"}, {"Настройка", "Выгрузка", "Формат CSV"},
	}
	for i, block := range parsed.Blocks {
		if !slices.Equal(block.SectionPath, want[i]) {
			t.Errorf("блок %d (%s): section_path %v вместо %v",
				i, block.Kind, block.SectionPath, want[i])
		}
	}
}

// TestConvertMarkdownKeepsBlockOrder — заголовок, за ним текст: перестановка
// блоков переставила бы страницы документа в выдаче.
func TestConvertMarkdownKeepsBlockOrder(t *testing.T) {
	source := "# Раздел\n\nПервый абзац.\n\nВторой абзац.\n"
	parsed := convertOk(t, "doc.md", []byte(source))

	want := []string{kindHeading, kindParagraph, kindParagraph}
	if got := kinds(parsed.Blocks); !slices.Equal(got, want) {
		t.Errorf("порядок блоков %v вместо %v", got, want)
	}
	if !positionsAreSequential(parsed.Blocks) {
		t.Error("position не подряд с нуля")
	}
	if parsed.Blocks[1].Text != "Первый абзац." || parsed.Blocks[2].Text != "Второй абзац." {
		t.Errorf("тексты абзацев не совпали: %q, %q",
			parsed.Blocks[1].Text, parsed.Blocks[2].Text)
	}
}

// TestConvertMarkdownKeepsInlineFormattingAsText — разметка не должна
// просачиваться в текст блока: индекс ищет по словам, а не по `**`.
func TestConvertMarkdownKeepsInlineFormattingAsText(t *testing.T) {
	source := "# Заголовок\n\nТекст с **жирным** и `кодом`.\n"
	parsed := convertOk(t, "doc.md", []byte(source))

	if got := parsed.Blocks[1].Text; got != "Текст с жирным и кодом." {
		t.Errorf("текст блока %q, ожидался без разметки", got)
	}
}

// TestConvertMarkdownTableDriven — разбор MD по случаям: пустой документ,
// только заголовок, без текста под ним.
func TestConvertMarkdownTableDriven(t *testing.T) {
	for _, test := range []struct {
		name      string
		source    string
		wantCount int
		wantErr   bool
	}{
		{name: "пустой документ", source: "", wantErr: true},
		{name: "только пробелы", source: "   \n\n  \n", wantErr: true},
		{name: "только заголовок", source: "# Один заголовок\n", wantCount: 1},
		{name: "абзац без заголовка", source: "Текст без заголовка.\n", wantCount: 1},
		{
			name:      "перенос строки внутри абзаца",
			source:    "Первая строка\nвторая строка\n",
			wantCount: 1,
		},
		{
			name:      "пустой заголовок отбрасывается",
			source:    "#\n\nТекст.\n",
			wantCount: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := documents.Convert(
				context.Background(), []byte(test.source), conversionRequest("doc.md"))
			if test.wantErr {
				assertParseError(t, err, domain.CodeInvalidMetadata)
				return
			}
			if err != nil {
				t.Fatalf("Convert: %v", err)
			}
			if len(parsed.Blocks) != test.wantCount {
				t.Errorf("блоков %d вместо %d: %v",
					len(parsed.Blocks), test.wantCount, kinds(parsed.Blocks))
			}
		})
	}
}

// TestConvertMarkdownListsAndCodeBecomeText — списки и код не теряются:
// иначе инструкция в формате «шаг 1 / шаг 2» не попала бы в поиск.
func TestConvertMarkdownListsAndCodeBecomeText(t *testing.T) {
	source := "# Как настроить\n\n1. Откройте раздел\n2. Нажмите кнопку\n\n" +
		"```bash\ncurl -X POST https://portal.example\n```\n"
	parsed := convertOk(t, "doc.md", []byte(source))

	if !slices.Contains(kinds(parsed.Blocks), kindList) {
		t.Errorf("списка нет среди блоков %v", kinds(parsed.Blocks))
	}
	if !slices.Contains(kinds(parsed.Blocks), kindCode) {
		t.Errorf("блока кода нет среди блоков %v", kinds(parsed.Blocks))
	}
	var code string
	for _, b := range parsed.Blocks {
		if b.Kind == kindCode {
			code = b.Text
		}
	}
	if !strings.Contains(code, "curl -X POST") {
		t.Errorf("текст блока кода %q потерял содержимое", code)
	}
}

// ── Обычный текст ───────────────────────────────────────────────────

// TestConvertPlainTextSplitsOnBlankLines — абзацы разделяются пустой строкой;
// без разделения весь файл стал бы одним блоком и одним чанком.
func TestConvertPlainTextSplitsOnBlankLines(t *testing.T) {
	source := "Первый абзац.\nВторая строка того же абзаца.\n\nВторой абзац.\n\n\nТретий.\n"
	parsed := convertOk(t, "notes.txt", []byte(source))

	if len(parsed.Blocks) != 3 {
		t.Fatalf("блоков %d вместо 3: %v", len(parsed.Blocks), kinds(parsed.Blocks))
	}
	if parsed.Blocks[0].Text != "Первый абзац.\nВторая строка того же абзаца." {
		t.Errorf("внутренние переносы не сохранены: %q", parsed.Blocks[0].Text)
	}
	if parsed.Blocks[1].Text != "Второй абзац." || parsed.Blocks[2].Text != "Третий." {
		t.Errorf("тексты абзацев не совпали: %q, %q",
			parsed.Blocks[1].Text, parsed.Blocks[2].Text)
	}
	for i, b := range parsed.Blocks {
		if b.Kind != kindParagraph {
			t.Errorf("блок %d: вид %q вместо paragraph", i, b.Kind)
		}
		if len(b.SectionPath) != 0 {
			t.Errorf("блок %d: section_path %v, у .txt разделов нет", i, b.SectionPath)
		}
		if b.Page != nil {
			t.Errorf("блок %d: page %v, у .txt страниц нет", i, *b.Page)
		}
	}
}

// TestTxtIsSupportedFormat — .txt добавлен в список приёма и в проверку
// формата, иначе валидный текстовый файл отвергался бы заглушкой.
func TestTxtIsSupportedFormat(t *testing.T) {
	if !slices.Contains(documents.SupportedFormats, ".txt") {
		t.Errorf("SupportedFormats = %v, в списке нет .txt", documents.SupportedFormats)
	}
	if !documents.IsSupported("notes.txt") {
		t.Error("IsSupported отверг .txt")
	}
	suffix, err := documents.ValidateFormat(context.Background(), "notes.txt", []byte("текст"))
	if err != nil {
		t.Fatalf("ValidateFormat(.txt): %v", err)
	}
	if suffix != ".txt" {
		t.Errorf("суффикс %q вместо .txt", suffix)
	}
}

// TestTxtRejectsNonUTF8 — .txt проверяется так же, как .md: не-UTF-8 файл
// разобрался бы в мусор и молча ушёл в индекс.
func TestTxtRejectsNonUTF8(t *testing.T) {
	_, err := documents.ValidateFormat(
		context.Background(), "notes.txt", []byte{0xff, 0xfe, 0x00})
	assertParseError(t, err, domain.CodeInvalidMetadata)
}

// ── PDF ────────────────────────────────────────────────────────────

// TestConvertPdfSetsPageNumbers — страницы нумеруются с 1, как в ТЗ 3.2;
// с нуля ссылка на страницу указывала бы на предыдущую.
func TestConvertPdfSetsPageNumbers(t *testing.T) {
	raw := buildPDF([]string{"Page one: export setup", "Page two: CSV format", "Page three: limits"})
	requireReadablePDF(t, raw, 3)

	parsed := convertOk(t, "export-guide.pdf", raw)

	if len(parsed.Blocks) == 0 {
		t.Fatal("PDF разобран в ноль блоков")
	}
	seenPages := make(map[int]bool, len(parsed.Blocks))
	for i, b := range parsed.Blocks {
		if b.Page == nil {
			t.Fatalf("блок %d: page не задан", i)
		}
		if *b.Page < 1 {
			t.Errorf("блок %d: page %d, нумерация должна начинаться с 1", i, *b.Page)
		}
		seenPages[*b.Page] = true
	}
	if len(seenPages) != 3 {
		t.Errorf("страниц в блоках %v, ожидались все 3", seenPages)
	}
}

// TestConvertPdfKeepsPageOrder — страницы идут по возрастанию номера,
// иначе locator.page_start указывал бы назад по документу.
func TestConvertPdfKeepsPageOrder(t *testing.T) {
	raw := buildPDF([]string{"First page text", "Second page text"})
	requireReadablePDF(t, raw, 2)

	parsed := convertOk(t, "two-pages.pdf", raw)

	previous := 0
	for i, b := range parsed.Blocks {
		if b.Page == nil || *b.Page < previous {
			t.Fatalf("блок %d: page нарушает порядок страниц", i)
		}
		previous = *b.Page
	}
}

// TestConvertPdfWithoutTextIsRejected — PDF без извлекаемого текста (например,
// скан) обязан отклониться ошибкой, а не уйти в индекс пустым: иначе
// документ считался бы успешно обработанным и никогда не стал бы искаться.
func TestConvertPdfWithoutTextIsRejected(t *testing.T) {
	raw := buildPDF([]string{"", ""})
	requireReadablePDF(t, raw, 2)

	_, err := documents.Convert(context.Background(), raw, conversionRequest("scan.pdf"))
	assertParseError(t, err, domain.CodeInvalidMetadata)
}

// TestConvertPdfRejectsGarbage — файл с сигнатурой PDF, но не PDF: сообщение
// должно быть своё, а не падение разбора с невнятным текстом.
func TestConvertPdfRejectsGarbage(t *testing.T) {
	garbage := []byte("%PDF-1.4\nэто не структура PDF\n")
	_, err := documents.Convert(context.Background(), garbage, conversionRequest("broken.pdf"))
	if err == nil {
		t.Fatal("ожидался отказ на битом PDF")
	}
	appErr, ok := domain.AsAppError(err)
	if !ok {
		t.Fatalf("ошибка не AppError: %v", err)
	}
	if appErr.Retryable() {
		t.Errorf("битый PDF не должен повторяться: код %s", appErr.Code)
	}
}

// ── DOCX ───────────────────────────────────────────────────────────

// TestConvertDocxExtractsParagraphs — абзацы DOCX вытаскиваются из
// word/document.xml по границам <w:p>, текстовые прогоны <w:t> склеиваются.
func TestConvertDocxExtractsParagraphs(t *testing.T) {
	raw := buildDocx([]string{
		`<w:p><w:r><w:t>Первый абзац документа</w:t></w:r></w:p>`,
		`<w:p><w:r><w:t>Второй </w:t></w:r><w:r><w:t>абзац из двух прогонов</w:t></w:r></w:p>`,
	})

	parsed := convertOk(t, "manual.docx", raw)

	want := []string{"Первый абзац документа", "Второй абзац из двух прогонов"}
	texts := make([]string, 0, len(parsed.Blocks))
	for _, b := range parsed.Blocks {
		texts = append(texts, b.Text)
		if b.Kind != kindParagraph {
			t.Errorf("вид блока %q вместо paragraph", b.Kind)
		}
		if b.Page != nil {
			t.Errorf("у DOCX page должен быть nil, получено %d", *b.Page)
		}
	}
	if !slices.Equal(texts, want) {
		t.Errorf("тексты %v вместо %v", texts, want)
	}
}

// TestConvertDocxSkipsEmptyParagraphs — пустой <w:p> не должен становиться
// блоком: блок без текста попал бы в чанк пустым текстом.
func TestConvertDocxSkipsEmptyParagraphs(t *testing.T) {
	raw := buildDocx([]string{
		`<w:p/>`,
		`<w:p><w:r><w:t>   </w:t></w:r></w:p>`,
		`<w:p><w:r><w:t>Содержательный абзац</w:t></w:r></w:p>`,
	})

	parsed := convertOk(t, "manual.docx", raw)

	if len(parsed.Blocks) != 1 || parsed.Blocks[0].Text != "Содержательный абзац" {
		t.Errorf("блоки %v, ожидался один содержательный", textsOf(parsed.Blocks))
	}
}

// TestConvertDocxWithoutDocumentXMLIsRejected — zip без word/document.xml —
// не документ: успешный разбор пустого файла скрыл бы потерю содержимого.
func TestConvertDocxWithoutDocumentXMLIsRejected(t *testing.T) {
	raw := buildDocxRaw(map[string]string{"word/styles.xml": "<w:styles/>"})

	_, err := documents.Convert(context.Background(), raw, conversionRequest("empty.docx"))
	assertParseError(t, err, domain.CodeInvalidMetadata)
}

// TestConvertDocxRejectsNonZip — сигнатура PK есть, архива нет.
func TestConvertDocxRejectsNonZip(t *testing.T) {
	_, err := documents.Convert(
		context.Background(), []byte("PK\x03\x04 это не архив"), conversionRequest("broken.docx"))
	assertParseError(t, err, domain.CodeInvalidMetadata)
}

// ── Конверт ParsedDocument ─────────────────────────────────────────

// TestConvertFillsParsedDocumentEnvelope — поля конверта заполняются из
// ConversionRequest и байтов файла: без них стадия index не найдёт версию.
func TestConvertFillsParsedDocumentEnvelope(t *testing.T) {
	content := []byte("# Раздел\n\nТекст документа.\n")
	parsed := convertOk(t, "export-guide.md", content)

	if parsed.SchemaVersion != "1.0" {
		t.Errorf("schema_version %q вместо 1.0", parsed.SchemaVersion)
	}
	if parsed.TenantID != testTenantID || parsed.KBID != testKBID ||
		parsed.DocumentID != testDocumentID || parsed.VersionID != testVersionID {
		t.Errorf("идентификаторы не совпали с запросом: %+v", parsed)
	}
	if parsed.Title != "Инструкция по выгрузке" || parsed.Filename != "export-guide.md" {
		t.Errorf("title/filename %q/%q", parsed.Title, parsed.Filename)
	}
	if url, ok := parsed.SourceURLValue(); !ok || url != testSourceURL {
		t.Errorf("source_url %q вместо %q", url, testSourceURL)
	}
	if parsed.Metadata["product"] != "portal" {
		t.Errorf("metadata не перенесена: %v", parsed.Metadata)
	}
	sum := sha256.Sum256(content)
	if parsed.ContentSHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("content_sha256 %q, ожидался sha256 байтов файла", parsed.ContentSHA256)
	}
}

// TestConvertRejectsUnsupportedFormat — файл неподдерживаемого вида обязан
// отклониться кодом UNSUPPORTED_FORMAT, а не разобраться «как получится».
func TestConvertRejectsUnsupportedFormat(t *testing.T) {
	for _, filename := range []string{"archive.zip", "sheet.xlsx", "image.png", "noext"} {
		t.Run(filename, func(t *testing.T) {
			_, err := documents.Convert(
				context.Background(), []byte("данные"), conversionRequest(filename))
			assertParseError(t, err, domain.CodeUnsupportedFormat)
		})
	}
}

// TestConvertPropagatesCancelledContext — отменённый контекст останавливает
// разбор: иначе воркер дорабатывал бы уже ненужное задание.
func TestConvertPropagatesCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := documents.Convert(ctx, []byte("# Раздел"), conversionRequest("doc.md"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("получено %v вместо context.Canceled", err)
	}
}

// ── Детерминированность ────────────────────────────────────────────

// TestConvertIsIdempotentAcrossRuns — повтор стадии convert на одном файле
// обязан дать побайтово тот же ParsedDocument: иначе повтор после упавшей
// транзакции переписал бы parsed.json и осиротил бы чанки прежней попытки.
func TestConvertIsIdempotentAcrossRuns(t *testing.T) {
	for _, test := range []struct {
		name     string
		filename string
		content  []byte
	}{
		{
			name:     "markdown",
			filename: "doc.md",
			content:  []byte("# A\n\nтекст\n\n## B\n\n- один\n- два\n\n```go\nx := 1\n```\n"),
		},
		{
			name:     "text",
			filename: "doc.txt",
			content:  []byte("первый абзац\n\nвторой абзац\n"),
		},
		{
			name:     "pdf",
			filename: "doc.pdf",
			content:  buildPDF([]string{"page one", "page two"}),
		},
		{
			name:     "docx",
			filename: "doc.docx",
			content: buildDocx([]string{
				`<w:p><w:r><w:t>абзац один</w:t></w:r></w:p>`,
				`<w:p><w:r><w:t>абзац два</w:t></w:r></w:p>`,
			}),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			first := convertOk(t, test.filename, test.content)
			second := convertOk(t, test.filename, test.content)

			if len(first.Blocks) != len(second.Blocks) {
				t.Fatalf("число блоков изменилось: %d и %d", len(first.Blocks), len(second.Blocks))
			}
			for i := range first.Blocks {
				a, b := first.Blocks[i], second.Blocks[i]
				if a.BlockID != b.BlockID {
					t.Errorf("блок %d: block_id %s и %s", i, a.BlockID, b.BlockID)
				}
				if a.Text != b.Text || a.Kind != b.Kind || a.Position != b.Position {
					t.Errorf("блок %d различается: %+v и %+v", i, a, b)
				}
				if !slices.Equal(a.SectionPath, b.SectionPath) {
					t.Errorf("блок %d: section_path %v и %v", i, a.SectionPath, b.SectionPath)
				}
			}
		})
	}
}

// TestConvertBlockIDsAreStableForSameInput — block_id выводится из версии,
// позиции, вида и текста, поэтому у одного и того же блока он не меняется
// между вызовами. Именно на этом держится идемпотентность стадии convert.
func TestConvertBlockIDsAreStableForSameInput(t *testing.T) {
	source := []byte("# Раздел\n\nПервый абзац.\n\nВторой абзац.\n")
	first := convertOk(t, "doc.md", source)
	second := convertOk(t, "doc.md", source)

	seen := make(map[uuid.UUID]bool, len(first.Blocks))
	for i, b := range first.Blocks {
		if b.BlockID != second.Blocks[i].BlockID {
			t.Errorf("блок %d: идентификатор нестабилен", i)
		}
		if seen[b.BlockID] {
			t.Errorf("блок %d: block_id %s повторяется в документе", i, b.BlockID)
		}
		seen[b.BlockID] = true
	}
}

// TestConvertBlockIDsDifferBetweenVersions — два документа с одинаковым
// текстом обязаны получить разные block_id: общий идентификатор склеил бы
// чанки разных документов под одним ключом локатора.
func TestConvertBlockIDsDifferBetweenVersions(t *testing.T) {
	source := []byte("# Раздел\n\nОдин и тот же текст.\n")
	first := convertOk(t, "doc.md", source)

	other := conversionRequest("doc.md")
	other.VersionID = uuid.MustParse("55555555-5555-4555-8555-555555555555")
	second, err := documents.Convert(context.Background(), source, other)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}

	if first.Blocks[0].BlockID == second.Blocks[0].BlockID {
		t.Error("у разных версий получился одинаковый block_id")
	}
}

// TestConvertWithoutSourceURLKeepsNil — отсутствующий source_url остаётся
// null, а не превращается в пустую строку: по нему судят о происхождении
// документа, и "" неотличим от забытого значения.
func TestConvertWithoutSourceURLKeepsNil(t *testing.T) {
	req := conversionRequest("doc.md")
	req.SourceURL = nil

	parsed, err := documents.Convert(context.Background(), []byte("# Раздел\n\nтекст\n"), req)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if parsed.SourceURL != nil {
		t.Errorf("source_url %q вместо nil", *parsed.SourceURL)
	}
}

// ── Общие проверки результата ──────────────────────────────────────

// TestConvertMarkdownKeepsAutolinkAddress — автоссылка <https://…> состоит
// только из адреса, и без него в блоке не осталось бы ничего: искать было бы
// нечем.
func TestConvertMarkdownKeepsAutolinkAddress(t *testing.T) {
	parsed := convertOk(t, "doc.md",
		[]byte("# Раздел\n\nСм. <https://portal.example/guide> за подробностями.\n"))

	text := textsOf(parsed.Blocks)
	joined := strings.Join(text, " ")
	if !strings.Contains(joined, "https://portal.example/guide") {
		t.Errorf("адрес автоссылки потерян: %q", joined)
	}
}

// TestConvertMarkdownUnwrapsBlockquote — цитата разбирается как своё
// содержимое: отдельного вида «цитата» в ТЗ 3.2 нет, а текст цитаты искать
// нужно так же, как обычный.
func TestConvertMarkdownUnwrapsBlockquote(t *testing.T) {
	parsed := convertOk(t, "doc.md",
		[]byte("# Раздел\n\n> Цитируемое мнение эксперта.\n"))

	joined := strings.Join(textsOf(parsed.Blocks), " ")
	if !strings.Contains(joined, "Цитируемое мнение эксперта") {
		t.Errorf("текст цитаты потерян: %q", joined)
	}
}

// TestConvertMarkdownNestedListKeepsItems — пункт с вложенным списком содержит
// несколько абзацев: обход только первого терял бы остальные.
func TestConvertMarkdownNestedListKeepsItems(t *testing.T) {
	parsed := convertOk(t, "doc.md", []byte(""+
		"# Шаги\n\n1. Откройте портал\n   - вкладка Интеграции\n   - пункт выгрузки\n"+
		"2. Нажмите кнопку\n"))

	var list []string
	for _, b := range parsed.Blocks {
		if b.Kind == kindList {
			list = append(list, b.Text)
		}
	}
	if len(list) != 1 {
		t.Fatalf("списков %d вместо 1", len(list))
	}
	for _, want := range []string{"Откройте портал", "вкладка Интеграции", "Нажмите кнопку"} {
		if !strings.Contains(list[0], want) {
			t.Errorf("в списке нет %q: %q", want, list[0])
		}
	}
}

// TestConvertMarkdownSkipsDeepFirstLevel — документ, начинающийся с ##,
// не должен получать пустой первый элемент section_path.
func TestConvertMarkdownSkipsDeepFirstLevel(t *testing.T) {
	parsed := convertOk(t, "doc.md", []byte("## Начало\n\nтекст\n"))

	for i, b := range parsed.Blocks {
		if len(b.SectionPath) == 0 {
			t.Errorf("блок %d: пустой section_path при заголовке второго уровня", i)
		}
		for _, part := range b.SectionPath {
			if part == "" {
				t.Errorf("блок %d: пустой элемент в section_path %v", i, b.SectionPath)
			}
		}
	}
}

// assertParseError требует конкретный код домена: необработанная ошибка
// разбора превратилась бы в бесконечные повторы в очереди.
func assertParseError(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("ожидалась ошибка с кодом %s", code)
	}
	appErr, ok := domain.AsAppError(err)
	if !ok {
		t.Fatalf("ошибка не AppError (код %s ожидался): %v", code, err)
	}
	if appErr.Code != code {
		t.Errorf("код %s вместо %s: %s", appErr.Code, code, appErr.Message)
	}
}

func textsOf(blocks []domain.Block) []string {
	out := make([]string, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, b.Text)
	}
	return out
}
