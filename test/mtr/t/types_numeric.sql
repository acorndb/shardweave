DROP TABLE IF EXISTS mtr_num;
CREATE TABLE mtr_num (
  id INT PRIMARY KEY,
  ti TINYINT, uti TINYINT UNSIGNED,
  si SMALLINT, mi MEDIUMINT, i INT, ui INT UNSIGNED,
  bi BIGINT, ubi BIGINT UNSIGNED,
  d DECIMAL(30,10), f FLOAT, db DOUBLE, b BIT(16)
);
INSERT INTO mtr_num VALUES
  (1, -128, 255, -32768, -8388608, -2147483648, 4294967295,
   -9223372036854775808, 18446744073709551615,
   '12345678901234567890.1234567890', 1.25, 2.5, b'1010101010101010'),
  (2, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL);

--echo # raw values
SELECT id, ti, uti, si, mi, i, ui, bi, ubi, d, f, db, b+0 FROM mtr_num ORDER BY id;

--echo # arithmetic
SELECT id, ti+1, i*2, d/3, f+0.5, db*2, MOD(i, 3), ABS(ti) FROM mtr_num ORDER BY id;

--echo # rounding and casts
SELECT id, ROUND(d, 2), FLOOR(f), CEIL(f), CAST(d AS CHAR), CAST(f AS DECIMAL(10,2))
FROM mtr_num ORDER BY id;

DROP TABLE mtr_num;
