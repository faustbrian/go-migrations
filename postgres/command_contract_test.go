package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	migrations "github.com/faustbrian/go-migrations/v3"
)

func TestCommandDiagnosticsRedactSourceAndBindings(t *testing.T) {
	const marker = "command_sensitive_preflight"
	b := CommandBatch{Preflight: marker}
	req := CommandRequest{ID: marker, Fingerprint: [32]byte{1}, IdempotencyDigest: [32]byte{2}, Batches: []CommandBatch{b}}
	for _, value := range []any{b, req, CommandHistory{}, CommandProgress{}} {
		if text := fmt.Sprintf("%v %+v %#v", value, value, value); strings.Contains(text, marker) || !strings.Contains(text, "redacted") {
			t.Fatal("command diagnostics expose internal state")
		}
		if _, err := json.Marshal(value); err == nil {
			t.Fatal("command diagnostic serialization accepted")
		}
	}
}

func TestCommandStoredJSONBudgets(t *testing.T) {
	for _, input := range []string{
		strings.Repeat("[", 9) + "0" + strings.Repeat("]", 9),
		`["` + strings.Repeat("a", 513) + `"]`,
		"[" + strings.Repeat("0,", maxCommandRecords) + "0]",
	} {
		if err := commandJSONBudget([]byte(input)); !errors.Is(err, ErrResourceLimit) {
			t.Fatal("oversized metadata accepted", err)
		}
	}
	for _, input := range []string{"]", "}", "[[}", "{", `{"x":`} {
		if err := commandJSONBudget([]byte(input)); !errors.Is(err, ErrCommandConflict) {
			t.Fatal("damaged metadata accepted", err)
		}
	}
}

func FuzzCommandStoredJSON(f *testing.F) {
	for _, seed := range []string{"null", "{}", "[]", "]", "{", `{"Version":1}`, `[[[[[[[[[0]]]]]]]]]`} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		if len(input) > 64<<10 {
			t.Skip()
		}
		var stored commandWire
		if decodeCommandJSON(input, &stored) == nil {
			// A successful decode is canonical, but not necessarily a valid job.
			_, _ = (commandStored{job: stored}).progress()
		}
	})
}

func TestCommandZeroOwnerAndCatalogBounds(t *testing.T) {
	var owner *CommandSession
	if _, err := owner.ReadLedger(context.Background(), ""); !errors.Is(err, ErrInvalidConfig) {
		t.Fatal(err)
	}
	if _, _, err := owner.ReadProgress(context.Background(), "command_identity_0001"); !errors.Is(err, ErrInvalidConfig) {
		t.Fatal(err)
	}
	if err := owner.Release(context.Background()); !errors.Is(err, ErrInvalidConfig) {
		t.Fatal(err)
	}
	for _, o := range []CatalogIdentity{
		{}, {Kind: CatalogSchema, Schema: "public", Name: "unexpected"},
		{Kind: CatalogRole, Schema: "public", Name: "role"},
		{Kind: CatalogColumn, Schema: "public", Name: "relation"},
		{Kind: CatalogRelation, Schema: "public", Name: strings.Repeat("a", 64)},
		{Kind: CatalogFunction, Schema: "public", Name: "invalid\x00"},
		{Kind: CatalogFunction, Schema: "public", Name: "\xff"},
	} {
		if validCatalogIdentity(o) {
			t.Fatal("invalid catalog tuple accepted")
		}
	}
	for _, o := range []CatalogIdentity{
		{Kind: CatalogSchema, Schema: "public"},
		{Kind: CatalogRole, Name: "role"},
		{Kind: CatalogRelation, Schema: "public", Name: strings.Repeat("a", 63)},
		{Kind: CatalogColumn, Schema: "public", Name: "relation", Member: "column"},
		{Kind: CatalogFunction, Schema: "public", Name: "quoted'identifier"},
	} {
		if !validCatalogIdentity(o) {
			t.Fatal("valid exact catalog tuple rejected")
		}
	}
}

func TestCommandCanonicalBoundsAndIdentity(t *testing.T) {
	m, err := migrations.NewMigration(1, "command", migrations.TransactionModeDefault, "SELECT 1", "")
	if err != nil {
		t.Fatal(err)
	}
	req := CommandRequest{ID: "command_identity_0001", Fingerprint: [32]byte{1}, IdempotencyDigest: [32]byte{2}, Histories: []CommandHistory{{Namespace: "owner", Source: []migrations.Migration{m}}}, Batches: []CommandBatch{{Namespace: "owner", Migrations: []migrations.Migration{m}}}}
	owned, wire, encoded, digest, err := canonicalCommand(req)
	if err != nil || digest == ([32]byte{}) {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "SELECT") {
		t.Fatal("durable metadata contains SQL")
	}
	owned.Histories[0].Source[0] = migrations.Migration{}
	owned.Batches[0].Migrations[0] = migrations.Migration{}
	if req.Histories[0].Source[0] != m || req.Batches[0].Migrations[0] != m {
		t.Fatal("canonical input retained caller collection aliases")
	}
	var decoded commandWire
	if err := decodeCommandJSON(encoded, &decoded); err != nil || decoded.ID != req.ID {
		t.Fatal("canonical roundtrip", err)
	}
	if err := decodeCommandJSON(append(encoded, ' '), &decoded); !errors.Is(err, ErrCommandConflict) {
		t.Fatal("noncanonical stored state accepted")
	}
	mutations := []func(*CommandRequest){
		func(r *CommandRequest) { r.ID = "command_identity_0002" },
		func(r *CommandRequest) { r.Fingerprint[0]++ },
		func(r *CommandRequest) { r.IdempotencyDigest[0]++ },
		func(r *CommandRequest) { r.Batches = nil },
	}
	for i, mutate := range mutations {
		copy := req
		mutate(&copy)
		_, _, _, changed, err := canonicalCommand(copy)
		if err != nil || changed == digest {
			t.Fatal("unbound command identity", i, err)
		}
	}
	for _, id := range []string{"", strings.Repeat("a", 15), strings.Repeat("a", 129), "command identity_1", "command_identity_é"} {
		copy := req
		copy.ID = id
		if _, _, _, _, err := canonicalCommand(copy); !errors.Is(err, ErrInvalidConfig) {
			t.Fatal("invalid identity accepted", err)
		}
	}
	for _, id := range []string{strings.Repeat("a", 16), strings.Repeat("a", 128)} {
		copy := req
		copy.ID = id
		if _, _, _, _, err := canonicalCommand(copy); err != nil {
			t.Fatal("identity boundary rejected", err)
		}
	}
	copy := req
	copy.Histories = append(copy.Histories, copy.Histories[0])
	if _, _, _, _, err := canonicalCommand(copy); !errors.Is(err, ErrInvalidConfig) {
		t.Fatal("duplicate physical history accepted", err)
	}
	copy = req
	copy.Batches = []CommandBatch{{Namespace: "foreign", Migrations: []migrations.Migration{m}}}
	if _, _, _, _, err := canonicalCommand(copy); !errors.Is(err, ErrInvalidConfig) {
		t.Fatal("foreign batch accepted", err)
	}
	copy = req
	copy.Batches = []CommandBatch{{Namespace: "owner", Migrations: []migrations.Migration{m, m}}}
	if _, _, _, _, err := canonicalCommand(copy); !errors.Is(err, ErrCommandConflict) {
		t.Fatal("duplicate execution accepted", err)
	}
	stored := commandStored{job: wire, identity: digest, completed: 1}
	if _, err := stored.progress(); !errors.Is(err, ErrCommandConflict) {
		t.Fatal("missing atomic attribution accepted", err)
	}
}
