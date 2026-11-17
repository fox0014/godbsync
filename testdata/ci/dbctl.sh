#!/usr/bin/env bash
# =============================================================================
# godbsync CI / 本地复现共用的数据库操作封装。
#
# 设计目标：把「四个方言各自的客户端差异」全部收敛到这一个文件里，
# 让 GitHub Actions workflow 与本地 docker-compose 走完全相同的代码路径 ——
# CI 绿了就说明本地也绿，不会出现「CI 能跑、本地不行」的双份真相。
#
# 用法（全部通过环境变量传参）：
#   DB_DIALECT   mysql | postgresql | sqlserver | oracle
#   DB_CID       容器 ID 或容器名
#   DB_USER      用户名
#   DB_PASS      密码
#   DB_NAME      数据库名（Oracle 留空）
#   DB_SERVICE   Oracle 的 service name（其他方言留空）
#
# 子命令：
#   wait            阻塞直到数据库真正可接受查询（不是只看端口通）
#   exec    <sql>   执行一条 SQL（-e 形式，单语句）
#   file    <path>  执行宿主机的 SQL 文件
#   createdb        幂等建库（仅 sqlserver 需要；mysql/pg 由官方镜像的 env 自动建）
#
# 退出码：0 成功；非 0 失败（sqlplus 的 ORA- 错误也会被捕获，见 run_file_oracle）
# =============================================================================
set -euo pipefail

: "${DB_DIALECT:?缺少 DB_DIALECT}"
: "${DB_CID:?缺少 DB_CID}"
: "${DB_USER:?缺少 DB_USER}"
: "${DB_PASS:?缺少 DB_PASS}"
DB_NAME="${DB_NAME:-}"
DB_SERVICE="${DB_SERVICE:-}"
DB_TIMEOUT="${DB_TIMEOUT:-240}"

MSSQL_SQLCMD=/opt/mssql-tools18/bin/sqlcmd

log() { printf '[dbctl:%s] %s\n' "$DB_DIALECT" "$*" >&2; }

# 容器内执行客户端命令；统一注入字符集环境，避免中文在传输链路上被转码。
dexec() {
  case "$DB_DIALECT" in
    oracle)
      docker exec -e NLS_LANG=AMERICAN_AMERICA.AL32UTF8 "$DB_CID" "$@"
      ;;
    postgresql)
      docker exec -e PGCLIENTENCODING=UTF8 "$DB_CID" "$@"
      ;;
    *)
      docker exec "$DB_CID" "$@"
      ;;
  esac
}

# 把宿主机 SQL 文件送进容器。sqlcmd 不支持从 stdin 读 -i，统一走文件更省心。
put_file() {
  local src=$1
  docker cp "$src" "$DB_CID:/tmp/godbsync_ci.sql" >/dev/null
  echo /tmp/godbsync_ci.sql
}

# ---------------------------------------------------------------------------
# 就绪探测：必须能真正跑通一条查询，端口通不算数。
# Oracle 尤其重要 —— 监听先起来，PDB 可能还没开。
# ---------------------------------------------------------------------------
probe() {
  case "$DB_DIALECT" in
    mysql)
      dexec mysqladmin ping -h 127.0.0.1 -u"$DB_USER" -p"$DB_PASS" --silent >/dev/null 2>&1
      ;;
    postgresql)
      dexec pg_isready -h 127.0.0.1 -U "$DB_USER" -d "$DB_NAME" >/dev/null 2>&1
      ;;
    sqlserver)
      dexec "$MSSQL_SQLCMD" -S 127.0.0.1 -U "$DB_USER" -P "$DB_PASS" -C -l 3 -Q "SELECT 1" >/dev/null 2>&1
      ;;
    oracle)
      dexec bash -c "echo 'select 1 from dual;' | sqlplus -s -L $DB_USER/$DB_PASS@//127.0.0.1:1521/$DB_SERVICE" >/dev/null 2>&1
      ;;
    *)
      log "未知方言，无法探测"; return 1
      ;;
  esac
}

cmd_wait() {
  local start=$SECONDS
  log "等待就绪（上限 ${DB_TIMEOUT}s）"
  until probe; do
    if (( SECONDS - start >= DB_TIMEOUT )); then
      log "等待超时，抓取容器最近日志："
      docker logs --tail 40 "$DB_CID" >&2 || true
      return 1
    fi
    sleep 3
  done
  log "就绪，耗时 $(( SECONDS - start ))s"
}

# ---------------------------------------------------------------------------
# 建库：
#   sqlserver 官方镜像不会自动建库，必须显式 CREATE DATABASE；
#   postgresql 的 POSTGRES_DB 只建一个库，本地复现时同一个容器要承载
#   源库和目标库两个库，故这里也做幂等补建；
#   mysql 靠 MYSQL_DATABASE、oracle 靠 APP_USER，均无需处理。
# ---------------------------------------------------------------------------
cmd_createdb() {
  case "$DB_DIALECT" in
    sqlserver)
      dexec "$MSSQL_SQLCMD" -S 127.0.0.1 -U "$DB_USER" -P "$DB_PASS" -C -b \
        -Q "IF DB_ID('$DB_NAME') IS NULL CREATE DATABASE [$DB_NAME];" >/dev/null
      log "确保数据库 $DB_NAME 存在"
      ;;
    postgresql)
      local exists
      exists=$(dexec psql -U "$DB_USER" -d postgres -tAc \
        "SELECT 1 FROM pg_database WHERE datname = '$DB_NAME'")
      if [ "$exists" != "1" ]; then
        dexec psql -U "$DB_USER" -d postgres -c "CREATE DATABASE \"$DB_NAME\"" >/dev/null
        log "创建数据库 $DB_NAME"
      else
        log "数据库 $DB_NAME 已存在"
      fi
      ;;
    *)
      log "该方言的库由容器初始化，跳过 createdb"
      ;;
  esac
}

# ---------------------------------------------------------------------------
# 执行 SQL 文件
# ---------------------------------------------------------------------------
cmd_file() {
  local path=$1
  [ -f "$path" ] || { log "SQL 文件不存在: $path"; return 1; }
  local inside
  inside=$(put_file "$path")

  case "$DB_DIALECT" in
    mysql)
      dexec mysql --default-character-set=utf8mb4 -u"$DB_USER" -p"$DB_PASS" "$DB_NAME" -e "source $inside"
      ;;
    postgresql)
      dexec psql -v ON_ERROR_STOP=1 -q -U "$DB_USER" -d "$DB_NAME" -f "$inside"
      ;;
    sqlserver)
      dexec "$MSSQL_SQLCMD" -S 127.0.0.1 -U "$DB_USER" -P "$DB_PASS" -C -b -f 65001 \
        -d "$DB_NAME" -i "$inside"
      ;;
    oracle)
      # sqlplus 对 SQL 错误默认只打印不返回非零，故先靠脚本内的
      # WHENEVER SQLERROR EXIT 兜底，再对输出做一次 ORA- 兜底扫描。
      local out
      out=$(dexec bash -c "sqlplus -S -L $DB_USER/$DB_PASS@//127.0.0.1:1521/$DB_SERVICE @$inside" 2>&1) || {
        printf '%s\n' "$out" >&2
        return 1
      }
      printf '%s\n' "$out"
      if printf '%s\n' "$out" | grep -Eq '^(ORA|SP2)-[0-9]+'; then
        log "sqlplus 报错，视为失败"
        return 1
      fi
      ;;
    *)
      log "未知方言"; return 1
      ;;
  esac
}

# ---------------------------------------------------------------------------
# 执行单条 SQL（增删改用，比造临时文件快）
# ---------------------------------------------------------------------------
cmd_exec() {
  local sql=$1
  case "$DB_DIALECT" in
    mysql)
      dexec mysql --default-character-set=utf8mb4 -u"$DB_USER" -p"$DB_PASS" "$DB_NAME" -e "$sql"
      ;;
    postgresql)
      dexec psql -v ON_ERROR_STOP=1 -q -U "$DB_USER" -d "$DB_NAME" -c "$sql"
      ;;
    sqlserver)
      dexec "$MSSQL_SQLCMD" -S 127.0.0.1 -U "$DB_USER" -P "$DB_PASS" -C -b -d "$DB_NAME" -Q "$sql"
      ;;
    oracle)
      local out
      out=$(dexec bash -c "echo \"${sql//\"/\\\"};\" | sqlplus -s -L $DB_USER/$DB_PASS@//127.0.0.1:1521/$DB_SERVICE" 2>&1) || return 1
      printf '%s\n' "$out"
      if printf '%s\n' "$out" | grep -Eq '^(ORA|SP2)-[0-9]+'; then
        return 1
      fi
      ;;
  esac
}

case "${1:-}" in
  wait)     cmd_wait ;;
  createdb) cmd_createdb ;;
  file)     shift; cmd_file "$@" ;;
  exec)     shift; cmd_exec "$@" ;;
  *)
    log "用法: $0 {wait|createdb|file <path>|exec <sql>}"
    exit 2
    ;;
esac
