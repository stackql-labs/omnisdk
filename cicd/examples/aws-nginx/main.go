// aws-nginx is a repository-local, document-merged EC2 lifecycle example.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl"
	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
	"github.com/stackql-labs/omnisdk/pkg/query"
	"gopkg.in/yaml.v3"
)

type config struct {
	Registry     string `json:"registry"`
	Collection   string `json:"collection"`
	Region       string `json:"region"`
	ZoneA        string `json:"zone_a"`
	ZoneB        string `json:"zone_b"`
	Image        string `json:"image"`
	InstanceType string `json:"instance_type"`
	SSHCIDR      string `json:"ssh_cidr"`
	HTTPCIDR     string `json:"http_cidr"`
	Prefix       string `json:"prefix"`
	PublicKey    string `json:"public_key"`
	PollInterval string `json:"poll_interval"`
	PollAttempts int    `json:"poll_attempts"`
}
type step struct {
	Key, Resource, Create, Delete, Root, ID, AddressedBy string
	Params                                               map[string]string
	Inputs                                               []omnisdk.Inbound
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) < 2 {
		return errors.New("usage: aws-nginx prepare|apply|status|verify|stop|destroy --case DIR [options]")
	}
	command := os.Args[1]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	directory := flags.String("case", "", "deployment directory")
	registry := flags.String("registry", "test/corpus/registry", "existing provider registry")
	region := flags.String("region", "ap-southeast-2", "AWS region")
	zoneA := flags.String("zone-a", "", "first subnet availability zone")
	zoneB := flags.String("zone-b", "", "second subnet availability zone")
	image := flags.String("image-id", "", "Amazon Linux 2023 x86_64 AMI in this region")
	instanceType := flags.String("instance-type", "t3.micro", "x86_64 instance type")
	sshCIDR := flags.String("ssh-cidr", "", "source CIDR allowed to SSH")
	httpCIDR := flags.String("http-cidr", "0.0.0.0/0", "source CIDR allowed to HTTP")
	prefix := flags.String("prefix", "omni-web", "deployment/key name prefix")
	publicKey := flags.String("public-key", "", "existing OpenSSH public key; otherwise generate a local pair")
	skipStop := flags.Bool("skip-stop", false, "recovery: skip explicit stop before ledger-based destruction")
	endpoint := flags.String("endpoint", "", "mock endpoint; empty uses AWS")
	poll := flags.String("poll-interval", "2s", "observation interval")
	attempts := flags.Int("poll-attempts", 120, "maximum observation attempts")
	if err := flags.Parse(os.Args[2:]); err != nil {
		return err
	}
	if *directory == "" {
		return errors.New("--case is required")
	}
	dir, err := filepath.Abs(*directory)
	if err != nil {
		return err
	}
	if command == "prepare" {
		if *image == "" || *zoneA == "" || *zoneB == "" || *sshCIDR == "" {
			return errors.New("prepare requires --image-id, --zone-a, --zone-b, and --ssh-cidr")
		}
		if !regexp.MustCompile(`^[a-zA-Z0-9-]{1,40}$`).MatchString(*prefix) {
			return errors.New("prefix must be 1–40 letters, digits or hyphens")
		}
		for _, cidr := range []string{*sshCIDR, *httpCIDR} {
			address, _, err := net.ParseCIDR(cidr)
			if err != nil {
				return err
			}
			if address.To4() == nil {
				return errors.New("this example requires IPv4 SSH and HTTP CIDRs")
			}
		}
		if _, err := time.ParseDuration(*poll); err != nil {
			return err
		}
		if *attempts < 1 {
			return errors.New("poll-attempts must be positive")
		}
		if _, err := os.Stat(filepath.Join(dir, "config.json")); err == nil {
			return errors.New("case already prepared; use a new directory")
		}
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		keyPath := *publicKey
		if keyPath == "" {
			private := filepath.Join(dir, "id_ed25519")
			if _, err := os.Stat(private); err == nil {
				return errors.New("local key already exists; refusing to overwrite")
			}
			output, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", *prefix, "-f", private).CombinedOutput()
			if err != nil {
				return fmt.Errorf("ssh-keygen: %w: %s", err, output)
			}
			keyPath = private + ".pub"
		}
		key, err := os.ReadFile(keyPath)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(string(key), "ssh-") {
			return errors.New("public key must be OpenSSH format")
		}
		base, err := filepath.Abs(*registry)
		if err != nil {
			return err
		}
		conf := config{Registry: base, Collection: *prefix, Region: *region, ZoneA: *zoneA, ZoneB: *zoneB, Image: *image, InstanceType: *instanceType, SSHCIDR: *sshCIDR, HTTPCIDR: *httpCIDR, Prefix: *prefix, PublicKey: strings.TrimSpace(string(key)), PollInterval: *poll, PollAttempts: *attempts}
		patch, err := makePatch(conf)
		if err != nil {
			return err
		}
		if err := writeJSON(filepath.Join(dir, "ec2.merge.json"), patch); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "user-data.sh"), []byte(bootstrap(conf.Prefix)), 0600); err != nil {
			return err
		}
		if err := writeJSON(filepath.Join(dir, "config.json"), conf); err != nil {
			return err
		}
		var manifest []map[string]any
		for _, step := range deployment(conf) {
			var edges []map[string]string
			for _, edge := range step.Inputs {
				edges = append(edges, map[string]string{"from": edge.From(), "src": edge.Src(), "as": edge.As()})
			}
			manifest = append(manifest, map[string]any{"key": step.Key, "create": step.Create, "delete": step.Delete, "inputs": edges, "params": step.Params})
		}
		return writeJSON(filepath.Join(dir, "dependency-plan.json"), manifest)
	}
	var conf config
	if err := readJSON(filepath.Join(dir, "config.json"), &conf); err != nil {
		return err
	}
	patch, err := os.ReadFile(filepath.Join(dir, "ec2.merge.json"))
	if err != nil {
		return err
	}
	view, err := omnisdk.EffectiveRegistry(conf.Registry, []omnisdk.DocPatch{{Service: "stackql_unstable_aws.ec2", Merge: patch}}, omnisdk.DocCache{Dir: filepath.Join(dir, "registry-cache")})
	if err != nil {
		return err
	}
	args := omnisdk.Args{Endpoint: *endpoint, Params: map[string]string{"region": conf.Region}, Tuning: omnisdk.Tuning{Parallelism: 4}}
	ctx := context.Background()
	switch command {
	case "apply":
		guard, err := os.OpenFile(filepath.Join(dir, "apply.started"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return errors.New("apply already started; do not replay creates—inspect the ledger and destroy the same collection")
		}
		guard.Close()
		graph, managed, err := createGraph(conf)
		if err != nil {
			return err
		}
		prepared, err := omnisdk.ConvergeGraph(view, conf.Collection, filepath.Join(dir, "state"), "create", graph, managed, args)
		if err != nil {
			return err
		}
		if err := drain(ctx, prepared, filepath.Join(dir, "apply.jsonl")); err != nil {
			return err
		}
		// Instance identity is published by its document-declared completed create.
		rows, err := readJSONLines(filepath.Join(dir, "apply.jsonl"))
		if err != nil {
			return err
		}
		id := ""
		for _, row := range rows {
			if text, ok := row["instance_id"].(string); ok {
				id = text
			}
			if value, ok := row["instance"].(map[string]any); ok {
				if text, ok := value["id"].(string); ok {
					id = text
				}
			}
			if text, ok := row["id"].(string); ok && strings.HasPrefix(text, "i-") {
				id = text
			}
		}
		if id == "" {
			return errors.New("instance handle absent from graph output; inspect apply.jsonl and ledger")
		}
		return writeJSON(filepath.Join(dir, "outputs.json"), map[string]string{"instance_id": id})
	case "status", "verify", "stop":
		var outputs map[string]string
		if err := readJSON(filepath.Join(dir, "outputs.json"), &outputs); err != nil {
			return err
		}
		id := outputs["instance_id"]
		if command == "stop" {
			return stopInstance(ctx, view, dir, id, conf, args)
		}
		row, err := instanceStatus(ctx, view, id, args)
		if err != nil {
			return err
		}
		if command == "status" {
			return json.NewEncoder(os.Stdout).Encode(row)
		}
		ip, _ := row["public_ip"].(string)
		if ip == "" {
			return errors.New("instance has no public IPv4 address yet")
		}
		return verifyHTTP(ctx, ip, conf.Prefix)
	case "destroy":
		// Stop is journalled but does not replace the resource's ownership/dependency
		// record. Destroy then uses the original ledger DAG and the terminate waiter.
		var outputs map[string]string
		if err := readJSON(filepath.Join(dir, "outputs.json"), &outputs); err == nil && !*skipStop {
			if err := stopInstance(ctx, view, dir, outputs["instance_id"], conf, args); err != nil {
				return err
			}
		}
		runID := fmt.Sprintf("destroy-%d", time.Now().UnixNano())
		prepared, err := omnisdk.Prepare(view, omnisdk.Destroy(), omnisdk.NewLedger(conf.Collection, filepath.Join(dir, "state"), runID), args)
		if err != nil {
			return err
		}
		return drain(ctx, prepared, filepath.Join(dir, runID+".jsonl"))
	default:
		return fmt.Errorf("unknown command %s", command)
	}
}
func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0600)
}
func readJSON(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}
func readJSONLines(path string) ([]map[string]any, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	var rows []map[string]any
	for {
		var row map[string]any
		if err := decoder.Decode(&row); err == io.EOF {
			return rows, nil
		} else if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
}
func drain(ctx context.Context, prepared omnisdk.Plan, path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	rows, err := prepared.Open(ctx)
	if err != nil {
		return err
	}
	defer rows.Close()
	encoder := json.NewEncoder(io.MultiWriter(file, os.Stdout))
	failed := false
	for rows.Next() {
		row := rows.Row()
		if err := encoder.Encode(row); err != nil {
			return err
		}
		if row["error"] != nil && row["error"] != "" || row["status"] == "outstanding" {
			failed = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if failed {
		return errors.New("run incomplete; inspect JSONL and retain the ledger")
	}
	return nil
}

func ref(value string) map[string]any { return map[string]any{"$ref": value} }
func makePatch(conf config) (map[string]any, error) {
	data, err := os.ReadFile(filepath.Join(conf.Registry, "aws/v00.00.00000/services/ec2.yaml"))
	if err != nil {
		return nil, err
	}
	var document map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	originals := document["components"].(map[string]any)["x-stackQL-resources"].(map[string]any)
	resources := map[string]any{}
	for _, s := range deployment(conf) {
		original, ok := originals[s.Resource].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("base document missing %s", s.Resource)
		}
		methods := original["methods"].(map[string]any)
		alias := "web_" + s.Key
		copied := map[string]any{}
		for role, name := range map[string]string{"create": s.Create, "delete": s.Delete} {
			raw, ok := methods[name]
			if !ok {
				action := ""
				for _, word := range strings.Split(name, "_") {
					action += strings.ToUpper(word[:1]) + word[1:]
				}
				path := "/?Action=" + action + "&Version=2016-11-15"
				paths := document["paths"].(map[string]any)
				operation, exists := paths[path].(map[string]any)
				if !exists || operation["get"] == nil {
					return nil, fmt.Errorf("base document missing %s.%s and its GET operation", s.Resource, name)
				}
				pointer := strings.ReplaceAll(strings.ReplaceAll(path, "~", "~0"), "/", "~1")
				raw = map[string]any{"operation": ref("#/paths/" + pointer + "/get"), "request": map[string]string{"nativeCasing": "pascal"}, "config": map[string]any{"requestTranslate": map[string]string{"algorithm": "get_query_to_post_form_utf_8"}, "queryParamTranspose": map[string]string{"algorithm": "AWSCanonical"}}}
			}
			encoded, _ := json.Marshal(raw)
			var method map[string]any
			_ = json.Unmarshal(encoded, &method)
			body := `{"ok":true}`
			if role == "create" && s.ID != "" {
				body = `{"id":{{ toJson (index . ` + s.Root + `) }}}`
			}
			method["response"] = response(body, "WebHandle")
			copied[role] = method
		}
		resource := map[string]any{"id": "aws.ec2." + alias, "name": alias, "methods": copied, "sqlVerbs": map[string]any{"insert": []any{ref("#/components/x-stackQL-resources/" + alias + "/methods/create")}, "delete": []any{ref("#/components/x-stackQL-resources/" + alias + "/methods/delete")}}}
		resources[alias] = resource
	}
	// Wait for newly allocated network objects before releasing dependent calls.
	for _, network := range []struct{ alias, source, method, root, set, id, input string }{
		{"web_vpc", "vpcs", "describe_vpcs", "DescribeVpcsResponse", "vpcSet", "vpcId", "VpcId.1"},
		{"web_subnet_a", "subnets", "describe_subnets", "DescribeSubnetsResponse", "subnetSet", "subnetId", "SubnetId.1"},
		{"web_subnet_b", "subnets", "describe_subnets", "DescribeSubnetsResponse", "subnetSet", "subnetId", "SubnetId.1"},
	} {
		methods := resources[network.alias].(map[string]any)["methods"].(map[string]any)
		raw := originals[network.source].(map[string]any)["methods"].(map[string]any)[network.method]
		encoded, _ := json.Marshal(raw)
		var waiter map[string]any
		_ = json.Unmarshal(encoded, &waiter)
		body := `{{ $v := index . "` + network.root + `" "` + network.set + `" "item" }}{"id":{{ toJson (index $v "` + network.id + `") }},"state":{{ toJson (index $v "state") }}}`
		waiter["response"] = response(body, "WebObservation")
		methods["wait_available"] = waiter
		methods["create"].(map[string]any)["x-stackQL-beta"] = map[string]any{"method": ref("#/components/x-stackQL-resources/" + network.alias + "/methods/wait_available"), "bind": map[string]string{network.input: "id"}, "await": map[string]any{"statusPath": "state", "done": "available", "interval": conf.PollInterval, "maxAttempts": conf.PollAttempts}}
	}
	// Resource state is derived by a response transform over the existing read.
	instance := resources["web_instance"].(map[string]any)
	methods := instance["methods"].(map[string]any)
	originalMethods := originals["instances"].(map[string]any)["methods"].(map[string]any)
	for _, state := range []string{"running", "stopped", "terminated"} {
		encoded, _ := json.Marshal(originalMethods["describe_instances"])
		var waiter map[string]any
		_ = json.Unmarshal(encoded, &waiter)
		waiter["response"] = response(instanceRead(), "WebObservation")
		methods["wait_"+state] = waiter
	}
	encoded, _ := json.Marshal(originalMethods["stop_instances"])
	var stop map[string]any
	_ = json.Unmarshal(encoded, &stop)
	stop["response"] = response(`{"id":{{ toJson (index . "StopInstancesResponse" "instancesSet" "item" "instanceId") }}}`, "WebHandle")
	methods["stop"] = stop
	for method, state := range map[string]string{"create": "running", "stop": "stopped", "delete": "terminated"} {
		methods[method].(map[string]any)["x-stackQL-beta"] = map[string]any{"method": ref("#/components/x-stackQL-resources/web_instance/methods/wait_" + state), "bind": map[string]string{}, "await": map[string]any{"statusPath": "state", "done": state, "interval": conf.PollInterval, "maxAttempts": conf.PollAttempts}}
	}
	methods["delete"].(map[string]any)["response"] = response(`{"id":{{ toJson (index . "TerminateInstancesResponse" "instancesSet" "item" "instanceId") }}}`, "WebHandle")
	// Each mutation response supplies the exact handle for its observation.
	for _, method := range []string{"create", "stop", "delete"} {
		methods[method].(map[string]any)["x-stackQL-beta"].(map[string]any)["bind"] = map[string]string{"InstanceId.1": "id"}
	}
	instance["sqlVerbs"].(map[string]any)["select"] = []any{ref("#/components/x-stackQL-resources/web_instance/methods/wait_running")}
	// Specialise only this isolated overlay to one instance. EC2's actual wire
	// input is InstanceId.1; naming it explicitly also makes required-input AOT
	// checks precise for stop and terminate (the source schema names the list).
	pathsPatch := map[string]any{}
	for _, action := range []string{"StopInstances", "TerminateInstances"} {
		path := "/?Action=" + action + "&Version=2016-11-15"
		operation := document["paths"].(map[string]any)[path].(map[string]any)["get"].(map[string]any)
		encoded, _ := json.Marshal(operation["parameters"])
		var parameters []map[string]any
		_ = json.Unmarshal(encoded, &parameters)
		for _, parameter := range parameters {
			if parameter["name"] == "InstanceId" {
				parameter["required"] = false
				parameters = append(parameters, map[string]any{"name": "InstanceId.1", "in": "query", "required": true, "schema": map[string]string{"type": "string"}})
				break
			}
		}
		pathsPatch[path] = map[string]any{"get": map[string]any{"parameters": parameters}}
	}
	return map[string]any{"paths": pathsPatch, "components": map[string]any{"x-stackQL-resources": resources, "schemas": map[string]any{
		"WebHandle":      map[string]any{"type": "object", "properties": map[string]any{"id": map[string]string{"type": "string"}, "ok": map[string]string{"type": "boolean"}}},
		"WebObservation": map[string]any{"type": "object", "properties": map[string]any{"id": map[string]string{"type": "string"}, "state": map[string]string{"type": "string"}, "public_ip": map[string]string{"type": "string"}}},
	}}}, nil
}
func response(body, schema string) map[string]any {
	return map[string]any{"openAPIDocKey": "200", "mediaType": "application/xml", "overrideMediaType": "application/json", "schema_override": ref("#/components/schemas/" + schema), "objectKey": "$", "transform": map[string]string{"type": "golang_template_mxj_v0.1.0", "body": body}}
}
func instanceRead() string {
	return `{{ $r := index . "DescribeInstancesResponse" }}{{ if index $r "reservationSet" }}{{ $v := index $r "reservationSet" "item" "instancesSet" "item" }}{"id":{{ toJson (index $v "instanceId") }},"state":{{ toJson (index $v "instanceState" "name") }},"public_ip":{{ toJson (index $v "ipAddress") }}}{{ else }}{"state":"unknown"}{{ end }}`
}

func deployment(c config) []step {
	in := func(from, as string) omnisdk.Inbound {
		field := "id"
		if from == "gateway_attachment" {
			field = "InternetGatewayId"
		}
		if from == "ingress" {
			field = "GroupId"
		}
		if from == "default_route" {
			field = "RouteTableId"
		}
		return omnisdk.NewInbound(from, field, as)
	}
	tag := func(kind, name string, params map[string]string) map[string]string {
		params["TagSpecification.1.ResourceType"] = kind
		params["TagSpecification.1.Tag.1.Key"] = "Name"
		params["TagSpecification.1.Tag.1.Value"] = c.Prefix + "-" + name
		return params
	}
	return []step{
		{Key: "vpc", Resource: "vpcs", Create: "create_vpc", Delete: "delete_vpc", ID: "id", Root: `"CreateVpcResponse" "vpc" "vpcId"`, AddressedBy: "VpcId", Params: tag("vpc", "vpc", map[string]string{"CidrBlock": "10.77.0.0/16"})},
		{Key: "subnet_a", Resource: "subnets", Create: "create_subnet", Delete: "delete_subnet", ID: "id", Root: `"CreateSubnetResponse" "subnet" "subnetId"`, AddressedBy: "SubnetId", Params: tag("subnet", "a", map[string]string{"CidrBlock": "10.77.1.0/24", "AvailabilityZone": c.ZoneA}), Inputs: []omnisdk.Inbound{in("vpc", "VpcId")}},
		{Key: "subnet_b", Resource: "subnets", Create: "create_subnet", Delete: "delete_subnet", ID: "id", Root: `"CreateSubnetResponse" "subnet" "subnetId"`, AddressedBy: "SubnetId", Params: tag("subnet", "b", map[string]string{"CidrBlock": "10.77.2.0/24", "AvailabilityZone": c.ZoneB}), Inputs: []omnisdk.Inbound{in("vpc", "VpcId")}},
		{Key: "gateway", Resource: "internet_gateways", Create: "create_internet_gateway", Delete: "delete_internet_gateway", ID: "id", Root: `"CreateInternetGatewayResponse" "internetGateway" "internetGatewayId"`, AddressedBy: "InternetGatewayId", Params: tag("internet-gateway", "gateway", map[string]string{})},
		{Key: "gateway_attachment", Resource: "internet_gateways", Create: "attach_internet_gateway", Delete: "detach_internet_gateway", AddressedBy: "InternetGatewayId", Inputs: []omnisdk.Inbound{in("gateway", "InternetGatewayId"), in("vpc", "VpcId")}},
		{Key: "route_table", Resource: "route_tables", Create: "create_route_table", Delete: "delete_route_table", ID: "id", Root: `"CreateRouteTableResponse" "routeTable" "routeTableId"`, AddressedBy: "RouteTableId", Params: tag("route-table", "routes", map[string]string{}), Inputs: []omnisdk.Inbound{in("vpc", "VpcId")}},
		{Key: "default_route", Resource: "route_tables", Create: "create_route", Delete: "delete_route", AddressedBy: "RouteTableId", Params: map[string]string{"DestinationCidrBlock": "0.0.0.0/0"}, Inputs: []omnisdk.Inbound{in("route_table", "RouteTableId"), in("gateway_attachment", "GatewayId")}},
		{Key: "association_a", Resource: "route_tables", Create: "associate_route_table", Delete: "disassociate_route_table", ID: "id", Root: `"AssociateRouteTableResponse" "associationId"`, AddressedBy: "AssociationId", Inputs: []omnisdk.Inbound{in("route_table", "RouteTableId"), in("subnet_a", "SubnetId")}},
		{Key: "association_b", Resource: "route_tables", Create: "associate_route_table", Delete: "disassociate_route_table", ID: "id", Root: `"AssociateRouteTableResponse" "associationId"`, AddressedBy: "AssociationId", Inputs: []omnisdk.Inbound{in("route_table", "RouteTableId"), in("subnet_b", "SubnetId")}},
		{Key: "security_group", Resource: "security_groups", Create: "create_security_group", Delete: "delete_security_group", ID: "id", Root: `"CreateSecurityGroupResponse" "groupId"`, AddressedBy: "GroupId", Params: tag("security-group", "ssh-http", map[string]string{"GroupName": c.Prefix + "-web", "GroupDescription": "omnisdk nginx demo"}), Inputs: []omnisdk.Inbound{in("vpc", "VpcId")}},
		{Key: "ingress", Resource: "security_groups", Create: "authorize_security_group_ingress", Delete: "revoke_security_group_ingress", AddressedBy: "GroupId", Params: map[string]string{
			"IpPermissions.1.IpProtocol": "tcp", "IpPermissions.1.FromPort": "22", "IpPermissions.1.ToPort": "22", "IpPermissions.1.IpRanges.1.CidrIp": c.SSHCIDR,
			"IpPermissions.2.IpProtocol": "tcp", "IpPermissions.2.FromPort": "80", "IpPermissions.2.ToPort": "80", "IpPermissions.2.IpRanges.1.CidrIp": c.HTTPCIDR,
		}, Inputs: []omnisdk.Inbound{in("security_group", "GroupId")}},
		{Key: "key", Resource: "key_pairs", Create: "import_key_pair", Delete: "delete_key_pair", ID: "id", Root: `"ImportKeyPairResponse" "keyName"`, AddressedBy: "KeyName", Params: map[string]string{"KeyName": c.Prefix + "-ssh", "PublicKeyMaterial": base64.StdEncoding.EncodeToString([]byte(c.PublicKey))}},
		{Key: "instance", Resource: "instances", Create: "run_instances", Delete: "terminate_instances", ID: "id", Root: `"RunInstancesResponse" "instancesSet" "item" "instanceId"`, AddressedBy: "InstanceId.1", Params: tag("instance", "nginx", map[string]string{
			"ImageId": c.Image, "InstanceType": c.InstanceType, "MinCount": "1", "MaxCount": "1", "ClientToken": c.Prefix + "-instance",
			"NetworkInterface.1.DeviceIndex": "0", "NetworkInterface.1.AssociatePublicIpAddress": "true",
			"MetadataOptions.HttpTokens": "required", "UserData": base64.StdEncoding.EncodeToString([]byte(bootstrap(c.Prefix))),
		}), Inputs: []omnisdk.Inbound{in("subnet_a", "subnet_id"), in("key", "key_name"), in("ingress", "security_group"), in("default_route", "route_ready"), in("association_a", "association_ready")}},
	}
}
func createGraph(conf config) (omnisdk.Graph, []omnisdk.Managed, error) {
	var nodes []omnisdk.Node
	var wirings []omnisdk.Wiring
	var managed []omnisdk.Managed
	for _, s := range deployment(conf) {
		nodes = append(nodes, omnisdk.NewMutationNode(s.Key, "stackql_unstable_aws.ec2.web_"+s.Key, "insert", s.Params, nil, nil))
		if len(s.Inputs) > 0 {
			if s.Key == "instance" {
				wirings = append(wirings, omnisdk.NewWiringWithTransform(s.Key, s.Inputs, dsl.Transform{Type: "starlark", Required: []string{"subnet_id", "key_name", "security_group", "route_ready", "association_ready"}, Body: `{"NetworkInterface.1.SubnetId":inbox["subnet_id"],"NetworkInterface.1.SecurityGroupId.1":inbox["security_group"],"KeyName":inbox["key_name"]}`}, "NetworkInterface.1.SubnetId", "NetworkInterface.1.SecurityGroupId.1", "KeyName"))
			} else {
				wirings = append(wirings, omnisdk.NewWiring(s.Key, s.Inputs, "", ""))
			}
		}
		managed = append(managed, omnisdk.NewManaged(s.Key, []string{s.Key}, "id", s.AddressedBy))
	}
	graph, err := omnisdk.NewGraphWithFilters(nodes, wirings, nil, omnisdk.NewRowOps(nil, nil, []query.Output{query.NewOutput("instance_id", query.NewColumn("instance", "id")), query.NewOutput("public_ip", query.NewColumn("instance", "public_ip")), query.NewOutput("state", query.NewColumn("instance", "state"))}))
	return graph, managed, err
}
func instanceStatus(ctx context.Context, view, id string, args omnisdk.Args) (map[string]any, error) {
	graph, err := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewNode("instance", "stackql_unstable_aws.ec2.web_instance", map[string]string{"InstanceId.1": id})}, nil)
	if err != nil {
		return nil, err
	}
	prepared, err := omnisdk.NewGraphQuery(view, graph, args)
	if err != nil {
		return nil, err
	}
	rows, err := prepared.Open(ctx)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var last map[string]any
	for rows.Next() {
		last = rows.Row()
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if inner, ok := last["instance"].(map[string]any); ok {
		return inner, nil
	}
	return last, nil
}
func stopInstance(ctx context.Context, view, dir, id string, conf config, args omnisdk.Args) error {
	status, err := instanceStatus(ctx, view, id, args)
	if err != nil {
		return err
	}
	if status["state"] == "terminated" || status["state"] == "stopped" {
		return nil
	}
	args.Journal = &omnisdk.Journal{State: filepath.Join(dir, "state"), RunID: fmt.Sprintf("stop-%d", time.Now().UnixNano())}
	graph, err := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewExecNode("stop", "stackql_unstable_aws.ec2.web_instance", "stop", map[string]string{"InstanceId.1": id}, nil)}, nil)
	if err != nil {
		return err
	}
	prepared, err := omnisdk.NewGraphQuery(view, graph, args)
	if err != nil {
		return err
	}
	return drain(ctx, prepared, filepath.Join(dir, args.Journal.RunID+".jsonl"))
}
func verifyHTTP(ctx context.Context, ip, prefix string) error {
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(ip, "80")+"/healthz", nil)
		reply, err := client.Do(request)
		if err == nil {
			body, _ := io.ReadAll(io.LimitReader(reply.Body, 4096))
			reply.Body.Close()
			if reply.StatusCode == 200 && strings.Contains(string(body), "omnisdk "+prefix+" ready") {
				fmt.Printf("nginx verified at http://%s/\n", ip)
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return errors.New("nginx did not become ready within five minutes; inspect cloud-init over SSH")
}
func bootstrap(prefix string) string {
	return `#!/bin/bash
set -euxo pipefail
dnf install -y nginx
cat > /etc/nginx/nginx.conf <<'NGINX'
user nginx;
worker_processes auto;
pid /run/nginx.pid;
error_log /var/log/nginx/error.log;
events { worker_connections 1024; }
http {
 include /etc/nginx/mime.types;
 default_type application/octet-stream;
 access_log /var/log/nginx/access.log;
 server {
  listen 80 default_server;
  server_name _;
  root /usr/share/nginx/html;
  add_header X-Omnisdk-Deployment "` + prefix + `" always;
  location / { try_files $uri $uri/ =404; }
  location = /healthz { default_type text/plain; return 200 "omnisdk ` + prefix + ` ready\n"; }
 }
}
NGINX
cat > /usr/share/nginx/html/index.html <<'HTML'
<!doctype html><html><head><title>omnisdk nginx</title></head><body><h1>omnisdk ` + prefix + `</h1><p>nginx, provisioned through merged EC2 documents.</p></body></html>
HTML
nginx -t
systemctl enable --now nginx
`
}
