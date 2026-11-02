#!/usr/bin/env bash
# Создаёт .env из .env.example и генерирует секреты ровно один раз:
# значения живут в .env (gitignored), повторные запуски существующие ключи не
# перезаписывают.
set -euo pipefail
cd "$(dirname "$0")/../.."

# JVM ждёт PKCS#8 (SEQUENCE {version, AlgorithmIdentifier, key}). На macOS
# LibreSSL игнорирует `-outform DER` у genpkey и выдаёт PKCS#1, поэтому берём
# base64 тела PEM: genpkey и там, и на OpenSSL 3 пишет PKCS#8.
gen_signing_key() {
  openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 2>/dev/null \
    | awk '/BEGIN PRIVATE KEY/{p=1;next} /END PRIVATE KEY/{p=0} p' \
    | tr -d '\n'
}

gen_token() {
  openssl rand -hex 24
}

[ -f .env ] || cp .env.example .env

# Пустое или отсутствующее значение → генерируем. Ключ всегда занимает одну
# строку: docker compose читает первое вхождение, а не последнее.
ensure() {
  local key="$1" generator="$2" value
  value="$(grep -E "^${key}=.+" .env | tail -n 1 | cut -d= -f2- || true)"
  if [ -z "$value" ]; then
    value="$("$generator")"
    echo ">> .env: сгенерирован $key"
  fi
  awk -v k="${key}=" -v v="${key}=${value}" '
    BEGIN { done = 0 }
    index($0, k) == 1 { if (!done) print v; done = 1; next }
    { print }
  ' .env >.env.tmp
  mv .env.tmp .env
}

ensure AUTH_SIGNING_KEY_B64 gen_signing_key
ensure DOCUMENTS_INTERNAL_TOKEN gen_token

echo ">> .env готов"
