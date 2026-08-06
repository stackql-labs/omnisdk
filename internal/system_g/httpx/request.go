package httpx

import (
	"bufio"
	"io"
	"net/http"
	"net/textproto"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/record"
	"github.com/stackql-labs/omnisdk/internal/system_g/value"
)

// An HTTP request modelled as a facade.Record, so it can flow between behaviour nodes and be
// transformed (e.g. signed) before sending. facade.Record has no key iterator, so headers are
// serialized into one value (HTTP wire form).
const (
	KeyMethod  = "req:method"
	KeyURL     = "req:url"
	KeyHeaders = "req:headers"
	KeyBody    = "req:body"
)

// NewRequestRecord builds a request record. header and body may be nil.
func NewRequestRecord(method, url string, header http.Header, body []byte) facade.Record {
	return record.NewRecord(map[string]facade.Value{
		KeyMethod:  value.NewBytesValue([]byte(method)),
		KeyURL:     value.NewBytesValue([]byte(url)),
		KeyHeaders: value.NewBytesValue(encodeHeader(header)),
		KeyBody:    value.NewBytesValue(body),
	})
}

// Method/URL/Header/ReqBody read a request page. They take a facade.Page (the materialized
// view), so a request-path transform reads them without any source handle; a facade.Record
// satisfies Page, so operators pass records to them too.
func Method(p facade.Page) string      { return string(p.Bytes(KeyMethod)) }
func URL(p facade.Page) string         { return string(p.Bytes(KeyURL)) }
func Header(p facade.Page) http.Header { return decodeHeader(p.Bytes(KeyHeaders)) }
func ReqBody(p facade.Page) []byte     { return p.Bytes(KeyBody) }

func encodeHeader(h http.Header) []byte {
	if len(h) == 0 {
		return nil
	}
	var b strings.Builder
	_ = h.Write(&b)
	return []byte(b.String())
}

func decodeHeader(b []byte) http.Header {
	h := http.Header{}
	if len(b) == 0 {
		return h
	}
	r := textproto.NewReader(bufio.NewReader(strings.NewReader(string(b) + "\r\n")))
	mime, err := r.ReadMIMEHeader()
	if err != nil && err != io.EOF {
		return h
	}
	for k, vs := range mime {
		h[k] = vs
	}
	return h
}
