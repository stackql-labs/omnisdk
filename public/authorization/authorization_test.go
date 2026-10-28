package authorization_test

import (
	"context"
	"sync"
	"testing"

	"github.com/stackql-labs/omnisdk/public/authorization"
	"github.com/stackql-labs/omnisdk/public/principal"
)

func TestDirectoryGroupsAndCallerIsolation(t *testing.T) {
	d, err := principal.NewMemoryDirectoryFromBytes([]byte(`{"groups":["readers"],"principals":[{"id":"alice","identities":[{"issuer":"one","subject":"same"}],"groups":["readers"]},{"id":"bob","identities":[{"issuer":"two","subject":"same"}],"groups":[]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	a, err := authorization.NewCasbin([]authorization.Rule{{Subject: "group:readers", Resource: "operation/demo/**", Action: "execute"}})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for n := 0; n < 30; n++ {
		for _, issuer := range []string{"one", "two"} {
			wg.Add(1)
			go func(issuer string) {
				defer wg.Done()
				i, _ := principal.NewIdentity(issuer, "same")
				p, err := d.Resolve(context.Background(), i)
				if err != nil {
					t.Error(err)
					return
				}
				groups := p.Groups()
				if len(groups) > 0 {
					groups[0] = "admin"
				}
				err = a.Authorize(context.Background(), p, "operation/demo/items/list", "execute")
				if (err == nil) != (issuer == "one") {
					t.Errorf("issuer=%s err=%v", issuer, err)
				}
				if a.Authorize(context.Background(), p, "credential/demo", "use") == nil {
					t.Error("credential inheritance")
				}
			}(issuer)
		}
	}
	wg.Wait()
	i, _ := principal.NewIdentity("unknown", "same")
	if _, err := authorization.NewSession(context.Background(), i, d, a); err == nil {
		t.Fatal("unresolved identity accepted")
	}
}
func TestDirectoryRejectsAmbiguity(t *testing.T) {
	for _, data := range []string{
		`{"groups":["g","g"]}`,
		`{"principals":[{"id":"a","identities":[{"issuer":"i","subject":"s"}]},{"id":"b","identities":[{"issuer":"i","subject":"s"}]}]}`,
		`{"principals":[{"id":"a","identities":[{"issuer":"i","subject":"s"}],"groups":["missing"]}]}`,
		`{"unknown":true}`,
	} {
		if _, err := principal.NewMemoryDirectoryFromBytes([]byte(data)); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
}
