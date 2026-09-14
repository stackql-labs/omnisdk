// Package semantics carries the per-provider metadata that cannot be derived by differencing
// schemas across a discovery document.
//
// Most of the resource model *is* derivable — in the create body means settable, response-only
// means computed, in the update body means mutable, in create but not update means immutable. What
// is left over is write-only fields, async completion conditions, cross-field constraints, whether
// create accepts an idempotency token, and reversibility. That residue is per-provider data, not
// hand-written code per resource, which is the difference from writing a provider by hand.
package semantics

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// Declaration is one exchange's semantics as authored. Reversibility is declared by naming the
// inverse: its absence is the declaration of non-invertibility, which also covers inverses that
// are not the obvious form — a disable undoing an enable.
type Declaration struct {
	Exchange string          `json:"exchange"`
	Form     string          `json:"form"`
	Inverse  string          `json:"inverse,omitempty"`
	Fidelity facade.Fidelity `json:"fidelity,omitempty"`
	// Update names the exchange that converges an object that already exists. Set it to Exchange
	// itself where the create is an upsert; leave it empty where re-issuing would mint a duplicate,
	// as an EC2 CreateVpc does.
	Update string `json:"update,omitempty"`
}

type inverse struct {
	exchange string
	fidelity facade.Fidelity
}

func (i inverse) Exchange() string          { return i.exchange }
func (i inverse) Fidelity() facade.Fidelity { return i.fidelity }

type static struct {
	forms    map[string]facade.FormClass
	inverses map[string]inverse
	updates  map[string]string
}

// FormOf maps a SQL verb, as the parsed provider documents carry it, onto the system-wide form
// class. Read is the default: an exchange that does not say it mutates is not treated as if it
// does.
func FormOf(verb string) facade.FormClass {
	switch strings.ToLower(verb) {
	case "insert", "create":
		return facade.FormCreate
	case "update", "replace", "patch":
		return facade.FormUpdate
	case "delete":
		return facade.FormDelete
	default:
		return facade.FormRead
	}
}

// New returns Semantics over the given declarations.
func New(decls []Declaration) (facade.Semantics, error) {
	s := &static{forms: map[string]facade.FormClass{}, inverses: map[string]inverse{}, updates: map[string]string{}}
	for _, d := range decls {
		if d.Exchange == "" {
			return nil, fmt.Errorf("semantics: declaration with no exchange")
		}
		s.forms[d.Exchange] = FormOf(d.Form)
		if d.Update != "" {
			s.updates[d.Exchange] = d.Update
		}
		if d.Inverse == "" {
			continue
		}
		f := d.Fidelity
		if f == "" {
			// Silence about fidelity is not a claim of exactness: an undeclared inverse fidelity
			// is treated as lossy, so a policy that gates on loss errs toward asking.
			f = facade.FidelityLossy
		}
		if f == facade.FidelityNone {
			return nil, fmt.Errorf("semantics: %s declares an inverse with fidelity none", d.Exchange)
		}
		s.inverses[d.Exchange] = inverse{exchange: d.Inverse, fidelity: f}
	}
	return s, nil
}

// FromJSON loads declarations from provider metadata.
func FromJSON(b []byte) (facade.Semantics, error) {
	var decls []Declaration
	if err := json.Unmarshal(b, &decls); err != nil {
		return nil, fmt.Errorf("semantics: decode: %w", err)
	}
	return New(decls)
}

func (s *static) Form(exchange string) (facade.FormClass, bool) {
	f, ok := s.forms[exchange]
	return f, ok
}

func (s *static) Update(exchange string) (string, bool) {
	u, ok := s.updates[exchange]
	return u, ok
}

func (s *static) Inverse(exchange string) (facade.InverseExchange, bool) {
	i, ok := s.inverses[exchange]
	if !ok {
		return nil, false
	}
	return i, true
}
