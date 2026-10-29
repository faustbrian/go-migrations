//go:build integration

package interop_test

import "os"

func envVersion() string { return os.Getenv("POSTGRES_VERSION") }
