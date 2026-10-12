package postgres

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/google/uuid"

	domain "github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
)

// ACLResolver — оракул прав по §6. Отвечает на один вопрос: какие контейнеры
// тенанта данный пользователь может читать, и какие принципалы его.
//
// Почему это запрос к базе, а не поле в индексе. §6 требует, чтобы смена прав
// на space применялась мгновенно и «не требовала переиндексации документов,
// только пересчёта списка доступных space у пользователя». Если бы права были
// размазаны по документам индекса, смена прав означала бы перезапись каждого
// документа контейнера — то есть ровно то, чего §6 запрещает. Права висят на
// контейнере, контейнеры лежат в acl_permissions, и список доступного — это
// результат одного SELECT. Документы переиндексируются при смене их текста,
// а не при смене прав на их контейнер.
//
// Оракул — чистое чтение базы, а не кэш и не вычисление на стороне приложения.
// Значений он не хранит: всё состояние в таблицах §6, и перезапуск процесса не
// меняет ничьих прав.
//
// Что этот файл НЕ делает. Он не отдаёт список разрешённых document_id.
// §6 прямо запрещает post-filter по id: разрешённый чанк уже оказался бы в
// памяти и в контексте LLM. Наружу уходят только термы фильтра — space_id и
// принципалы, — которые адаптер Elasticsearch превращает в terms-фильтр внутри
// запроса. Если появится код, спрашивающий здесь «какие документы мне видны»,
// он нарушит правило §6, и отсутствие такого метода в интерфейсе — лучшая
// защита от этого.
type ACLResolver struct{}

// Цепочка AGDLP, общая для обоих входов: пользователь состоит в глобальных
// группах, глобальные группы — в ресурсных, права назначены ресурсным группам
// на контейнеры. Каждое соединение — один шаг AGDLP, и ни одного шага в
// обратную сторону в схеме не существует (§6: пользователи состоят только в
// глобальных группах, глобальные — только в ресурсных).
//
// Два входа в цепочку, а не один, и это не избыточность. §6 берёт группы на
// MVP из claim groups при входе, а со SCIM (фаза 2) они приходят в
// acl_global_group_members, и увольнение должно отзывать доступ за минуты, а
// не к следующему входу. Пока данных нет ни в одной из двух таблиц, принципалы
// разворачиваются по claim — то есть MVP работает, ничего не засевая. Когда
// появится SCIM, membership-таблица начнёт давать вторую половину цепочки, и
// ни этот оракул, ни вызывающий код не меняются: это ровно то свойство,
// ради которого §8 требует прятать усложнение за интерфейс.
//
// DISTINCT обязателен в обоих запросах: у пользователя может быть несколько
// глобальных групп в одной ресурсной, и без него один контейнер вернулся бы
// столько раз, сколько путей до него ведёт. Дубли в terms-фильтре
// Elasticsearch не ломают запрос, но раздувают его и делают логи нечитаемыми.

// NewACLResolver создаёт оракул прав.
func NewACLResolver() *ACLResolver { return &ACLResolver{} }

// ScopeParams — вход оракула прав.
type ScopeParams struct {
	// Principal — субъект запроса: tenant, user и его глобальные группы из
	// claim (§6, «токены не содержат список групп, принципалы разворачиваются
	// на сервере» — здесь они уже развёрнуты аутентификацией в Principal).
	Principal domain.Principal
	// KBID едет в AccessScope для выбора индекса адаптером. Права от него не
	// зависят: контейнеры принадлежат тенанту, а не базе знаний (§6).
	KBID string
}

// ResolveScope возвращает область доступа, которую адаптер индексу превратит
// в terms-фильтр.
//
// Возвращаются только разрешения — списком. Запрета в модели нет (§6: «deny
// нет, только разрешения»), и потому здесь не может быть и «список запрещённых
// »: пустой результат означает «не разрешено ничего», а не «разрешено всё».
// Тот, кто не имеет ни одного контейнера, получает пустой Scope, и
// AccessScope.AllowsNothing() велит вернуть пустое, не обращаясь к индексу.
func (r *ACLResolver) ResolveScope(ctx context.Context, q ports.Queryer, in ScopeParams) (domain.AccessScope, error) {
	if in.Principal.TenantID == uuid.Nil {
		// Тенант обязателен всегда: пустой tenant в WHERE отбрасывает всё, но
		// ошибка честнее пустого результата — она говорит вызывающему, что
		// пришёл запрос без субъекта, а не что у субъекта нет прав.
		return domain.AccessScope{}, domain.InvalidMetadata("tenant_id обязателен: права без тенанта — это права всех")
	}
	if in.Principal.UserID == "" {
		// Без user_key нельзя ни найти membership, ни построить «user:<id>»,
		// а именно этот терм открывает личные заметки (§6: «права личных
		// заметок acl = [user:<id>] и больше никто»).
		return domain.AccessScope{}, domain.InvalidMetadata("user_id обязателен: принципал «user:<id>» не из чего собрать")
	}

	// Терм пользователя. Он же — единственный способ увидеть личную заметку,
	// и он же входит в Principals ответа при любых правах на контейнеры.
	principals := []string{"user:" + in.Principal.UserID}

	// Контейнеры и ресурсные группы из membership-таблиц (SCIM-путь).
	spaces, resourceKeys, err := r.readableFromMembership(ctx, q, in.Principal)
	if err != nil {
		return domain.AccessScope{}, err
	}
	principals = append(principals, principalTerms(resourceKeys)...)

	// Контейнеры и ресурсные группы из claim групп (MVP-путь, §6).
	claimSpaces, claimResources, err := r.readableFromClaim(ctx, q, in.Principal)
	if err != nil {
		return domain.AccessScope{}, err
	}
	spaces = mergeSorted(spaces, claimSpaces)
	principals = append(principals, principalTerms(claimResources)...)

	return domain.AccessScope{
		TenantID:   in.Principal.TenantID.String(),
		KBID:       in.KBID,
		SpaceIDs:   spaces,
		Principals: sortedUnique(principals),
	}, nil
}

// readableFromMembership — цепочка AGDLP от membership-таблицы (§6, SCIM).
func (r *ACLResolver) readableFromMembership(
	ctx context.Context, q ports.Queryer, p domain.Principal,
) (spaces, resourceKeys []string, err error) {
	return r.readable(ctx, q, membershipByUserSQL, p.TenantID, p.UserID, permissionRead)
}

// readableFromClaim — цепочка AGDLP от claim групп (§6, MVP).
func (r *ACLResolver) readableFromClaim(
	ctx context.Context, q ports.Queryer, p domain.Principal,
) (spaces, resourceKeys []string, err error) {
	// Пустой список групп — не запрос с пустым IN, а отсутствие входа в цепочку:
	// ANY пустого массива не совпадает ни с одной строкой, и запрос
	// возвращает ноль строк, то есть «ничего не разрешено». Это тот же исход,
	// что и у пользователя без прав, и он корректен.
	groups := p.Groups
	if groups == nil {
		groups = []string{}
	}
	return r.readable(ctx, q, membershipByGroupsSQL, p.TenantID, groups, permissionRead)
}

// membershipByUserSQL — точка входа в цепочку по строке membership.
//
// Пользователь ищется по user_key, а не по id: внешний код аутентификации знает
// «anna», и переводить его в uuid аккаунта до похода в базу означало бы
// лишний запрос на каждый чих. user_key уникален внутри тенанта.
const membershipByUserSQL = `
	SELECT DISTINCT c.container_key, rg.resource_key
	FROM acl_users u
	JOIN acl_global_group_members m
	  ON m.tenant_id = u.tenant_id AND m.user_id = u.id
	JOIN acl_resource_group_members rm
	  ON rm.tenant_id = m.tenant_id AND rm.global_group_id = m.group_id
	JOIN acl_resource_groups rg
	  ON rg.tenant_id = rm.tenant_id AND rg.id = rm.resource_group_id
	JOIN acl_permissions p
	  ON p.tenant_id = rm.tenant_id AND p.resource_group_id = rg.id
	JOIN acl_containers c
	  ON c.tenant_id = p.tenant_id AND c.id = p.container_id
	WHERE u.tenant_id = $1
	  AND u.user_key = $2
	  AND p.action = $3`

// membershipByGroupsSQL — тот же обход, но начиная с групп claim.
//
// Claim приходит в токене и в базе не лежит, поэтому группы пользователя
// разворачиваются прямо в запросе: для каждой группы claim ищется строка
// acl_global_groups, и дальше цепочка идёт обычным порядком. Пользователя при
// этом может не быть в базе вовсе — JIT создаёт его при первом входе (§6), и
// требование строки в acl_users отрезало бы работающих пользователей.
const membershipByGroupsSQL = `
	SELECT DISTINCT c.container_key, rg.resource_key
	FROM acl_global_groups gg
	JOIN acl_resource_group_members rm
	  ON rm.tenant_id = gg.tenant_id AND rm.global_group_id = gg.id
	JOIN acl_resource_groups rg
	  ON rg.tenant_id = rm.tenant_id AND rg.id = rm.resource_group_id
	JOIN acl_permissions p
	  ON p.tenant_id = rm.tenant_id AND p.resource_group_id = rg.id
	JOIN acl_containers c
	  ON c.tenant_id = p.tenant_id AND c.id = p.container_id
	WHERE gg.tenant_id = $1
	  AND gg.group_key = ANY($2::text[])
	  AND p.action = $3`

// readable выполняет цепочку и разбирает её в два списка.
//
// Аргументы принимаются готовыми, а не собираются общим списком на все
// запросы: у каждого запроса своя раскладка плейсхолдеров, и общий список
// означал бы, что изменение одного SQL молча ломает другой — ровно тот класс
// ошибок, который компилятор не ловит, а тест ловит на живых данных.
//
// action передаётся параметром, а не вписывается в текст: константа 'read' в
// строке запроса выглядит безобидно, пока однажды в это место не подставят
// что-то другое.
func (r *ACLResolver) readable(
	ctx context.Context, q ports.Queryer, sqlTemplate string, args ...any,
) (spaces, resourceKeys []string, err error) {
	rows, err := q.Query(ctx, sqlTemplate, args...)
	if err != nil {
		return nil, nil, err
	}

	spaces = []string{}
	resourceKeys = []string{}
	err = collect(rows, func() error {
		var containerKey, resourceKey string
		if err := rows.Scan(&containerKey, &resourceKey); err != nil {
			return err
		}
		spaces = append(spaces, containerKey)
		resourceKeys = append(resourceKeys, resourceKey)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return spaces, resourceKeys, nil
}

// permissionRead — единственное действие, которое спрашивает поиск.
//
// §6 упоминает read/write/admin, но оракул отвечает на вопрос «что видно в
// поиске», и он — read. Выдача write или admin искала бы то же самое и
// вернула бы права, которых поиск не проверяет: иметь право писать не значит
// иметь право читать в выдаче.
const permissionRead = "read"

// CanReadSpace сообщает, может ли пользователь читать контейнер.
//
// Это защита в глубину из §6: «перед тем как отдать top-N в UI или в контекст
// LLM через MCP, повторно проверяем can_read(user, doc_ids) по Postgres».
// Индекс может отставать по правам (пересчёт списка занимает минуты, §6), и
// без этой проверки устаревший индекс отдал бы документ контейнера, права на
// который уже отозвали.
//
// Отдельного запроса «существует ли контейнер» здесь нет и быть не должно:
// ответ на вопрос о несуществующем контейнере и о запрещённом обязан быть
// одинаковым, иначе перебор ключей space_id выдаёт перебор каталога (тот же
// запрет на оракул существования, что в access/auth/authorizer.go: «NOT_FOUND
// so probing cannot learn what exists»).
func (r *ACLResolver) CanReadSpace(
	ctx context.Context, q ports.Queryer, p domain.Principal, spaceID string,
) (bool, error) {
	scope, err := r.ResolveScope(ctx, q, ScopeParams{Principal: p})
	if err != nil {
		return false, err
	}
	return containsTerm(scope.SpaceIDs, spaceID), nil
}

// RequireReadSpace проверяет право на контейнер, не выдавая разницы между
// «запрещено» и «не существует».
//
// Возвращается NOT_FOUND с тем же текстом, что и у отсутствующего контейнера,
// — по правилу access/auth/authorizer.go: перебор не должен уметь отличить
// запрещённый контейнер от несуществующего. Вызывающий получает ровно тот же
// отказ в обоих случаях и не может по нему определить, что где-то в tenant'е
// есть.
func (r *ACLResolver) RequireReadSpace(
	ctx context.Context, q ports.Queryer, p domain.Principal, spaceID string,
) error {
	allowed, err := r.CanReadSpace(ctx, q, p, spaceID)
	if err != nil {
		return err
	}
	if !allowed {
		return errDocumentNotFound()
	}
	return nil
}

// principalTerms превращает ключи ресурсных групп в термы индекса §6.
//
// Форма «group:<id>» — дословно из спецификации: §6 приводит её в примере
// «acl = [user:<id>]», а фильтр индекса строится по тем же строкам, что
// лежат в documents.acl_override. Ресурсная группа попадает в этот список не
// напрямую — «res:space-platform:read» в индексе не лежит, — а через префикс
// group:, потому что документ, к которому применено право ресурсной группы,
// получает именно эту строку.
func principalTerms(resourceKeys []string) []string {
	terms := make([]string, 0, len(resourceKeys))
	for _, key := range resourceKeys {
		terms = append(terms, "group:"+key)
	}
	return terms
}

// concat склеивает два среза в новый. Новый обязателен: append к срезу, который
// ещё где-то читается, переписывает его буфер на месте, а права строятся из
// значений, которые ещё пригодятся.
func concat(first, second []string) []string {
	out := make([]string, 0, len(first)+len(second))
	out = append(out, first...)
	return append(out, second...)
}

// sortedUnique сортирует и убирает повторы.
//
// Сортировка обязательна, и не для красоты: список принципалов уезжает в
// terms-фильтр Elasticsearch и в логи запроса, а AccessScopeFor в конверте
// делает то же самое. Если два вызова вернут одинаковые права в разном
// порядке, тест на «фильтр не изменился» начнёт мигать без причины.
func sortedUnique(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	sorted := make([]string, len(values))
	copy(sorted, values)
	sortStrings(sorted)

	unique := sorted[:0]
	for i, value := range sorted {
		if i > 0 && sorted[i-1] == value {
			continue
		}
		unique = append(unique, value)
	}
	return unique
}

// mergeSorted сливает два отсортированных уникальных списка.
//
// Списки приходят из двух источников (membership и claim) и могут
// пересекаться: пользователь, у которого есть и строка в membership, и та же
// группа в claim, иначе получил бы один контейнер дважды.
func mergeSorted(first, second []string) []string {
	merged := make([]string, 0, len(first)+len(second))
	merged = append(merged, first...)
	merged = append(merged, second...)
	return sortedUnique(merged)
}

// containsTerm сообщает о наличии значения в списке.
func containsTerm(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// ── Права одного документа: серверный авторитет для стадии index ──────────────

// DocumentRightsSQL — единственный источник серверного мнения о правах
// документа на пути в индекс.
//
// Тенант обязателен и стоит первым: без него запрос вернул бы acl_override
// документа с тем же id из чужого тенанта, а права — это ровно то, что не
// должно пересекать границу организаций. deleted_at IS NULL по той же
// причине, что и в остальном репозитории: удалённый документ не должен
// воскреснуть в индексе как действующий.
const documentRightsSQL = `
	SELECT d.acl_override
	FROM documents d
	WHERE d.tenant_id = $1 AND d.id = $2 AND d.deleted_at IS NULL`

// aclOverridePayload — разбор documents.acl_override.
//
// Ключи названы теми же именами, что пишет aclOverrideJSON, и только ими:
// accessFilterSQL смотрит ровно в allowed_user_ids и allowed_group_ids, и
// любое другое поле этой строки база в правах не учитывает. Переводить в
// термы индекса то, чего accessFilterSQL не читает, — значит разойтись с ним
// и выдать документ в поиске тому, кому Postgres его не отдаст.
type aclOverridePayload struct {
	AllowedUserIDs  []string `json:"allowed_user_ids"`
	AllowedGroupIDs []string `json:"allowed_group_ids"`
}

// DocumentRights возвращает серверное мнение о правах документа: переопределение,
// записанное администратором, переведённое в словарь индекса §6.
//
// Три состояния, и различать их обязательно:
//
//   - acl_override IS NULL — наследование. Возвращается маркер ports.InheritRights,
//     и вызывающий оставляет объявленный конвертом контейнер (§6: права висят
//     на контейнере).
//   - override с принципалами — документ ограничен лично. Контейнера в ответе
//     нет: фильтр индекса ставит terms(space_id) и terms(acl) через ИЛИ, и
//     унаследованный контейнер открыл бы документ всему контейнеру вопреки
//     ограничению администратора.
//   - override с пустыми списками — документ закрыт всем. Это НЕ наследование:
//     по accessFilterSQL пустой объект пропускает только администратора, а NULL
//     отдаёт документ всем, кому открыт контейнер.
//
// Контейнера оракул не возвращает намеренно: в documents нет колонки с
// контейнером (миграция 0001), связь документа с контейнером живёт только в
// конверте. Оракул отвечает на вопрос «ограничил ли администратор документ» и
// не притворяется вторым источником контейнера.
//
// Ошибка — всегда отказ: неизвестный документ, чужой тенант и удалённый
// документ неразличимы наружу (errDocumentNotFound), потому что стадия индексации
// обязана остановиться, а не уехать в индекс с правами по умолчанию.
func (r *DocumentRepository) DocumentRights(
	ctx context.Context, q ports.Queryer, tenantID, documentID string,
) (domain.DocumentRights, error) {
	tenant, err := parseUUID("tenant_id", tenantID)
	if err != nil {
		return domain.DocumentRights{}, err
	}
	doc, err := parseUUID("document_id", documentID)
	if err != nil {
		return domain.DocumentRights{}, err
	}

	var override []byte
	if err := q.QueryRow(ctx, documentRightsSQL, tenant, doc).Scan(&override); err != nil {
		if isNoRows(err) {
			return domain.DocumentRights{}, errDocumentNotFound()
		}
		return domain.DocumentRights{}, err
	}
	if override == nil {
		return domain.DocumentRights{SpaceID: ports.InheritRights}, nil
	}
	return rightsFromOverride(override)
}

// rightsFromOverride переводит override в права индекса.
//
// Словари разные, и это главная опасность перевода. override хранит legacy
// формы — голые user_key и ключи глобальных групп, — а индекс и фильтр запроса
// говорят на «user:<id>» и «group:<ключ>» (§6, internal/envelope/acl.go).
// Подстановка голой строки означала бы либо отказ в доступе (никто не ищет
// терм «anna»), либо, что хуже, выдачу чужого документа всем, у кого в scope
// окажется такая строка. Перевод делается конструкторами конверта, чтобы
// префикс жил в одном месте.
func rightsFromOverride(override []byte) (domain.DocumentRights, error) {
	var payload aclOverridePayload
	if err := json.Unmarshal(override, &payload); err != nil {
		// Мусор в authority-колонке — отказ, а не «прав нет»: неизвестное
		// значение нельзя трактовать как наследование, это была бы выдача
		// документа по контейнеру. Отказ закрытый и виден оператору.
		// Причина уходит в Cause (логи), а не в Details: детали ошибки
		// возвращаются клиенту как есть, и эхо разбираемого значения там лишнее.
		return domain.DocumentRights{}, domain.InvalidMetadata(
			"acl_override документа не разбирается как объект прав").WithCause(err)
	}

	users := make([]string, 0, len(payload.AllowedUserIDs))
	for _, id := range payload.AllowedUserIDs {
		if id = strings.TrimSpace(id); id != "" {
			users = append(users, domain.UserPrincipal(id))
		}
	}
	// Группы идут тем же principalTerms, что и членства в контейнерах: ключ
	// ресурсной группы и ключ claim-группы — одна и та же строка «group:<ключ>».
	keys := make([]string, 0, len(payload.AllowedGroupIDs))
	for _, key := range payload.AllowedGroupIDs {
		if key = strings.TrimSpace(key); key != "" {
			keys = append(keys, key)
		}
	}

	terms := sortedUnique(concat(users, principalTerms(keys)))
	if len(terms) == 0 {
		// Пустой override — это «закрыто всем», а не наследование: см.
		// DocumentRights. Маркер наследования здесь не возвращается никогда.
		return domain.DocumentRights{}, nil
	}
	return domain.DocumentRights{ACL: terms}, nil
}
