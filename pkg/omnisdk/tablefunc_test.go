package omnisdk_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
	"github.com/stackql-labs/omnisdk/pkg/query"
)

// firewalls answers two firewalls, one with two source ranges and one with none.
func firewalls(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"name":"a","sourceRanges":["10.0.0.0/8","1.2.3.4/32"]},{"name":"b","sourceRanges":[]}]`)
	}))
}

func runFirewalls(t *testing.T, srv *httptest.Server, from []query.Join, where []query.Predicate, sel []query.Output) []string {
	t.Helper()
	q, err := query.New(from, where, sel)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	tbl, err := omnisdk.DescribeTable(authRegistry, "stackql_unstable_fw.compute.firewalls")
	if err != nil {
		t.Fatal(err)
	}
	res, err := omnisdk.Resolve(q, map[string]omnisdk.Table{"f": tbl})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	pl, err := omnisdk.NewGraphSelectQuery(authRegistry, res.Graph(), omnisdk.Args{Endpoint: srv.URL, Params: res.Params()})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	rows, err := pl.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		r := rows.Row()
		var parts []string
		for _, o := range sel {
			parts = append(parts, fmt.Sprintf("%s=%v", o.Name(), r[o.Name()]))
		}
		out = append(out, strings.Join(parts, ","))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	slices.Sort(out)
	return out
}

var (
	fwBase   = query.NewJoin(query.NewResource("f", "fw.compute.firewalls"), query.Base)
	fwRanges = query.NewTableFunction("sr", query.NewCall("json_array_elements_text", query.NewColumn("f", "sourceRanges")))
	fwOut    = []query.Output{
		query.NewOutput("name", query.NewColumn("f", "name")),
		query.NewOutput("range", query.NewColumn("sr", "value")),
	}
)

// FROM firewalls f, json_array_elements_text(f.sourceRanges) sr: one row per range, run per
// firewall; a firewall with no ranges has no rows.
func TestTableFunctionCrossJoin(t *testing.T) {
	srv := firewalls(t)
	defer srv.Close()
	got := runFirewalls(t, srv, []query.Join{fwBase, query.NewJoin(fwRanges, query.Cross)}, nil, fwOut)
	if want := []string{"name=a,range=1.2.3.4/32", "name=a,range=10.0.0.0/8"}; !reflect.DeepEqual(got, want) {
		t.Errorf("rows = %v, want %v", got, want)
	}
}

// LEFT JOIN keeps the firewall with no ranges, its range NULL.
func TestTableFunctionLeftJoin(t *testing.T) {
	srv := firewalls(t)
	defer srv.Close()
	got := runFirewalls(t, srv, []query.Join{fwBase, query.NewJoin(fwRanges, query.Left)}, nil, fwOut)
	if want := []string{"name=a,range=1.2.3.4/32", "name=a,range=10.0.0.0/8", "name=b,range=<nil>"}; !reflect.DeepEqual(got, want) {
		t.Errorf("rows = %v, want %v", got, want)
	}
}

// A condition on the function's column filters its rows.
func TestTableFunctionFiltered(t *testing.T) {
	srv := firewalls(t)
	defer srv.Close()
	got := runFirewalls(t, srv, []query.Join{fwBase, query.NewJoin(fwRanges, query.Cross)},
		[]query.Predicate{query.NewTest(query.NewCall("like", query.NewColumn("sr", "value"), query.NewLiteral("10.%")))}, fwOut)
	if want := []string{"name=a,range=10.0.0.0/8"}; !reflect.DeepEqual(got, want) {
		t.Errorf("rows = %v, want %v", got, want)
	}
}

// json_each yields key and value; its arguments may only read the tables before it.
func TestTableFunctionJsonEachAndScope(t *testing.T) {
	srv := firewalls(t)
	defer srv.Close()
	each := query.NewTableFunction("e", query.NewCall("json_each", query.NewColumn("f", "sourceRanges")))
	got := runFirewalls(t, srv, []query.Join{fwBase, query.NewJoin(each, query.Cross)}, nil, []query.Output{
		query.NewOutput("name", query.NewColumn("f", "name")),
		query.NewOutput("key", query.NewColumn("e", "key")),
		query.NewOutput("value", query.NewColumn("e", "value")),
	})
	if want := []string{"name=a,key=0,value=10.0.0.0/8", "name=a,key=1,value=1.2.3.4/32"}; !reflect.DeepEqual(got, want) {
		t.Errorf("rows = %v, want %v", got, want)
	}
	forward := query.NewTableFunction("e", query.NewCall("json_each", query.NewColumn("later", "x")))
	if _, err := query.New([]query.Join{fwBase, query.NewJoin(forward, query.Cross),
		query.NewJoin(query.NewResource("later", "fw.compute.firewalls"), query.Cross)}, nil, fwOut); err == nil {
		t.Error("a table function reading a table joined after it was accepted")
	}
}
