#!/usr/bin/env bash
# Creates a demo database on a source cluster and, optionally, keeps writing to it.
#
#   dev/demo-data.sh create SOURCE_URI [ROWS]   # create database "shop" with orders and items
#   dev/demo-data.sh write  SOURCE_URI          # insert 200 orders a second until Ctrl+C
#   dev/demo-data.sh count  URI                 # row counts, to compare source and target by hand
#
# SOURCE_URI is a libpq URI for the maintenance database, for example
#   postgresql://doadmin:PASSWORD@HOST:25060/defaultdb?sslmode=require
# For the local test clusters (spikes/env/local-clusters.sh):
#   postgresql://doadmin:spike-password-not-secret@127.0.0.1:55432/defaultdb
set -euo pipefail
cmd=${1:-}; uri=${2:-}; rows=${3:-200000}
[[ -n $cmd && -n $uri ]] || { sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }
shop_uri() { python3 -c 'import sys,urllib.parse as u; p=u.urlsplit(sys.argv[1]); print(u.urlunsplit(p._replace(path="/shop")))' "$1"; }
case $cmd in
  create)
    psql "$uri" -v ON_ERROR_STOP=1 -qc "CREATE DATABASE shop"
    psql "$(shop_uri "$uri")" -v ON_ERROR_STOP=1 -q <<SQL
CREATE TABLE orders(id bigserial PRIMARY KEY, customer int NOT NULL, amount numeric(12,2), note text, created_at timestamptz DEFAULT now());
CREATE TABLE items(order_id bigint REFERENCES orders(id), line int, sku text, qty int, PRIMARY KEY(order_id, line));
INSERT INTO orders(customer, amount, note) SELECT g%1000, (g%9999)/100.0, md5(g::text) FROM generate_series(1,$rows) g;
INSERT INTO items SELECT id, l, 'sku-'||(id*l)%5000, l FROM orders, generate_series(1,2) l;
CREATE INDEX ON orders(customer);
ANALYZE;
SQL
    echo "created shop with $rows orders" ;;
  write)
    echo "writing 200 orders a second to shop; Ctrl+C to stop (stop this before the cutover)"
    while true; do
      psql "$(shop_uri "$uri")" -qc "INSERT INTO orders(customer, amount, note) SELECT g%1000, 1.50, 'live' FROM generate_series(1,200) g" >/dev/null
      sleep 1
    done ;;
  count)
    psql "$(shop_uri "$uri")" -Atc "SELECT 'orders', count(*) FROM orders UNION ALL SELECT 'items', count(*) FROM items" ;;
  *) echo "unknown command $cmd" >&2; exit 2 ;;
esac
