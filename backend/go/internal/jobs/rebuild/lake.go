package rebuild

// ПОСТРАНИЧНОЕ ЧТЕНИЕ LAKE.
//
// Пересборка читает ленту ЦЕЛИКОМ, а не «по мере надобности»: свёртка
// (fold.go) определяет последнее событие каждого документа, а события одного
// документа разнесены по всей ленте. Постраничное чтение с накоплением в памяти
// даёт тот же результат и позволяет повторить прерванную пересборку с нуля —
// что §9 и требует от учебной тревоги.
//
// КУРСОР. Формат курсора принадлежит АДАПТЕРУ (lake.go, «метка|event_id»), и
// порт EventStore его не выдаёт. Здесь он собирается из последнего прочитанного
// события тем же правилом — и это единственное место в дереве, где курсор
// строится вручную. Дублировать формат в пересборке нельзя было бы: смена
// формата адаптером тихо испортила бы чтение, и пересборка объявила бы пустой
// lake «пустым lake».
//
// КОНТРОЛЬ ЗАСТРЯВАНИЯ. Цикл останавливается по пустой странице, и это
// достаточное условие: адаптер не может вернуть непустую страницу за пределами
// ленты. Добавлена вторая страховка — курсор, который не сдвинулся. Иначе
// адаптер, вернувший повторяющуюся страницу, крутил бы пересборку до конца
// времени и «пересобрал» бы индекс из первой страницы, отчитавшись об успехе.

import (
	"context"
	"time"

	domain "github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
)

// cursorSeparator — разделитель в курсоре lake.
//
// Тот же, что в адаптере: формат курсора не является частью порта, и обе стороны
// обязаны собирать его одинаково. Значение совпадает с lakeCursorSeparator в
// internal/store/pg/lake.go и меняется вместе с ним.
const cursorSeparator = "|"

// readLake читает всю ленту тенанта в порядке, который обещает порт.
func readLake(ctx context.Context, store ports.EventStore, scope Scope) ([]domain.Event, error) {
	limit := scope.limit()
	cursor := ""
	out := make([]domain.Event, 0, limit)

	for {
		if err := ctx.Err(); err != nil {
			return nil, interrupted(err, len(out), 0)
		}
		page, err := store.Read(ctx, domain.EventQuery{
			TenantID: scope.TenantID,
			Limit:    limit,
			Cursor:   cursor,
		})
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			return out, nil
		}

		out = append(out, page...)
		next := encodeCursor(page[len(page)-1])
		if next == cursor {
			return nil, domain.Internal("Чтение lake не продвигается").
				WithDetails(map[string]any{
					"cursor":    cursor,
					"page_size": len(page),
				})
		}
		cursor = next
	}
}

// encodeCursor собирает курсор lake из последнего прочитанного события.
func encodeCursor(event domain.Event) string {
	return event.OccurredAt.UTC().Format(time.RFC3339Nano) +
		cursorSeparator + event.EventID.String()
}
