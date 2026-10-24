package store

import (
	"path/filepath"
	"testing"
)

func TestFrameworkDefaultsAreEmpty(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(db.Services) != 0 {
		t.Fatalf("fresh install should have no service entries, got %d", len(db.Services))
	}
	if got := DefaultNtfyURL(); got != "" {
		t.Fatalf("fresh install should not configure a notification channel, got %q", got)
	}
	if db.TaskWatch.On {
		t.Fatal("fresh install should not enable task watching")
	}
	if len(db.TaskWatch.Roots) != 0 {
		t.Fatalf("fresh install should not scan task roots, got %v", db.TaskWatch.Roots)
	}
}
