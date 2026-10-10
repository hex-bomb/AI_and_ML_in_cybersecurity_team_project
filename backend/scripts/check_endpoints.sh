#!/usr/bin/env bash
# Проверка всех эндпоинтов и сценариев публичного API.
#
# Скрипт не падает на первом неудачном ответе: он собирает полную картину и
# печатает итог в конце. Иначе при десятке маршрутов пришлось бы чинить по
# одному, не видя общей картины.
#
# Требуется поднятый стенд (по умолчанию http://127.0.0.1:8099).
set -uo pipefail

API="${API:-http://127.0.0.1:8099}"
V1="$API/api/v1"
PASS=0
FAIL=0
FAILED_ROUTES=()

# check <имя> <ожидаемый код> <метод> <путь> [тело]
check() {
  local name="$1" want="$2" method="$3" path="$4" body="${5:-}"
  local out code
  if [[ -n "$body" ]]; then
    out=$(curl -s -o /tmp/_body -w '%{http_code}' -X "$method" "$path" \
      -H 'Content-Type: application/json' -H 'X-Request-ID: 11111111-2222-4333-8444-555555555555' \
      -d "$body")
  else
    out=$(curl -s -o /tmp/_body -w '%{http_code}' -X "$method" "$path" \
      -H 'X-Request-ID: 11111111-2222-4333-8444-555555555555')
  fi
  code="$out"
  if [[ "$code" == "$want" ]]; then
    PASS=$((PASS + 1))
    printf '  \033[32mok\033[0m   %-52s %s\n' "$name" "$code"
  else
    FAIL=$((FAIL + 1))
    FAILED_ROUTES+=("$name (ждали $want, получили $code)")
    printf '  \033[31mFAIL\033[0m %-52s %s\n' "$name" "$code"
    head -c 200 /tmp/_body; echo
  fi
}

# envelope проверяет общий формат ошибки ТЗ 2.3.
envelope() {
  python3 - <<'PY'
import json, sys
try:
    d = json.load(open('/tmp/_body'))
except Exception as exc:
    print(f"  тело не JSON: {exc}"); sys.exit(1)
if 'error' not in d:
    print("  нет поля error"); sys.exit(1)
for key in ('code', 'message'):
    if key not in d['error']:
        print(f"  в error нет {key}"); sys.exit(1)
if 'request_id' not in d:
    print("  нет request_id"); sys.exit(1)
print(f"  код={d['error']['code']} request_id={d['request_id']}")
PY
}

# query_string возвращает значение поля из ответа.
field() {
  python3 -c "import json;print(json.load(open('/tmp/_body'))$1)"
}

echo "== health =="
check "GET /health/live"            200 GET "$API/health/live"
check "GET /health/ready"           200 GET "$API/health/ready"
echo -n "  ключи checks: "; python3 -c "import json;print(sorted(json.load(open('/tmp/_body'))['checks']))"

echo
echo "== базы знаний =="
check "POST /kbs"                   201 POST "$V1/kbs" '{"name":"Проверка контракта"}'
KB=$(field "['kb_id']")
echo "  создана база: $KB"
check "POST /kbs с лишним полем"    422 POST "$V1/kbs" '{"name":"Лишнее","выдумка":1}'
check "GET  /kbs"                   200 GET  "$V1/kbs?limit=5"
# name необязателен: без него база получает имя по умолчанию.
check "POST /kbs без имени"         201 POST "$V1/kbs" '{}'
check "GET  /kbs несуществующий"    404 GET  "$V1/kbs/99999999-9999-4999-8999-999999999999"
echo -n "  формат ошибки: "; envelope

echo
echo "== документы =="
printf '# Проверка\n\nМомент затяжки муфты 45 Нм.\n' > /tmp/_doc.md
# metadata.title обязателен: конвертер берёт его оттуда, а не из имени файла.
UPLOAD=$(curl -s -o /tmp/_body -w '%{http_code}' -X POST "$V1/kbs/$KB/documents" \
  -H 'Idempotency-Key: 11111111-1111-4111-8111-111111111111' \
  -F 'metadata={"title":"Проверка контракта"};type=application/json' \
  -F "file=@/tmp/_doc.md;type=text/markdown")
DOC=$(python3 -c "import json;d=json.load(open('/tmp/_body'));print(d.get('document_id',''))" 2>/dev/null)
if [[ "$UPLOAD" == "202" || "$UPLOAD" == "201" ]]; then
  PASS=$((PASS+1)); printf '  \033[32mok\033[0m   %-52s %s\n' "POST /kbs/{id}/documents" "$UPLOAD"
else
  FAIL=$((FAIL+1)); printf '  \033[31mFAIL\033[0m %-52s %s\n' "POST /kbs/{id}/documents" "$UPLOAD"; head -c 200 /tmp/_body; echo
fi
JOB=$(python3 -c "import json;d=json.load(open('/tmp/_body'));print(d.get('job_id',''))" 2>/dev/null)
echo "  документ: $DOC, задание: $JOB"
for _ in $(seq 1 40); do
  ST=$(curl -s "$V1/jobs/$JOB" | python3 -c "import json,sys;print(json.load(sys.stdin).get('status',''))" 2>/dev/null)
  [[ "$ST" == "succeeded" || "$ST" == "failed" ]] && break
  sleep 2
done
echo "  статус задания: $ST"
check "GET /documents"              200 GET "$V1/documents?kb_id=$KB&limit=5"
if [[ -n "$DOC" ]]; then
  check "GET /documents/{id}"         200 GET "$V1/documents/$DOC"
  check "GET /documents/{id}/source"  200 GET "$V1/documents/$DOC/source"
  check "GET /documents несуществующий" 404 GET "$V1/documents/99999999-9999-4999-8999-999999999999"
fi
check "GET /jobs несуществующее"     404 GET "$V1/jobs/99999999-9999-4999-8999-999999999999"
echo -n "  формат ошибки: "; envelope

echo
echo "== поиск и ответы =="
check "POST /search"                 200 POST "$V1/search" "{\"kb_id\":\"$KB\",\"query\":\"момент затяжки\",\"top_k\":3}"
check "POST /answers"                200 POST "$V1/answers" "{\"kb_id\":\"$KB\",\"question\":\"Какой момент затяжки у муфты?\"}"
echo -n "  цитат в ответе: "; python3 -c "import json;print(len(json.load(open('/tmp/_body')).get('citations',[])))"
check "POST /search без kb_id"       422 POST "$V1/search" '{"query":"что угодно"}'
check "POST /search пустой вопрос"   422 POST "$V1/search" "{\"kb_id\":\"$KB\",\"query\":\"\"}"
# Чужая база отвергается на входе (kbs.Require): опечатка в kb_id даёт
# 404, а не пустой результат, как /settings/diagnostics на той же базе.
check "POST /search  чужой kb"  404 POST "$V1/search" '{"kb_id":"99999999-9999-4999-8999-999999999999","query":"х"}'
check "POST /answers чужой kb"  404 POST "$V1/answers" '{"kb_id":"99999999-9999-4999-8999-999999999999","question":"х"}'
check "GET  /settings/diagnostics чужой kb"  404 GET "$V1/settings/diagnostics?kb_id=99999999-9999-4999-8999-999999999999"

echo
echo "== диагностика =="
check "GET /settings/diagnostics"    200 GET "$V1/settings/diagnostics?kb_id=$KB"
check "GET /openapi.json"            200 GET "$V1/openapi.json"

echo
echo "== заголовки =="
XREQ=$(curl -s -D - -o /dev/null "$V1/kbs?limit=1" \
  -H 'X-Request-ID: 11111111-2222-4333-8444-555555555555' | grep -i '^x-request-id' | tr -d '\r')
echo "  $XREQ"
if [[ "$XREQ" == *"11111111-2222-4333-8444-555555555555"* ]]; then
  PASS=$((PASS+1)); echo "  \033[32mok\033[0m   валидный входящий X-Request-ID сохранён"
else
  FAIL=$((FAIL+1)); echo "  \033[31mFAIL\033[0m валидный входящий X-Request-ID НЕ сохранён"
fi

echo
echo "=========================================="
echo "  успешно: $PASS, неудачно: $FAIL"
if (( FAIL > 0 )); then
  echo "  не прошли:"
  for route in ${FAILED_ROUTES[@]+"${FAILED_ROUTES[@]}"}; do
    echo "    - $route"
  done
  exit 1
fi
echo "  все проверки прошли"
