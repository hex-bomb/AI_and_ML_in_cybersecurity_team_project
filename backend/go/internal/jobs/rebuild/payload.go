package rebuild

// ТЕЛО СОБЫТИЯ raw.v1 В ВИДЕ, В КОТОРОМ ЕГО ЧИТАЕТ ПЕРЕСБОРКА.
//
// Это контракт, а не описание: пересборка индекса обязана знать поля поимённо,
// поэтому тип здесь типизированный, а не map[string]any. Карта означала бы, что
// любое поле можно забыть и тихо потерять — а потерянное поле документа здесь
// означает потерянный документ в выдаче.
//
// КОНТРАКТ raw.v1 ДЛЯ ПЕРЕСБОРКИ (все поля необязательные, кроме оговорённых
// для стадии index):
//
//	schema_version  версия схемы документа (теперь 1.0)
//	doc_id          идентификатор документа — обязан совпадать с шапкой
//	tenant_id       тенант — обязан совпадать с шапкой
//	stage           стадия — обязана совпадать с шапкой
//	content_hash    хэш содержимого; та же идемпотентность §4
//	kb_id           база знаний документа — ОБЯЗАТЕЛЬНА для стадии index
//	version_id      версия документа — ОБЯЗАТЕЛЬНА для стадии index
//	title           заголовок документа
//	space_id        контейнер прав (§6); пустая строка означает «контейнера нет»
//	acl             точечные исключения (§6) в словаре user:/role:/group:
//	blocks          разобранный документ (domain.Block) — ОБЯЗАТЕЛЬНЫ для
//	                стадии index: нарезка на чанки идёт ровно по ним
//	chunk_count     число чанков прошлой индексации (диагностика)
//
// ПОЧЕМУ ПРАВА ЧИТАЮТСЯ ИМЕННО ТАК. Пустой space_id и пустой acl — законные
// значения, а не «поле отсутствует»: документ, лично закрытый администратором
// на пустой список, обязан восстановиться закрытым. Отличать «нет поля» от
// «поле пустое» здесь не нужно, потому что тело события формирует стадия index
// и обязана писать оба поля всегда.
//
// СВЕРКА С ШАПКОЙ ОБЯЗАТЕЛЬНА. Тело, в котором лежит чужой doc_id или чужой
// тенант, привело бы к тому, что один документ собрался бы из чужого
// содержимого, и пересборка прошла бы, выдав неверный индекс. Это ровно тот
// случай, о котором предупреждает delete.go в комментарии к tombstonePayload.

import (
	"strings"

	domain "github.com/kbsearch/kb/internal/envelope"
)

// lakePayload — тело события версии raw.v1 в виде пересборки.
type lakePayload struct {
	SchemaVersion string         `json:"schema_version"`
	DocID         string         `json:"doc_id"`
	TenantID      string         `json:"tenant_id"`
	Stage         string         `json:"stage"`
	ContentHash   string         `json:"content_hash"`
	KBID          string         `json:"kb_id"`
	VersionID     string         `json:"version_id"`
	Title         string         `json:"title"`
	SpaceID       string         `json:"space_id"`
	ACL           []string       `json:"acl"`
	Blocks        []domain.Block `json:"blocks"`
	ChunkCount    int            `json:"chunk_count"`
}

// checkAgainst сверяет тело с шапкой события.
//
// Сверка строгая там, где поле объявлено, и молчаливая там, где его нет: тела
// промежуточных стадий (collect, normalize, enrich, derive) не обязаны
// повторять заголовок целиком, а тело стадии index обязано (это проверяется
// отдельно, в document). Обратная строгость была бы непоследовательной: она
// роняла бы пересборку из-за стадии, которая в индекс ничего не пишет.
func (p lakePayload) checkAgainst(event domain.Event) error {
	cases := []struct {
		field, got, want string
	}{
		{"doc_id", p.DocID, event.DocID},
		{"tenant_id", p.TenantID, event.TenantID.String()},
		{"stage", p.Stage, event.Stage.String()},
		{"content_hash", p.ContentHash, event.ContentHash},
	}
	for _, c := range cases {
		if c.got == "" {
			continue
		}
		if c.got != c.want {
			return domain.InvalidMetadata(
				"Тело события lake содержит чужое поле " + c.field + ": пересборка собрала бы документ из чужого содержимого").
				WithDetails(map[string]any{
					"stage":    event.Stage.String(),
					"event_id": event.EventID.String(),
					"field":    c.field,
					"got":      c.got,
					"want":     c.want,
				})
		}
	}
	return nil
}

// requireIdentity проверяет, что тело стадии index называет себя.
//
// Шапка события авторитетна (она лежит в колонках lake и в неё поверить можно),
// но тело обязано её подтверждать: тело, собранное из другого источника, дало
// бы документ, у которого заголовок и содержимое описывают разные вещи.
func (p lakePayload) requireIdentity(event domain.Event) error {
	missing := make([]string, 0, 2)
	if strings.TrimSpace(p.DocID) == "" {
		missing = append(missing, "doc_id")
	}
	if strings.TrimSpace(p.TenantID) == "" {
		missing = append(missing, "tenant_id")
	}
	if len(missing) == 0 {
		return nil
	}
	return domain.InvalidMetadata(
		"Тело события стадии index не называет документ: пересборка не знает, что indexed").
		WithDetails(map[string]any{
			"event_id": event.EventID.String(),
			"missing":  missing,
		})
}
