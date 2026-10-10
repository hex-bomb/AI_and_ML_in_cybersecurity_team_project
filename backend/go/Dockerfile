# Проверка базового образа и сборка Go-модуля backend.
#
# Образ многостадийный: сначала компиляция, затем distroless. Секреты и
# локальное окружение в образ не попадают.

FROM golang:1.27.0-bookworm@sha256:ded31c68586d2e49e760acc2e65a884b23d032e9bbbed0ae0c55abd3fcaf4452 AS build

ENV CGO_ENABLED=0 \
    GOFLAGS=-mod=mod

WORKDIR /src

# Сначала манифесты: слой с зависимостями переиспользуется, пока не меняются
# go.mod/go.sum, и правка исходников не пересобирает весь граф.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api \
 && go build -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker \
 && mkdir -p /out/var/files /out/var/models \
 && chown -R 65532:65532 /out/var

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/api /usr/local/bin/api
COPY --from=build /out/worker /usr/local/bin/worker
COPY --from=build --chown=65532:65532 /out/var/files /var/lib/kbsearch/files
COPY --from=build --chown=65532:65532 /out/var/models /var/lib/kbsearch/models

# Общий том оригиналов, JSON ParsedDocument и кэша моделей для API и worker
# (ТЗ 1.1, 5.2). Миграции вшиты в бинарь через //go:embed, каталог с SQL в
# образ не копируется.
ENV FILE_STORAGE_ROOT=/var/lib/kbsearch/files \
    EMBEDDING_MODEL_CACHE=/var/lib/kbsearch/models

USER nonroot:nonroot
WORKDIR /srv

EXPOSE 8000

# health/live не требует токена (ТЗ 10.1). В distroless нет curl и оболочки,
# поэтому проверку выполняет сам бинарь: -healthcheck делает HTTP-запрос и
# возвращает код 0, если сервис жив.
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=5 \
    CMD ["/usr/local/bin/api", "-healthcheck"]

ENTRYPOINT ["/usr/local/bin/api"]
