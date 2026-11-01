--echo # string functions
SELECT UPPER('a'), LOWER('A'), LENGTH('世界'), CHAR_LENGTH('世界'),
       CONCAT('a','b'), CONCAT_WS('-','a','b'), SUBSTRING('abcdef',2,3),
       LEFT('abc',2), RIGHT('abc',2), LPAD('x',3,'0'), REPLACE('aba','a','c'),
       REVERSE('abc'), TRIM('  x  '), LTRIM('  x'), RTRIM('x  ');

--echo # math functions
SELECT ABS(-2), CEIL(1.2), FLOOR(1.8), ROUND(1.235,2), TRUNCATE(1.239,2),
       MOD(7,3), POW(2,3), SQRT(9), SIGN(-2), GREATEST(1,3,2), LEAST(1,3,2);

--echo # date functions
SELECT DATE('2026-11-01 12:34:56'), YEAR('2026-11-01'), MONTH('2026-11-01'),
       DAY('2026-11-01'), HOUR('12:34:56'), MINUTE('12:34:56'), SECOND('12:34:56'),
       DAYOFWEEK('2026-11-01'),
       DATE_FORMAT('2026-11-01 12:34:56','%Y-%m-%d %H:%i:%s'),
       STR_TO_DATE('2026-11-01','%Y-%m-%d');

--echo # json functions
SELECT JSON_EXTRACT('{"a":[1,2]}','$.a[1]'), JSON_LENGTH('{"a":[1,2]}'),
       JSON_UNQUOTE(JSON_EXTRACT('{"a":"世界"}','$.a'));
