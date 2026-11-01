--echo # arithmetic and bitwise
SELECT 1+2, 5-3, 2*3, 7/2, 7 DIV 2, 7 MOD 2, 2<<3, 8>>2, 5&3, 5|2, 5^1;

--echo # comparisons and predicates
SELECT 1=1, 1<>2, 1<2, NULL=NULL, NULL IS NULL, NULL <=> NULL,
       1 BETWEEN 0 AND 2, 2 IN (1,2,3), 'a' LIKE 'a%', 'abc' REGEXP '^a';

--echo # case/if/nulls/casts
SELECT CASE WHEN 1=1 THEN 'y' ELSE 'n' END, IF(1=1,'y','n'),
       COALESCE(NULL,2), NULLIF(1,1), CAST('12' AS SIGNED), CAST(1.9 AS SIGNED);

--echo # division by zero yields NULL
SELECT 1/0, 1%0;
