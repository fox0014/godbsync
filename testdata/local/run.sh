#!/usr/bin/env bash
# =============================================================================
# 本地一键复现某个同步方向。
#
#   ./testdata/local/run.sh oracle2mysql
#   ./testdata/local/run.sh pg2oracle
#   ./testdata/local/run.sh down              # 停止并清理容器
#   ./testdata/local/run.sh list              # 列出可跑的方向
#
# 与 GitHub Actions 走同一套 testdata/ci/dbctl.sh，差异只有端口与容器名，
# 所以 CI 通过的方向，本地一定也能通过（反之亦然）。
#
# 环境变量：
#   DB_PASS       数据库口令（默认与 workflow 一致）
#   SKIP_SEED=1   跳过灌数据（表里已有数据时省时间）
#   STAGE=init|full|validate  只跑某一阶段
# =============================================================================
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$ROOT"

DB_PASS="${DB_PASS:-SyncCi2024pw}"
COMPOSE=(docker compose -f testdata/local/docker-compose.yml)
PROJECT=godbsync-local
WORK="$ROOT/testdata/local/.work"
DBCTL="$ROOT/testdata/ci/dbctl.sh"

DIALECTS=(mysql postgresql sqlserver oracle)

usage() {
  cat <<'EOF'
用法: bash bash testdata/local/run.sh <源方言>2<目标方言>

示例:
  testdata/local/run.sh mysql2mysql
  testdata/local/run.sh oracle2sqlserver

辅助命令:
  list     列出全部 12 个合法方向
  down     停止并删除本地容器
EOF
}

# 方向名 → 该方言在本地方案里对应的 compose service / 端口 / 账号 / 库 / service / schema
# 输出: svc|port|user|db|service|schema
slot() {
  local dialect=$1 side=$2 db
  if [ "$side" = src ]; then db=srcdb; else db=dstdb; fi
  case "$dialect" in
    mysql)      printf 'mysql%s|%s|root|%s||\n' \
                  "$([ "$side" = src ] && echo 1 || echo 2)" \
                  "$([ "$side" = src ] && echo 33061 || echo 33062)" "$db" ;;
    postgresql) printf 'pg|5433|postgres|%s||public\n' "$db" ;;
    sqlserver)  printf 'mssql|14330|sa|%s||dbo\n' "$db" ;;
    oracle)     printf 'oracle|15210|GODBSYNC||FREEPDB1|GODBSYNC\n' ;;
    *)          return 1 ;;
  esac
}

cmd_list() {
  local a b
  for a in "${DIALECTS[@]}"; do
    for b in "${DIALECTS[@]}"; do
      echo "${a}2${b}"
    done
  done
}

cmd_down() {
  "${COMPOSE[@]}" down -v
  rm -rf "$WORK"
}

case "${1:-}" in
  ""|-h|--help) usage; exit 0 ;;
  list)         cmd_list; exit 0 ;;
  down)         cmd_down; exit 0 ;;
esac

DIRECTION=$1
case "$DIRECTION" in
  *2*) ;;
  *) echo "方向名必须形如 <源>2<目标>，例如 oracle2mysql" >&2; exit 2 ;;
esac
SRC=${DIRECTION%%2*}
DST=${DIRECTION##*2}
slot "$SRC" src >/dev/null || { echo "未知源方言: $SRC" >&2; exit 2; }
slot "$DST" dst >/dev/null || { echo "未知目标方言: $DST" >&2; exit 2; }

IFS='|' read -r S_SVC S_PORT S_USER S_DB S_SERVICE S_SCHEMA <<< "$(slot "$SRC" src)"
IFS='|' read -r D_SVC D_PORT D_USER D_DB D_SERVICE D_SCHEMA <<< "$(slot "$DST" dst)"
S_CID="$PROJECT-$S_SVC-1"
D_CID="$PROJECT-$D_SVC-1"

echo "==> 方向 $SRC → $DST"
echo "    源   $S_CID  :$S_PORT  $S_USER@${S_DB:-$S_SERVICE}"
echo "    目标 $D_CID  :$D_PORT  $D_USER@${D_DB:-$D_SERVICE}"

# dbctl.sh 的参数全部走环境变量，这里做一层薄封装，避免每次重复 6 个变量。
dbctl() {  # dbctl <dialect> <cid> <user> <db> <service> <子命令> [参数...]
  local dialect=$1 cid=$2 user=$3 db=$4 service=$5
  shift 5
  DB_DIALECT="$dialect" DB_CID="$cid" DB_USER="$user" DB_PASS="$DB_PASS" \
  DB_NAME="$db" DB_SERVICE="$service" "$DBCTL" "$@"
}

mkdir -p "$WORK"

echo "==> 启动容器（$S_SVC / $D_SVC）"
"${COMPOSE[@]}" up -d "$S_SVC" "$D_SVC"

echo "==> 等待源库并就绪灌数据"
dbctl "$SRC" "$S_CID" "$S_USER" "$S_DB" "$S_SERVICE" wait
dbctl "$SRC" "$S_CID" "$S_USER" "$S_DB" "$S_SERVICE" createdb
if [ "${SKIP_SEED:-0}" != "1" ]; then
  dbctl "$SRC" "$S_CID" "$S_USER" "$S_DB" "$S_SERVICE" file "testdata/seed/$SRC.sql"
fi

echo "==> 等待目标库"
dbctl "$DST" "$D_CID" "$D_USER" "$D_DB" "$D_SERVICE" wait
dbctl "$DST" "$D_CID" "$D_USER" "$D_DB" "$D_SERVICE" createdb

CONFIG="$WORK/config.$DIRECTION.yaml"
cat > "$CONFIG" <<EOF
source:
  dialect: $SRC
  host: 127.0.0.1
  port: $S_PORT
  username: "$S_USER"
  password: "$DB_PASS"
  service: "$S_SERVICE"
  database: "$S_DB"
  schema: "$S_SCHEMA"
  charset: utf8mb4
  timezone: UTC

target:
  dialect: $DST
  host: 127.0.0.1
  port: $D_PORT
  username: "$D_USER"
  password: "$DB_PASS"
  service: "$D_SERVICE"
  database: "$D_DB"
  schema: "$D_SCHEMA"

state:
  backend: sqlite
  path: $WORK/state.$DIRECTION.db

workers: 4
log_level: info
auto_validate: true
validate_sample_size: 200
batch_size: 400
batch_timeout: 120s

tables:
  - { name: T_ANCHOR,   mode: full, truncate: true }
  - { name: T_TYPES,    mode: full, truncate: true }
  - { name: T_RESERVED, mode: full, truncate: true }
  - { name: T_NOPK,     mode: full, truncate: true }
  - { name: T_EMPTY,    mode: full, truncate: true }
EOF
echo "==> 配置生成: $CONFIG"

echo "==> 构建"
go build -ldflags="-s -w" -o "$WORK/godbsync" .

run_stage() {
  echo "==> $1"
  "$WORK/godbsync" "$1" -config "$CONFIG"
}

case "${STAGE:-}" in
  init)     run_stage init ;;
  full)     run_stage full ;;
  validate) run_stage validate ;;
  *)
    run_stage init
    run_stage full
    run_stage validate
    ;;
esac

echo
echo "==> 通过: $DIRECTION"
echo "    重新查看状态: $WORK/godbsync monitor -cli -config $CONFIG --verify"
echo "    清理环境:     testdata/local/run.sh down"
