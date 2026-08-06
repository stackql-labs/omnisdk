package plan

import "github.com/stackql-labs/omnisdk/internal/system_g/facade"

// Plan IR, behind interfaces (impls unexported, built via constructors).

// ExchangeSpec declares one exchange: name, β inbox attrs (In), β outputs (Out), a factory, and
// an optional Flatten (how its join tuple merges into the row; nil = generic).
type ExchangeSpec interface {
	Name() string
	In() []string
	Out() []string
	Make(bound map[string]any) facade.Operator
	Flatten() facade.Transform
}

// BetaEdge is a β binding Src→Tgt from exchange From to To (From "" = a κ input).
type BetaEdge interface {
	From() string
	To() string
	Src() string
	Tgt() string
}

// AlphaEdge is a behavioural (E_α) annotation on the From→To edge.
type AlphaEdge interface {
	From() string
	To() string
	Alpha() facade.Alpha
}

// Plan is a selected slice ready to Compose.
type Plan interface {
	Exchanges() []ExchangeSpec
	Betas() []BetaEdge
	Alphas() []AlphaEdge
	Inputs() map[string]any
	Egress() []facade.Transform
	Encoder() facade.Encoder
}

type exchangeSpec struct {
	name    string
	in, out []string
	make    func(map[string]any) facade.Operator
	flatten facade.Transform
}

// NewExchangeSpec builds an exchange declaration (flatten may be nil).
func NewExchangeSpec(name string, in, out []string, make func(map[string]any) facade.Operator, flatten facade.Transform) ExchangeSpec {
	return exchangeSpec{name: name, in: in, out: out, make: make, flatten: flatten}
}

func (e exchangeSpec) Name() string                              { return e.name }
func (e exchangeSpec) In() []string                              { return e.in }
func (e exchangeSpec) Out() []string                             { return e.out }
func (e exchangeSpec) Make(bound map[string]any) facade.Operator { return e.make(bound) }
func (e exchangeSpec) Flatten() facade.Transform                 { return e.flatten }

type betaEdge struct{ from, to, src, tgt string }

// NewBetaEdge wires Src→Tgt from exchange from to to (from "" = a κ input).
func NewBetaEdge(from, to, src, tgt string) BetaEdge {
	return betaEdge{from: from, to: to, src: src, tgt: tgt}
}

func (b betaEdge) From() string { return b.from }
func (b betaEdge) To() string   { return b.to }
func (b betaEdge) Src() string  { return b.src }
func (b betaEdge) Tgt() string  { return b.tgt }

type alphaEdge struct {
	from, to string
	alpha    facade.Alpha
}

// NewAlphaEdge annotates the from→to edge.
func NewAlphaEdge(from, to string, alpha facade.Alpha) AlphaEdge {
	return alphaEdge{from: from, to: to, alpha: alpha}
}

func (a alphaEdge) From() string        { return a.from }
func (a alphaEdge) To() string          { return a.to }
func (a alphaEdge) Alpha() facade.Alpha { return a.alpha }

type planData struct {
	exchanges []ExchangeSpec
	betas     []BetaEdge
	alphas    []AlphaEdge
	inputs    map[string]any
	egress    []facade.Transform
	encoder   facade.Encoder
}

// NewPlan assembles a plan (alphas, inputs, egress, encoder may be nil).
func NewPlan(exchanges []ExchangeSpec, betas []BetaEdge, alphas []AlphaEdge, inputs map[string]any, egress []facade.Transform, encoder facade.Encoder) Plan {
	return planData{exchanges: exchanges, betas: betas, alphas: alphas, inputs: inputs, egress: egress, encoder: encoder}
}

func (p planData) Exchanges() []ExchangeSpec  { return p.exchanges }
func (p planData) Betas() []BetaEdge          { return p.betas }
func (p planData) Alphas() []AlphaEdge        { return p.alphas }
func (p planData) Inputs() map[string]any     { return p.inputs }
func (p planData) Egress() []facade.Transform { return p.egress }
func (p planData) Encoder() facade.Encoder    { return p.encoder }
