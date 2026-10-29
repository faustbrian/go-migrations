package postgres

import "strings"

const namespaceLedgerPrefix = "golib_migrations_"

// ForNamespace derives an IO-free backend for one independent migration owner.
// It borrows the same pool and copies timeout policy without changing the
// receiver. Namespace is canonical lowercase ASCII snake case, starts with a
// letter, and contains 1..46 bytes. Empty, uppercase, ambiguous or oversized
// names return ErrInvalidConfig; values are never normalized or truncated.
//
// The exact ledger is public.golib_migrations_<namespace>. No other history is
// discovered, adopted, renamed or merged. Retain the same namespace throughout
// an owner's lifetime; changing it selects empty/different authority, not an
// upgrade. Selection does not alter migration SQL, versions or checksums and is
// not an authorization boundary. The default backend still owns public.migrations.
//
// Every operation, including inspection/baseline and atomic installation, uses
// the selected ledger. Schema inspection remains database-wide: only this
// ledger's objects are excluded; nonselected ledgers remain visible. All owners
// retain the same native advisory lock so schema mutation stays serialized.
// Derived backends are safe for concurrent use with immutable configuration;
// the caller retains pool ownership. Nil/zero receivers deny without IO.
func (backend *Backend) ForNamespace(namespace string) (*Backend, error) {
	if backend == nil || backend.pool == nil || !validNamespace(namespace) {
		return nil, ErrInvalidConfig
	}
	derived := *backend
	derived.namespace = namespace
	return &derived, nil
}

func validNamespace(namespace string) bool {
	if len(namespace) == 0 || len(namespace) > 63-len(namespaceLedgerPrefix) || namespace[0] < 'a' || namespace[0] > 'z' {
		return false
	}
	underscore := false
	for index := 1; index < len(namespace); index++ {
		value := namespace[index]
		if value == '_' {
			if underscore {
				return false
			}
			underscore = true
			continue
		}
		if (value < 'a' || value > 'z') && (value < '0' || value > '9') {
			return false
		}
		underscore = false
	}
	return !underscore
}

func ledgerName(namespace string) string {
	if namespace == "" {
		return "migrations"
	}
	return namespaceLedgerPrefix + namespace
}

// Only native ledger statements pass here, never caller-owned migration SQL.
// Namespace has already passed canonical bounded identifier validation.
func ledgerSQL(query, namespace string) string {
	if namespace == "" {
		return query
	}
	return strings.ReplaceAll(query, "public.migrations", "public."+ledgerName(namespace))
}

func schemaSQL(namespace string) string {
	if namespace == "" {
		return schemaObjectsSQL
	}
	query := strings.ReplaceAll(schemaObjectsSQL, ".relname = 'migrations'", ".relname = '"+ledgerName(namespace)+"'")
	const policyPrefix = "FROM pg_policy p\n    JOIN pg_class c ON c.oid = p.polrelid\n    JOIN pg_namespace n ON n.oid = c.relnamespace\n    WHERE"
	return strings.Replace(query, policyPrefix, policyPrefix+" NOT (n.nspname = 'public' AND c.relname = '"+ledgerName(namespace)+"')\n      AND", 1)
}
