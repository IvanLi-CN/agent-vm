package main

import (
	"strings"
	"testing"
)

func TestInitializeStateRejectsAlternateTargetsBeforeOpeningState(t *testing.T) {
	for _, args := range [][]string{{"--config", "/tmp/other.json"}, {"--manifest", "/tmp/other.json"}, {"unexpected"}} {
		if err := initializeState(args); err == nil {
			t.Fatal("unsafe initialization accepted")
		}
	}
	if err := initializeState([]string{"--config", "/tmp/other.json"}); err == nil || !strings.Contains(err.Error(), "fixed initial blue slot") {
		t.Fatalf("wrong error: %v", err)
	}
}
