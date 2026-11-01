USE bench;
SET NAMES utf8mb4;

-- user_id % 4 == 2 belongs to s2 (hash_mod over 4 slices).
INSERT INTO orders (id, user_id, amount) VALUES
(3,2,3),
(7,6,103),
(11,10,203),
(15,14,303),
(19,18,403),
(23,22,503),
(27,26,603),
(31,30,703),
(35,34,803),
(39,38,903),
(43,42,1003),
(47,46,1103),
(51,50,1203),
(55,54,1303),
(59,58,1403),
(63,62,1503),
(67,66,1603),
(71,70,1703),
(75,74,1803),
(79,78,1903);

INSERT IGNORE INTO sql_type_test (id, shard_key, tiny_i, utiny_i, small_i, medium_i, int_i, uint_i, big_i, ubig_i,
  dec_v, float_v, double_v, bit_v, char_v, varchar_v,
  tinytext_v, text_v, mediumtext_v, longtext_v,
  binary_v, varbinary_v, tinyblob_v, blob_v, mediumblob_v, longblob_v,
  date_v, time_v, datetime_v, timestamp_v, year_v,
  enum_v, set_v, json_v, bool_v, ci_v, bin_v, nopad_v)
VALUES (3, 2, 7, 7, 7, 7, 42, 42, 42, 42, 12.3400000000, 3.5, 7.25, b'1',
  'char2', 'shard2-世界', 'tiny2', 'text2', 'medium2', 'long2',
  x'0102030405060708', x'cafebabe', x'02', x'0304', x'050607', x'08090a',
  '2026-11-02', '01:02:03.000004', '2026-11-02 01:02:03.000004',
  '2026-11-02 01:02:03.000004', 2026, 'c', 'y,z', '{"who":"shard2","n":2}', 0,
  'alpha', 'Alpha ', 'alpha');
