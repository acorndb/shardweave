--echo # user_id IN (1,5): both hash to s1
SELECT id, user_id, amount FROM orders WHERE user_id IN (1, 5) ORDER BY id;
SELECT COUNT(*) FROM orders WHERE user_id IN (1, 5);
