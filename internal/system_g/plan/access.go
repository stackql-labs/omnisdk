package plan

import (
	"context"
	"slices"

	"github.com/stackql-labs/omnisdk/internal/system_g/buffer"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// AccessPolicy is inherited through exchange wrappers. Nil means unclassified.
type AccessPolicy interface {
	Permissions() []facade.Permission
	Internal() bool
	TrustedTransport() bool
	Guarded() bool
	Denied(error) error
}

type accessPolicy struct {
	permissions                  []facade.Permission
	internal, transport, guarded bool
}

func (p accessPolicy) Permissions() []facade.Permission { return slices.Clone(p.permissions) }
func (p accessPolicy) Guarded() bool                    { return p.guarded }
func (p accessPolicy) Internal() bool                   { return p.internal }
func (p accessPolicy) TrustedTransport() bool           { return p.transport }
func (accessPolicy) Denied(err error) error             { return err }

// WithAccess declares a published operation's policy. Only trusted document compilers may
// authorize the underlying transport implicitly. The same gate also protects direct Make calls.
func WithAccess(spec ExchangeSpec, trustedTransport bool, permissions ...facade.Permission) ExchangeSpec {
	return accessSpec{ExchangeSpec: spec, policy: accessPolicy{permissions: slices.Clone(permissions), transport: trustedTransport, guarded: true}}
}

type accessSpec struct {
	ExchangeSpec
	policy AccessPolicy
}

func (s accessSpec) Access() AccessPolicy { return s.policy }
func (s accessSpec) Make(bound map[string]any) facade.Operator {
	return accessOperator{spec: s.ExchangeSpec, bound: bound, policy: s.policy}
}

type internalSpec struct{ ExchangeSpec }

func (internalSpec) Access() AccessPolicy { return accessPolicy{internal: true} }

// WithInternal declares trusted planner scaffolding, never published operations or I/O callbacks.
func WithInternal(spec ExchangeSpec) ExchangeSpec { return internalSpec{ExchangeSpec: spec} }

// Make is the execution boundary used by normal stages, cycles and admission groups.
func Make(spec ExchangeSpec, bound map[string]any) facade.Operator {
	// Classified compiler wrappers already gate their factories. Do not add another operator
	// to ordinary query execution; effects override Guarded because their WAL comes first.
	policy := spec.Access()
	if policy != nil && (policy.Internal() || policy.Guarded()) {
		return spec.Make(bound)
	}
	return accessOperator{spec: spec, bound: bound, policy: policy}
}

type accessOperator struct {
	spec   ExchangeSpec
	bound  map[string]any
	policy AccessPolicy
}

func (o accessOperator) Open(ctx context.Context) facade.Records {
	// Ordinary unprotected execution does not construct or clone authorization metadata.
	if !facade.AccessEnabled(ctx) && ctx.Err() == nil {
		return o.spec.Make(o.bound).Open(ctx)
	}
	policy := o.policy
	if policy == nil {
		policy = o.spec.Access()
	}
	authorized, err := Authorize(ctx, policy)
	if err != nil {
		denied := buffer.NewBuffer(1, 1, 0)
		denied.Complete(err)
		return denied.Reader()
	}
	return o.spec.Make(o.bound).Open(authorized)
}

type denialPolicy struct {
	AccessPolicy
	denied func(error) error
}

func (denialPolicy) Guarded() bool            { return false }
func (p denialPolicy) Denied(err error) error { return p.denied(err) }

// WithDenial preserves an operation's error semantics without duplicating authorization.
func WithDenial(policy AccessPolicy, denied func(error) error) AccessPolicy {
	if policy == nil {
		policy = accessPolicy{}
	}
	return denialPolicy{AccessPolicy: policy, denied: denied}
}

// Authorize applies an inherited declaration to an execution or cached-result boundary.
func Authorize(ctx context.Context, policy AccessPolicy) (context.Context, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !facade.AccessEnabled(ctx) {
		return ctx, nil
	}
	if policy != nil && policy.Internal() {
		return ctx, nil
	}
	var permissions []facade.Permission
	if policy != nil {
		permissions = policy.Permissions()
	}
	authorized, err := facade.Authorize(ctx, permissions...)
	if err != nil {
		if policy != nil {
			err = policy.Denied(err)
		}
		return nil, err
	}
	if policy != nil && policy.TrustedTransport() {
		authorized = facade.WithAuthorizedExchange(authorized)
	}
	return authorized, nil
}
