package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Value is a rule's operand.
//
// It is a tagged union rather than an `any`, because an `any` decoded from
// JSON gives back float64 for every number, and a resolution that is
// 1080.0000000001 after a round trip is a comparison that fails for reasons
// nobody can see. Exactly one field is meaningful, decided by Kind.
//
// On the wire it renders as the bare JSON value — 1080, "remux", true,
// ["remux", "bluray"] — so a profile reads the way §62 writes one, and a rule
// does not carry a redundant type tag the attribute already implies.
type Value struct {
	Kind Kind
	// Num is set when Kind is KindInt.
	Num int64
	// Text is set when Kind is KindText and the operator is a single name.
	Text string
	// Texts is set when Kind is KindText and the operator is a set (in, nin).
	Texts []string
	// Flag is set when Kind is KindFlag.
	Flag bool
	// set distinguishes a zero Value from an absent one. A rule with no
	// operand at all is a different mistake from a rule whose operand is 0,
	// and they need different messages.
	set bool
}

// Num builds an integer operand.
func Num(n int64) Value { return Value{Kind: KindInt, Num: n, set: true} }

// Text builds a single-name operand, normalised.
func Text(s string) Value { return Value{Kind: KindText, Text: normaliseText(s), set: true} }

// Texts builds a set operand, each member normalised.
func Texts(ss ...string) Value {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, normaliseText(s))
	}
	return Value{Kind: KindText, Texts: out, set: true}
}

// Flag builds a boolean operand.
func Flag(b bool) Value { return Value{Kind: KindFlag, Flag: b, set: true} }

// IsSet reports whether an operand was supplied at all.
func (v Value) IsSet() bool { return v.set }

// isSetValue reports whether this operand is a set rather than a single name.
func (v Value) isSetValue() bool { return v.Texts != nil }

// String renders the operand for a human, in an error or a §63 reason.
func (v Value) String() string {
	if !v.set {
		return "(none)"
	}
	switch v.Kind {
	case KindInt:
		return strconv.FormatInt(v.Num, 10)
	case KindFlag:
		return strconv.FormatBool(v.Flag)
	case KindText:
		if v.isSetValue() {
			return "[" + strings.Join(v.Texts, ", ") + "]"
		}
		return v.Text
	}
	return "(unknown)"
}

// MarshalJSON renders the operand as the bare JSON value.
func (v Value) MarshalJSON() ([]byte, error) {
	if !v.set {
		return []byte("null"), nil
	}
	switch v.Kind {
	case KindInt:
		return json.Marshal(v.Num)
	case KindFlag:
		return json.Marshal(v.Flag)
	case KindText:
		if v.isSetValue() {
			return json.Marshal(v.Texts)
		}
		return json.Marshal(v.Text)
	}
	return nil, fmt.Errorf("policy: a value of unknown kind %q cannot be encoded", v.Kind)
}

// UnmarshalJSON reads a bare JSON value and infers its shape.
//
// The KIND is inferred from the JSON, not from the attribute, and then checked
// against the attribute during validation. Doing it the other way round —
// coercing whatever arrived into the attribute's kind — would turn
// `{"attribute": "resolution", "value": "1080"}` into a silently-working rule
// and then a differently-silently-working one when someone sends "1O80".
func (v *Value) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "null" || trimmed == "" {
		*v = Value{}
		return nil
	}
	switch trimmed[0] {
	case '[':
		var ss []string
		if err := json.Unmarshal(data, &ss); err != nil {
			return errors.New("a set operand must be a list of names, like [\"remux\", \"bluray\"]")
		}
		*v = Texts(ss...)
		// Texts(nothing) and an explicit [] must stay tellable apart from a
		// single name, so the empty set is still a set. Validation refuses it.
		if v.Texts == nil {
			v.Texts = []string{}
		}
		return nil
	case '"':
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*v = Text(s)
		return nil
	case 't', 'f':
		var b bool
		if err := json.Unmarshal(data, &b); err != nil {
			return err
		}
		*v = Flag(b)
		return nil
	default:
		// A number. Reject a fractional one rather than truncating: nothing in
		// the vocabulary is fractional, so 1080.5 is a typo and rounding it is
		// how a typo becomes a working rule that means something else.
		var n int64
		dec := json.NewDecoder(strings.NewReader(trimmed))
		dec.UseNumber()
		var raw json.Number
		if err := dec.Decode(&raw); err != nil {
			return fmt.Errorf("a value must be a number, a name, a list of names or a boolean, not %s", trimmed)
		}
		n, err := strconv.ParseInt(raw.String(), 10, 64)
		if err != nil {
			return fmt.Errorf("a numeric value must be a whole number, not %s", raw.String())
		}
		*v = Num(n)
		return nil
	}
}

// describe names the JSON shape that arrived, for an error message.
func (v Value) describe() string {
	if !v.set {
		return "nothing"
	}
	switch v.Kind {
	case KindInt:
		return "a number"
	case KindFlag:
		return "a boolean"
	case KindText:
		if v.isSetValue() {
			return "a list of names"
		}
		return "a name"
	}
	return "an unknown value"
}
