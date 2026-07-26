CREATE TABLE migrations (
    id serial PRIMARY KEY,
    migration text NOT NULL,
    batch integer NOT NULL
);

INSERT INTO migrations (migration, batch) VALUES
    ('2020_01_01_000000_create_users', 1),
    ('2020_01_02_000000_create_orders', 1);

CREATE TABLE users (
    id bigint PRIMARY KEY,
    email text NOT NULL,
    created_at timestamptz
);

CREATE UNIQUE INDEX users_email_unique ON users (email);
