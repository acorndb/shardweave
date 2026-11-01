DROP TABLE IF EXISTS mtr_err;
CREATE TABLE mtr_err (id INT PRIMARY KEY, v VARCHAR(4) NOT NULL);
INSERT INTO mtr_err VALUES (1,'a');

--echo # duplicate key
--error 1062
INSERT INTO mtr_err VALUES (1,'b');

--echo # not null
--error 1048
INSERT INTO mtr_err VALUES (2,NULL);

--echo # data too long
--error 1406
INSERT INTO mtr_err VALUES (3,'toolong');

--echo # unknown table / column
--error 1146
SELECT * FROM mtr_no_such_table;
--error 1054
SELECT no_such_column FROM mtr_err;

--echo # syntax error
--error 1064
SELEC 1;

--echo # connection still usable
SELECT 1;

DROP TABLE mtr_err;
