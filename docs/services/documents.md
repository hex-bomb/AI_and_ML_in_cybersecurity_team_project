# documents

Хранение документов и их версий, разбор файлов, публикация событий в Kafka.
Владелец таблиц `documents` и `document_versions`.

## Ответственность

Публичный API (требует JWT, только роль `employee`):

- `POST /api/v1/documents` — загрузка `.md/.txt/.pdf/.docx`.
- `GET /api/v1/documents` — список документов базы.
- `GET /api/v1/documents/{externalId}/source` — текст последней версии.

Внутренний API (требует заголовок `X-Internal-Token`):

- `POST /internal/documents` — запись документа из внешнего источника.
- `GET /internal/documents/{id}/versions/{version}/text` — текст версии.
- `POST /internal/extract` — разбор файла на текст.

Не индексирует и не ищет: публикует событие, индексацию делает `ingest`.

## Контракт

Событие Kafka `kb.document.upserted` (ключ — `document_id`):

```json
{
  "event": "document.upserted",
  "document_id": "...",
  "version_id": "...",
  "kb_id": "...",
  "audience": "...",
  "title": "...",
  "content_sha256": "..."
}
```

Публикация асинхронна: ошибка брокера логируется в callback и не валит запрос —
индекс производный, событие чинится повторной публикацией.

## Модель данных

`documents` — один документ (`kb_id` + `external_id`), `document_versions` — версии
с уникальностью по `(document_id, content_sha256)`: повторная загрузка того же
содержимого не создаёт дубликата. `external_id` — SHA-256 содержимого (первые 16
hex), поэтому файлы с похожими именами не перетирают друг друга.

## Защита internal-эндпоинтов

Все `/internal/*` сверяют заголовок `X-Internal-Token` с `documents.internal-token`.
Входной router эти пути не маршрутизирует, а порт публикуется только на
loopback — это и есть защита внутреннего контура. Пустое значение токена
запрещает все внутренние вызовы (fail closed). Токен генерируется в
`infra/scripts/ensure-env.sh` и попадает в контейнеры через переменную
окружения `DOCUMENTS_INTERNAL_TOKEN` из `.env`.

## Конфигурация

| Переменная                 | По умолчанию           | Назначение                                 |
| -------------------------- | ---------------------- | ------------------------------------------ |
| `DOCUMENTS_INTERNAL_TOKEN` | —                      | Общий секрет `/internal/*`; пусто = запрет |
| `DOCUMENTS_TOPIC`          | `kb.document.upserted` | Топик событий                              |
| `MAX_UPLOAD_BYTES`         | `5242880`              | Лимит файла, байт                          |
| `JWKS_URL`                 | `http://auth:8080/...` | Ключи для проверки JWT                     |
| `AUTH_KB_ID`               | `74ade…5868`           | База по умолчанию                          |

Ключ подписи и пароль БД — секреты из `.env` (`AUTH_SIGNING_KEY_B64`,
`POSTGRES_PASSWORD`), не из Git.

## Разбор файлов

`Extract` поддерживает `.pdf` (PDFBox), `.docx` (POI), `.md`, `.txt`. Файл без
извлекаемого текста → `400`. Разбор вынесен в `documents`, потому что PDFBox и POI
блокируют поток и не должны мешать event loop Python-сервисов.

## Авторизация

JWT проверяется по JWKS `auth` (кэш на 5 минут, принудительное обновление при
неизвестном `kid`). Читать источник документа можно только своей аудиторией
(`canRead` по `scp`); «нет документа» и «нет прав» неразличимы — оба `404`.

## Наблюдаемость

`ObsFilter` пишет метрики по шаблону маршрута (без раздувания cardinality) и
пробрасывает `X-Request-ID`. Spring: `/actuator/health`, `/actuator/prometheus`.

## Отказы

- Нет/неверный JWT → `403 FORBIDDEN`.
- Нет `X-Internal-Token` на `/internal/*` → `403 FORBIDDEN`.
- Файл больше лимита → `413 PAYLOAD_TOO_LARGE`.
- Неподдерживаемый формат → `400 INVALID_REQUEST`.
- Kafka недоступна → запрос успешен, ошибка публикации в логе.

## Тесты

Юнит-тестов нет. Сборка и `mvn test` гоняются в CI.

## Ссылки

`contracts/openapi/openapi.yaml`, `docs/infrastructure/kafka.md`.
