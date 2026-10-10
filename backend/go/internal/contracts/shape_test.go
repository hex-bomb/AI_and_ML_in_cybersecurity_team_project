package contracts_test

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Контрактные числа: 17 путей, 25 схем — фронт сгенерирован по ним, расхождение роняет контракт.

const (
	wantPaths   = 17
	wantSchemas = 25
)

// specFile — путь к спецификации относительно каталога пакета.
func specFile() string {
	return filepath.Join("..", "..", "..", "..", "contracts", "openapi.json")
}

// readSpec читает contracts/openapi.json (источник ожиданий — спецификация).
func readSpec(t *testing.T) (paths []string, schemas []string) {
	t.Helper()
	raw, err := os.ReadFile(specFile())
	if err != nil {
		t.Fatalf("читаю %s: %v", specFile(), err)
	}
	var spec struct {
		Paths      map[string]json.RawMessage `json:"paths"`
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("разбираю %s: %v", specFile(), err)
	}
	for name := range spec.Paths {
		paths = append(paths, name)
	}
	for name := range spec.Components.Schemas {
		schemas = append(schemas, name)
	}
	slices.Sort(paths)
	slices.Sort(schemas)
	return paths, schemas
}

// parseGenerated разбирает api.gen.go через go/ast (не текстовым поиском).
func parseGenerated(t *testing.T) *ast.File {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "api.gen.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("разбираю api.gen.go: %v", err)
	}
	return file
}

// serverInterfaceMethodDocs возвращает путь и метод по комментариям ServerInterface.
func serverInterfaceMethodDocs(t *testing.T, file *ast.File) map[string]string {
	t.Helper()
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name.Name != "ServerInterface" {
				continue
			}
			iface, ok := ts.Type.(*ast.InterfaceType)
			if !ok {
				t.Fatalf("ServerInterface не интерфейс, а %T", ts.Type)
			}
			out := make(map[string]string, len(iface.Methods.List))
			for _, m := range iface.Methods.List {
				if m.Doc == nil {
					continue
				}
				// Комментарий операции: имя, заголовок, (МЕТОД путь).
				doc := m.Doc.Text()
				idx := strings.LastIndex(doc, "(")
				if idx < 0 {
					continue
				}
				fields := strings.Fields(strings.TrimSuffix(strings.TrimSpace(doc[idx+1:]), ")"))
				if len(fields) != 2 {
					continue
				}
				out[fields[1]] = fields[0]
			}
			return out
		}
	}
	t.Fatal("в api.gen.go нет интерфейса ServerInterface")
	return nil
}

// goTypeName переводит имя схемы OpenAPI в имя Go-типа как oapi-codegen.
func goTypeName(schemaName string) string {
	var b strings.Builder
	for _, part := range strings.Split(schemaName, "_") {
		if part == "" {
			continue
		}
		runes := []rune(part)
		runes[0] = []rune(strings.ToUpper(string(runes[0])))[0]
		b.WriteString(string(runes))
	}
	return b.String()
}

// TestSpecHasExpectedContractSize фиксирует числа контракта (падение — сначала понять зачем).
func TestSpecHasExpectedContractSize(t *testing.T) {
	paths, schemas := readSpec(t)
	if len(paths) != wantPaths {
		t.Errorf("в contracts/openapi.json %d путей, ожидалось %d", len(paths), wantPaths)
	}
	if len(schemas) != wantSchemas {
		t.Errorf("в contracts/openapi.json %d схем, ожидалось %d", len(schemas), wantSchemas)
	}
}

// TestServerInterfaceCoversEveryPath проверяет покрытие путей методами ServerInterface.
func TestServerInterfaceCoversEveryPath(t *testing.T) {
	file := parseGenerated(t)
	got := serverInterfaceMethodDocs(t, file)
	spec, _ := readSpec(t)

	if len(got) != wantPaths {
		t.Errorf("в ServerInterface %d операций, ожидалось %d", len(got), wantPaths)
	}
	if len(spec) != len(got) {
		t.Fatalf("в спецификации %d путей, в ServerInterface %d операций", len(spec), len(got))
	}
	for _, path := range spec {
		if _, ok := got[path]; !ok {
			t.Errorf("путь %s есть в спецификации, но нет метода в ServerInterface", path)
		}
	}
	for path := range got {
		if !slices.Contains(spec, path) {
			t.Errorf("в ServerInterface есть операция для пути %s, которого нет в спецификации", path)
		}
	}
}

// TestEverySchemaHasGeneratedType проверяет типы схем в api.gen.go.
func TestEverySchemaHasGeneratedType(t *testing.T) {
	file := parseGenerated(t)

	declared := make(map[string]ast.Expr)
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			declared[ts.Name.Name] = ts.Type
		}
	}

	_, schemas := readSpec(t)
	if len(schemas) != wantSchemas {
		t.Fatalf("в спецификации %d схем, ожидалось %d", len(schemas), wantSchemas)
	}

	// Считаем схемы, не вспомогательные типы генератора.
	matched := 0
	for _, schema := range schemas {
		name := goTypeName(schema)
		if _, ok := declared[name]; !ok {
			t.Errorf("схема %s ожидается как тип %s, но его нет в api.gen.go", schema, name)
			continue
		}
		matched++
	}
	if matched != wantSchemas {
		t.Errorf("найдено %d типов схем, ожидалось %d", matched, wantSchemas)
	}
}

// TestGeneratedCodeCompiles ловит баг oapi-codegen с пустой схемой `{}` (метод на
// interface{}): обойдён локальным шаблоном strict-interface.tmpl.
func TestGeneratedCodeCompiles(t *testing.T) {
	cmd := exec.Command("go", "build", ".")
	cmd.Dir = "." // каталог пакета internal/contracts
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("api.gen.go не компилируется: %v\n%s\n"+
			"Похоже, oapi-codegen снова объявил `type X interface{}` с методом "+
			"(баг пустой схемы `{}`): проверьте, применяется ли локальный шаблон "+
			"strict/strict-interface.tmpl из oapi-codegen.yaml", err, out)
	}
}
