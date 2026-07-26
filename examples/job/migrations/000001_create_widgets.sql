-- +migrations Up
CREATE TABLE widgets (
    id bigint PRIMARY KEY,
    name text NOT NULL
);

-- +migrations Down
DROP TABLE widgets;
