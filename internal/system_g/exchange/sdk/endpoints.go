package sdk

import (
	"sync"

	ep "github.com/stackql-labs/omnisdk/internal/system_g/endpoint"
)

// Every provider host this package talks to, declared ONCE at exchange init time. {vars} are expanded
// per request. This is the single statement of where a service lives: the …Base helpers below ask for
// it rather than restating a literal, so a host cannot be defined in two places and drift.
func init() {
	ep.Register(ep.AWSS3, "https://s3.{region}.amazonaws.com")
	ep.Register(ep.AWSEC2, "https://ec2.{region}.amazonaws.com")
	// IAM is global: one endpoint, no region in the URL (SigV4 still signs into one).
	ep.Register(ep.AWSIAM, "https://iam.amazonaws.com")
	ep.Register(ep.AzureLogin, azureLoginDefault)
	ep.Register(ep.AzureMgmt, azureMgmtDefault)
	ep.Register(ep.AzureGraph, "https://graph.microsoft.com")
	ep.Register(ep.GCPOAuth, "https://oauth2.googleapis.com/token")
	ep.Register(ep.GCPStorage, "https://storage.googleapis.com/storage/v1")
	ep.Register(ep.GCPCRM, "https://cloudresourcemanager.googleapis.com/v3")
	ep.Register(ep.GCPCompute, "https://compute.googleapis.com/compute/v1/projects")
}

// resolve is the ONE place a provider host is retargeted: registered default, {vars} expanded, then
// any override applied — whole-URL or fragment.
//
// spec is the caller's endpoint config, carried as a string because it rides on the public Args DTO
// (see pkg/omnisdk). Parsing is memoised: this runs per request under fan-out and the same spec recurs
// for a whole run.
func resolve(spec, service string, vars map[string]string) string {
	return resolver(spec).Resolve(service, vars)
}

// overridden reports whether a service is redirected — for callers whose real addressing differs from
// a mock's, e.g. S3's virtual-host buckets collapsing to path-style against one local host.
func overridden(spec, service string) bool { return resolver(spec).IsOverridden(service) }

var resolverCache sync.Map // spec string → ep.Endpoints

func resolver(spec string) ep.Endpoints {
	if v, ok := resolverCache.Load(spec); ok {
		return v.(ep.Endpoints)
	}
	// A malformed spec degrades to the real clouds rather than failing here, deep in request
	// construction. pkg/omnisdk validates it up front, where the error can name the caller.
	e, err := ep.Parse(spec)
	if err != nil {
		e = ep.Real()
	}
	resolverCache.Store(spec, e)
	return e
}
