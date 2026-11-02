# Kafka

Один брокер `apache/kafka:3.8.0` в режиме KRaft: без ZooKeeper, узел совмещает
роли `broker` и `controller`.

## Топики

| Топик                  | Producer    | Consumer | Ключ          |
| ---------------------- | ----------- | -------- | ------------- |
| `kb.document.upserted` | `documents` | `ingest` | `external_id` |

Один раздел (`KAFKA_NUM_PARTITIONS=1`), репликация `1`. Порядок по ключу
документа сохраняется, параллелизм внутри топика отсутствует.

## Схема сообщения

```json
{
  "kb_id": "74ade984-591f-4382-a051-ef1898855868",
  "external_id": "sha256 содержимого",
  "version_id": "uuid",
  "audience": "public | employee",
  "title": "Название",
  "sha256": "хеш текста"
}
```

Тела документа в сообщении нет: `ingest` забирает текст по
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
