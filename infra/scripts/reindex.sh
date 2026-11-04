#!/usr/bin/env bash
# Пересобрать поисковый индекс из базы.
#
#   ./infra/scripts/reindex.sh
#
# Индекс производный и в резервную копию не входит. После восстановления из
# копии, смены модели эмбеддингов или ручной чистки Elasticsearch его собирают
# заново: сервис documents ставит в очередь все опубликованные версии, ingest
# перечитывает текст и пересчитывает векторы.
set -euo pipefail
cd "$(dirname "$0")/../.."

BASE=${DOCUMENTS_SEED_URL:-http://127.0.0.1:18081}
TOKEN=${DOCUMENTS_INTERNAL_TOKEN:-$(sed -n 's/^DOCUMENTS_INTERNAL_TOKEN=//p' .env | head -n1)}
DEADLINE=${REINDEX_TIMEOUT:-300}

echo ">> ставлю опубликованные документы в очередь"
curl -fsS -X POST "$BASE/internal/reindex" \
  -H "X-Internal-Token: $TOKEN" \
  -H 'Content-Type: application/json' -d '{}' | tr -d '\n'
echo

expected=$(docker compose exec -T postgres psql -U vektor -d vektor_docs -tAc \
  "select count(*) from documents where publication = 'published'" | tr -d '[:space:]')

# Пустой outbox значит «отправлено», но не «проиндексировано». Ждём, пока в
# Elasticsearch окажется столько же опубликованных документов, сколько в базе:
# именно это и есть согласованность после восстановления.
indexed() {
  docker compose exec -T elasticsearch curl -s -H 'Content-Type: application/json' \
    'localhost:9200/vektor-chunks-v2/_search' \
    -d '{"size":0,"query":{"term":{"published":true}},"aggs":{"docs":{"cardinality":{"field":"document_ref"}}}}' |
    python3 -c 'import json,sys; print(json.load(sys.stdin)["aggregations"]["docs"]["value"])'
}

waited=0
live=0
while [ "$waited" -lt "$DEADLINE" ]; do
  live=$(indexed 2>/dev/null || echo 0)
  [ "$live" = "$expected" ] && break
  sleep 2
  waited=$((waited + 2))
done

if [ "$live" != "$expected" ]; then
  echo ">> за ${DEADLINE}с проиндексировано $live из $expected: смотрите docker compose logs ingest" >&2
  exit 1
fi

chunks=$(docker compose exec -T elasticsearch curl -s \
  'localhost:9200/vektor-chunks-v2/_count' | sed -n 's/.*"count":\([0-9]*\).*/\1/p')
echo ">> готово: опубликовано документов $live, фрагментов ${chunks:-0}"
