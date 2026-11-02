# Docker Compose

Единственный способ развёртывания стенда: `docker-compose.yml` в корне +
секреты в `.env`.

## Состав

| Группа        | Сервисы                                                                       |
| ------------- | ----------------------------------------------------------------------------- |
| Вход          | `router` (nginx) — **единый порт 8080**                                       |
| Данные        | `postgres`, `elasticsearch`, `kafka`                                          |
| Наблюдаемость | `prometheus` (127.0.0.1:9090), `grafana` (127.0.0.1:3000)                     |
| Приложения    | `auth`, `documents`, `retrieval`, `answer`, `embedding`, `ingest`, `frontend` |

Конфиги для маунтов лежат в `infra/`: `nginx/router.conf`,
`prometheus/prometheus.yml`, `grafana/` (datasource, provider, дашборд
«Vektor»), `postgres/init.sql`.

## Вход и порты

`router` слушает `:8080` и маршрутизирует: `jwks` и `/api/v1/auth` → `auth`,
`/api/v1/search` → `retrieval`, `/api/v1/answers` → `answer`,
`/api/v1/documents` → `documents`, остальное → `frontend`.
`/internal/*` не маршрутизируется. Правила и лимиты (8 MiB тела, 2 rps на
login, 20 rps вообще) описаны в `infra/nginx/router.conf` и проверяются
CI-скриптом `scripts/check-contracts.py` на паритет с OpenAPI.

Loopback-порты для отладки (наружу не светятся):

| Порт               | Что                                                 |
| ------------------ | --------------------------------------------------- |
| 8081 / 8082        | swagger `auth` / `documents`                        |
| 8083 / 8084 / 8085 | FastAPI `/docs` у `answer` / `embedding` / `ingest` |
| 18081              | `/internal/*` `documents` (заливка корпуса)         |
| 9090 / 3000        | Prometheus / Grafana                                |

## Секреты

`make up` вызывает `infra/scripts/ensure-env.sh`: при отсутствии `.env`
копирует `.env.example`, генерирует `AUTH_SIGNING_KEY_B64` (RSA, подпись JWT)
и `DOCUMENTS_INTERNAL_TOKEN` (общий секрет `/internal/*`). Генерация
идемпотентна: существующие значения не перезаписываются. `.env` в Git не
входит.

Другие переменные `.env` (пароль Postgres, `LLM_*`) описаны в
`.env.example` и в [../environments/local-development.md](../environments/local-development.md).

## Команды

```bash
make up     # .env → compose up --wait → корпус → smoke
make ps     # контейнеры
make logs SVC=answer
make clean  # остановить и удалить тома (.env сохраняется)
```

Разово без make: `docker compose up -d --build --wait`,
`docker compose logs -f <сервис>`, `docker compose exec postgres psql -U vektor -d vektor_docs`.

## Гарантии запуска

- healthcheck у всех сервисов; `make up` возвращается только когда все
  зелёные — порядок обеспечивается `depends_on: service_healthy` (postgres →
  `auth`/`documents`, kafka/es → `documents`/`ingest`).
- `/healthz` у `ingest` отвечает `503`, пока жив конвейер: healthcheck читает
  именно его, мёртвый конвейер уводит контейнер из реплик.
- Данные — в томах `pg-data`, `es-data`, `kafka-data`; `make clean` их
  удаляет, `make down` сохраняет.

## Связь с кодом

В конфиге Java-сервисов остаётся `spring.config.import: optional:vault://` —
без Vault он молча скипается, а `auth.signing-key-b64` и `db.password`
приходят из окружения (`AUTH_SIGNING_KEY_B64`, `SPRING_DATASOURCE_PASSWORD`
из `.env`). Полный вывод зависимостей: `docker compose config`.
