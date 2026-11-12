package omnisdk

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/httpx"
)

func TestIaCDiagnosticExtractsProviderErrorWithoutDumpingResponse(t *testing.T) {
	cause := &httpx.StatusError{Status: 400, URL: "https://secret:password@example.com/?token=hidden", Body: `<Response><Errors><Error><Code>SubnetLimitExceeded</Code><Message>Limit reached</Message></Error></Errors><RequestID>request-123</RequestID><Secret>do-not-store</Secret></Response>`}
	d := failureDiagnostic("effect", &EffectError{Outcome: ErrRejected, Verb: "insert", Alias: "subnet", Cause: cause})
	if d.Outcome != "rejected" || d.ProviderCode != "SubnetLimitExceeded" || d.RequestID != "request-123" || d.Message != "Limit reached" {
		t.Fatal(d)
	}
	if strings.Contains(fmt.Sprint(d), "do-not-store") || strings.Contains(fmt.Sprint(d), "password") {
		t.Fatal(d)
	}
}

func TestIaCDiagnosticStripsCredentialBearingURLs(t *testing.T) {
	d := failureDiagnostic("run", fmt.Errorf("request https://user:password@example.com/path?secret=hidden failed"))
	if strings.Contains(d.Message, "password") || strings.Contains(d.Message, "hidden") || !strings.Contains(d.Message, "example.com/path") {
		t.Fatal(d)
	}
}
