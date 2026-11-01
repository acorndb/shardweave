DROP TABLE IF EXISTS mtr_expr;
CREATE TABLE mtr_expr (
  id INT PRIMARY KEY, i INT, d DECIMAL(12,3), s VARCHAR(32), dt DATETIME(6)
);
INSERT INTO mtr_expr VALUES
  (1, 2, 1.500, 'Abc', '2026-11-01 01:02:03.000004'),
  (2, -3, NULL, 'abc ', NULL);

--echo # arithmetic
SELECT id, i+1, i*2, d/3, -i, ABS(i) FROM mtr_expr ORDER BY id;

--echo # case/if
SELECT id, CASE WHEN i > 0 THEN 'pos' ELSE 'neg' END, IF(i IS NULL, 'n', 'v')
FROM mtr_expr ORDER BY id;

--echo # strings
SELECT id, UPPER(s), LOWER(s), LENGTH(s), CONCAT(s, '!')
FROM mtr_expr ORDER BY id;

--echo # temporal
SELECT id, DATE_ADD(dt, INTERVAL 1 DAY), YEAR(dt),
       DATE_FORMAT(dt, '%Y-%m-%d %H:%i:%s')
FROM mtr_expr ORDER BY id;

--echo # nulls
SELECT id, d IS NULL, COALESCE(d, 0), IFNULL(d, -1)
FROM mtr_expr ORDER BY id;

--echo # casts
SELECT CAST('12abc' AS SIGNED), CAST(1.9 AS SIGNED), CAST(1.234 AS DECIMAL(10,2));

DROP TABLE mtr_expr;
