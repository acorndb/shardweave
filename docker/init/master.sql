USE bench;

-- Replication account used by the one-shot `replication` service. The app
-- user and schema themselves come from the entrypoint/replication stream.
CREATE USER IF NOT EXISTS 'repl'@'%' IDENTIFIED WITH mysql_native_password BY 'repl-pass-123';
GRANT REPLICATION SLAVE ON *.* TO 'repl'@'%';
