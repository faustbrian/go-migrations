CREATE TABLE migrations (
    id serial PRIMARY KEY,
    migration text NOT NULL,
    batch integer NOT NULL
);

INSERT INTO migrations (migration, batch) VALUES
    ('2020_01_01_000000_create_users', 1),
    ('2020_01_02_000000_create_orders', 1);
