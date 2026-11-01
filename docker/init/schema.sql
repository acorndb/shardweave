-- Shared fixture schema. Mounted before the per-mode seed scripts so every
-- topology (single / rwsplit / sharding) starts with the same tables.
USE bench;

CREATE TABLE IF NOT EXISTS corpus_demo (
  id INT NOT NULL PRIMARY KEY,
  v VARCHAR(32) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS orders (
  id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
  user_id INT NOT NULL,
  amount INT NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS profiles (
  name VARCHAR(32) NOT NULL PRIMARY KEY,
  score INT NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- One row per type family, including NULLs and boundary widths. shard_key is
-- the sharding column when the table is used through the sharding proxy.
CREATE TABLE IF NOT EXISTS sql_type_test (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  shard_key INT NOT NULL DEFAULT 0,
  tiny_i TINYINT NULL,
  utiny_i TINYINT UNSIGNED NULL,
  small_i SMALLINT NULL,
  medium_i MEDIUMINT NULL,
  int_i INT NULL,
  uint_i INT UNSIGNED NULL,
  big_i BIGINT NULL,
  ubig_i BIGINT UNSIGNED NULL,
  dec_v DECIMAL(30,10) NULL,
  float_v FLOAT NULL,
  double_v DOUBLE NULL,
  bit_v BIT(16) NULL,
  char_v CHAR(8) NULL,
  varchar_v VARCHAR(64) NULL,
  tinytext_v TINYTEXT NULL,
  text_v TEXT NULL,
  mediumtext_v MEDIUMTEXT NULL,
  longtext_v LONGTEXT NULL,
  binary_v BINARY(8) NULL,
  varbinary_v VARBINARY(64) NULL,
  tinyblob_v TINYBLOB NULL,
  blob_v BLOB NULL,
  mediumblob_v MEDIUMBLOB NULL,
  longblob_v LONGBLOB NULL,
  date_v DATE NULL,
  time_v TIME(6) NULL,
  datetime_v DATETIME(6) NULL,
  timestamp_v TIMESTAMP(6) NULL DEFAULT NULL,
  year_v YEAR NULL,
  enum_v ENUM('a','b','c') NULL,
  set_v SET('x','y','z') NULL,
  json_v JSON NULL,
  bool_v BOOLEAN NULL,
  ci_v VARCHAR(32) COLLATE utf8mb4_general_ci NULL,
  bin_v VARCHAR(32) COLLATE utf8mb4_bin NULL,
  nopad_v VARCHAR(32) COLLATE utf8mb4_0900_ai_ci NULL,
  gen_v INT GENERATED ALWAYS AS (int_i + 1) VIRTUAL,
  KEY idx_int (int_i),
  KEY idx_varchar (varchar_v)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
