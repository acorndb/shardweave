--echo # keyed rows: user_id % 4 selects the slice
SELECT id, user_id, amount FROM orders WHERE user_id = 0;
SELECT id, user_id, amount FROM orders WHERE user_id = 1;
SELECT id, user_id, amount FROM orders WHERE user_id = 2;
SELECT id, user_id, amount FROM orders WHERE user_id = 3;

--echo # keyed count
SELECT COUNT(*) FROM orders WHERE user_id = 2;

--echo # keyed locking read (autocommit)
SELECT id, user_id, amount FROM orders WHERE user_id = 2 AND id = 3 FOR UPDATE;
