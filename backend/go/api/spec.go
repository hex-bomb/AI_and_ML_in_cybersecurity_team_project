package contracts

import _ "embed"

//go:embed openapi.json
var specJSON []byte

// Spec возвращает встроенную копию contracts/openapi.json.
// Копия, а не чтение с диска: бинарь distroless не видит файлы репозитория.
// Синхронность с оригиналом проверяет TestEmbeddedSpecMatchesFile.
func Spec() []byte {
	out := make([]byte, len(specJSON))
	copy(out, specJSON)
	return out
}
