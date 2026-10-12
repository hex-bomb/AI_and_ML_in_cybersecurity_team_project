package arch

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const moduleRoot = "../.."

const modulePrefix = "github.com/kbsearch/kb/"

// Слои по схеме развёртывания (kb-architecture.drawio, стр. 2):
// домен → порты → use-cases → транспорт, и адаптеры, реализующие порты.
var forbidden = map[string][]string{
	"internal/envelope": {
		modulePrefix + "internal/ports",
		modulePrefix + "internal/ingest",
		modulePrefix + "internal/search",
		modulePrefix + "internal/access",
		modulePrefix + "internal/docs",
		modulePrefix + "internal/jobs",
		modulePrefix + "internal/store",
		modulePrefix + "internal/api",
		"github.com/jackc/pgx",
		"github.com/elastic/go-elasticsearch",
		"github.com/knights-analytics/hugot",
		"github.com/tmc/langchaingo",
	},
	"internal/ports": {
		modulePrefix + "internal/store",
		modulePrefix + "internal/api",
	},
	"internal/store": {
		modulePrefix + "internal/api",
		modulePrefix + "internal/search",
		modulePrefix + "internal/access",
		modulePrefix + "internal/ingest",
		modulePrefix + "internal/docs",
		modulePrefix + "internal/jobs",
	},
	"internal/api": {
		modulePrefix + "internal/store",
	},
}

// useCaseLayers — сценарии, которым запрещён прямой импорт адаптеров.
var useCaseLayers = []string{
	"internal/ingest",
	"internal/search",
	"internal/access",
	"internal/docs",
	"internal/jobs",
}

var domainAllows = map[string]bool{
	"github.com/google/uuid": true,
}

func TestLayerImportsAreOneDirectional(t *testing.T) {
	for layer, banned := range forbidden {
		t.Run(layer, func(t *testing.T) {
			dir := filepath.Join(moduleRoot, layer)
			if _, err := os.Stat(dir); err != nil {
				t.Skipf("слой %s ещё не создан", layer)
			}

			imports, files, err := collectImports(dir)
			if err != nil {
				t.Fatalf("разбор слоя: %v", err)
			}
			if files == 0 {
				t.Fatalf("в слое %s не нашлось ни одного проверяемого файла", layer)
			}
			for _, imp := range imports {
				for _, bad := range banned {
					if matches(imp.path, bad) {
						t.Errorf("%s импортирует %q — слой %s не должен зависеть от него",
							imp.file, imp.path, layer)
					}
				}
			}
		})
	}
}

// TestPortsLiveInTheirOwnLayer ensures ports are declared in internal/ports,
// while the envelope keeps only value types, invariants and errors.
func TestPortsLiveInTheirOwnLayer(t *testing.T) {
	ports := []string{"ports.go", "sql.go", "repos.go", "clock.go"}
	for _, name := range ports {
		if _, err := os.Stat(filepath.Join(moduleRoot, "internal/ports", name)); err != nil {
			t.Errorf("порт %s отсутствует в internal/ports: %v", name, err)
		}
	}
	for _, name := range []string{"ports.go", "pgshim.go", "clock.go"} {
		if _, err := os.Stat(filepath.Join(moduleRoot, "internal/envelope", name)); err == nil {
			t.Errorf("порт %s остался в internal/envelope", name)
		}
	}
}

// allowedFeatureImports — документированный долг: одни сценарии пока
// импортируют других напрямую (например, ответы — поиск и настройки).
// Список закрытый и может только уменьшаться: новая прямая ссылка между
// сценариями провалит тест. По схеме развёртывания (стр. 2, L2) сценарии
// общаются только через порты и события, и этот список — план разбора.
var allowedFeatureImports = map[string][]string{
	modulePrefix + "internal/search/answers": {
		modulePrefix + "internal/search/service",
		modulePrefix + "internal/docs/settings",
	},
	modulePrefix + "internal/search/service": {
		modulePrefix + "internal/docs/settings",
	},
	modulePrefix + "internal/jobs/indexing": {
		modulePrefix + "internal/ingest/parser",
	},
	modulePrefix + "internal/jobs/worker": {
		modulePrefix + "internal/jobs/indexing",
	},
	modulePrefix + "internal/docs/kb": {
		modulePrefix + "internal/docs/settings",
	},
}

// adapterImports — адаптеры, которые сценарий узнаёт только через порты.
// Список вынесен из forbidden["internal/store"] намеренно: тот описывает, что
// не должен импортировать слой store, и про сами адаптеры не говорит ничего,
// хотя имя теста обещает запрет ровно их. Без этого списка прямой импорт
// pgx или клиента Elasticsearch в сценарии проходил молча.
var adapterImports = []string{
	modulePrefix + "internal/store",
	"github.com/jackc/pgx",
	"github.com/elastic/go-elasticsearch",
	"github.com/knights-analytics/hugot",
	"github.com/tmc/langchaingo",
}

// TestUseCasesDoNotImportAdapters — сценарий узнаёт внешний мир только через
// порты. Прямой импорт pgx или клиента Elasticsearch здесь означает, что
// подменить его заглушкой в тесте уже не получится.
//
// Список banned взят у internal/store, а значит включает и сами слои
// сценариев: такой импорт дублирует TestFeatureCrossImportsOnlyShrink.
// Раньше список allowed проверялся в двух местах, и один и тот же долг
// назывался то допустимым, то запрещённым; источник правды один —
// allowedFeatureImports, поэтому здесь долг пропускается, а всё остальное
// (адаптеры, транспорт, новая прямая ссылка) по-прежнему роняет тест.
func TestUseCasesDoNotImportAdapters(t *testing.T) {
	banned := slices.Concat(forbidden["internal/store"], adapterImports)
	for _, layer := range useCaseLayers {
		t.Run(layer, func(t *testing.T) {
			imports, files, err := collectImports(filepath.Join(moduleRoot, layer))
			if err != nil {
				t.Fatalf("разбор слоя: %v", err)
			}
			if files == 0 {
				t.Fatalf("в слое %s не нашлось ни одного проверяемого файла", layer)
			}
			for _, imp := range imports {
				for _, bad := range banned {
					if !matches(imp.path, bad) || documentedDebt(imp.file, imp.path) {
						continue
					}
					t.Errorf("%s импортирует %q — сценарий работает через порты",
						imp.file, imp.path)
				}
			}
		})
	}
}

// documentedDebt сообщает, входит ли импорт path в документированный долг
// пакета, которому принадлежит file. Ключ allowlist — пакет сценария,
// а не слой: внутри слоя internal/search долг answers и долг service
// разные, и сводить их к одному было бы новой поблажкой.
func documentedDebt(file, path string) bool {
	rel, err := filepath.Rel(moduleRoot, file)
	if err != nil {
		return false
	}
	from := modulePrefix + filepath.ToSlash(filepath.Dir(rel))
	for _, allowed := range allowedFeatureImports[from] {
		if matches(path, allowed) {
			return true
		}
	}
	return false
}

// TestFeatureCrossImportsOnlyShrink — новая прямая зависимость между
// сценариями запрещена: её место в ports или в событиях.
func TestFeatureCrossImportsOnlyShrink(t *testing.T) {
	for from, allowed := range allowedFeatureImports {
		imports, _, err := collectImports(filepath.Join(moduleRoot, strings.TrimPrefix(from, modulePrefix)))
		if err != nil {
			t.Fatalf("разбор %s: %v", from, err)
		}
		for _, imp := range imports {
			if !strings.HasPrefix(imp.path, modulePrefix+"internal/") {
				continue
			}
			feature := strings.TrimSuffix(imp.path, "/")
			if isUseCaseFeature(feature) && !slices.Contains(allowed, feature) &&
				!slices.Contains(allowed, feature+"/") {
				t.Errorf("%s импортирует %q — сценарии общаются через порты и события; "+
					"если зависимость неизбежна, добавьте её в allowedFeatureImports с причиной",
					from, imp.path)
			}
		}
	}
}

// isUseCaseFeature сообщает, относится ли пакет к слою сценариев.
func isUseCaseFeature(path string) bool {
	for _, layer := range useCaseLayers {
		if path == modulePrefix+layer || strings.HasPrefix(path, modulePrefix+layer+"/") {
			return true
		}
	}
	return false
}

func TestEnvelopeImportsOnlyValueTypes(t *testing.T) {
	dir := filepath.Join(moduleRoot, "internal/envelope")
	imports, files, err := collectImports(dir)
	if err != nil {
		t.Fatalf("разбор конверта: %v", err)
	}
	if files == 0 {
		t.Fatal("в конверте не нашлось ни одного проверяемого файла")
	}
	for _, imp := range imports {
		first := strings.SplitN(imp.path, "/", 2)[0]
		if !strings.Contains(first, ".") || domainAllows[imp.path] {
			continue
		}
		t.Errorf("%s импортирует %q — в домене допустимы только стандартная "+
			"библиотека и типы-значения из internal/arch: %v",
			imp.file, imp.path, domainAllows)
	}
}

type foundImport struct {
	file string
	path string
}

func collectImports(dir string) ([]foundImport, int, error) {
	fset := token.NewFileSet()
	var out []foundImport
	files := 0

	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		files++
		for _, spec := range f.Imports {
			p, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			out = append(out, foundImport{file: fset.Position(spec.Pos()).Filename, path: p})
		}
		return nil
	})
	return out, files, err
}

func matches(path, banned string) bool {
	return path == banned || strings.HasPrefix(path, banned+"/")
}
