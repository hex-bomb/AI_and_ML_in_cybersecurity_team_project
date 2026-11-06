#!/usr/bin/env bash
# Восстановление из копии, снятой backup.sh.
#
#   ./infra/scripts/restore.sh backups/20261103-120000
#
# Операция разрушительная: текущие базы и оригиналы заменяются содержимым
# копии. Поэтому сервисы останавливаются, а запуск подтверждается отдельно.
set -euo pipefail
cd "$(dirname "$0")/../.."

DIR=${1:?укажите каталог копии: ./infra/scripts/restore.sh backups/<метка>}
[ -f "$DIR/manifest.txt" ] || {
  echo ">> в $DIR нет manifest.txt: это не копия backup.sh" >&2
  exit 1
}

# Копия могла доехать по сети: сверяем контрольные суммы до остановки сервисов.
if [ -f "$DIR/SHA256SUMS" ]; then
  ( cd "$DIR" && shasum -a 256 -c SHA256SUMS >/dev/null ) || {
    echo ">> контрольные суммы не совпали: копия повреждена" >&2
    exit 1
  }
  echo ">> контрольные суммы в порядке"
fi

if [ "${RESTORE_CONFIRM:-}" != "yes" ]; then
  read -r -p "Текущие данные будут заменены копией из $DIR. Продолжить? [yes/no] " answer
  [ "$answer" = "yes" ] || {
    echo ">> отменено"
    exit 1
  }
fi

echo ">> останавливаю сервисы"
docker compose stop answer documents retrieval ingest router >/dev/null

for db in vektor_auth vektor_docs vektor_answer; do
  echo ">> $db"
  docker compose exec -T postgres psql -U vektor -d postgres \
    -c "drop database if exists $db with (force)" \
    -c "create database $db owner vektor" >/dev/null
  docker compose exec -T postgres pg_restore -U vektor -d "$db" --no-owner --no-privileges \
    <"$DIR/$db.dump"
done

echo ">> оригиналы"
# /data/blobs — точка монтирования тома: удалять её нельзя, чистим содержимое.
docker compose run --rm --no-deps -T --entrypoint sh documents \
  -c 'find /data/blobs -mindepth 1 -delete && tar -C /data -xf -' <"$DIR/blobs.tar"

echo ">> запускаю сервисы"
docker compose up -d --wait answer documents retrieval ingest router >/dev/null

# Elasticsearch в копию не входит: он производный, и после восстановления его
# нужно собрать заново, иначе выдача будет от прежнего состояния базы.
echo ">> пересобираю поисковый индекс"
"$(dirname "$0")/reindex.sh"

echo ">> копия восстановлена; проверка: ./infra/scripts/doctor.sh"
