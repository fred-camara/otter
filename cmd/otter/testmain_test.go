package main

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	_ = os.Setenv("OTTER_AUDIT_DISABLED", "true")
	os.Exit(m.Run())
}
