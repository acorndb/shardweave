DROP VIEW IF EXISTS mtr_v;
DROP TABLE IF EXISTS mtr_ddl;
CREATE TABLE mtr_ddl (id INT PRIMARY KEY, v VARCHAR(16));
CREATE INDEX idx_mtr_ddl_v ON mtr_ddl (v);
ALTER TABLE mtr_ddl ADD COLUMN n INT NOT NULL DEFAULT 0;

--echo # show metadata
SHOW TABLES LIKE 'mtr_ddl';
SHOW COLUMNS FROM mtr_ddl;
SHOW INDEX FROM mtr_ddl;
SHOW CREATE TABLE mtr_ddl;
DESC mtr_ddl;

--echo # view
CREATE VIEW mtr_v AS SELECT 1 AS x;
SELECT * FROM mtr_v;

--echo # rename and truncate
RENAME TABLE mtr_ddl TO mtr_ddl2;
SHOW TABLES LIKE 'mtr_ddl2';
TRUNCATE TABLE mtr_ddl2;

DROP VIEW mtr_v;
DROP TABLE mtr_ddl2;
