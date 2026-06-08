package handlers_test

import (
	"context"
	"testing"
)

// ctx returns a fresh background context for DB queries inside tests.
func ctx(t testing.TB) context.Context {
	t.Helper()
	return context.Background()
}
