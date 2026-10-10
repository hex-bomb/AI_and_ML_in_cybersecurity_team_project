package indexing_test

import (
	"strings"
	"testing"

	"github.com/kbsearch/backend-go/internal/application/indexing"
)

// TestEstimateTokensMatchesPython сверяет счётчик с эталоном. Python считает
// len(str) в кодах Python 3, то есть в символах Unicode, поэтому эталонные
// значения посчитаны на том же принципе — важна проверка именно кириллицы.
func TestEstimateTokensMatchesPython(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"пустая строка", "", 0},
		{"только пробелы", "   \n\t ", 0},
		// Непустой текст короче 4 символов даёт минимум 1, а не 0.
		{"один символ", "a", 1},
		{"три символа", "abc", 1},
		{"ровно четыре", "abcd", 1},
		{"пять", "abcde", 1},
		{"восемь", "abcdefgh", 2},
		{"40 символов", strings.Repeat("a", 40), 10},
		{"41 символ", strings.Repeat("a", 41), 10},
		{"44 символа", strings.Repeat("a", 44), 11},
		// Пробелы по краям не считаются: строка обрезается перед делением.
		{"обрезка краёв", "  abcdefgh  ", 2},
		{"перевод строки внутри", "ab\ncd\nefgh", 2},
		// Кириллица: 8 символов = 2 токена, хотя в UTF-8 это 16 байт.
		{"кириллица 8 символов", "Инструкция", 2},
		{"кириллица 40 символов", strings.Repeat("я", 40), 10},
		// Эмодзи вне BMP: один символ, четыре байта.
		{"эмодзи", "\U0001F600\U0001F600\U0001F600\U0001F600", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := indexing.EstimateTokens(tc.in); got != tc.want {
				t.Errorf("EstimateTokens(%q) = %d, ожидалось %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestEstimateTokensCountsRunesNotBytes фиксирует ключевое отличие от
// len(s) в байтах. Счёт по байтам завышал бы русский текст вдвое и резал
// чанки вдвое мельче, а смена CharsPerToken из-за этого сломала бы
// границы всех ранее проиндексированных версий.
func TestEstimateTokensCountsRunesNotBytes(t *testing.T) {
	text := strings.Repeat("я", 40) // 40 символов, 80 байт
	if got := indexing.EstimateTokens(text); got != 10 {
		t.Errorf("EstimateTokens(40 кириллических символов) = %d, ожидалось 10", got)
	}
	if len(text) != 80 {
		t.Fatalf("проверка сломана: ожидалось 80 байт, получилось %d", len(text))
	}
}

// TestEstimateTokensNeverNegativeOrZeroForVisibleText: текст с видимыми
// символами не должен давать 0 токенов — иначе он не занимает места в
// бюджете и цикл деления может не завершиться.
func TestEstimateTokensNeverNegativeOrZeroForVisibleText(t *testing.T) {
	for _, in := range []string{"a", "ab", "abc", " аб "} {
		if got := indexing.EstimateTokens(in); got < 1 {
			t.Errorf("EstimateTokens(%q) = %d, ожидалось >= 1", in, got)
		}
	}
}

// TestEstimateTokensMatchesPythonOnWhitespace: текст из одних пробелов и
// переводов строк даёт 0 — ровно как в эталоне
// `app.indexing.chunker.estimate_tokens`, где `text.strip()` пуст и функция
// выходит раньше `max(1, …)`.
//
// Значение зафиксировано сверкой с Python, а не выведено из желаемого
// поведения: chunk_id считается от границ чанков, и расхождение с эталоном
// здесь делало бы переиндексацию несовместимой с уже записанными версиями.
func TestEstimateTokensMatchesPythonOnWhitespace(t *testing.T) {
	for _, in := range []string{"", " ", "\n", "\t\t", " \n\t "} {
		if got := indexing.EstimateTokens(in); got != 0 {
			t.Errorf("EstimateTokens(%q) = %d, эталон Python даёт 0", in, got)
		}
	}
}
