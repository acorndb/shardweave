DROP TABLE IF EXISTS mtr_lock;
CREATE TABLE mtr_lock (id INT PRIMARY KEY, v INT);
INSERT INTO mtr_lock VALUES (1,10),(2,20);

--echo # select for update
BEGIN;
SELECT id, v FROM mtr_lock WHERE id = 1 FOR UPDATE;
COMMIT;

--echo # lock in share mode
BEGIN;
SELECT id, v FROM mtr_lock WHERE id = 1 LOCK IN SHARE MODE;
COMMIT;

--echo # DO
DO 1;

DROP TABLE mtr_lock;
