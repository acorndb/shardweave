--echo # keyed insert/update/delete (user_id=2 -> s2)
INSERT INTO orders (id, user_id, amount) VALUES (1001, 2, 777);
SELECT id, user_id, amount FROM orders WHERE user_id = 2 AND id = 1001;

UPDATE orders SET amount = 888 WHERE user_id = 2 AND id = 1001;
SELECT amount FROM orders WHERE id = 1001;

DELETE FROM orders WHERE user_id = 2 AND id = 1001;
SELECT COUNT(*) FROM orders WHERE id = 1001;
