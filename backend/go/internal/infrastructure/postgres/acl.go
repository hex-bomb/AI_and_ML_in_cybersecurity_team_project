package postgres

import (
	"encoding/json"
	"fmt"

	"github.com/kbsearch/backend-go/internal/domain"
)

// accessFilterSQL — единственное место правила доступа (ТЗ 2.1): группы через
// EXISTS + ANY; ACL документа только сужает ACL базы; acl_override — объект,
// сравнивается через jsonb_build_object.
const accessFilterSQL = `(
	(kb.allowed_user_ids @> $%[1]d::jsonb
	  OR EXISTS (SELECT 1 FROM jsonb_array_elements_text(kb.allowed_group_ids) g
	             WHERE g = ANY($%[2]d::text[])))
	AND (
		d.acl_override IS NULL
		OR d.acl_override @> jsonb_build_object('allowed_user_ids', $%[1]d::jsonb)
		OR EXISTS (SELECT 1 FROM jsonb_array_elements_text(d.acl_override->'allowed_group_ids') g
		             WHERE g = ANY($%[2]d::text[]))
	)
)`

// adminFilterSQL — администратор видит всё содержимое своего tenant.
const adminFilterSQL = "TRUE"

// accessFilter возвращает условие доступа и два аргумента (порядок связан
// с номерами плейсхолдеров; firstArg — номер первого).
func accessFilter(p domain.Principal, firstArg int) (string, []any) {
	if p.HasRole("admin") {
		return adminFilterSQL, nil
	}
	userArg, groupsArg := firstArg, firstArg+1
	condition := fmt.Sprintf(accessFilterSQL, userArg, groupsArg)
	return condition, []any{
		[]string{p.UserID},
		nonNil(p.Groups),
	}
}

// nonNil заменяет nil-срез на пустой: pgx кодирует nil []string как NULL.
func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// aclJSON кодирует ACL двумя массивами под колонки базы знаний.
func aclJSON(a domain.ACL) (users, groups []byte, err error) {
	users, err = json.Marshal(nonNil(a.Users))
	if err != nil {
		return nil, nil, err
	}
	groups, err = json.Marshal(nonNil(a.Groups))
	if err != nil {
		return nil, nil, err
	}
	return users, groups, nil
}

// aclOverrideJSON кодирует собственный ACL документа в jsonb-объект.
func aclOverrideJSON(a domain.ACL) ([]byte, error) {
	return json.Marshal(struct {
		AllowedUserIDs  []string `json:"allowed_user_ids"`
		AllowedGroupIDs []string `json:"allowed_group_ids"`
	}{
		AllowedUserIDs:  nonNil(a.Users),
		AllowedGroupIDs: nonNil(a.Groups),
	})
}

// checkACLEncodable отказывает в ACL с ролями (в схеме нет allowed_role_ids).
func checkACLEncodable(a domain.ACL) error {
	if len(a.Roles) > 0 {
		return domain.Conflict(
			"ACL с ролями не поддерживается: в схеме нет поля allowed_role_ids " +
				"(migration 0001). Запишите роли как группы или уберите их из ACL.",
		)
	}
	return nil
}
