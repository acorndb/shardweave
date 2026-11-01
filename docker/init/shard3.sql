USE bench;
SET NAMES utf8mb4;

-- user_id % 4 == 3 belongs to s3 (hash_mod over 4 slices).
INSERT INTO orders (id, user_id, amount) VALUES
(4,3,4),
(8,7,104),
(12,11,204),
(16,15,304),
(20,19,404),
(24,23,504),
(28,27,604),
(32,31,704),
(36,35,804),
(40,39,904),
(44,43,1004),
(48,47,1104),
(52,51,1204),
(56,55,1304),
(60,59,1404),
(64,63,1504),
(68,67,1604),
(72,71,1704),
(76,75,1804),
(80,79,1904);

INSERT IGNORE INTO sql_type_test (id, shard_key, tiny_i, utiny_i, small_i, medium_i, int_i, uint_i, big_i, ubig_i,
  dec_v, float_v, double_v, bit_v, char_v, varchar_v,
  tinytext_v, text_v, mediumtext_v, longtext_v,
  binary_v, varbinary_v, tinyblob_v, blob_v, mediumblob_v, longblob_v,
  date_v, time_v, datetime_v, timestamp_v, year_v,
  enum_v, set_v, json_v, bool_v, ci_v, bin_v, nopad_v)
VALUES (4, 3, -1, 1, -2, -3, -4, 5, -6, 7, 0.0000000001, 0.5, 0.125, b'1111',
  'char3', 'shard3-世界', 'tiny3', 'text3', 'medium3', 'long3',
  x'1112131415161718', x'01020304', x'03', x'0405', x'060708', x'090a0b',
  '2026-11-03', '23:59:59.999999', '2026-11-03 23:59:59.999999',
  '2026-11-03 23:59:59.999999', 2026, 'a', 'x', '{"who":"shard3","n":3}', 1,
  'BETA', 'beta ', 'BETA');
