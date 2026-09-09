package migrationsservice

import (
	"errors"
	"testing"
)

func TestTranslateOptionsErrorPreservesUnrelatedErrors(t *testing.T) {
	want := errors.New("unrelated")
	if got := translateOptionsError(want); got != want {
		t.Fatalf("translateOptionsError() = %v, want original error", got)
	}
}
