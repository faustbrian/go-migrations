package postgres

import (
	"context"
	"errors"

	migrations "github.com/faustbrian/go-migrations/v5"
)

// ErrCommandConflict means the exact command, source, base or prefix differs.
var ErrCommandConflict = errors.New("PostgreSQL migration command conflict")

// ErrCommandOutcomeUnknown requires authoritative discovery under ownership;
// cancellation or a failed commit acknowledgement is never rollback evidence.
var ErrCommandOutcomeUnknown = errors.New("PostgreSQL migration command outcome unknown")

// CommandHistory describes one original native source and its original base.
// Namespace empty selects public.migrations; other values use ForNamespace's
// exact grammar. Base is never advanced by the caller during same-command resume.
type CommandHistory struct {
	// Namespace selects the original physical ledger; empty selects the default.
	Namespace string
	// Source is the complete immutable history, not just the pending suffix.
	Source []migrations.Migration
	// Base is the original acknowledged history, never advanced during resume.
	Base []migrations.Record
}

// CommandBatch is one atomic execution unit. Original native steps and optional
// reviewed owner preflight commit together with their command attribution.
// Only transactional migrations are supported. A fresh preflight requires an
// empty original history and the owner's complete source in a single batch.
// SQL is a reviewed deployment artifact: it must not contain transaction
// control or nontransactional effects, or mutate migration ownership state.
type CommandBatch struct {
	// Namespace must match exactly one original CommandHistory owner.
	Namespace string
	// Migrations is the next contiguous pending prefix for that owner.
	Migrations []migrations.Migration
	// Preflight is reviewed transactional setup for a complete fresh owner only.
	Preflight string
}

// CommandRequest binds one globally unique canonical command to a keyed caller
// fingerprint and complete original sources/base/ordered execution units.
// Fingerprint is caller-owned policy binding, never a raw idempotency key.
// Calls copy collections; callers must not mutate concurrently with entry.
type CommandRequest struct {
	// ID is a stable globally unique ASCII command identifier of 16..128 bytes.
	ID string
	// Fingerprint binds caller policy; it must be nonzero and remain unchanged.
	Fingerprint [32]byte
	// IdempotencyDigest is a purpose-bound keyed digest, never a plaintext key.
	IdempotencyDigest [32]byte
	// Histories is the complete original owner/source/base selection.
	Histories []CommandHistory
	// Batches is the original ordered execution plan, including committed steps.
	Batches []CommandBatch
}

// LedgerSnapshot contains detached original records for one physical owner.
type LedgerSnapshot struct {
	// Namespace identifies the physical ledger from the original command.
	Namespace string
	// Records retains detached original metadata, not renewed/current authority.
	Records []migrations.Record
}

// CommandReceipt atomically attributes one canonical native record to a command.
// Ordered receipts are an exact prefix, never inferred from schema presence.
type CommandReceipt struct {
	// Namespace identifies the owner whose transaction produced Record.
	Namespace string
	// Record is the canonical native ledger entry committed with the SQL effect.
	Record migrations.Record
}

// CommandProgress is an immutable native checkpoint. Its zero value is invalid.
// Command records must be retained while resume or reconciliation is possible.
type CommandProgress struct {
	id                string
	fingerprint       [32]byte
	idempotencyDigest [32]byte
	base              []LedgerSnapshot
	receipts          []CommandReceipt
	complete          bool
}

// ID returns the original stable command identity, or empty for zero.
func (p CommandProgress) ID() string { return p.id }

// Fingerprint returns the exact original caller policy binding.
func (p CommandProgress) Fingerprint() [32]byte { return p.fingerprint }

// IdempotencyDigest returns the original retained keyed binding for recovery.
// It is not a credential or permission to renew/replace the original key.
func (p CommandProgress) IdempotencyDigest() [32]byte { return p.idempotencyDigest }

// Base returns detached original physical-ledger snapshots, not current state.
func (p CommandProgress) Base() []LedgerSnapshot {
	result := make([]LedgerSnapshot, len(p.base))
	for i, b := range p.base {
		result[i] = LedgerSnapshot{Namespace: b.Namespace, Records: append([]migrations.Record(nil), b.Records...)}
	}
	return result
}

// Receipts returns a detached exact durable prefix in execution order.
func (p CommandProgress) Receipts() []CommandReceipt {
	return append([]CommandReceipt(nil), p.receipts...)
}

// Complete reports that every selected batch has durable native attribution.
// It does not revalidate current schema or ledger state; Execute does that.
func (p CommandProgress) Complete() bool { return p.complete }

// CommandSession retains one native global ownership interval and physical
// connection across independent namespaces. Methods serialize with Release;
// each has a bounded context. Release is required; the pool stays borrowed.
type CommandSession struct{ owned *session }

// AcquireCommand acquires native ownership without preparing any schema.
// It preserves existing Runner and Session contracts and their global lock.
func (b *Backend) AcquireCommand(ctx context.Context) (*CommandSession, error) {
	if ctx == nil {
		return nil, ErrInvalidConfig
	}
	owned, err := b.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	return &CommandSession{owned: owned.(*session)}, nil
}

// Release releases/discards the native connection; use a finite independent
// cleanup context after cancellation. It does not close the borrowed pool.
func (s *CommandSession) Release(ctx context.Context) error {
	if s == nil || s.owned == nil || ctx == nil {
		return ErrInvalidConfig
	}
	return s.owned.Release(ctx)
}

func (s *CommandSession) begin(ctx context.Context) (context.Context, func(), error) {
	if s == nil || s.owned == nil || ctx == nil {
		return nil, nil, ErrInvalidConfig
	}
	bounded, finish, err := s.owned.beginOperation(ctx)
	if err != nil {
		return nil, nil, err
	}
	if s.owned.released || s.owned.connection == nil || s.owned.tainted {
		finish()
		return nil, nil, ErrSessionReleased
	}
	return bounded, finish, nil
}

// ReadLedger discovers the exact namespace through the retained connection.
// Missing history stays empty; no schema is prepared or ownership reacquired.
func (s *CommandSession) ReadLedger(ctx context.Context, namespace string) ([]migrations.Record, error) {
	if namespace != "" && !validNamespace(namespace) {
		return nil, ErrInvalidConfig
	}
	bounded, finish, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()
	return readOwnedLedger(bounded, s.owned.connection, namespace, s.taint)
}

// Called only while the native operation gate owns session state.
func (s *CommandSession) taint() { s.owned.tainted = true }

// ReadProgress discovers only native attributed progress, without preparing
// command state. Missing commands return exists=false and an invalid zero value.
func (s *CommandSession) ReadProgress(ctx context.Context, id string) (CommandProgress, bool, error) {
	if !validCommandID(id) {
		return CommandProgress{}, false, ErrInvalidConfig
	}
	bounded, finish, err := s.begin(ctx)
	if err != nil {
		return CommandProgress{}, false, err
	}
	defer finish()
	stored, exists, err := readNativeCommand(bounded, s.owned.connection, id)
	if err != nil || !exists {
		return CommandProgress{}, exists, err
	}
	progress, err := stored.progress()
	return progress, true, err
}

// Execute validates every participating history before any write, then commits
// original transactional SQL and native command-prefix attribution atomically.
// Only the exact original request resumes; external matching progress conflicts.
func (s *CommandSession) Execute(ctx context.Context, req CommandRequest) (CommandProgress, error) {
	return s.execute(ctx, req)
}
