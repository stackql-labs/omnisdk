// Package merge implements the field-ownership strategies behind facade.Merge.
//
// The whole argument is two rows of the ownership table: a field we asked for last run and no
// longer ask for must be unset, and a field we have never mentioned belongs to another actor and
// must be left alone. Only ThreeWay is correct on both, which is the entire reason n-1 is stored.
//
// Depends on the standard library and facade's contracts, nothing else.
package merge

import (
	"encoding/json"
	"fmt"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// unset is the value that removes a field. A JSON null in a merge document means "remove", per the
// convention every merge-patch API already follows.
var unset any = nil

type object = map[string]any

// Replace enforces desired and unsets everything else the target holds. It ignores prior — with no
// notion of ownership, silence always means remove, which stomps fields another actor owns. Sound
// only under exclusive ownership of the object, and impossible against a PATCH-only API.
func Replace() facade.Merge { return replace{} }

// Additive enforces desired and never removes. It ignores prior too, so a field we drop can never
// be unset — this is why Terraform's Optional+Computed fields can never be removed.
func Additive() facade.Merge { return additive{} }

// ThreeWay enforces desired, unsets what prior claimed and desired dropped, and leaves everything
// else alone. It is the only strategy correct on both contested rows.
func ThreeWay() facade.Merge { return threeWay{} }

type replace struct{}
type additive struct{}
type threeWay struct{}

// Each strategy differs only in which document supplies the fields that a silence removes.
// Replace takes it from actual, so anything the target holds and desired omits goes; ThreeWay
// takes it from prior, so only what we previously claimed goes; Additive takes it from nothing.

func (replace) Apply(_, desired, actual []byte) ([]byte, error) {
	d, a, err := decode2("desired", desired, "actual", actual)
	if err != nil {
		return nil, err
	}
	return encode(enforce(d, a))
}

func (additive) Apply(_, desired, _ []byte) ([]byte, error) {
	d, err := decode("desired", desired)
	if err != nil {
		return nil, err
	}
	return encode(enforce(d, nil))
}

func (threeWay) Apply(prior, desired, _ []byte) ([]byte, error) {
	d, p, err := decode2("desired", desired, "prior", prior)
	if err != nil {
		return nil, err
	}
	return encode(enforce(d, p))
}

// enforce builds the mutation: every field of desired is sent unconditionally — agreement between
// prior and desired is no reason to skip it when the target may have drifted — and every field of
// drops that desired does not mention is unset.
func enforce(desired, drops object) object {
	out := object{}
	for k, dv := range desired {
		if sub, ok := dv.(object); ok {
			out[k] = enforce(sub, childOf(drops, k))
			continue
		}
		out[k] = dv
	}
	for k, ov := range drops {
		if _, kept := desired[k]; kept {
			continue
		}
		// A subtree present in drops and absent from desired is removed whole; there is nothing
		// beneath it we still claim.
		_ = ov
		out[k] = unset
	}
	return out
}

func childOf(o object, k string) object {
	if c, ok := o[k].(object); ok {
		return c
	}
	return nil
}

func decode(what string, b []byte) (object, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var o object
	if err := json.Unmarshal(b, &o); err != nil {
		return nil, fmt.Errorf("merge: %s is not a JSON object: %w", what, err)
	}
	return o, nil
}

func decode2(n1 string, b1 []byte, n2 string, b2 []byte) (object, object, error) {
	o1, err := decode(n1, b1)
	if err != nil {
		return nil, nil, err
	}
	o2, err := decode(n2, b2)
	if err != nil {
		return nil, nil, err
	}
	return o1, o2, nil
}

func encode(o object) ([]byte, error) {
	b, err := json.Marshal(o)
	if err != nil {
		return nil, fmt.Errorf("merge: encode mutation: %w", err)
	}
	return b, nil
}
