# documents

Хранение документов, версий и оригиналов, разбор файлов, публикация.
Владелец таблиц `vektor_docs`.

## Ответственность

Публичный API (JWT, роль `employee`):

- `POST /api/v1/documents` — загрузка `.md/.txt/.pdf/.docx`;
- `GET /api/v1/documents` — список доступных документов;
- `GET /api/v1/documents/{externalId}/source` — текст опубликованной версии;
- `POST /api/v1/documents/{externalId}/publish` — опубликовать готовую версию;
- `POST /api/v1/documents/{externalId}/withdraw` — вывести из публикации.

Внутренний API (`X-Internal-Token`):

- `POST /internal/documents` — запись документа из внешнего источника;
- `POST /internal/publish/{externalId}` — публикация для заливки корпуса;
- `POST /internal/versions/{versionId}/index` — отчёт индексатора о готовности;
- `GET /internal/documents/{id}/versions/{version}/text` — текст версии;
- `POST /internal/extract` — разбор файла на текст.

Не индексирует и не ищет: пишет событие в outbox, индексацию делает `ingest`.

## Три состояния

`processing` у версии (`pending → running → ready | failed`), `publication` у
документа (`draft → published → withdrawn`) и `lifecycle`
(`active → deleting → deleted`) меняются независимо. Опубликовать версию можно
только после `ready`; это проверяет триггер в БД, а не только сервис.

## События

`documents` пишет событие в `outbox_events` в той же транзакции, что и изменение
документа. `OutboxRelay` раз в `DOCUMENTS_OUTBOX_INTERVAL_MS` публикует пачку в
Kafka и проставляет `published_at`. Топик — `DOCUMENTS_TOPIC`, ключ —
`aggregate_id` (внутренний id документа), поэтому порядок изменений одного
документа сохраняется в партиции.

| Событие                    | Когда                                                          |
| -------------------------- | -------------------------------------------------------------- |
| `document.version.created` | появилась новая версия, её нужно проиндексировать              |
| `document.published`       | документ опубликован: индексатор выставляет `published = true` |
| `document.access.changed`  | изменился канал доступа опубликованного документа              |
| `document.withdrawn`       | документ выведен из публикации                                 |

Семантика at-least-once: событие может уйти повторно, если процесс упал между
отправкой и отметкой `published_at`. Потребитель обязан быть идемпотентен.

## Модель данных

`documents` — документ с уникальным `external_id`, `document_versions` —
неизменяемые версии с уникальностью по `(document_id, content_sha256)`.
`external_id` для загруженного файла — SHA-256 содержимого (первые 16 hex),
поэтому файлы с похожими именами не перетирают друг друга, а повторная загрузка
возвращает существующую версию.

Оригинал и извлечённый текст лежат в `BlobStore` (том `docs-blobs`), в БД —
только метаданные и `storage_key`. Ссылки: `blob_id` (оригинал) и `text_blob_id`
(текст) в `document_versions`.

## Доступ

Один предикат на все чтения — список, источник, поиск: грант с `document_id`
или без него, канал из `channels()` токена, группа из `grp`. Грант без
`group_key` действует на всех пользователей канала. «Нет документа» и «нет
прав» неразличимы — оба `404`.

Сотрудник видит каналы `public` и `internal`, посетитель — только `public`.

## Публикация

Публикация — это `UPDATE documents SET published_version_id = ...`; триггер
проверяет, что версия принадлежит документу, `processing = ready` и документ
`active`. Опубликованную версию нельзя удалить, пока документ из неё не выведен.

## Отчёт индексатора

`POST /internal/versions/{id}/index` принимает `generation`, `embedding_model`,
`embedding_dim`, `chunk_count`. Если `generation` не совпадает с текущей
генерацией версии, отчёт отклоняется: worker, доработавший после загрузки новой
версии, не должен вернуть документ в прежнее состояние. Ответ
`{"accepted": false}`.

## Конфигурация

| Переменная                     | По умолчанию           | Назначение                       |
| ------------------------------ | ---------------------- | -------------------------------- |
| `DOCUMENTS_INTERNAL_TOKEN`     | —                      | Общий секрет `/internal/*`       |
| `DOCUMENTS_TOPIC`              | `documents`            | Топик событий                    |
| `DOCUMENTS_BLOB_DIR`           | `/data/blobs`          | Каталог оригиналов и текста      |
| `DOCUMENTS_OUTBOX_INTERVAL_MS` | `500`                  | Период публикации outbox         |
| `MAX_UPLOAD_BYTES`             | `5242880`              | Лимит файла, байт                |
| `MAX_TEXT_CHARS`               | `2000000`              | Лимит текста для внутреннего API |
| `JWKS_URL`                     | `http://auth:8080/...` | Ключи для проверки JWT           |

## Разбор файлов

`Extract` поддерживает `.pdf` (PDFBox), `.docx` (POI), `.md`, `.txt`. Файл без
извлекаемого текста или текст короче 20 символов → `400`. Формат определяется по
расширению; проверка MIME по содержимому ещё не сделана.

## Отказы

- Нет или неверный JWT → `403 FORBIDDEN`.
- Нет `X-Internal-Token` на `/internal/*` → `403 FORBIDDEN`.
- Файл больше лимита → `413 PAYLOAD_TOO_LARGE`.
- Неподдерживаемый формат или пустой текст → `400 INVALID_REQUEST`.
- Публикация неготовой версии → `400 INVALID_REQUEST`.
- Kafka недоступна → запрос успешен, событие остаётся в outbox до следующей попытки.

## Тесты

`DocumentRepositoryTest` — интеграционные на PostgreSQL 16 в Testcontainers:
создание версии и события, повторная загрузка, публикация только готовой версии,
устаревшая генерация, фильтр по каналам, отзыв. `TokenVerifierTest` — юнит на
проверку JWT и каналов.

## Ссылки

`contracts/openapi/openapi.yaml`, [data-model.md](../architecture/data-model.md).
