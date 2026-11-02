#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

ruff check apps/backend infra/scripts
ruff format --check apps/backend infra/scripts
( cd apps/backend/retrieval && test -z "$(gofmt -l .)" )
semgrep --config .semgrep --severity=ERROR --error --metrics=off
