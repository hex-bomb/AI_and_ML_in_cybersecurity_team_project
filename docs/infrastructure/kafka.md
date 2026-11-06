# Kafka

Один брокер `apache/kafka:3.8.0` в режиме KRaft: без ZooKeeper, узел совмещает
роли `broker` и `controller`.

## Топики

| Топик                      | Producer    | Consumer     | Ключ          |
| -------------------------- | ----------- | ------------ | ------------- |
| Событие                    | Публикует   | Обрабатывает | Ключ          |
| -------------------------- | ----------- | ------------ | ------------- |
| `document.version.created` | `documents` | `ingest`     | `document_id` |
| `document.published`       | `documents` | `ingest`     | `document_id` |
| `document.access.changed`  | `documents` | `ingest`     | `document_id` |
| `document.withdrawn`       | `documents` | `ingest`     | `document_id` |

Один раздел (`KAFKA_NUM_PARTITIONS=1`), репликация `1`. Порядок по ключу
документа сохраняется, параллелизм внутри топика отсутствует.

События пишет `documents` через outbox: отправка отделена от транзакции,
поэтому падение между коммитом и отправкой не теряет событие.

## Схема сообщения

```json
{
  "event": "document.version.created",
  "document_id": "uuid документа",
  "external_id": "about-vektor",
  "version_id": "uuid версии",
  "generation": 1,
  "title": "Название",
  "section": "Компания",
  "content_sha256": "хеш текста"
}
```

В событии публикации есть `channels` и `group_keys` — ингест выставляет по ним
видимость фрагментов. Тела документа в сообщении нет: `ingest` забирает текст по
`GET /internal/documents/{external_id}/versions/{version_id}/text`. Это держит
сообщения маленькими и убирает дублирование данных.

## Доставка

At-least-once: `ingest` коммитит offset после успешной записи в Elasticsearch.
Повторная доставка безопасна — фрагменты пишутся по детерминированному
`chunk_id`.

Outbox и DLQ не сделаны осознанно. Если событие потерялось между коммитом в
Postgres и отправкой в Kafka, документ дожимается идемпотентным
`POST /internal/documents`.

## Проверка

```bash
docker compose logs kafka
docker compose exec kafka /opt/kafka/bin/kafka-topics.sh \
  --bootstrap-server localhost:9092 --list
```

## Файлы

Сервис Kafka описан в `docker-compose.yml`.
