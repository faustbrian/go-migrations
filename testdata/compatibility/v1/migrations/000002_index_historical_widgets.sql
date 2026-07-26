-- +migrations NoTransaction
-- +migrations Up
CREATE UNIQUE INDEX CONCURRENTLY historical_widgets_code_idx
    ON historical_widgets (code);
-- +migrations Down
DROP INDEX CONCURRENTLY historical_widgets_code_idx;
