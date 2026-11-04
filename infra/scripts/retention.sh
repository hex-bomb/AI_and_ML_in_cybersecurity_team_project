#!/usr/bin/env bash
# Удаление того, что можно удалить без потери знаний.
#
#   ./infra/scripts/retention.sh
#   CONVERSATION_DAYS=180 ./infra/scripts/retention.sh
#
# По умолчанию чистятся только производные данные: отправленные события outbox
# и просроченные записи идемпотентности. История диалогов удаляется, только если
# задан CONVERSATION_DAYS: это данные продукта, и срок хранения — решение
# заказчика, а не сервиса.
#
# Оригиналы и старые версии документов автоматически не удаляются: удаление
# файла и строки версии должно идти вместе, и делать это фоном без явной
# политики значит однажды потерять документ, который ещё нужен.
set -euo pipefail
cd "$(dirname "$0")/../.."

OUTBOX_DAYS=${OUTBOX_DAYS:-7}
CONVERSATION_DAYS=${CONVERSATION_DAYS:-0}

query() {
  docker compose exec -T postgres psql -U vektor -d "$1" -tAc "$2"
}

echo ">> outbox: отправленное старше $OUTBOX_DAYS дней"
query vektor_docs "
  with removed as (
    delete from outbox_events
     where published_at is not null
       and created_at < now() - make_interval(days => $OUTBOX_DAYS)
     returning 1
  )
  select 'удалено событий: ' || count(*) from removed"

echo ">> идемпотентность: просроченные записи"
query vektor_docs "
  with removed as (
    delete from idempotency_records where expires_at < now() returning 1
  )
  select 'удалено записей: ' || count(*) from removed"
query vektor_answer "
  with removed as (
    delete from idempotency_records where expires_at < now() returning 1
  )
  select 'удалено записей: ' || count(*) from removed"

if [ "$CONVERSATION_DAYS" -gt 0 ]; then
  echo ">> диалоги: старше $CONVERSATION_DAYS дней"
  query vektor_answer "
    with removed as (
      delete from conversations
       where coalesce(last_turn_at, created_at) < now() - make_interval(days => $CONVERSATION_DAYS)
       returning 1
    )
    select 'удалено диалогов: ' || count(*) from removed"
  query vektor_answer "
    with removed as (
      delete from product_events
       where occurred_at < now() - make_interval(days => $CONVERSATION_DAYS)
       returning 1
    )
    select 'удалено событий: ' || count(*) from removed"
else
  echo ">> история диалогов не тронута (CONVERSATION_DAYS=0)"
fi
