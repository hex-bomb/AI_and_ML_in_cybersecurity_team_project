# Модель данных

Одна установка — одна организация. Пространство знаний одно, приложений с
виджетом может быть несколько.

Как читать страницу: здесь владение таблицами и инварианты. Контракт API —
[../reference/api.md](../reference/api.md), принципы деления сервисов —
[overview.md](overview.md).

## Владение

Каждый сервис пишет только в свою базу. Чужих таблиц не читает, потому что
между базами нет внешних ключей.

| Сервис      | База            | Таблицы                                                                                                                                                                                             |
| ----------- | --------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `auth`      | `vektor_auth`   | `users`, `groups`, `memberships`                                                                                                                                                                    |
| `documents` | `vektor_docs`   | `blobs`, `documents`, `document_versions`, `extraction_runs`, `index_generations`, `access_grants`, `widget_installations`, `page_bindings`, `outbox_events`, `idempotency_records`, `audit_events` |
| `answer`    | `vektor_answer` | `conversations`, `turns`, `citations`, `product_events`, `idempotency_records`                                                                                                                      |
| `ingest`    | —               | только Elasticsearch и Kafka                                                                                                                                                                        |
| `retrieval` | —               | только Elasticsearch                                                                                                                                                                                |

## Три независимых состояния

Смешивать их нельзя: «файл обработан» и «документ доступен посетителям» —
разные события.

```text
processing    document_versions.processing   pending → running → ready | failed
publication   documents.publication          draft → published → withdrawn
lifecycle     documents.lifecycle            active → deleting → deleted
```

Пока `processing` не стал `ready`, версия не может быть опубликована. Пока
документ не `active`, новые задания обработки не принимаются.

## Инварианты и где они держатся

Оригинал и извлечённый текст лежат в блоб-хранилище: в БД только метаданные и
`storage_key`. У версии две ссылки — `blob_id` (оригинал) и `text_blob_id`
(текст), поэтому источник отдаётся без повторного разбора, а резервная копия
состоит из базы и тома с блобами.

Сервис может упасть в любой момент, поэтому часть правил вынесена в схему.

| Инвариант                                                                                    | Где проверяется                       |
| -------------------------------------------------------------------------------------------- | ------------------------------------- |
| Нельзя опубликовать необработанную версию, версию чужого документа или документ в `deleting` | триггер `trg_enforce_publication`     |
| Публикация без указателя версии невозможна                                                   | `ck_documents_published`              |
| Опубликованную версию нельзя удалить, пока документ из неё не выведен                        | триггер `trg_forbid_published_delete` |
| На версию идёт не больше одной сборки индекса                                                | `uq_generations_building`             |
| Аудит нельзя переписать или удалить                                                          | триггер `trg_audit_append_only`       |
| Повтор сообщения с тем же `client_message_id` не создаёт вторую реплику                      | `uq_turns_client_message`             |
| В диалоге не больше одной активной генерации                                                 | `uq_turns_active`                     |
| Версия уникальна по паре «документ + sha256»                                                 | `uq_versions_hash`                    |
| Грант доступа не дублируется                                                                 | `uq_grants`                           |

Остальные правила держатся в коде сервисов: порядок событий, идемпотентность
записи в Elasticsearch, соблюдение границ таймаутов.

## События

`documents` пишет событие в `outbox_events` в той же транзакции, что и изменение
документа, а отдельный relay публикует его в Kafka и проставляет
`published_at`. Так событие не теряется при падении между коммитом и отправкой.

| Событие                    | Публикует   | Обрабатывает |
| -------------------------- | ----------- | ------------ |
| `document.version.created` | `documents` | `ingest`     |
| `document.published`       | `documents` | `ingest`     |
| `document.access.changed`  | `documents` | `ingest`     |
| `document.withdrawn`       | `documents` | `ingest`     |

Готовность индекса приходит внутренним вызовом
`POST /internal/versions/{id}/index`, а не событием: `documents` сравнивает
номер генерации до того, как поставить версии `ready`.

События ключуются по `document_id`: порядок изменений одного документа
сохраняется внутри партиции.

## Elasticsearch

Индекс производный: его можно пересобрать из `blobs` и `document_versions`.
Чанк несёт поля, по которым `retrieval` фильтрует без обращения к `documents`:

```text
chunk_id, document_id, version_id, published, channel, group_keys[],
page_start, page_end, section_path, text, embedding
```

`published` проставляется событиями `document.published` и
`document.withdrawn`. Между коммитом в Postgres и применением события в индексе
есть задержка: публикация становится видимой для поиска не мгновенно. Это
ограничение описано, а не спрятано.

Индекс называется `vektor-chunks-v2`: поля доступа изменились несовместимо
(`kb_id` и `audience` убраны), и старый индекс нельзя переиспользовать — его
можно удалить после переиндексации.

## Миграции

Схема меняется целиком, поэтому старые `V1`/`V2` в `auth` и `documents` заменены
одним baseline. На стенде с прежней базой нужен чистый том:

```bash
docker compose down -v && make up
```

`auth` и `documents` накатывают миграции через Flyway. `answer` получает базу
впервые; миграции лежат в `apps/backend/answer/migrations` и применяются
отдельной командой до старта сервиса.

Требуется PostgreSQL 15 или новее: `UNIQUE NULLS NOT DISTINCT` появился в 15.
