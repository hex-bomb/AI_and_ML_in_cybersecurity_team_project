package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"

	"github.com/google/uuid"
)

// Block kinds of a parsed document (ТЗ 3.2). Набор закрытый: потребитель
// ParsedDocument опирается на эти виды, и новый вид конвертер заводить не вправе.
const (
	BlockKindHeading   = "heading"
	BlockKindParagraph = "paragraph"
	BlockKindList      = "list"
	BlockKindCode      = "code"
	BlockKindTable     = "table"
)

// BlockIDParams is the deterministic block identifier input.
type BlockIDParams struct {
	// VersionID is the namespace: идентификаторы блоков двух версий одного
	// документа не должны пересекаться.
	VersionID uuid.UUID
	Position  int
	Kind      string
	Text      string
}

// BlockIDFor returns the deterministic block id (uuid.NewSHA1 — это UUIDv5).
//
// Детерминизм обязателен, а не удобен: повтор стадии convert переписывает
// parsed.json, и случайный block_id осиротил бы чанки прежней попытки,
// locator которых ссылался бы на несуществующие блоки. Имя UUIDv5 собирается
// из позиции, вида и SHA-256 текста — по ТЗ 3.2; хеш текста, а не сам
// текст, держит длину имени постоянной при больших блоках.
func BlockIDFor(p BlockIDParams) uuid.UUID {
	sum := sha256.Sum256([]byte(p.Text))
	name := strconv.Itoa(p.Position) + "|" + p.Kind + "|" + hex.EncodeToString(sum[:])
	return uuid.NewSHA1(p.VersionID, []byte(name))
}
