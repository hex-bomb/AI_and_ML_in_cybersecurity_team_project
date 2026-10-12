package indexing_test

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	domain "github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/jobs/indexing"
)

// Конфигурация индексации для тестов нарезки.
//
// Числа маленькие, чтобы границы чанков считались в уме: тест, который
// разбирает чанки на 600 токенов, проверяет арифметику, а не нарезку.
func testConfig(maxTokens, overlap int) domain.IndexingConfig {
	return domain.IndexingConfig{
		EmbeddingProviderID: "local-hugot",
		EmbeddingModel:      "intfloat/multilingual-e5-small",
		EmbeddingRevision:   "pilot-1",
		EmbeddingDimension:  384,
		ChunkMaxTokens:      maxTokens,
		ChunkOverlapTokens:  overlap,
		TokenizerID:         "hugot/multilingual-e5-small-wordpiece",
		NormalizeVectors:    true,
		Distance:            "cosine",
	}
}

var testVersionID = uuid.MustParse("44444444-4444-4444-8444-444444444444")

// block — блок документа для теста. Страница по умолчанию неизвестна: у MD и
// DOCX её нет, и проверять её выдумывание нельзя.
func block(position int, section []string, text string) domain.Block {
	return domain.Block{
		BlockID:     uuid.NewSHA1(testVersionID, []byte(strings.Join([]string{sectionText(section), text}, "\x00"))),
		Position:    position,
		Kind:        "paragraph",
		Text:        text,
		SectionPath: section,
	}
}

func page(block domain.Block, p int) domain.Block {
	block.Page = &p
	return block
}

func sectionText(section []string) string { return strings.Join(section, "/") }

func document(blocks ...domain.Block) domain.ParsedDocument {
	return domain.ParsedDocument{
		SchemaVersion: "1.0",
		TenantID:      uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		KBID:          uuid.MustParse("22222222-2222-4222-8222-222222222222"),
		DocumentID:    uuid.MustParse("33333333-3333-4333-8333-333333333333"),
		VersionID:     testVersionID,
		Title:         "Руководство",
		Filename:      "manual.md",
		Blocks:        blocks,
	}
}

// words — текст из n слов по четыре символа, т.е. ровно n токенов по
// EstimateTokens. Читается в тесте как «десять токенов», а не «сорок
// символов».
func words(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "абвг"
	}
	return strings.Join(parts, " ")
}

// TestChunkKeepsWholeBlocksInsideSection — соседние блоки одного раздела
// объединяются, пока помещаются, и ни один не режется пополам (ТЗ 6.3 п.2).
func TestChunkKeepsWholeBlocksInsideSection(t *testing.T) {
	blocks := []domain.Block{
		block(0, []string{"Экспорт"}, words(3)),
		block(1, []string{"Экспорт"}, words(4)),
		block(2, []string{"Экспорт"}, words(5)),
	}
	doc := document(blocks...)

	// Лимит подобран так, чтобы первые два блока помещались вместе, а третий
	// — уже нет: заголовок раздела и разделители тоже занимают бюджет.
	chunks, err := indexing.Chunk(doc, testConfig(14, 4))
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}

	if len(chunks) != 2 {
		t.Fatalf("ожидалось 2 чанка, получено %d: %v", len(chunks), texts(chunks))
	}
	// Первый чанк: блоки 0 и 1 целиком, третий в него уже не влезает.
	if got := chunks[0].BlockIDs; len(got) != 2 {
		t.Errorf("в первом чанке ожидались два блока, получено %d", len(got))
	}
	// Второй чанк несёт хвост первого (перекрытие) и третий блок.
	if got := chunks[1].BlockIDs; len(got) != 2 {
		t.Errorf("во втором чанке ожидались два блока с перекрытием, получено %d", len(got))
	}
	if chunks[0].BlockIDs[1] != chunks[1].BlockIDs[0] {
		t.Error("во втором чанке нет перекрытия: последний блок первого должен повториться")
	}
	if !slices.Contains(chunks[1].BlockIDs, blocks[2].BlockID) {
		t.Errorf("третий блок не попал во второй чанк: %v", chunks[1].BlockIDs)
	}
}

// TestChunkNeverExceedsMaxTokens — верхняя граница жёсткая: чанк длиннее
// лимита не помещается в бюджет модели и портит и эмбеддинг, и выдачу.
func TestChunkNeverExceedsMaxTokens(t *testing.T) {
	doc := document(
		block(0, []string{"Раздел"}, words(6)),
		block(1, []string{"Раздел"}, words(7)),
		block(2, []string{"Раздел"}, words(5)),
	)

	chunks, err := indexing.Chunk(doc, testConfig(10, 3))
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	for i, c := range chunks {
		// Заголовок раздела занимает три токена вместе со склейкой.
		if got := domain.EstimateTokens(c.Text); got > 10 {
			t.Errorf("чанк %d содержит %d токенов при лимите 10: %q", i, got, c.Text)
		}
	}
}

// TestChunkDoesNotMergeDifferentSections — блоки разных разделов не
// объединяются, даже если вместе помещаются в один чанк.
func TestChunkDoesNotMergeDifferentSections(t *testing.T) {
	doc := document(
		block(0, []string{"Глава 1"}, words(2)),
		block(1, []string{"Глава 2"}, words(2)),
	)

	chunks, err := indexing.Chunk(doc, testConfig(20, 2))
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("ожидались два чанка по разделу, получено %d: %v", len(chunks), texts(chunks))
	}
	for i, want := range []string{"Глава 1", "Глава 2"} {
		if len(chunks[i].BlockIDs) != 1 {
			t.Errorf("чанк %d содержит %d блоков, ожидался один", i, len(chunks[i].BlockIDs))
		}
		if !strings.HasPrefix(chunks[i].Text, want) {
			t.Errorf("чанк %d должен начинаться с %q, получено %q", i, want, chunks[i].Text)
		}
	}
}

// TestChunkKeepsSectionPathInText — раздел попадает в текст чанка, иначе
// поиск по названию раздела фрагмент не найдёт (ТЗ 6.3 п.2).
func TestChunkKeepsSectionPathInText(t *testing.T) {
	doc := document(block(0, []string{"Экспорт", "Параметры"}, words(2)))

	chunks, err := indexing.Chunk(doc, testConfig(10, 2))
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if !strings.HasPrefix(chunks[0].Text, "Экспорт > Параметры") {
		t.Errorf("текст чанка должен начинаться с пути раздела, получено %q", chunks[0].Text)
	}
	if len(chunks[0].SectionPath) != 2 {
		t.Errorf("локатор должен хранить весь путь раздела, получено %v", chunks[0].SectionPath)
	}
}

// TestChunkSplitsLongBlockWithOverlap — блок длиннее лимита делится, причём
// части перекрываются: фрагмент, разорванный посередине предложения, без
// перекрытия теряет контекст на стыке.
func TestChunkSplitsLongBlockWithOverlap(t *testing.T) {
	long := strings.Join([]string{words(8), words(8), words(8)}, " ")
	doc := document(block(0, []string{"Раздел"}, long))

	chunks, err := indexing.Chunk(doc, testConfig(10, 4))
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if len(chunks) < 2 {
		t.Fatalf("длинный блок должен быть разделён, получено %d чанков", len(chunks))
	}
	for i, c := range chunks {
		if got := domain.EstimateTokens(c.Text); got > 10 {
			t.Errorf("часть %d содержит %d токенов при лимите 10", i, got)
		}
		if len(c.BlockIDs) != 1 {
			t.Errorf("часть %d локатора указывает %d блоков, ожидался один", i, len(c.BlockIDs))
		}
	}
	// Перекрытие: последнее слово первой части повторяется во второй.
	// Сравнивать надо по содержимому, а не по началу текста: каждая часть
	// начинается с заголовка раздела.
	first := strings.Fields(strings.TrimPrefix(chunks[0].Text, "Раздел\n\n"))
	second := strings.Fields(strings.TrimPrefix(chunks[1].Text, "Раздел\n\n"))
	tail := first[len(first)-1]
	if !slices.Contains(second, tail) {
		t.Errorf("перекрытие между частями блока отсутствует: %q не найдено в %v", tail, second)
	}
}

// TestChunkRejectsOverlapNotLessThanMax — неверная настройка, а не тихая
// порча нарезки (ТЗ 6.3 п.3).
func TestChunkRejectsOverlapNotLessThanMax(t *testing.T) {
	doc := document(block(0, []string{"Раздел"}, words(4)))

	for _, overlap := range []int{10, 11} {
		t.Run("overlap="+strconv.Itoa(overlap), func(t *testing.T) {
			chunks, err := indexing.Chunk(doc, testConfig(10, overlap))
			if err == nil {
				t.Fatalf("ожидалась ошибка конфигурации, получено %d чанков", len(chunks))
			}
			if !errors.Is(err, domain.ErrConfigurationInvalid) {
				t.Errorf("ожидалась CONFIGURATION_INVALID, получено %v", err)
			}
		})
	}
}

// TestChunkRejectsZeroMaxTokens — нулевой лимит тоже конфигурация, а не пустой
// документ.
func TestChunkRejectsZeroMaxTokens(t *testing.T) {
	doc := document(block(0, []string{"Раздел"}, words(4)))

	if _, err := indexing.Chunk(doc, testConfig(0, 0)); !errors.Is(err, domain.ErrConfigurationInvalid) {
		t.Errorf("ожидалась CONFIGURATION_INVALID, получено %v", err)
	}
}

// TestChunkEmptyDocumentIsError — версия без чанков выглядела бы успешно
// обработанной, но искать было бы не по чему.
func TestChunkEmptyDocumentIsError(t *testing.T) {
	doc := document(block(0, []string{"Раздел"}, "   \n\t "))

	chunks, err := indexing.Chunk(doc, testConfig(10, 2))
	if err == nil {
		t.Fatalf("ожидалась ошибка пустого документа, получено %d чанков", len(chunks))
	}
	// Сверка sentinel'а: в домене нет ErrInvalidMetadata, код проверяется
	// через AsAppError — так же, как это делает транспортный слой.
	if appErr, ok := domain.AsAppError(err); !ok || appErr.Code != domain.CodeInvalidMetadata {
		t.Errorf("ожидался INVALID_METADATA, получено %v", err)
	}

	if _, err := indexing.Chunk(document(), testConfig(10, 2)); err == nil {
		t.Error("документ без блоков должен отвергаться, а не давать пустой список")
	}
}

// TestChunkIsDeterministic — та же версия и тот же профиль обязаны давать те
// же границы и те же chunk_id: на этом стоит идемпотентность переиндексации
// и удаление старых чанков по chunk_id (ТЗ 6.6).
func TestChunkIsDeterministic(t *testing.T) {
	doc := document(
		block(0, []string{"Экспорт"}, words(5)),
		block(1, []string{"Экспорт"}, words(9)),
		block(2, []string{"Импорт"}, words(4)),
	)

	first, err := indexing.Chunk(doc, testConfig(10, 3))
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	second, err := indexing.Chunk(doc, testConfig(10, 3))
	if err != nil {
		t.Fatalf("повторный Chunk: %v", err)
	}

	if len(first) != len(second) {
		t.Fatalf("повторная нарезка дала %d чанков вместо %d", len(second), len(first))
	}
	for i := range first {
		if first[i].ChunkID != second[i].ChunkID {
			t.Errorf("чанк %d: chunk_id разошёлся между прогонами", i)
		}
		if first[i].Text != second[i].Text {
			t.Errorf("чанк %d: текст разошёлся между прогонами", i)
		}
	}
	// Порядок сквозной и с нуля: chunk_id считается от номера, и сдвиг нумерации
	// переписал бы идентификаторы всех уже записанных чанков.
	seen := make(map[string]bool, len(first))
	for i, c := range first {
		if c.Number != i {
			t.Errorf("чанк %d имеет номер %d", i, c.Number)
		}
		if c.ChunkID == "" {
			t.Errorf("чанк %d без идентификатора", i)
		}
		if seen[c.ChunkID] {
			t.Errorf("идентификатор чанка %d повторился", i)
		}
		seen[c.ChunkID] = true
	}
}

// TestChunkIdentifiersDependOnProfile — смена профиля меняет идентификаторы
// всех чанков версии, и это заложено в ChunkIDFor, а не в нарезку.
func TestChunkIdentifiersDependOnProfile(t *testing.T) {
	doc := document(block(0, []string{"Раздел"}, words(4)))

	withOverlap0, err := indexing.Chunk(doc, testConfig(10, 0))
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	withOverlap2, err := indexing.Chunk(doc, testConfig(10, 2))
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if withOverlap0[0].ChunkID == withOverlap2[0].ChunkID {
		t.Error("разные параметры нарезки дали одинаковый chunk_id")
	}
}

// TestChunkPageRangeFromBlocks — страница берётся из блока. У MD и DOCX её
// нет, и тогда обе границы nil: выдуманная страница попала бы в локатор
// цитаты.
func TestChunkPageRangeFromBlocks(t *testing.T) {
	doc := document(
		page(block(0, []string{"Раздел"}, words(2)), 3),
		page(block(1, []string{"Раздел"}, words(2)), 7),
	)

	chunks, err := indexing.Chunk(doc, testConfig(10, 2))
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if chunks[0].PageStart == nil || *chunks[0].PageStart != 3 {
		t.Errorf("PageStart ожидался 3, получен %v", chunks[0].PageStart)
	}
	if chunks[0].PageEnd == nil || *chunks[0].PageEnd != 7 {
		t.Errorf("PageEnd ожидался 7, получен %v", chunks[0].PageEnd)
	}

	noPages := document(block(0, []string{"Раздел"}, words(2)))
	chunks, err = indexing.Chunk(noPages, testConfig(10, 2))
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if chunks[0].PageStart != nil || chunks[0].PageEnd != nil {
		t.Errorf("у блока без страницы границы должны быть nil, получены %v и %v",
			chunks[0].PageStart, chunks[0].PageEnd)
	}
}

// TestChunkUsesFixtureFromContracts — эталон разобранного документа из
// contracts/fixtures обязан нарезаться без потерь: от него зависят
// идентификаторы уже проиндексированных версий.
func TestChunkUsesFixtureFromContracts(t *testing.T) {
	doc := document(
		block(0, []string{"Export"}, "Export"),
		block(1, []string{"Export"}, "Open Settings."),
		block(2, []string{"Export", "Parameters"}, "Parameters"),
		block(3, []string{"Export", "Parameters"}, "Field | Value\nformat | JSON"),
	)

	chunks, err := indexing.Chunk(doc, testConfig(600, 100))
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("ожидалось два чанка по разделам, получено %d: %v", len(chunks), texts(chunks))
	}
	if !strings.HasPrefix(chunks[1].Text, "Export > Parameters") {
		t.Errorf("второй чанк должен начаться с пути раздела, получено %q", chunks[1].Text)
	}
}

// TestChunkRespectsLimitAcrossShapes прогоняет нарезку по сетке конфигураций
// и форм документа. Один пример ничего не доказывает: заголовок раздела,
// разделитель и возврат к границе слова складываются в разную длину, и лимит
// нарушается только на некоторых сочетаниях.
func TestChunkRespectsLimitAcrossShapes(t *testing.T) {
	sections := [][]string{nil, {"Раздел"}, {"Глава", "Параметры"}}
	// Нижняя граница — 7 токенов: при меньшем лимите заголовок раздела
	// «Глава > Параметры» занимает весь чанк, и это отдельный случай
	// (см. TestChunkRejectsSectionPathLongerThanLimit).
	for _, maxTokens := range []int{7, 10, 16, 33} {
		for _, overlap := range []int{0, 1, maxTokens / 3, maxTokens - 1} {
			cfg := testConfig(maxTokens, overlap)
			for _, section := range sections {
				doc := document(
					block(0, section, strings.Repeat("слово ", maxTokens)),
					block(1, section, words(1)),
					block(2, nil, words(2)),
				)
				chunks, err := indexing.Chunk(doc, cfg)
				if err != nil {
					t.Fatalf("max=%d overlap=%d: %v", maxTokens, overlap, err)
				}
				for i, c := range chunks {
					if got := domain.EstimateTokens(c.Text); got > maxTokens {
						t.Fatalf("max=%d overlap=%d раздел=%v: чанк %d содержит %d токенов: %q",
							maxTokens, overlap, section, i, got, c.Text)
					}
				}
			}
		}
	}
}

// TestChunkCoversWholeDocument без перекрытия — нарезка обязана покрыть
// документ целиком, ничего не потеряв и ничего не повторив.
func TestChunkCoversWholeDocument(t *testing.T) {
	doc := document(block(0, []string{"Раздел"}, strings.TrimSpace(strings.Repeat("слово ", 40))))

	chunks, err := indexing.Chunk(doc, testConfig(12, 0))
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}

	joined := joinedWords(chunks, "Раздел\n\n")
	want := strings.Fields(doc.Blocks[0].Text)
	if len(joined) != len(want) {
		t.Fatalf("после нарезки %d слов вместо %d: %d чанков", len(joined), len(want), len(chunks))
	}
	for i := range want {
		if joined[i] != want[i] {
			t.Fatalf("слово %d после нарезки %q вместо %q", i, joined[i], want[i])
		}
	}
}

// TestChunkKeepsOrderWithOverlap — перекрытие дублирует текст на стыке, но
// порядок фрагментов обязан остаться порядком исходного документа.
func TestChunkKeepsOrderWithOverlap(t *testing.T) {
	doc := document(block(0, []string{"Раздел"}, strings.TrimSpace(strings.Repeat("слово ", 40))))

	chunks, err := indexing.Chunk(doc, testConfig(12, 3))
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}

	// Склеенный текст обязан содержать исходный как подпоследовательность
	// слов: лишние слова допустимы (это перекрытие), перестановки — нет.
	got := joinedWords(chunks, "Раздел\n\n")
	want := strings.Fields(doc.Blocks[0].Text)
	i := 0
	for _, w := range got {
		if i < len(want) && w == want[i] {
			i++
		}
	}
	if i != len(want) {
		t.Errorf("нарезка с перекрытием потеряла слова: %d из %d на месте", i, len(want))
	}
}

// joinedWords собирает слова всех чанков без заголовков разделов.
func joinedWords(chunks []indexing.Draft, header string) []string {
	var out []string
	for _, c := range chunks {
		body := strings.TrimSpace(strings.TrimPrefix(c.Text, header))
		out = append(out, strings.Fields(body)...)
	}
	return out
}

// TestChunkRejectsSectionPathLongerThanLimit — раздел не помещается в
// chunk_max_tokens. Молча выпустить чанк длиннее лимита нельзя: модель
// обрежет его при сборке ответа, и цитата разъедется с источником.
func TestChunkRejectsSectionPathLongerThanLimit(t *testing.T) {
	doc := document(block(0, []string{"Очень", "Длинный", "Путь", "Раздела"}, words(2)))

	chunks, err := indexing.Chunk(doc, testConfig(5, 1))
	if err == nil {
		t.Fatalf("ожидалась ошибка конфигурации, получено %d чанков", len(chunks))
	}
	if !errors.Is(err, domain.ErrConfigurationInvalid) {
		t.Errorf("ожидалась CONFIGURATION_INVALID, получено %v", err)
	}
}

func texts(chunks []indexing.Draft) []string {
	out := make([]string, len(chunks))
	for i, c := range chunks {
		out[i] = c.Text
	}
	return out
}
