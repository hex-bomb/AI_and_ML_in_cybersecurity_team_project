package documents

import (
	"bytes"

	"github.com/ledongthuc/pdf"

	"github.com/kbsearch/backend-go/internal/domain"
)

// Разбор PDF через ledongthuc/pdf.
//
// WHY постранично: у PDF есть достоверная пагинация, и она попадает в
// locator.page_start/page_end чанка. Склеенный в один текст документ потерял
// бы ссылку на страницу, по которой нашли фрагмент.

// maxPDFPages ограничивает число страниц на версию.
//
// Защита не от «большого» файла (размер уже ограничен при загрузке), а от
// документа-бомбы: разбор каждой страницы держит память, и файл из сотен
// тысяч пустых страниц выел бы воркер. Обрезанный разбор лучше отказа, и
// пользователь увидит документ с номером страницы.
const maxPDFPages = 5000

// parsePDF раскладывает страницы PDF на блоки с номерами страниц.
func parsePDF(source []byte) ([]draft, error) {
	reader, err := pdf.NewReader(bytes.NewReader(source), int64(len(source)))
	if err != nil {
		return nil, err
	}

	pages := reader.NumPage()
	if pages > maxPDFPages {
		pages = maxPDFPages
	}

	out := make([]draft, 0, pages)
	for number := 1; number <= pages; number++ {
		text, pageErr := plainPageText(reader, number)
		if pageErr != nil {
			// Одна нечитаемая страница не должна ронять весь документ:
			// остальные страницы ищутся нормально, и потерянная страница
			// видна как отсутствие текста на её номере.
			continue
		}
		// Номер страницы копируется в каждый блок страницы, иначе он указывал
		// бы на чужой адрес памяти — значение переиспользуется в цикле.
		page := number
		for _, block := range blankLines(text) {
			out = append(out, draft{
				kind: domain.BlockKindParagraph, text: block, page: &page,
			})
		}
	}
	return out, nil
}

// plainPageText достаёт текст страницы с общим кэшем шрифтов.
//
// Кэш обязателен: без него каждая страница заново разбирает таблицу символов
// шрифта, а документ на сотни страниц тратил бы всё время на неё.
func plainPageText(reader *pdf.Reader, number int) (string, error) {
	page := reader.Page(number)
	fonts := make(map[string]*pdf.Font)
	for _, name := range page.Fonts() {
		// Уже разобранный шрифт пропускается: page.Font заново читает
		// таблицу символов, и без этой проверки страница платила бы за неё
		// дважды.
		if _, ok := fonts[name]; ok {
			continue
		}
		font := page.Font(name)
		fonts[name] = &font
	}
	return page.GetPlainText(fonts)
}
