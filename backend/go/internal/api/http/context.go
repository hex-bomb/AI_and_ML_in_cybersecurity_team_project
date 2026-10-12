package httpapi

import (
	"context"
	"log"
)

// requestIDKey — частный тип ключа контекста.
type requestIDKey struct{}

// contextWithRequestID кладёт идентификатор запроса в контекст.
func contextWithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// requestIDFrom достаёт идентификатор (вне middleware его может не быть).
func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// logPrintf пишет в stderr.
func logPrintf(format string, args ...any) {
	log.Printf(format, args...)
}
