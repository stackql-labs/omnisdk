// Command omnicli exercises System-G query graphs against cloud APIs (real or a mock endpoint),
// emitting JSONL. It is a THIN consumer of the public facade (pkg/omnisdk) and imports nothing from
// internal/ — every command plans a method (or a merge of methods) via omnisdk and streams the rows.
// This is the shape a consumer such as stackql sees: discover resources/methods, then run one.
//
//	./build/omnicli list --aws-region us-east-1          # aws.s3.buckets.enumerate → JSONL
//	./build/omnicli encryption --aws-region us-east-1    # aws.s3.buckets.encryption (β bowtie) → JSONL
//	./build/omnicli blob-audit-shallow --aws-region us-east-1 --project P   # AWS+Azure+GCP merged
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

func main() {
	var outPath, logPath, endpoint string
	var insecureTLS bool
	var awsRegion string
	var t tune

	root := &cobra.Command{
		Use:           "omnicli",
		Short:         "Run System-G query graphs via the omnisdk facade (JSONL output)",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	pf := root.PersistentFlags()
	pf.StringVarP(&outPath, "out", "o", "", "output file (default stdout)")
	pf.StringVar(&logPath, "log", "", "log raw responses to this file (default off)")
	pf.StringVar(&endpoint, "endpoint", "", "endpoint override, path-style (e.g. http://localhost:8085); default real cloud")
	pf.BoolVar(&insecureTLS, "tls-skip-verify", false, "accept a self-signed certificate; only applies with --endpoint")
	pf.IntVar(&t.parallelism, "parallelism", 16, "max concurrent fan-out units (bind-join inners)")
	pf.IntVar(&t.perHost, "max-per-host", 8, "max concurrent requests per backend host")
	pf.IntVar(&t.retryTries, "retry-tries", 4, "total attempts per request incl. the first (ephemeral failures)")
	pf.Float64Var(&t.retryRate, "retry-rate", 20, "max aggregate retries per second across the run")
	pf.IntVar(&t.limit, "limit", 0, "stop cleanly after N output records (0 = unlimited)")
	pf.StringVar(&awsRegion, "aws-region", "", "AWS region (required for AWS commands; scope, not read from env)")

	// streamRows opens a planned query and encodes each row as JSONL. Shared by the single-method and
	// merged paths — the facade owns the run policies, ret/abort/limit, and the concurrency-safe log.
	streamRows := func(pl omnisdk.Plan, w io.Writer) error {
		rows, err := pl.Open(context.Background())
		if err != nil {
			return err
		}
		defer rows.Close()
		enc := json.NewEncoder(w)
		for rows.Next() {
			if err := enc.Encode(rows.Row()); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	// withSinks opens the out + log writers, runs fn, and closes them (surfacing close errors only if
	// fn itself succeeded). Both are plain writers — the facade wraps the log in its own async sink.
	withSinks := func(fn func(cmd *cobra.Command, w, logw io.Writer) error) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, _ []string) (err error) {
			w, closeOut, e := openOut(outPath)
			if e != nil {
				return e
			}
			defer func() {
				if ce := closeOut(); ce != nil && err == nil {
					err = ce
				}
			}()
			logw, closeLog, e := openLog(logPath)
			if e != nil {
				return e
			}
			defer func() {
				if ce := closeLog(); ce != nil && err == nil {
					err = ce
				}
			}()
			return fn(cmd, w, logw)
		}
	}
	// withArgs is the shared shape: mkArgs assembles the method's scope/auth Args from flags; the run
	// knobs, endpoint, and log are filled in here so every command applies them identically.
	withArgs := func(mkArgs func(*cobra.Command) (omnisdk.Args, error), plan func(omnisdk.Args) (omnisdk.Plan, error)) func(*cobra.Command, []string) error {
		return withSinks(func(cmd *cobra.Command, w, logw io.Writer) error {
			a, e := mkArgs(cmd)
			if e != nil {
				return e
			}
			a.Endpoint, a.Log, a.Tuning = endpoint, logw, t.facade()
			a.InsecureSkipTLSVerify = insecureTLS
			pl, e := plan(a)
			if e != nil {
				return e
			}
			return streamRows(pl, w)
		})
	}
	// runFacade plans ONE method; runMerged fans several into one cursor (the multi-cloud audit).
	runFacade := func(path string, mkArgs func(*cobra.Command) (omnisdk.Args, error)) func(*cobra.Command, []string) error {
		return withArgs(mkArgs, func(a omnisdk.Args) (omnisdk.Plan, error) { return omnisdk.New(path, a) })
	}
	runMerged := func(paths []string, mkArgs func(*cobra.Command) (omnisdk.Args, error)) func(*cobra.Command, []string) error {
		return withArgs(mkArgs, func(a omnisdk.Args) (omnisdk.Plan, error) { return omnisdk.NewMerged(paths, a) })
	}
	regionArgs := func(_ *cobra.Command) (omnisdk.Args, error) {
		return omnisdk.Args{Params: map[string]string{"region": awsRegion}}, nil
	}

	// ---- AWS ------------------------------------------------------------------
	root.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "Enumerate S3 buckets → JSONL {name, created, arn} (aws.s3.buckets.enumerate)",
		RunE:  runFacade("aws.s3.buckets.enumerate", regionArgs),
	})
	root.AddCommand(&cobra.Command{
		Use:   "encryption",
		Short: "ListBuckets ⋈ GetBucketEncryption (β bowtie) → JSONL (aws.s3.buckets.encryption)",
		RunE:  runFacade("aws.s3.buckets.encryption", regionArgs),
	})
	provCmd := &cobra.Command{
		Use:   "provision",
		Short: "Create a VPC then a subnet in it (β flow); CREATES REAL AWS RESOURCES",
		RunE: runFacade("aws.ec2.networks.provision", func(cmd *cobra.Command) (omnisdk.Args, error) {
			return omnisdk.Args{Params: map[string]string{
				"region":      awsRegion,
				"vpc_cidr":    mustFlag(cmd, "vpc-cidr"),
				"subnet_cidr": mustFlag(cmd, "subnet-cidr"),
			}}, nil
		}),
	}
	provCmd.Flags().String("vpc-cidr", "", "VPC CIDR block, e.g. 10.0.0.0/16 (required)")
	provCmd.Flags().String("subnet-cidr", "", "subnet CIDR block, e.g. 10.0.1.0/24 (required)")
	root.AddCommand(provCmd)

	// IaC: a client names a blueprint handle and supplies its inputs. Idempotence, ordering, locking
	// and compensation are the system's problem, not the caller's.
	root.AddCommand(&cobra.Command{
		Use:   "iac-handles",
		Short: "List the precanned deployments addressable by handle",
		RunE: func(*cobra.Command, []string) error {
			type published struct {
				Handle  string          `json:"handle"`
				Summary string          `json:"summary"`
				Params  []omnisdk.Param `json:"params"`
			}
			var out []published
			for _, b := range omnisdk.Blueprints() {
				out = append(out, published{Handle: b.Handle(), Summary: b.Summary(), Params: b.Params()})
			}
			return printJSON(out)
		},
	})

	// Declared IaC: a resource states its provider, its document address and the residue its document
	// leaves unsaid. No Go entry is added for a new service — the same relationship doc-graph has to
	// a query.
	iacApply := &cobra.Command{
		Use:   "iac-apply <registry> <spec-json>",
		Short: "Converge resources declared inline against provider documents; CREATES REAL RESOURCES",
		Args:  cobra.ExactArgs(2),
		RunE: withSinks(func(cmd *cobra.Command, w, logw io.Writer) error {
			var spec struct {
				Name      string `json:"name"`
				State     string `json:"state"`
				RunID     string `json:"run_id,omitempty"`
				Resources []struct {
					Key      string            `json:"key"`
					Provider string            `json:"provider"`
					Address  string            `json:"address"`
					Desired  json.RawMessage   `json:"desired,omitempty"`
					Params   map[string]string `json:"params,omitempty"`
					Inbound  []struct {
						From string `json:"from"`
						As   string `json:"as,omitempty"`
					} `json:"inbound,omitempty"`
					ViaType          string `json:"via_type,omitempty"`
					Via              string `json:"via,omitempty"`
					Identity         string `json:"identity,omitempty"`
					AddressedBy      string `json:"addressed_by,omitempty"`
					CorrelationParam string `json:"correlation_param,omitempty"`
				} `json:"resources"`
				Args *omnisdk.Args `json:"args,omitempty"`
			}
			if err := json.Unmarshal([]byte(cmdArgs(cmd)[1]), &spec); err != nil {
				return fmt.Errorf("spec json: %w", err)
			}
			resources := make([]omnisdk.ManagedResource, 0, len(spec.Resources))
			for _, r := range spec.Resources {
				inbound := make([]omnisdk.Arrival, 0, len(r.Inbound))
				for _, in := range r.Inbound {
					inbound = append(inbound, omnisdk.Arrival{From: in.From, As: in.As})
				}
				resources = append(resources, omnisdk.NewResource(r.Key, r.Provider, r.Address,
					[]byte(r.Desired), r.Params, inbound, r.ViaType, r.Via,
					r.Identity, r.AddressedBy, r.CorrelationParam))
			}
			a := omnisdk.Args{}
			if spec.Args != nil {
				a = *spec.Args
			}
			if a.Params == nil {
				a.Params = map[string]string{}
			}
			if _, given := a.Params["region"]; !given && awsRegion != "" {
				a.Params["region"] = awsRegion
			}
			a.Endpoint, a.Log, a.Tuning = endpoint, logw, t.facade()
			a.InsecureSkipTLSVerify = insecureTLS
			pl, err := omnisdk.Converge(cmdArgs(cmd)[0], spec.Name, spec.State, spec.RunID, resources, a)
			if err != nil {
				return err
			}
			return streamRows(pl, w)
		}),
	}
	root.AddCommand(iacApply)

	iacCmd := &cobra.Command{
		Use:   "iac",
		Short: "Converge a deployment by handle, with a durable ledger; CREATES REAL RESOURCES",
		RunE: withSinks(func(cmd *cobra.Command, w, logw io.Writer) error {
			handle := mustFlag(cmd, "handle")
			bp, ok := omnisdk.BlueprintFor(handle)
			if !ok {
				return fmt.Errorf("unknown handle %q; see `omnicli iac-handles`", handle)
			}
			inputs, err := jsonInputs(mustFlag(cmd, "input"))
			if err != nil {
				return fmt.Errorf("--input: %w", err)
			}
			// Region is a global flag rather than an input, so it reads the same way as every other
			// AWS command; an explicit input still wins.
			if _, given := inputs["region"]; !given && awsRegion != "" {
				inputs["region"] = awsRegion
			}
			resources, err := bp.Resources(inputs)
			if err != nil {
				return err
			}
			a := omnisdk.Args{Params: map[string]string{"region": inputs["region"]}}
			a.Endpoint, a.Log, a.Tuning = endpoint, logw, t.facade()
			a.InsecureSkipTLSVerify = insecureTLS
			pl, err := omnisdk.Converge(mustFlag(cmd, "registry"), mustFlag(cmd, "name"),
				mustFlag(cmd, "state"), mustFlag(cmd, "run-id"), resources, a)
			if err != nil {
				return err
			}
			return streamRows(pl, w)
		}),
	}
	iacCmd.Flags().String("registry", "", "provider-document registry root; every effect is compiled from the document that declares it (required)")
	iacCmd.Flags().String("handle", "", "blueprint to converge, e.g. aws-vpc-subnet (required)")
	iacCmd.Flags().String("name", "", "collection name; the ledger key prefix and the correlation tag (required)")
	iacCmd.Flags().String("state", "", "directory holding the ledger and run journals; local disk only (required)")
	iacCmd.Flags().String("input", "", `blueprint inputs as a JSON object, e.g. {"vpc_cidr":"10.0.0.0/16"} (required)`)
	iacCmd.Flags().String("run-id", "", "journal name for this run (default: a UTC timestamp)")
	// Scope is explicit input, never inferred: which deployment, under which name, recorded in which
	// ledger are the three things a wrong guess would silently apply to the wrong resources.
	for _, f := range []string{"registry", "handle", "name", "state", "input"} {
		_ = iacCmd.MarkFlagRequired(f)
	}
	root.AddCommand(iacCmd)

	// ---- GCP ------------------------------------------------------------------
	gcpCmd := &cobra.Command{
		Use:   "gcp-provision",
		Short: "GCP: OAuth → CreateNetwork → poll → CreateSubnet → poll (β token + async); CREATES REAL GCP RESOURCES",
		RunE: runFacade("gcp.compute.networks.provision", func(cmd *cobra.Command) (omnisdk.Args, error) {
			return omnisdk.Args{Params: map[string]string{
				"project": mustFlag(cmd, "project"),
				"region":  mustFlag(cmd, "region"),
			}}, nil
		}),
	}
	gcpCmd.Flags().String("project", "", "GCP project to provision into (required)")
	gcpCmd.Flags().String("region", "us-central1", "GCP region for the subnetwork")
	root.AddCommand(gcpCmd)

	// ---- Azure ----------------------------------------------------------------
	root.AddCommand(&cobra.Command{
		Use:   "azure-vnets",
		Short: "Azure: every subnet under every VNet under every subscription (env creds: AZURE_*)",
		RunE:  runFacade("azure.network.subnets.list", func(_ *cobra.Command) (omnisdk.Args, error) { return omnisdk.Args{}, nil }),
	})

	// ---- blob-audit-shallow: per-object blob-store encryption status ----------
	awsBlob := &cobra.Command{
		Use:   "blob-audit-shallow-aws",
		Short: "AWS S3 bucket audit (aws.s3.buckets.list)",
		RunE:  runFacade("aws.s3.buckets.list", regionArgs),
	}
	root.AddCommand(awsBlob)
	root.AddCommand(&cobra.Command{
		Use:   "blob-audit-shallow-azure",
		Short: "Azure blob-container audit (azure.storage.containers.list, env creds)",
		RunE:  runFacade("azure.storage.containers.list", func(_ *cobra.Command) (omnisdk.Args, error) { return omnisdk.Args{}, nil }),
	})
	azAuth := &cobra.Command{
		Use:   "blob-audit-shallow-azure-auth",
		Short: "Azure blob-container audit with config-driven auth (--auth JSON: client_credentials | bearer)",
		RunE: runFacade("azure.storage.containers.list", func(cmd *cobra.Command) (omnisdk.Args, error) {
			a, err := loadFacadeAuth(mustFlag(cmd, "auth"))
			return omnisdk.Args{Auth: &a}, err
		}),
	}
	azAuth.Flags().String("auth", "", "auth config as JSON, or @file (required)")
	_ = azAuth.MarkFlagRequired("auth")
	root.AddCommand(azAuth)
	gcpBlob := &cobra.Command{
		Use:   "blob-audit-shallow-gcp",
		Short: "GCP Cloud Storage bucket audit in a project (google.storage.buckets.list)",
		RunE: runFacade("google.storage.buckets.list", func(cmd *cobra.Command) (omnisdk.Args, error) {
			return omnisdk.Args{Params: map[string]string{"google_project": mustFlag(cmd, "project")}}, nil
		}),
	}
	requireProject(gcpBlob)
	root.AddCommand(gcpBlob)
	gcpBlobOrg := &cobra.Command{
		Use:   "blob-audit-shallow-gcp-org",
		Short: "GCP bucket audit across an ENTIRE org (google.storage.buckets.list, org scope)",
		RunE: runFacade("google.storage.buckets.list", func(cmd *cobra.Command) (omnisdk.Args, error) {
			return omnisdk.Args{Params: map[string]string{"google_org": mustFlag(cmd, "gcp-org")}}, nil
		}),
	}
	requireGcpOrg(gcpBlobOrg)
	root.AddCommand(gcpBlobOrg)

	// Combined = NewMerged: three disjoint DAGs fanned into ONE output cursor (System-G owns even
	// disconnected graphs under a single output node; the consumer opens one Plan).
	blobMethods := []string{"aws.s3.buckets.list", "azure.storage.containers.list", "google.storage.buckets.list"}
	allBlob := &cobra.Command{
		Use:   "blob-audit-shallow",
		Short: "Blob-store encryption status across AWS+Azure+GCP, merged into one cursor",
		RunE: runMerged(blobMethods, func(cmd *cobra.Command) (omnisdk.Args, error) {
			return omnisdk.Args{Params: map[string]string{"region": awsRegion, "google_project": mustFlag(cmd, "project")}}, nil
		}),
	}
	requireProject(allBlob)
	root.AddCommand(allBlob)
	allBlobOrg := &cobra.Command{
		Use:   "blob-audit-shallow-org",
		Short: "Org-wide blob audit across AWS+Azure+GCP (Azure=all subs, GCP=whole org via --gcp-org)",
		RunE: runMerged(blobMethods, func(cmd *cobra.Command) (omnisdk.Args, error) {
			return omnisdk.Args{Params: map[string]string{"region": awsRegion, "google_org": mustFlag(cmd, "gcp-org")}}, nil
		}),
	}
	requireGcpOrg(allBlobOrg)
	root.AddCommand(allBlobOrg)

	// ---- document-driven: no catalog entry, the provider doc IS the metadata ---
	docCmd := &cobra.Command{
		Use:   "doc-select <doc.yaml> <resource>",
		Short: "Run a resource's SELECT straight from a stackql provider document (e.g. doc-select ec2.yaml instances)",
		Args:  cobra.ExactArgs(2),
		RunE: withSinks(func(cmd *cobra.Command, w, logw io.Writer) error {
			pos := cmd.Flags().Args()
			doc, err := os.ReadFile(pos[0])
			if err != nil {
				return err
			}
			a := omnisdk.Args{
				Params:   map[string]string{"region": awsRegion},
				Endpoint: endpoint,
				Log:      logw,
				Tuning:   t.facade(),
			}
			pl, err := omnisdk.NewFromDoc(doc, pos[1], a)
			if err != nil {
				return err
			}
			return streamRows(pl, w)
		}),
	}
	root.AddCommand(docCmd)

	// doc-catalog: what a provider BUNDLE makes addressable.
	// doc-catalog drills DOWN by how many arguments you give it: bundle → services, +service →
	// resources, +resource → methods. Documents are parsed on demand and not retained by a listing,
	// so walking a 232-service bundle costs one document at a time.
	catCmd := &cobra.Command{
		Use:   "doc-catalog <dir> [provider] [service] [resource]",
		Short: "Browse a provider bundle OR a registry root: providers → services → resources → methods",
		Args:  cobra.RangeArgs(1, 4),
		RunE: func(cmd *cobra.Command, args []string) error {
			quiet, _ := cmd.Flags().GetBool("quiet")
			switch len(args) {
			case 4:
				ms, err := omnisdk.DocMethods(args[0], args[1], args[2], args[3])
				if err != nil {
					return err
				}
				if quiet {
					for _, m := range ms {
						fmt.Println(m.Name)
					}
					return nil
				}
				return printJSON(ms)
			case 3:
				rs, err := omnisdk.DocResources(args[0], args[1], args[2])
				if err != nil {
					return err
				}
				if quiet {
					for _, r := range rs {
						fmt.Println(r)
					}
					return nil
				}
				return printJSON(rs)
			case 2:
				services, addresses, err := omnisdk.DocCatalog(args[0], args[1])
				if err != nil {
					return err
				}
				if quiet {
					for _, a := range addresses {
						fmt.Println(a)
					}
					return nil
				}
				return printJSON(map[string]any{
					"services": services, "service_count": len(services),
					"addresses": addresses, "address_count": len(addresses),
				})
			default:
				// A registry root has no single catalogue to list, so listing it lists the providers.
				if provs, err := omnisdk.DocProviders(args[0]); err == nil {
					if quiet {
						for p := range provs {
							fmt.Println(p)
						}
						return nil
					}
					return printJSON(provs)
				}
				services, addresses, err := omnisdk.DocCatalog(args[0])
				if err != nil {
					return err
				}
				if quiet {
					for _, a := range addresses {
						fmt.Println(a)
					}
					return nil
				}
				return printJSON(map[string]any{
					"services": services, "service_count": len(services),
					"addresses": addresses, "address_count": len(addresses),
				})
			}
		},
	}
	catCmd.Flags().BoolP("quiet", "q", false, "print bare addresses, one per line")
	root.AddCommand(catCmd)

	// doc-run: run one address out of a bundle.
	// A document describes one provider and cannot state a relationship spanning two, or one its
	// author simply left out. doc-graph lets the query say what the documents do not.
	docGraph := &cobra.Command{
		Use:   "doc-graph <dir> <graph-json>",
		Short: "Run several document exchanges joined by β edges the caller declares",
		Args:  cobra.ExactArgs(2),
		RunE: withSinks(func(cmd *cobra.Command, w, logw io.Writer) error {
			var spec struct {
				Addresses []string `json:"addresses"`
				Wirings   []struct {
					To      string `json:"to"`
					Inbound []struct {
						From string `json:"from"`
						Src  string `json:"src"`
						As   string `json:"as,omitempty"`
					} `json:"inbound"`
					ViaType  string   `json:"via_type,omitempty"`
					Via      string   `json:"via,omitempty"`
					Provides []string `json:"provides,omitempty"`
				} `json:"wirings"`
				Overrides []struct {
					Address     string `json:"address"`
					ObjectKey   string `json:"object_key,omitempty"`
					MediaType   string `json:"media_type,omitempty"`
					ProgramType string `json:"program_type,omitempty"`
					Program     string `json:"program,omitempty"`
				} `json:"overrides,omitempty"`
				Projections []struct {
					Address string       `json:"address"`
					Select  []selectJSON `json:"select"`
				} `json:"projections,omitempty"`
				Args *omnisdk.Args `json:"args,omitempty"`
			}
			if err := json.Unmarshal([]byte(cmdArgs(cmd)[1]), &spec); err != nil {
				return fmt.Errorf("graph json: %w", err)
			}
			wirings := make([]omnisdk.Wiring, 0, len(spec.Wirings))
			for _, wr := range spec.Wirings {
				in := make([]omnisdk.Inbound, 0, len(wr.Inbound))
				for _, i := range wr.Inbound {
					in = append(in, omnisdk.NewInbound(i.From, i.Src, i.As))
				}
				wirings = append(wirings, omnisdk.NewWiring(wr.To, in, wr.ViaType, wr.Via, wr.Provides...))
			}
			overrides := make([]omnisdk.Override, 0, len(spec.Overrides))
			for _, o := range spec.Overrides {
				overrides = append(overrides, omnisdk.NewOverride(o.Address, o.ObjectKey, o.MediaType, o.ProgramType, o.Program))
			}
			projections := make([]omnisdk.Projection, 0, len(spec.Projections))
			for _, p := range spec.Projections {
				cols := make([]omnisdk.SelectColumn, 0, len(p.Select))
				for _, c := range p.Select {
					e, err := parseExpr(c.exprJSON)
					if err != nil {
						return fmt.Errorf("projection on %s, column %q: %w", p.Address, c.Out, err)
					}
					cols = append(cols, omnisdk.NewSelectColumn(c.Out, e))
				}
				pr, err := omnisdk.NewProjection(p.Address, cols)
				if err != nil {
					return err
				}
				projections = append(projections, pr)
			}
			g, err := omnisdk.NewGraphWithProjections(spec.Addresses, wirings, projections, overrides...)
			if err != nil {
				return err
			}
			a := omnisdk.Args{}
			if spec.Args != nil {
				a = *spec.Args
			}
			if a.Params == nil {
				a.Params = map[string]string{}
			}
			if _, given := a.Params["region"]; !given && awsRegion != "" {
				a.Params["region"] = awsRegion
			}
			a.Endpoint, a.Log, a.Tuning = endpoint, logw, t.facade()
			a.InsecureSkipTLSVerify = insecureTLS
			pl, err := omnisdk.NewGraphQuery(cmdArgs(cmd)[0], g, a)
			if err != nil {
				return err
			}
			return streamRows(pl, w)
		}),
	}
	root.AddCommand(docGraph)

	root.AddCommand(&cobra.Command{
		Use:   "doc-run <dir> <address> [args-json]",
		Short: `Run an addressed exchange, e.g. doc-run ~/.stackql/src stackql_unstable_google.storage.buckets '{"params":{"project":"p"}}'`,
		Args:  cobra.RangeArgs(2, 3),
		RunE: withSinks(func(cmd *cobra.Command, w, logw io.Writer) error {
			pos := cmd.Flags().Args()
			var a omnisdk.Args
			if len(pos) == 3 {
				if err := json.Unmarshal([]byte(pos[2]), &a); err != nil {
					return fmt.Errorf("args: parse: %w", err)
				}
			}
			if a.Params == nil {
				a.Params = map[string]string{}
			}
			if _, ok := a.Params["region"]; !ok && awsRegion != "" {
				a.Params["region"] = awsRegion
			}
			a.Log = logw
			if a.Endpoint == "" {
				a.Endpoint = endpoint
			}
			if !a.InsecureSkipTLSVerify {
				a.InsecureSkipTLSVerify = insecureTLS
			}
			if (a.Tuning == omnisdk.Tuning{}) {
				a.Tuning = t.facade()
			}
			pl, err := omnisdk.NewFromCatalog(pos[0], pos[1], a)
			if err != nil {
				return err
			}
			return streamRows(pl, w)
		}),
	})

	// ---- discovery (straight off the facade catalog) --------------------------
	resCmd := &cobra.Command{
		Use:   "resources [resource-path]",
		Short: "List resources (--filter regex) or show one resource's metadata (dot-path, e.g. google.storage.buckets)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				r, ok := omnisdk.GetResource(args[0])
				if !ok {
					return fmt.Errorf("no resource %q", args[0])
				}
				return printJSON(r)
			}
			rs, err := omnisdk.Resources(mustFlag(cmd, "filter"))
			if err != nil {
				return err
			}
			if quiet, _ := cmd.Flags().GetBool("quiet"); quiet {
				for _, r := range rs {
					fmt.Println(r.Path)
				}
				return nil
			}
			return printJSON(rs)
		},
	}
	resCmd.Flags().String("filter", "", "regex to filter resource paths")
	resCmd.Flags().BoolP("quiet", "q", false, "print bare dot-paths, one per line (pipeable)")
	root.AddCommand(resCmd)
	methodsCmd := &cobra.Command{
		Use:   "methods <resource-path>",
		Short: "List a resource's methods and signatures (e.g. methods google.storage.buckets)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ms, err := omnisdk.Methods(args[0])
			if err != nil {
				return err
			}
			if quiet, _ := cmd.Flags().GetBool("quiet"); quiet {
				for _, m := range ms {
					fmt.Println(m.Path)
				}
				return nil
			}
			return printJSON(ms)
		},
	}
	methodsCmd.Flags().BoolP("quiet", "q", false, "print bare dot-paths, one per line (pipeable)")
	root.AddCommand(methodsCmd)
	root.AddCommand(&cobra.Command{
		Use:   "method <method-path>",
		Short: "Show one method's signature (input params + output schema)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			m, ok := omnisdk.GetMethod(args[0])
			if !ok {
				return fmt.Errorf("no method %q", args[0])
			}
			return printJSON(m)
		},
	})

	// run: the GENERIC form — plan and run ANY method, supplying its Args as a JSON object deserialized
	// with Go's intrinsic encoding/json (field names are case-insensitive; see the omnisdk.Args / Auth
	// DTOs — {"params":{...},"auth":{...},"endpoint":"...","tuning":{...}}). The transport log is wired
	// here; endpoint/tuning fall back to the global flags when the JSON omits them.
	root.AddCommand(&cobra.Command{
		Use:   "run <method-path> [args-json]",
		Short: `Run any method with Args as JSON, e.g. run google.storage.buckets.list '{"params":{"project":"p"}}'`,
		Args:  cobra.RangeArgs(1, 2),
		RunE: withSinks(func(cmd *cobra.Command, w, logw io.Writer) error {
			pos := cmd.Flags().Args()
			var a omnisdk.Args
			if len(pos) == 2 {
				if err := json.Unmarshal([]byte(pos[1]), &a); err != nil {
					return fmt.Errorf("args: parse: %w", err)
				}
			}
			a.Log = logw
			if a.Endpoint == "" {
				a.Endpoint = endpoint
			}
			if !a.InsecureSkipTLSVerify {
				a.InsecureSkipTLSVerify = insecureTLS
			}
			if (a.Tuning == omnisdk.Tuning{}) {
				a.Tuning = t.facade()
			}
			pl, err := omnisdk.New(pos[0], a)
			if err != nil {
				return err
			}
			return streamRows(pl, w)
		}),
	})

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "omnicli:", err)
		os.Exit(1)
	}
}

// cmdArgs returns the positional arguments cobra parsed for a command whose RunE was wrapped by
// withSinks, which hides them behind its own signature.
func cmdArgs(cmd *cobra.Command) []string { return cmd.Flags().Args() }

// jsonInputs parses a blueprint's inputs: a JSON object of string keys to string values. Nested
// values (tags) are themselves JSON strings, which keeps one flag rather than one per parameter.
func jsonInputs(s string) (map[string]string, error) {
	if strings.TrimSpace(s) == "" {
		return map[string]string{}, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, fmt.Errorf("expected a JSON object: %w", err)
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		var str string
		if err := json.Unmarshal(v, &str); err == nil {
			out[k] = str
			continue
		}
		// A nested object (tags) is kept verbatim, so the blueprint parses it in its own terms.
		out[k] = string(v)
	}
	return out, nil
}

// requireProject / requireGcpOrg add the REQUIRED scope flag. Scope (which project / which org) is
// explicit user input — never inferred from env or the key — so runs are deterministic.
func requireProject(cmd *cobra.Command) {
	cmd.Flags().String("project", "", "GCP project to audit (required)")
	_ = cmd.MarkFlagRequired("project")
}

func requireGcpOrg(cmd *cobra.Command) {
	cmd.Flags().String("gcp-org", "", "GCP organization id to audit (required)")
	_ = cmd.MarkFlagRequired("gcp-org")
}

// openOut resolves the output writer (plain: streamRows encodes sequentially). Empty path → stdout.
func openOut(path string) (io.Writer, func() error, error) {
	if path == "" {
		return os.Stdout, func() error { return nil }, nil
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, nil, err
	}
	return f, f.Close, nil
}

// openLog resolves the raw-log writer; empty path means off (io.Discard). The facade wraps it in a
// concurrency-safe async sink internally (every exchange's log tap writes to it).
func openLog(path string) (io.Writer, func() error, error) {
	if path == "" {
		return io.Discard, func() error { return nil }, nil
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, nil, err
	}
	return f, f.Close, nil
}

func mustFlag(cmd *cobra.Command, name string) string {
	v, _ := cmd.Flags().GetString(name)
	return v
}

// printJSON writes v as indented JSON to stdout (the discovery commands' output).
func printJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

// loadFacadeAuth reads a facade Auth DTO from inline JSON, or from a file when prefixed with '@'.
func loadFacadeAuth(s string) (omnisdk.Auth, error) {
	b := []byte(s)
	if strings.HasPrefix(s, "@") {
		f, err := os.ReadFile(strings.TrimPrefix(s, "@"))
		if err != nil {
			return omnisdk.Auth{}, err
		}
		b = f
	}
	var a omnisdk.Auth
	if err := json.Unmarshal(b, &a); err != nil {
		return omnisdk.Auth{}, fmt.Errorf("auth: parse: %w", err)
	}
	return a, nil
}

// tune holds the user-facing execution knobs.
type tune struct {
	parallelism int
	perHost     int
	retryTries  int
	retryRate   float64
	limit       int
}

// facade maps the CLI knobs onto the public facade's Tuning.
func (t tune) facade() omnisdk.Tuning {
	return omnisdk.Tuning{
		Parallelism: t.parallelism,
		MaxPerHost:  t.perHost,
		RetryTries:  t.retryTries,
		RetryRate:   t.retryRate,
		Limit:       t.limit,
		Timeout:     60 * time.Second,
	}
}

// exprJSON is one expression in a select list, written as exactly one of three things: a literal, a
// field of the row, or a function over further expressions.
type exprJSON struct {
	Field   string          `json:"field,omitempty"`
	Literal json.RawMessage `json:"literal,omitempty"`
	Fn      string          `json:"fn,omitempty"`
	Args    []exprJSON      `json:"args,omitempty"`
}

// selectJSON is an output column: the name it is emitted under, plus the expression inline.
type selectJSON struct {
	Out string `json:"out"`
	exprJSON
}

// parseExpr builds one expression. Exactly one form must be present: a column that named two would
// have to be resolved by a precedence rule nobody stated.
func parseExpr(e exprJSON) (omnisdk.Expression, error) {
	forms := 0
	for _, present := range []bool{e.Field != "", e.Literal != nil, e.Fn != ""} {
		if present {
			forms++
		}
	}
	switch {
	case forms == 0:
		return nil, fmt.Errorf(`expression needs one of "field", "literal" or "fn"`)
	case forms > 1:
		return nil, fmt.Errorf(`expression names more than one of "field", "literal", "fn"`)
	}
	switch {
	case e.Field != "":
		return omnisdk.NewField(e.Field), nil
	case e.Literal != nil:
		var v any
		if err := json.Unmarshal(e.Literal, &v); err != nil {
			return nil, fmt.Errorf("literal: %w", err)
		}
		return omnisdk.NewLiteral(v), nil
	}
	args := make([]omnisdk.Expression, 0, len(e.Args))
	for i, a := range e.Args {
		x, err := parseExpr(a)
		if err != nil {
			return nil, fmt.Errorf("%s argument %d: %w", e.Fn, i+1, err)
		}
		args = append(args, x)
	}
	return omnisdk.NewCall(e.Fn, args...), nil
}
