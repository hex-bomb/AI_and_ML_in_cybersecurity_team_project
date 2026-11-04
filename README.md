# ВЕКТОР

Чат по базе знаний с ролями, гибридным поиском и записью через диалог.
Микросервисы на Go, Java и Python; локальная развёртка — docker-compose и `.env`.

## Структура

| Каталог          | Что внутри                                                                                      |
| ---------------- | ----------------------------------------------------------------------------------------------- |
| `apps/backend/`  | Сервисы: `auth`, `documents` (Java), `retrieval` (Go), `answer`, `embedding`, `ingest` (Python) |
| `apps/frontend/` | `web-app` (лендинг) и `widget` (встраиваемый виджет), npm workspaces                            |
| `contracts/`     | OpenAPI и коллекция Postman                                                                     |
| `infra/`         | Конфиги (router, Prometheus, Grafana, Postgres), скрипты и демо-корпус                          |
| `docs/`          | Документация (MkDocs Material)                                                                  |
| `scripts/`       | `test.sh`, `lint.sh`, `check-contracts.py`                                                      |

## Запуск

```sh
make up      # .env, сборка образов, контейнеры, корпус, smoke
make down    # остановить (тома сохраняются)
make clean   # остановить и удалить тома (.env сохраняется)
make test    # ruff, pytest, контракты, go build/vet
make lint    # проверки без тестов
make doctor  # проверка установки: базы, миграции, индекс, очередь, модели
make backup  # дампы трёх баз и оригиналы документов в backups/
make restore DIR=backups/<метка>  # восстановление и пересборка индекса
make reindex # пересобрать поисковый индекс из базы
```

После `make up`:

| Что                                       | Где                                                      |
| ----------------------------------------- | -------------------------------------------------------- |
| Лендинг и виджет                          | <http://localhost:8080>                                  |
| Grafana (дашборд «Vektor»)                | <http://localhost:3000>                                  |
| Prometheus                                | <http://localhost:9090>                                  |
| Swagger `auth` / `documents`              | `localhost:8081` / `localhost:8082` → `/swagger-ui.html` |
| FastAPI `answer` / `embedding` / `ingest` | `localhost:8083`–`8085` → `/docs`                        |

Подробности, включая разбор сбоев, — в
[документации](docs/index.md).

## Документация

Сайт собирается MkDocs Material из каталога `docs/`.

```sh
python -m venv .venv-docs
. .venv-docs/bin/activate
pip install -r docs/requirements-docs.txt

mkdocs serve -f docs/mkdocs.yml -a 127.0.0.1:8001
```

Страница откроется на <http://127.0.0.1:8001>. Проверка перед PR — та же сборка
со строгим режимом, она же выполняется в CI:

```sh
mkdocs build --strict -f docs/mkdocs.yml
```

## Пользователи

Вход — в шапке лендинга справа. Сессия — JWT в httpOnly-cookie.

| Логин      | Пароль                        | Роль      | Что видит              |
| ---------- | ----------------------------- | --------- | ---------------------- |
| `user`     | `user`                        | клиент    | публичные документы    |
| `employee` | `employee`                    | сотрудник | публичные и внутренние |
| `admin`    | см. `AUTH_DEV_ADMIN_PASSWORD` | сервисный | публичные и внутренние |

Аккаунты создаются только с профилем `dev` (задан в `docker-compose.yml`).
Токен `admin` живёт практически без срока (`AUTH_ADMIN_SESSION_TTL`, по умолчанию
~100 лет) — исключение для сервисных вызовов на стенде, в проде сиды выключены.
