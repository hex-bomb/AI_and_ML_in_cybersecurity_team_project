package domain

import (
	"testing"

	"github.com/google/uuid"
)

var (
	blockIDVersion = uuid.MustParse("44444444-4444-4444-8444-444444444444")
	otherVersion   = uuid.MustParse("55555555-5555-4555-8555-555555555555")
)

// TestBlockIDForIsDeterministic — повторная конвертация того же файла обязана
// дать те же block_id: иначе повтор стадии convert переписал бы parsed.json
// новыми идентификаторами, и чанки прежней попытки остались бы со ссылками на
// несуществующие блоки.
func TestBlockIDForIsDeterministic(t *testing.T) {
	p := BlockIDParams{VersionID: blockIDVersion, Position: 3, Kind: "paragraph", Text: "Текст абзаца"}
	first := BlockIDFor(p)
	for i := 0; i < 8; i++ {
		if got := BlockIDFor(p); got != first {
			t.Fatalf("повтор %d дал %s вместо %s", i, got, first)
		}
	}
}

// TestBlockIDForIsUUIDv5 — версия UUID фиксирована: block_id пересекает границу
// процессов и приезжает в Elasticsearch как locator.block_ids.
func TestBlockIDForIsUUIDv5(t *testing.T) {
	got := BlockIDFor(BlockIDParams{VersionID: blockIDVersion, Position: 0, Kind: "heading", Text: "Раздел"})
	if got.Version() != 5 {
		t.Errorf("версия UUID %d, ожидалась 5 (uuid.NewSHA1)", got.Version())
	}
	if got.Variant() != uuid.RFC4122 {
		t.Errorf("вариант UUID %v, ожидался RFC4122", got.Variant())
	}
}

// TestBlockIDForVariesByEveryInput — сдвиг любой части входа обязан менять
// идентификатор, иначе два разных блока склеились бы под одним block_id.
func TestBlockIDForVariesByEveryInput(t *testing.T) {
	base := BlockIDParams{VersionID: blockIDVersion, Position: 1, Kind: "paragraph", Text: "Текст"}
	want := BlockIDFor(base)

	variants := map[string]BlockIDParams{
		"другая позиция": {VersionID: blockIDVersion, Position: 2, Kind: "paragraph", Text: "Текст"},
		"другой вид":     {VersionID: blockIDVersion, Position: 1, Kind: "heading", Text: "Текст"},
		"другой текст":   {VersionID: blockIDVersion, Position: 1, Kind: "paragraph", Text: "Другой"},
		"другая версия":  {VersionID: otherVersion, Position: 1, Kind: "paragraph", Text: "Текст"},
	}
	for name, params := range variants {
		if got := BlockIDFor(params); got == want {
			t.Errorf("%s: block_id не изменился (%s)", name, got)
		}
	}
}

// TestBlockIDForDistinctPositions — соседние блоки не должны делить один id.
func TestBlockIDForDistinctPositions(t *testing.T) {
	seen := make(map[uuid.UUID]int, 8)
	for position := 0; position < 8; position++ {
		id := BlockIDFor(BlockIDParams{
			VersionID: blockIDVersion, Position: position, Kind: "paragraph", Text: "Один и тот же текст",
		})
		if previous, ok := seen[id]; ok {
			t.Fatalf("позиции %d и %d дали один block_id %s", previous, position, id)
		}
		seen[id] = position
	}
}
