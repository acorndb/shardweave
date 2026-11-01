SET SESSION time_zone = '+00:00';
DROP TABLE IF EXISTS mtr_time;
CREATE TABLE mtr_time (
  id INT PRIMARY KEY,
  d DATE, t TIME(6), dt DATETIME(6), ts TIMESTAMP(6) NULL, y YEAR
);
INSERT INTO mtr_time VALUES
  (1, '2026-11-01', '12:34:56.123456', '2026-11-01 12:34:56.654321',
   '2026-11-01 12:34:56.000001', 2026),
  (2, NULL, NULL, NULL, NULL, NULL);

--echo # raw temporal values
SELECT id, d, t, dt, ts, y FROM mtr_time ORDER BY id;

--echo # date arithmetic and formatting
SELECT id, DATE_ADD(d, INTERVAL 1 DAY), DATE_SUB(dt, INTERVAL 1 HOUR),
       TIMESTAMPDIFF(DAY, '2026-01-01', d),
       DATE_FORMAT(dt, '%Y-%m-%d %H:%i:%s.%f')
FROM mtr_time ORDER BY id;

--echo # extraction and string casts
SELECT id, YEAR(d), MONTH(d), DAY(d), HOUR(t), MINUTE(t), SECOND(t),
       CAST(dt AS CHAR), CAST(t AS CHAR)
FROM mtr_time ORDER BY id;

DROP TABLE mtr_time;
