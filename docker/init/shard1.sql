USE bench;
SET NAMES utf8mb4;

-- user_id % 4 == 1 belongs to s1 (hash_mod over 4 slices).
INSERT INTO orders (id, user_id, amount) VALUES
(2,1,2),
(6,5,102),
(10,9,202),
(14,13,302),
(18,17,402),
(22,21,502),
(26,25,602),
(30,29,702),
(34,33,802),
(38,37,902),
(42,41,1002),
(46,45,1102),
(50,49,1202),
(54,53,1302),
(58,57,1402),
(62,61,1502),
(66,65,1602),
(70,69,1702),
(74,73,1802),
(78,77,1902);

INSERT IGNORE INTO sql_type_test (id, shard_key, tiny_i, utiny_i, small_i, medium_i, int_i, uint_i, big_i, ubig_i,
  dec_v, float_v, double_v, bit_v, char_v, varchar_v,
  tinytext_v, text_v, mediumtext_v, longtext_v,
  binary_v, varbinary_v, tinyblob_v, blob_v, mediumblob_v, longblob_v,
  date_v, time_v, datetime_v, timestamp_v, year_v,
  enum_v, set_v, json_v, bool_v, ci_v, bin_v, nopad_v)
VALUES (2, 1, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL);
