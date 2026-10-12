// Package documents — приём и разбор документов.
//
// Разбор вынесен в отдельный слой по одной причине: без него стадия convert
// нечем было бы заняться, версия оставалась бы без parsed.json, а стадия
// index — всегда падала бы на отсутствии разобранного документа.
package documents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/google/uuid"

	"github.com/kbsearch/kb/internal/envelope"
)

// ConversionRequest identifies the version being converted.
type ConversionRequest struct {
	TenantID   uuid.UUID
	KBID       uuid.UUID
	DocumentID uuid.UUID
	VersionID  uuid.UUID
	Title      string
	Filename   string
	SourceURL  *string
	Metadata   map[string]any
}

// SchemaVersion — версия контракта ParsedDocument.
//
// Должна совпадать со schemaVersion стадии index: чанк ссылается на блоки
// разобранного документа, и разные версии схемы в паре «файл ↔ читатель»
// означали бы, что читатель не знает, что во что превратилось.
const SchemaVersion = "1.0"

// Convert разбирает файл версии в ParsedDocument (ТЗ 3.2).
//
// Формат определяется расширением имени файла, а не содержимым: тот же
// файл, сохранённый под другим именем, обязан разбираться одинаково —
// иначе повтор загрузки дал бы другой ParsedDocument для тех же байт.
//
// Ошибка возвращается всегда: пустой ParsedDocument без ошибки прошёл бы
// дальше в индекс и документ считался бы обработанным, ни разу не
// найдясь по поиску.
func Convert(ctx context.Context, content []byte, req ConversionRequest) (domain.ParsedDocument, error) {
	if err := ctx.Err(); err != nil {
		return domain.ParsedDocument{}, err
	}

	suffix, err := ValidateFormat(ctx, req.Filename, content)
	if err != nil {
		return domain.ParsedDocument{}, err
	}

	drafts, err := parseDrafts(suffix, content)
	if err != nil {
		return domain.ParsedDocument{}, err
	}

	blocks := toBlocks(drafts, req.VersionID)
	if len(blocks) == 0 {
		// Пустой результат — не успех. ТЗ 3.2 требует вернуть EMPTY_DOCUMENT
		// или OCR_REQUIRED, а не успешный пустой документ: иначе версия
		// дошла бы до статуса ready и исчезла бы из внимания оператора.
		return domain.ParsedDocument{}, domain.InvalidMetadata(
			"Из файла не извлечён текст: возможно, это скан без распознанного слоя",
		).WithDetails(map[string]any{
			"filename": req.Filename, "format": suffix,
		})
	}

	// Права нормализуются ДО сборки ParsedDocument: §4 и §6 требуют, чтобы ACL
	// ехал вместе с документом на каждом шаге, включая индекс, и разбор —
	// последнее место, где неверная форма поля прав ещё видна как ошибка
	// ввода, а не как готовый документ, который не ищется. Карта уже скопирована,
	// поэтому нормализация не трогает метаданные вызывающего.
	metadata := copyMetadata(req.Metadata)
	if err := normalizeRightsMetadata(metadata); err != nil {
		return domain.ParsedDocument{}, err
	}

	sum := sha256.Sum256(content)
	return domain.ParsedDocument{
		SchemaVersion: SchemaVersion,
		TenantID:      req.TenantID,
		KBID:          req.KBID,
		DocumentID:    req.DocumentID,
		VersionID:     req.VersionID,
		Title:         req.Title,
		Filename:      req.Filename,
		ContentSHA256: hex.EncodeToString(sum[:]),
		SourceURL:     copyString(req.SourceURL),
		Metadata:      metadata,
		Blocks:        blocks,
	}, nil
}

// parseDrafts выбирает разбор по расширению.
//
// Ошибки разбора zip и PDF — это ошибки формата, а не внутренняя ошибка:
// файл может быть битым по виду пользователя, и такой отказ повторять бессмысленно.
func parseDrafts(suffix string, content []byte) ([]draft, error) {
	switch suffix {
	case ".md":
		return parseMarkdown(content), nil
	case ".txt":
		return parsePlainText(content), nil
	case ".pdf":
		drafts, err := parsePDF(content)
		if err != nil {
			return nil, invalidDocument("PDF не читается", suffix, err)
		}
		return drafts, nil
	case ".docx":
		drafts, err := parseDocx(content)
		if err != nil {
			// Разбор сам уже вернул AppError с нужным кодом (битый XML,
			// отсутствующий document.xml) — оборачивать повторно незачем.
			if _, ok := domain.AsAppError(err); ok {
				return nil, err
			}
			return nil, invalidDocument("DOCX не читается", suffix, err)
		}
		return drafts, nil
	default:
		return nil, domain.UnsupportedFormat("Поддерживаются MD, TXT, DOCX и текстовые PDF")
	}
}

// invalidDocument превращает ошибку разбора файла в INVALID_METADATA.
//
// Почему не «внутренняя ошибка»: повреждённый файл приходит от пользователя,
// и его повтор не исправит — задание должно завершиться, а версия уйти в
// failed с понятным оператору кодом.
func invalidDocument(message, suffix string, cause error) error {
	return domain.InvalidMetadata(message).
		WithDetails(map[string]any{"format": suffix}).
		WithCause(cause)
}

// copyString возвращает копию указателя: ParsedDocument переживает вызывающего,
// и общий указатель дал бы позднюю подмену значения.
func copyString(value *string) *string {
	if value == nil {
		return nil
	}
	out := *value
	return &out
}

// copyMetadata копирует карту: разобранный документ уходит в JSON на диск и
// в индекс, а общая с вызывающим карта дала бы подмену метаданных после
// записи файла.
func copyMetadata(value map[string]any) map[string]any {
	out := make(map[string]any, len(value))
	for k, v := range value {
		out[k] = v
	}
	return out
}
