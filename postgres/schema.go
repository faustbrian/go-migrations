package postgres

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	migrations "github.com/faustbrian/go-migrations/v2"
)

// ErrInvalidSchemaSnapshot indicates ambiguous catalog data that cannot form a
// reviewed baseline contract.
var ErrInvalidSchemaSnapshot = errors.New("invalid PostgreSQL schema snapshot")

const (
	// MaxSchemaObjects bounds the number of catalog entries in one snapshot.
	MaxSchemaObjects = 10_000
	// MaxSchemaObjectIdentityBytes bounds one canonical catalog identity.
	MaxSchemaObjectIdentityBytes = 4 << 10
	// MaxSchemaObjectDefinitionBytes bounds one canonical catalog definition.
	MaxSchemaObjectDefinitionBytes = 1 << 20
	// MaxSchemaSnapshotBytes bounds aggregate identities and definitions in one
	// retained snapshot.
	MaxSchemaSnapshotBytes = 16 << 20
)

// SchemaObject is one canonical PostgreSQL catalog identity and definition.
// Objects are sorted during fingerprinting, so database row order is irrelevant.
type SchemaObject struct {
	Identity   string
	Definition string
}

const schemaObjectsSQL = `WITH schema_objects AS (
    SELECT
        'relation:' || n.nspname || '.' || c.relname AS object_identity,
		concat_ws(' ', 'kind=' || c.relkind::text, 'persistence=' || c.relpersistence::text,
            'view=' || COALESCE(pg_get_viewdef(c.oid, true), '')) AS definition
    FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
      AND n.nspname !~ '^pg_toast'
      AND c.relkind IN ('r', 'p', 'v', 'm', 'S', 'f')
      AND NOT (n.nspname = 'public' AND c.relname = 'go_schema_migrations')

    UNION ALL

    SELECT
        'column:' || n.nspname || '.' || c.relname || '.' || a.attname,
        concat_ws(' ', format_type(a.atttypid, a.atttypmod),
            CASE WHEN a.attnotnull THEN 'not-null' ELSE 'nullable' END,
            'default=' || COALESCE(pg_get_expr(d.adbin, d.adrelid), ''),
			'identity=' || a.attidentity::text, 'generated=' || a.attgenerated::text,
            'collation=' || COALESCE(coll.collname, ''))
    FROM pg_attribute a
    JOIN pg_class c ON c.oid = a.attrelid
    JOIN pg_namespace n ON n.oid = c.relnamespace
    LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
    LEFT JOIN pg_collation coll ON coll.oid = a.attcollation
	WHERE a.attnum > 0 AND NOT a.attisdropped
	  AND c.relkind IN ('r', 'p', 'v', 'm', 'f')
	  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
      AND n.nspname !~ '^pg_toast'
      AND NOT (n.nspname = 'public' AND c.relname = 'go_schema_migrations')

    UNION ALL

    SELECT 'constraint:' || n.nspname || '.' || c.relname || '.' || con.conname,
           pg_get_constraintdef(con.oid, true)
    FROM pg_constraint con
    JOIN pg_class c ON c.oid = con.conrelid
    JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
      AND n.nspname !~ '^pg_toast'
      AND NOT (n.nspname = 'public' AND c.relname = 'go_schema_migrations')

    UNION ALL

    SELECT 'index:' || n.nspname || '.' || idx.relname, pg_get_indexdef(idx.oid)
    FROM pg_index i
    JOIN pg_class idx ON idx.oid = i.indexrelid
    JOIN pg_class tbl ON tbl.oid = i.indrelid
    JOIN pg_namespace n ON n.oid = tbl.relnamespace
    WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
      AND n.nspname !~ '^pg_toast'
      AND NOT (n.nspname = 'public' AND tbl.relname = 'go_schema_migrations')

    UNION ALL

    SELECT 'trigger:' || n.nspname || '.' || c.relname || '.' || t.tgname,
           pg_get_triggerdef(t.oid, true)
    FROM pg_trigger t
    JOIN pg_class c ON c.oid = t.tgrelid
    JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE NOT t.tgisinternal
      AND n.nspname NOT IN ('pg_catalog', 'information_schema')
      AND n.nspname !~ '^pg_toast'
      AND NOT (n.nspname = 'public' AND c.relname = 'go_schema_migrations')

    UNION ALL

    SELECT 'policy:' || n.nspname || '.' || c.relname || '.' || p.polname,
		   concat_ws(' ', 'command=' || p.polcmd::text, 'permissive=' || p.polpermissive::text,
               'using=' || COALESCE(pg_get_expr(p.polqual, p.polrelid), ''),
               'check=' || COALESCE(pg_get_expr(p.polwithcheck, p.polrelid), ''))
    FROM pg_policy p
    JOIN pg_class c ON c.oid = p.polrelid
    JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
      AND n.nspname !~ '^pg_toast'

    UNION ALL

    SELECT 'type:' || n.nspname || '.' || t.typname,
		   concat_ws(' ', 'kind=' || t.typtype::text, 'category=' || t.typcategory::text,
               'base=' || COALESCE(format_type(t.typbasetype, t.typtypmod), ''),
			   'not-null=' || t.typnotnull::text,
               'default=' || COALESCE(t.typdefault, ''),
               'enum=' || COALESCE((SELECT string_agg(e.enumlabel, ',' ORDER BY e.enumsortorder)
                                    FROM pg_enum e WHERE e.enumtypid = t.oid), ''))
    FROM pg_type t
    JOIN pg_namespace n ON n.oid = t.typnamespace
    WHERE t.typtype IN ('d', 'e')
      AND n.nspname NOT IN ('pg_catalog', 'information_schema')
      AND n.nspname !~ '^pg_toast'

    UNION ALL

    SELECT 'function:' || n.nspname || '.' || p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')',
           pg_get_functiondef(p.oid)
    FROM pg_proc p
    JOIN pg_namespace n ON n.oid = p.pronamespace
    WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
      AND n.nspname !~ '^pg_toast'

    UNION ALL

    SELECT 'extension:' || extname, extversion FROM pg_extension
)
SELECT object_identity, definition FROM schema_objects
ORDER BY object_identity, definition`

// Fingerprint hashes a complete, unambiguous PostgreSQL schema snapshot.
func Fingerprint(objects []SchemaObject) (migrations.Checksum, error) {
	if len(objects) > MaxSchemaObjects {
		return migrations.Checksum{}, ErrResourceLimit
	}
	totalBytes := 0
	for _, object := range objects {
		var err error
		totalBytes, err = validateSchemaObjectBounds(totalBytes, object)
		if err != nil {
			return migrations.Checksum{}, err
		}
	}
	canonical := append([]SchemaObject(nil), objects...)
	slices.SortFunc(canonical, func(left, right SchemaObject) int {
		if identityOrder := cmp.Compare(left.Identity, right.Identity); identityOrder != 0 {
			return identityOrder
		}

		return cmp.Compare(left.Definition, right.Definition)
	})

	var builder strings.Builder
	builder.WriteString("go-migrations/postgres-schema/v1\n")
	for index, object := range canonical {
		if object.Identity == "" || object.Definition == "" ||
			(index > 0 && object.Identity == canonical[index-1].Identity) {
			return migrations.Checksum{}, ErrInvalidSchemaSnapshot
		}
		builder.WriteString("identity:")
		builder.WriteString(strconv.Itoa(len(object.Identity)))
		builder.WriteByte(':')
		builder.WriteString(object.Identity)
		builder.WriteByte('\n')
		builder.WriteString("definition:")
		builder.WriteString(strconv.Itoa(len(object.Definition)))
		builder.WriteByte(':')
		builder.WriteString(object.Definition)
		builder.WriteByte('\n')
	}

	return migrations.ChecksumData([]byte(builder.String())), nil
}

// Inspect returns the current schema fingerprint for review and baseline
// contract generation. It does not mutate either migration ledger.
func (backend *Backend) Inspect(ctx context.Context) (migrations.Checksum, error) {
	if backend == nil || backend.database == nil {
		return migrations.Checksum{}, ErrInvalidConfig
	}

	operationCtx, cancel := boundedOperationContext(ctx, backend.operationTimeout)
	defer cancel()
	connection, err := backend.database.Conn(operationCtx)
	if err != nil {
		return migrations.Checksum{}, databaseContextFailure(operationCtx, "acquire schema inspection connection", err)
	}
	defer func() { _ = connection.Close() }()

	return inspectSchema(operationCtx, connection)
}

// InspectObjects returns the canonical catalog objects used for review and
// fingerprint diagnostics. The package-owned ledger is excluded.
func (backend *Backend) InspectObjects(ctx context.Context) ([]SchemaObject, error) {
	if backend == nil || backend.database == nil {
		return nil, ErrInvalidConfig
	}

	operationCtx, cancel := boundedOperationContext(ctx, backend.operationTimeout)
	defer cancel()
	connection, err := backend.database.Conn(operationCtx)
	if err != nil {
		return nil, databaseContextFailure(operationCtx, "acquire schema inspection connection", err)
	}
	defer func() { _ = connection.Close() }()

	return inspectObjects(operationCtx, connection)
}

func (session *session) Baseline(ctx context.Context, baseline migrations.Baseline) (migrations.Record, error) {
	operationCtx, finish, err := session.beginOperation(ctx)
	if err != nil {
		return migrations.Record{}, err
	}
	defer finish()
	if session.released || session.connection == nil {
		return migrations.Record{}, ErrSessionReleased
	}

	transaction, err := session.connection.BeginTx(operationCtx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return migrations.Record{}, databaseContextFailure(operationCtx, "begin baseline transaction", err)
	}
	defer func() { _ = transaction.Rollback() }()

	fingerprint, err := inspectSchema(operationCtx, transaction)
	if err != nil {
		return migrations.Record{}, err
	}
	if fingerprint != baseline.Fingerprint() {
		return migrations.Record{}, fmt.Errorf(
			"%w: expected %s, got %s",
			migrations.ErrBaselineMismatch,
			baseline.Fingerprint(),
			fingerprint,
		)
	}

	appliedAt := time.Now().UTC()
	_, err = transaction.ExecContext(
		operationCtx,
		`INSERT INTO public.go_schema_migrations (version, kind, name, checksum, started_at, finished_at, execution_time_ms, dirty, engine, engine_version) VALUES ($1, $2, $3, $4, $5, $5, 0, false, 'baseline', 'postgres-schema-v1')`,
		baselineLedgerVersion(baseline),
		"baseline",
		baseline.Name(),
		baseline.Fingerprint().String(),
		appliedAt,
	)
	if err != nil {
		return migrations.Record{}, databaseContextFailure(operationCtx, "insert schema baseline", err)
	}
	if err := transaction.Commit(); err != nil {
		return migrations.Record{}, databaseContextFailure(operationCtx, "commit schema baseline", err)
	}

	return migrations.NewRecord(
		migrations.RecordKindBaseline,
		baseline.Version(),
		baseline.Name(),
		baseline.Fingerprint(),
		appliedAt,
		0,
		false,
	)
}

type schemaQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func baselineLedgerVersion(baseline migrations.Baseline) int64 {
	// #nosec G115 -- Baseline has private fields and NewBaseline rejects
	// versions above math.MaxInt64 before PostgreSQL persistence can receive one.
	return int64(baseline.Version())
}

func inspectSchema(ctx context.Context, queryer schemaQueryer) (migrations.Checksum, error) {
	objects, err := inspectObjects(ctx, queryer)
	if err != nil {
		return migrations.Checksum{}, err
	}

	return Fingerprint(objects)
}

func inspectObjects(ctx context.Context, queryer schemaQueryer) ([]SchemaObject, error) {
	rows, err := queryer.QueryContext(ctx, schemaObjectsSQL)
	if err != nil {
		return nil, databaseContextFailure(ctx, "inspect PostgreSQL schema", err)
	}
	defer func() { _ = rows.Close() }()

	objects := make([]SchemaObject, 0, min(MaxSchemaObjects, 128))
	totalBytes := 0
	for rows.Next() {
		if len(objects) >= MaxSchemaObjects {
			return nil, ErrResourceLimit
		}
		var object SchemaObject
		if err := rows.Scan(&object.Identity, &object.Definition); err != nil {
			return nil, databaseContextFailure(
				ctx,
				"scan PostgreSQL schema object",
				errors.Join(ErrInvalidSchemaSnapshot, err),
			)
		}
		totalBytes, err = validateSchemaObjectBounds(totalBytes, object)
		if err != nil {
			return nil, err
		}
		objects = append(objects, object)
	}
	if err := rows.Err(); err != nil {
		return nil, databaseContextFailure(ctx, "iterate PostgreSQL schema rows", err)
	}

	return objects, nil
}

func validateSchemaObjectBounds(total int, object SchemaObject) (int, error) {
	if len(object.Identity) > MaxSchemaObjectIdentityBytes ||
		len(object.Definition) > MaxSchemaObjectDefinitionBytes {
		return 0, ErrResourceLimit
	}

	return boundedTextBytes(total, MaxSchemaSnapshotBytes, object.Identity, object.Definition)
}
