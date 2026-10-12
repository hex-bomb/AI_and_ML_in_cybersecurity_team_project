package domain

import (
	"slices"
	"strings"
)

// Файл — вторая половина контракта прав §6. Адаптер Elasticsearch строит фильтр
// из запроса (acl_filter.go); здесь описано, что индекс в него записывает, и
// словарь принципалов, общий для обеих сторон.
//
// Почему отдельный тип, а не существующий ACL: у ACL обратный смысл пустого
// значения — пустой ACL открывает доступ всем (AllowsAny), и словарь у него
// другой, голые user_id без префикса. Здесь пустое значение безопасное, а строки
// те же, что ищет фильтр: «user:<id>», «role:<имя>», «group:<имя>» (§6).
//
// Права едут вместе с документом на каждом шаге (§4, стадия index: «Upsert в
// Elasticsearch вместе с ACL»), и адрес документа без них бесполезен: terms по
// ненаписанному полю не совпадает ни с чем, то есть фильтр прав тихо закрывает
// поиск целиком — утечки нет, но и прав не будет ни у кого.

// Префиксы принципалов §6. Словарь задан здесь один раз, потому что обе
// стороны обязаны говорить об одном и том же на языке термов: сторона запроса
// строит terms из AccessScope.Principals, сторона индекса пишет те же строки в
// поле acl документа. «user:anna» и «anna» — это разные права, и подмена одного
// другим означала бы либо отказ в доступе, либо выдачу чужого.
const (
	principalPrefixUser  = "user:"
	principalPrefixRole  = "role:"
	principalPrefixGroup = "group:"
)

// UserPrincipal is the one term that opens a personal note: §6, «Права личных
// заметок: acl = [user:<id>] и больше никто».
func UserPrincipal(userID string) string { return principalPrefixUser + userID }

// RolePrincipal is an expanded role term (§6, AGDLP: роль приходит из IdP).
func RolePrincipal(role string) string { return principalPrefixRole + role }

// GroupPrincipal is a global-group term from the token claim or SCIM (§6).
// Ключи ресурсных групп попадают сюда же: строки вида «res:space-platform:read»
// в индексе не лежат, документ получает вместо них «group:<ключ>».
func GroupPrincipal(group string) string { return principalPrefixGroup + group }

// DocumentRights is what §6 lets one document carry into the index: the
// container it lives in and, only for point exceptions, the principals that
// reach it directly.
//
// Права висят на контейнере (roadmap §6: «Права висят на контейнере (space_id:
// канал Slack, папка, ящик), а точечный acl[] на документе нужен только для
// исключений»). SpaceID — основное поле, ACL — исключение из правила «доступ
// выдаётся на space», и у большинства документов оно пустое.
//
// Пустой ACL здесь означает «исключений нет», а не «доступ всем»: документ виден
// ровно тем, кому доступен его space. Перепутать эти два смысла — значит либо
// закрыть личные заметки их владельцу, либо открыть чужой документ всем.
type DocumentRights struct {
	// SpaceID is the container: канал Slack, папка, ящик (§6). Права на него
	// живут в Postgres, поэтому смена прав не трогает индекс.
	SpaceID string
	// ACL are point-level exceptions in principal form (§6). Пустой срез
	// безопаснее отсутствующего поля и безопаснее null: null в terms
	// исторически означал «не ограничивать», то есть полный обход прав.
	ACL []string
}

// ContainerRights is the common case: документ виден всем, кому доступен его
// контейнер, и больше никому.
func ContainerRights(spaceID string) DocumentRights {
	return DocumentRights{SpaceID: strings.TrimSpace(spaceID)}
}

// PersonalNoteRights is §6 verbatim: личная заметка принадлежит своему автору.
// Контейнер ей не назначен, поэтому доступ открывает сам acl — принципал
// «user:<id>» без контейнера и без других участников.
func PersonalNoteRights(userID string) DocumentRights {
	return DocumentRights{ACL: []string{UserPrincipal(userID)}}
}

// DocumentRightsFor builds rights from a container and explicit exceptions.
// Пустой срез исключений приводится к nil, чтобы «исключений нет» и «список не
// задан» не разъезжались в индекс двумя формами одного и того же документа.
func DocumentRightsFor(spaceID string, principals []string) DocumentRights {
	terms := normalizePrincipalTerms(principals)
	if len(terms) == 0 {
		return ContainerRights(spaceID)
	}
	return DocumentRights{SpaceID: strings.TrimSpace(spaceID), ACL: terms}
}

// ExceptionTerms returns the point-level exceptions as index terms.
//
// Срез ВСЕГДА новый и никогда не nil. nil здесь сериализовался бы в null, а
// null в terms означает «не ограничивать» — худший из возможных исходов при
// опечатке в коде сборки запроса (то же замечание, что у aclTerms и filterClauses
// на стороне запроса). Пустой массив означает «исключений нет» и оставляет
// документ видимым ровно по своему контейнеру.
//
// Чистится только внешняя обрезка пробелов: форму принципала задаёт конверт, и
// приходит она от источника уже готовой («res:space-platform:read»). Сортировка
// и склейка дублей — ради повторяемости _source, тестов и diff'ов (те же
// соображения, что в AccessScopeFor).
func (r DocumentRights) ExceptionTerms() []string {
	return normalizePrincipalTerms(r.ACL)
}

func normalizePrincipalTerms(raw []string) []string {
	out := make([]string, 0, len(raw))
	for _, value := range raw {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Grants reports whether this scope sees the document. Это зеркало клозы §6 из
// acl_filter.go: совпадение по контейнеру ИЛИ по принципалу, и никаких иных
// путей. Источник правды один, чтобы Go-фильтр и terms-фильтр не разошлись по
// смыслу.
//
// Нужен не адаптеру (адаптер фильтрует сам индекс), а сценариям и тестам: пока
// правило живёт только в теле запроса, его нечем проверить, не поднимая кластер.
func (s AccessScope) Grants(rights DocumentRights) bool {
	if space := strings.TrimSpace(rights.SpaceID); space != "" &&
		slices.Contains(s.SpaceIDs, space) {
		return true
	}
	for _, term := range rights.ExceptionTerms() {
		if slices.Contains(s.Principals, term) {
			return true
		}
	}
	return false
}
