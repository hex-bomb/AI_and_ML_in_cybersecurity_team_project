package httpapi

import (
	"github.com/kbsearch/backend-go/internal/config"
)

// apiHandler реализует ServerInterface (параметры генератора игнорируются:
// тело и query разбирает транспорт через strictJSON).
type apiHandler struct {
	cfg config.Config
	svc Services
}
