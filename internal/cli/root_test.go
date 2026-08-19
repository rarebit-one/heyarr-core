package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestExecuteVersionJSON(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Execute([]string{"heyarr", "version", "--json"}, &out, &errb); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, errb.String())
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("version --json is not valid JSON: %v", err)
	}
	if _, ok := got["version"]; !ok {
		t.Errorf("version --json missing %q key: %v", "version", got)
	}
}

func TestExecuteUnknownCommand(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Execute([]string{"heyarr", "nope"}, &out, &errb); code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "unknown command") {
		t.Errorf("stderr = %q, want it to name the unknown command", errb.String())
	}
}

func TestExecuteRolesAreNotImplementedYet(t *testing.T) {
	for _, role := range []string{"controller", "worker", "peer", "all"} {
		var out, errb bytes.Buffer
		if code := Execute([]string{"heyarr", role}, &out, &errb); code != 69 {
			t.Errorf("%s: exit code = %d, want 69", role, code)
		}
	}
}
