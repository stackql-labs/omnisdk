package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"strings"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

func configureResourceBindings(resources []omnisdk.ManagedResource, args *omnisdk.Args, fileInputs map[string]any, flag string) error {
	inputs := maps.Clone(args.Inputs)
	if inputs == nil {
		inputs = map[string]any{}
	}
	maps.Copy(inputs, fileInputs)
	if flag != "" {
		raw := []byte(flag)
		if name, isFile := strings.CutPrefix(flag, "@"); isFile {
			var err error
			raw, err = os.ReadFile(name)
			if err != nil {
				return fmt.Errorf("input file: %w", err)
			}
		}
		var values map[string]any
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&values); err != nil {
			return fmt.Errorf("--input must be a JSON object: %w", err)
		}
		if values == nil {
			return fmt.Errorf("--input must be a JSON object")
		}
		var trailing any
		if err := dec.Decode(&trailing); err != io.EOF {
			return fmt.Errorf("--input has trailing data")
		}
		maps.Copy(inputs, values)
	}
	args.Inputs = inputs
	names, err := omnisdk.ResourceEnvironmentVariables(resources)
	if err != nil {
		return err
	}
	env := maps.Clone(args.Environment)
	if env == nil {
		env = map[string]any{}
	}
	for _, name := range names {
		_, given := env[name]
		root, _, _ := strings.Cut(name, ".")
		_, declaredRoot := env[root]
		if given || declaredRoot {
			continue
		}
		if value, found := os.LookupEnv(name); found {
			env[name] = value
		}
	}
	args.Environment = env
	return nil
}
