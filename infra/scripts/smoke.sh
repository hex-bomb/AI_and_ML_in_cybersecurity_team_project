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
  -d '{"query":"гарантия","top_k":3}' \
  "$BASE/api/v1/search"
echo

echo -n "answer:   "
code -b "$JAR" -H 'Content-Type: application/json' \
  -d '{"question":"какая гарантия на станки?"}' \
  "$BASE/api/v1/answers"
echo

echo -n "jwks:     "
code "$BASE/.well-known/jwks.json"
echo

rm -f "$JAR"
