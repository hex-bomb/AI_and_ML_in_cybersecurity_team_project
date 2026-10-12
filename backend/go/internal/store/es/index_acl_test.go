package elasticsearch

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	domain "github.com/kbsearch/kb/internal/envelope"
)

// Тесты индекса прав §6. Кластера не требуют: проверяют форму _source и mapping'а,
// которые уехали бы в сеть. Интеграция — в elasticsearch_integration_test.go.
//
// Предмет проверки — разрыв между двумя половинами контракта. Сторона запроса
// (acl_filter.go) фильтрует по space_id и acl, сторона индекса (index_body.go)
// обязана писать ровно эти поля и ровно эти строки. Расхождение не падает с
// ошибкой: terms по ненаписанному полю просто не совпадает, и поиск возвращает
// пустоту всем.

// docJSON сериализует документ индекса так же, как он уедет в Elasticsearch.
func docJSON(t *testing.T, source docSource) string {
	t.Helper()
	blob, err := json.Marshal(source)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(blob)
}

// chunkWithRights собирает чанк с заданными правами на заданном профиле.
func chunkWithRights(profileID string, dimension int, rights domain.DocumentRights) domain.Chunk {
	chunk := testChunk(profileID, dimension, "текст чанка")
	chunk.Rights = rights
	return chunk
}

// TestДокументНесётКонтейнерИТочечныеИсключения — основной случай §6: права
// едут вместе с документом, и форма их та же, что ищет фильтр запроса.
//
// Три вида документа из спецификации: обычный (только контейнер), документ с
// точечным исключением и личная заметка «acl = [user:<id>] и больше никто».
func TestДокументНесётКонтейнерИТочечныеИсключения(t *testing.T) {
	cfg := testConfig(384)
	cases := map[string]struct {
		rights    domain.DocumentRights
		wantSpace string
		wantACL   []string
	}{
		"только контейнер": {
			rights:    domain.ContainerRights("space-platform"),
			wantSpace: "space-platform",
			wantACL:   []string{},
		},
		"контейнер с исключением": {
			rights: domain.DocumentRightsFor("space-platform",
				[]string{"group:team-backend", "res:space-platform:read"}),
			wantSpace: "space-platform",
			wantACL:   []string{"group:team-backend", "res:space-platform:read"},
		},
		"личная заметка без контейнера": {
			rights:    domain.PersonalNoteRights("anna"),
			wantSpace: "",
			wantACL:   []string{"user:anna"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			chunk := chunkWithRights(cfg.ProfileID(), 384, tc.rights)
			source := toDocSource(chunk)

			if source.SpaceID != tc.wantSpace {
				t.Errorf("space_id = %q, ожидалось %q", source.SpaceID, tc.wantSpace)
			}
			if !slices.Equal(source.ACL, tc.wantACL) {
				t.Errorf("acl = %#v, ожидалось %#v", source.ACL, tc.wantACL)
			}
			blob := docJSON(t, source)
			if !strings.Contains(blob, `"space_id":"`+tc.wantSpace+`"`) {
				t.Errorf("в _source нет space_id=%q: %s", tc.wantSpace, blob)
			}
		})
	}
}

// TestПоляПравОбъявленыВMappingКакKeyword: exact-семантика terms требует keyword.
// Тип text здесь — тихая поломка прав: анализатор разобьёт «user:anna» на токены,
// и фильтр перестанет узнавать собственный термин. Отдельно проверяется, что
// имена полей совпадают с константами стороны запроса.
func TestПоляПравОбъявленыВMappingКакKeyword(t *testing.T) {
	props, ok := indexBody(384)["mappings"].(map[string]any)["properties"].(map[string]any)
	if !ok {
		t.Fatal("в indexBody нет свойств mapping'а")
	}
	for _, field := range []string{aclFieldSpace, aclFieldPrincipals} {
		t.Run(field, func(t *testing.T) {
			prop, declared := props[field]
			if !declared {
				t.Fatalf("поле %s не объявлено в mapping'е: terms по нему ничего не найдёт", field)
			}
			settings, ok := prop.(map[string]any)
			if !ok {
				t.Fatalf("свойство %s не объект: %T", field, prop)
			}
			if got := settings["type"]; got != "keyword" {
				t.Errorf("тип поля %s = %v, ожидался keyword", field, got)
			}
			if _, analyzed := settings["analyzer"]; analyzed {
				t.Errorf("поле %s объявлено с анализатором: %v", field, settings)
			}
		})
	}
}

// TestПоляПравСовпадаютСКонстантамиЗапроса — тот контракт, из-за которого имена
// берутся из acl_filter.go, а не пишутся строкой здесь. Стороны обязаны называть
// поля одинаково; расхождение не падает, а молча закрывает доступ.
func TestПоляПравСовпадаютСКонстантамиЗапроса(t *testing.T) {
	if aclFieldSpace != "space_id" {
		t.Errorf("aclFieldSpace = %q, ожидалось space_id (§6)", aclFieldSpace)
	}
	if aclFieldPrincipals != "acl" {
		t.Errorf("aclFieldPrincipals = %q, ожидалось acl (§6)", aclFieldPrincipals)
	}
	// И те же имена обязаны быть в _source, а не только в mapping'е: mapping
	// без поля в документе — то же самое, что отсутствие поля.
	blob := docJSON(t, toDocSource(testChunk(testConfig(384).ProfileID(), 384, "текст")))
	for _, name := range []string{"space_id", "acl"} {
		if !strings.Contains(blob, `"`+name+`":`) {
			t.Errorf("в _source нет поля %s: %s", name, blob)
		}
	}
}

// TestПустыеПраваСериализуютсяПустымМассивом: документ без исключений не имеет
// права превратиться в null. null в terms — историческое «не ограничивать», то
// есть полный обход прав, а не «исключений нет».
func TestПустыеПраваСериализуютсяПустымМассивом(t *testing.T) {
	cfg := testConfig(384)
	cases := map[string]domain.DocumentRights{
		"нулевые права":      {},
		"контейнер":          domain.ContainerRights("space-hr"),
		"контейнер с пустым": {SpaceID: "space-hr", ACL: []string{}},
		"личная заметка":     domain.PersonalNoteRights("anna"),
	}
	for name, rights := range cases {
		t.Run(name, func(t *testing.T) {
			source := toDocSource(chunkWithRights(cfg.ProfileID(), 384, rights))
			if source.ACL == nil {
				t.Fatal("acl в документе nil: такой срез сериализуется как null")
			}
			blob := docJSON(t, source)
			if strings.Contains(blob, `"acl":null`) {
				t.Errorf("acl сериализован как null — это «не ограничивать»: %s", blob)
			}
			if !strings.Contains(blob, `"acl":[`) {
				t.Errorf("acl не сериализован массивом: %s", blob)
			}
		})
	}
}

// TestПустойAclНеРасширяетДоступ: документ без исключений обязан остаться
// видимым ровно по своему контейнеру. Регрессия на подмену «исключений нет» на
// «прочий доступ всем» — это утечка всего тенанта, и заметить её можно только
// тестом: пустой terms не совпадает ни с чем, а «доступ всем» совпадает со всем.
func TestПустойAclНеРасширяетДоступ(t *testing.T) {
	cfg := testConfig(384)
	spaceOnly := domain.ContainerRights("space-hr")
	scope := domain.AccessScope{TenantID: "tenant-1", KBID: "kb-1", SpaceIDs: []string{"space-hr"}}

	if !scope.Grants(spaceOnly) {
		t.Fatal("владелец контейнера не видит свой документ")
	}
	source := toDocSource(chunkWithRights(cfg.ProfileID(), 384, spaceOnly))
	if len(source.ACL) != 0 {
		t.Errorf("документ без исключений получил acl = %v", source.ACL)
	}

	// Соседний контейнер документ не открывает: права контейнера — это ровно он.
	neighbour := domain.AccessScope{
		TenantID: "tenant-1", KBID: "kb-1",
		SpaceIDs: []string{"space-platform"}, Principals: []string{"user:anna"},
	}
	if neighbour.Grants(spaceOnly) {
		t.Error("документ одного контейнера открылся по правам другого")
	}
}

// TestПустаяСтрокаВПравахНеПопадаетВИндекс: пробельный или пустой идентификатор
// в terms бесполезен и засоряет _source; на стороне запроса тем же занимается
// aclTerms, и обе стороны обязаны вести себя одинаково.
func TestПустаяСтрокаВПравахНеПопадаетВИндекс(t *testing.T) {
	cfg := testConfig(384)
	chunk := chunkWithRights(cfg.ProfileID(), 384, domain.DocumentRights{
		SpaceID: "space-hr",
		ACL:     []string{" user:anna ", "", "   ", "user:anna", "group:team-backend"},
	})
	source := toDocSource(chunk)

	want := []string{"group:team-backend", "user:anna"}
	if !slices.Equal(source.ACL, want) {
		t.Errorf("acl = %#v, ожидалось %#v", source.ACL, want)
	}
}

// TestСловарьПринципаловИндексаСовпадаетСФильтромЗапроса — сквозной контракт
// §6. Берём область доступа, которую оракул прав отдаёт пользователю, и права
// документа, которые уедут в индекс; значения обязаны быть одним и тем же
// словарём. Сейчас это единственный тест, который поймает «user:anna» против
// «anna», не поднимая кластер.
func TestСловарьПринципаловИндексаСовпадаетСФильтромЗапроса(t *testing.T) {
	cfg := testConfig(384)
	principal := domain.Principal{
		TenantID: uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		UserID:   "anna",
		Roles:    []string{"admin"},
		Groups:   []string{"team-backend"},
	}
	scope := domain.AccessScopeFor(principal, "kb-1", []string{"space-platform"})

	// Права документа в том же словаре: контейнер плюс точечные исключения.
	// Строка ресурсной группы «res:space-platform:read» сюда НЕ входит
	// намеренно: в области доступа её нет и быть не должно — оракул прав
	// (store/pg/acl_resolver.go) кладёт ресурсные группы под префиксом group:.
	// Она проверяется отдельно, как форма, которую нельзя переписывать.
	rights := domain.DocumentRightsFor("space-platform", []string{
		domain.UserPrincipal("anna"),
		domain.RolePrincipal("admin"),
		domain.GroupPrincipal("team-backend"),
	})

	// 1. Область доступа даёт ровно те строки, что и записаны в документ.
	for _, term := range rights.ExceptionTerms() {
		if !slices.Contains(scope.Principals, term) {
			t.Errorf("индекс пишет %q, которого нет в области доступа %v", term, scope.Principals)
		}
	}

	// 2. Фильтр запроса видит документ, написанный этими же правами.
	if !scope.Grants(rights) {
		t.Errorf("пользователь не видит собственный документ: scope=%v rights=%+v",
			scope, rights)
	}

	// 3. Форма строк совпадает с эталоном §6 по обе стороны.
	clause := aclJSON(t, aclAccessClause(scope))
	blob := docJSON(t, toDocSource(chunkWithRights(cfg.ProfileID(), 384, rights)))
	for _, want := range []string{"user:anna", "role:admin", "group:team-backend"} {
		if !strings.Contains(clause, `"`+want+`"`) {
			t.Errorf("в фильтре запроса нет %s: %s", want, clause)
		}
		if !strings.Contains(blob, `"`+want+`"`) {
			t.Errorf("в _source документа нет %s: %s", want, blob)
		}
	}
}

// TestЧанкБезПравНеВиденНиКому — fail-closed на текущем состоянии конвейера.
//
// Стадия индексации собирает domain.Chunk из ParsedDocument, и права в неё
// пока не проставляются: чанк уходит с пустыми правами. Такой документ не
// должен достаться НИКОМУ — даже пользователю с полными правами на весь
// тенант, — потому что ни space_id, ни acl не совпадут ни с одним термом.
// Проверка фиксирует направление отказа: как только права начнут доезжать до
// чанка, тест начнёт падать на первом же заполненном space, и это будет
// сигналом, что сценарий индексации надо дочитать, а не «ослабить тест».
func TestЧанкБезПравНеВиденНиКому(t *testing.T) {
	cfg := testConfig(384)
	source := toDocSource(chunkWithRights(cfg.ProfileID(), 384, domain.DocumentRights{}))

	if source.SpaceID != "" {
		t.Fatalf("чанк без прав получил контейнер %q", source.SpaceID)
	}
	if len(source.ACL) != 0 {
		t.Fatalf("чанк без прав получил исключения %v", source.ACL)
	}

	// Пользователь с максимумом прав: и контейнеры, и принципалы.
	full := domain.AccessScopeFor(
		domain.Principal{
			TenantID: uuid.MustParse("11111111-1111-4111-8111-111111111111"),
			UserID:   "anna",
			Roles:    []string{"admin"},
			Groups:   []string{"team-backend"},
		},
		"kb-1",
		[]string{"space-platform", "space-hr"},
	)
	if full.Grants(domain.DocumentRights{SpaceID: source.SpaceID, ACL: source.ACL}) {
		t.Error("документ без прав виден пользователю с полными правами — это утечка")
	}
}

// TestРесурснаяГруппаДоезжаетДоДокументаБезИскажений: строка «res:<ключ>:read»
// приходит от источников прав (коннектор превращает шары Drive и каналы Slack в
// ресурсные группы, §4) и обязана попасть в индекс ровно такой. Ресурсная группа
// не лежит в индексе сама по себе — принципал получает «group:<ключ>», — но если
// кто-то всё же записал в документ исходную форму, адаптер не имеет права её
// переписывать: молчаливая подмена строки означает либо отказ, либо выдачу чужого.
func TestРесурснаяГруппаДоезжаетДоДокументаБезИскажений(t *testing.T) {
	cfg := testConfig(384)
	principals := []string{
		domain.UserPrincipal("anna"),
		domain.RolePrincipal("admin"),
		domain.GroupPrincipal("team-backend"),
		"res:space-platform:read",
		"res:space-hr:read",
	}
	source := toDocSource(chunkWithRights(cfg.ProfileID(), 384,
		domain.DocumentRightsFor("space-platform", principals)))
	blob := docJSON(t, source)

	for _, want := range principals {
		if !strings.Contains(blob, `"`+want+`"`) {
			t.Errorf("принципал %s не дошёл до _source без изменений: %s", want, blob)
		}
	}
}

// TestПраваКопируютсяВДокумент: чанк принадлежит сценарию индексации и может быть
// переиспользован, а общий срез в _source — это гонка данных между стадиями.
func TestПраваКопируютсяВДокумент(t *testing.T) {
	cfg := testConfig(384)
	chunk := chunkWithRights(cfg.ProfileID(), 384, domain.DocumentRights{
		SpaceID: "space-hr",
		ACL:     []string{"user:anna"},
	})
	source := toDocSource(chunk)

	// Мутируем оригинал после построения документа.
	chunk.Rights.ACL[0] = "user:petya"
	chunk.Rights.ACL = append(chunk.Rights.ACL, "user:ivan")
	chunk.Rights.SpaceID = "space-platform"

	if got := source.ACL[0]; got != "user:anna" {
		t.Errorf("acl в документе изменился вслед за чанком: %q", got)
	}
	if len(source.ACL) != 1 {
		t.Errorf("в документ добавился чужой принципал: %v", source.ACL)
	}
	if source.SpaceID != "space-hr" {
		t.Errorf("space_id в документе изменился вслед за чанком: %q", source.SpaceID)
	}
}

// TestПовторнаяСборкаДокументаВоспроизводима: _source уходит в diff'ы и в логи
// индексации, поэтому порядок исключений обязан быть независим от порядка входа.
func TestПовторнаяСборкаДокументаВоспроизводима(t *testing.T) {
	// Один и тот же чанк: testChunk выдаёт новый block_id на каждый вызов, а
	// сравнивать здесь надо только порядок прав.
	chunk := chunkWithRights(testConfig(384).ProfileID(), 384,
		domain.ContainerRights("space-hr"))

	chunk.Rights = domain.DocumentRightsFor("space-hr", []string{"user:z", "user:a", "user:m"})
	first := docJSON(t, toDocSource(chunk))
	chunk.Rights = domain.DocumentRightsFor("space-hr", []string{"user:m", "user:z", "user:a"})
	second := docJSON(t, toDocSource(chunk))

	if first != second {
		t.Errorf("порядок прав зависит от входа:\n %s\n %s", first, second)
	}
}

// TestДобавлениеПолейПравНеМеняетИмяИндекса: §5 переиндексацию делает новым
// полем и переключением alias, а имя индекса считается по векторному профилю.
// Если бы права попали в ProfileID, любое их изменение переименовало бы индекс и
// сломало бы схему алиасов.
func TestДобавлениеПолейПравНеМеняетИмяИндекса(t *testing.T) {
	cfg := testConfig(384)

	// Имя индекса считается по векторному профилю и не зависит от прав: иначе
	// любое изменение прав переименовало бы индекс и сломало бы схему алиасов.
	if !strings.HasPrefix(cfg.IndexName(), domain.IndexPrefix+"-") {
		t.Errorf("имя индекса %q не построено от %s", cfg.IndexName(), domain.IndexPrefix)
	}
	// ProfileID обязан быть детерминированным: два независимо собранных
	// конфига одного векторного профиля дают одно имя, иначе алиасы индексов
	// расходятся при каждом перезапуске.
	if got, want := testConfig(384).ProfileID(), cfg.ProfileID(); got != want {
		t.Errorf("ProfileID не детерминирован: %q против %q", got, want)
	}
	// И он обязан зависеть от векторного профиля, иначе смена модели
	// переиспользовала бы индекс, построенный для другой размерности.
	if testConfig(768).ProfileID() == cfg.ProfileID() {
		t.Error("ProfileID не учитывает размерность вектора")
	}

	// Поля прав присутствуют в теле индекса, и оно остаётся собираемым.
	body := indexBody(cfg.EmbeddingDimension)
	if _, err := marshalBody(body); err != nil {
		t.Fatalf("тело индекса перестало собираться: %v", err)
	}
	props, ok := body["mappings"].(map[string]any)["properties"].(map[string]any)
	if !ok {
		t.Fatal("в indexBody нет свойств mapping'а")
	}
	for _, field := range []string{aclFieldSpace, aclFieldPrincipals} {
		if _, declared := props[field]; !declared {
			t.Errorf("поле прав %s пропало из тела индекса", field)
		}
	}
}
