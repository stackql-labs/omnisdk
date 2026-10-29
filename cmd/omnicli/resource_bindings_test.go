package main

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

func TestResourceBindingInputsAndExplicitEnvironment(t *testing.T) {
	t.Setenv("OMNI_BIND_REGION", "ap-southeast-2")
	t.Setenv("OMNI_BIND_UNREQUESTED", "must-not-copy")
	resources, err := omnisdk.ParseResources([]byte(`[{"key":"one","write_once":{"region":{"from":"env","attribute":"OMNI_BIND_REGION"}},"desired":{"name":{"from":"input","attribute":"name"}}}]`))
	if err != nil {
		t.Fatal(err)
	}
	args := omnisdk.Args{Inputs: map[string]any{"name": "args", "keep": true}}
	err = configureResourceBindings(resources, &args, map[string]any{"name": "file"}, `{"name":"cli","count":9007199254740993}`)
	if err != nil {
		t.Fatal(err)
	}
	if args.Inputs["name"] != "cli" || args.Inputs["keep"] != true || args.Inputs["count"] != json.Number("9007199254740993") {
		t.Fatalf("inputs=%v", args.Inputs)
	}
	if len(args.Environment) != 1 || args.Environment["OMNI_BIND_REGION"] != "ap-southeast-2" {
		t.Fatal("unrequested environment read")
	}
}

func TestResourceBindingsCLI(t *testing.T) {
	if os.Getenv("OMNI_BINDING_CLI_CHILD") == "1" {
		for i, arg := range os.Args {
			if arg == "--" {
				os.Args = append([]string{"omnicli"}, os.Args[i+1:]...)
				main()
				return
			}
		}
		os.Exit(2)
	}
	var calls atomic.Int32
	requests := make(chan map[string]any, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		requests <- body
		reply := maps.Clone(body)
		reply["id"] = "w-1"
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(reply)
	}))
	defer server.Close()
	for _, tc := range []struct {
		name, source, attribute, want string
		missing                       bool
	}{
		{"self", "self", "key", "widget", false},
		{"input", "input", "color", "chosen-input", false},
		{"env", "env", "OMNI_BINDING_COLOR", "chosen-env", false},
		{"missing", "input", "absent", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := map[string]any{"inputs": map[string]any{"color": "file-input"}, "resources": []any{map[string]any{"key": "widget", "provider": "widgets", "address": "things.widgets", "identity": "id", "addressed_by": "id", "desired": map[string]any{"size": "S", "color": map[string]any{"from": tc.source, "attribute": tc.attribute}}}}}
			raw, _ := json.Marshal(spec)
			path := filepath.Join(t.TempDir(), "spec.json")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			before := calls.Load()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestResourceBindingsCLI$", "--", "iac-apply", "../../pkg/omnisdk/testdata/authreg", "@"+path, "--input", `{"color":"chosen-input"}`, "--name", "bindings", "--state", t.TempDir(), "--run-id", "run", "--endpoint", server.URL)
			cmd.Env = append(os.Environ(), "OMNI_BINDING_CLI_CHILD=1", "OMNI_BINDING_COLOR=chosen-env")
			out, err := cmd.CombinedOutput()
			if tc.missing {
				if err == nil || !strings.Contains(string(out), "unresolved input") {
					t.Fatalf("error=%v output=%s", err, out)
				}
				if calls.Load() != before {
					t.Fatal("effect before binding failure")
				}
				return
			}
			if err != nil {
				t.Fatalf("error=%v output=%s", err, out)
			}
			if calls.Load() != before+1 {
				t.Fatalf("no successful request: %s", out)
			}
			body := <-requests
			if body["color"] != tc.want || body["size"] != "S" || len(body) != 2 {
				t.Fatalf("request=%v", body)
			}
		})
	}
}
