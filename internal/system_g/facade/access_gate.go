package facade

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Permission is the resource/action contract declared by a queryable operation.
type Permission struct{ Resource, Action string }

// ErrAccessDenied is shared by all authorization boundaries, including classification failures.
var ErrAccessDenied = errors.New("access denied")

type grantedPermission struct {
	permission Permission
	previous   *grantedPermission
}
type grantedKey struct{}

// Authorize is the inherited execution gate. In protected contexts, unclassified operations
// fail closed, even under a wildcard policy. Successful exact grants flow only down this context.
// Without a configured controller, legacy unprotected execution remains unchanged.
func Authorize(ctx context.Context, permissions ...Permission) (context.Context, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !AccessEnabled(ctx) {
		return ctx, nil
	}
	if len(permissions) == 0 {
		return nil, fmt.Errorf("%w: operation has no permission metadata", ErrAccessDenied)
	}
	// Validate the entire declaration before consulting policy or executing anything.
	for _, permission := range permissions {
		if strings.TrimSpace(permission.Resource) == "" || strings.TrimSpace(permission.Action) == "" {
			return nil, fmt.Errorf("%w: incomplete permission metadata", ErrAccessDenied)
		}
	}
	grants, _ := ctx.Value(grantedKey{}).(*grantedPermission)
	inherited := grants
	for _, permission := range permissions {
		found := false
		for previous := grants; previous != nil; previous = previous.previous {
			if previous.permission == permission {
				found = true
				break
			}
		}
		if found {
			continue
		}
		if err := CheckAccess(ctx, permission.Resource, permission.Action); err != nil {
			return nil, err
		}
		grants = &grantedPermission{permission: permission, previous: grants}
	}
	if grants == inherited {
		return ctx, nil
	}
	return context.WithValue(ctx, grantedKey{}, grants), nil
}

// DocumentPermissions derives one shared classification for document reads and mutations.
func DocumentPermissions(provider, method, operation string) []Permission {
	if strings.TrimSpace(provider) == "" || strings.TrimSpace(method) == "" || strings.TrimSpace(operation) == "" {
		return nil
	}
	return []Permission{{Resource: "operation/" + provider + "/" + method + "/" + operation, Action: "execute"}, {Resource: "credential/" + provider, Action: "use"}}
}
