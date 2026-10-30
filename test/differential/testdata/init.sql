-- 差分校准用:direct 侧库由容器 WithDatabase 创建,此处补建 proxied 侧库并授权
-- (镜像 env 创建的用户默认只有 MYSQL_DATABASE 的权限)。
CREATE DATABASE IF NOT EXISTS calib_proxied CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;
GRANT ALL PRIVILEGES ON calib_proxied.* TO 'calib'@'%';
