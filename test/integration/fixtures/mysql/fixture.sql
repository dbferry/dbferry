-- dbferry MySQL integration fixture (poc-plan 0.3).
--
-- Exercises schema, data, indexes, foreign keys, a trigger, a stored function,
-- a stored procedure, and an event — the MySQL-specific objects a per-database
-- backup must preserve (mysqldump needs --routines and --events for the last
-- two; triggers travel with the table).
--
-- Loaded with the `mysql` CLI, which honours the DELIMITER directives. The
-- trigger is idempotent (BEFORE INSERT normalization) for the same reason as
-- the PostgreSQL fixture.

DROP DATABASE IF EXISTS app;
CREATE DATABASE app;
USE app;

SET SESSION cte_max_recursion_depth = 1000000;

CREATE TABLE users (
    id          BIGINT PRIMARY KEY,
    email       VARCHAR(255) NOT NULL,
    email_lower VARCHAR(255) NOT NULL,
    status      VARCHAR(16) NOT NULL DEFAULT 'active',
    created_at  DATETIME NOT NULL DEFAULT '2026-01-01 00:00:00'
) ENGINE = InnoDB;

CREATE TRIGGER trg_normalize_user
    BEFORE INSERT ON users
    FOR EACH ROW SET NEW.email_lower = LOWER(NEW.email);

CREATE TABLE orders (
    id        BIGINT PRIMARY KEY,
    user_id   BIGINT NOT NULL,
    amount    DECIMAL(10, 2) NOT NULL,
    placed_at DATETIME NOT NULL DEFAULT '2026-02-01 00:00:00',
    INDEX idx_orders_user (user_id),
    CONSTRAINT fk_orders_user FOREIGN KEY (user_id) REFERENCES users (id)
) ENGINE = InnoDB;

CREATE UNIQUE INDEX idx_users_email_lower ON users (email_lower);

DELIMITER //
CREATE FUNCTION order_total(uid BIGINT) RETURNS DECIMAL(12, 2)
    DETERMINISTIC READS SQL DATA
BEGIN
    DECLARE total DECIMAL(12, 2);
    SELECT COALESCE(SUM(amount), 0) INTO total FROM orders WHERE user_id = uid;
    RETURN total;
END//
CREATE PROCEDURE count_users(OUT n INT)
BEGIN
    SELECT COUNT(*) INTO n FROM users;
END//
DELIMITER ;

CREATE EVENT ev_noop
    ON SCHEDULE EVERY 1 DAY
    DO SELECT 1;

INSERT INTO users (id, email, email_lower)
WITH RECURSIVE seq(n) AS (
    SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < 2000
)
SELECT n, CONCAT('User', n, '@Example.COM'), '' FROM seq;

INSERT INTO orders (id, user_id, amount)
WITH RECURSIVE seq(n) AS (
    SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < 8000
)
SELECT n, (n % 2000) + 1, n * 1.5 FROM seq;
