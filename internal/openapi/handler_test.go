package openapi

import "testing"

func TestSetVersion(t *testing.T) {
	orig := specJSON
	t.Cleanup(func() { specJSON = orig })

	SetVersion("dev")
	if string(specJSON) != string(orig) {
		t.Error("dev build changed the spec")
	}
	SetVersion("v9.8.7")
	if got := Version(); got != "9.8.7" {
		t.Errorf("version = %q, want 9.8.7", got)
	}
}
