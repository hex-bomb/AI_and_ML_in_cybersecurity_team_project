#!/usr/bin/env bash
# Проверяет публичный контур: вход, поиск, ответ, JWKS.
set -euo pipefail
BASE="http://localhost:8080"
JAR=$(mktemp)

code() { curl -sS -o /dev/null -w '%{http_code}' "$@"; }

echo -n "login:    "
code -c "$JAR" -H 'Content-Type: application/json' \
  -d '{"username":"employee","password":"employee"}' "$BASE/api/v1/auth/login"
echo

echo -n "search:   "
code -b "$JAR" -H 'Content-Type: application/json' \
  -d '{"kb_id":"74ade984-591f-4382-a051-ef1898855868","query":"гарантия","top_k":3}' \
  "$BASE/api/v1/search"
echo

echo -n "answer:   "
code -b "$JAR" -H 'Content-Type: application/json' \
  -d '{"kb_id":"74ade984-591f-4382-a051-ef1898855868","question":"какая гарантия на станки?"}' \
  "$BASE/api/v1/answers"
echo

echo -n "jwks:     "
code "$BASE/.well-known/jwks.json"
echo

rm -f "$JAR"
