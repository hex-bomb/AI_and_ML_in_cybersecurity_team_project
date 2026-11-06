#!/usr/bin/env bash
# Резервная копия установки: три базы и оригиналы документов.
#
#   ./infra/scripts/backup.sh [каталог]
#
# Без оригиналов копия неполная: в базе только метаданные и storage_key, а сам
# файл лежит в томе docs-blobs. Скрипт снимает и то, и другое плюс контрольные
# суммы, чтобы восстановление можно было проверить до того, как оно понадобится.
set -euo pipefail
cd "$(dirname "$0")/../.."

DIR=${1:-backups/$(date +%Y%m%d-%H%M%S)}
mkdir -p "$DIR"

echo ">> каталог: $DIR"

for db in vektor_auth vektor_docs vektor_answer; do
  docker compose exec -T postgres pg_dump -U vektor -d "$db" --format=custom >"$DIR/$db.dump"
  echo "   $db: $(du -h "$DIR/$db.dump" | cut -f1)"
done

# Тот же том, что смонтирован в documents: читаем его отдельным контейнером,
# чтобы не зависеть от того, что сервис сейчас запущен.
docker compose run --rm --no-deps -T --entrypoint sh documents \
  -c 'tar -C /data -cf - blobs' >"$DIR/blobs.tar"
echo "   blobs.tar: $(du -h "$DIR/blobs.tar" | cut -f1)"

{
  echo "created_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "git_commit=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
  docker compose images --format '{{.Service}}={{.ID}}' 2>/dev/null || true
} >"$DIR/manifest.txt"

( cd "$DIR" && shasum -a 256 ./*.dump ./blobs.tar >SHA256SUMS )

echo ">> готово: $(ls -1 "$DIR" | tr '\n' ' ')"
echo ">> восстановление: ./infra/scripts/restore.sh $DIR"
