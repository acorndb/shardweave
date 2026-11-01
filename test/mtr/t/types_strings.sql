SET NAMES utf8mb4;
DROP TABLE IF EXISTS mtr_str;
CREATE TABLE mtr_str (
  id INT PRIMARY KEY,
  c CHAR(8), v VARCHAR(64), tt TINYTEXT, t TEXT, mt MEDIUMTEXT, lt LONGTEXT,
  e ENUM('a','b','c'), s SET('x','y','z'), j JSON,
  ci VARCHAR(32) COLLATE utf8mb4_general_ci,
  bin VARCHAR(32) COLLATE utf8mb4_bin,
  np VARCHAR(32) COLLATE utf8mb4_0900_ai_ci
);
INSERT INTO mtr_str VALUES
  (1, 'char', 'varchar-世界', 'tiny', 'text', 'medium', 'long',
   'b', 'x,z', '{"k":"v","n":1}', 'Alpha', 'Alpha ', 'Alpha'),
  (2, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL);

--echo # raw strings and lengths
SELECT id, c, v, tt, t, mt, lt, e, s,
       LENGTH(v), CHAR_LENGTH(v), CHAR_LENGTH(mt)
FROM mtr_str ORDER BY id;

--echo # string functions
SELECT id, UPPER(v), LOWER(v), CONCAT(v, '!'), SUBSTRING(v, 1, 7),
       TRIM('  x  '), REPLACE(v, 'a', 'A')
FROM mtr_str ORDER BY id;

--echo # collation comparisons and patterns
SELECT id, c = 'char ', ci = 'alpha', bin = 'Alpha', np = 'alpha',
       v LIKE '%世界%', v REGEXP '^varchar'
FROM mtr_str ORDER BY id;

--echo # enum/set/json
SELECT id, e, s, FIND_IN_SET('x', s), j,
       JSON_EXTRACT(j, '$.k'), JSON_UNQUOTE(JSON_EXTRACT(j, '$.k')), JSON_LENGTH(j)
FROM mtr_str ORDER BY id;

DROP TABLE mtr_str;
