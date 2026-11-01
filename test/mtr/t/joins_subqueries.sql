DROP TABLE IF EXISTS mtr_a;
DROP TABLE IF EXISTS mtr_b;
CREATE TABLE mtr_a (id INT PRIMARY KEY, v VARCHAR(8));
CREATE TABLE mtr_b (id INT PRIMARY KEY, a_id INT, n INT);
INSERT INTO mtr_a VALUES (1,'one'),(2,'two'),(3,'three');
INSERT INTO mtr_b VALUES (10,1,5),(11,2,7),(12,2,9);

--echo # inner join
SELECT a.id, a.v, b.id, b.n FROM mtr_a a JOIN mtr_b b ON b.a_id = a.id
ORDER BY a.id, b.id;

--echo # left and right join
SELECT a.id, b.n FROM mtr_a a LEFT JOIN mtr_b b ON b.a_id = a.id
ORDER BY a.id, b.n;
SELECT a.id, b.id FROM mtr_a a RIGHT JOIN mtr_b b ON b.a_id = a.id
ORDER BY b.id;

--echo # cross and self join
SELECT COUNT(*) FROM mtr_a CROSS JOIN mtr_b;
SELECT a.id, b.id FROM mtr_a a JOIN mtr_a b ON a.id < b.id ORDER BY a.id, b.id;

--echo # subqueries
SELECT id FROM mtr_a WHERE EXISTS (SELECT 1 FROM mtr_b WHERE a_id = mtr_a.id) ORDER BY id;
SELECT id FROM mtr_a WHERE id IN (SELECT a_id FROM mtr_b) ORDER BY id;
SELECT (SELECT MAX(n) FROM mtr_b) AS max_n;
SELECT * FROM (SELECT id, v FROM mtr_a) d ORDER BY id;

--echo # union
SELECT id FROM mtr_a UNION SELECT a_id FROM mtr_b ORDER BY id;
SELECT id FROM mtr_a UNION ALL SELECT a_id FROM mtr_b ORDER BY id;

DROP TABLE mtr_b;
DROP TABLE mtr_a;
