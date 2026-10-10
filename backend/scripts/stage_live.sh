#!/usr/bin/env bash
# Поднимает изолированный стенд для живой проверки: своя база, свой
# Elasticsearch, настройки аналитика. Существующие kbpg/kbos не трогает.
#
# Стенд нужен потому, что общий Elasticsearch сносится параллельными
# прогонами тестов: на общем индексе проверкаsearch регулярно видела
# no_results вместо результата.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO_DIR="$ROOT/go"
cd "$ROOT"

DB_NAME="${KB_LIVE_DB:-kb_live}"
PG_PORT="${PG_PORT:-55432}"
ES_PORT="${ES_LIVE_PORT:-59300}"
API_PORT="${API_LIVE_PORT:-8099}"
STATE="${KB_LIVE_STATE:-/private/var/folders/nl/xkmrs2b53x19645_wbj19shc0000gn/T/opencode/kb_live_files}"
TENANT="11111111-1111-4111-8111-111111111111"
KB="22222222-2222-4222-8222-222222222222"

step() { printf '\n=== %s ===\n' "$1"; }

step "Elasticsearch на порту $ES_PORT"
if ! curl -s -o /dev/null "http://localhost:$ES_PORT"; then
  docker rm -f es_live >/dev/null 2>&1 || true
  docker run -d --name es_live -p "$ES_PORT":9200 \
    -e "discovery.type=single-node" \
    -e "xpack.security.enabled=false" \
    -e "xpack.license.self_generated.type=basic" \
    -e "ES_JAVA_OPTS=-Xms1g -Xmx1g" \
    docker.elastic.co/elasticsearch/elasticsearch:8.17.3 >/dev/null
  for _ in $(seq 1 40); do
    curl -s -o /dev/null "http://localhost:$ES_PORT" && break
    sleep 5
  done
fi
curl -s -o /dev/null -w "  elasticsearch HTTP %{http_code}\n" "http://localhost:$ES_PORT"

step "База $DB_NAME"
PGPASSWORD=dev docker exec kbpg psql -U kb -d kb -q \
  -c "DROP DATABASE IF EXISTS $DB_NAME;" -c "CREATE DATABASE $DB_NAME;" >/dev/null

# Базовые секреты и настройки — из .env, адреса стенда — поверх них.
set -a; . ./.env; set +a
export DATABASE_URL="postgresql+asyncpg://kb:dev@localhost:$PG_PORT/$DB_NAME"
export FILE_STORAGE_ROOT="$STATE"
export ELASTICSEARCH_URL="http://localhost:$ES_PORT"
rm -rf "$FILE_STORAGE_ROOT" && mkdir -p "$FILE_STORAGE_ROOT"

step "Сборка"
(cd "$GO_DIR" && go build -o "$ROOT/bin/api" ./cmd/api && go build -o "$ROOT/bin/worker" ./cmd/worker)
echo "  бинари собраны"

step "Миграции"
"$ROOT/bin/api" -migrate
echo "  схема применена"

# Аналитик заводит tenant, базу и настройки: миграции этого не делают (ТЗ 5.3, 10.1).
PGPASSWORD=dev docker exec -i kbpg psql -U kb -d "$DB_NAME" -q <<SQL
INSERT INTO tenants (id, name) VALUES ('$TENANT', 'local');
INSERT INTO knowledge_bases (id, tenant_id, name, allowed_user_ids, allowed_group_ids, created_at)
VALUES ('$KB', '$TENANT', 'kb', '[]', '[]', now());
SQL
PGPASSWORD=dev docker exec -i kbpg psql -U kb -d "$DB_NAME" -q < scripts/seed_settings.sql
echo "  tenant, база и настройки заведены"

step "Запуск"
pkill -f "$ROOT/bin/api" 2>/dev/null || true
pkill -f "$ROOT/bin/worker" 2>/dev/null || true
sleep 1
nohup "$ROOT/bin/api" -addr "127.0.0.1:$API_PORT" > /tmp/api.log 2>&1 &
nohup "$ROOT/bin/worker" > /tmp/worker.log 2>&1 &
sleep 4
curl -s "http://127.0.0.1:$API_PORT/health/ready"; echo ""

printf '\nСтенд готов.\n  API:         http://127.0.0.1:%s/api/v1\n  База знаний: %s\n  Elasticsearch: %s\n' \
  "$API_PORT" "$KB" "$ES_PORT"
printf '  Загрузка документов и ответы: ./scripts/seed_kb.sh\n'
printf '  Полная проверка эндпоинтов:   ./scripts/check_endpoints.sh\n'
