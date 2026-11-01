DROP TABLE IF EXISTS mtr_tx;
CREATE TABLE mtr_tx (id INT PRIMARY KEY, v INT);

--echo # rollback
BEGIN;
INSERT INTO mtr_tx VALUES (1,1);
ROLLBACK;
SELECT COUNT(*) FROM mtr_tx;

--echo # commit
BEGIN;
INSERT INTO mtr_tx VALUES (2,2);
COMMIT;
SELECT id, v FROM mtr_tx ORDER BY id;

--echo # autocommit=0
SET autocommit = 0;
INSERT INTO mtr_tx VALUES (3,3);
COMMIT;
SELECT id, v FROM mtr_tx ORDER BY id;
SET autocommit = 1;

--echo # savepoint
BEGIN;
INSERT INTO mtr_tx VALUES (4,4);
SAVEPOINT s1;
INSERT INTO mtr_tx VALUES (5,5);
ROLLBACK TO SAVEPOINT s1;
COMMIT;
SELECT id, v FROM mtr_tx ORDER BY id;

--echo # isolation
SET SESSION transaction_isolation = 'READ-COMMITTED';
SELECT @@session.transaction_isolation;

DROP TABLE mtr_tx;
