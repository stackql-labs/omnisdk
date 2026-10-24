package docx

import (
	"bytes"
	"fmt"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/httpx"
	"github.com/stackql-labs/omnisdk/internal/system_g/record"
	"github.com/stackql-labs/omnisdk/internal/system_g/value"
	"strconv"
)

type requestObservation struct{ observer facade.HTTPObserver }

func (o requestObservation) Apply(page facade.Page) (facade.Record, error) {
	method, url, body := httpx.Method(page), httpx.URL(page), httpx.ReqBody(page)
	o.observer.HTTPRequest(method, url, bytes.Clone(body))
	return httpx.NewRequestRecord(method, url, httpx.Header(page), body), nil
}

type responseObservation struct{ observer facade.HTTPObserver }

func (o responseObservation) Apply(page facade.Page) (facade.Record, error) {
	status := page.Bytes(httpx.KeyStatus)
	code, _ := strconv.Atoi(string(status))
	body := page.Bytes(facade.AnonymousPayload)
	o.observer.HTTPResponse(code, bytes.Clone(body))
	return record.NewRecord(map[string]facade.Value{httpx.KeyStatus: value.NewBytesValue(status), httpx.KeyURL: value.NewBytesValue(page.Bytes(httpx.KeyURL)), facade.AnonymousPayload: value.NewBytesValue(body)}), nil
}

type rowsObservation struct{ observer facade.HTTPObserver }

func (o rowsObservation) Apply(page facade.Page) (facade.Record, error) {
	doc, _ := page.Doc(facade.AnonymousPayload)
	o.observer.HTTPRows(doc)
	return record.NewRecord(map[string]facade.Value{facade.AnonymousPayload: value.NewDocValue(doc)}), nil
}

type responseDecodeDiagnostic struct{ facade.Transform }

func (d responseDecodeDiagnostic) Apply(page facade.Page) (facade.Record, error) {
	result, err := d.Transform.Apply(page)
	if err != nil {
		return nil, fmt.Errorf("error processing response: %w", err)
	}
	return result, nil
}
