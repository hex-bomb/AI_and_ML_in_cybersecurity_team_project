#!/usr/bin/env bash
#
# seed_kb.sh — демонстрационная загрузка базы знаний «ВЕКТОР» (ТЗ 10.1, 7.4, 7.5).
#
# Скрипт работает ТОЛЬКО через публичный HTTP API (/api/v1), поэтому проверяет
# ровно тот контракт, который получает фронтенд:
#   POST /api/v1/kbs                      — база знаний;
#   POST /api/v1/kbs/{kb_id}/documents    — загрузка (multipart, Idempotency-Key);
#   GET  /api/v1/jobs/{job_id}            — опрос задания каждые 2 секунды;
#   POST /api/v1/search                   — проверка поиска;
#   POST /api/v1/answers                  — проверка ответа с цитатами.
#
# Исключения, которых нет в обычном сценарии, и почему:
#   * если в БД нет локального tenant — применяется scripts/seed_tenant.sql;
#     без записи tenants POST /kbs даёт 500 по внешнему ключу. Миграции не
#     засевают tenants (ТЗ 2.2, 5.3), а в рабочем окружении tenant приходит
#     из JWT-издателя;
#   * если настроек базы нет — применяется scripts/seed_settings.sql, который
#     подготовлен аналитиком. Приложение настройки не создаёт (ТЗ 2.2).
#
# Идемпотентность: скрипт можно запускать повторно.
#   * база знаний переиспользуется по имени, если уже существует;
#   * документы идентифицируются постоянным external_id из имени файла, поэтому
#     повторная загрузка даёт 409 DOCUMENT_EXISTS и скрипт не падает;
#   * каждая попытка загрузки получает НОВЫЙ Idempotency-Key (UUID);
#   * готовый документ повторно не индексируется.
#
# Переменные окружения:
#   API_URL            базовый адрес API        (по умолчанию http://127.0.0.1:8000)
#   TOKEN              Bearer-токен             (по умолчанию не отправляется —
#                                              локальная сборка MVP работает без JWT)
#   KB_ID              готовая база знаний      (если задана, создание пропускается)
#   KB_NAME            имя новой базы           (по умолчанию «ВЕКТОР — промышленное оборудование»)
#   FIXTURES_DIR       каталог файлов           (по умолчанию contracts/fixtures/vektor)
#   POLL_INTERVAL      пауза опроса задания, с (по умолчанию 2)
#   JOB_TIMEOUT        таймаут задания, с       (по умолчанию 120)
#   SEED_DB            auto|1|0 — применять ли SQL-файлы оператора (по умолчанию auto)
#   QUERY              поисковый запрос для проверки  (по умолчанию см. SEARCH_QUERY)
#   QUESTION           вопрос для проверки ответов   (по умолчанию см. ANSWER_QUESTION)
#   DATABASE_URL       адрес БД для применения SQL-файлов оператора. Если не задан,
#                      берётся из .env. ВАЖНО: когда API поднят на другом порту
#                      или в другом compose-проекте, задайте DATABASE_URL явно,
#                      иначе скрипт применит SQL не в ту базу.
#   PSQL_BIN           путь к клиенту psql (автоопределение: psql, psql-18 …, docker exec)
#
# Пример:
#   API_URL=http://127.0.0.1:8000 ./scripts/seed_kb.sh
#   KB_ID=22222222-2222-4222-8222-222222222222 ./scripts/seed_kb.sh   # повтор в ту же базу

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(dirname -- "$SCRIPT_DIR")"
# contracts/ лежит уровнем выше backend/ — по монорепо-раскладке ТЗ.
REPO_DIR="$(dirname -- "$ROOT_DIR")"

API_URL="${API_URL:-http://127.0.0.1:8000}"
API_URL="${API_URL%/}"
# Префикс бизнес-API и префикс health (ТЗ 2.3, 10.1).
API_PREFIX="${API_PREFIX:-/api/v1}"
HEALTH_PREFIX="${HEALTH_PREFIX:-/health}"
TOKEN="${TOKEN:-}"
KB_ID="${KB_ID:-}"
KB_NAME="${KB_NAME:-ВЕКТОР — промышленное оборудование}"
FIXTURES_DIR="${FIXTURES_DIR:-$REPO_DIR/contracts/fixtures/vektor}"
POLL_INTERVAL="${POLL_INTERVAL:-2}"
JOB_TIMEOUT="${JOB_TIMEOUT:-120}"
SEED_DB="${SEED_DB:-auto}"
SEARCH_QUERY="${QUERY:-Как выполняется ежесменное обслуживание станка?}"
ANSWER_QUESTION="${QUESTION:-Какие работы входят в ежесменное обслуживание станка ВЕКТОР-16?}"

# ── Вывод ──────────────────────────────────────────────────────────
if [ -t 1 ]; then
  C_RESET=$'\033[0m'; C_INFO=$'\033[36m'; C_OK=$'\033[32m'
  C_WARN=$'\033[33m'; C_ERR=$'\033[31m'; C_STEP=$'\033[1m'
else
  C_RESET=""; C_INFO=""; C_OK=""; C_WARN=""; C_ERR=""; C_STEP=""
fi

step() { printf '%s==>%s %s%s%s\n' "$C_INFO" "$C_RESET" "$C_STEP" "$1" "$C_RESET"; }
ok()   { printf '%s  ok%s %s\n' "$C_OK" "$C_RESET" "$1"; }
warn() { printf '%s  !%s %s\n' "$C_WARN" "$C_RESET" "$1" >&2; }
die()  { printf '%sошибка%s %s\n' "$C_ERR" "$C_RESET" "$1" >&2; exit 1; }

# ── Утилиты ────────────────────────────────────────────────────────
# Разбор JSON делает Python: он есть в проекте всегда, а jq может быть не
# установлен. Выражение передаётся аргументом и вычисляется с именем `d`.
if [ -x "$ROOT_DIR/.venv/bin/python" ]; then PYTHON="$ROOT_DIR/.venv/bin/python"
else PYTHON="${PYTHON_BIN:-python3}"; fi

json_get() {
  "$PYTHON" -c 'import builtins, json, sys
data = json.load(sys.stdin)
safe = {n: getattr(builtins, n) for n in ("len", "chr", "any", "all", "next", "sum",
          "min", "max", "int", "str", "list", "dict", "set", "bool", "sorted",
          "round", "abs", "enumerate")}
print(eval(sys.argv[1], {"__builtins__": {}, **safe}, {"d": data}))' "$1"
}

new_uuid() {
  "$PYTHON" -c 'import uuid; print(uuid.uuid4())'
}

AUTH_ARGS=()
[ -n "$TOKEN" ] && AUTH_ARGS=(-H "Authorization: Bearer $TOKEN")
# Пустой массив под `set -u` в bash 3.2 (системный bash macOS) нельзя
# разворачивать как "${AUTH_ARGS[@]}", поэтому разворачиваем безопасно:
# auth_argv печатает аргументы одной строкой (или пустую строку).
auth_argv() {
  [ "${#AUTH_ARGS[@]}" -eq 0 ] && return 0
  printf '%q ' "${AUTH_ARGS[@]}"
}

# http METHOD PATH [curl args...]
#   -> $HTTP_STATUS (код ответа) и $HTTP_BODY (тело ответа).
# Вызывать БЕЗ подстановки команды: иначе переменные остаются в подshell.
HTTP_STATUS=""
HTTP_BODY=""
BODY_FILE="$(mktemp -t kbseed)"
trap 'rm -f "$BODY_FILE"' EXIT

http() {
  local method="$1" path="$2"; shift 2
  # shellcheck disable=SC2046 # auth_argv возвращает уже экранированные аргументы
  HTTP_STATUS="$(curl -sS -o "$BODY_FILE" -w '%{http_code}' \
    -X "$method" $(auth_argv) "$@" "$API_URL$path" || echo 000)"
  HTTP_BODY="$(cat "$BODY_FILE")"
}

command -v curl >/dev/null 2>&1 || die "не найден curl"
command -v "$PYTHON" >/dev/null 2>&1 || die "нужен python3 для разбора JSON-ответов"

# ── Применение SQL-файлов оператора ────────────────────────────────
# Миграции не создают ни tenants, ни settings (ТЗ 2.2, 5.3). Значения вносит
# оператор; скрипт помогает применить подготовленные им файлы.
# DATABASE_URL из .env — asyncpg URL; psql понимает обычный postgresql://.
database_url_sync() {
  local url="${DATABASE_URL:-}"
  if [ -z "$url" ] && [ -f "$ROOT_DIR/.env" ]; then
    url="$(grep -E '^DATABASE_URL=' "$ROOT_DIR/.env" | tail -n1 | cut -d= -f2-)"
  fi
  printf '%s' "${url/postgresql+asyncpg/postgresql}"
}

psql_client() {
  if [ -n "${PSQL_BIN:-}" ]; then printf '%s' "$PSQL_BIN"; return 0; fi
  if command -v psql >/dev/null 2>&1 && psql --version >/dev/null 2>&1; then
    printf '%s' "psql"; return 0
  fi
  local candidate
  for candidate in psql-18 psql-17 psql-16 psql-15 psql-14; do
    if command -v "$candidate" >/dev/null 2>&1; then printf '%s' "$candidate"; return 0; fi
  done
  return 1
}

# apply_sql_file <файл> <подпись результата>
apply_sql_file() {
  local sql_file="$1" label="$2" url client container
  [ -f "$sql_file" ] || { warn "нет файла $sql_file"; return 1; }
  url="$(database_url_sync)"
  if client="$(psql_client)"; then
    if "$client" "$url" -q -v ON_ERROR_STOP=1 -f "$sql_file" >/dev/null 2>&1; then
      ok "$label применён через $client"
      return 0
    fi
    warn "не удалось применить $sql_file через $client — внесите вручную:"
    warn "    $client \"$url\" -v ON_ERROR_STOP=1 -f scripts/$(basename -- "$sql_file")"
    return 1
  fi
  if command -v docker >/dev/null 2>&1; then
    container="${POSTGRES_CONTAINER:-}"
    if [ -z "$container" ]; then
      container="$(docker ps --format '{{.Names}}' | grep -m1 -E 'postgres' || true)"
    fi
    if [ -n "$container" ] && docker exec -i "$container" \
        psql -U "${POSTGRES_USER:-kb}" -d "${POSTGRES_DB:-kb}" \
        -q -v ON_ERROR_STOP=1 < "$sql_file" >/dev/null 2>&1; then
      ok "$label применён через docker exec $container"
      return 0
    fi
    warn "не удалось применить $sql_file через docker exec (контейнер: ${container:-не найден})"
    return 1
  fi
  warn "нет клиента psql — $label внесите вручную: scripts/$(basename -- "$sql_file")"
  return 1
}

# Локальный tenant нужен БД сразу после migrate: без записи в tenants
# POST /kbs возвращает 500 по внешнему ключу knowledge_bases.tenant_id.
# Список баз при этом отдаётся нормально, поэтому tenant определяется по
# результату создания базы. В рабочем окружении tenant приходит из
# JWT-издателя, и шаг не нужен.
create_kb_or_bootstrap() {
  local attempt
  for attempt in 1 2; do
    http POST "$API_PREFIX/kbs" -H 'Content-Type: application/json' \
      -d "{\"name\":\"$KB_NAME\",\"allowed_user_ids\":[],\"allowed_group_ids\":[]}"
    [ "$HTTP_STATUS" = "201" ] && return 0
    if [ "$attempt" = "1" ] && [ "$SEED_DB" != "0" ] && [ "$HTTP_STATUS" = "500" ]; then
      # Первая попытка упала: почти наверняка нет записи в tenants.
      step "POST /kbs вернул 500 — применяю scripts/seed_tenant.sql и повторяю"
      apply_sql_file "$ROOT_DIR/scripts/seed_tenant.sql" "tenant" || true
      continue
    fi
    return 1
  done
  return 1
}

# ── Настройки базы знаний (ТЗ 2.2, раздел 9) ──────────────────────
# Приложение их не создаёт: если аналитик ещё не вносил значения, помогаем
# применить подготовленный оператором scripts/seed_settings.sql.
ensure_settings() {
  local count
  http GET "$API_PREFIX/settings/diagnostics?kb_id=$1"
  count="$(printf '%s' "$HTTP_BODY" | json_get "len(d['missing_keys'])" 2>/dev/null || echo 1)"
  [ "$count" = "0" ] && { ok "настройки базы уже внесены"; return 0; }

  if [ "$SEED_DB" = "0" ]; then
    warn "настроек не хватает ($count ключей), SEED_DB=0 — продолжу и покажу ошибку"
    return 0
  fi
  step "Настроек не хватает ($count ключей) — применяю scripts/seed_settings.sql"
  apply_sql_file "$ROOT_DIR/scripts/seed_settings.sql" "настройки" || true
}

# ── База знаний ────────────────────────────────────────────────────
resolve_kb() {
  if [ -n "$KB_ID" ]; then
    step "Использую базу знаний из KB_ID"
    http GET "$API_PREFIX/kbs?limit=100"
    if [ "$HTTP_STATUS" = "200" ] && printf '%s' "$HTTP_BODY" | json_get \
        "any(i['kb_id'] == '$KB_ID' for i in d['items'])" | grep -qi '^true$'; then
      ok "база $KB_ID доступна"
      ensure_settings "$KB_ID"
      return 0
    fi
    die "KB_ID=$KB_ID не найден среди доступных баз (проверьте GET /kbs)"
  fi

  step "Ищу базу знаний «${KB_NAME}»"
  http GET "$API_PREFIX/kbs?limit=100"
  [ "$HTTP_STATUS" = "200" ] || die "GET /kbs вернул $HTTP_STATUS: $HTTP_BODY"
  local found
  found="$(printf '%s' "$HTTP_BODY" | json_get \
    "next((i['kb_id'] for i in d['items'] if i['name'] == '''$KB_NAME'''), '')")"
  if [ -n "$found" ] && [ "$found" != "None" ]; then
    KB_ID="$found"
    ok "база уже существует: $KB_ID"
  else
    step "Создаю базу знаний «${KB_NAME}»"
    create_kb_or_bootstrap || die "POST /kbs вернул $HTTP_STATUS: $HTTP_BODY"
    KB_ID="$(printf '%s' "$HTTP_BODY" | json_get "d['kb_id']")"
    ok "создана база $KB_ID (настройки приложение не создаёт — ТЗ 2.2)"
  fi
  ensure_settings "$KB_ID"
}

# ── Загрузка файлов ────────────────────────────────────────────────
wait_for_job() {
  local job_id="$1" label="$2" waited=0 status="" job
  while [ "$waited" -lt "$JOB_TIMEOUT" ]; do
    http GET "$API_PREFIX/jobs/$job_id"
    if [ "$HTTP_STATUS" != "200" ]; then
      warn "GET /jobs/$job_id вернул $HTTP_STATUS: $HTTP_BODY"
      return 1
    fi
    job="$HTTP_BODY"
    status="$(printf '%s' "$job" | json_get "d['status']")"
    local stage done total
    stage="$(printf '%s' "$job" | json_get "d['stage']")"
    done="$(printf '%s' "$job" | json_get "d['processed_chunks']")"
    total="$(printf '%s' "$job" | json_get "d['total_chunks'] or '?'")"
    printf '\r    %s: stage=%s status=%s чанки=%s/%s (%ss)   ' \
      "$label" "$stage" "$status" "$done" "$total" "$waited"
    case "$status" in
      succeeded)
        printf '\r    %s: succeeded, чанков %s                 \n' "$label" "$done"
        return 0 ;;
      failed|cancelled)
        local code msg
        code="$(printf '%s' "$job" | json_get "d['error_code']")"
        msg="$(printf '%s' "$job" | json_get "d['error_message']")"
        printf '\r    %s: %s (%s: %s)             \n' "$label" "$status" "$code" "$msg"
        return 1 ;;
    esac
    sleep "$POLL_INTERVAL"
    waited=$((waited + POLL_INTERVAL))
  done
  printf '\r    %s: таймаут %ss (последний status=%s)         \n' "$label" "$JOB_TIMEOUT" "$status"
  return 1
}

upload_document() {
  local file="$1" external_id="$2" title="$3" source_url="$4"
  local key metadata upload_status body
  key="$(new_uuid)"
  metadata="$(printf '{"title":"%s","external_id":"%s","source_url":"%s","metadata":{"product":"ВЕКТОР","file":"%s"}}' \
    "$title" "$external_id" "$source_url" "$(basename -- "$file")")"

  # shellcheck disable=SC2046 # auth_argv возвращает уже экранированные аргументы
  upload_status="$(curl -sS -o "$BODY_FILE" -w '%{http_code}' -X POST $(auth_argv) \
    -H "Idempotency-Key: $key" \
    -F "file=@${file};type=text/markdown" \
    -F "metadata=${metadata}" \
    "$API_URL$API_PREFIX/kbs/$KB_ID/documents")"
  body="$(cat "$BODY_FILE")"

  case "$upload_status" in
    202)
      local job_id
      job_id="$(printf '%s' "$body" | json_get "d['job_id']")"
      ok "$(basename -- "$file") -> job $job_id (Idempotency-Key $key)"
      wait_for_job "$job_id" "$(basename -- "$file")" || return 1
      ;;
    409)
      # Документ с таким external_id уже есть — идемпотентность повторного запуска.
      local code doc active re re_job
      code="$(printf '%s' "$body" | json_get "d['error']['code']")"
      if [ "$code" != "DOCUMENT_EXISTS" ]; then
        warn "$(basename -- "$file") -> $upload_status $code: $(printf '%s' "$body" | tr -d '\n')"
        return 1
      fi
      http GET "$API_PREFIX/documents?kb_id=$KB_ID&limit=100"
      doc="$(printf '%s' "$HTTP_BODY" | json_get \
        "next((i['document_id'] for i in d['items'] if i['external_id'] == '$external_id'), '')")"
      if [ -z "$doc" ] || [ "$doc" = "None" ]; then
        # ТЗ 5.3: уникальный external_id учитывает и soft-deleted документы,
        # поэтому удалённый документ заново не загрузить. Это ожидаемо.
        warn "$(basename -- "$file") -> DOCUMENT_EXISTS, но среди доступных документов его нет:"
        warn "    документ с external_id=$external_id удалён (soft-delete) и вновь не загружается;"
        warn "    удалите запись из БД вручную либо укажите другой FIXTURES_DIR."
        return 1
      fi
      http GET "$API_PREFIX/documents/$doc"
      active="$(printf '%s' "$HTTP_BODY" | json_get "(d['active_version'] or {}).get('status') or ''")"
      if [ "$active" = "ready" ]; then
        ok "$(basename -- "$file") -> уже загружен и проиндексирован (document $doc)"
        return 0
      fi
      warn "$(basename -- "$file") -> DOCUMENT_EXISTS, документ $doc ещё не готов ($active); запускаю reindex"
      # shellcheck disable=SC2046
      re="$(curl -sS -o "$BODY_FILE" -w '%{http_code}' -X POST $(auth_argv) \
        -H "Idempotency-Key: $(new_uuid)" -H 'Content-Type: application/json' \
        -d '{}' "$API_URL$API_PREFIX/documents/$doc/reindex")"
      [ "$re" = "202" ] || { warn "reindex вернул $re: $(cat "$BODY_FILE")"; return 1; }
      re_job="$(cat "$BODY_FILE" | json_get "d['job_id']")"
      wait_for_job "$re_job" "$(basename -- "$file")/reindex" || return 1
      ;;
    *)
      warn "$(basename -- "$file") -> $upload_status: $(printf '%s' "$body" | tr -d '\n')"
      return 1
      ;;
  esac
}

upload_all() {
  step "Файлы-фикстуры: $FIXTURES_DIR"
  [ -d "$FIXTURES_DIR" ] || die "каталог $FIXTURES_DIR не найден"
  local files
  files="$(find "$FIXTURES_DIR" -maxdepth 1 -type f \( -name '*.md' -o -name '*.docx' -o -name '*.pdf' \) ! -name 'README.md' | sort)"
  [ -n "$files" ] || die "в $FIXTURES_DIR нет файлов .md/.docx/.pdf"

  local failed=0 total=0 file base external_id title
  while IFS= read -r file; do
    [ -n "$file" ] || continue
    total=$((total + 1))
    base="$(basename -- "$file")"
    external_id="${base%.*}"
    title="$(grep -m1 '^# ' "$file" 2>/dev/null | sed 's/^# *//')"
    [ -n "$title" ] || title="$base"
    upload_document "$file" "$external_id" "$title" \
      "https://vektor.example/docs/${base%.*}" || failed=$((failed + 1))
  done <<< "$files"
  printf '\n'
  if [ "$failed" -gt 0 ]; then
    warn "загружено документов: $((total - failed)) из $total, с ошибкой: $failed"
    return 1
  fi
  ok "загружено документов: $total из $total"
}

# ── Проверка поиска и ответов ──────────────────────────────────────
check_search() {
  step "POST /search: «${SEARCH_QUERY}»"
  http POST "$API_PREFIX/search" -H 'Content-Type: application/json' \
    -d "$(printf '{"kb_id":"%s","query":"%s","top_k":5}' "$KB_ID" "$SEARCH_QUERY")"
  if [ "$HTTP_STATUS" != "200" ]; then
    warn "поиск вернул $HTTP_STATUS: $HTTP_BODY"
    return 1
  fi
  printf '%s\n' "$HTTP_BODY"
  local n status
  n="$(printf '%s' "$HTTP_BODY" | json_get "len(d['hits'])")"
  status="$(printf '%s' "$HTTP_BODY" | json_get "d['status']")"
  if [ "$n" -eq 0 ]; then
    warn "поиск вернул status=$status без результатов"
    return 1
  fi
  ok "status=$status, найдено фрагментов: $n"
  printf '%s\n' "$HTTP_BODY" | json_get \
    "chr(10).join('    [%d] %s (score=%.6f) %s' % (i, h['title'][:60], h['score'], h['locator']['section_path']) for i, h in enumerate(d['hits'], 1))"
}

check_answers() {
  step "POST /answers: «${ANSWER_QUESTION}»"
  http POST "$API_PREFIX/answers" -H 'Content-Type: application/json' \
    -d "$(printf '{"kb_id":"%s","question":"%s"}' "$KB_ID" "$ANSWER_QUESTION")"
  if [ "$HTTP_STATUS" != "200" ]; then
    warn "ответы вернули $HTTP_STATUS: $HTTP_BODY"
    return 1
  fi
  printf '%s\n' "$HTTP_BODY"
  local status answer cites
  status="$(printf '%s' "$HTTP_BODY" | json_get "d['status']")"
  answer="$(printf '%s' "$HTTP_BODY" | json_get "d['answer']")"
  cites="$(printf '%s' "$HTTP_BODY" | json_get "len(d['citations'])")"
  ok "status=$status, цитат: $cites"
  printf '    ответ: %s\n' "$answer"
  printf '%s\n' "$HTTP_BODY" | json_get \
    "chr(10).join('    [%d] %s -> %s' % (c['number'], c['title'][:50], c['quote'][:80].replace(chr(10), ' ')) for c in d['citations'])"
  if [ "$cites" -eq 0 ]; then
    warn "ответ без цитат — проверьте настройки answer.* и tools.enabled"
    return 1
  fi
}

# ── Основной сценарий ──────────────────────────────────────────────
main() {
  step "Проверяю доступность API $API_URL"
  http GET "$HEALTH_PREFIX/live"
  [ "$HTTP_STATUS" = "200" ] || die "GET /health/live вернул $HTTP_STATUS: $HTTP_BODY"
  ok "health/live: $HTTP_BODY"
  http GET "$HEALTH_PREFIX/ready"
  if [ "$HTTP_STATUS" = "200" ]; then
    ok "health/ready: $(printf '%s' "$HTTP_BODY" | tr -d '\n')"
  else
    warn "health/ready вернул $HTTP_STATUS — поиск и ответы, скорее всего, не заработают: $HTTP_BODY"
  fi

  printf '%s\n' "AI-поиск по базе знаний — демо-загрузка «ВЕКТОР»"
  printf '%s\n\n' "API: $API_URL   фикстуры: $FIXTURES_DIR"

  resolve_kb
  upload_all
  check_search
  check_answers
  printf '\n%sГотово.%s База знаний: %s\n' "$C_OK" "$C_RESET" "$KB_ID"
  printf 'Повторный запуск в ту же базу:  KB_ID=%s ./scripts/seed_kb.sh\n' "$KB_ID"
}

main "$@"