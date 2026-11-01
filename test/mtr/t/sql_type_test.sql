--echo # integers
SELECT id, tiny_i, utiny_i, small_i, medium_i, int_i, uint_i, big_i, ubig_i
FROM sql_type_test ORDER BY id;

--echo # decimals and bit
SELECT id, dec_v, float_v, double_v, bit_v+0
FROM sql_type_test ORDER BY id;

--echo # temporal
SELECT id, date_v, time_v, datetime_v, timestamp_v, year_v
FROM sql_type_test ORDER BY id;

--echo # strings and collations
SELECT id, char_v, varchar_v, ci_v, bin_v, nopad_v, LENGTH(varchar_v)
FROM sql_type_test ORDER BY id;

--echo # binary
SELECT id, HEX(binary_v), HEX(varbinary_v), HEX(blob_v), HEX(tinyblob_v),
       HEX(mediumblob_v), HEX(longblob_v)
FROM sql_type_test ORDER BY id;

--echo # enum, set, json
SELECT id, enum_v, set_v, json_v, JSON_EXTRACT(json_v, '$.k')
FROM sql_type_test ORDER BY id;

--echo # generated column
SELECT id, int_i, gen_v FROM sql_type_test ORDER BY id;
