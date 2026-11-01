DROP TABLE IF EXISTS mtr_bin;
CREATE TABLE mtr_bin (
  id INT PRIMARY KEY,
  b BINARY(8), vb VARBINARY(64), tb TINYBLOB, bl BLOB, mb MEDIUMBLOB, lb LONGBLOB,
  bitv BIT(16)
);
INSERT INTO mtr_bin VALUES
  (1, x'0001020304050607', x'deadbeef', x'00', x'0102', x'030405', x'060708',
   b'1010101010101010'),
  (2, NULL, NULL, NULL, NULL, NULL, NULL, NULL);

--echo # binary values
SELECT id, HEX(b), HEX(vb), HEX(tb), HEX(bl), HEX(mb), HEX(lb), bitv+0
FROM mtr_bin ORDER BY id;

--echo # binary lengths and comparisons
SELECT id, LENGTH(b), LENGTH(vb), b = x'0001020304050607', vb = x'deadbeef'
FROM mtr_bin ORDER BY id;

DROP TABLE mtr_bin;
