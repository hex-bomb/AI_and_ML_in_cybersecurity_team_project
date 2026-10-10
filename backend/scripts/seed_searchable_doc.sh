#!/usr/bin/env bash
set -uo pipefail
API="${API:-http://127.0.0.1:8099}"
KB="${KB:-74ade984-591f-4382-a051-ef1898855868}"
TENANT=11111111-1111-4111-8111-111111111111
TITLE="${1:-}"; FILE="${2:-}"
if [[ -z "$TITLE" || -z "$FILE" ]]; then
  echo "usage: $0 'Заголовок' file.md [kb_id]"; exit 2
fi
if [[ -n "${3:-}" ]]; then KB="$3"; fi
DOC=$(python3 -c 'import uuid;print(uuid.uuid4())')
VER=$(python3 -c 'import uuid;print(uuid.uuid4())')
JOB=$(python3 -c 'import uuid;print(uuid.uuid4())')
TEXT=$(cat "$FILE")
python3 - "$DOC" "$VER" "$TENANT" "$KB" "$TITLE" "$FILE" <<'PY'
import uuid, json, hashlib, sys
doc, ver, tenant, kb, title, path = sys.argv[1:7]
text = open(path, encoding="utf-8").read()
paras = [p.strip() for p in text.split("\n\n") if p.strip()]
blocks = [{"block_id": str(uuid.uuid4()), "position": i, "kind": "paragraph",
           "text": p, "section_path": [], "page": None} for i, p in enumerate(paras)]
parsed = {"schema_version": "1.0", "tenant_id": tenant, "kb_id": kb,
 "document_id": doc, "version_id": ver, "title": title,
 "filename": path.split("/")[-1],
 "content_sha256": hashlib.sha256(text.encode()).hexdigest(),
 "source_url": None, "metadata": {}, "blocks": blocks}
open("/tmp/_parsed.json", "w", encoding="utf-8").write(json.dumps(parsed, ensure_ascii=False))
PY
docker run --rm -v kbsearch_files_data:/v -v /tmp/_parsed.json:/s/parsed.json -v "$FILE:/s/original.bin" alpine \
  sh -c "mkdir -p /v/$TENANT/$VER && cp /s/parsed.json /v/$TENANT/$VER/parsed.json && cp /s/original.bin /v/$TENANT/$VER/original.bin && chown -R 65532:65532 /v/$TENANT"
docker compose exec -T postgres psql -U kb -d kb -v ON_ERROR_STOP=1 -q <<SQL
INSERT INTO documents (id, tenant_id, kb_id, external_id, created_at) VALUES ('$DOC','$TENANT','$KB',NULL,now());
INSERT INTO document_versions (id, tenant_id, document_id, filename, title, source_url, metadata, content_sha256, original_key, parsed_key, status, created_at) VALUES ('$VER','$TENANT','$DOC','$TITLE','$TITLE',NULL,'{}','x','$TENANT/$VER/original.bin','$TENANT/$VER/parsed.json','parsed',now());
UPDATE documents SET latest_version_id='$VER' WHERE id='$DOC';
INSERT INTO jobs (id, tenant_id, document_id, version_id, kind, stage, status, attempt, next_attempt_at, created_at, updated_at) VALUES ('$JOB','$TENANT','$DOC','$VER','ingest','index','queued',0,now(),now(),now());
SQL
echo "document=$DOC version=$VER job=$JOB"
echo "статус: docker compose exec -T postgres psql -U kb -d kb -t -c \"select status from jobs where id='$JOB';\""
