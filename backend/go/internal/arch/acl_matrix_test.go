package arch

// ВОРОТА «ПРАВА» — §9: «автотест в CI на каждый PR проверяет матрицу
// “пользователь × документ” и сообщает об ошибке при любой утечке».
//
// ЧТО ТАКОЕ УТЕЧКА В ЭТОМ ТЕСТЕ, ДОСЛОВНО.
//
// Утечка — это пара (user, document), для которой:
//
//	оракул прав (internal/store/pg.ACLResolver → AccessScope) ЗАПРЕЩАЕТ
//		увидеть документ, а
//	фильтр, который реально ушёл в Elasticsearch
//		(internal/store/es.aclAccessClause внутри KeywordSearchScoped)
//	МАТЧИТ этот документ.
//
// Оракул и фильтр — две НЕЗАВИСИМЫ реализации одного правила §6: оракул
// читает Postgres и отдаёт список термов, фильтр превращает термы в
// Elasticsearch-клозу. Расхождение между ними и есть утечка. Обе стороны
// вызваны как есть; ни одна из них не переписана в тесте.
//
// Второй, обратный класс дефекта — «пропуск результата» (oracle разрешает,
// фильтр не матчит). Утечкой он не является, но §9 требует, чтобы права были
// не только безопасны, но и работали, поэтому он падает отдельно.
//
// Файл НЕ проверяет выдачу Elasticsearch: ответ подставного кластера пуст по
// построению, и «посмотреть, что пришло в ответ» — это ровно тот post-filter,
// который §6 запрещает. Проверяется тело ушедшего запроса и реальный _source
// записанного документа.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	domain "github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
	esadapter "github.com/kbsearch/kb/internal/store/es"
	pgadapter "github.com/kbsearch/kb/internal/store/pg"
)

// ── Мир ворот: два тенанта, четыре контейнера, одиннадцать документов ───────

// Идентификаторы фикстур фиксированы числами: отчёт об утечке обязан быть
// воспроизводим между прогонами и запусками, а uuid.New под это не годится.
var (
	gateTenantAcme   = uuid.MustParse("0a000000-0000-4000-8000-00000000ac01")
	gateTenantGlobex = uuid.MustParse("0b000000-0000-4000-8000-00000000ac02")
)

// Идентификаторы сущностей §6 фиксированы числами: отчёт об утечке обязан
// быть воспроизводим между прогонами и запусками, а uuid.New под это не
// годится.
var (
	gateAcmeUser    = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	gateAcmeBackend = uuid.MustParse("55555555-5555-4555-8555-555555555555")
	gateAcmeHRGroup = uuid.MustParse("66666666-6666-4666-8666-666666666666")

	gateGlobexGroup  = uuid.MustParse("99999999-9999-4999-8999-999999999999")
	gateGlobexResOne = uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1")
	gateGlobexResTwo = uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa2")

	gateAcmeResRead = uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc")

	gateAcmePlatformID = uuid.MustParse("22222222-2222-4222-8222-222222222222")
	gateAcmeHRID       = uuid.MustParse("33333333-3333-4333-8333-333333333333")
	gateAcmeSecretID   = uuid.MustParse("44444444-4444-4444-8444-444444444444")

	gateGlobexForeignID  = uuid.MustParse("77777777-7777-4777-8777-777777777777")
	gateGlobexPlatformID = uuid.MustParse("88888888-8888-4888-8888-888888888888")
)

// Ключи контейнеров. Именно они, а не идентификаторы, уезжают в индекс
// термом space_id (§6), поэтому и проверяются поимённо.
const (
	gateAcmePlatform = "space-platform"
	gateAcmeHR       = "space-hr"
	gateAcmeSecret   = "space-secret"

	gateGlobexForeign  = "space-foreign"
	gateGlobexPlatform = "space-platform"
)

// gateFixture — собранный мир ворот: права AGDLP, пользователи и корпус.
type gateFixture struct {
	world  *gateWorld
	users  []gateUserFixture
	corpus []gateDoc
}

// gateFixtureWorld собирает мир §6.
//
// ЧТО ЗДЕСЬ ПРОВЕРЯЕТСЯ ПО СУЩЕСТВУ:
//
//   - два тенанта, причём у них ОДИН И ТАКОЙ ЖЕ ключ контейнера
//     space-platform: term по space_id не различает тенанты, и различает их
//     term по tenant_id. Если этот терм из фильтра выпадет — Globex увидит
//     документы Acme;
//   - контейнер space-hr есть, но права на него не выданы никому;
//   - контейнер space-secret есть, права на него не выданы никому — и его
//     не видит даже пользователь с ролью admin;
//   - личные заметки без контейнера, открытые только своему автору;
//   - точечное исключение user:<id> ВНУТРИ контейнера, который виден не всем;
//   - документ без прав вовсе: ни контейнера, ни исключений.
//
// Права и корпус собираются из одного gateWorld: оракул и индекс читают один
// источник правды, поэтому расхождение между ними может означать только ошибку
// в реализации, а не в фикстуре.
func gateFixtureWorld() *gateFixture {
	world := newGateWorld()

	// ── Контейнеры. space-platform есть у обоих тенантов.
	world.addContainer(gateTenantAcme, gateAcmePlatform, gateAcmePlatformID)
	world.addContainer(gateTenantAcme, gateAcmeHR, gateAcmeHRID)
	world.addContainer(gateTenantAcme, gateAcmeSecret, gateAcmeSecretID)
	world.addContainer(gateTenantGlobex, gateGlobexForeign, gateGlobexForeignID)
	world.addContainer(gateTenantGlobex, gateGlobexPlatform, gateGlobexPlatformID)

	// ── Глобальные группы (claim groups / SCIM).
	world.addGlobalGroup(gateTenantAcme, "team-backend", gateAcmeBackend)
	world.addGlobalGroup(gateTenantAcme, "team-hr", gateAcmeHRGroup)
	world.addGlobalGroup(gateTenantGlobex, "team-foreign", gateGlobexGroup)

	// ── Ресурсные группы. Их ключ — форма §6 «res:<контейнер>:<действие>».
	world.addResourceGroup(gateTenantAcme, "res:space-platform:read", gateAcmeResRead)
	world.addResourceGroup(gateTenantGlobex, "res:space-foreign:read", gateGlobexResOne)
	world.addResourceGroup(gateTenantGlobex, "res:space-platform:read", gateGlobexResTwo)

	// ── Membership. Acme: anna приходит из SCIM-таблицы (путь membership),
	// остальные — из claim групп (путь claim). Оба пути обязаны работать.
	world.addUser(gateTenantAcme, "anna", gateAcmeUser)
	world.addGlobalMember(gateTenantAcme, gateAcmeBackend, gateAcmeUser)

	// ── Глобальные → ресурсные.
	world.addResourceMember(gateTenantAcme, gateAcmeResRead, gateAcmeBackend)
	world.addResourceMember(gateTenantGlobex, gateGlobexResOne, gateGlobexGroup)
	world.addResourceMember(gateTenantGlobex, gateGlobexResTwo, gateGlobexGroup)

	// ── Права. На space-hr и space-secret прав нет НИ У КОГО — это и есть
	// случай «документ есть, а прочитать его не может никто».
	world.addPermission(gateTenantAcme, gateAcmeResRead, gateAcmePlatformID, gateReadAction)
	world.addPermission(gateTenantGlobex, gateGlobexResOne, gateGlobexForeignID, gateReadAction)
	world.addPermission(gateTenantGlobex, gateGlobexResTwo, gateGlobexPlatformID, gateReadAction)

	corpus := []gateDoc{
		// Контейнер виден части пользователей Acme.
		gateDocFixture("acme-platform-runbook", gateTenantAcme, "doc-acme-1",
			domain.ContainerRights("space-platform"), "регламент платформы"),
		// Точечное исключение ВНУТРИ контейнера, который виден не всем.
		// ВНИМАНИЕ на смысл: исключение в §6 РАСШИРЯЕТ доступ, а не сужает
		// его («deny нет, только разрешения»). Читает черновик и борис по
		// исключению, и все, кому доступен контейнер, — по контейнеру.
		// Сузить видимость внутри контейнера модель §6 не умеет, и это
		// зафиксировано тестом, а не оставлено на волю случая.
		gateDocFixture("acme-platform-draft", gateTenantAcme, "doc-acme-2",
			domain.DocumentRightsFor("space-platform", []string{domain.UserPrincipal("boris")}),
			"черновик, личный к борису"),
		// Контейнер есть, права на него не выданы никому.
		gateDocFixture("acme-hr-policy", gateTenantAcme, "doc-acme-3",
			domain.ContainerRights("space-hr"), "кадровая политика"),
		// Исключение пробивает закрытый контейнер: анна видит, остальные нет.
		gateDocFixture("acme-hr-for-anna", gateTenantAcme, "doc-acme-4",
			domain.DocumentRightsFor("space-hr", []string{domain.UserPrincipal("anna")}),
			"служебная записка для анны"),
		// Контейнер без прав вообще — включая админа.
		gateDocFixture("acme-secret", gateTenantAcme, "doc-acme-5",
			domain.ContainerRights("space-secret"), "секрет"),
		// Прав нет вовсе: ни контейнера, ни исключений.
		gateDocFixture("acme-no-rights", gateTenantAcme, "doc-acme-6",
			domain.DocumentRights{}, "документ без прав"),
		// Личные заметки: контейнера нет, открыты только автору (§6).
		gateDocFixture("acme-note-anna", gateTenantAcme, "doc-acme-7",
			domain.PersonalNoteRights("anna"), "заметка анны"),
		gateDocFixture("acme-note-boris", gateTenantAcme, "doc-acme-8",
			domain.PersonalNoteRights("boris"), "заметка бориса"),
		gateDocFixture("acme-note-elena", gateTenantAcme, "doc-acme-9",
			domain.PersonalNoteRights("elena"), "заметка елены"),
		// Чужой тенант.
		gateDocFixture("globex-foreign", gateTenantGlobex, "doc-globex-1",
			domain.ContainerRights("space-foreign"), "globex: иностранное"),
		// Тот же ключ контейнера, что у Acme. Видна только Zoe.
		gateDocFixture("globex-platform", gateTenantGlobex, "doc-globex-2",
			domain.ContainerRights("space-platform"), "globex: своя платформа"),
	}

	return &gateFixture{world: world, users: gateUsers(), corpus: corpus}
}

// gateDocFixture собирает документ корпуса.
func gateDocFixture(id string, tenant uuid.UUID, document string,
	rights domain.DocumentRights, text string) gateDoc {
	return gateDoc{
		ID:       id,
		TenantID: tenant.String(),
		Document: document,
		Rights:   rights,
		Text:     text,
	}
}

// gateUsers перечисляет субъектов матрицы.
//
// Роли и группы расставлены так, чтобы закрыть четыре класса §6:
//
//	anna   — полная цепочка AGDLP через membership-таблицу (SCIM);
//	boris  — та же цепочка через claim-группу;
//	vera   — состоит в глобальной группе, у которой НЕТ ресурсных: цепочка
//	         обрывается на середине, и пользователь не видит ничего;
//	dmitry — роль admin. §6 не даёт ей обхода прав: роль разворачивается в
//	         терм «role:admin», а права выдаются только ресурсным группам;
//	elena  — видит свой контейнер и только свою заметку;
//	ghost  — нет ни строки в acl_users, ни групп: путь JIT (§6);
//	zoe    — другой тенант, другое право на одноимённый контейнер.
func gateUsers() []gateUserFixture {
	return []gateUserFixture{
		{
			Name:      "anna",
			Principal: domain.Principal{TenantID: gateTenantAcme, UserID: "anna"},
			Expect:    []string{"acme-platform-runbook", "acme-platform-draft", "acme-hr-for-anna", "acme-note-anna"},
			Never: []string{
				"acme-hr-policy", "acme-secret", "acme-no-rights",
				"acme-note-boris", "acme-note-elena", "globex-foreign", "globex-platform",
			},
		},
		{
			Name: "boris",
			Principal: domain.Principal{
				TenantID: gateTenantAcme, UserID: "boris", Groups: []string{"team-backend"},
			},
			Expect: []string{"acme-platform-runbook", "acme-platform-draft", "acme-note-boris"},
			Never: []string{
				"acme-hr-policy", "acme-hr-for-anna", "acme-secret", "acme-no-rights",
				"acme-note-anna", "acme-note-elena", "globex-foreign", "globex-platform",
			},
		},
		{
			Name: "vera",
			Principal: domain.Principal{
				TenantID: gateTenantAcme, UserID: "vera", Groups: []string{"team-hr"},
			},
			Expect: []string{},
			Never: []string{
				"acme-platform-runbook", "acme-hr-policy", "acme-secret",
				"acme-note-anna", "globex-platform",
			},
		},
		{
			Name: "dmitry",
			Principal: domain.Principal{
				TenantID: gateTenantAcme, UserID: "dmitry", Roles: []string{"admin"},
			},
			Expect: []string{},
			Never: []string{
				"acme-platform-runbook", "acme-hr-policy", "acme-secret",
				"acme-no-rights", "acme-note-anna", "globex-foreign", "globex-platform",
			},
		},
		{
			Name: "elena",
			Principal: domain.Principal{
				TenantID: gateTenantAcme, UserID: "elena", Groups: []string{"team-backend"},
			},
			Expect: []string{"acme-platform-runbook", "acme-platform-draft", "acme-note-elena"},
			Never: []string{
				"acme-hr-for-anna", "acme-hr-policy", "acme-secret",
				"acme-note-anna", "acme-note-boris", "globex-foreign", "globex-platform",
			},
		},
		{
			Name:      "ghost",
			Principal: domain.Principal{TenantID: gateTenantAcme, UserID: "ghost"},
			Expect:    []string{},
			Never:     []string{"acme-platform-runbook", "acme-secret", "globex-foreign"},
		},
		{
			Name: "zoe",
			Principal: domain.Principal{
				TenantID: gateTenantGlobex, UserID: "zoe", Groups: []string{"team-foreign"},
			},
			Expect: []string{"globex-foreign", "globex-platform"},
			Never: []string{
				"acme-platform-runbook", "acme-hr-policy", "acme-secret",
				"acme-note-anna", "acme-no-rights",
			},
		},
	}
}

// ── Матрица ─────────────────────────────────────────────────────────────────

// aclCell — одна ячейка матрицы «пользователь × документ».
type aclCell struct {
	User     string
	Document string
	Oracle   bool
	Filtered bool
}

// aclMatrix — полный результат прогона матрицы.
type aclMatrix struct {
	Cells   []aclCell
	Visible map[string]map[string]bool
}

// Leaks возвращает пары, где оракул запретил, а фильтр пропустил.
func (m aclMatrix) Leaks() []aclCell {
	return m.selectCells(func(c aclCell) bool { return !c.Oracle && c.Filtered })
}

// Misses возвращает пары, где оракул разрешил, а фильтр отсёк.
func (m aclMatrix) Misses() []aclCell {
	return m.selectCells(func(c aclCell) bool { return c.Oracle && !c.Filtered })
}

func (m aclMatrix) selectCells(want func(aclCell) bool) []aclCell {
	var out []aclCell
	for _, cell := range m.Cells {
		if want(cell) {
			out = append(out, cell)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].User != out[j].User {
			return out[i].User < out[j].User
		}
		return out[i].Document < out[j].Document
	})
	return out
}

// runACLMatrix прогоняет матрицу против заданного оракула.
//
// q — ports.Queryer, на котором работает НАСТОЯЩИЙ ACLResolver. Одна и та же
// функция обслуживает и подставной Queryer, и живой PostgreSQL: сравнение
// оракула с фильтром не должно расходиться между стендами.
func runACLMatrix(t *testing.T, q ports.Queryer,
	users []gateUserFixture, corpus []gateDoc,
	index *esadapter.Index, wire *gateWire, cfg domain.IndexingConfig) aclMatrix {
	t.Helper()
	ctx := context.Background()
	resolver := pgadapter.NewACLResolver()
	matrix := aclMatrix{Visible: map[string]map[string]bool{}}

	for _, user := range users {
		scope, err := resolver.ResolveScope(ctx, q, pgadapter.ScopeParams{
			Principal: user.Principal, KBID: gateKBID,
		})
		if err != nil {
			t.Fatalf("ResolveScope(%s): %v", user.Name, err)
		}
		filter := gateFilterOf(t, index, wire, scope, cfg)
		visible := gateVisible(t, filter, corpus)

		matrix.Visible[user.Name] = visible
		for _, doc := range corpus {
			matrix.Cells = append(matrix.Cells, aclCell{
				User:     user.Name,
				Document: doc.ID,
				// Оракул §6: свой тенант И (контейнер ИЛИ принципал).
				Oracle:   doc.TenantID == scope.TenantID && scope.Grants(doc.Rights),
				Filtered: visible[doc.ID],
			})
		}
	}
	return matrix
}

// TestМатрицаПравНеТечёт — ворота §9 «Права». Прогоняется на каждом PR.
//
// Падает громко и поимённо: каждая строка отчёта называет пользователя и
// документ, потому что «ошибка при утечке» без имён бесполезна в разборе PR.
func TestМатрицаПравНеТечёт(t *testing.T) {
	fixture := gateFixtureWorld()
	cfg := gateConfig()
	wire := newGateWire(t)
	index := gateAdapter(t, wire)
	corpus := gateIndexCorpus(t, index, wire, cfg, fixture.corpus)

	matrix := runACLMatrix(t, &gateQueryer{world: fixture.world},
		fixture.users, corpus, index, wire, cfg)

	// Полнота прогона: каждая ячейка «пользователь × документ» обязана быть
	// посчитана. Уменьшенная матрица прошла бы все проверки выше, не проверив
	// ничего, поэтому число клеток зашито.
	want := len(fixture.users) * len(fixture.corpus)
	if len(matrix.Cells) != want {
		t.Fatalf("проверено %d ячеек матрицы, ожидалось %d (%d пользователей × %d документов)",
			len(matrix.Cells), want, len(fixture.users), len(fixture.corpus))
	}

	if leaks := matrix.Leaks(); len(leaks) > 0 {
		t.Errorf("УТЕЧКА ПРАВ (§9): оракул запретил %d документов, а фильтр индекса их пропустил.\n"+
			"Фильтр отдан в Elasticsearch вместе с правами: каждый такой документ был бы "+
			"прочитан до того, как сработала бы какая-либо проверка.\n%s",
			len(leaks), gateReport(leaks))
	}
	if misses := matrix.Misses(); len(misses) > 0 {
		t.Errorf("ПРОПУЩЕННЫЕ РЕЗУЛЬТАТЫ (§9): оракул разрешил %d документов, а фильтр индекса их отсёк.\n"+
			"Утечки здесь нет, но пользователь не получает то, что ему положено.\n%s",
			len(misses), gateReport(misses))
	}
}

// gateReport печатает список ячеек таблицей: пользователь, документ, что
// сказал оракул и что сказал фильтр.
func gateReport(cells []aclCell) string {
	var out strings.Builder
	out.WriteString("\n  ПОЛЬЗОВАТЕЛЬ  |  ДОКУМЕНТ  |  ОРАКУЛ  |  ФИЛЬТР\n")
	for _, cell := range cells {
		fmt.Fprintf(&out, "  %-12s  |  %-21s  |  %-7v |  %v\n",
			cell.User, cell.Document, cell.Oracle, cell.Filtered)
	}
	return out.String()
}

// TestОжиданияМатрицыСовпадаютСОракулом — третий источник правды.
//
// Без этого теста ворота проверяли бы согласие двух реализаций между собой и
// молча проходили бы, если бы обе стали запрещать всё. Здесь ожидания
// написаны в gateUsers от руки и обязаны совпасть с оракулом ДО того, как
// начнётся сравнение с фильтром.
func TestОжиданияМатрицыСовпадаютСОракулом(t *testing.T) {
	fixture := gateFixtureWorld()
	queryer := &gateQueryer{world: fixture.world}
	resolver := pgadapter.NewACLResolver()
	allDocs := make([]string, 0, len(fixture.corpus))
	for _, doc := range fixture.corpus {
		allDocs = append(allDocs, doc.ID)
	}
	sort.Strings(allDocs)

	for _, user := range fixture.users {
		t.Run(user.Name, func(t *testing.T) {
			scope, err := resolver.ResolveScope(context.Background(), queryer,
				pgadapter.ScopeParams{Principal: user.Principal, KBID: gateKBID})
			if err != nil {
				t.Fatalf("ResolveScope: %v", err)
			}
			var got []string
			for _, doc := range fixture.corpus {
				if doc.TenantID == scope.TenantID && scope.Grants(doc.Rights) {
					got = append(got, doc.ID)
				}
			}
			sort.Strings(got)
			want := slices.Clone(user.Expect)
			sort.Strings(want)
			if !slices.Equal(got, want) {
				t.Errorf("оракул разрешил пользователю %v, а фикстура ждёт %v.\n"+
					"Это расхождение фикстуры, а не прав: сначала почините gateUsers, "+
					"иначе ворота будут проверять неверный эталон", got, want)
			}
			// Отрицания обязаны быть настоящими отрицаниями: иначе Never
			// ничего не проверяет.
			for _, id := range user.Never {
				if slices.Contains(got, id) {
					t.Errorf("документ %s в списке Never, но оракул его разрешает: "+
						"список отрицаний в gateUsers врёт", id)
				}
			}
		})
	}
}

// TestМатрицаНеПустая — ворота обязаны что-то проверять.
//
// Явная проверка объёма: минимум один пользователь с правами, минимум одна
// разрешённая пара и ни одной утечки при полном переборе. Тест, который
// прошёл бы на пустом мире, ничего не говорит о правах.
func TestМатрицаНеПустая(t *testing.T) {
	fixture := gateFixtureWorld()
	cfg := gateConfig()
	wire := newGateWire(t)
	index := gateAdapter(t, wire)
	corpus := gateIndexCorpus(t, index, wire, cfg, fixture.corpus)

	matrix := runACLMatrix(t, &gateQueryer{world: fixture.world},
		fixture.users, corpus, index, wire, cfg)

	allowed := 0
	usersWithRights := map[string]bool{}
	docsWithRights := map[string]bool{}
	for _, cell := range matrix.Cells {
		if !cell.Oracle {
			continue
		}
		allowed++
		usersWithRights[cell.User] = true
		docsWithRights[cell.Document] = true
	}
	if len(usersWithRights) < 3 {
		t.Errorf("в rights видят только %d пользователей: %v — цепочка AGDLP в фикстуре "+
			"почти не работает, и ворота ничего не проверяют", len(usersWithRights), gateSortedKeys(usersWithRights))
	}
	if len(docsWithRights) < 5 {
		t.Errorf("прав есть только на %d документах: %v — корпус вырожден",
			len(docsWithRights), gateSortedKeys(docsWithRights))
	}
	if allowed < 10 {
		t.Errorf("разрешённых пар «пользователь × документ» всего %d: матрица слишком мала", allowed)
	}
	if denied := len(matrix.Cells) - allowed; denied < 30 {
		t.Errorf("запрещённых пар всего %d: ворота почти не проверяют запрет", denied)
	}
}

// ── Именованные случаи §9 ──────────────────────────────────────────────────

// TestПраваПоСлучаям — четыре случая, названные в §9 и в дорожной карте,
// каждый отдельным подтестом. Матрица выше даёт полный перебор; здесь важно
// другое: при падении видно, КАКОЕ правило §6 сломалось.
//
// Подтесты переиспользуют один и тот же корпус и тот же настоящий фильтр —
// отдельной логики в них нет, только утверждения.
func TestПраваПоСлучаям(t *testing.T) {
	fixture := gateFixtureWorld()
	cfg := gateConfig()
	wire := newGateWire(t)
	index := gateAdapter(t, wire)
	corpus := gateIndexCorpus(t, index, wire, cfg, fixture.corpus)

	visibleFor := func(t *testing.T, principal domain.Principal) map[string]bool {
		t.Helper()
		scope, queryer := gateResolveScope(t, fixture.world, principal)
		byUser, byGroup := queryer.Queries()
		if byUser == 0 || byGroup == 0 {
			t.Fatalf("оракул не пошёл по обеим ветвям AGDLP (membership=%d, claim=%d): "+
				"половина прав не проверяется", byUser, byGroup)
		}
		return gateVisible(t, gateFilterOf(t, index, wire, scope, cfg), corpus)
	}

	user := func(name string) domain.Principal {
		for _, fixtureUser := range fixture.users {
			if fixtureUser.Name == name {
				return fixtureUser.Principal
			}
		}
		t.Fatalf("в фикстуре нет пользователя %s", name)
		return domain.Principal{}
	}

	t.Run("ЛичнаяЗаметкаВиднаТолькоАвтору", func(t *testing.T) {
		notes := []string{"acme-note-anna", "acme-note-boris", "acme-note-elena"}
		owners := map[string]string{
			"acme-note-anna": "anna", "acme-note-boris": "boris", "acme-note-elena": "elena",
		}
		for _, reader := range []string{"anna", "boris", "elena", "dmitry", "zoe"} {
			visible := visibleFor(t, user(reader))
			for _, note := range notes {
				got := visible[note]
				want := owners[note] == reader
				if got != want {
					t.Errorf("личная заметка %s: пользователь %s её %s, ожидалось %v",
						note, reader, gateSeen(got), want)
				}
			}
		}
	})

	t.Run("ПраваНеПересекаютТенантов", func(t *testing.T) {
		// space-platform есть у обоих тенантов. Term по space_id одинаковый,
		// и различает их только term по tenant_id.
		acme := visibleFor(t, user("anna"))
		globex := visibleFor(t, user("zoe"))
		for _, doc := range fixture.corpus {
			switch doc.TenantID {
			case gateTenantAcme.String():
				if globex[doc.ID] {
					t.Errorf("утечка: пользователь тенанта Globex видит документ %s тенанта Acme", doc.ID)
				}
			case gateTenantGlobex.String():
				if acme[doc.ID] {
					t.Errorf("утечка: пользователь тенанта Acme видит документ %s тенанта Globex", doc.ID)
				}
			}
		}
		if !globex["globex-platform"] || acme["globex-platform"] {
			t.Errorf("одноимённые контейнеры не разделены тенантом: globex-platform виден = %v, "+
				"у acme = %v", globex["globex-platform"], acme["globex-platform"])
		}
	})

	t.Run("ДокументБезПравНеВиденНикому", func(t *testing.T) {
		for _, docID := range []string{"acme-secret", "acme-no-rights", "acme-hr-policy"} {
			for _, reader := range []string{"anna", "boris", "elena", "dmitry", "ghost", "zoe"} {
				if visibleFor(t, user(reader))[docID] {
					t.Errorf("утечка: документ без прав %s виден пользователю %s", docID, reader)
				}
			}
		}
	})

	t.Run("АдминНеОбходитПрава", func(t *testing.T) {
		// §6: deny нет, только разрешения, и роль разворачивается в терм.
		// Обхода через роль не существует — и воротa это проверяет явно.
		visible := visibleFor(t, user("dmitry"))
		if len(visible) != 0 {
			t.Errorf("пользователь с ролью admin видит %v: роль не должна обходить права §6",
				gateSortedKeys(visible))
		}
	})

	t.Run("ТочечноеИсключениеОткрываетДокументОдному", func(t *testing.T) {
		// Документ в контейнере без прав (space-hr), открытый исключением
		// ровно одному человеку. Читать его должна только анна: ни коллеги по
		// контейнеру, ни админ, ни чужой тенант.
		wantVisible := "acme-hr-for-anna"
		for _, reader := range []string{"boris", "elena", "dmitry", "ghost", "zoe"} {
			if visibleFor(t, user(reader))[wantVisible] {
				t.Errorf("утечка: документ %s, открытый только анне, виден пользователю %s",
					wantVisible, reader)
			}
		}
		if !visibleFor(t, user("anna"))[wantVisible] {
			t.Errorf("анна не видит документ, открытый ей исключением %s: "+
				"ветка terms по acl не работает", wantVisible)
		}
	})

	t.Run("ИсключениеНеСужаетВидимостьКонтейнера", func(t *testing.T) {
		// §6: запретов нет, только разрешения, поэтому исключение внутри
		// контейнера РАСШИРЯЕТ доступ и не может его сузить. Черновик с
		// исключением для бориса виден и всем, кому доступен контейнер.
		//
		// Это важно понимать до того, как кто-то попробует закрыть черновик
		// «исключением для одного»: так не выйдет, и черновик останется у
		// всех. Сужение потребовало бы deny, а §6 его запрещает.
		for _, reader := range []string{"anna", "boris", "elena"} {
			if !visibleFor(t, user(reader))["acme-platform-draft"] {
				t.Errorf("черновик в общем контейнере не виден пользователю %s, "+
					"которому контейнер доступен: исключение начало сужать видимость, "+
					"а §6 запретов не имеет", reader)
			}
		}
	})
}

func gateSeen(got bool) string {
	if got {
		return "ВИДИТ"
	}
	return "не видит"
}

// TestВоротаЛовятУтечку — отрицательный контроль самих ворот.
//
// Тест выше проходит, когда всё хорошо. Этот проходит, когда ворота ОБЯЗАНЫ
// падать, и падают они на настоящей утечке.
//
// ЧТО СДЕЛАНО. Клон фильтра anna намеренно подменён: остаётся весь её набор
// прав, но в область добавлены контейнеры ЧУЖОГО тенанта. Оракул при этом
// остаётся настоящим и по-прежнему anna-овским, поэтому он Globex по-прежнему
// запрещает. Фильтр собран НАСТОЯЩИМ aclAccessClause по подменённой области и
// Globex открывает.
//
// Если сравнение ворот живое, matrix.Leaks() обязан быть непустым. Если же
// подмена фильтра не влияет на результат, значит ворота проверяют что-то другое
// (свой копией правила, константой, всегда пустым результатом) и им нельзя
// верить как воротам.
func TestВоротаЛовятУтечку(t *testing.T) {
	fixture := gateFixtureWorld()
	cfg := gateConfig()
	wire := newGateWire(t)
	index := gateAdapter(t, wire)
	corpus := gateIndexCorpus(t, index, wire, cfg, fixture.corpus)

	anna := gateUsers()[0].Principal
	scope, _ := gateResolveScope(t, fixture.world, anna)

	// Клон области с правами на чужие контейнеры. Именно так выглядел бы баг
	// «забыли отфильтровать по тенанту» или «цепочка AGDLP соединила не ту
	// группу».
	leaky := scope
	leaky.TenantID = gateTenantGlobex.String()
	leaky.SpaceIDs = append(slices.Clone(scope.SpaceIDs), gateGlobexForeign)
	leaky.Principals = append(slices.Clone(scope.Principals), "user:zoe")

	visible := gateVisible(t, gateFilterOf(t, index, wire, leaky, cfg), corpus)
	matrix := aclMatrix{Visible: map[string]map[string]bool{"anna(подмена)": visible}}
	for _, doc := range corpus {
		oracle := doc.TenantID == scope.TenantID && scope.Grants(doc.Rights)
		matrix.Cells = append(matrix.Cells, aclCell{
			User: "anna(подмена)", Document: doc.ID, Oracle: oracle, Filtered: visible[doc.ID],
		})
	}

	leaks := matrix.Leaks()
	if len(leaks) == 0 {
		t.Fatalf("подменённый фильтр открыл чужие документы, а ворота не заметили: " +
			"значит сравнение оракула с фильтром не работает и ворота §9 бесполезны")
	}
	if !slices.Contains(gateLeakDocuments(leaks), "globex-foreign") {
		t.Errorf("подмена не открыла globex-foreign (найдено %v) — тест контроля "+
			"сам себе не доказывает полезность", gateLeakDocuments(leaks))
	}
	t.Logf("отчёт ворот, который должен был сработать:\n%s", gateReport(leaks))
}

func gateLeakDocuments(cells []aclCell) []string {
	out := make([]string, 0, len(cells))
	for _, cell := range cells {
		out = append(out, cell.Document)
	}
	return out
}

// TestПустаяОбластьЗакрываетВыдачу — пользователь без прав не получает
// документов и не стоит обращения к кластеру.
//
// Это настоящее поведение KeywordSearchScoped, а не соглашение ворот: пустой
// Scope.AllowsNothing() срезает запрос до сети. Проверяется и нулевой счётчик
// обращений, и нулевая выдача — иначе нулевой результат можно получить
// адаптером, который вообще ничего не делает.
func TestПустаяОбластьЗакрываетВыдачу(t *testing.T) {
	cfg := gateConfig()
	wire := newGateWire(t)
	index := gateAdapter(t, wire)

	hits, err := index.KeywordSearchScoped(context.Background(), "учебная тревога",
		domain.AccessScope{TenantID: gateTenantAcme.String(), KBID: gateKBID}, cfg, 10)
	if err != nil {
		t.Fatalf("KeywordSearchScoped: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("без прав возвращено %d кандидатов", len(hits))
	}
	if got := wire.searches(); got != 0 {
		t.Errorf("обращений к кластеру = %d, ожидался 0: пользователь без прав не должен стоить запроса",
			got)
	}
}

// TestОбластьИзПробеловДаётMatchNone — defense in depth фильтра.
//
// Пустые строки в правах невозможны из базы: container_key защищён CHECK
// (btrim <> ”). Но если этот CHECK обойти, область из одних пробелов обязана
// закрыть выдачу, а не разомкнуть её. Проверяется настоящий match_none,
// собранный aclAccessClause, и то, что он не матчит НИ ОДИН документ корпуса.
//
// Область собрана вручную намеренно: оракул прав такой вернуть не может, и
// именно поэтому этот случай не проверяется матрицей.
func TestОбластьИзПробеловДаётMatchNone(t *testing.T) {
	fixture := gateFixtureWorld()
	cfg := gateConfig()
	wire := newGateWire(t)
	index := gateAdapter(t, wire)
	corpus := gateIndexCorpus(t, index, wire, cfg, fixture.corpus)

	scope := domain.AccessScope{
		TenantID:   gateTenantAcme.String(),
		KBID:       gateKBID,
		SpaceIDs:   []string{"", "  "},
		Principals: []string{"", "\t"},
	}
	filter := gateFilterOf(t, index, wire, scope, cfg)
	if !filter.Requested {
		t.Fatal("запрос не ушёл в сеть: область из пробелов обязана превратиться в match_none " +
			"внутри запроса, а не в молчаливый пустой ответ")
	}
	if !strings.Contains(wire.lastBodyOf(t, "_search"), `"match_none"`) {
		t.Errorf("в теле запроса нет match_none: %s", wire.lastBodyOf(t, "_search"))
	}
	if visible := gateVisible(t, filter, corpus); len(visible) != 0 {
		t.Errorf("match_none пропустил документы %v", gateSortedKeys(visible))
	}
}

// TestФильтрПравНесётОбязательныеТермы — фильтр состоит из term по тенанту,
// по базе знаний, по профилю векторов и клозы прав, и все четыре присутствуют
// в ушедшем теле запроса.
//
// Это отдельная проверка от матрицы: она ловит выпадение ЛЮБОГО из четырёх
// термов, включая те, что не про права. Матрица права проверяет в том числе
// term по тенанту, но явное утверждение здесь даёт читаемое сообщение.
func TestФильтрПравНесётОбязательныеТермы(t *testing.T) {
	fixture := gateFixtureWorld()
	cfg := gateConfig()
	wire := newGateWire(t)
	index := gateAdapter(t, wire)
	scope, _ := gateResolveScope(t, fixture.world, gateUsers()[0].Principal)
	filter := gateFilterOf(t, index, wire, scope, cfg)

	body := wire.lastBodyOf(t, "_search")
	for _, want := range []string{
		`"term":{"tenant_id":"` + gateTenantAcme.String() + `"}`,
		`"term":{"kb_id":"` + gateKBID + `"}`,
		`"term":{"embedding_profile_id":"` + cfg.ProfileID() + `"}`,
		`"terms":{"space_id":[`,
		`"terms":{"acl":[`,
		`"minimum_should_match":1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("в теле запроса нет %s:\n%s", want, body)
		}
	}

	// Чужая база знаний отсекается, даже при полных правах.
	foreign := domain.DocumentRightsFor("space-platform", nil)
	foreignDoc := map[string]any{
		"tenant_id": gateTenantAcme.String(), "kb_id": "kb-other",
		"document_id": "doc-foreign-kb", "space_id": foreign.SpaceID,
		"acl": foreign.ExceptionTerms(), "embedding_profile_id": cfg.ProfileID(),
	}
	if gateESMatch(t, filter.Clauses, foreignDoc) {
		t.Error("документ чужой базы знаний прошёл фильтр: term по kb_id не работает")
	}
}

// TestПоляПравВМаппингеИндексаОбъявленыKeyword — опора для тестовой заглушки.
//
// gateESMatch сравнивает значения точно и не анализирует текст, потому что
// поля прав объявлены keyword. Этот тест проверяет объявление по исходнику
// адаптера: если space_id или acl переедут в text, настоящий Elasticsearch
// начнёт разбирать «user:anna» на части, а заглушка — нет, и ворота
// разойдутся с продом молча.
//
// Разбор исходника, а не вызов маппинга: indexBody не экспортирован, а
// EnsureIndex требует кластера.
func TestПоляПравВМаппингеИндексаОбъявленыKeyword(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "store", "es", "index_body.go"))
	if err != nil {
		t.Fatalf("чтение index_body.go: %v", err)
	}
	text := string(source)
	for _, field := range []string{"aclFieldSpace", "aclFieldPrincipals"} {
		declaration := field + `:`
		at := strings.Index(text, declaration)
		if at < 0 {
			t.Fatalf("в index_body.go нет поля прав %s", field)
		}
		line := text[at:]
		if end := strings.IndexByte(line, '\n'); end >= 0 {
			line = line[:end]
		}
		if !strings.Contains(line, `"type": "keyword"`) {
			t.Errorf("поле прав %s объявлено не как keyword: %q — тестовая заглушка "+
				"трактует его как keyword и разойдётся с настоящим Elasticsearch", field, line)
		}
	}
	// Имена полей в mapping обязаны совпадать с именами в запросе: расхождение
	// имён не открывает доступ, а тихо его закрывает.
	filterSource, err := os.ReadFile(filepath.Join("..", "store", "es", "acl_filter.go"))
	if err != nil {
		t.Fatalf("чтение acl_filter.go: %v", err)
	}
	for _, literal := range []string{`"space_id"`, `"acl"`} {
		if !strings.Contains(string(filterSource), literal) {
			t.Errorf("в acl_filter.go нет поля %s: сторона запроса и сторона mapping разошлись", literal)
		}
	}
}

// TestОракулОтказываетБезСубъекта — ворота прав начинаются с оракула, и оракул
// обязан fail-closed на запросе без тенанта или без пользователя.
//
// Пустой tenant в WHERE отбросил бы всё молча, а ошибка говорит вызывающему,
// что пришёл запрос без субъекта. Это отличие проверяется здесь явно.
func TestОракулОтказываетБезСубъекта(t *testing.T) {
	fixture := gateFixtureWorld()
	resolver := pgadapter.NewACLResolver()
	queryer := &gateQueryer{world: fixture.world}

	cases := map[string]domain.Principal{
		"без тенанта":   {UserID: "anna"},
		"без user_id":   {TenantID: gateTenantAcme},
		"совсем пусто":  {},
		"пустой тенант": {TenantID: uuid.Nil, UserID: "anna"},
	}
	for name, principal := range cases {
		t.Run(name, func(t *testing.T) {
			scope, err := resolver.ResolveScope(context.Background(), queryer,
				pgadapter.ScopeParams{Principal: principal, KBID: gateKBID})
			if err == nil {
				t.Fatalf("оракул вернул область %+v на запрос без субъекта: "+
					"права без тенанта — это права всех", scope)
			}
			if !scope.AllowsNothing() {
				t.Errorf("на отказ область всё же непуста: %+v", scope)
			}
		})
	}
}

// ── Тот же прогон против настоящего PostgreSQL ──────────────────────────────

// gateLiveDSN — адрес стенда. Тот же, что у internal/store/pg, чтобы одна
// переменная CI управляла обоими пакетами.
func gateLiveDSN() string {
	if custom := os.Getenv("KB_GO_TEST_DATABASE_URL"); custom != "" {
		return custom
	}
	return "postgres://kb:dev@127.0.0.1:55432/kb_go_test?sslmode=disable"
}

// TestМатрицаПравНаНастоящейPostgres — ТА ЖЕ матрица против живой базы.
//
// ГЛАВНОЕ: без базы этот тест ПРОПУСКАЕТСЯ, а не проходит. Подставной
// Queryer в основном прогоне заменяет базу, а вместе с ней и проверку
// правильности JOIN-ов в тексте SQL оракула и ограничений CHECK схемы. Этот
// прогон закрывает именно их — на настоящих таблицах и настоящих
// миграциях.
//
// Намеренно своя схема (gateLiveSchema): тесты internal/store/pg чистят свои
// таблицы TRUNCATE, и общая база сделала бы два пакета зависящими от порядка
// запуска. Своя схема исключает пересечение целиком.
func TestМатрицаПравНаНастоящейPostgres(t *testing.T) {
	if testing.Short() {
		t.Skip("интеграционный прогон матрицы: нужен PostgreSQL (запустите без -short)")
	}
	fixture := gateFixtureWorld()
	cfg := gateConfig()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)

	db := gateOpenLiveSchema(t, ctx, gateLiveSchemaName,
		"0001_initial_schema.up.sql", "0002_lake_acl.up.sql")
	gateSeedWorld(t, ctx, db, fixture)

	wire := newGateWire(t)
	index := gateAdapter(t, wire)
	corpus := gateIndexCorpus(t, index, wire, cfg, fixture.corpus)

	matrix := runACLMatrix(t, db, fixture.users, corpus, index, wire, cfg)

	if leaks := matrix.Leaks(); len(leaks) > 0 {
		t.Errorf("УТЕЧКА ПРАВ на живой PostgreSQL: %d документов.\n%s", len(leaks), gateReport(leaks))
	}
	if misses := matrix.Misses(); len(misses) > 0 {
		t.Errorf("ПРОПУЩЕННЫЕ РЕЗУЛЬТАТЫ на живой PostgreSQL: %d документов.\n%s",
			len(misses), gateReport(misses))
	}
	if allowed := len(matrix.Cells) - len(matrix.Leaks()) - len(matrix.Misses()); allowed < 10 {
		t.Errorf("на живой базе разрешено %d пар — оракул, скорее всего, ничего не нашёл: "+
			"проверьте фикстуру и миграцию", allowed)
	}
}

// gateLiveSchemaName — имя отдельной схемы ворот прав. Своя схема исключает
// пересечение с internal/store/pg, который чистит свои таблицы TRUNCATE.
const gateLiveSchemaName = "kb_gates_matrix"

// gateSeedWorld заполняет таблицы §6 содержимым gateWorld.
//
// Порядок обхода неважен для результата (строки независимы), но сортировка
// ключей делает журнал предсказуемым при падении.
func gateSeedWorld(t *testing.T, ctx context.Context, q ports.Queryer, fixture *gateFixture) {
	t.Helper()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := q.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("наполнение прав: %v\n%s", err, sql)
		}
	}
	exec(`INSERT INTO tenants (id, name) VALUES ($1, 'acme'), ($2, 'globex')`,
		gateTenantAcme, gateTenantGlobex)

	world := fixture.world
	for _, tenant := range gateSortedTenants(world.users) {
		for _, key := range gateSortedMapKeys(world.users[tenant]) {
			exec(`INSERT INTO acl_users (id, tenant_id, user_key) VALUES ($1, $2, $3)`,
				world.users[tenant][key], tenant, key)
		}
		for _, key := range gateSortedMapKeys(world.globalGroups[tenant]) {
			exec(`INSERT INTO acl_global_groups (id, tenant_id, group_key) VALUES ($1, $2, $3)`,
				world.globalGroups[tenant][key], tenant, key)
		}
		for _, key := range gateSortedMapKeys(world.containers[tenant]) {
			exec(`INSERT INTO acl_containers (id, tenant_id, container_key, kind, source)
			      VALUES ($1, $2, $3, 'space', 'confluence')`,
				world.containers[tenant][key], tenant, key)
		}
		for _, key := range gateSortedMapKeys(world.resourceGroups[tenant]) {
			exec(`INSERT INTO acl_resource_groups (id, tenant_id, resource_key) VALUES ($1, $2, $3)`,
				world.resourceGroups[tenant][key], tenant, key)
		}
		for _, member := range world.globalMembers[tenant] {
			exec(`INSERT INTO acl_global_group_members (tenant_id, group_id, user_id)
			      VALUES ($1, $2, $3)`, tenant, member.Group, member.User)
		}
		for _, member := range world.resourceMember[tenant] {
			exec(`INSERT INTO acl_resource_group_members (tenant_id, resource_group_id, global_group_id)
			      VALUES ($1, $2, $3)`, tenant, member.Resource, member.Global)
		}
		for _, perm := range world.permissions[tenant] {
			exec(`INSERT INTO acl_permissions (tenant_id, resource_group_id, container_id, action, granted_by)
			      VALUES ($1, $2, $3, $4, 'system')`, tenant, perm.Resource, perm.Container, perm.Action)
		}
	}
}

// gateSortedTenants — тенанты, для которых в мире вообще есть данные.
func gateSortedTenants(m map[uuid.UUID]map[string]uuid.UUID) []uuid.UUID {
	var out []uuid.UUID
	for tenant, entries := range m {
		if len(entries) > 0 {
			out = append(out, tenant)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

func gateSortedMapKeys(m map[string]uuid.UUID) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
