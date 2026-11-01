--echo # whitelisted session variables survive backend connection changes
SET SESSION time_zone = '+08:00';
SELECT @@session.time_zone;

SET SESSION transaction_isolation = 'READ-COMMITTED';
SELECT @@session.transaction_isolation;
