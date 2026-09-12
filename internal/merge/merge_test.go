package merge_test

import (
	"encoding/json"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/merge"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// The ownership table, directly. Per field the only question is whether it appears in the previous
// intent and the current one; actual never enters the decision, because it decides drift rather
// than ownership.
//
//	in n-1 | in n | Replace | Additive | ThreeWay
//	  yes  | yes  | enforce | enforce  | enforce
//	  no   | yes  | enforce | enforce  | enforce
//	  yes  | no   | unset   | leave    | unset
//	  no   | no   | unset   | leave    | leave
func TestOwnershipTable(t *testing.T) {
	const (
		enforce = "enforce"
		unset   = "unset"
		leave   = "leave"
	)
	// Every field is present in actual, so Replace has something to remove on the silent rows.
	actual := []byte(`{"both":"drifted","onlyN":"drifted","onlyPrior":"drifted","neither":"drifted"}`)
	prior := []byte(`{"both":"old","onlyPrior":"old"}`)
	desired := []byte(`{"both":"new","onlyN":"new"}`)

	rows := []struct {
		field                          string
		replace, additive, threeWayExp string
	}{
		{"both", enforce, enforce, enforce},
		{"onlyN", enforce, enforce, enforce},
		{"onlyPrior", unset, leave, unset},
		{"neither", unset, leave, leave},
	}

	for _, strategy := range []struct {
		name string
		m    facade.Merge
		want func(r int) string
	}{
		{"Replace", merge.Replace(), func(r int) string { return rows[r].replace }},
		{"Additive", merge.Additive(), func(r int) string { return rows[r].additive }},
		{"ThreeWay", merge.ThreeWay(), func(r int) string { return rows[r].threeWayExp }},
	} {
		t.Run(strategy.name, func(t *testing.T) {
			b, err := strategy.m.Apply(prior, desired, actual)
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("decode mutation: %v", err)
			}
			for i, row := range rows {
				v, present := got[row.field]
				switch strategy.want(i) {
				case enforce:
					if !present || v != "new" {
						t.Errorf("%s: got %v (present=%v), want enforced as new", row.field, v, present)
					}
				case unset:
					if !present || v != nil {
						t.Errorf("%s: got %v (present=%v), want explicit null", row.field, v, present)
					}
				case leave:
					if present {
						t.Errorf("%s: got %v, want absent from the mutation", row.field, v)
					}
				}
			}
		})
	}
}

// "In n → enforce" is unconditional: agreement between prior and desired is no reason to omit a
// field, because the target may have drifted since.
func TestDesiredIsEnforcedEvenWhenPriorAgrees(t *testing.T) {
	b, err := merge.ThreeWay().Apply([]byte(`{"a":"same"}`), []byte(`{"a":"same"}`), []byte(`{"a":"drifted"}`))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["a"] != "same" {
		t.Errorf("a = %v, want same enforced despite agreement with prior", got["a"])
	}
}

func TestNestedObjectsFollowTheSameRule(t *testing.T) {
	b, err := merge.ThreeWay().Apply(
		[]byte(`{"net":{"cidr":"10.0.0.0/8","dns":"old"}}`),
		[]byte(`{"net":{"cidr":"10.0.0.0/16"}}`),
		nil,
	)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	var got map[string]map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["net"]["cidr"] != "10.0.0.0/16" {
		t.Errorf("cidr = %v, want enforced", got["net"]["cidr"])
	}
	if v, present := got["net"]["dns"]; !present || v != nil {
		t.Errorf("dns = %v (present=%v), want explicit null inside the nested object", v, present)
	}
}

// A create is the degenerate case: no prior, so nothing can be unset.
func TestCreateHasNoPriorAndUnsetsNothing(t *testing.T) {
	b, err := merge.ThreeWay().Apply(nil, []byte(`{"a":1}`), nil)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if string(b) != `{"a":1}` {
		t.Errorf("mutation = %s, want just the desired fields", b)
	}
}

func TestMalformedInputIsRejected(t *testing.T) {
	if _, err := merge.ThreeWay().Apply(nil, []byte(`["not an object"]`), nil); err == nil {
		t.Error("apply on a JSON array = nil, want rejection")
	}
}

// A number passes through as the digits it was written as. Decoding into float64 rewrites
// 10000000000000001 as 10000000000000000, so the mutation sent to the provider is not what was
// asked for — and the convergence check then compares the corrupted value against the real one.
func TestNumbersKeepTheirPrecision(t *testing.T) {
	for _, tc := range []struct{ name, desired, want string }{
		{"int64 beyond float64", `{"n":10000000000000001}`, `{"n":10000000000000001}`},
		{"trailing zero is not normalised", `{"n":1.10}`, `{"n":1.10}`},
		{"exponent kept", `{"n":1e3}`, `{"n":1e3}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := merge.ThreeWay().Apply(nil, []byte(tc.desired), nil)
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("mutation = %s, want %s", got, tc.want)
			}
		})
	}
}
