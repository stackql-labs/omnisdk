package encoder

import (
	"encoding/json"
	"io"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/value"
)

type anonymousEncoder struct {
}

func NewAnonymousEncoder() facade.Encoder {
	return &anonymousEncoder{}
}

func (e *anonymousEncoder) Encode(w io.Writer, r facade.Record) (int64, error) {
	// Write the anonymous payload to the writer.
	v := r.Get(facade.AnonymousPayload)
	if v == nil {
		return 0, nil
	}
	return io.Copy(w, v.Reader())
}

// jsonlEncoder renders the record's agnostic document as JSON Lines: one line per element
// when the document is a list, otherwise a single line. A non-agnostic payload is copied
// through as one raw line.
type jsonlEncoder struct{}

func NewJSONLEncoder() facade.Encoder {
	return jsonlEncoder{}
}

func (jsonlEncoder) Encode(w io.Writer, r facade.Record) (int64, error) {
	v := r.Get(facade.AnonymousPayload)
	if v == nil {
		return 0, nil
	}
	doc, ok := value.Doc(v)
	if !ok {
		n, err := io.Copy(w, v.Reader())
		if err != nil {
			return n, err
		}
		m, err := io.WriteString(w, "\n")
		return n + int64(m), err
	}

	rows, isList := doc.([]any)
	if !isList {
		rows = []any{doc}
	}
	var total int64
	for _, row := range rows {
		b, err := json.Marshal(row)
		if err != nil {
			return total, err
		}
		b = append(b, '\n')
		n, err := w.Write(b)
		total += int64(n)
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
