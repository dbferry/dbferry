-- dbferry PostgreSQL integration fixture (poc-plan 0.3).
--
-- Exercises schema, data, indexes (plain, unique, partial), foreign keys, a
-- check constraint, an array/jsonb column, and a trigger + trigger function.
--
-- The trigger is deliberately IDEMPOTENT (a BEFORE trigger that normalizes a
-- column). pg_dump restores schema before data, so triggers exist during the
-- COPY data load and fire again on restore; a row-producing trigger would then
-- diverge from the source. Normalizing to a value already present in the dumped
-- data is a no-op on re-fire, so source and restored stay byte-comparable.

DROP SCHEMA IF EXISTS app CASCADE;
CREATE SCHEMA app;
SET search_path TO app;

CREATE TABLE users (
    id          bigint PRIMARY KEY,
    email       text NOT NULL,
    email_lower text NOT NULL,
    status      text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    profile     jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at  timestamptz NOT NULL DEFAULT '2026-01-01T00:00:00Z'
);

CREATE FUNCTION normalize_user() RETURNS trigger AS $$
BEGIN
    NEW.email_lower := lower(NEW.email);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_normalize_user
    BEFORE INSERT OR UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION normalize_user();

CREATE TABLE orders (
    id        bigint PRIMARY KEY,
    user_id   bigint NOT NULL REFERENCES users(id),
    amount    numeric(10, 2) NOT NULL,
    tags      text[] NOT NULL DEFAULT '{}',
    placed_at timestamptz NOT NULL DEFAULT '2026-02-01T00:00:00Z'
);

CREATE INDEX idx_orders_user ON orders (user_id);
CREATE UNIQUE INDEX idx_users_email_lower ON users (email_lower);
CREATE INDEX idx_users_active ON users (id) WHERE status = 'active';

-- email_lower is passed empty on purpose; the trigger fills it. This proves the
-- trigger ran (in the source) and re-runs harmlessly (on restore).
INSERT INTO users (id, email, email_lower, profile)
SELECT g, 'User' || g || '@Example.COM', '', jsonb_build_object('n', g)
FROM generate_series(1, 2000) g;

INSERT INTO orders (id, user_id, amount, tags)
SELECT g, (g % 2000) + 1, (g * 1.5)::numeric(10, 2), ARRAY['t' || (g % 5)]
FROM generate_series(1, 8000) g;
