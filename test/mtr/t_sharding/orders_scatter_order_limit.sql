--echo # multi-column order
SELECT id, user_id, amount FROM orders ORDER BY user_id DESC, id LIMIT 10;

--echo # limit offset
SELECT id FROM orders ORDER BY id LIMIT 5 OFFSET 10;
