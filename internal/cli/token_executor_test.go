package cli

import (
	"context"
	"encoding/json"
	"testing"
)

// `heyarr token create <name> --executor` (ADR-0104) mints a restricted
// read,write token, says so in its JSON and its listing, refuses an explicit
// --scopes, and refuses a name already held by an ordinary service. The
// ordinary token's JSON carries no restricted field (the golden files pin that).
func TestTokenCreateExecutor(t *testing.T) {
	cfg := tokenConfig(t)
	ctx := context.Background()

	out, _, err := run(t, ctx, "--config", cfg, "token", "create", "executor", "--executor", "--json")
	if err != nil {
		t.Fatalf("token create --executor: %v", err)
	}
	var created struct {
		Scopes     []string `json:"scopes"`
		Restricted bool     `json:"restricted"`
	}
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatal(err)
	}
	if !created.Restricted || len(created.Scopes) != 2 || created.Scopes[0] != "read" || created.Scopes[1] != "write" {
		t.Errorf("an executor token came back as %+v", created)
	}

	list, _, err := run(t, ctx, "--config", cfg, "token", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Restricted bool `json:"restricted"`
	}
	if err := json.Unmarshal([]byte(list), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].Restricted {
		t.Errorf("the listing does not show the token as restricted: %s", list)
	}

	if _, _, err := run(t, ctx, "--config", cfg, "token", "create", "x", "--executor", "--scopes", "admin"); err == nil {
		t.Error("--executor with --scopes was accepted")
	}
	if _, _, err := run(t, ctx, "--config", cfg, "token", "create", "svc", "--scopes", "read"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run(t, ctx, "--config", cfg, "token", "create", "svc", "--executor"); err == nil {
		t.Error("an executor token was minted for an ordinary service's name")
	}
	if _, _, err := run(t, ctx, "--config", cfg, "token", "create", "executor", "--scopes", "read"); err == nil {
		t.Error("an ordinary token was minted for an executor's name")
	}
}
