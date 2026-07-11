#!/usr/bin/env bash
# Backup benchmark for the streaming-memory and throughput claims (poc-plan 6.3):
# peak RSS must stay under 256 MiB and must NOT grow with the dump size, since
# the pipeline streams with no intermediate disk. Needs the stand up
# (`make stand-up`) and a built binary. Usage:
#
#   DBFERRY_BIN=/tmp/dbferry scripts/bench.sh pg   <db> <rows> <random|text>
#   DBFERRY_BIN=/tmp/dbferry scripts/bench.sh mysql <db> <rows> <random|text>
set -euo pipefail

BIN=${DBFERRY_BIN:-/tmp/dbferry}
REC=$(cat test/integration/.stand/age-recipient.txt)
export AWS_ACCESS_KEY_ID=${AWS_ACCESS_KEY_ID:-minioadmin}
export AWS_SECRET_ACCESS_KEY=${AWS_SECRET_ACCESS_KEY:-minioadmin}
export AWS_REGION=${AWS_REGION:-us-east-1}
ENDPOINT=http://localhost:9000

report() { # timing_file json_file label extra
  local rss real ct
  rss=$(awk '/maximum resident/{print $1}' "$1")
  real=$(awk '/ real/{print $1; exit}' "$1")
  ct=$(python3 -c "import json,sys;print(json.load(open('$2'))['ciphertext_bytes'])")
  printf "%-22s tablesize=%-9s ciphertext=%-11s rss=%4dMiB real=%ss\n" \
    "$3" "$4" "$ct" "$((rss / 1024 / 1024))" "$real"
}

bench_pg() {
  local db=$1 rows=$2 mode=$3
  local psql="docker exec dbferry-stand-pg17-1 psql -U dbferry -qtA"
  local payload="gen_random_bytes(1024)"
  [ "$mode" = text ] && payload="convert_to(repeat(md5(g::text), 32), 'UTF8')"
  $psql -d postgres -c "DROP DATABASE IF EXISTS $db" >/dev/null
  $psql -d postgres -c "CREATE DATABASE $db" >/dev/null
  $psql -d "$db" -c "CREATE EXTENSION IF NOT EXISTS pgcrypto; CREATE TABLE t(id int primary key, payload bytea);" >/dev/null
  $psql -d "$db" -c "INSERT INTO t SELECT g, $payload FROM generate_series(1,$rows) g;" >/dev/null
  local size; size=$($psql -d "$db" -c "SELECT pg_size_pretty(pg_total_relation_size('t'));")
  local jf; jf=$(mktemp)
  DBFERRY_DSN="postgres://dbferry:dbferry@localhost:5417/$db" \
    /usr/bin/time -l "$BIN" run --dest "s3://dbferry-backups/bench/$db" \
      --age-recipient "$REC" --s3-endpoint "$ENDPOINT" --json >"$jf" 2>/tmp/bench_time.txt
  report /tmp/bench_time.txt "$jf" "pg $mode rows=$rows" "$size"
  [ "${KEEP:-}" = 1 ] || $psql -d postgres -c "DROP DATABASE IF EXISTS $db" >/dev/null
}

bench_mysql() {
  local db=$1 rows=$2 mode=$3
  local my="mysql -h127.0.0.1 -P3308 -uroot --protocol=TCP"
  export MYSQL_PWD=dbferry
  local payload="RANDOM_BYTES(1024)"
  [ "$mode" = text ] && payload="CONVERT(REPEAT(MD5(seq.n), 32) USING binary)"
  $my -e "DROP DATABASE IF EXISTS $db; CREATE DATABASE $db; SET SESSION cte_max_recursion_depth=100000000;
    CREATE TABLE $db.t(id BIGINT PRIMARY KEY, payload VARBINARY(2048));
    INSERT INTO $db.t WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n<$rows)
    SELECT n, $payload FROM seq;"
  local size; size=$($my -N -e "SELECT ROUND((data_length+index_length)/1048576)||'MB' FROM information_schema.tables WHERE table_schema='$db' AND table_name='t';")
  local jf; jf=$(mktemp)
  DBFERRY_DSN="mysql://root:dbferry@127.0.0.1:3308/$db" \
    /usr/bin/time -l "$BIN" run --dest "s3://dbferry-backups/bench/$db" \
      --age-recipient "$REC" --s3-endpoint "$ENDPOINT" --json >"$jf" 2>/tmp/bench_time.txt
  report /tmp/bench_time.txt "$jf" "mysql $mode rows=$rows" "$size"
  [ "${KEEP:-}" = 1 ] || $my -e "DROP DATABASE IF EXISTS $db"
}

"bench_$1" "$2" "$3" "$4"
