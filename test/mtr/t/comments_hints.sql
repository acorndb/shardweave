--echo # plain comment
/* plain comment */ SELECT 1;

--echo # sql line comment
-- a line comment
SELECT 2;

--echo # optimizer hint
SELECT /*+ MAX_EXECUTION_TIME(1000) */ 1;

--echo # version comment
/*!40101 SET NAMES utf8mb4 */;
SELECT 3;
