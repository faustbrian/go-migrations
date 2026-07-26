-- +migrations NoTransaction
-- +migrations Up
CREATE INDEX CONCURRENTLY widgets_name_idx ON widgets (name);

-- +migrations Down
DROP INDEX CONCURRENTLY widgets_name_idx;
