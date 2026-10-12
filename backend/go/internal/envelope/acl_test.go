package domain

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Тесты контракта прав §6 на стороне конверта. Кластера не требуют.
//
// Главное, что здесь проверяется: словарь принципалов и пустое значение.
// «user:anna» и «anna» — это разные права, а пустой список исключений обязан
// означать «исключений нет», а не «доступ всем».

func annaPrincipal() Principal {
	return Principal{
		TenantID: uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		UserID:   "anna",
		Roles:    []string{"admin"},
		Groups:   []string{"team-backend"},
	}
}

// TestСловарьПринципаловСовпадаетСAccessScopeFor — главная проверка формы.
// AccessScopeFor разворачивает пользователя в список принципалов для фильтра
// запроса; те же строки обязан уметь собрать индекс для поля acl документа.
// Один словарь на обе стороны — единственный способ не разъехаться «user:anna»
// и «anna» тихо, без ошибки.
func TestСловарьПринципаловСовпадаетСAccessScopeFor(t *testing.T) {
	scope := AccessScopeFor(annaPrincipal(), "kb-1", []string{"space-platform"})
	// Порядок отсортирован: AccessScopeFor сортирует и склеивает дубли, и
	// ExceptionTerms обязан вести себя так же, иначе _source и тело запроса
	// расходились бы при одинаковых правах.
	want := []string{
		GroupPrincipal("team-backend"),
		RolePrincipal("admin"),
		UserPrincipal("anna"),
	}
	if !slices.Equal(scope.Principals, want) {
		t.Errorf("AccessScopeFor дал %v, ожидалось %v", scope.Principals, want)
	}
	// Формы зафиксированы посимвольно: это контракт с индексом и с §6.
	cases := map[string]string{
		"user":  UserPrincipal("anna"),
		"role":  RolePrincipal("admin"),
		"group": GroupPrincipal("team-backend"),
	}
	for name, got := range cases {
		if !strings.HasPrefix(got, name+":") {
			t.Errorf("форма %s дала %q, ожидался префикс %q", name, got, name+":")
		}
	}
}

// TestAccessScopeForНеМеняетсяОтПереименованияКонстант: если префиксы когда-нибудь
// поменяют, упадут обе стороны сразу — и это лучше, чем упасть одной.
func TestAccessScopeForНеМеняетсяОтПереименованияКонстант(t *testing.T) {
	scope := AccessScopeFor(annaPrincipal(), "kb-1", nil)
	for _, want := range []string{"user:anna", "role:admin", "group:team-backend"} {
		if !slices.Contains(scope.Principals, want) {
			t.Errorf("в области доступа нет %q: %v", want, scope.Principals)
		}
	}
	// Контейнеры приходят отдельно: право на ресурсную группу выдаётся
	// пользователю, а не хранится в его профиле (§5 шаг 1).
	if !slices.Equal(scope.SpaceIDs, []string(nil)) {
		t.Errorf("SpaceIDs = %v, ожидался пустой срез", scope.SpaceIDs)
	}
}

// TestПраваДокументаПоВидам — три случая из §6: контейнер, точечное исключение и
// личная заметка.
func TestПраваДокументаПоВидам(t *testing.T) {
	cases := map[string]struct {
		rights    DocumentRights
		wantSpace string
		wantACL   []string
	}{
		"контейнер без исключений": {
			rights:    ContainerRights("space-platform"),
			wantSpace: "space-platform",
			wantACL:   []string{},
		},
		"контейнер с обрезкой пробелов": {
			rights:    ContainerRights("  space-hr  "),
			wantSpace: "space-hr",
			wantACL:   []string{},
		},
		"личная заметка": {
			rights:    PersonalNoteRights("anna"),
			wantSpace: "",
			wantACL:   []string{"user:anna"},
		},
		"контейнер и исключения": {
			rights: DocumentRightsFor("space-hr", []string{
				"group:team-backend", "user:anna", "res:space-hr:read",
			}),
			wantSpace: "space-hr",
			wantACL:   []string{"group:team-backend", "res:space-hr:read", "user:anna"},
		},
		"исключений нет": {
			rights:    DocumentRightsFor("space-hr", nil),
			wantSpace: "space-hr",
			wantACL:   []string{},
		},
		"исключения из пустых строк": {
			rights:    DocumentRightsFor("space-hr", []string{"", "  ", "\t"}),
			wantSpace: "space-hr",
			wantACL:   []string{},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := tc.rights.SpaceID; got != tc.wantSpace {
				t.Errorf("SpaceID = %q, ожидалось %q", got, tc.wantSpace)
			}
			if got := tc.rights.ExceptionTerms(); !slices.Equal(got, tc.wantACL) {
				t.Errorf("ExceptionTerms = %#v, ожидалось %#v", got, tc.wantACL)
			}
		})
	}
}

// TestExceptionTermsНикогдаНеNil: nil сериализуется в null, а null в terms —
// историческое «не ограничивать», то есть полный обход прав. Здесь же проверяется
// и второй край: пустой массив означает «исключений нет», а не «доступ всем».
func TestExceptionTermsНикогдаНеNil(t *testing.T) {
	cases := map[string]DocumentRights{
		"нулевые права":  {},
		"контейнер":      ContainerRights("space-hr"),
		"пустой список":  {SpaceID: "space-hr", ACL: []string{}},
		"пустые строки":  {SpaceID: "space-hr", ACL: []string{"", " "}},
		"личная заметка": PersonalNoteRights("anna"),
	}
	for name, rights := range cases {
		t.Run(name, func(t *testing.T) {
			terms := rights.ExceptionTerms()
			if terms == nil {
				t.Fatal("ExceptionTerms вернул nil: такой срез сериализуется как null")
			}
			blob, err := json.Marshal(map[string]any{"acl": terms})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if strings.Contains(string(blob), "null") {
				t.Errorf("acl сериализован как null: %s", blob)
			}
			if !strings.Contains(string(blob), `"acl":[`) {
				t.Errorf("acl не сериализован массивом: %s", blob)
			}
		})
	}
}

// TestExceptionTermsЧиститИСтабилизирует: пустые строки и дубли бесполезны в
// terms и засоряют _source, а порядок обязан быть воспроизводимым — иначе
// мигают тесты и нечитаемы diff'ы тел документов.
func TestExceptionTermsЧиститИСтабилизирует(t *testing.T) {
	rights := DocumentRights{SpaceID: "space-hr", ACL: []string{
		" user:anna ", "", "   ", "user:anna", "group:team-backend", "user:anna",
	}}
	want := []string{"group:team-backend", "user:anna"}
	if got := rights.ExceptionTerms(); !slices.Equal(got, want) {
		t.Errorf("ExceptionTerms = %#v, ожидалось %#v", got, want)
	}
	// Порядок входа не влияет на порядок термов.
	shuffled := DocumentRights{SpaceID: "space-hr", ACL: []string{
		"group:team-backend", "user:anna",
	}}
	if !slices.Equal(rights.ExceptionTerms(), shuffled.ExceptionTerms()) {
		t.Error("порядок термов зависит от порядка входа")
	}
}

// TestExceptionTermsВозвращаетНовыйСрез: права принадлежат сценарию индексации и
// могут быть переиспользованы, а общий срез в _source — гонка данных.
func TestExceptionTermsВозвращаетНовыйСрез(t *testing.T) {
	rights := PersonalNoteRights("anna")
	first := rights.ExceptionTerms()
	first[0] = "user:petya"

	if got := rights.ExceptionTerms()[0]; got != "user:anna" {
		t.Errorf("права изменились вслед за чужой правкой: %q", got)
	}
}

// TestGrantsПовторяетКлозуФильтра — зеркало §6: контейнер ИЛИ принципал, больше
// никаких путей. Проверка нужна потому, что правило живёт сразу в двух местах
// (этот метод и клоза адаптера) и одно обязано повторять другое.
func TestGrantsПовторяетКлозуФильтра(t *testing.T) {
	scope := AccessScopeFor(annaPrincipal(), "kb-1", []string{"space-platform"})
	cases := []struct {
		name   string
		rights DocumentRights
		want   bool
	}{
		{"свой контейнер", ContainerRights("space-platform"), true},
		{"чужой контейнер", ContainerRights("space-hr"), false},
		{"своя личная заметка", PersonalNoteRights("anna"), true},
		{"чужая личная заметка", PersonalNoteRights("petya"), false},
		{"по своей роли", DocumentRights{SpaceID: "", ACL: []string{RolePrincipal("admin")}}, true},
		{"по своей группе", DocumentRights{SpaceID: "", ACL: []string{GroupPrincipal("team-backend")}}, true},
		{"по чужой группе", DocumentRights{SpaceID: "", ACL: []string{GroupPrincipal("team-sales")}}, false},
		{"ресурсная группа мимо group:", DocumentRights{ACL: []string{"res:space-platform:read"}}, false},
		{"пустой контейнер не совпадает с пустым scope", DocumentRights{SpaceID: " "}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scope.Grants(tc.rights); got != tc.want {
				t.Errorf("Grants = %v, ожидалось %v (scope=%+v rights=%+v)",
					got, tc.want, scope, tc.rights)
			}
		})
	}
}

// TestПустаяОбластьНикогоНеВидит: область без прав — это «нечего не видно».
// Grants обязан согласиться с AllowsNothing, иначе сценарий, поверивший только
// одному из двух, отдаст документ всем.
func TestПустаяОбластьНикогоНеВидит(t *testing.T) {
	scope := AccessScope{}
	if !scope.AllowsNothing() {
		t.Fatal("пустая область не считается пустой")
	}
	cases := map[string]DocumentRights{
		"контейнер":  ContainerRights("space-platform"),
		"личная":     PersonalNoteRights("anna"),
		"исключение": {ACL: []string{"role:admin"}},
	}
	for name, rights := range cases {
		t.Run(name, func(t *testing.T) {
			if scope.Grants(rights) {
				t.Error("пустая область увидела документ — это полный обход прав")
			}
		})
	}
}

// TestПустойAclНеОткрываетДокументВсем: главный риск новой семантики. Документ с
// пустым acl виден РОВНО по своему контейнеру, и никак иначе — соседний
// контейнер его не открывает.
func TestПустойAclНеОткрываетДокументВсем(t *testing.T) {
	rights := ContainerRights("space-hr")

	owner := AccessScope{TenantID: "tenant-1", SpaceIDs: []string{"space-hr"}}
	if !owner.Grants(rights) {
		t.Error("владелец контейнера не видит свой документ")
	}

	for _, scope := range []AccessScope{
		{TenantID: "tenant-1", SpaceIDs: []string{"space-platform"}},
		{TenantID: "tenant-1", Principals: []string{"user:anna"}},
		{TenantID: "tenant-1"},
	} {
		if scope.Grants(rights) {
			t.Errorf("документ контейнера открылся по чужим правам: %+v", scope)
		}
	}
}

// TestПустойAclОтличаетсяОтСпискаВсем: это разные значения, и путать их нельзя.
// Второе означало бы утечку всего тенанта — в отличие от первого, которое просто
// оставляет документ видимым по своему контейнеру.
func TestПустойAclОтличаетсяОтСпискаВсем(t *testing.T) {
	empty := ContainerRights("space-hr")
	if len(empty.ExceptionTerms()) != 0 {
		t.Errorf("у документа без исключений acl = %v", empty.ExceptionTerms())
	}
	// «Прочий доступ всем» в модели §6 не существует вовсе: deny нет, только
	// разрешения, и единственный способ выдать доступ всем — не выдать его
	// контейнеру. Права без контейнера и без исключений поэтому ничего не
	// открывают никому.
	nobody := DocumentRights{}
	everyone := AccessScope{Principals: []string{"user:anna", "role:admin"}}
	if everyone.Grants(nobody) {
		t.Error("документ без прав и без контейнера виден всем подряд")
	}
}

// TestЧужоеПолеНеПодменяетСловарь: legacy ACL (Roles/Groups/Users) нельзя
// случайно выдать за DocumentRights. У него обратный смысл пустого значения —
// «доступ всем» (AllowsAny) — и другой словарь, голые user_id без префикса.
// Смешать их в индексе значит либо закрыть личные заметки владельцу, либо
// открыть чужой документ всем.
func TestЧужоеПолеНеПодменяетСловарь(t *testing.T) {
	legacy := ACL{Users: []string{"anna"}}
	if legacy.AllowsAny() {
		t.Fatal("ACL с пользователем не должен открывать базу всем")
	}
	if (ACL{}).AllowsAny() != true {
		t.Error("пустой legacy ACL обязан оставаться «доступ всем» — обратная семантика")
	}
	// Словарь разный: «anna» и «user:anna» — разные термы.
	if !legacy.Grants(Principal{UserID: "anna"}) {
		t.Error("legacy ACL перестал работать: это не задача этого файла, но его нельзя ломать")
	}
	if legacy.Grants(Principal{UserID: UserPrincipal("anna")}) {
		t.Error("legacy ACL вдруг начал понимать префиксы — значит, словари смешались")
	}
}

// TestRightsЕдутВЧанке: права принадлежат чанку, который едет в индекс, а не
// чему-то, что собирается заново на стороне адаптера.
func TestRightsЕдутВЧанке(t *testing.T) {
	chunk := Chunk{ChunkID: "sha256:chunk", Rights: PersonalNoteRights("anna")}
	if got := chunk.Rights.ExceptionTerms(); !slices.Equal(got, []string{"user:anna"}) {
		t.Errorf("права чанка = %#v, ожидалось [user:anna]", got)
	}
	if chunk.Rights.SpaceID != "" {
		t.Errorf("личная заметка получила контейнер %q", chunk.Rights.SpaceID)
	}
}
