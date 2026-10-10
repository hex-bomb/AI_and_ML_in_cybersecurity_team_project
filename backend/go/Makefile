# Go-модуль backend: контракт, сборка, проверки.
#
# Команды вызываются из этого каталога. Всё, что требует сети или Docker,
# помечено явно — иначе «make test» молча поднимет половину окружения.

GO ?= go
GOLANGCI_LINT ?= $(shell $(GO) env GOPATH)/bin/golangci-lint
# Линтер собирается тулчейном 1.27: модуль требует go 1.27.0 из-за hugot,
# а бинарь, собранный более старым go, отказывается читать конфиг.
GOTOOLCHAIN ?= go1.27.0

MODULE := github.com/kbsearch/backend-go
# Контракт лежит в корне монорепозитория, а модуль — на два уровня ниже.
SPEC := ../../contracts/openapi.json
GENERATED := internal/contracts/api.gen.go

.PHONY: help
help: ## Показать доступные цели
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: contract
contract: ## Сгенерировать типы API из contracts/openapi.json
	$(GO) tool oapi-codegen --config oapi-codegen.yaml $(SPEC)

.PHONY: contract-check
contract-check: ## Убедиться, что сгенерированный код совпадает со спецификацией
	@cp $(GENERATED) $(GENERATED).bak
	@$(MAKE) --no-print-directory contract
	@if diff -q $(GENERATED) $(GENERATED).bak >/dev/null; then \
		rm -f $(GENERATED).bak; \
		echo "контракт в git совпадает со спецификацией"; \
	else \
		mv $(GENERATED).bak $(GENERATED); \
		echo "ОШИБКА: $(GENERATED) разошёлся с $(SPEC) — запусти make contract и закомминь"; \
		exit 1; \
	fi

.PHONY: build
build: ## Собрать все пакеты
	$(GO) build ./...

.PHONY: test
test: ## Прогнать тесты с гонками
	$(GO) test -race ./...

.PHONY: test-short
test-short: ## Тесты без гонок — быстрый цикл при разработке
	$(GO) test ./...

.PHONY: test-e5
test-e5: ## Тесты с настоящей ONNX-моделью (448 МБ, нужен кэш)
	$(GO) test -tags e5 -timeout 30m -v ./internal/infrastructure/embeddings/

.PHONY: cover
cover: ## Покрытие пакетами
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

.PHONY: vet
vet: ## go vet
	$(GO) vet ./...

.PHONY: lint
lint: ## Линтер со правилом слоёв clean architecture
	@command -v $(GOLANGCI_LINT) >/dev/null 2>&1 || { \
		echo "golangci-lint не найден. Установить:"; \
		echo "  GOTOOLCHAIN=$(GOTOOLCHAIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest"; \
		exit 1; \
	}
	$(GOLANGCI_LINT) run

.PHONY: lint-install
lint-install: ## Установить golangci-lint нужной версии тулчейна
	GOTOOLCHAIN=$(GOTOOLCHAIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest

.PHONY: fmt
fmt: ## Форматирование (переписывает файлы)
	$(GO) fmt ./...

.PHONY: fmt-check
fmt-check: ## Проверить форматирование, ничего не переписывая
	@unformatted=$$(gofmt -l . 2>/dev/null); \
		if [ -n "$$unformatted" ]; then \
			echo "не отформатировано:"; echo "$$unformatted"; \
			echo "исправить: make fmt"; exit 1; \
		fi

# Модель для test-e5 скачивается при первом запуске в каталог hugot по
# умолчанию. Чтобы не качать её каждый раз, укажите кэш:
#   EMBEDDING_MODEL_CACHE=/tmp/hugottest/models make test-e5

# check ничего не переписывает: `go fmt ./...` правил бы файлы соседних слоёв,
# пока над ними работают другие агенты. Цель проверяет, а не чинит.
.PHONY: check
check: fmt-check vet build contract-check test lint ## Всё, что обязано быть зелёным перед коммитом

.PHONY: tidy
tidy: ## Привести go.mod/go.sum в порядок
	$(GO) mod tidy
