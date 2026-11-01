DROP TABLE IF EXISTS mtr_dml;
DROP TABLE IF EXISTS mtr_src;
CREATE TABLE mtr_dml (id INT PRIMARY KEY, v INT, note VARCHAR(16));
CREATE TABLE mtr_src (id INT PRIMARY KEY, v INT);
INSERT INTO mtr_src VALUES (10,100),(11,110);

--echo # insert forms
INSERT INTO mtr_dml VALUES (1,1,'a'),(2,2,'b'),(3,3,'c');
INSERT INTO mtr_dml SELECT id, v, 'src' FROM mtr_src;
INSERT INTO mtr_dml VALUES (2,20,'dup') ON DUPLICATE KEY UPDATE v = VALUES(v);
SELECT id, v, note FROM mtr_dml ORDER BY id;

--echo # replace and update join
REPLACE INTO mtr_dml VALUES (3,30,'replaced');
UPDATE mtr_dml d JOIN mtr_src s ON s.id = d.id SET d.v = s.v * 2;
SELECT id, v, note FROM mtr_dml ORDER BY id;

--echo # delete forms
DELETE FROM mtr_dml WHERE v < 3 LIMIT 1;
DELETE FROM mtr_dml WHERE id IN (SELECT id FROM mtr_src);
SELECT id, v, note FROM mtr_dml ORDER BY id;

DROP TABLE mtr_src;
DROP TABLE mtr_dml;
