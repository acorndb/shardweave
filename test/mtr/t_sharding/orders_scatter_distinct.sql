--echo # distinct values
SELECT DISTINCT amount FROM orders ORDER BY amount LIMIT 10;
SELECT COUNT(DISTINCT amount) FROM orders;
