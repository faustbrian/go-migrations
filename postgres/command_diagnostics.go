package postgres

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
)

// ErrCommandSerialization denies diagnostic serialization of execution inputs
// and retained keyed bindings. Native durable encoding is private and SQL-free.
var ErrCommandSerialization = errors.New("migration command serialization forbidden")

// Format redacts source SQL and policy bindings for every formatting verb.
func (CommandRequest) Format(s fmt.State, _ rune) {
	_, _ = io.WriteString(s, "CommandRequest([redacted])")
}

// MarshalJSON denies accidental execution-input serialization.
func (CommandRequest) MarshalJSON() ([]byte, error) { return nil, ErrCommandSerialization }

// MarshalText denies accidental execution-input serialization.
func (CommandRequest) MarshalText() ([]byte, error) { return nil, ErrCommandSerialization }

// LogValue excludes SQL, keyed bindings and retained execution state.
func (CommandRequest) LogValue() slog.Value { return slog.StringValue("CommandRequest([redacted])") }

// Format redacts original source SQL for every formatting verb.
func (CommandHistory) Format(s fmt.State, _ rune) {
	_, _ = io.WriteString(s, "CommandHistory([redacted])")
}

// MarshalJSON denies accidental source serialization.
func (CommandHistory) MarshalJSON() ([]byte, error) { return nil, ErrCommandSerialization }

// MarshalText denies accidental source serialization.
func (CommandHistory) MarshalText() ([]byte, error) { return nil, ErrCommandSerialization }

// LogValue excludes original source SQL and base metadata.
func (CommandHistory) LogValue() slog.Value { return slog.StringValue("CommandHistory([redacted])") }

// Format redacts preflight and migration SQL for every formatting verb.
func (CommandBatch) Format(s fmt.State, _ rune) { _, _ = io.WriteString(s, "CommandBatch([redacted])") }

// MarshalJSON denies accidental executable SQL serialization.
func (CommandBatch) MarshalJSON() ([]byte, error) { return nil, ErrCommandSerialization }

// MarshalText denies accidental executable SQL serialization.
func (CommandBatch) MarshalText() ([]byte, error) { return nil, ErrCommandSerialization }

// LogValue excludes original executable SQL.
func (CommandBatch) LogValue() slog.Value { return slog.StringValue("CommandBatch([redacted])") }

// Format redacts retained command bindings for every formatting verb.
func (CommandProgress) Format(s fmt.State, _ rune) {
	_, _ = io.WriteString(s, "CommandProgress([redacted])")
}

// MarshalJSON denies accidental retained-binding serialization.
func (CommandProgress) MarshalJSON() ([]byte, error) { return nil, ErrCommandSerialization }

// MarshalText denies accidental retained-binding serialization.
func (CommandProgress) MarshalText() ([]byte, error) { return nil, ErrCommandSerialization }

// LogValue excludes retained keyed bindings and prefix metadata.
func (CommandProgress) LogValue() slog.Value { return slog.StringValue("CommandProgress([redacted])") }
