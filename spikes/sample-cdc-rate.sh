#!/usr/bin/env bash
# Samples a running pgcopydb follow run every N seconds: source WAL position, the
# sentinel's received (write) and applied (replay) positions, the target origin progress
# and the CDC directory size. Usage: sample-cdc-rate.sh <db> <workdir> <origin> <out.csv> [interval]
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); source "$HERE/lib.sh"
db=$1 dir=$2 origin=$3 out=$4 every=${5:-30}
echo "ts,source_lsn,write_lsn,replay_lsn,origin_lsn,cdc_bytes,write_behind_bytes,replay_behind_bytes" >"$out"
while [[ -d "$dir" ]]; do
  cur=$(src "$db" -c "select pg_current_wal_lsn()" 2>/dev/null) || break
  w=$(sentinel_field "$db" "$dir" write_lsn); r=$(sentinel_field "$db" "$dir" replay_lsn)
  o=$(dst defaultdb -c "select coalesce(pg_replication_origin_progress('$origin', false)::text,'')" 2>/dev/null)
  sz=$(du -sb "$dir/cdc" 2>/dev/null | cut -f1)
  wb=$(src defaultdb -c "select pg_wal_lsn_diff('$cur','$w')::bigint" 2>/dev/null)
  rb=$(src defaultdb -c "select pg_wal_lsn_diff('$cur','$r')::bigint" 2>/dev/null)
  echo "$(date -u +%FT%TZ),$cur,$w,$r,$o,$sz,$wb,$rb" >>"$out"
  sleep "$every"
done
