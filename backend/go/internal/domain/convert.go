package domain

// ConvertVersion is the version row plus the original bytes read for parsing.
//
// WHY это значение, а не два вызова репозитория: конвертеру нужны и атрибуты
// версии (имя файла, title, source_url, metadata), и сами байты оригинала.
// Читать их отдельно означало бы два разных снимка состояния версии — между
// ними Retry успел бы сменить статус, и разобранное не соответствовало бы
// тому, что лежит в базе.
type ConvertVersion struct {
	Filename      string
	Title         string
	SourceURL     *string
	Metadata      map[string]any
	ContentSHA256 string
	Content       []byte
}

// ConversionResult is the convert-stage outcome written to the queue.
//
// ParsedKey — ключ записанного ParsedDocument в FileStorage: стадия index
// читает разобранный документ по ключу из базы, и без него версия выглядела
// бы разобранной при пустом файле.
type ConversionResult struct {
	VersionID  string
	ParsedKey  string
	BlockCount int
}
