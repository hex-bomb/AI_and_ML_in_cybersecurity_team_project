package documents

import (
	"strconv"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"

	domain "github.com/kbsearch/kb/internal/envelope"
)

// Разбор Markdown через goldmark.
//
// WHY AST, а не регулярные выражения: разметка вложенная (заголовки внутри
// списков, списки внутри блоков цитат), и регуляркой уровень заголовка и путь
// раздела восстановить нельзя — заголовок нашёлся бы, но не на своём месте.

// parseMarkdown раскладывает MD на блоки с section_path из заголовков.
func parseMarkdown(source []byte) []draft {
	normalized := []byte(normalizeText(string(source)))
	root := goldmark.DefaultParser().Parse(text.NewReader(normalized))

	b := &markdownBuilder{}
	for child := root.FirstChild(); child != nil; child = child.NextSibling() {
		b.walk(normalized, child)
	}
	return b.blocks
}

// markdownBuilder держит разбор во время обхода.
//
// WHY сборщик, а не передача срезов по цепочке: заголовок меняет путь раздела
// для всех следующих блоков, поэтому состояние проходит через всё дерево.
// Сборщик делает его накопление явным и избавляет от ошибки «присвоили новый
// срез — вызывающий ничего не увидел».
type markdownBuilder struct {
	blocks []draft
	// headings — стек заголовков по уровню: элемент i отвечает за заголовок
	// уровня i+1. Стек, а не одна переменная, потому что `## B` после
	// `# A > ### C` обязан вернуть путь ["A","B"], а не ["A","C","B"].
	headings []string
}

// walk разбирает один узел верхнего уровня.
//
// Почему только верхний уровень: обход в глубину здесь означал бы, что
// абзац внутри элемента списка попал бы в блок дважды — отдельным блоком и
// частью блока списка.
func (b *markdownBuilder) walk(source []byte, node ast.Node) {
	switch n := node.(type) {
	case *ast.Heading:
		heading := inlineText(source, n)
		if heading == "" {
			// Заголовок без текста (`#`) не задаёт раздела: иначе в
			// section_path попал бы пустой элемент, а с ним и заголовок
			// чанка вида "Раздел > ", который ничего не ищет.
			return
		}
		// Заголовок открывает свой раздел и принадлежит ему же. Так он
		// склеивается со своим содержимым в один чанк: иначе заголовок
		// остался бы отдельным фрагментом, а раздел — разорванным.
		b.headings = pushHeading(b.headings, n.Level, heading)
		b.add(draft{
			kind: domain.BlockKindHeading, text: heading,
			sectionPath: b.path(),
		})

	case *ast.Paragraph:
		b.addParagraph(source, n)

	case *ast.List:
		// Список — один блок: разбивать его на пункты нельзя, потому что
		// чанкер склеивает соседние блоки одного раздела, и разбитый список
		// разорвал бы фразу, читаемую только целиком.
		if text := listText(source, n); text != "" {
			b.add(draft{
				kind: domain.BlockKindList, text: text, sectionPath: b.path(),
			})
		}

	case *ast.FencedCodeBlock:
		b.addCode(n.Lines().Value(source))

	case *ast.CodeBlock:
		b.addCode(n.Lines().Value(source))

	case *ast.Blockquote:
		// Цитата разбирается как её содержимое: отдельный вид «цитата» в
		// ТЗ 3.2 не предусмотрен, а текст цитаты искать нужно.
		for child := n.FirstChild(); child != nil; child = child.NextSibling() {
			b.walk(source, child)
		}
	}
}

// add кладёт черновик с текущим путём раздела.
func (b *markdownBuilder) add(d draft) {
	d.sectionPath = b.path()
	b.blocks = append(b.blocks, d)
}

// addParagraph добавляет абзац, непустой после очистки.
func (b *markdownBuilder) addParagraph(source []byte, n ast.Node) {
	if text := cleanBlockText(inlineText(source, n)); text != "" {
		b.add(draft{kind: domain.BlockKindParagraph, text: text})
	}
}

// path отдаёт копию стека заголовков: блоки хранят снимок на момент своего
// появления, а не общий срез, который меняет следующий заголовок.
func (b *markdownBuilder) path() []string { return clonePath(b.headings) }

// addCode добавляет блок кода: содержимое сохраняется дословно, потому
// что в инструкции команда и её вывод — разные вещи.
func (b *markdownBuilder) addCode(raw []byte) {
	if text := cleanBlockText(string(raw)); text != "" {
		b.add(draft{kind: domain.BlockKindCode, text: text})
	}
}

// listText собирает текст списка вместе с его пунктами.
//
// Маркер пункта сохраняется: в найденном фрагменте «- нажмите кнопку» читается
// как инструкция, а голое «нажмите кнопку» теряет принадлежность к шагу.
func listText(source []byte, list *ast.List) string {
	marker := string(list.Marker)
	ordered := list.IsOrdered()

	parts := make([]string, 0, 8)
	for item := list.FirstChild(); item != nil; item = item.NextSibling() {
		body := itemText(source, item)
		if body == "" {
			continue
		}
		parts = append(parts, marker+itemMarker(ordered, len(parts)+1)+body)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, newline)
}

// itemMarker возвращает суффикс после маркера: у нумерованного списка после
// «.» нужен пробел, у маркированного — нет («- текст», а не «-  текст»).
func itemMarker(ordered bool, position int) string {
	if !ordered {
		return ""
	}
	return " " + strconv.Itoa(position) + ". "
}

// itemText собирает текст пункта списка вместе с вложенным содержимым.
func itemText(source []byte, item ast.Node) string {
	if paragraph, ok := item.FirstChild().(*ast.Paragraph); ok {
		if rest := item.FirstChild().NextSibling(); rest == nil {
			return cleanBlockText(inlineText(source, paragraph))
		}
	}
	// Пункт с вложенным списком или несколькими абзацами: обходим всё
	// содержимое, иначе пропала бы вторая строка пункта.
	parts := make([]string, 0, 4)
	for child := item.FirstChild(); child != nil; child = child.NextSibling() {
		parts = append(parts, childText(source, child))
	}
	return cleanBlockText(strings.Join(parts, newline))
}

// childText отдаёт текст узла в зависимости от его вида.
func childText(source []byte, node ast.Node) string {
	switch n := node.(type) {
	case *ast.Paragraph:
		return inlineText(source, n)
	case *ast.List:
		return listText(source, n)
	case *ast.TextBlock:
		return string(n.Lines().Value(source))
	default:
		return inlineText(source, n)
	}
}

// pushHeading кладёт заголовок в стек по его уровню.
//
// Уровень, превышающий глубину стека (документ начинается с `##`), не
// раздувает стек пустыми элементами: section_path получает только реально
// встреченные заголовки.
func pushHeading(headings []string, level int, heading string) []string {
	if level < 1 {
		level = 1
	}
	if level > len(headings)+1 {
		level = len(headings) + 1
	}
	next := make([]string, level)
	copy(next, headings[:level-1])
	next[level-1] = heading
	return next
}

// inlineText собирает читаемый текст узла, снимая inline-разметку.
//
// WHY обход AST, а не рендерер goldmark: рендерер по умолчанию даёт HTML, и
// текст блока начинался бы с «<p>». В индекс попадает текст, по которому
// ищут, а не разметка.
//
// Обход в глубину здесь безопасен в отличие от обхода блочного уровня:
// строчные узлы не порождают блоков, они только наполняют уже выбранный.
func inlineText(source []byte, node ast.Node) string {
	var buf strings.Builder
	// Обход не может прерваться: статус всегда WalkContinue, а ошибка
	// возвращается только самим Walk и на нашем AST не возникает. Проверять
	// её всё равно нужно — молча проглоченная ошибка разбора дала бы блок
	// без текста, а такой блок выглядел бы как пустой документ.
	if err := ast.Walk(node, func(current ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch typed := current.(type) {
		case *ast.Text:
			// Жёсткий перенос строки внутри абзаца («две trailing пробелы»)
			// превращается в перенос строки: разрыв там — оформление, но он
			// часть текста, и молча склеенный он склеил бы и слова.
			if typed.HardLineBreak() {
				buf.WriteString(newline)
			}
			buf.Write(typed.Value(source))
		case *ast.String:
			buf.Write(typed.Value)
		case *ast.AutoLink:
			appendAutoLink(&buf, typed, source)
		}
		return ast.WalkContinue, nil
	}); err != nil {
		// Сбой обхода goldmark — дефект его обхода, а не документа.
		// Возвращаем накопленное: пустой блок выглядел бы как потерянный текст.
		return buf.String()
	}
	return cleanBlockText(buf.String())
}

// appendAutoLink сохраняет адрес автоссылки: <https://example.com> иначе
// потерял бы единственное, что в нём есть.
func appendAutoLink(buf *strings.Builder, node *ast.AutoLink, source []byte) {
	value := string(node.URL(source))
	if value == "" {
		return
	}
	if buf.Len() > 0 {
		buf.WriteString(" ")
	}
	buf.WriteString(value)
}

// clonePath копирует путь раздела: заголовки обновляют стек на месте, а блоки
// хранят снимок на момент своего появления.
func clonePath(headings []string) []string {
	if len(headings) == 0 {
		return []string{}
	}
	out := make([]string, len(headings))
	copy(out, headings)
	return out
}
