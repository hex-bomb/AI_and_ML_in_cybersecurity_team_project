package arch

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const moduleRoot = "../.."

const modulePrefix = "github.com/kbsearch/backend-go/"

var forbidden = map[string][]string{
	"internal/domain": {
		modulePrefix + "internal/application",
		modulePrefix + "internal/delivery",
		modulePrefix + "internal/infrastructure",
		"github.com/jackc/pgx",
		"github.com/elastic/go-elasticsearch",
		"github.com/knights-analytics/hugot",
		"github.com/tmc/langchaingo",
	},
	"internal/application": {
		modulePrefix + "internal/delivery",
		modulePrefix + "internal/infrastructure",
	},
	"internal/delivery": {
		modulePrefix + "internal/infrastructure",
	},
	"internal/infrastructure": {
		modulePrefix + "internal/application/answers",
		modulePrefix + "internal/application/auth",
		modulePrefix + "internal/application/documents",
		modulePrefix + "internal/application/indexing",
		modulePrefix + "internal/application/jobs",
		modulePrefix + "internal/application/kb",
		modulePrefix + "internal/application/search",
		modulePrefix + "internal/application/settings",
		modulePrefix + "internal/delivery",
	},
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

// TestPortsLiveInApplication ensures ports are declared in application/ports,
// while domain keeps only value types, invariants and errors.
func TestPortsLiveInApplication(t *testing.T) {
	ports := []string{"ports.go", "sql.go", "repos.go", "clock.go"}
	for _, name := range ports {
		if _, err := os.Stat(filepath.Join(moduleRoot, "internal/application/ports", name)); err != nil {
			t.Errorf("порт %s отсутствует в internal/application/ports: %v", name, err)
		}
	}
	for _, name := range []string{"ports.go", "pgshim.go", "clock.go"} {
		if _, err := os.Stat(filepath.Join(moduleRoot, "internal/domain", name)); err == nil {
			t.Errorf("инфраструктурный порт %s остался в internal/domain", name)
		}
	}
}

func TestDomainImportsOnlyValueTypes(t *testing.T) {
	dir := filepath.Join(moduleRoot, "internal/domain")
	imports, files, err := collectImports(dir)
	if err != nil {
		t.Fatalf("разбор домена: %v", err)
	}
	if files == 0 {
		t.Fatal("в домене не нашлось ни одного проверяемого файла")
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
