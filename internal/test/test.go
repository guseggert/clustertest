package test

import (
	"os"
	"testing"
)

func Integration(t *testing.T) {
	if os.Getenv("RUN_INTEGRATION_TESTS") == "" {
		t.SkipNow()
	}
}
