SET NAMES utf8mb4;
SET SESSION time_zone = '+08:00';
SELECT @@session.time_zone;

SET SESSION transaction_isolation = 'READ-COMMITTED';
SELECT @@session.transaction_isolation;

SET @x = 42;
SELECT @x;

DROP TABLE IF EXISTS mtr_sess;
CREATE TABLE mtr_sess (id INT AUTO_INCREMENT PRIMARY KEY, v INT);
INSERT INTO mtr_sess (v) VALUES (10),(20);
SELECT LAST_INSERT_ID();
SELECT ROW_COUNT();
SELECT id, v FROM mtr_sess ORDER BY id;

USE bench;
SELECT DATABASE();

DROP TABLE mtr_sess;
