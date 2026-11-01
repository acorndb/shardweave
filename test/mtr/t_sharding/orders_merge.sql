--echo # scatter aggregate
SELECT COUNT(*), SUM(amount), MIN(amount), MAX(amount) FROM orders;
SELECT COUNT(DISTINCT user_id) FROM orders;

--echo # scatter order/limit
SELECT id, user_id, amount FROM orders ORDER BY id LIMIT 10;

--echo # scatter group by (key in output)
SELECT user_id, COUNT(*) FROM orders GROUP BY user_id ORDER BY user_id LIMIT 10;

--echo # scatter distinct
SELECT DISTINCT amount FROM orders ORDER BY amount LIMIT 10;
