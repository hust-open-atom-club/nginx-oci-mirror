package main

import "testing"

func TestReferenceValidation(t *testing.T) {
	for ref, want := range map[string]bool{"latest": true, "v1.0": true, "..": false, "../escape": false, "bad\";": false} {
		if validReference("library/alpine", ref) != want {
			t.Errorf("incorrect validation of %q", ref)
		}
	}
	if validReference("library/../escape", "latest") {
		t.Fatal("accepted repository traversal")
	}
}
