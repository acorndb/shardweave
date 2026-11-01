DROP TABLE IF EXISTS mtr_types;
CREATE TABLE mtr_types (
  id INT PRIMARY KEY,
  d DECIMAL(30,10), f FLOAT, db DOUBLE,
  dt DATETIME(6), b VARBINARY(16), j JSON
);
INSERT INTO mtr_types VALUES
  (1, '12345678901234567890.1234567890', 1.25, 2.5,
   '2026-11-01 12:34:56.654321', x'deadbeef',
   '{"a":[1,2,3],"b":"世界"}');

--echo # values
SELECT id, d, f, db, dt, HEX(b),
       JSON_EXTRACT(j, '$.a[1]'), JSON_UNQUOTE(JSON_EXTRACT(j, '$.b'))
FROM mtr_types;

--echo # casts
SELECT CAST(d AS CHAR), CAST(dt AS CHAR), LENGTH(b) FROM mtr_types;

DROP TABLE mtr_types;
