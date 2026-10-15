package postgres

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	migrations "github.com/faustbrian/go-migrations/v3"
)

const maxCommandBytes = 16 << 20
const maxCommandRecords = 16384

type commandRecordWire struct {
	Kind      migrations.RecordKind
	Version   migrations.Version
	Name      string
	Checksum  string
	AppliedAt time.Time
	Duration  time.Duration
	Dirty     bool
}
type commandMigrationWire struct {
	Version  migrations.Version
	Name     string
	Checksum string
	Mode     migrations.TransactionMode
}
type commandHistoryWire struct {
	Namespace string
	Source    []commandMigrationWire
	Base      []commandRecordWire
}
type commandBatchWire struct {
	Namespace       string
	Steps           []commandMigrationWire
	PreflightDigest [32]byte
}
type commandWire struct {
	Version           uint8
	ID                string
	Fingerprint       [32]byte
	IdempotencyDigest [32]byte
	Histories         []commandHistoryWire
	Batches           []commandBatchWire
}
type commandReceiptWire struct {
	Namespace string
	Record    commandRecordWire
}
type commandStored struct {
	job       commandWire
	identity  [32]byte
	receipts  []commandReceiptWire
	completed int
}

func validCommandID(id string) bool {
	if len(id) < 16 || len(id) > 128 {
		return false
	}
	for i := range len(id) {
		c := id[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

func recordWire(r migrations.Record) commandRecordWire {
	return commandRecordWire{r.Kind(), r.Version(), r.Name(), r.Checksum().String(), r.AppliedAt().UTC(), r.Duration(), r.Dirty()}
}
func (w commandRecordWire) record() (migrations.Record, error) {
	checksum, err := migrations.ParseChecksum(w.Checksum)
	if err != nil {
		return migrations.Record{}, err
	}
	return migrations.NewRecord(w.Kind, w.Version, w.Name, checksum, w.AppliedAt.UTC(), w.Duration, w.Dirty)
}
func migrationWire(m migrations.Migration) commandMigrationWire {
	return commandMigrationWire{m.Version(), m.Name(), m.Checksum().String(), m.TransactionMode()}
}

// Bound input before copying/serialization; native immutable values retain
// strings without copying SQL, and only checksums enter durable command state.
func canonicalCommand(req CommandRequest) (CommandRequest, commandWire, []byte, [32]byte, error) {
	bad := func(err error) (CommandRequest, commandWire, []byte, [32]byte, error) {
		return CommandRequest{}, commandWire{}, nil, [32]byte{}, err
	}
	if !validCommandID(req.ID) || req.Fingerprint == ([32]byte{}) || req.IdempotencyDigest == ([32]byte{}) || len(req.Histories) < 1 || len(req.Histories) > 128 || len(req.Batches) > migrations.MaxMigrationFiles {
		return bad(ErrInvalidConfig)
	}
	result := req
	result.Histories = slices.Clone(req.Histories)
	result.Batches = slices.Clone(req.Batches)
	w := commandWire{Version: 1, ID: req.ID, Fingerprint: req.Fingerprint, IdempotencyDigest: req.IdempotencyDigest}
	owners := make(map[string]CommandHistory)
	pending := make(map[string][]migrations.Step)
	bytesUsed, sourceCount, baseCount, stepCount := 0, 0, 0, 0
	for i, h := range req.Histories {
		if h.Namespace != "" && !validNamespace(h.Namespace) {
			return bad(ErrInvalidConfig)
		}
		if _, exists := owners[h.Namespace]; exists || len(h.Source) == 0 || len(h.Source) > migrations.MaxMigrationFiles || len(h.Base) > migrations.MaxMigrationRecords {
			return bad(ErrInvalidConfig)
		}
		sourceCount += len(h.Source)
		baseCount += len(h.Base)
		if sourceCount > maxCommandRecords || baseCount > maxCommandRecords {
			return bad(ErrResourceLimit)
		}
		plan, err := migrations.PlanUp(h.Source, h.Base)
		if err != nil {
			return bad(err)
		}
		pending[h.Namespace] = plan.Steps()
		copy := CommandHistory{h.Namespace, slices.Clone(h.Source), slices.Clone(h.Base)}
		result.Histories[i] = copy
		owners[h.Namespace] = copy
		wire := commandHistoryWire{Namespace: h.Namespace}
		for _, m := range h.Source {
			if len(m.UpSQL()) > migrations.MaxMigrationSourceBytes-bytesUsed {
				return bad(ErrResourceLimit)
			}
			bytesUsed += len(m.UpSQL())
			if len(m.DownSQL()) > migrations.MaxMigrationSourceBytes-bytesUsed {
				return bad(ErrResourceLimit)
			}
			bytesUsed += len(m.DownSQL())
			wire.Source = append(wire.Source, migrationWire(m))
		}
		for _, r := range h.Base {
			wire.Base = append(wire.Base, recordWire(r))
		}
		w.Histories = append(w.Histories, wire)
	}
	positions := make(map[string]int)
	for i, b := range req.Batches {
		h, exists := owners[b.Namespace]
		if !exists || len(b.Migrations) == 0 || len(b.Migrations) > migrations.MaxMigrationFiles-stepCount {
			return bad(ErrInvalidConfig)
		}
		stepCount += len(b.Migrations)
		if len(b.Preflight) > migrations.MaxMigrationSourceBytes-bytesUsed || !utf8.ValidString(b.Preflight) || strings.IndexByte(b.Preflight, 0) >= 0 || b.Preflight != "" && strings.TrimSpace(b.Preflight) == "" {
			return bad(ErrInvalidConfig)
		}
		bytesUsed += len(b.Preflight)
		if b.Preflight != "" && (len(h.Base) != 0 || positions[b.Namespace] != 0 || len(b.Migrations) != len(h.Source)) {
			return bad(ErrCommandConflict)
		}
		wire := commandBatchWire{Namespace: b.Namespace, PreflightDigest: sha256.Sum256([]byte(b.Preflight))}
		for _, m := range b.Migrations {
			position := positions[b.Namespace]
			steps := pending[b.Namespace]
			if position >= len(steps) || m != steps[position].Migration() {
				return bad(ErrCommandConflict)
			}
			if m.TransactionMode() != migrations.TransactionModeDefault {
				return bad(migrations.ErrInvalidTransactionMode)
			}
			positions[b.Namespace]++
			wire.Steps = append(wire.Steps, migrationWire(m))
		}
		result.Batches[i] = CommandBatch{b.Namespace, slices.Clone(b.Migrations), b.Preflight}
		w.Batches = append(w.Batches, wire)
	}
	wire, err := json.Marshal(w)
	if err != nil || len(wire) > maxCommandBytes {
		return bad(ErrResourceLimit)
	}
	return result, w, wire, commandIdentity(wire), nil
}

func commandIdentity(wire []byte) [32]byte {
	h := sha256.New()
	_, _ = h.Write([]byte("golib/migrations/postgres/command/v1\x00"))
	_, _ = h.Write(wire)
	return [32]byte(h.Sum(nil))
}

func decodeCommandJSON(wire []byte, target any) error {
	if len(wire) == 0 || len(wire) > maxCommandBytes {
		return ErrResourceLimit
	}
	if err := commandJSONBudget(wire); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(wire))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return ErrCommandConflict
	}
	canonical, err := json.Marshal(target)
	if err != nil || !bytes.Equal(canonical, wire) {
		return ErrCommandConflict
	}
	return nil
}

// Check collection/depth budgets in a streaming pass before a typed decoder
// allocates slices from persisted (possibly damaged) metadata.
func commandJSONBudget(wire []byte) error {
	d := json.NewDecoder(bytes.NewReader(wire))
	type frame struct {
		kind  json.Delim
		count int
	}
	stack := make([]frame, 0, 8)
	for {
		token, err := d.Token()
		if err == io.EOF {
			if len(stack) != 0 {
				return ErrCommandConflict
			}
			return nil
		}
		if err != nil {
			return ErrCommandConflict
		}
		delim, isDelimiter := token.(json.Delim)
		closing := isDelimiter && (delim == ']' || delim == '}')
		if !closing && len(stack) > 0 {
			f := &stack[len(stack)-1]
			f.count++
			if f.kind == '[' && f.count > maxCommandRecords || f.kind == '{' && f.count > 32 {
				return ErrResourceLimit
			}
		}
		if isDelimiter {
			if closing {
				stack = stack[:len(stack)-1]
			} else {
				if len(stack) >= 8 {
					return ErrResourceLimit
				}
				stack = append(stack, frame{kind: delim})
			}
		} else if text, ok := token.(string); ok && len(text) > 512 {
			return ErrResourceLimit
		}
	}
}

func (stored commandStored) progress() (CommandProgress, error) {
	w := stored.job
	if w.Version != 1 || !validCommandID(w.ID) || w.Fingerprint == ([32]byte{}) || w.IdempotencyDigest == ([32]byte{}) || len(w.Histories) < 1 || len(w.Histories) > 128 || len(w.Batches) > migrations.MaxMigrationFiles || stored.completed < 0 || stored.completed > len(w.Batches) || len(stored.receipts) > migrations.MaxMigrationFiles {
		return CommandProgress{}, ErrCommandConflict
	}
	p := CommandProgress{id: w.ID, fingerprint: w.Fingerprint, idempotencyDigest: w.IdempotencyDigest, complete: stored.completed == len(w.Batches)}
	baseCount := 0
	sourceCount := 0
	owners := make(map[string]bool)
	for _, h := range w.Histories {
		if h.Namespace != "" && !validNamespace(h.Namespace) || owners[h.Namespace] || len(h.Base) > migrations.MaxMigrationRecords || len(h.Source) == 0 || len(h.Source) > migrations.MaxMigrationFiles {
			return CommandProgress{}, ErrCommandConflict
		}
		owners[h.Namespace] = true
		sourceCount += len(h.Source)
		if sourceCount > maxCommandRecords {
			return CommandProgress{}, ErrResourceLimit
		}
		var previous migrations.Version
		for _, source := range h.Source {
			checksum, err := migrations.ParseChecksum(source.Checksum)
			if err != nil || source.Version <= previous || source.Mode != migrations.TransactionModeDefault && source.Mode != migrations.TransactionModeNone {
				return CommandProgress{}, ErrCommandConflict
			}
			if _, err := migrations.NewRecord(migrations.RecordKindMigration, source.Version, source.Name, checksum, time.Unix(1, 0), 0, false); err != nil {
				return CommandProgress{}, ErrCommandConflict
			}
			previous = source.Version
		}
		baseCount += len(h.Base)
		if baseCount > maxCommandRecords {
			return CommandProgress{}, ErrResourceLimit
		}
		base := LedgerSnapshot{Namespace: h.Namespace}
		for _, record := range h.Base {
			r, err := record.record()
			if err != nil || r.Dirty() {
				return CommandProgress{}, ErrCommandConflict
			}
			base.Records = append(base.Records, r)
		}
		p.base = append(p.base, base)
	}
	position := 0
	stepCount := 0
	for _, b := range w.Batches {
		if !owners[b.Namespace] || len(b.Steps) == 0 || len(b.Steps) > migrations.MaxMigrationFiles-stepCount {
			return CommandProgress{}, ErrCommandConflict
		}
		stepCount += len(b.Steps)
	}
	for i, b := range w.Batches {
		if i >= stored.completed {
			break
		}
		if len(b.Steps) == 0 || len(b.Steps) > len(stored.receipts)-position {
			return CommandProgress{}, ErrCommandConflict
		}
		for _, step := range b.Steps {
			receipt := stored.receipts[position]
			r, err := receipt.Record.record()
			if err != nil || r.Dirty() || r.Kind() != migrations.RecordKindMigration || receipt.Namespace != b.Namespace || r.Version() != step.Version || r.Name() != step.Name || r.Checksum().String() != step.Checksum {
				return CommandProgress{}, ErrCommandConflict
			}
			p.receipts = append(p.receipts, CommandReceipt{receipt.Namespace, r})
			position++
		}
	}
	if position != len(stored.receipts) {
		return CommandProgress{}, ErrCommandConflict
	}
	return p, nil
}
