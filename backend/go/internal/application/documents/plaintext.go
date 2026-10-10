package documents

import (
	"github.com/kbsearch/backend-go/internal/domain"
)

// parsePlainText раскладывает .txt на абзацы по пустым строкам.
//
// WHY разделение по пустым строкам: у .txt нет разметки, и единственный
// доступный признак границы абзаца — пустая строка. Склеивать весь файл в
// один блок нельзя: чанкер режет его по токенам в произвольном месте и теряет
// границы фраз, а объединять всё подряд — прочитать как один абзац.

// Разделы и страницы у .txt неизвестны и выдумывать их нельзя: в section_path
// попал бы раздел, которого в документе нет, а page сослался бы на страницу
// файлового редактора.
func parsePlainText(source []byte) []draft {
	paragraphs := blankLines(string(source))
	out := make([]draft, 0, len(paragraphs))
	for _, text := range paragraphs {
		out = append(out, draft{kind: domain.BlockKindParagraph, text: text})
	}
	return out
}
