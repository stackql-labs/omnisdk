// Package namespace resolves the names an exchange addresses.
//
// A request draws its values from several places at once — a path segment, a query parameter, a
// header, a field of the request body — and answers with a projected row. Those are separate spaces
// that share one flat vocabulary: a caller writing a binding, an override or a template says
// "VpcId", not "the query parameter VpcId".
//
// The vocabulary has to be known AHEAD OF TIME. A name that two spaces both claim is ambiguous
// whatever happens at run time, and resolving it by whichever space is consulted first is how a
// value reaches somewhere nobody meant. So the namespace is built when an exchange is resolved, and
// an ambiguity is an error there rather than a surprise later.
//
// Nothing here knows about documents. The inventory is a flat list of attributes, so a provider
// document, a hand-authored plan and a caller's own wiring all fill it the same way.
package namespace

import (
	"fmt"
	"sort"
	"strings"
)

// Space is where an attribute lives. Spaces are open: a transport with somewhere else to put a value
// names a new one rather than forcing it into an existing space.
type Space string

const (
	// Projection is the shape of a row the exchange emits — the attributes at the document's row
	// path. A bare identifier means one of these by default, because these calls read as SELECTs and
	// in a SELECT a bare identifier is a column.
	Projection Space = "projection"
	// Body is a field of the request body.
	Body Space = "body"
	// Query is a query-string parameter.
	Query Space = "query"
	// Path is a segment of the request path.
	Path Space = "path"
	// Header is a request header.
	Header Space = "header"
	// Inbox is what arrives from another exchange over a β edge, before any T_in.
	Inbox Space = "inbox"
)

// Attribute is one addressable name.
type Attribute interface {
	Space() Space
	Name() string
	// Qualified is "<space>.<name>", which always resolves whatever else claims the bare name.
	Qualified() string
}

// New builds an attribute.
func New(space Space, name string) Attribute { return attribute{space: space, name: name} }

type attribute struct {
	space Space
	name  string
}

func (a attribute) Space() Space      { return a.space }
func (a attribute) Name() string      { return a.name }
func (a attribute) Qualified() string { return string(a.space) + "." + a.name }

// Order is a partial ranking of spaces: Outranks reports whether a name claimed by both is resolved
// in favour of the first.
//
// Partial is the point. Body, query and path have no claim over one another, so a name two of them
// declare is ambiguous and stays that way; only a space with a stated reason to win does.
type Order interface {
	Outranks(winner, loser Space) bool
}

// DefaultOrder resolves a bare name to the projection over any request space, and leaves the request
// spaces incomparable among themselves.
//
// The reading is what justifies it: these exchanges answer questions, so an unqualified name means
// something about the answer. Nothing similar distinguishes a query parameter from a body field, and
// inventing a ranking there would resolve an ambiguity the caller should settle.
func DefaultOrder() Order { return defaultOrder{} }

type defaultOrder struct{}

func (defaultOrder) Outranks(winner, loser Space) bool {
	return winner == Projection && loser != Projection
}

// RankedOrder ranks spaces in the given sequence, highest first. A space absent from the sequence is
// incomparable with every other.
func RankedOrder(spaces ...Space) Order {
	rank := make(map[Space]int, len(spaces))
	for i, s := range spaces {
		rank[s] = i
	}
	return ranked{rank: rank}
}

type ranked struct{ rank map[Space]int }

func (r ranked) Outranks(winner, loser Space) bool {
	w, wok := r.rank[winner]
	l, lok := r.rank[loser]
	return wok && lok && w < l
}

// Namespace is an exchange's resolved vocabulary.
type Namespace interface {
	// Resolve reads a bare or qualified name. A bare name resolves when one space claims it, or when
	// one claimant outranks every other.
	Resolve(name string) (Attribute, error)
	// In lists a space's attributes, sorted.
	In(space Space) []Attribute
	// Spaces are the spaces holding at least one attribute, sorted.
	Spaces() []Space
}

// Option configures the namespace.
type Option func(*options)

type options struct{ order Order }

// WithPrecedence replaces the default ranking. Two callers may legitimately read one document
// differently; what must not happen is a name resolving differently at different moments of the same
// run, which is why the ranking is fixed when the namespace is built.
func WithPrecedence(order Order) Option {
	return func(o *options) { o.order = order }
}

// Build assembles a namespace and reports every ambiguity it contains.
//
// Ambiguity is reported HERE — when the exchange is resolved — and not at Resolve: a name nobody
// happens to look up is still ambiguous, and finding out only when a query touches it makes the
// fault depend on the query rather than on the document.
func Build(attrs []Attribute, opts ...Option) (Namespace, error) {
	o := options{order: DefaultOrder()}
	for _, opt := range opts {
		opt(&o)
	}

	byName := map[string][]Attribute{}
	for _, a := range attrs {
		byName[a.Name()] = append(byName[a.Name()], a)
	}

	resolved := make(map[string]Attribute, len(byName))
	var ambiguous []string
	for name, claimants := range byName {
		winner, ok := rank(claimants, o.order)
		if !ok {
			ambiguous = append(ambiguous, describe(name, claimants))
			continue
		}
		resolved[name] = winner
	}
	if len(ambiguous) > 0 {
		sort.Strings(ambiguous)
		return nil, fmt.Errorf("namespace: ambiguous %s (qualify to disambiguate)", strings.Join(ambiguous, "; "))
	}
	return &namespace{attrs: attrs, bare: resolved}, nil
}

// rank picks the claimant that outranks every other, if there is one. Two attributes in the SAME
// space with one name are always ambiguous: nothing can separate them.
func rank(claimants []Attribute, order Order) (Attribute, bool) {
	if len(claimants) == 1 {
		return claimants[0], true
	}
	// Compared by POSITION, not by value: two attributes of the same space and name are equal as
	// values, so skipping "itself" would skip the duplicate too and let it win unopposed.
	for i, candidate := range claimants {
		wins := true
		for j, other := range claimants {
			if i == j {
				continue
			}
			if !order.Outranks(candidate.Space(), other.Space()) {
				wins = false
				break
			}
		}
		if wins {
			return candidate, true
		}
	}
	return nil, false
}

func describe(name string, claimants []Attribute) string {
	spaces := make([]string, 0, len(claimants))
	for _, c := range claimants {
		spaces = append(spaces, string(c.Space()))
	}
	sort.Strings(spaces)
	if len(spaces) == 2 && spaces[0] == spaces[1] {
		return fmt.Sprintf("%q declared twice in space %q", name, spaces[0])
	}
	return fmt.Sprintf("%q claimed by %s", name, strings.Join(spaces, " and "))
}

type namespace struct {
	attrs []Attribute
	bare  map[string]Attribute
}

func (n *namespace) Resolve(name string) (Attribute, error) {
	if space, bare, qualified := strings.Cut(name, "."); qualified {
		for _, a := range n.attrs {
			if a.Space() == Space(space) && a.Name() == bare {
				return a, nil
			}
		}
		return nil, fmt.Errorf("namespace: no %q in space %q", bare, space)
	}
	a, ok := n.bare[name]
	if !ok {
		return nil, fmt.Errorf("namespace: no attribute %q", name)
	}
	return a, nil
}

func (n *namespace) In(space Space) []Attribute {
	var out []Attribute
	for _, a := range n.attrs {
		if a.Space() == space {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

func (n *namespace) Spaces() []Space {
	seen := map[Space]bool{}
	var out []Space
	for _, a := range n.attrs {
		if !seen[a.Space()] {
			seen[a.Space()], out = true, append(out, a.Space())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
