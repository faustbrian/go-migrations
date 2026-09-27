package migrations

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestSourceRejectsHostileMetadataWithoutDisclosingNames(t *testing.T) {
	const privateName = "password=private_payload"
	for _, test := range []struct {
		entry  SourceEntry
		target error
	}{
		{entry: SourceEntry{Name: privateName, Directory: true}, target: ErrUnexpectedSourceEntry},
		{entry: SourceEntry{Name: privateName + ".txt"}, target: ErrUnexpectedSourceEntry},
		{entry: SourceEntry{Name: privateName + ".sql"}, target: ErrInvalidFilename},
	} {
		source, err := NewFSSource(fixedSourceFileSystem{entries: []SourceEntry{test.entry}}, ".")
		if err != nil {
			t.Fatal(err)
		}
		_, err = source.Load(context.Background())
		if !errors.Is(err, test.target) {
			t.Fatalf("load error = %v, want %v", err, test.target)
		}
		for _, format := range []string{"%v", "%+v", "%#v", "%q"} {
			if rendered := fmt.Sprintf(format, err); strings.Contains(rendered, "private_payload") || strings.Contains(rendered, "password") {
				t.Fatalf("invalid metadata disclosed in %s: %s", format, rendered)
			}
		}
	}
}
