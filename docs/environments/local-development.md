# Локальная развёртка

**Кому:** инженер, который поднимает стенд локально.
**Что получится:** docker-compose-стенд с шестью сервисами, Postgres, Elasticsearch,
Kafka, Prometheus и Grafana; 20 документов корпуса в индексе.

## Что нужно

- Docker с Docker Compose v2.
- Веса модели в `apps/backend/embedding/models` (`model.onnx`, `tokenizer.json`).
  Каталог не в git; без него сервис `embedding` поднимется в режиме `degraded`,
  поиск останется лексическим.
- Для генерации ответов — Authorization key GigaChat или любой
  OpenAI-совместимый эндпоинт (см. [Генерация ответов](#генерация-ответов-llm)).

## Запуск

```bash
make up
```

`make up` идемпотентен и делает по порядку:

1. `infra/scripts/ensure-env.sh`: создаёт `.env` из `.env.example` (первый раз)
   и генерирует секреты — `AUTH_SIGNING_KEY_B64` (подпись JWT) и
   `DOCUMENTS_INTERNAL_TOKEN` (`/internal/*`); существующие значения не трогает;
2. `docker compose up -d --build --wait` — собирает и запускает всё, ждёт
   healthchecks (порядок обеспечивается `depends_on: service_healthy`);
3. заливает корпус из `infra/scripts/data/corpus.json` через
   `POST /internal/documents` на `127.0.0.1:18081`;
4. прогоняет `infra/scripts/smoke.sh` — вход, поиск, ответ, JWKS.

Остановка и осмотр:

```bash
make down              # остановить, тома сохраняются
make clean             # остановить и удалить тома (.env сохраняется)
make ps                # контейнеры
make logs SVC=answer   # логи сервиса
make smoke             # повторить smoke
```

## Проверка, что всё живо

`make ps` — все контейнеры `healthy`. У `ingest` healthcheck читает `/healthz`,
который отвечает `503`, пока мёртв конвейер: без него контейнер выглядел бы
живым, а индексация — мёртвой.

Дальше — публичный контур (единственный вход — router на `:8080`,
Host-заголовок не обязателен):

```bash
curl -sS http://localhost:8080/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"employee","password":"employee"}' -c /tmp/j

curl -sS -b /tmp/j http://localhost:8080/api/v1/search \
  -H 'Content-Type: application/json' \
  -d '{"query":"гарантия","top_k":3}'
```

`make smoke` делает то же самое и печатает коды ответов: `login`, `search`,
`answer`, `jwks`.

Swagger и OpenAPI-схемы каждого сервиса — по loopback-портам, см.
[../reference/api.md](../reference/api.md).

## Генерация ответов (LLM)

По умолчанию LLM не настроена: `/answers` отвечает `503 LLM_UNAVAILABLE`,
`/search` и `/documents` работают полностью. `answer` разговаривает по
OpenAI-протоколу (`POST <base>/chat/completions`), поэтому подойдёт любой
совместимый эндпоинт.

Всё настраивается переменными в `.env`; контейнеры читают их на старте, поэтому
после правки `.env` пересоздайте сервис:

```bash
docker compose up -d answer
```

| Переменная       | Что делает                               | Пример                     |
| ---------------- | ---------------------------------------- | -------------------------- |
| `LLM_BASE_URL`   | адрес эндпоинта без `/chat/completions`  | `https://api.giga.chat/v1` |
| `LLM_MODEL`      | имя модели                               | `GigaChat-2`, `qwen2.5:3b` |
| `LLM_API_KEY`    | значение `Authorization: Bearer`         | ключ или `any`             |
| `LLM_PROVIDER`   | `gigachat` — нативный режим обмена ключа | `gigachat`                 |
| `GIGACHAT_SCOPE` | scope OAuth (только для `gigachat`)      | `GIGACHAT_API_PERS`        |

### GigaChat (нативно)

Платформа работает с GigaChat напрямую: `answer` сам обменивает
Authorization key на `access_token` (живёт 30 минут) и обновляет его до
истечения — прокси не нужен.

**`LLM_API_KEY` — это Authorization key** с
[developers.sber.ru](https://developers.sber.ru/) (base64 от
`ClientID:ClientSecret`). Не путайте: это не access_token — его answer
получает сам; если вместо ключа передать готовый JWT-токен, он тоже будет
принят как есть.

```bash
# .env
LLM_PROVIDER=gigachat
LLM_API_KEY=<authorization key>
```

Адрес (`https://api.giga.chat/v1`) и модель (`GigaChat-2`) подставляются
автоматически.

Детали контракта (проверено живьём по спеке GigaChat API):

- фактический список моделей — `GET /v1/models`; «голого» `GigaChat` на
  `api.giga.chat` нет, доступны, в частности, `GigaChat-2`,
  `GigaChat-2-Pro`, `GigaChat-3-Lightning`, `GigaChat-3-Ultra`;
- ответ OpenAI-совместим (`choices[0].message.content`), поэтому парсинг
  `answer` работает без изменений;
- scope по умолчанию `GIGACHAT_API_PERS`, меняется через `GIGACHAT_SCOPE`
  (`GIGACHAT_API_B2B` / `GIGACHAT_API_CORP`);
- при истечении токена `answer` повторяет запрос один раз с новым токеном;
  неудачный обмен — `503 LLM_UNAVAILABLE` с причиной в логе.

Проверка: `make smoke`, строка `answer` должна стать `200` вместо `503`.

### Локальная модель (ollama)

```bash
# .env
LLM_BASE_URL=http://host.docker.internal:11434/v1
LLM_MODEL=qwen2.5:3b
LLM_API_KEY=any
```

На Linux вместо `host.docker.internal` используйте IP хоста или `172.17.0.1`.

## Наблюдаемость

| Что                       | Где                                                                           |
| ------------------------- | ----------------------------------------------------------------------------- |
| Grafana, дашборд «Vektor» | <http://localhost:3000>                                                       |
| Prometheus, targets       | <http://localhost:9090>                                                       |
| Метрики сервисов          | `localhost:8081`–`8085` → Java: `/actuator/prometheus`, Python/Go: `/metrics` |

`retrieval` наружу не публикуется — его метрики доступны только через
Prometheus. Имена общие: `http_requests_total`, `http_request_duration_seconds`;
доменные: `answer_total{status}`, `ingest_documents_total{status}`,
`embedding_texts_total{kind}`, `jvm_memory_used_bytes`.

## Секреты

Секреты живут в `.env` (в git не входит; шаблон — `.env.example`):

| Ключ                       | Что                                      |
| -------------------------- | ---------------------------------------- |
| `AUTH_SIGNING_KEY_B64`     | RSA-ключ подписи сессий, PKCS#8 в base64 |
| `DOCUMENTS_INTERNAL_TOKEN` | общий токен `/internal/*`                |
| `POSTGRES_PASSWORD`        | пароль Postgres                          |

Генерирует `infra/scripts/ensure-env.sh` (идемпотентно, ровно один раз).
Java-сервисы читают ключ через окружение (`AUTH_SIGNING_KEY_B64`,
`SPRING_DATASOURCE_PASSWORD`); `spring.config.import: optional:vault://` в
конфиге молча скипается без Vault.

Пересоздать ключ принудительно: удалить строку из `.env` и выполнить
`make env`, затем `docker compose up -d auth documents` — сессии старых
пользователей при этом инвалидируются.

## Разбор сбоев

| Симптом                                              | Проверка                     | Действие                                                                               |
| ---------------------------------------------------- | ---------------------------- | -------------------------------------------------------------------------------------- |
| Контейнер перезапускается                            | `make logs SVC=<svc>`        | чаще всего не задан `AUTH_SIGNING_KEY_B64` (запустить `make env`) или нет весов модели |
| `auth` не стартует с `auth.signing-key-b64 не задан` | `.env`                       | `make env`, затем `docker compose up -d auth documents`                                |
| `/search` отвечает `503`                             | `make logs SVC=retrieval`    | проверить Elasticsearch и `embedding`                                                  |
| `/search` находит только точные слова                | `GET localhost:8084/healthz` | нет весов: положить их в `apps/backend/embedding/models` и перезапустить               |
| Документ загружен, но не находится                   | `make logs SVC=ingest`       | смотреть ошибку консьюмера; повторная публикация через `/internal/documents`           |
| `/answers` -> `503 LLM_UNAVAILABLE`                  | `LLM_*` в `.env`             | задать `LLM_PROVIDER=gigachat LLM_API_KEY=…`, затем `docker compose up -d answer`      |
| `429` на логине                                      | ожидаемо                     | лимит router: 2 rps на IP                                                              |
| `413` на файле                                       | размер                       | лимит 8 МиБ на тело, 5 МиБ на файл                                                     |

Полезное:

```bash
docker compose ps
docker compose logs -f ingest
docker compose exec postgres psql -U vektor -d vektor_docs -c '\dt'
docker compose exec elasticsearch curl -s 'localhost:9200/vektor-chunks-v2/_count'
```

## Ограничения стенда

- Один узел Elasticsearch — реплик нет, индекс производный.
- Нет TLS; cookie без `Secure`. Стенд локальный.
- Нет DLQ и outbox: см. [../architecture/overview.md](../architecture/overview.md).
