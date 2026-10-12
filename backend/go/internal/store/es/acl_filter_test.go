package elasticsearch

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	domain "github.com/kbsearch/kb/internal/envelope"
)

// Тесты этого файла не требуют кластера: они проверяют форму клозы прав,
// которая уехала бы в сеть. Интеграция — в elasticsearch_integration_test.go.

func scopedScope() domain.AccessScope {
	return domain.AccessScope{
		TenantID:   "tenant-1",
		KBID:       "kb-1",
		SpaceIDs:   []string{"space-hr", "space-platform"},
		Principals: []string{"user:anna", "group:team-backend", "role:admin"},
	}
}

// aclJSON сериализует клозу так же, как она уедет в Elasticsearch.
func aclJSON(t *testing.T, clause any) string {
	t.Helper()
	blob, err := json.Marshal(clause)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(blob)
}

// TestКлозаПравСовпадаетСФормойДорожнойКарты — эталон §6: term тенанта плюс
// should из space_id и acl с minimum_should_match. Форма зафиксирована целиком,
// чтобы переименование поля или потеря minimum_should_match упали здесь, а не
// на проде в виде «пользователь не видит своего».
func TestКлозаПравСовпадаетСФормойДорожнойКарты(t *testing.T) {
	want := `{"bool":{"minimum_should_match":1,"should":[` +
		`{"terms":{"space_id":["space-hr","space-platform"]}},` +
		`{"terms":{"acl":["group:team-backend","role:admin","user:anna"]}}]}}`
	got := aclJSON(t, aclAccessClause(scopedScope()))
	if got != want {
		t.Errorf("форма клозы прав разошлась с дорожной картой:\n получено: %s\n ожидалось: %s", got, want)
	}
}

// TestПустаяОбластьЗакрываетДоступ: область без прав — это «нечего не видно».
// Отсутствие же фильтра означало бы «видно всё», то есть полный обход прав.
//
// Гарантию даёт КЛОЗА, а не domain.AccessScope.AllowsNothing(): тот смотрит на
// длины срезов и потому промахивается на области из одних пробелов. Адаптер
// такой запрос всё равно отправит в сеть — и обязан отправить его закрытым,
// поэтому последний кейс проверяет непустой scope.
func TestПустаяОбластьЗакрываетДоступ(t *testing.T) {
	cases := map[string]struct {
		scope        domain.AccessScope
		wantShortcut bool
	}{
		"ничего не задано": {domain.AccessScope{TenantID: "tenant-1", KBID: "kb-1"}, true},
		"пустые срезы": {
			domain.AccessScope{TenantID: "tenant-1", SpaceIDs: []string{}, Principals: []string{}},
			true,
		},
		"только пробелы": {
			domain.AccessScope{TenantID: "tenant-1", SpaceIDs: []string{" ", ""}, Principals: []string{"\t"}},
			false,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := tc.scope.AllowsNothing(); got != tc.wantShortcut {
				t.Errorf("AllowsNothing = %v, ожидалось %v", got, tc.wantShortcut)
			}
			got := aclJSON(t, aclAccessClause(tc.scope))
			if got != `{"match_none":{}}` {
				t.Fatalf("область без прав дала %s, ожидался match_none", got)
			}
			if strings.Contains(got, "null") {
				t.Errorf("в закрытом запросе есть null: %s", got)
			}
			if strings.Contains(got, "terms") {
				t.Errorf("закрытый запрос упоминает terms: %s", got)
			}
		})
	}
}

// TestПустаяОбластьНеСериализуетсяКакNull повторяет защиту filterClauses для
// document_id: null в terms — это «не ограничивать», худший из возможных
// исходов при опечатке в коде сборки запроса.
func TestПустаяОбластьНеСериализуетсяКакNull(t *testing.T) {
	terms := aclTerms(nil)
	if terms == nil {
		t.Fatal("aclTerms(nil) вернул nil: такой срез сериализуется как null")
	}
	if got := aclJSON(t, map[string]any{"terms": map[string]any{aclFieldSpace: terms}}); got != `{"terms":{"space_id":[]}}` {
		t.Errorf("пустые термы сериализованы как %s, ожидался пустой массив", got)
	}
}

// TestВеткиПравСобираютсяПоФакту: пустую ветку в should не добавляем (terms с
// пустым массивом не матчит ничего), но minimum_should_match остаётся всегда —
// он и задаёт контракт «хотя бы одна ветка совпала».
func TestВеткиПравСобираютсяПоФакту(t *testing.T) {
	cases := []struct {
		name      string
		scope     domain.AccessScope
		wantTerms int
		wantEmpty bool
	}{
		{
			name:      "только контейнеры",
			scope:     domain.AccessScope{SpaceIDs: []string{"space-hr"}},
			wantTerms: 1,
		},
		{
			name:      "только принципалы",
			scope:     domain.AccessScope{Principals: []string{"user:anna"}},
			wantTerms: 1,
		},
		{
			name:      "контейнеры и принципалы",
			scope:     scopedScope(),
			wantTerms: 2,
		},
		{
			name:      "прав нет",
			scope:     domain.AccessScope{TenantID: "tenant-1"},
			wantTerms: 0,
			wantEmpty: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clause, ok := aclAccessClause(tc.scope).(map[string]any)
			if !ok {
				t.Fatalf("клоза прав не map: %T", aclAccessClause(tc.scope))
			}
			if tc.wantEmpty {
				if _, isMatchNone := clause["match_none"]; !isMatchNone {
					t.Fatalf("без прав ожидался match_none, получено: %v", clause)
				}
				return
			}
			inner := clause["bool"].(map[string]any)
			should := inner["should"].([]any)
			if len(should) != tc.wantTerms {
				t.Errorf("веток should = %d, ожидалось %d: %v", len(should), tc.wantTerms, should)
			}
			if inner["minimum_should_match"] != 1 {
				t.Errorf("minimum_should_match = %v, ожидалось 1", inner["minimum_should_match"])
			}
		})
	}
}

// TestТермыЧистятсяОтПустыхСтрокИДублей: пустой идентификатор пространства или
// принципала в terms бесполезен и засоряет тело запроса, дубли — тем более.
func TestТермыЧистятсяОтПустыхСтрокИДублей(t *testing.T) {
	got := aclTerms([]string{" user:anna ", "", "   ", "user:anna", "group:team-backend"})
	want := []string{"group:team-backend", "user:anna"}
	if !slices.Equal(got, want) {
		t.Errorf("aclTerms = %v, ожидалось %v", got, want)
	}
}

// TestПорядокТермовНеЗависитОтВхода: список прав приходит из кэша с произвольным
// порядком, а тело запроса обязано быть воспроизводимым — иначе diff тел в логах
// и «мигающие» тесты.
func TestПорядокТермовНеЗависитОтВхода(t *testing.T) {
	first := domain.AccessScope{SpaceIDs: []string{"b", "a", "c"}, Principals: []string{"user:z", "user:a"}}
	second := domain.AccessScope{SpaceIDs: []string{"c", "b", "a"}, Principals: []string{"user:a", "user:z"}}
	if aclJSON(t, aclAccessClause(first)) != aclJSON(t, aclAccessClause(second)) {
		t.Errorf("порядок термов зависит от входа:\n %s\n %s",
			aclJSON(t, aclAccessClause(first)), aclJSON(t, aclAccessClause(second)))
	}
}

// TestКлозаПравНеСодержитИдентификаторовДокументов — структурная защита от
// возврата к материализации: в правах нет места для списка document_id.
func TestКлозаПравНеСодержитИдентификаторовДокументов(t *testing.T) {
	if strings.Contains(aclJSON(t, aclAccessClause(scopedScope())), "document_id") {
		t.Error("в клозе прав появился список документов — это путь к post-filter")
	}
}

// TestФормаПринципалаДоходитДоЗапросаБезИскажений: строка принципала — общий
// контракт с индексом (§6: user:, role:, group:, ресурсные группы res:).
// Адаптер не имеет права её переписывать: «user:anna» и «anna» — это разные
// права, и подмена одного другим означала бы либо отказ в доступе, либо выдачу
// чужого.
func TestФормаПринципалаДоходитДоЗапросаБезИскажений(t *testing.T) {
	principals := []string{
		"user:anna",
		"role:admin",
		"group:team-backend",
		"res:space-platform:read",
		"res:space-hr:read",
	}
	for _, principal := range principals {
		t.Run(principal, func(t *testing.T) {
			clause := aclJSON(t, aclAccessClause(domain.AccessScope{Principals: []string{principal}}))
			if !strings.Contains(clause, `"`+principal+`"`) {
				t.Errorf("принципал %s не дошёл до запроса без изменений: %s", principal, clause)
			}
		})
	}
}

// TestПринципалыИзAccessScopeForПопадаютВФильтрКакЕсть соединяет конверт и
// адаптер: разворот пользователя в список принципалов (§5 шаг 1) обязан давать
// ровно те строки, которые потом сравниваются с полем acl документа.
func TestПринципалыИзAccessScopeForПопадаютВФильтрКакЕсть(t *testing.T) {
	principal := domain.Principal{
		TenantID: uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		UserID:   "anna",
		Roles:    []string{"admin"},
		Groups:   []string{"team-backend"},
	}
	scope := domain.AccessScopeFor(principal, "kb-1", []string{"space-platform"})
	clause := aclJSON(t, aclAccessClause(scope))

	for _, want := range []string{
		"user:anna",
		"role:admin",
		"group:team-backend",
		"space-platform",
	} {
		if !strings.Contains(clause, `"`+want+`"`) {
			t.Errorf("в фильтре нет %s: %s", want, clause)
		}
	}
	if scope.AllowsNothing() {
		t.Error("область с правами не должна считаться пустой")
	}
}

// aclFieldsMissingFromIndex — поля §6, которых в mapping'е индекса пока нет:
// индекс сегодня пишет конверт без прав (см. долг в acl_filter.go). Отсутствие
// поля означает, что terms по нему не совпадёт ни с одним документом, то есть
// поиск закрыт, а не открыт.
var aclFieldsMissingFromIndex = []string{aclFieldSpace, aclFieldPrincipals}

// TestПоляПравСогласованыСМаппингомИндекса: любое поле, по которому фильтрует
// адаптер, обязано быть объявлено в mapping'е как keyword. Пока нет — запрос
// молча ничего не находит; расхождение типа (text вместо keyword) — тоже
// молчание, потому что анализатор разобьёт «user:anna» на части.
func TestПоляПравСогласованыСМаппингомИндекса(t *testing.T) {
	props, ok := indexBody(dimension)["mappings"].(map[string]any)["properties"].(map[string]any)
	if !ok {
		t.Fatal("в indexBody нет свойств mapping'а")
	}
	for _, field := range []string{
		"tenant_id", "kb_id", "embedding_profile_id", aclFieldSpace, aclFieldPrincipals,
	} {
		prop, declared := props[field]
		if !declared {
			if !slices.Contains(aclFieldsMissingFromIndex, field) {
				t.Errorf("поле %s не объявлено в mapping'е индекса: запрос по нему ничего не найдёт",
					field)
			}
			continue
		}
		t.Logf("поле %s объявлено в mapping'е", field)
		if got := prop.(map[string]any)["type"]; got != "keyword" {
			t.Errorf("тип поля %s = %v, ожидался keyword", field, got)
		}
	}
}
