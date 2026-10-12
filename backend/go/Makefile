# Go-модуль backend: контракт, сборка, проверки.
#
# Команды вызываются из этого каталога. Всё, что требует сети или Docker,
# помечено явно — иначе «make test» молча поднимет половину окружения.

GO ?= go
GOLANGCI_LINT ?= $(shell $(GO) env GOPATH)/bin/golangci-lint
# Линтер собирается тулчейном 1.27: модуль требует go 1.27.0 из-за hugot,
# а бинарь, собранный более старым go, отказывается читать конфиг.
GOTOOLCHAIN ?= go1.27.0

MODULE := github.com/kbsearch/kb
# Контракт лежит в корне монорепозитория, а модуль — на два уровня ниже.
SPEC := ../../contracts/openapi.json
GENERATED := api/api.gen.go

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
	$(GO) test -tags e5 -timeout 30m -v ./internal/store/tei/

.PHONY: cover
cover: ## Покрытие пакетами
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

# Порог покрытия — храповик: он не падает «вниз», а должен расти. Значение
# поднимается вручную вместе с тестами, а не падает с 80% одним куском.
COVER_MIN ?= 35
.PHONY: cover-gate
cover-gate: ## Проверить, что покрытие не ниже COVER_MIN (по умолчанию 35%)
	$(GO) test -coverprofile=coverage.out ./... >/dev/null
	@total=$$($(GO) tool cover -func=coverage.out | awk '/^total:/ {gsub("%","",$$3); print $$3}'); \
		awk -v t="$$total" -v m="$(COVER_MIN)" 'BEGIN { \
			if (t+0 < m+0) { printf "покрытие %.1f%% ниже порога %s%%\n", t, m; exit 1 } \
			printf "покрытие %.1f%% (порог %s%%)\n", t, m }'

.PHONY: vet
vet: ## go vet
	$(GO) vet ./...

# Лимиты замечаний сняты намеренно: по умолчанию golangci-lint показывает
# не больше трёх однотипных замечаний, из-за чего прогон выглядит коротким
# и почти зелёным, хотя правок может быть сто.
.PHONY: lint
lint: ## Линтер со правилом слоёв clean architecture
	@command -v $(GOLANGCI_LINT) >/dev/null 2>&1 || { \
		echo "golangci-lint не найден. Установить:"; \
		echo "  GOTOOLCHAIN=$(GOTOOLCHAIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest"; \
		exit 1; \
	}
	$(GOLANGCI_LINT) run --max-issues-per-linter=0 --max-same-issues=0

.PHONY: lint-install
lint-install: ## Установить golangci-lint нужной версии тулчейна
	GOTOOLCHAIN=$(GOTOOLCHAIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest

.PHONY: fmt
fmt: ## Форматирование (переписывает файлы)
	@command -v $(GOLANGCI_LINT) >/dev/null 2>&1 || { \
		echo "golangci-lint не найден. Установить:"; \
		echo "  GOTOOLCHAIN=$(GOTOOLCHAIN) go install github.com/golangci-lint/v2/cmd/golangci-lint@latest"; \
		exit 1; \
	}
	$(GOLANGCI_LINT) fmt

# Проверка форматирования идёт тем же инструментом, что и lint. `gofmt -l`
# здесь бесполезен: в конфиге линтера gofmt включён с simplify, а упрощения
# (s[:len(s)] -> s[:], T{...} -> {...}) одноимённый CLI-флаг без -s не
# делает. На таких файлах gofmt -l рапортовал «чисто», а lint падал, и
# расхождение выглядело как неисправность линтера.
.PHONY: fmt-check
fmt-check: ## Проверить форматирование, ничего не переписывая
	@command -v $(GOLANGCI_LINT) >/dev/null 2>&1 || { \
		echo "golangci-lint не найден, пропускаем проверку форматирования"; \
		exit 0; \
	}
	@$(GOLANGCI_LINT) fmt --diff . > /tmp/kb-fmt.diff 2>&1 || { \
		echo "не отформатировано:"; cat /tmp/kb-fmt.diff; \
		echo "исправить: make fmt"; exit 1; \
	}

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
