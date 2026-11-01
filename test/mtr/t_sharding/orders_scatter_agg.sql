--echo # scatter aggregates
SELECT COUNT(*), SUM(amount), MIN(amount), MAX(amount), AVG(amount) FROM orders;

--echo # distinct aggregates
SELECT COUNT(DISTINCT user_id), COUNT(DISTINCT amount) FROM orders;
