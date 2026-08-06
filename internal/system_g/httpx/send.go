package httpx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/stackql-labs/omnisdk/internal/system_g/buffer"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/record"
	"github.com/stackql-labs/omnisdk/internal/system_g/value"
)

var (
	_ facade.Exchange = &sendExchange{}
)

// sendExchange performs each (already-signed) request record pulled from upstream and emits the
// response. It is the operator form of a send — for graphs wired as explicit pull nodes (see the
// beetle: literalSource → sign → send → sink) — complementing Make, which drives sending inline.
// It carries a FormClass because it has a side effect.
type sendExchange struct {
	id        int64
	emits     []facade.Beta
	receives  []facade.Beta
	formClass facade.FormClass
	readers   int

	upstream facade.Operator
	client   *http.Client
}

// NewSendExchange wires a send node over upstream request records. client may be nil.
func NewSendExchange(
	id int64,
	upstream facade.Operator,
	client *http.Client,
	formClass facade.FormClass,
	readers int,
) facade.Exchange {
	if client == nil {
		client = http.DefaultClient
	}
	return &sendExchange{
		id:        id,
		upstream:  upstream,
		client:    client,
		formClass: formClass,
		readers:   readers,
		emits:     []facade.Beta{},
		receives:  []facade.Beta{},
	}
}

func (e *sendExchange) AddEmit(b facade.Beta)    { e.emits = append(e.emits, b) }
func (e *sendExchange) AddReceive(b facade.Beta) { e.receives = append(e.receives, b) }
func (e *sendExchange) Emits() []facade.Beta     { return e.emits }
func (e *sendExchange) Receives() []facade.Beta  { return e.receives }

func (e *sendExchange) getReaderCount() int {
	if e.readers < 1 {
		return 1
	}
	return e.readers
}

func (e *sendExchange) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write([]byte(fmt.Sprintf("Send Exchange id = %d\n", e.id)))
	return int64(n), err
}

func (e *sendExchange) Open(ctx context.Context) facade.Records {
	buf := buffer.NewBuffer(e.getReaderCount(), 1024, 0)
	in := e.upstream.Open(ctx)
	go func() {
		var cerr error
		defer func() { buf.Complete(cerr) }()
		defer in.Close()
		for in.Next(ctx) {
			status, body, err := e.send(ctx, in.Record())
			if err != nil {
				cerr = err
				return
			}
			if err := buf.Append(ctx, newResponseRecord(status, body)); err != nil {
				if !errors.Is(err, buffer.ErrAllReadersClosed) {
					cerr = err
				}
				return
			}
		}
		cerr = in.Err()
	}()
	return buf.Reader()
}

// send performs one already-signed request record; no signing happens here.
func (e *sendExchange) send(ctx context.Context, rec facade.Record) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, Method(rec), URL(rec), bytes.NewReader(ReqBody(rec)))
	if err != nil {
		return 0, nil, err
	}
	for name, vals := range Header(rec) {
		for _, v := range vals {
			req.Header.Add(name, v)
		}
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, payload, nil
}

// newResponseRecord packages a status + body into the standard response record shape (status
// under KeyStatus, body under facade.AnonymousPayload).
func newResponseRecord(status int, body []byte) facade.Record {
	return record.NewRecord(map[string]facade.Value{
		KeyStatus:               value.NewBytesValue([]byte(strconv.Itoa(status))),
		facade.AnonymousPayload: value.NewBytesValue(body),
	})
}
