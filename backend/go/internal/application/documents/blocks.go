package documents

import (
	"strings"
	"unicode"

	"github.com/google/uuid"

	"github.com/kbsearch/backend-go/internal/domain"
)

// Общие правила нормализации текста блока (ТЗ 3.2).
//
// WHY общие: каждый разбор обязан одинаково приводить CRLF к LF и отбрасывать
// пустые блоки, иначе один и тот же документ, сохранённый разными редакторами,
// дал бы разные block_id и разные чанки.

// newline — нормализованный перенос строки внутри блока.
const newline = "\n"

// normalizeText приводит текст блока к каноническому виду.
//
// Перевод CRLF/CR в LF обязателен: иначе одинаковый документ, сохранённый на
// Windows и на Linux, дал бы разные block_id и дважды попал бы в индекс.
func normalizeText(text string) string {
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", newline), "\r", newline)
}

// cleanBlockText убирает внешние пробелы и схлопывает внутренние переносы строк
// в пробел. Внутренние одиночные переносы сохраняются: в текстовом файле они
// несут смысл, а не accidents абзацной вёрстки.
func cleanBlockText(text string) string {
	lines := strings.Split(normalizeText(text), newline)
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	cleaned := strings.TrimSpace(strings.Join(lines, newline))
	return strings.TrimFunc(cleaned, func(r rune) bool {
		return unicode.IsSpace(r)
	})
}

// blankLines разбивает текст на непустые абзацы по пустым строкам.
func blankLines(text string) []string {
	normalized := normalizeText(text)
	raw := strings.Split(normalized, newline)

	out := make([]string, 0, len(raw))
	current := make([]string, 0, len(raw))
	flush := func() {
		if len(current) == 0 {
			return
		}
		if block := cleanBlockText(strings.Join(current, newline)); block != "" {
			out = append(out, block)
		}
		current = current[:0]
	}
	for _, line := range raw {
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		current = append(current, line)
	}
	flush()
	return out
}

// draft is a block before identifiers and positions are assigned.
//
// WHY промежуточный тип: разбор не знает номера позиции и идентификатора — их
// назначает convertBlockIDs в конце, когда известен весь документ. Иначе каждый
// разбор считал бы позиции сам и мог бы разойтись с итоговым порядком блоков.
type draft struct {
	kind        string
	text        string
	sectionPath []string
	page        *int
}

// toBlocks превращает черновики в блоки ParsedDocument с детерминированными
// идентификаторами. Пустые тексты отбрасываются: блок без текста попал бы в
// чанк пустым фрагментом и только раздул бы индекс.
func toBlocks(drafts []draft, versionID uuid.UUID) []domain.Block {
	blocks := make([]domain.Block, 0, len(drafts))
	for _, d := range drafts {
		text := cleanBlockText(d.text)
		if text == "" {
			continue
		}
		blocks = append(blocks, domain.Block{
			BlockID:     domain.BlockIDFor(domain.BlockIDParams{VersionID: versionID, Position: len(blocks), Kind: d.kind, Text: text}),
			Position:    len(blocks),
			Kind:        d.kind,
			Text:        text,
			SectionPath: d.sectionPath,
			Page:        d.page,
		})
	}
	return blocks
}
