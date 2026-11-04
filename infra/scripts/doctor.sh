#!/usr/bin/env bash
# Проверка установки: конфигурация, базы, миграции, блобы, поиск, очередь, модели.
#
# Скрипт ничего не меняет. Проблема уровня FAIL даёт код возврата 1: установка
# не готова к работе. WARN — повод посмотреть, но сервис работает.
set -uo pipefail
cd "$(dirname "$0")/../.."

FAILED=0
COMPOSE=(docker compose)

ok() { printf 'ok:    %s\n' "$1"; }
warn() { printf 'warn:  %s\n' "$1"; }
fail() { printf 'FAIL:  %s\n' "$1"; FAILED=$((FAILED + 1)); }

# Значение из .env без его загрузки в оболочку: секреты не должны попасть в ps.
env_value() {
  [ -f .env ] || return 0
  sed -n "s/^$1=//p" .env | head -n1
}

psql_scalar() {
  "${COMPOSE[@]}" exec -T postgres psql -U vektor -d "$1" -tAc "$2" 2>/dev/null | tr -d '[:space:]'
}

echo "== конфигурация"
if [ -f .env ]; then
  ok ".env на месте"
else
  fail ".env нет: выполните ./infra/scripts/ensure-env.sh"
fi

for name in POSTGRES_PASSWORD DOCUMENTS_INTERNAL_TOKEN AUTH_SIGNING_KEY_B64; do
  if [ -n "$(env_value "$name")" ]; then
    ok "$name задан"
  else
    warn "$name пуст: dev-значение подставится по умолчанию"
  fi
done

if [ -n "$(env_value LLM_BASE_URL)" ]; then
  ok "генерация настроена (LLM_BASE_URL)"
else
  warn "LLM_BASE_URL пуст: /answers будет отвечать 503, поиск работает"
fi

echo "== контейнеры"
running=$("${COMPOSE[@]}" ps --status running --services 2>/dev/null | wc -l | tr -d ' ')
if [ "$running" -ge 12 ]; then
  ok "запущено сервисов: $running"
else
  fail "запущено сервисов: $running из 12"
fi

unhealthy=$("${COMPOSE[@]}" ps --format '{{.Name}} {{.Status}}' 2>/dev/null | grep -c 'unhealthy')
if [ "$unhealthy" -eq 0 ]; then
  ok "нет unhealthy-контейнеров"
else
  fail "unhealthy-контейнеров: $unhealthy"
fi

echo "== PostgreSQL"
if ! "${COMPOSE[@]}" exec -T postgres pg_isready -U vektor >/dev/null 2>&1; then
  fail "PostgreSQL недоступен"
else
  ok "PostgreSQL отвечает"
  databases=$(psql_scalar postgres "select count(*) from pg_database where datname in ('vektor_auth','vektor_docs','vektor_answer')")
  if [ "$databases" = "3" ]; then
    ok "базы vektor_auth, vektor_docs, vektor_answer на месте"
  else
    fail "баз найдено $databases из 3"
  fi

  for pair in "vektor_auth:flyway_schema_history" "vektor_docs:flyway_schema_history" "vektor_answer:schema_migrations"; do
    db=${pair%%:*}
    table=${pair##*:}
    applied=$(psql_scalar "$db" "select count(*) from $table")
    if [ -n "$applied" ] && [ "$applied" != "0" ]; then
      ok "$db: миграций применено $applied"
    else
      fail "$db: таблица $table пуста или отсутствует"
    fi
  done

  pending=$(psql_scalar vektor_docs "select count(*) from outbox_events where published_at is null")
  if [ "$pending" = "0" ]; then
    ok "outbox пуст"
  else
    warn "в outbox не отправлено событий: $pending"
  fi
fi

echo "== блобы"
if "${COMPOSE[@]}" exec -T documents sh -c 'test -w /data/blobs' >/dev/null 2>&1; then
  ok "каталог /data/blobs доступен на запись"
else
  fail "каталог /data/blobs недоступен на запись"
fi

echo "== Elasticsearch"
health=$("${COMPOSE[@]}" exec -T elasticsearch curl -s localhost:9200/_cluster/health 2>/dev/null |
  sed -n 's/.*"status":"\([a-z]*\)".*/\1/p')
case "$health" in
green | yellow) ok "кластер $health" ;;
red) fail "кластер red" ;;
*) fail "кластер не отвечает" ;;
esac

chunks=$("${COMPOSE[@]}" exec -T elasticsearch curl -s 'localhost:9200/vektor-chunks-v2/_count' 2>/dev/null |
  sed -n 's/.*"count":\([0-9]*\).*/\1/p')
if [ -n "$chunks" ]; then
  if [ "$chunks" -gt 0 ]; then
    ok "фрагментов в индексе: $chunks"
  else
    warn "индекс пуст: опубликуйте документы или загрузите корпус"
  fi
else
  fail "индекс vektor-chunks-v2 не найден"
fi

echo "== Kafka"
topic=$("${COMPOSE[@]}" exec -T kafka /opt/kafka/bin/kafka-topics.sh \
  --bootstrap-server localhost:9092 --list 2>/dev/null | grep -c '^documents$')
if [ "$topic" = "1" ]; then
  ok "топик documents есть"
  lag=$("${COMPOSE[@]}" exec -T kafka /opt/kafka/bin/kafka-consumer-groups.sh \
    --bootstrap-server localhost:9092 --describe --group ingest 2>/dev/null |
    awk '/^ingest/ {sum += $6} END {print sum + 0}')
  if [ "$lag" = "0" ]; then
    ok "очередь разобрана"
  else
    warn "в очереди ingest осталось сообщений: $lag"
  fi
else
  fail "топика documents нет"
fi

echo "== модели"
dims=$("${COMPOSE[@]}" exec -T embedding python -c '
import json, urllib.request
request = urllib.request.Request(
    "http://127.0.0.1:8000/embed",
    data=json.dumps({"texts": ["проверка"], "kind": "query"}).encode(),
    headers={"Content-Type": "application/json"},
)
print(len(json.load(urllib.request.urlopen(request, timeout=20))["vectors"][0]))
' 2>/dev/null | tr -d '[:space:]')
if [ "$dims" = "384" ]; then
  ok "embedding отдаёт векторы размерности 384"
elif [ -n "$dims" ]; then
  fail "embedding отдаёт размерность $dims вместо 384: индекс несовместим"
else
  fail "embedding не отвечает"
fi

for service in answer retrieval; do
  port=$([ "$service" = answer ] && echo 8000 || echo 8080)
  if "${COMPOSE[@]}" exec -T "$service" python -c "import urllib.request as u; u.urlopen('http://127.0.0.1:$port/healthz', timeout=5)" >/dev/null 2>&1 ||
    "${COMPOSE[@]}" exec -T "$service" wget -qO- "localhost:$port/healthz" >/dev/null 2>&1; then
    ok "$service отвечает на /healthz"
  else
    fail "$service не отвечает на /healthz"
  fi
done

echo
if [ "$FAILED" -eq 0 ]; then
  echo "итог: проблем уровня FAIL нет"
  exit 0
fi
echo "итог: проблем уровня FAIL — $FAILED"
exit 1
