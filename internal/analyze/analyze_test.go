package analyze_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/analyze"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

type method struct {
	name     string
	required []string
	accepts  []string
	body     bool
}

func (m method) Name() string              { return m.name }
func (m method) Required() []string        { return m.required }
func (m method) TakesBody() bool           { return m.body }
func (m method) Accepts(input string) bool { return m.body || slices.Contains(m.accepts, input) }

type caps map[string][]facade.Method

func (c caps) Methods(resource, verb string) ([]facade.Method, error) { return c[verb], nil }

type refuse string

func (r refuse) Permit(_ context.Context, _ facade.Statement, m facade.Method) error {
	if m.Name() == string(r) {
		return errors.New("refused by policy")
	}
	return nil
}

var (
	list = method{name: "list", accepts: []string{"region"}}
	get  = method{name: "get", required: []string{"id"}, accepts: []string{"id", "region"}}
	// A select bound beside list that needs a body is not a read.
	check   = method{name: "check", body: true}
	tenancy = method{name: "modify_tenancy", required: []string{"id"}, accepts: []string{"id", "tenancy"}}
	cidr    = method{name: "associate_cidr", required: []string{"id"}, accepts: []string{"id", "cidr"}}
)

func analyze1(t *testing.T, c caps, p facade.Policy, verb string, available, sent []string, pinned string) (string, error) {
	t.Helper()
	m, err := analyze.New(c, p).Analyze(context.Background(), analyze.NewStatement(facade.AnonymousPrincipal, "r", verb, available, sent, pinned))
	if err != nil {
		return "", err
	}
	return m.Name(), nil
}

// Supplying an identifier means asking about that one thing: the method needing it wins.
func TestMostSpecificWins(t *testing.T) {
	c := caps{"select": {list, get}}
	if got, err := analyze1(t, c, analyze.PermitAll(), "select", []string{"id", "region"}, nil, ""); err != nil || got != "get" {
		t.Errorf("got %q, %v; want get", got, err)
	}
	if got, err := analyze1(t, c, analyze.PermitAll(), "select", []string{"region"}, nil, ""); err != nil || got != "list" {
		t.Errorf("got %q, %v; want list", got, err)
	}
}

// A method that does not accept a field being sent would drop or reject it, so it is never chosen.
func TestEverySentInputMustBeAccepted(t *testing.T) {
	c := caps{"update": {tenancy, cidr}}
	got, err := analyze1(t, c, analyze.PermitAll(), "update", []string{"id", "tenancy"}, []string{"id", "tenancy"}, "")
	if err != nil || got != "modify_tenancy" {
		t.Errorf("got %q, %v; want modify_tenancy", got, err)
	}
	_, err = analyze1(t, c, analyze.PermitAll(), "update", []string{"id", "tenancy", "cidr"}, []string{"id", "tenancy", "cidr"}, "")
	if err == nil || !strings.Contains(err.Error(), "does not accept") {
		t.Errorf("err = %v; want a refusal naming what is not accepted", err)
	}
}

// Equal candidates are ambiguous and reported, never broken by guessing — except that among reads
// the one needing no body is the read.
func TestTiesAreReported(t *testing.T) {
	c := caps{"update": {tenancy, method{name: "other", required: []string{"id"}, accepts: []string{"id"}}}}
	_, err := analyze1(t, c, analyze.PermitAll(), "update", []string{"id"}, []string{"id"}, "")
	if err == nil || !strings.Contains(err.Error(), "several update methods") {
		t.Errorf("err = %v; want an ambiguity", err)
	}
	c = caps{"select": {check, list}}
	if got, err := analyze1(t, c, analyze.PermitAll(), "select", nil, nil, ""); err != nil || got != "list" {
		t.Errorf("got %q, %v; want list", got, err)
	}
}

// A policy narrows what the documents allow: a refused method is not chosen, and a refused pin
// fails rather than falling back to another method.
func TestPolicyNarrows(t *testing.T) {
	c := caps{"select": {list, get}}
	if got, err := analyze1(t, c, refuse("get"), "select", []string{"id", "region"}, nil, ""); err != nil || got != "list" {
		t.Errorf("got %q, %v; want list", got, err)
	}
	if _, err := analyze1(t, c, refuse("get"), "select", []string{"id"}, nil, "get"); err == nil {
		t.Error("a refused pin was honoured")
	}
}

// A pinned method decides which method, not whether it can run.
func TestPinnedIsStillChecked(t *testing.T) {
	c := caps{"select": {list, get}}
	if got, err := analyze1(t, c, analyze.PermitAll(), "select", []string{"id"}, nil, "list"); err != nil || got != "list" {
		t.Errorf("got %q, %v; want list", got, err)
	}
	if _, err := analyze1(t, c, analyze.PermitAll(), "select", nil, nil, "get"); err == nil {
		t.Error("a pin missing its required input was honoured")
	}
	if _, err := analyze1(t, c, analyze.PermitAll(), "select", nil, nil, "nope"); err == nil {
		t.Error("a pin naming no method was honoured")
	}
}

type partialMethod struct{ facade.Method }

func (partialMethod) PartialUpdate() bool { return true }

func TestUpdateSelectsDeclaredPartialUpdateOverReplacement(t *testing.T) {
	patch := partialMethod{method{name: "patch", required: []string{"id"}, body: true}}
	put := method{name: "put", required: []string{"id"}, body: true}
	c := caps{"update": {put, patch}, "replace": {put}}
	if got, err := analyze1(t, c, analyze.PermitAll(), "update", []string{"id"}, []string{"id", "labels"}, ""); err != nil || got != "patch" {
		t.Fatalf("UPDATE selected %q: %v", got, err)
	}
	if got, err := analyze1(t, c, analyze.PermitAll(), "replace", []string{"id"}, []string{"id", "labels"}, ""); err != nil || got != "put" {
		t.Fatalf("REPLACE selected %q: %v", got, err)
	}
	if got, err := analyze1(t, c, analyze.PermitAll(), "update", []string{"id"}, nil, "put"); err != nil || got != "put" {
		t.Fatalf("explicit pin selected %q: %v", got, err)
	}
}
