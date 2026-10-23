package docx

import (
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/httpx"
)

type xmlTestEnvelope struct{}

func (xmlTestEnvelope) XMLRootName() string       { return "UpdateRequest" }
func (xmlTestEnvelope) XMLDeclaration() string    { return `<?xml version="1.0" encoding="UTF-8"?>` }
func (xmlTestEnvelope) XMLRootAnnotation() string { return `xmlns="https://example.test/"` }

func TestXMLRequestRetainsEnvelopeAndStructuredFragments(t *testing.T) {
	transform := xmlRequestBody{declaration: xmlTestEnvelope{}, schema: requestShape(t).BodySchema()}
	input := httpx.NewRequestRecord("POST", "https://example.test", nil,
		[]byte(`{"Label":"<literal>","Options":"<Retries>3</Retries>","Protocols":["sftp","ftp"]}`))
	output, err := transform.Apply(input)
	if err != nil {
		t.Fatal(err)
	}
	want := `<?xml version="1.0" encoding="UTF-8"?><UpdateRequest xmlns="https://example.test/"><Label>&lt;literal&gt;</Label><Options><Retries>3</Retries></Options><Protocols>sftp</Protocols><Protocols>ftp</Protocols></UpdateRequest>`
	if got := string(httpx.ReqBody(output)); got != want {
		t.Fatalf("wire body = %s, want %s", got, want)
	}
	for _, fragment := range []string{"<Broken>", "</fragment><Other/><fragment>", "<!DOCTYPE unsafe><Value/>"} {
		if err := validateXMLFragment(fragment); err == nil {
			t.Fatalf("accepted invalid structured fragment %q", fragment)
		}
	}
}
