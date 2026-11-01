DROP TABLE IF EXISTS mtr_err;
CREATE TABLE mtr_err (id INT PRIMARY KEY, v VARCHAR(8) NOT NULL);
INSERT INTO mtr_err VALUES (1, 'a');

--error 1062
INSERT INTO mtr_err VALUES (1, 'b');

--echo # connection is still usable after the error
SELECT id, v FROM mtr_err ORDER BY id;

DROP TABLE mtr_err;
