package omnisdk_test

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

// The storage examples converge one tagged bucket per cloud from desired state. Each runs three
// times: the first creates, the second only reads, and after the tags are changed outside the tool the
// third puts them back.

// wireLog records what a stand-in was asked, method and path, in order.
type wireLog struct {
	mu  sync.Mutex
	got []string
}

func (c *wireLog) add(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, s)
}

func (c *wireLog) took() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := strings.Join(c.got, ", ")
	c.got = nil
	return out
}

// converge runs desired state under the collection "demo" and fails on any error a row reports.
func converge(t *testing.T, registry, state, runID string, args omnisdk.Args, rs ...omnisdk.ManagedResource) {
	t.Helper()
	pl, err := omnisdk.Prepare(registry, omnisdk.DesiredState(rs), omnisdk.NewLedger("demo", state, runID), args)
	if err != nil {
		t.Fatalf("%s: prepare: %v", runID, err)
	}
	for _, row := range drain(t, pl) {
		if row["error"] != nil {
			t.Fatalf("%s: %v", runID, row)
		}
	}
}

// A GCS bucket with labels: created by insert, read back by get, and its labels corrected by patch.
func TestExampleGCSBucket(t *testing.T) {
	requireCorpus(t)
	t.Setenv("GOOGLE_CREDENTIALS", serviceAccountKey(t))
	var c wireLog
	var mu sync.Mutex
	var bucket map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/token") {
			fmt.Fprint(w, `{"access_token":"tok","expires_in":3600}`)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/b"):
			c.add("insert")
			bucket = body
			bucket["id"] = body["name"]
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/b/omni-demo"):
			c.add("get")
			if bucket == nil {
				http.Error(w, `{"error":{"code":404}}`, http.StatusNotFound)
				return
			}
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/b/omni-demo"):
			c.add("patch")
			for k, v := range body {
				bucket[k] = v
			}
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(bucket)
	}))
	defer srv.Close()

	b := omnisdk.NewResource("google/storage/bucket", "google", "storage.buckets",
		[]byte(`{"name":"omni-demo","labels":{"env":"dev","team":"platform"}}`), nil, nil, "", "", "name", "bucket", "")
	args := omnisdk.Args{Endpoint: srv.URL, Params: map[string]string{"project": "demo"}}
	state := t.TempDir()

	converge(t, corpus, state, "r1", args, b)
	if got := c.took(); got != "insert" {
		t.Errorf("first run: %s, want an insert", got)
	}
	converge(t, corpus, state, "r2", args, b)
	if got := c.took(); got != "get" {
		t.Errorf("rerun: %s, want only a read", got)
	}
	mu.Lock()
	bucket["labels"] = map[string]any{"env": "prod", "team": "platform"}
	mu.Unlock()
	converge(t, corpus, state, "r3", args, b)
	if got := c.took(); got != "get, patch" {
		t.Errorf("after drift: %s, want a read and a patch", got)
	}
	if fmt.Sprint(bucket["labels"]) != "map[env:dev team:platform]" {
		t.Errorf("labels = %v, want them put back", bucket["labels"])
	}
}

// An Azure storage account with tags: created by PUT, read back by listing its resource group and
// matching its name — the document has no get — and its tags corrected by PATCH.
func TestExampleAzureStorageAccount(t *testing.T) {
	requireCorpus(t)
	t.Setenv("AZURE_TENANT_ID", "t")
	t.Setenv("AZURE_CLIENT_ID", "c")
	t.Setenv("AZURE_CLIENT_SECRET", "s")
	var c wireLog
	var mu sync.Mutex
	var account map[string]any
	const base = "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Storage/storageAccounts"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/token") {
			fmt.Fprint(w, `{"access_token":"tok","expires_in":3600}`)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		switch {
		case r.Method == http.MethodPut && r.URL.Path == base+"/omnidemo":
			c.add("create")
			account = body
			account["name"] = "omnidemo"
		case r.Method == http.MethodGet && r.URL.Path == base:
			c.add("list")
			other := map[string]any{"name": "someoneelse", "location": "eastus", "tags": map[string]any{}}
			rows := []any{other}
			if account != nil {
				rows = append(rows, account)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"value": rows})
			return
		case r.Method == http.MethodPatch && r.URL.Path == base+"/omnidemo":
			c.add("update")
			for k, v := range body {
				account[k] = v
			}
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(account)
	}))
	defer srv.Close()

	a := omnisdk.NewResource("azure/storage/account", "azure", "storage.storage_accounts",
		[]byte(`{"location":"eastus","kind":"StorageV2","sku":{"name":"Standard_LRS"},"tags":{"env":"dev"}}`),
		map[string]string{"resource_group_name": "rg", "account_name": "omnidemo"}, nil, "", "", "name", "account_name", "")
	args := omnisdk.Args{Endpoint: srv.URL, Params: map[string]string{"subscription_id": "sub"}}
	state := t.TempDir()

	converge(t, corpus, state, "r1", args, a)
	if got := c.took(); got != "create" {
		t.Errorf("first run: %s, want a create", got)
	}
	converge(t, corpus, state, "r2", args, a)
	if got := c.took(); got != "list" {
		t.Errorf("rerun: %s, want only a read", got)
	}
	mu.Lock()
	account["tags"] = map[string]any{"env": "prod"}
	mu.Unlock()
	converge(t, corpus, state, "r3", args, a)
	if got := c.took(); got != "list, update" {
		t.Errorf("after drift: %s, want a read and an update", got)
	}
	if fmt.Sprint(account["tags"]) != "map[env:dev]" {
		t.Errorf("tags = %v, want them put back", account["tags"])
	}
}

// An S3 bucket and its tags. The bucket is created by PUT and read back by listing buckets and
// matching its name — the document has no get. Its tags are a resource of their own, written whole by
// PUT ?tagging with an XML body, the only write the document declares for them.
func TestExampleS3BucketWithTags(t *testing.T) {
	requireCorpus(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIATEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	var c wireLog
	var mu sync.Mutex
	created := false
	tagging := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/xml")
		_, tagged := r.URL.Query()["tagging"]
		b, _ := io.ReadAll(r.Body)
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/omni-demo" && tagged:
			// S3 refuses a tagging body without its own integrity header.
			sum := md5.Sum(b)
			if r.Header.Get("Content-MD5") != base64.StdEncoding.EncodeToString(sum[:]) {
				http.Error(w, `<Error><Code>InvalidRequest</Code><Message>Missing required header for this request: Content-MD5</Message></Error>`, http.StatusBadRequest)
				return
			}
			c.add("put tagging")
			tagging = string(b)
		case r.Method == http.MethodPut && r.URL.Path == "/omni-demo":
			c.add("create")
			created = true
		case r.Method == http.MethodGet && r.URL.Path == "/":
			c.add("list")
			fmt.Fprint(w, `<ListAllMyBucketsResult><Buckets><Bucket><Name>someone-else</Name></Bucket>`)
			if created {
				fmt.Fprint(w, `<Bucket><Name>omni-demo</Name></Bucket>`)
			}
			fmt.Fprint(w, `</Buckets></ListAllMyBucketsResult>`)
		case r.Method == http.MethodGet && r.URL.Path == "/omni-demo" && tagged:
			c.add("get tagging")
			fmt.Fprint(w, tagging)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.String(), http.StatusNotFound)
		}
	}))
	defer srv.Close()

	bucket := omnisdk.NewResource("aws/s3/bucket", "aws", "s3.buckets", []byte(`{}`),
		map[string]string{"bucket": "omni-demo"}, nil, "", "", "Name", "bucket", "")
	tags := omnisdk.NewResource("aws/s3/bucket-tags", "aws", "s3.bucket_taggings",
		[]byte(`{"Tagging":{"TagSet":{"Tag":[{"Key":"env","Value":"dev"},{"Key":"team","Value":"platform"}]}}}`),
		nil, []omnisdk.Arrival{{From: "aws/s3/bucket", As: "bucket"}}, "", "", "", "bucket", "")
	args := omnisdk.Args{Endpoint: srv.URL, Params: map[string]string{"region": "us-east-1"}}
	state := t.TempDir()

	converge(t, corpus, state, "r1", args, bucket, tags)
	if got := c.took(); got != "create, put tagging" {
		t.Errorf("first run: %s, want the bucket then its tags", got)
	}
	want := `<Tagging><TagSet><Tag><Key>env</Key><Value>dev</Value></Tag><Tag><Key>team</Key><Value>platform</Value></Tag></TagSet></Tagging>`
	if tagging != want {
		t.Errorf("tagging body = %s, want %s", tagging, want)
	}
	// The bucket is read and kept. The tags are read and written again: what S3 reads back for them
	// is not the document written, so they never compare equal — a whole PUT, so the same tags.
	converge(t, corpus, state, "r2", args, bucket, tags)
	if got := c.took(); strings.Contains(got, "create") || !strings.Contains(got, "list") {
		t.Errorf("rerun: %s, want the bucket read and not created", got)
	}
	mu.Lock()
	tagging = `<Tagging><TagSet><Tag><Key>env</Key><Value>prod</Value></Tag></TagSet></Tagging>`
	mu.Unlock()
	converge(t, corpus, state, "r3", args, bucket, tags)
	c.took()
	if tagging != want {
		t.Errorf("tagging after drift = %s, want it put back", tagging)
	}
}

// An Azure blob container with metadata, inside the storage account it reads its name from. The
// container is created once the account exists, and read back within that account.
func TestExampleAzureContainerWithMetadata(t *testing.T) {
	requireCorpus(t)
	t.Setenv("AZURE_TENANT_ID", "t")
	t.Setenv("AZURE_CLIENT_ID", "c")
	t.Setenv("AZURE_CLIENT_SECRET", "s")
	var c wireLog
	var mu sync.Mutex
	var account, container map[string]any
	const base = "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Storage/storageAccounts"
	const cpath = base + "/omnidemo/blobServices/default/containers/omni-demo"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/token") {
			fmt.Fprint(w, `{"access_token":"tok","expires_in":3600}`)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		switch {
		case r.Method == http.MethodPut && r.URL.Path == base+"/omnidemo":
			c.add("create account")
			account = body
			account["name"] = "omnidemo"
			_ = json.NewEncoder(w).Encode(account)
		case r.Method == http.MethodGet && r.URL.Path == base:
			rows := []any{}
			if account != nil {
				rows = append(rows, account)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"value": rows})
		case r.Method == http.MethodPut && r.URL.Path == cpath:
			if account == nil {
				http.Error(w, `{"error":{"code":"ParentResourceNotFound"}}`, http.StatusNotFound)
				return
			}
			c.add("create container")
			container = body
			container["name"] = "omni-demo"
			_ = json.NewEncoder(w).Encode(container)
		case r.Method == http.MethodDelete && r.URL.Path == cpath:
			c.add("delete container")
			container = nil
		case r.Method == http.MethodDelete && r.URL.Path == base+"/omnidemo":
			if container != nil {
				http.Error(w, `{"error":{"code":"Conflict"}}`, http.StatusConflict)
				return
			}
			c.add("delete account")
			account = nil
		case r.Method == http.MethodGet && r.URL.Path == cpath:
			c.add("get container")
			if container == nil {
				http.Error(w, `{"error":{"code":"ContainerNotFound"}}`, http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(container)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	account0 := omnisdk.NewResource("azure/storage/account", "azure", "storage.storage_accounts",
		[]byte(`{"location":"eastus","kind":"StorageV2","sku":{"name":"Standard_LRS"},"tags":{"env":"dev"}}`),
		map[string]string{"resource_group_name": "rg", "account_name": "omnidemo"}, nil, "", "", "name", "account_name", "")
	container0 := omnisdk.NewResource("azure/storage/container", "azure", "storage.blob_containers",
		[]byte(`{"properties":{"metadata":{"env":"dev","team":"platform"}}}`),
		map[string]string{"resource_group_name": "rg", "container_name": "omni-demo"},
		[]omnisdk.Arrival{{From: "azure/storage/account", As: "account_name"}}, "", "", "name", "container_name", "")
	args := omnisdk.Args{Endpoint: srv.URL, Params: map[string]string{"subscription_id": "sub"}}
	state := t.TempDir()

	converge(t, corpus, state, "r1", args, account0, container0)
	if got := c.took(); got != "create account, create container" {
		t.Errorf("first run: %s, want the account then the container", got)
	}
	if fmt.Sprint(container["properties"]) != "map[metadata:map[env:dev team:platform]]" {
		t.Errorf("container = %v, want its metadata", container)
	}
	converge(t, corpus, state, "r2", args, account0, container0)
	if got := c.took(); got != "get container" {
		t.Errorf("rerun: %s, want only reads", got)
	}

	// Destroy deletes the container within its account — whose name it read from the account, and
	// the ledger recorded where it came from — then the account.
	pl, err := omnisdk.Prepare(corpus, omnisdk.Destroy(), omnisdk.NewLedger("demo", state, "d1"), args)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range drain(t, pl) {
		if row["status"] != "destroyed" {
			t.Errorf("destroy: %v", row)
		}
	}
	if got := c.took(); got != "delete container, delete account" {
		t.Errorf("destroy: %s, want the container then the account", got)
	}
}

// A parameter given empty — an unset variable spliced into a spec — is refused by name, before
// anything runs.
func TestEmptyParameterIsRefusedByName(t *testing.T) {
	requireCorpus(t)
	r := omnisdk.NewResource("azure/storage/account", "azure", "storage.storage_accounts", []byte(`{"location":"eastus"}`),
		map[string]string{"resource_group_name": "", "account_name": "omnidemo"}, nil, "", "", "name", "account_name", "")
	_, err := omnisdk.Prepare(corpus, omnisdk.DesiredState([]omnisdk.ManagedResource{r}), omnisdk.NewLedger("demo", t.TempDir(), "r1"),
		omnisdk.Args{Params: map[string]string{"subscription_id": "sub"}})
	if err == nil || !strings.Contains(err.Error(), `"resource_group_name" is empty`) {
		t.Errorf("err = %v, want the empty parameter named", err)
	}
}
