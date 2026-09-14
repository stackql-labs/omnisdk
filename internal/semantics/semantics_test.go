package semantics_test

import (
	"testing"

	"github.com/stackql-labs/omnisdk/internal/semantics"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// Non-invertibility is the absence of a declared inverse, not a negative flag. A create is
// generically invertible by delete, but a particular create may not be — deletion protection,
// scheduled destruction, object-lock, anything billable.
func TestUndeclaredInverseMeansNonInvertible(t *testing.T) {
	s, err := semantics.New([]semantics.Declaration{
		{Exchange: "CreateBucket", Form: "insert", Inverse: "DeleteBucket", Fidelity: facade.FidelityExact},
		{Exchange: "CreateKmsKey", Form: "insert"},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	if _, ok := s.Inverse("CreateBucket"); !ok {
		t.Error("CreateBucket should be invertible")
	}
	if _, ok := s.Inverse("CreateKmsKey"); ok {
		t.Error("CreateKmsKey declares no inverse and must read as non-invertible")
	}
}

// An inverse that is not the obvious form is expressible, which a form-class rule cannot do.
func TestInverseNeedNotBeTheObviousForm(t *testing.T) {
	s, err := semantics.New([]semantics.Declaration{
		{Exchange: "EnableApi", Form: "update", Inverse: "DisableApi", Fidelity: facade.FidelityExact},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	inv, ok := s.Inverse("EnableApi")
	if !ok || inv.Exchange() != "DisableApi" {
		t.Errorf("inverse = %v (ok=%v), want DisableApi", inv, ok)
	}
}

// Silence about fidelity is not a claim of exactness, so a policy gating on loss errs toward
// asking rather than assuming.
func TestUndeclaredFidelityIsLossy(t *testing.T) {
	s, err := semantics.New([]semantics.Declaration{
		{Exchange: "CreateVm", Form: "insert", Inverse: "DeleteVm"},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	inv, _ := s.Inverse("CreateVm")
	if inv.Fidelity() != facade.FidelityLossy {
		t.Errorf("fidelity = %q, want lossy by default", inv.Fidelity())
	}
}

func TestInverseWithFidelityNoneIsRejected(t *testing.T) {
	if _, err := semantics.New([]semantics.Declaration{
		{Exchange: "CreateVm", Form: "insert", Inverse: "DeleteVm", Fidelity: facade.FidelityNone},
	}); err == nil {
		t.Error("declaring an inverse with fidelity none = nil, want rejection")
	}
}

func TestFormOfMapsVerbsAndDefaultsToRead(t *testing.T) {
	cases := map[string]facade.FormClass{
		"insert": facade.FormCreate,
		"update": facade.FormUpdate,
		"delete": facade.FormDelete,
		"select": facade.FormRead,
		"":       facade.FormRead,
		"weird":  facade.FormRead,
	}
	for verb, want := range cases {
		if got := semantics.FormOf(verb); got != want {
			t.Errorf("FormOf(%q) = %v, want %v", verb, got, want)
		}
	}
}

func TestFromJSON(t *testing.T) {
	s, err := semantics.FromJSON([]byte(`[
		{"exchange":"CreateVpc","form":"insert","inverse":"DeleteVpc","fidelity":"exact"}
	]`))
	if err != nil {
		t.Fatalf("from json: %v", err)
	}
	if f, ok := s.Form("CreateVpc"); !ok || f != facade.FormCreate {
		t.Errorf("form = %v (ok=%v), want create", f, ok)
	}
}
