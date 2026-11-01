DROP TABLE IF EXISTS mtr_dml;
CREATE TABLE mtr_dml (id INT PRIMARY KEY, v INT NOT NULL);
INSERT INTO mtr_dml VALUES (1,1),(2,2),(3,3),(4,4);

UPDATE mtr_dml SET v = v * 10 WHERE id IN (2, 4);
SELECT id, v FROM mtr_dml ORDER BY id;

DELETE FROM mtr_dml WHERE v < 10;
SELECT COUNT(*), SUM(v), MIN(v), MAX(v) FROM mtr_dml;

DROP TABLE mtr_dml;
