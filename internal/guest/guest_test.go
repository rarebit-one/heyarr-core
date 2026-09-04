package guest_test

import (
	"reflect"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/guest"
)

func TestIdentityIsReadOnlyAnonymousAndMarked(t *testing.T) {
	id := guest.Identity()

	if !id.Guest {
		t.Fatal("a guest identity must carry the Guest marker so RefuseGuest can see it")
	}
	if id.Anonymous {
		t.Fatal("a guest is not the disabled-auth anonymous admin identity")
	}
	if id.Principal.Kind != "guest" {
		t.Fatalf("principal kind = %q, want guest", id.Principal.Kind)
	}
	if !id.Allows(auth.ScopeRead) {
		t.Fatal("a guest must be allowed to read")
	}
	if id.Allows(auth.ScopeWrite) {
		t.Fatal("a guest must never carry write")
	}
	if id.Allows(auth.ScopeAdmin) {
		t.Fatal("a guest must never carry admin")
	}
}

func TestVisible(t *testing.T) {
	cases := map[string]bool{
		guest.ClassManaged: true,
		guest.ClassLinked:  true,
		guest.ClassVault:   false,
		"":                 false,
		"something-new":    false,
	}
	for class, want := range cases {
		if got := guest.Visible(class); got != want {
			t.Errorf("Visible(%q) = %v, want %v", class, got, want)
		}
	}
}

func TestVisibleClassesIsTheSameAllowlistAsVisible(t *testing.T) {
	classes := guest.VisibleClasses()

	// Sorted and stable, so a SQL IN clause built from it is deterministic.
	if !reflect.DeepEqual(classes, []string{guest.ClassLinked, guest.ClassManaged}) {
		t.Fatalf("VisibleClasses() = %v, want [linked managed]", classes)
	}

	// Every class the list advertises is one Visible admits, and vice versa:
	// the two must not drift, because one guards single items and the other a
	// query.
	for _, c := range classes {
		if !guest.Visible(c) {
			t.Errorf("VisibleClasses() lists %q but Visible(%q) is false", c, c)
		}
	}
	if guest.Visible(guest.ClassVault) {
		t.Error("vault must not be visible")
	}
}
