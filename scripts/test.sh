#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

ruff check apps/backend infra/scripts
ruff format --check apps/backend infra/scripts
python3 -m pytest apps/backend/answer/tests -q
python3 scripts/check-contracts.py
( cd apps/backend/retrieval && go build ./... && go vet ./... && test -z "$(gofmt -l .)" )
