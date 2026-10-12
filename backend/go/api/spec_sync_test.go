package contracts_test

import (
	"bytes"
	"os"
	"testing"

	contracts "github.com/kbsearch/kb/api"
)

// TestEmbeddedSpecMatchesFile ловит рассинхрон встроенной копии спеки.
// Правило: cp contracts/openapi.json backend/go/api/openapi.json
func TestEmbeddedSpecMatchesFile(t *testing.T) {
	raw, err := os.ReadFile(specFile())
	if err != nil {
		t.Fatalf("читаю %s: %v", specFile(), err)
	}
	if !bytes.Equal(contracts.Spec(), raw) {
		t.Fatalf("встроенная api/openapi.json отличается от %s: скопируй файл", specFile())
	}
}
