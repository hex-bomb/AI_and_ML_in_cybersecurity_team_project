# API платформы

Публичный контракт микросервисной версии. Машинночитаемый первоисточник —
`contracts/openapi/openapi.yaml`; он же проверяется в CI вместе с Postman-коллекцией
и маршрутами входного router (`scripts/check-contracts.py`).

Базовый адрес локального стенда: `http://localhost:8080` — единый вход
(service router в `docker-compose.yml`, Host-заголовок не обязателен).

## Интерактивная документация

Каждый сервис отдаёт свою схему сам. Публично они не маршрутизируются —
доступны по loopback-портам, проброшенным в `docker-compose.yml`:

| Сервис      | Swagger UI                                      | OpenAPI JSON                  |
| ----------- | ----------------------------------------------- | ----------------------------- |
| `auth`      | `localhost:8081/swagger-ui.html`                | `localhost:8081/v3/api-docs`  |
| `documents` | `localhost:8082/swagger-ui.html`                | `localhost:8082/v3/api-docs`  |
| `answer`    | `localhost:8083/docs`                           | `localhost:8083/openapi.json` |
| `embedding` | `localhost:8084/docs`                           | `localhost:8084/openapi.json` |
| `ingest`    | `localhost:8085/docs`                           | `localhost:8085/openapi.json` |
| `retrieval` | — (контракт в `contracts/openapi/openapi.yaml`) | —                             |

Java-сервисы отдают схему через springdoc, Python — через FastAPI. `retrieval`
написан на стандартной библиотеке Go и схему не генерирует: его контракт описан
в общем `openapi.yaml`.

## Эндпоинты

| Метод | Путь                                      | Доступ             | Сервис                  |
| ----- | ----------------------------------------- | ------------------ | ----------------------- |
| POST  | `/api/v1/auth/login`                      | открыт             | `auth`                  |
| POST  | `/api/v1/auth/logout`                     | открыт             | `auth`                  |
| GET   | `/api/v1/auth/me`                         | cookie             | `auth`                  |
| GET   | `/.well-known/jwks.json`                  | открыт             | `auth`                  |
| POST  | `/api/v1/search`                          | cookie             | `retrieval`             |
| POST  | `/api/v1/answers`                         | cookie             | `answer`                |
| POST  | `/api/v1/answers/file`                    | cookie             | `answer` -> `documents` |
| GET   | `/api/v1/documents`                       | cookie + сотрудник | `documents`             |
| POST  | `/api/v1/documents`                       | cookie + сотрудник | `documents`             |
| GET   | `/api/v1/documents/{documentId}/source`   | cookie             | `documents`             |
| POST  | `/api/v1/documents/{documentId}/publish`  | cookie + сотрудник | `documents`             |
| POST  | `/api/v1/documents/{documentId}/withdraw` | cookie + сотрудник | `documents`             |

Сессия — JWT в httpOnly cookie `vektor_session` с `SameSite=Lax`. Заголовок
`Authorization: Bearer` поддерживается для вызовов между сервисами.

### Служебные ручки

Входной router их не маршрутизирует — доступны только изнутри сети compose
(порт `18081` опубликован на loopback):

| Метод | Путь                                           | Сервис      | Назначение                                         |
| ----- | ---------------------------------------------- | ----------- | -------------------------------------------------- |
| POST  | `/internal/documents`                          | `documents` | залить текст без файла (сид корпуса), идемпотентно |
| POST  | `/internal/publish/{externalId}`               | `documents` | опубликовать готовую версию для сида               |
| POST  | `/internal/versions/{versionId}/index`         | `documents` | отчёт индексатора о готовности версии              |
| GET   | `/internal/documents/{id}/versions/{vid}/text` | `documents` | текст версии для `ingest`                          |
| POST  | `/internal/extract`                            | `documents` | разобрать файл и вернуть текст                     |
| POST  | `/internal/reindex`                            | `documents` | пересобрать индекс после restore или смены модели  |
| GET   | `/internal/page-bindings/{contentKey}`         | `documents` | документ, привязанный к странице виджета           |
| POST  | `/internal/page-bindings`                      | `documents` | создать привязку страницы (сид, интеграции)        |

## Лимиты

| Что                      | Значение                 | Где проверяется                                 |
| ------------------------ | ------------------------ | ----------------------------------------------- |
| Тело запроса             | 8 МиБ                    | router (`client_max_body_size`)                 |
| Файл                     | 5 МиБ                    | Spring multipart + явная проверка в `documents` |
| `question`               | 1–4000 символов          | pydantic в `answer`                             |
| `query`                  | ≤ 1000                   | `retrieval`                                     |
| `focus_document_ref`     | ≤ 64                     | `retrieval`                                     |
| `conversation_id`        | uuid                     | pydantic в `answer`                             |
| `client_message_id`      | ≤ 100 символов           | pydantic в `answer`                             |
| `page_context`           | поля обрезаются          | `app/page_context.py` в `answer`                |
| `top_k`                  | 1–10                     | `retrieval`                                     |
| Частота, обычные запросы | 20 rps на IP             | router (`limit_req zone=main`)                  |
| Частота, `/auth/login`   | 2 rps на IP, ответ `429` | router (`limit_req zone=login`)                 |

## Отличия от монолита

| Что                             | Монолит                           | Платформа                                                 |
| ------------------------------- | --------------------------------- | --------------------------------------------------------- |
| `POST /documents` ответ         | `{document_id, chunks, replaced}` | `{document_id, version_id, created}`                      |
| Индексация                      | синхронно в запросе               | асинхронно через Kafka                                    |
| `document_id`                   | slug из имени файла               | префикс SHA-256 содержимого                               |
| Приватный документ по чужому id | `403`                             | `404` (неотличимо от несуществующего)                     |
| Swagger                         | один на всё                       | у каждого сервиса свой (Java: springdoc, Python: FastAPI) |

Виджету эти отличия не мешают: пути и формы ответов `/search` и `/answers`
сохранены.

## Коды ошибок

Конверт тот же, что в монолите:

```json
{
  "error": { "code": "FORBIDDEN", "message": "Доступ запрещён" },
  "request_id": "…"
}
```

| HTTP | `code`                | Когда                                                         |
| ---- | --------------------- | ------------------------------------------------------------- |
| 400  | `INVALID_REQUEST`     | валидация, пустой файл, неразбираемый формат                  |
| 401  | `UNAUTHORIZED`        | нет cookie, истёк или испорчен токен                          |
| 403  | `FORBIDDEN`           | нет роли, запрос отклонён guard                               |
| 404  | `NOT_FOUND`           | документа нет или он недоступен                               |
| 404  | `CONVERSATION_NOT_FOUND` | диалога нет или он принадлежит другому пользователю        |
| 409  | `TURN_IN_PROGRESS`    | в диалоге уже идёт генерация                                  |
| 409  | `IDEMPOTENCY_CONFLICT` | `client_message_id` уже использован с другим вопросом        |
| 413  | `PAYLOAD_TOO_LARGE`   | файл или тело больше лимита                                   |
| 429  | —                     | превышен лимит router; тело — штатное для nginx, без конверта |
| 502  | `LLM_BAD_OUTPUT`      | модель вернула пустой ответ                                   |
| 503  | `SEARCH_UNAVAILABLE`  | Elasticsearch недоступен                                      |
| 503  | `LLM_UNAVAILABLE`     | генерация не настроена или упала                              |
| 503  | `SERVICE_UNAVAILABLE` | недоступен соседний сервис (`documents`, `embedding`)         |
| 503  | `AUTH_UNAVAILABLE`    | JWKS auth не удалось получить                                 |
| 500  | `INTERNAL`            | непойманное исключение, искать по `request_id` в логах        |

`403` против `404` на исходнике документа — осознанно: перебор идентификаторов
не должен показывать, какие внутренние документы существуют.

## Postman

`contracts/postman/vektor-platform.postman_collection.json` — переменные
`baseUrl` (по умолчанию `http://localhost:8080`), `kbId`, `documentId`.
Cookie сохраняется автоматически: сначала выполните `auth/login`, дальше
остальные запросы пойдут с сессией.

CI проверяет, что все пути из коллекции есть в `openapi.yaml`.
