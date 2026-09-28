package sdk

import (
	"strings"

	"github.com/stackql-labs/omnisdk/internal/system_g/httpx"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
)

// ClientCredentialsSpec is an OAuth2 client-credentials token exchange against tokenURL: the id and
// secret buy an access token, emitted as "token" for the calls that carry it. It is the grant any
// provider states by token URL and scopes; Azure's is the same grant at a login endpoint derived from
// the tenant. The id and secret travel as inputs rather than in the request's text.
func ClientCredentialsSpec(tokenURL string, scopes []string, clientID, clientSecret string) (plan.ExchangeSpec, map[string]any) {
	params := map[string]any{
		"grant_type":    "client_credentials",
		"client_id":     "{client_id}",
		"client_secret": "{client_secret}",
	}
	if len(scopes) > 0 {
		params["scope"] = strings.Join(scopes, " ")
	}
	req := httpx.Request{Method: "POST", URL: tokenURL, Body: httpx.Body{Encoding: httpx.EncodingForm, Params: params}}
	spec := plan.NewExchangeSpec("Token", []string{"client_id", "client_secret"}, []string{"token"},
		httpx.MakeAgnostic(req), httpx.NewJSONExtract(map[string]string{"token": "access_token"}))
	return spec, map[string]any{"client_id": clientID, "client_secret": clientSecret}
}
