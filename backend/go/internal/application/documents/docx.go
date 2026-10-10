package documents

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"strings"

	"github.com/kbsearch/backend-go/internal/domain"
)

// Разбор DOCX без новых зависимостей: DOCX — это zip, а текст лежит в
// word/document.xml.
//
// WHY токенный обход, а не xml.Unmarshal в структуру: у OOXML десятки типов
// элементов, и структура под каждый из них была бы файлом ради одного поля.
// Reader отдаёт только то, что нужно — границы абзацев и текстовые прогоны.

// docXMLPath — обязательная часть пакета OOXML с телом документа.
const docXMLPath = "word/document.xml"

// documentXMLMaxBytes ограничивает разбор document.xml.
//
// Защита от zip-бомбы: сжатый архив в 20 МиБ распаковывается в гигабайты, и
// без предела разбор съел бы память воркера.
const documentXMLMaxBytes = 64 << 20

// Имена элементов OOXML. Пространства имён в Reader отбрасываются, поэтому
// сравнение идёт по локальному имени.
const (
	xmlBody      = "body"
	xmlParagraph = "p"
	xmlText      = "t"
	xmlLineBreak = "br"
	xmlTab       = "tab"
)

// parseDocx раскладывает тело документа на абзацы.
func parseDocx(source []byte) ([]draft, error) {
	archive, err := zip.NewReader(bytes.NewReader(source), int64(len(source)))
	if err != nil {
		return nil, err
	}

	entry := findEntry(archive, docXMLPath)
	if entry == nil {
		return nil, domain.InvalidMetadata("DOCX не содержит word/document.xml")
	}

	paragraphs, err := readDocxParagraphs(entry)
	if err != nil {
		return nil, err
	}

	out := make([]draft, 0, len(paragraphs))
	for _, text := range paragraphs {
		if text != "" {
			out = append(out, draft{kind: domain.BlockKindParagraph, text: text})
		}
	}
	return out, nil
}

// findEntry ищет часть архива по точному имени.
//
// Сравнение регистрозависимое, как в самом формате: часть «word/Document.xml»
// в корректном DOCX не встречается, а подмена регистра в zip-читаке разных
// библиотек трактуется по-разному — здесь выбран строгий вариант.
func findEntry(archive *zip.Reader, name string) *zip.File {
	for _, file := range archive.File {
		if file.Name == name {
			return file
		}
	}
	return nil
}

// readDocxParagraphs вытаскивает абзацы: текстовые прогоны <w:t> склеиваются
// внутри абзаца, граница абзаца — <w:p>.
func readDocxParagraphs(entry *zip.File) ([]string, error) {
	reader, err := entry.Open()
	if err != nil {
		return nil, domain.InvalidMetadata("DOCX не читается").WithCause(err)
	}
	defer func() { _ = reader.Close() }()

	limited := io.LimitReader(reader, documentXMLMaxBytes)
	decoder := xml.NewDecoder(limited)
	// Строгий разбор: повреждённый XML должен отклониться, а не молча
	// разобраться в никуда и дать пустой документ.
	decoder.Strict = true

	var (
		out       []string
		current   strings.Builder
		depth     int
		inText    bool
		seenBody  bool
		parseFail error
	)
	for {
		token, tokenErr := decoder.Token()
		if errors.Is(tokenErr, io.EOF) {
			break
		}
		if tokenErr != nil {
			parseFail = domain.InvalidMetadata("DOCX содержит повреждённый XML").WithCause(tokenErr)
			break
		}

		switch typed := token.(type) {
		case xml.StartElement:
			switch typed.Name.Local {
			case xmlBody:
				seenBody = true
			case xmlParagraph:
				// depth, а не флаг: <w:p> вкладывается в ячейки таблицы, и
				// флаг сбросился бы на выходе из вложенного абзаца.
				depth++
				current.Reset()
			case xmlText:
				inText = true
			case xmlLineBreak:
				current.WriteString(newline)
			case xmlTab:
				current.WriteString(" ")
			}
		case xml.CharData:
			// Вне <w:t> символы — это форматирование XML (пробелы между
			// тегами), и в текст документа они не попадают.
			if inText {
				current.Write(typed)
			}
		case xml.EndElement:
			switch typed.Name.Local {
			case xmlText:
				inText = false
			case xmlParagraph:
				depth--
				if depth == 0 {
					out = append(out, cleanBlockText(current.String()))
					current.Reset()
				}
			}
		}
	}

	if parseFail != nil {
		return nil, parseFail
	}
	// Файл без <w:body> — не документ: на месте текста там стили или
	// метаданные, и «успешный» разбор скрыл бы потерю содержимого.
	if !seenBody {
		return nil, domain.InvalidMetadata("DOCX не содержит тела документа")
	}
	return out, nil
}
