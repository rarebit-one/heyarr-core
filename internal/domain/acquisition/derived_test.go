package acquisition

import (
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
)

// A title-derived attribute value is named as such in §63's reason detail
// (ADR-0091), and an asserted one is not — so an operator can tell which facts
// came from the tracker and which from the release name.
func TestDescribeNamesADerivedValue(t *testing.T) {
	rule := policy.Rule{Attribute: policy.AttrResolution, Op: policy.OpGTE, Value: policy.Num(1080)}

	asserted := describe(rule, policy.Num(2160), true)
	if strings.Contains(asserted, "read from the release name") {
		t.Errorf("an asserted value was annotated as derived: %q", asserted)
	}

	derived := describe(rule, policy.Num(2160).AsDerived(), true)
	if !strings.Contains(derived, "(read from the release name)") {
		t.Errorf("a derived value was not named as read from the title: %q", derived)
	}
	// The comparison prose is otherwise unchanged.
	if !strings.Contains(derived, "at least") {
		t.Errorf("derived detail lost its comparison prose: %q", derived)
	}
}
