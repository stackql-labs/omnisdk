package httpx

import (
	"net/http"
	"net/url"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// GetQueryToPostForm is the request translation a document declares as
// get_query_to_post_form_utf_8: the call is described as a GET with its inputs in the query string,
// and sent as a POST with that query as its form-encoded body. The URL carries nothing, so its
// length bounds nothing. It runs before signing, so what is signed is what is sent.
func GetQueryToPostForm() facade.Transform { return getToPost{} }

// GetQueryToPostFormUTF8 names the translation as documents declare it.
const GetQueryToPostFormUTF8 = "get_query_to_post_form_utf_8"

type getToPost struct{}

func (getToPost) Apply(in facade.Page) (facade.Record, error) {
	method, raw, body := Method(in), URL(in), ReqBody(in)
	if method != http.MethodGet || len(body) > 0 {
		return NewRequestRecord(method, raw, Header(in), body), nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	form := []byte(u.RawQuery)
	u.RawQuery = ""
	h := Header(in).Clone()
	if h == nil {
		h = http.Header{}
	}
	h.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	return NewRequestRecord(http.MethodPost, u.String(), h, form), nil
}
