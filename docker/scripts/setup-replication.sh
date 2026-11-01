#!/bin/sh
# One-shot compose service: wire mysql-replica to mysql-master via GTID and
# wait until every master transaction (including the app user and schema) has
# been applied. Exiting non-zero keeps shardweave from starting.
set -eu

MASTER_HOST="${MASTER_HOST:-mysql-master}"
REPLICA_HOST="${REPLICA_HOST:-mysql-replica}"
export MYSQL_PWD="${MYSQL_ROOT_PASSWORD:-root}"

wait_for() {
  host="$1"
  i=0
  until mysql -h "$host" -uroot -e "SELECT 1" >/dev/null 2>&1; do
    i=$((i + 1))
    if [ "$i" -ge 60 ]; then
      echo "mysql $host not ready after ${i}s" >&2
      exit 1
    fi
    sleep 1
  done
}

wait_for "$MASTER_HOST"
wait_for "$REPLICA_HOST"

mysql -h "$REPLICA_HOST" -uroot -e "STOP REPLICA" >/dev/null 2>&1 || true
mysql -h "$REPLICA_HOST" -uroot -e "
  CHANGE REPLICATION SOURCE TO
    SOURCE_HOST='$MASTER_HOST',
    SOURCE_PORT=3306,
    SOURCE_USER='repl',
    SOURCE_PASSWORD='repl-pass-123',
    SOURCE_AUTO_POSITION=1,
    GET_SOURCE_PUBLIC_KEY=1;
  START REPLICA;
"

GTID=$(mysql -h "$MASTER_HOST" -uroot -N -e "SELECT @@GLOBAL.gtid_executed")
mysql -h "$REPLICA_HOST" -uroot -e "SELECT WAIT_FOR_EXECUTED_GTID_SET('$GTID', 30)" >/dev/null

# Replicas are read-only once caught up, so a misrouted write fails loudly.
mysql -h "$REPLICA_HOST" -uroot -e "SET GLOBAL read_only = ON"
echo "replication ready: $GTID"
