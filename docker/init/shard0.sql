USE bench;
SET NAMES utf8mb4;

-- user_id % 4 == 0 belongs to s0 (hash_mod over 4 slices).
INSERT INTO orders (id, user_id, amount) VALUES
(1,0,1),
(5,4,101),
(9,8,201),
(13,12,301),
(17,16,401),
(21,20,501),
(25,24,601),
(29,28,701),
(33,32,801),
(37,36,901),
(41,40,1001),
(45,44,1101),
(49,48,1201),
(53,52,1301),
(57,56,1401),
(61,60,1501),
(65,64,1601),
(69,68,1701),
(73,72,1801),
(77,76,1901);

INSERT IGNORE INTO sql_type_test (id, shard_key, tiny_i, utiny_i, small_i, medium_i, int_i, uint_i, big_i, ubig_i,
  dec_v, float_v, double_v, bit_v, char_v, varchar_v,
  tinytext_v, text_v, mediumtext_v, longtext_v,
  binary_v, varbinary_v, tinyblob_v, blob_v, mediumblob_v, longblob_v,
  date_v, time_v, datetime_v, timestamp_v, year_v,
  enum_v, set_v, json_v, bool_v, ci_v, bin_v, nopad_v)
VALUES (1, 0, -128, 255, -32768, -8388608, -2147483648, 4294967295,
  -9223372036854775808, 18446744073709551615,
  '12345678901234567890.1234567890', 1.25, 2.5, b'1010101010101010',
  'char', 'varchar-utf8-世界', 'tinytext', 'text', 'mediumtext', 'longtext',
  x'0001020304050607', x'deadbeef', x'00', x'0102', x'030405', x'060708',
  '2026-11-01', '12:34:56.123456', '2026-11-01 12:34:56.654321',
  '2026-11-01 12:34:56.000001', 2026, 'b', 'x,z', '{"k":"v","n":1}', 1,
  'Alpha', 'Alpha ', 'Alpha');
