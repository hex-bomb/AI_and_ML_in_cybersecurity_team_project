FROM golang:1.23-alpine AS build
WORKDIR /src
# Сначала только манифесты — слой с зависимостями кэшируется, пока не меняется go.mod.
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /retrieval ./cmd/retrieval

FROM alpine:3.20
RUN adduser -D -u 10001 app
COPY --from=build /retrieval /retrieval
USER 10001
EXPOSE 8080
ENTRYPOINT ["/retrieval"]
