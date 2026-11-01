--echo # group by key
SELECT user_id, COUNT(*) FROM orders GROUP BY user_id ORDER BY user_id LIMIT 10;

--echo # having on aggregate in output
SELECT user_id, SUM(amount) FROM orders GROUP BY user_id
HAVING SUM(amount) > 100 ORDER BY user_id LIMIT 5;
