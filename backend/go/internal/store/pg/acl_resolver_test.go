package postgres

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	domain "github.com/kbsearch/kb/internal/envelope"
	documents "github.com/kbsearch/kb/internal/ingest/parser"
	"github.com/kbsearch/kb/internal/ports"
)

// Тесты оракула прав §6. Главные ворота фазы A — «0 утечек прав» (§9), и этот
// файл проверяет их по частям: пользователь с полной цепочкой AGDLP видит
// контейнер, пользователь с одним лишь точечным исключением не видит ничего
// лишнего, а чужой tenant не видит вообще ничего.
//
// Всё это — тесты на живом PostgreSQL, тем же стендом, что и остальной пакет.
// Без БД и в -short — пропуск (requireLakeDB в lake_test.go).

// Идентификаторы фикстур прав. Отдельные от констант 0001, чтобы seed
// тестов lake и seed тестов прав не мешали друг другу при параллельном
// запуске пакета.
var (
	aclUserAnna  = uuid.MustParse("a1111111-1111-4111-8111-111111111111")
	aclUserBoris = uuid.MustParse("a2222222-2222-4222-8222-222222222222")
	// Глобальная группа «team-backend» и ресурсная «res:space-platform:read».
	aclGlobalBackend = uuid.MustParse("b1111111-1111-4111-8111-111111111111")
	aclResourceRead  = uuid.MustParse("d1111111-1111-4111-8111-111111111111")
	// Контейнеры: платформа (выдано право) и кадры (не выдано).
	aclSpacePlatform = uuid.MustParse("c1111111-1111-4111-8111-111111111111")
	aclSpaceHR       = uuid.MustParse("c2222222-2222-4222-8222-222222222222")

	// Ключи групп в строковом виде — так они лежат в claim groups IdP (§6).
	aclGroupKeyBackend = "team-backend"
	aclResourceKeyRead = "res:space-platform:read"
)

// aclPrincipal собирает субъекта запроса.
func aclPrincipal(tenant uuid.UUID, userID string, groups ...string) domain.Principal {
	return domain.Principal{
		TenantID: tenant,
		UserID:   userID,
		Groups:   groups,
	}
}

// seedACLTenants создаёт тенанты для тестов прав.
func seedACLTenants(t *testing.T, ctx context.Context) {
	t.Helper()
	for _, tenant := range []uuid.UUID{tenantID, otherTenant} {
		if _, err := testDB.Exec(ctx,
			`INSERT INTO tenants (id, name) VALUES ($1, 'local') ON CONFLICT DO NOTHING`, tenant); err != nil {
			t.Fatalf("создание tenant %s: %v", tenant, err)
		}
	}
}

// seedAGDLPChain создаёт полную цепочку прав §6 для тенанта: пользователя,
// глобальную группу, контейнеры, ресурсную группу и разрешение read на
// space-platform (но не на space-hr).
//
// Два контейнера нужны для проверки отрицания: оракул, который вернул бы всё
// подряд, прошёл бы тест «пользователь видит свой контейнер», и упал бы
// только здесь.
func seedAGDLPChain(t *testing.T, ctx context.Context) {
	t.Helper()
	seedACLTenants(t, ctx)

	execRaw(t, ctx, `INSERT INTO acl_users (id, tenant_id, user_key, email)
		VALUES ($1, $2, 'anna', 'anna@corp.ru'), ($3, $2, 'boris', 'boris@corp.ru')`,
		aclUserAnna, tenantID, aclUserBoris)

	execRaw(t, ctx, `INSERT INTO acl_global_groups (id, tenant_id, group_key)
		VALUES ($1, $2, $3)`, aclGlobalBackend, tenantID, aclGroupKeyBackend)

	execRaw(t, ctx, `INSERT INTO acl_containers (id, tenant_id, container_key, kind, source)
		VALUES ($1, $2, 'space-platform', 'space', 'confluence'),
		       ($3, $2, 'space-hr', 'space', 'confluence')`,
		aclSpacePlatform, tenantID, aclSpaceHR)

	execRaw(t, ctx, `INSERT INTO acl_resource_groups (id, tenant_id, resource_key)
		VALUES ($1, $2, $3)`, aclResourceRead, tenantID, aclResourceKeyRead)

	execRaw(t, ctx, `INSERT INTO acl_global_group_members (tenant_id, group_id, user_id)
		VALUES ($1, $2, $3)`, tenantID, aclGlobalBackend, aclUserAnna)

	execRaw(t, ctx, `INSERT INTO acl_resource_group_members (tenant_id, resource_group_id, global_group_id)
		VALUES ($1, $2, $3)`, tenantID, aclResourceRead, aclGlobalBackend)

	execRaw(t, ctx, `INSERT INTO acl_permissions (tenant_id, resource_group_id, container_id, action, granted_by)
		VALUES ($1, $2, $3, 'read', 'user:anna')`, tenantID, aclResourceRead, aclSpacePlatform)
}

// TestResolveScopeAllowsFullAGDLPChain — эталонный случай §6: пользователь в
// глобальной группе, глобальная группа в ресурсной, права назначены ресурсной
// группе на контейнер. Такой пользователь читает контейнер.
//
// Проверяется и контейнер, и терм ресурсной группы: документ получает в
// индексе строку «group:res:space-platform:read», и без этого терма личное
// исключение на документе внутри контейнера не сработало бы.
func TestResolveScopeAllowsFullAGDLPChain(t *testing.T) {
	requireLakeDB(t)
	ctx := testContext(t)
	seedAGDLPChain(t, ctx)

	scope, err := NewACLResolver().ResolveScope(ctx, testDB, ScopeParams{
		Principal: aclPrincipal(tenantID, "anna"),
		KBID:      kbID.String(),
	})
	if err != nil {
		t.Fatalf("ResolveScope: %v", err)
	}

	if !slices.Contains(scope.SpaceIDs, "space-platform") {
		t.Errorf("space-platform не в доступе: %v", scope.SpaceIDs)
	}
	// Контейнер без выданного права обязан остаться закрытым: право выдано
	// ресурсной группе на space-platform, а не на весь тенант.
	if slices.Contains(scope.SpaceIDs, "space-hr") {
		t.Errorf("space-hr в доступе без выданного права: %v", scope.SpaceIDs)
	}
	if !slices.Contains(scope.Principals, "group:"+aclResourceKeyRead) {
		t.Errorf("в принципалах нет терма ресурсной группы: %v", scope.Principals)
	}
	if !slices.Contains(scope.Principals, "user:anna") {
		t.Errorf("в принципалах нет терма пользователя: %v", scope.Principals)
	}
	if scope.AllowsNothing() {
		t.Error("scope с правом на контейнер не должен считаться пустым")
	}
}

// TestResolveScopeAllowsUserWithNothingButOwnPrincipal — пользователь без
// групп и без прав. Единственный его терм — «user:<id>», и именно он открывает
// личные заметки (§6: «права личных заметок: acl = [user:<id>] и больше никто»).
//
// Контейнеров при этом ноль: объект с одним лишь своим термом не должен
// получать доступ к чужим space. Если бы в этот тест попал контейнер, тест
// означал бы утечку прав на контейнер — главный ворота фазы A.
func TestResolveScopeAllowsUserWithNothingButOwnPrincipal(t *testing.T) {
	requireLakeDB(t)
	ctx := testContext(t)
	seedAGDLPChain(t, ctx)

	scope, err := NewACLResolver().ResolveScope(ctx, testDB, ScopeParams{
		Principal: aclPrincipal(tenantID, "boris"),
		KBID:      kbID.String(),
	})
	if err != nil {
		t.Fatalf("ResolveScope: %v", err)
	}

	if len(scope.SpaceIDs) != 0 {
		t.Errorf("у пользователя без прав есть контейнеры %v: права не выдавались", scope.SpaceIDs)
	}
	// Личная заметка остаётся доступной: собственный терм обязателен.
	if !slices.Contains(scope.Principals, "user:boris") {
		t.Errorf("в принципалах нет собственного терма: %v", scope.Principals)
	}
	// Чужой терм пользователя не должен появляться ни при каких условиях.
	if slices.Contains(scope.Principals, "user:anna") {
		t.Errorf("в принципалах чужой пользователь: %v", scope.Principals)
	}
	for _, principal := range scope.Principals {
		if strings.HasPrefix(principal, "group:") {
			t.Errorf("у пользователя без групп появился терм ресурсной группы %q: %v", principal, scope.Principals)
		}
	}
}

// TestResolveScopeRejectsCrossTenantUser — пользователь чужого тенанта не
// получает ничего, даже если он состоит в группе, которой право выдано.
//
// Это самая дорогая ошибка, какая возможна в этой таблице: tenant_id в ключе
// внешних соединений оракула — единственное, что отделяет организации друг от
// друга, и ослабление одного предиката в соединении означает утечку всего
// индекса другого тенанта.
func TestResolveScopeRejectsCrossTenantUser(t *testing.T) {
	requireLakeDB(t)
	ctx := testContext(t)
	seedAGDLPChain(t, ctx)

	// Тот же пользователь, но от другого тенанта и с чужой группой в claim.
	foreign := aclPrincipal(otherTenant, "anna", aclGroupKeyBackend)
	scope, err := NewACLResolver().ResolveScope(ctx, testDB, ScopeParams{
		Principal: foreign,
		KBID:      otherKBID.String(),
	})
	if err != nil {
		t.Fatalf("ResolveScope: %v", err)
	}

	if len(scope.SpaceIDs) != 0 {
		t.Errorf("чужой tenant получил контейнеры %v", scope.SpaceIDs)
	}
	if scope.TenantID != otherTenant.String() {
		t.Errorf("tenant_id в scope = %q, ожидался %q", scope.TenantID, otherTenant)
	}
	// Свой терм у чужого пользователя есть (он и в правда anna), но группы
	// своего тенанта он получить не может.
	for _, principal := range scope.Principals {
		if principal == "group:"+aclResourceKeyRead {
			t.Errorf("чужой tenant получил ресурсную группу своего тенанта: %v", scope.Principals)
		}
	}
}

// TestResolveScopeExpandsIdPClaimGroups — §6 MVP-путь: группы приходят из
// claim groups IdP, а не из membership-таблицы. Пользователь, которого в базе
// нет (JIT создаёт его при первом входе), всё равно получает права по своей
// группе.
func TestResolveScopeExpandsIdPClaimGroups(t *testing.T) {
	requireLakeDB(t)
	ctx := testContext(t)
	seedAGDLPChain(t, ctx)

	// Пользователя «vera» в acl_users нет — только claim.
	vera := aclPrincipal(tenantID, "vera", aclGroupKeyBackend)
	scope, err := NewACLResolver().ResolveScope(ctx, testDB, ScopeParams{
		Principal: vera,
		KBID:      kbID.String(),
	})
	if err != nil {
		t.Fatalf("ResolveScope: %v", err)
	}

	if !slices.Contains(scope.SpaceIDs, "space-platform") {
		t.Errorf("пользователь с группой в claim не получил контейнер: %v", scope.SpaceIDs)
	}
	if !slices.Contains(scope.Principals, "user:vera") {
		t.Errorf("нет собственного терма пользователя: %v", scope.Principals)
	}
}

// TestResolveScopeUnionsClaimAndMembership — два источника групп (§6: claim на
// MVP, membership со SCIM на фазе 2) объединяются, и пересечение не
// задваивает контейнеры.
//
// Без объединения пользователь, чья группа есть и в claim, и в membership,
// получил бы на MVP права от claim и потерял бы их в момент прихода SCIM — то
// есть отзыв и выдача прав зависели бы от того, какая из двух таблиц сейчас
// заполнена.
func TestResolveScopeUnionsClaimAndMembership(t *testing.T) {
	requireLakeDB(t)
	ctx := testContext(t)
	seedAGDLPChain(t, ctx)

	// anna уже в membership; добавляем ей ту же группу в claim.
	anna := aclPrincipal(tenantID, "anna", aclGroupKeyBackend)
	scope, err := NewACLResolver().ResolveScope(ctx, testDB, ScopeParams{
		Principal: anna,
		KBID:      kbID.String(),
	})
	if err != nil {
		t.Fatalf("ResolveScope: %v", err)
	}

	if !slices.Contains(scope.SpaceIDs, "space-platform") {
		t.Errorf("контейнер не получен: %v", scope.SpaceIDs)
	}
	if len(scope.SpaceIDs) != 1 {
		t.Errorf("контейнер продублирован: %v", scope.SpaceIDs)
	}
	if len(scope.Principals) != 2 {
		t.Errorf("принципалы %v, ожидались ровно user:anna и одна ресурсная группа", scope.Principals)
	}
}

// TestResolveScopeRejectsEmptySubject — права без субъекта — это дефект
// вызывающего, и он обязан быть виден как ошибка, а не как «ничего не
// разрешено»: второе выглядело бы как корректный пустой ответ и тихо уронило
// бы выдачу.
func TestResolveScopeRejectsEmptySubject(t *testing.T) {
	requireLakeDB(t)
	ctx := testContext(t)
	seedAGDLPChain(t, ctx)

	cases := []struct {
		name string
		in   ScopeParams
	}{
		{name: "без тенанта", in: ScopeParams{Principal: aclPrincipal(uuid.Nil, "anna")}},
		{name: "без пользователя", in: ScopeParams{Principal: aclPrincipal(tenantID, "")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewACLResolver().ResolveScope(ctx, testDB, tc.in); err == nil {
				t.Error("ожидался отказ на права без субъекта")
			}
		})
	}
}

// TestRequireReadSpaceHidesExistence — §6 и правило authorizer.go: запрещённый
// контейнер и несуществующий обязаны отвечать одинаково, иначе перебор ключей
// space_id перечисляет каталог tenant'а.
//
// Тест сравнивает не коды, а тексты ошибок: различаться здесь нечему.
func TestRequireReadSpaceHidesExistence(t *testing.T) {
	requireLakeDB(t)
	ctx := testContext(t)
	seedAGDLPChain(t, ctx)

	resolver := NewACLResolver()
	anna := aclPrincipal(tenantID, "anna")

	allowedErr := resolver.RequireReadSpace(ctx, testDB, anna, "space-platform")
	if allowedErr != nil {
		t.Fatalf("доступ к своему контейнеру отказан: %v", allowedErr)
	}

	forbiddenErr := resolver.RequireReadSpace(ctx, testDB, anna, "space-hr")
	if forbiddenErr == nil {
		t.Fatal("доступ к контейнеру без права разрешён")
	}
	missingErr := resolver.RequireReadSpace(ctx, testDB, anna, "space-nonexistent")
	if missingErr == nil {
		t.Fatal("доступ к несуществующему контейнеру разрешён")
	}

	forbiddenApp, ok := domain.AsAppError(forbiddenErr)
	if !ok {
		t.Fatalf("ошибка запрета не доменная: %v", forbiddenErr)
	}
	missingApp, ok := domain.AsAppError(missingErr)
	if !ok {
		t.Fatalf("ошибка отсутствия не доменная: %v", missingErr)
	}

	if forbiddenApp.Code != domain.CodeNotFound {
		t.Errorf("код отказа по правам = %q, ожидался %q: перебор не должен отличать запрет от отсутствия",
			forbiddenApp.Code, domain.CodeNotFound)
	}
	if forbiddenApp.Message != missingApp.Message {
		t.Errorf("тексты отличаются — по тексту можно перебором выяснить, что контейнер существует: "+
			"запрет %q, отсутствие %q", forbiddenApp.Message, missingApp.Message)
	}
}

// TestCanReadSpaceAnswersForOwnedAndForeign — проверка в глубину из §6: «перед
// тем как отдать top-N, повторно проверяем can_read(user, doc_ids) по
// Postgres». Индекс может отставать по правам, и проверка обязана быть точной
// в обе стороны.
func TestCanReadSpaceAnswersForOwnedAndForeign(t *testing.T) {
	requireLakeDB(t)
	ctx := testContext(t)
	seedAGDLPChain(t, ctx)

	resolver := NewACLResolver()
	cases := []struct {
		name    string
		p       domain.Principal
		spaceID string
		want    bool
	}{
		{name: "свой контейнер", p: aclPrincipal(tenantID, "anna"), spaceID: "space-platform", want: true},
		{name: "чужой контейнер", p: aclPrincipal(tenantID, "anna"), spaceID: "space-hr", want: false},
		{name: "несуществующий", p: aclPrincipal(tenantID, "anna"), spaceID: "space-nonexistent", want: false},
		{name: "чужой tenant", p: aclPrincipal(otherTenant, "anna"), spaceID: "space-platform", want: false},
		{name: "пользователь без прав", p: aclPrincipal(tenantID, "boris"), spaceID: "space-platform", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolver.CanReadSpace(ctx, testDB, tc.p, tc.spaceID)
			if err != nil {
				t.Fatalf("CanReadSpace: %v", err)
			}
			if got != tc.want {
				t.Errorf("CanReadSpace = %v, ожидалось %v", got, tc.want)
			}
		})
	}
}

// TestSchemaHasNoDenyRules — §6: «запретов (deny) нет: только разрешения, иначе
// фильтр в индексе усложняется». Тест проверяет это по схеме, а не по коду:
// колонка эффекта или тип действия с запретом появились бы в DDL, а не в Go.
//
// Проверяются обе стороны: в таблице прав нет ничего похожего на запрет, и
// база физически отказывается хранить действие-запрет.
func TestSchemaHasNoDenyRules(t *testing.T) {
	requireLakeDB(t)
	ctx := testContext(t)

	// 1. Ни в одной таблице прав нет колонки эффекта/запрета.
	rows, err := testDB.Query(ctx, `
		SELECT table_name, column_name
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name LIKE 'acl_%'
		  AND (column_name ILIKE '%deny%'
		    OR column_name ILIKE '%effect%'
		    OR column_name ILIKE '%forbid%'
		    OR column_name ILIKE '%block%'
		    OR column_name ILIKE '%mode%')
		ORDER BY table_name, column_name`)
	if err != nil {
		t.Fatalf("чтение information_schema: %v", err)
	}
	var forbidden []string
	err = collect(rows, func() error {
		var tableName, columnName string
		if err := rows.Scan(&tableName, &columnName); err != nil {
			return err
		}
		forbidden = append(forbidden, tableName+"."+columnName)
		return nil
	})
	if err != nil {
		t.Fatalf("обход information_schema: %v", err)
	}
	if len(forbidden) != 0 {
		t.Errorf("в таблицах прав есть колонки запрета %v: §6 допускает только разрешения", forbidden)
	}

	// 2. Действие-запрет не принимается базой: CHECK ck_acl_permissions_action
	// закрывает множество значений.
	seedACLTenants(t, ctx)
	_, err = testDB.Exec(ctx, `
		INSERT INTO acl_permissions (tenant_id, resource_group_id, container_id, action, granted_by)
		VALUES ($1, $2, $3, 'deny', 'user:anna')`,
		tenantID, aclResourceRead, aclSpacePlatform)
	if err == nil {
		t.Error("база приняла запрет в таблице прав: §6 допускает только разрешения")
	}
}

// TestSchemaForbidsDirectUserPermission — §6: «права назначаются только
// ресурсным группам». Право, выданное напрямую пользователю, обошло бы всю
// цепочку AGDLP, поэтому его некуда записать: в acl_permissions нет колонки
// пользователя.
func TestSchemaForbidsDirectUserPermission(t *testing.T) {
	requireLakeDB(t)
	ctx := testContext(t)
	seedACLTenants(t, ctx)

	_, err := testDB.Exec(ctx, `
		INSERT INTO acl_permissions (tenant_id, user_id, container_id, action, granted_by)
		VALUES ($1, $2, $3, 'read', 'user:anna')`,
		tenantID, aclUserAnna, aclSpacePlatform)
	if err == nil {
		t.Error("право выдано напрямую пользователю: §6 требует выдавать только ресурсным группам")
	}
}

// TestSchemaForbidsUserInResourceGroup — §6: «пользователи входят только в
// глобальные группы». Связать пользователя с ресурсной группой напрямую некуда:
// в acl_resource_group_members нет колонки пользователя.
func TestSchemaForbidsUserInResourceGroup(t *testing.T) {
	requireLakeDB(t)
	ctx := testContext(t)
	seedACLTenants(t, ctx)

	_, err := testDB.Exec(ctx, `
		INSERT INTO acl_resource_group_members (tenant_id, resource_group_id, user_id)
		VALUES ($1, $2, $3)`,
		tenantID, aclResourceRead, aclUserAnna)
	if err == nil {
		t.Error("пользователь попал в ресурсную группу напрямую: §6 требует только глобальные группы")
	}
}

// TestSchemaKeepsGlobalAndResourceGroupsApart — два уровня групп нельзя
// перепутать местами даже при совпадении id: внешние ключи составные и
// указывают на свои таблицы, а group_kind зафиксирован CHECK'ом.
func TestSchemaKeepsGlobalAndResourceGroupsApart(t *testing.T) {
	requireLakeDB(t)
	ctx := testContext(t)

	cases := []struct {
		name string
		sql  string
		args []any
	}{
		{
			name: "id ресурсной группы в членстве глобальных",
			sql: `INSERT INTO acl_resource_group_members (tenant_id, resource_group_id, global_group_id)
			      VALUES ($1, $2, $2)`,
			args: []any{tenantID, aclResourceRead},
		},
		{
			name: "id глобальной группы в членстве ресурсных",
			sql: `INSERT INTO acl_resource_group_members (tenant_id, resource_group_id, global_group_id)
			      VALUES ($1, $1, $1)`,
			args: []any{tenantID, aclGlobalBackend},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := testDB.Exec(ctx, tc.sql, tc.args...); err == nil {
				t.Error("уровни групп перепутаны: AGDLP требует раздельных уровней")
			}
		})
	}
}

// ── Оракул прав: проверки без БД ────────────────────────────────────────────

// TestPrincipalTermsUseSpecForm — §6 приводит форму принципала дословно:
// «user:<id>» и ресурсные группы вроде «res:space-platform:read». Эти строки
// едут в terms-фильтр индекса, и любое расхождение с формой означает тихую
// потерю доступа: фильтр не найдёт свой терм и вернёт пустое.
func TestPrincipalTermsUseSpecForm(t *testing.T) {
	got := principalTerms([]string{aclResourceKeyRead, "res:space-hr:write"})
	want := []string{"group:res:space-platform:read", "group:res:space-hr:write"}
	if !slices.Equal(got, want) {
		t.Errorf("principalTerms = %v, ожидалось %v", got, want)
	}
}

// TestSortedUniqueKeepsOrderStable — список принципалов уезжает в terms-фильтр
// и в логи. Порядок обязан быть детерминирован, иначе тест «фильтр не
// изменился» мигает без причины, а расхождение логов двух одинаковых запросов
// нельзя будет отличить от настоящего изменения прав.
func TestSortedUniqueKeepsOrderStable(t *testing.T) {
	cases := []struct {
		name   string
		values []string
		want   []string
	}{
		{name: "пусто", values: nil, want: []string{}},
		{name: "без повторов", values: []string{"group:b", "user:a"}, want: []string{"group:b", "user:a"}},
		{name: "с повторами", values: []string{"user:a", "group:b", "user:a"}, want: []string{"group:b", "user:a"}},
		{name: "в разном порядке", values: []string{"user:a", "group:b"}, want: []string{"group:b", "user:a"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sortedUnique(tc.values)
			if !slices.Equal(got, tc.want) {
				t.Errorf("sortedUnique(%v) = %v, ожидалось %v", tc.values, got, tc.want)
			}
		})
	}
}

// TestMergeSortedDedupesOverlappingSources — два источника групп (§6: claim и
// membership) пересекаются, и общий контейтор обязан попасть в список один
// раз. Дубль в terms-фильтре Elasticsearch не ломает запрос, но раздувает его.
func TestMergeSortedDedupesOverlappingSources(t *testing.T) {
	got := mergeSorted([]string{"space-a", "space-b"}, []string{"space-b", "space-c"})
	want := []string{"space-a", "space-b", "space-c"}
	if !slices.Equal(got, want) {
		t.Errorf("mergeSorted = %v, ожидалось %v", got, want)
	}
}

// TestResolverDoesNotImportAdapters — оракул прав работает через порт
// Queryer и не знает ни об Elasticsearch, ни о lake. Если это перестанет быть
// правдой, смена прав станет зависеть от переиндексации — ровно то, что §6
// запрещает. Проверка отличается от проверки слоя в internal/arch: здесь важен
// не импорт как таковой, а то, что тип доступа — Queryer.
func TestResolverTakesQueryerNotConcretePool(t *testing.T) {
	// Оракул принимает ports.Queryer: подмена на заглушку не требует базы.
	var q ports.Queryer = nil
	resolver := NewACLResolver()
	if _, err := resolver.ResolveScope(context.Background(), q, ScopeParams{
		Principal: aclPrincipal(uuid.Nil, "anna"),
	}); err == nil {
		t.Error("ожидался отказ до обращения к базе: права без тенанта")
	}
}

// TestPermissionReadIsTheOnlyActionAsked — оракул спрашивает только read.
// Право write или admin не должно молча расширять выдачу: иметь право писать не
// значит иметь право читать в поиске.
func TestPermissionReadIsTheOnlyActionAsked(t *testing.T) {
	if permissionRead != "read" {
		t.Errorf("permissionRead = %q, ожидался read", permissionRead)
	}
	for _, sql := range []string{membershipByUserSQL, membershipByGroupsSQL} {
		if !strings.Contains(sql, "p.action = $3") {
			t.Errorf("действие передаётся не параметром, а вписано в текст запроса:\n%s", sql)
		}
	}
}

// TestRequireReadSpaceUsesNotFoundError — отказ по правам обязан быть NOT_FOUND
// с тем же текстом, что и у отсутствующего документа (errDocumentNotFound в
// errors.go). Это тот же запрет на оракул существования, что и в
// access/auth/authorizer.go.
func TestRequireReadSpaceUsesNotFoundError(t *testing.T) {
	err := errDocumentNotFound()
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("errDocumentNotFound = %v, ожидался NOT_FOUND", err)
	}
}

// ── Права одного документа: серверный авторитет для стадии index ──────────────
//
// Эти тесты НЕ требуют живой базы: оракул читает одну колонку одним запросом,
// и подделка Queryer проверяет ровно то, что важно, — какое значение приходит и
// в какие термы §6 оно превращается. Интеграционная проверка на живой базе ниже
// (TestDocumentRightsOnLiveDatabase), она дополнительно проверяет, что JSONB
// отдаёт те же байты, что ждёт разбор.
//
// Подделка повторяет настоящее поведение обёртки запросов: pgx.ErrNoRows
// доходит до вызывающего неизменённым (wrapDBError), поэтому noRows/isNoRows
// в тестах ведут себя так же, как на живой базе.

type stubRightsRow struct {
	value  []byte
	noRows bool
}

func (r stubRightsRow) Scan(dest ...any) error {
	if r.noRows {
		return pgx.ErrNoRows
	}
	if len(dest) == 0 {
		return errors.New("Scan без получателей")
	}
	target, ok := dest[0].(*[]byte)
	if !ok {
		return fmt.Errorf("неподдерживаемый получатель %T", dest[0])
	}
	*target = r.value
	return nil
}

// stubRightsQueryer отдаёт одну заранее заданную строку и запоминает запрос.
type stubRightsQueryer struct {
	row  ports.Row
	sql  string
	args []any
}

func (q *stubRightsQueryer) Query(context.Context, string, ...any) (ports.Rows, error) {
	return nil, errors.New("многорядный запрос здесь не используется")
}

func (q *stubRightsQueryer) QueryRow(_ context.Context, sql string, args ...any) ports.Row {
	q.sql = sql
	q.args = args
	return q.row
}

func (q *stubRightsQueryer) Exec(context.Context, string, ...any) (ports.CommandTag, error) {
	return nil, errors.New("Exec здесь не используется")
}

// resolveDocumentRights прогоняет оракул по подделке с одним значением override.
func resolveDocumentRights(t *testing.T, override []byte, noRows bool) (domain.DocumentRights, error) {
	t.Helper()
	q := &stubRightsQueryer{row: stubRightsRow{value: override, noRows: noRows}}
	docs := newTestDocuments()
	rights, err := docs.DocumentRights(context.Background(), q, tenantID.String(), docID.String())
	if err != nil {
		return domain.DocumentRights{}, err
	}
	if !strings.Contains(q.sql, "documents") {
		t.Errorf("оракул читает не documents: %s", q.sql)
	}
	return rights, nil
}

// TestDocumentRightsOverrideReplacesContainer — ГЛАВНАЯ регрессия утечки.
//
// Документ лично закрыт на anna (acl_override = {users:[anna]}) и при этом лежит
// в контейнере space-hr. Ответ оракула обязан быть «user:anna» БЕЗ контейнера:
// фильтр индекса ставит terms(space_id) и terms(acl) через ИЛИ, и контейнер в
// ответе вернул бы документ всем в space-hr вопреки ограничению администратора.
//
// При старом поведении (конверт как единственный источник прав) этот тест падает
// на проверке контейнера: индекс уезжал с space_id от клиента.
func TestDocumentRightsOverrideReplacesContainer(t *testing.T) {
	override := []byte(`{"allowed_user_ids":["anna"],"allowed_group_ids":[]}`)

	rights, err := resolveDocumentRights(t, override, false)
	if err != nil {
		t.Fatalf("DocumentRights: %v", err)
	}
	if rights.SpaceID != "" {
		t.Errorf("переопределение вернуло контейнер %q: контейнер при override не наследуется", rights.SpaceID)
	}
	if got := rights.ExceptionTerms(); !slices.Equal(got, []string{"user:anna"}) {
		t.Errorf("права %v, ожидалось [user:anna]", got)
	}

	// Тот же запрет на уровне правила §6: сосед с правом на весь контейнер не
	// должен получать документ.
	rightsFor := domain.DocumentRights{ACL: rights.ExceptionTerms()}
	hrMember := domain.AccessScope{SpaceIDs: []string{"space-hr"}, Principals: []string{"user:petya"}}
	if hrMember.Grants(rightsFor) {
		t.Error("документ, закрытый на anna, виден соседу по контейнеру space-hr")
	}
	anna := domain.AccessScope{Principals: []string{"user:anna"}}
	if !anna.Grants(rightsFor) {
		t.Error("переопределение не открывает докуponymу, кому оно выдано")
	}
}

// TestDocumentRightsNullOverrideMeansInherit — NULL в acl_override означает
// «действует ACL базы»: серверного ограничения нет, и контейнер из конверта
// остаётся в силе. Маркер наследования обязан отличаться от закрытого документа.
func TestDocumentRightsNullOverrideMeansInherit(t *testing.T) {
	rights, err := resolveDocumentRights(t, nil, false)
	if err != nil {
		t.Fatalf("DocumentRights: %v", err)
	}
	if rights.SpaceID != ports.InheritRights {
		t.Errorf("NULL override дал %q, ожидался маркер наследования %q",
			rights.SpaceID, ports.InheritRights)
	}
	if len(rights.ExceptionTerms()) != 0 {
		t.Errorf("маркер наследования не должен нести исключений: %v", rights.ExceptionTerms())
	}
}

// TestDocumentRightsEmptyOverrideClosesDocument — ГЛАВНОЕ РАЗЛИЧИЕ NULL и пустого.
//
// Пустой override по коду accessFilterSQL (store/pg/acl.go) означает «закрыто для
// всех, кроме администратора», а NULL — «действует ACL базы». Если бы пустой
// override выглядел как наследование, документ, закрытый администратором лично,
// вернулся бы всему своему контейнеру — это утечка.
//
// Отсюда же виден и обратный край: маркер наследования не должен совпадать с
// «прав нет», иначе стадия индексации не отличит одно от другого.
func TestDocumentRightsEmptyOverrideClosesDocument(t *testing.T) {
	for name, override := range map[string]string{
		"пустой объект":         `{}`,
		"пустой JSON null":      `null`,
		"пустые списки":         `{"allowed_user_ids":[],"allowed_group_ids":[]}`,
		"только ключи без имён": `{"allowed_user_ids":null,"allowed_group_ids":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			rights, err := resolveDocumentRights(t, []byte(override), false)
			if err != nil {
				t.Fatalf("DocumentRights: %v", err)
			}
			if rights.SpaceID == ports.InheritRights {
				t.Errorf("пустой override выглядит как наследование: документ вернётся контейнеру")
			}
			if len(rights.ExceptionTerms()) != 0 {
				t.Errorf("пустой override выдал исключения %v", rights.ExceptionTerms())
			}
			// Права без контейнера и без исключений не видны НИКОМУ (§6, тест
			// TestПустойAclОтличаетсяОтСпискаВсем со стороны конверта).
			everyone := domain.AccessScope{
				SpaceIDs:   []string{"space-hr", "space-platform"},
				Principals: []string{"user:anna", "user:petya", "role:admin", "group:team-backend"},
			}
			if everyone.Grants(rights) {
				t.Error("документ, закрытый пустым override, виден пользователю с полными правами")
			}
		})
	}
}

// TestDocumentRightsMapsLegacyVocabularyToSpecTerms — перевод legacy-списков в
// словарь §6. В override лежат голые ключи («anna», «team-hr»), а индекс и фильтр
// запроса говорят на «user:<id>» и «group:<ключ>». Голые строки в индексе не
// совпали бы ни с одним термом фильтра, а «anna» вместо «user:anna» — это ровно
// тот словарь, который запрещён тестом TestЧужоеПолеНеПодменяетСловарь.
func TestDocumentRightsMapsLegacyVocabularyToSpecTerms(t *testing.T) {
	override := []byte(`{
		"allowed_user_ids":["anna","boris"],
		"allowed_group_ids":["team-hr","team-hr"]}`)

	rights, err := resolveDocumentRights(t, override, false)
	if err != nil {
		t.Fatalf("DocumentRights: %v", err)
	}
	want := []string{"group:team-hr", "user:anna", "user:boris"}
	if got := rights.ExceptionTerms(); !slices.Equal(got, want) {
		t.Errorf("перевод override = %v, ожидалось %v", got, want)
	}
	for _, term := range rights.ExceptionTerms() {
		prefixed := false
		for _, prefix := range []string{"user:", "group:", "role:"} {
			if strings.HasPrefix(term, prefix) {
				prefixed = true
				break
			}
		}
		if !prefixed {
			t.Errorf("терм %q без префикса §6: это legacy-словарь, индекс его не ищет", term)
		}
	}
}

// TestDocumentRightsIgnoresFieldsOutsideTheAuthority — accessFilterSQL смотрит
// ровно в allowed_user_ids и allowed_group_ids. Перевод в индекс чего-то ещё
// разошёлся бы с базой: поиск показал бы документ тому, кому Postgres его не
// отдаёт. В частности ролей в схеме нет вовсе (checkACLEncodable их отвергает).
func TestDocumentRightsIgnoresFieldsOutsideTheAuthority(t *testing.T) {
	override := []byte(`{
		"allowed_user_ids":["anna"],
		"allowed_role_ids":["admin"],
		"roles":["admin"],
		"allowed_group_ids":[]}`)

	rights, err := resolveDocumentRights(t, override, false)
	if err != nil {
		t.Fatalf("DocumentRights: %v", err)
	}
	if got := rights.ExceptionTerms(); !slices.Equal(got, []string{"user:anna"}) {
		t.Errorf("в термы попало лишнее %v: база эти поля в правах не учитывает", got)
	}
}

// TestDocumentRightsRejectsMalformedOverride — мусор в authority-колонке останавливает
// стадию, а не трактуется как наследование. «Не понял» и «прав нет» — это разные
// исходы: первое обязано быть видно оператору, второе тихо закрывает документ.
func TestDocumentRightsRejectsMalformedOverride(t *testing.T) {
	for name, override := range map[string]string{
		"массив":  `["anna"]`,
		"строка":  `"anna"`,
		"не JSON": `{`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := resolveDocumentRights(t, []byte(override), false); err == nil {
				t.Error("ожидался отказ на неразбираемом acl_override")
			}
		})
	}
}

// TestDocumentRightsFailsClosedOnUnknownDocument — отсутствующий документ обязан
// дать ошибку, а не пустые права: пустые права в унаследованном пути означали бы
// «действует контейнер из конверта», то есть утечку вместо отказа.
func TestDocumentRightsFailsClosedOnUnknownDocument(t *testing.T) {
	_, err := resolveDocumentRights(t, nil, true)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("неизвестный документ: %v, ожидался NOT_FOUND", err)
	}
}

// TestDocumentRightsQueryIsTenantScoped — tenant_id обязан быть в запросе: права —
// последнее, что пересекает границу организаций, и подмена «ищем по id» на
// «ищем по id и тенанту» убрала бы ровно её.
func TestDocumentRightsQueryIsTenantScoped(t *testing.T) {
	q := &stubRightsQueryer{row: stubRightsRow{value: nil}}
	docs := newTestDocuments()
	if _, err := docs.DocumentRights(context.Background(), q, tenantID.String(), docID.String()); err != nil {
		t.Fatalf("DocumentRights: %v", err)
	}

	for _, want := range []string{"tenant_id = $1", "id = $2", "deleted_at IS NULL"} {
		if !strings.Contains(q.sql, want) {
			t.Errorf("в запросе нет %q:\n%s", want, q.sql)
		}
	}
	if len(q.args) != 2 || q.args[0] != tenantID || q.args[1] != docID {
		t.Errorf("аргументы запроса %v, ожидались [%s %s]", q.args, tenantID, docID)
	}
	// Плейсхолдеры обязаны быть параметрами: склейка идентификатора в текст
	// запроса — это SQL-инъекция по tenant_id.
	if strings.Contains(q.sql, tenantID.String()) {
		t.Errorf("tenant_id вписан в текст запроса вместо плейсхолдера:\n%s", q.sql)
	}
}

// TestDocumentRightsRejectsBrokenIdentifiers — неразбираемые идентификаторы — дефект
// вызывающего, и пустой SELECT по ним означал бы «документа нет», то есть тихую
// потерю серверного мнения о правах.
func TestDocumentRightsRejectsBrokenIdentifiers(t *testing.T) {
	docs := newTestDocuments()
	for _, tc := range []struct{ tenant, document string }{
		{tenant: "не-uuid", document: docID.String()},
		{tenant: tenantID.String(), document: "не-uuid"},
	} {
		if _, err := docs.DocumentRights(context.Background(), &stubRightsQueryer{}, tc.tenant, tc.document); err == nil {
			t.Errorf("ожидался отказ на неразбираемых идентификаторах %v", tc)
		}
	}
}

// TestDocumentRightsInheritMarkerIsUnreachableAsContainer — маркер наследования
// не может стать контейнером, даже если бы кто-то его объявил.
//
// NUL внутри маркера не принимается ни в text, ни в jsonb PostgreSQL, а
// объявленный контейнер приезжает клиентом через document_versions.metadata, то
// есть через jsonb. Но «не может прийти» — довод, а не защита: проверяется худший
// случай. Объявленный маркер обязан дать документ, невидимый НИКОМУ (такого
// контейнера нет ни в одном acl_containers), а не документ, который кто-то
// неожиданно получит.
func TestDocumentRightsInheritMarkerIsUnreachableAsContainer(t *testing.T) {
	if !strings.Contains(ports.InheritRights, "\x00") {
		t.Errorf("маркер %q без NUL: такой ключ можно было бы записать в контейнеры", ports.InheritRights)
	}

	rights, err := documents.RightsFromMetadata(map[string]any{"space_id": ports.InheritRights})
	if err != nil {
		t.Fatalf("RightsFromMetadata: %v", err)
	}
	everyone := domain.AccessScope{
		SpaceIDs:   []string{"space-hr", "space-platform"},
		Principals: []string{"user:anna", "role:admin", "group:team-backend"},
	}
	if everyone.Grants(rights) {
		t.Errorf("маркер как контейнер что-то открыл: %+v", rights)
	}
}

// TestDocumentRightsOnLiveDatabase — тот же контракт на настоящей базе: JSONB
// отдаёт те же байты, NULL отличается от пустого объекта, а удалённый документ
// неразрешим.
func TestDocumentRightsOnLiveDatabase(t *testing.T) {
	requireLakeDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	docs := newTestDocuments()

	t.Run("null override означает наследование", func(t *testing.T) {
		rights, err := docs.DocumentRights(ctx, testDB, tenantID.String(), docID.String())
		if err != nil {
			t.Fatalf("DocumentRights: %v", err)
		}
		if rights.SpaceID != ports.InheritRights {
			t.Errorf("получено %q, ожидался маркер наследования", rights.SpaceID)
		}
	})

	t.Run("override возвращает принципалов без контейнера", func(t *testing.T) {
		if err := NewKBRepository().SetDocumentACL(ctx, testDB, docID.String(),
			&domain.ACL{Users: []string{"anna"}, Groups: []string{"team-hr"}}); err != nil {
			t.Fatalf("SetDocumentACL: %v", err)
		}
		rights, err := docs.DocumentRights(ctx, testDB, tenantID.String(), docID.String())
		if err != nil {
			t.Fatalf("DocumentRights: %v", err)
		}
		want := []string{"group:team-hr", "user:anna"}
		if got := rights.ExceptionTerms(); !slices.Equal(got, want) {
			t.Errorf("права %v, ожидалось %v", got, want)
		}
		if rights.SpaceID != "" {
			t.Errorf("override вернул контейнер %q", rights.SpaceID)
		}
	})

	t.Run("чужой тенант не разрешает документ", func(t *testing.T) {
		if _, err := docs.DocumentRights(ctx, testDB, otherTenant.String(), docID.String()); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("чужой тенант: %v, ожидался NOT_FOUND", err)
		}
	})

	t.Run("несуществующий документ не разрешается", func(t *testing.T) {
		missing := uuid.MustParse("44444444-4444-4444-8444-444444444444")
		if _, err := docs.DocumentRights(ctx, testDB, tenantID.String(), missing.String()); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("несуществующий документ: %v, ожидался NOT_FOUND", err)
		}
	})
}
