package migrations_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	migrations "github.com/faustbrian/golib/pkg/migrations"
)

type compatibilityManifest struct {
	ContractVersion string                           `json:"contract_version"`
	GooseVersion    string                           `json:"goose_version"`
	Migrations      []compatibilityManifestMigration `json:"migrations"`
}

type compatibilityManifestMigration struct {
	Version  uint64 `json:"version"`
	Name     string `json:"name"`
	Mode     string `json:"mode"`
	Checksum string `json:"checksum"`
	Applied  bool   `json:"applied"`
}

func TestV1CompatibilityCorpusPreservesCanonicalIdentity(t *testing.T) {
	t.Parallel()

	const root = "testdata/compatibility/v1"
	manifestData, err := os.ReadFile(root + "/manifest.json")
	if err != nil {
		t.Fatalf("read compatibility manifest: %v", err)
	}
	var manifest compatibilityManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatalf("decode compatibility manifest: %v", err)
	}
	if manifest.ContractVersion != "v1" || manifest.GooseVersion != "v3.26.0" {
		t.Fatalf("compatibility manifest metadata = %#v", manifest)
	}
	ledgerData, err := os.ReadFile(root + "/ledger.sql")
	if err != nil {
		t.Fatalf("read compatibility ledger: %v", err)
	}
	if strings.Contains(strings.ToLower(string(ledgerData)), "goose") {
		t.Fatal("compatibility ledger contains replaceable adapter identity")
	}

	source, err := migrations.NewFSSource(os.DirFS(root), "migrations")
	if err != nil {
		t.Fatalf("NewFSSource() error = %v", err)
	}
	loaded, err := source.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(loaded) != len(manifest.Migrations) {
		t.Fatalf("Load() count = %d, want %d", len(loaded), len(manifest.Migrations))
	}
	for index, expected := range manifest.Migrations {
		actual := loaded[index]
		mode := "transaction"
		if actual.TransactionMode() == migrations.TransactionModeNone {
			mode = "no_transaction"
		}
		if uint64(actual.Version()) != expected.Version ||
			actual.Name() != expected.Name ||
			mode != expected.Mode ||
			actual.Checksum().String() != expected.Checksum {
			t.Fatalf("migration %d identity = %d %q %q %s, want %#v",
				index,
				actual.Version(),
				actual.Name(),
				mode,
				actual.Checksum(),
				expected,
			)
		}
		persisted := strings.Contains(string(ledgerData), expected.Checksum) &&
			strings.Contains(string(ledgerData), expected.Name)
		if persisted != expected.Applied {
			t.Fatalf("migration %d persisted = %t, want %t", index, persisted, expected.Applied)
		}
	}
}
