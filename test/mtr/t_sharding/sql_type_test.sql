--echo # type rows scatter
SELECT id, shard_key, int_i, varchar_v FROM sql_type_test ORDER BY id;

--echo # type rows keyed
SELECT id, shard_key, int_i, varchar_v FROM sql_type_test WHERE shard_key = 2;

--echo # type aggregates across shards
SELECT COUNT(*), SUM(int_i), MIN(varchar_v), MAX(varchar_v) FROM sql_type_test;

--echo # type count
SELECT COUNT(*) FROM sql_type_test;
