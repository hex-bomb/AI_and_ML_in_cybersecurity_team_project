package documents_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/ledongthuc/pdf"
)

// buildPDF собирает минимальный валидный PDF с указанным текстом на каждой
// странице. Своими руками, а не фикстурой в репозитории: бинарный файл в git
// нечитаем, а текст страниц в нём всё равно должен задаваться тестом.
//
// Смещения xref считаются по фактической длине объектов — иначе библиотека
// не найдёт страницы и тест молча проверит пустой разбор.
//
// Текст страниц — ASCII: встроенного шрифта в фикстуре нет, а Helvetica со
// StandardEncoding не кодирует кириллицу. Проверка страниц от содержимого не
// зависит, а русский текст проверяется на MD/DOCX, где кодировка не вовлечена.
func buildPDF(pages []string) []byte {
	var body bytes.Buffer
	body.WriteString("%PDF-1.4\n")

	// Нумерация объектов: 1 — каталог, 2 — дерево страниц, 3 — шрифт,
	// далее на каждую страницу пара «страница, поток содержимого».
	// Шрифт объявлен третьим, чтобы пара страниц не наезжала на него при
	// любом числе страниц.
	const fontObj = 3
	total := fontObj + 2*len(pages)

	offsets := make(map[int]int, total)
	write := func(num int, content string) {
		offsets[num] = body.Len()
		fmt.Fprintf(&body, "%d 0 obj\n%s\nendobj\n", num, content)
	}

	write(1, "<< /Type /Catalog /Pages 2 0 R >>")

	kids := make([]string, 0, len(pages))
	for i := range pages {
		kids = append(kids, fmt.Sprintf("%d 0 R", fontObj+1+2*i))
	}
	write(2, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>",
		strings.Join(kids, " "), len(pages)))
	write(fontObj, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")

	for i, text := range pages {
		page := fontObj + 1 + 2*i
		content := page + 1
		write(page, fmt.Sprintf(
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] "+
				"/Resources << /Font << /F1 %d 0 R >> >> /Contents %d 0 R >>",
			fontObj, content))
		stream := fmt.Sprintf("BT /F1 12 Tf 72 720 Td (%s) Tj ET", escapePDFText(text))
		write(content, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream))
	}

	xref := body.Len()
	fmt.Fprintf(&body, "xref\n0 %d\n", total+1)
	body.WriteString("0000000000 65535 f \n")
	for num := 1; num <= total; num++ {
		fmt.Fprintf(&body, "%010d 00000 n \n", offsets[num])
	}
	fmt.Fprintf(&body, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		total+1, xref)
	return body.Bytes()
}

func escapePDFText(text string) string {
	replacer := strings.NewReplacer("\\", "\\\\", "(", "\\(", ")", "\\)")
	return replacer.Replace(text)
}

// requireReadablePDF страхует фикстуру: если сгенерированный PDF перестанет
// читаться библиотекой, тесты разбора не должны падать с непонятным «пустой
// документ» — падение обязано быть про сам файл.
func requireReadablePDF(t *testing.T, raw []byte, wantPages int) {
	t.Helper()
	reader, err := pdf.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("фикстура PDF не читается: %v", err)
	}
	if reader.NumPage() != wantPages {
		t.Fatalf("в фикстуре %d страниц, ожидалось %d", reader.NumPage(), wantPages)
	}
}
