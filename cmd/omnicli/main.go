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
	root.AddCommand(&cobra.Command{
		Use:   "doc-run <bundle-dir> <address>",
		Short: `Run an addressed exchange from a provider bundle, e.g. doc-run ./testdata aws.ec2.instances`,
		Args:  cobra.ExactArgs(2),
		RunE: withSinks(func(cmd *cobra.Command, w, logw io.Writer) error {
			pos := cmd.Flags().Args()
			pl, err := omnisdk.NewFromCatalog(pos[0], pos[1], omnisdk.Args{
				Params:   map[string]string{"region": awsRegion},
				Endpoint: endpoint,
				Log:      logw,
				Tuning:   t.facade(),
			})
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
