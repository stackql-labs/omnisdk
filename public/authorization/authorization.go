// Package authorization evaluates application-owned policy using Casbin.
package authorization

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/casbin/casbin/v2"
	"github.com/casbin/casbin/v2/model"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/public/principal"
)

type Authorizer = facade.AccessAuthorizer

var ErrDenied = errors.New("access denied")

// Rule grants one exact subject/group a resource glob and exact action (or *).
// Subject is user:<id> or group:<id>. Membership always comes from the directory.
type Rule struct {
	Subject  string `json:"subject"`
	Resource string `json:"resource"`
	Action   string `json:"action"`
}
type authorizer struct {
	mu     sync.Mutex
	engine *casbin.Enforcer
}

const policyModel = `[request_definition]
r = sub, obj, act
[policy_definition]
p = sub, obj, act
[policy_effect]
e = some(where (p.eft == allow))
[matchers]
m = r.sub == p.sub && globMatch(r.obj, p.obj) && (r.act == p.act || p.act == "*")`

func NewCasbin(rules []Rule) (Authorizer, error) {
	m, err := model.NewModelFromString(policyModel)
	if err != nil {
		return nil, err
	}
	e, err := casbin.NewEnforcer(m)
	if err != nil {
		return nil, err
	}
	for _, r := range rules {
		if r.Subject == "" || r.Resource == "" || r.Action == "" {
			return nil, errors.New("policy fields are required")
		}
		if _, err := e.AddPolicy(r.Subject, r.Resource, r.Action); err != nil {
			return nil, err
		}
	}
	return &authorizer{engine: e}, nil
}
func (a *authorizer) Authorize(ctx context.Context, p principal.Principal, resource, action string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil || p.ID() == "" || resource == "" || action == "" {
		return ErrDenied
	}
	subjects := []string{"user:" + p.ID()}
	for _, g := range p.Groups() {
		subjects = append(subjects, "group:"+g)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range subjects {
		ok, err := a.engine.Enforce(s, resource, action)
		if err != nil {
			return fmt.Errorf("authorization decision: %w", err)
		}
		if ok {
			return nil
		}
	}
	return fmt.Errorf("%w: %s on %s", ErrDenied, action, resource)
}

// Session is an authenticated, resolved, immutable caller snapshot. Directory and policy refresh
// occurs when a new session is established, not halfway through concurrent execution.
type Session = facade.AccessSession

type session struct {
	p principal.Principal
	a Authorizer
}

func NewSession(ctx context.Context, i principal.Identity, d principal.Directory, a Authorizer) (Session, error) {
	if i == nil || d == nil || a == nil {
		return nil, errors.New("identity, directory and authorizer are required")
	}
	p, err := d.Resolve(ctx, i)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, errors.New("directory returned no principal")
	}
	snapshot, err := principal.NewPrincipal(p.ID(), p.Groups(), p.Attributes())
	if err != nil {
		return nil, err
	}
	return session{snapshot, a}, nil
}
func (s session) Principal() principal.Principal { return s.p }
func (s session) Context(ctx context.Context) context.Context {
	return facade.WithAccessController(ctx, s)
}
func (s session) Authorize(ctx context.Context, resource, action string) error {
	return s.a.Authorize(ctx, s.p, resource, action)
}

// Check applies the session policy. Applications must establish a session before enabling access.
func Check(ctx context.Context, resource, action string) error {
	if !facade.AccessEnabled(ctx) {
		return ErrDenied
	}
	return facade.CheckAccess(ctx, resource, action)
}
