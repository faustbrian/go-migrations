package postgres

import (
	"context"
	"strings"
	"unicode/utf8"
)

// CatalogKind identifies bounded PostgreSQL ownership metadata, not SQL.
// Zero is invalid. Relations include indexes and other pg_class entries.
type CatalogKind uint8

const (
	// CatalogSchema identifies a namespace, including an empty owned schema.
	CatalogSchema CatalogKind = iota + 1
	// CatalogRelation identifies one named relation/index in its exact schema.
	CatalogRelation
	// CatalogRole identifies one cluster role, with Schema and Member empty.
	CatalogRole
	// CatalogColumn identifies one non-dropped relation column in Member.
	CatalogColumn
	// CatalogFunction identifies any overload with the exact schema/name.
	CatalogFunction
)

// CatalogIdentity is one exact PostgreSQL identifier tuple. Each populated
// identifier is 1..63 valid UTF-8 bytes without NUL; no normalization occurs.
// Schema uses Schema alone; role uses Name alone; relation/function use
// Schema+Name; column additionally requires Member. Other fields must be empty.
type CatalogIdentity struct {
	// Kind selects the identifier tuple grammar and the catalog query.
	Kind CatalogKind
	// Schema names the exact PostgreSQL namespace; it is empty for roles.
	Schema string
	// Name names the object within Schema, or the cluster role for CatalogRole.
	Name string
	// Member names the column for CatalogColumn and is otherwise empty.
	Member string
}

func validCatalogIdentity(o CatalogIdentity) bool {
	name := func(s string) bool {
		return len(s) > 0 && len(s) <= 63 && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
	}
	switch o.Kind {
	case CatalogSchema:
		return name(o.Schema) && o.Name == "" && o.Member == ""
	case CatalogRole:
		return o.Schema == "" && name(o.Name) && o.Member == ""
	case CatalogRelation, CatalogFunction:
		return name(o.Schema) && name(o.Name) && o.Member == ""
	case CatalogColumn:
		return name(o.Schema) && name(o.Name) && name(o.Member)
	default:
		return false
	}
}

// CatalogPresence reads 1..4096 exact identities in one statement through the
// retained native ownership connection, without preparing or reacquiring any
// ledger. Results preserve every input position, including duplicates, and
// belong to the caller. This is presence evidence, not schema adoption proof.
// Cancellation or failure returns no partial results. PostgreSQL is borrowed;
// method calls serialize with Execute/Release and obey the native timeout.
func (s *CommandSession) CatalogPresence(ctx context.Context, objects []CatalogIdentity) ([]bool, error) {
	if len(objects) < 1 || len(objects) > 4096 {
		return nil, ErrInvalidConfig
	}
	kinds := make([]int16, len(objects))
	schemas, names, members := make([]string, len(objects)), make([]string, len(objects)), make([]string, len(objects))
	for i, o := range objects {
		if !validCatalogIdentity(o) {
			return nil, ErrInvalidConfig
		}
		kinds[i], schemas[i], names[i], members[i] = int16(o.Kind), o.Schema, o.Name, o.Member
	}
	bounded, finish, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()
	rows, err := s.owned.connection.QueryContext(bounded, `SELECT i.position,
 CASE i.kind
 WHEN 1 THEN EXISTS(SELECT 1 FROM pg_catalog.pg_namespace n WHERE n.nspname=i.schema)
 WHEN 2 THEN EXISTS(SELECT 1 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=i.schema AND c.relname=i.name)
 WHEN 3 THEN EXISTS(SELECT 1 FROM pg_catalog.pg_roles r WHERE r.rolname=i.name)
 WHEN 4 THEN EXISTS(SELECT 1 FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_class c ON c.oid=a.attrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=i.schema AND c.relname=i.name AND a.attname=i.member AND NOT a.attisdropped)
 WHEN 5 THEN EXISTS(SELECT 1 FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname=i.schema AND p.proname=i.name)
 ELSE false END
 FROM unnest($1::smallint[],$2::text[],$3::text[],$4::text[]) WITH ORDINALITY AS i(kind,schema,name,member,position) ORDER BY i.position`, kinds, schemas, names, members)
	if err != nil {
		return nil, databaseContextFailure(bounded, "discover owned catalog presence", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]bool, 0, len(objects))
	for rows.Next() {
		var position int
		var present bool
		if err := rows.Scan(&position, &present); err != nil {
			return nil, databaseContextFailure(bounded, "read owned catalog presence", err)
		}
		if position != len(result)+1 || len(result) >= len(objects) {
			return nil, ErrResourceLimit
		}
		result = append(result, present)
	}
	if err := rows.Err(); err != nil {
		return nil, databaseContextFailure(bounded, "finish owned catalog presence", err)
	}
	if len(result) != len(objects) {
		return nil, ErrLedgerConflict
	}
	return result, nil
}
