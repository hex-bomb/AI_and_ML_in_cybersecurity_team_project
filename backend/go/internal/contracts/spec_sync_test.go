package contracts_test

import (
	"bytes"
	"os"
	"testing"

	"github.com/kbsearch/backend-go/internal/contracts"
)

// TestEmbeddedSpecMatchesFile ловит рассинхрон встроенной копии спеки.
// Правило: cp contracts/openapi.json backend/go/internal/contracts/openapi.json
func TestEmbeddedSpecMatchesFile(t *testing.T) {
	raw, err := os.ReadFile(specFile())
	if err != nil {
		t.Fatalf("читаю %s: %v", specFile(), err)
	}
	if !bytes.Equal(contracts.Spec(), raw) {
		t.Fatalf("встроенная internal/contracts/openapi.json отличается от %s: скопируй файл", specFile())
	}
}
