-- +migrations Up
CREATE TABLE historical_widgets (
    id bigint PRIMARY KEY,
    code text NOT NULL
);
-- +migrations Down
DROP TABLE historical_widgets;
