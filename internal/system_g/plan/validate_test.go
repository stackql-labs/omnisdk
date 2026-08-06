package plan

import (
	"context"
	"io"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// A required input (In) with neither a β source nor a κ input must be rejected.
func TestValidateRejectsMissingKappaInput(t *testing.T) {
	spec := NewExchangeSpec("CreateNetwork", []string{"project"}, nil, nil, nil)

	if err := Validate(NewPlan([]ExchangeSpec{spec}, nil, nil, nil, nil, nil)); err == nil {
		t.Fatal("expected rejection for missing κ input 'project'")
	}

	// satisfied by a κ input
	if err := Validate(NewPlan([]ExchangeSpec{spec}, nil, nil, map[string]any{"project": "proj"}, nil, nil)); err != nil {
		t.Errorf("κ-satisfied plan should pass: %v", err)
	}

	// empty κ input is treated as absent
	if err := Validate(NewPlan([]ExchangeSpec{spec}, nil, nil, map[string]any{"project": ""}, nil, nil)); err == nil {
		t.Error("empty κ input should be rejected")
	}

	// satisfied by a β edge from an upstream exchange
	p2 := NewPlan(
		[]ExchangeSpec{NewExchangeSpec("A", nil, []string{"x"}, nil, nil), NewExchangeSpec("B", []string{"x"}, nil, nil, nil)},
		[]BetaEdge{NewBetaEdge("A", "B", "x", "x")}, nil, nil, nil, nil)
	if err := Validate(p2); err != nil {
		t.Errorf("β-satisfied plan should pass: %v", err)
	}
}

// Compose must reject an invalid plan instantly on Open — before any exchange runs.
func TestComposeRejectsInstantly(t *testing.T) {
	spec := NewExchangeSpec("CreateNetwork", []string{"project"}, nil,
		func(map[string]any) facade.Operator { t.Fatal("Make must not run on an invalid plan"); return nil }, nil)
	p := NewPlan([]ExchangeSpec{spec}, nil, nil, nil, nil, nil)
	rs := Compose(1, p, io.Discard).Open(context.Background())
	defer rs.Close()
	if rs.Next(context.Background()) {
		t.Fatal("expected no records from a rejected plan")
	}
	if rs.Err() == nil {
		t.Fatal("expected a rejection error from Open")
	}
}
