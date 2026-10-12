package documents_test

import (
	"archive/zip"
	"bytes"
)

// buildDocx собирает минимальный DOCX: zip с единственным word/document.xml,
// где каждый элемент переданного среза — один абзац <w:p>.
//
// Архив пишется тестом, а не хранится в репозитории: бинарный файл в git
// нечитаем, а структуру документа (границы абзацев, текстовые прогоны)
// тест всё равно задаёт руками.
func buildDocx(paragraphs []string) []byte {
	body := `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>`
	for _, p := range paragraphs {
		body += p
	}
	body += `</w:body></w:document>`
	return buildDocxRaw(map[string]string{"word/document.xml": body})
}

// buildDocxRaw собирает DOCX с произвольным набором записей архива.
func buildDocxRaw(entries map[string]string) []byte {
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	for name, content := range entries {
		file, err := writer.Create(name)
		if err != nil {
			panic("создание записи docx: " + err.Error())
		}
		if _, err := file.Write([]byte(content)); err != nil {
			panic("запись содержимого docx: " + err.Error())
		}
	}
	if err := writer.Close(); err != nil {
		panic("закрытие архива docx: " + err.Error())
	}
	return buf.Bytes()
}
