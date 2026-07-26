CREATE TABLE public.go_schema_migrations (
    version bigint PRIMARY KEY CHECK (version > 0),
    kind text NOT NULL CHECK (kind IN ('migration', 'baseline')),
    name text NOT NULL CHECK (name <> ''),
    checksum text NOT NULL CHECK (checksum ~ '^sha256:[0-9a-f]{64}$'),
    started_at timestamptz NOT NULL,
    finished_at timestamptz NULL,
    execution_time_ms bigint NOT NULL DEFAULT 0
        CHECK (execution_time_ms >= 0),
    dirty boolean NOT NULL,
    engine text NOT NULL,
    engine_version text NOT NULL,
    CHECK (
        (dirty AND finished_at IS NULL)
        OR (NOT dirty AND finished_at IS NOT NULL)
    )
);

CREATE TABLE historical_widgets (
    id bigint PRIMARY KEY,
    code text NOT NULL
);

INSERT INTO public.go_schema_migrations (
    version,
    kind,
    name,
    checksum,
    started_at,
    finished_at,
    execution_time_ms,
    dirty,
    engine,
    engine_version
) VALUES (
    1,
    'migration',
    'create_historical_widgets',
    'sha256:7b2d2bbcedd48de94dc33f840a0c8dce6dd1d74ecabf6002676fc27515b172f7',
    '2025-01-01T00:00:00Z',
    '2025-01-01T00:00:00.012Z',
    12,
    false,
    'postgres',
    'v1'
);
