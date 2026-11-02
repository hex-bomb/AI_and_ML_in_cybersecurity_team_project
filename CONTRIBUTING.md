# Как контрибьютить

## Структура

| Каталог         | Что внутри                                                                                             |
| --------------- | ------------------------------------------------------------------------------------------------------ |
| `apps/backend`  | Шесть сервисов: Java (`auth`, `documents`), Go (`retrieval`), Python (`answer`, `embedding`, `ingest`) |
| `apps/frontend` | `web-app` и `widget`, npm workspaces                                                                   |
| `contracts`     | OpenAPI и коллекция Postman                                                                            |
| `infra`         | Конфиги развёртывания (router, Prometheus, Grafana), скрипты                                           |
| `docs`          | Документация (MkDocs)                                                                                  |
| `scripts`       | Команды разработчика                                                                                   |

Язык выбирается по задаче, а не по вкусу: инфраструктурный I/O — Go, работа с
моделями и текстом — Python, транзакции, разбор офисных форматов и
криптография — Java.

## Локальный цикл

```bash
make up        # compose: инфраструктура + сервисы + корпус + smoke
make ps        # контейнеры
make logs SVC=answer
make test      # ruff, pytest, контракты, go build/vet/gofmt
make lint      # плюс semgrep
```

## Перед пушем

```bash
pip install pre-commit && pre-commit install
pre-commit run --all-files
```

Хуки: whitespace, ruff, gofmt, `go vet`, проверка контрактов, semgrep.
Semgrep блокирует коммит только на `critical` (PII, секреты литералом,
отключённый TLS, небезопасное выполнение).

## Правила изменений

- Код без комментариев, объясняющих очевидное. Java и Go — стандартные
  docstring-комментарии, Python — короткие numpy-докстринги.
- Меняете API — обновите `contracts/openapi/openapi.yaml`,
  `contracts/postman/` и `docs/reference/api.md`: `make test` проверяет
  согласованность.
- Новый Kafka-топик или поле события — правьте `docs/infrastructure/kafka.md`.
- Не коммитьте секреты и веса модели: они в `.gitignore`.
- Не добавляйте абстракцию без второго потребителя.

## Ветки и коммиты

`main` — стабильно, `develop` — интеграция. Коммит: `<область>: <что
изменилось>`, например `answer: не терять request_id при 503`.
