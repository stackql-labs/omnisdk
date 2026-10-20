package stream

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/stackql-labs/omnisdk/pkg/encode"
)

// object writes one row as a JSON object, columns in declared order and each cell by encode.JSON,
// so an object column stays a nested object.
func object(b *bytes.Buffer, columns []string, cells []any) error {
	b.WriteByte('{')
	for i, c := range columns {
		if i > 0 {
			b.WriteByte(',')
		}
		k, err := encode.JSON(c)
		if err != nil {
			return err
		}
		v, err := encode.JSON(cells[i])
		if err != nil {
			return fmt.Errorf("column %q: %w", c, err)
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return nil
}

// NewJSONL is a sink writing one JSON object per line — the framing a Kafka JSON topic or a
// Datadog log intake takes, one row to a message.
func NewJSONL(out io.Writer, columns []string, opts Options) (Writer, error) {
	return newSink(out, columns, opts, &jsonl{columns: columns})
}

type jsonl struct{ columns []string }

func (f *jsonl) batch(out io.Writer, rows [][]any) error {
	var b bytes.Buffer
	for _, r := range rows {
		if err := object(&b, f.columns, r); err != nil {
			return err
		}
		b.WriteByte('\n')
	}
	_, err := out.Write(b.Bytes())
	return err
}

func (f *jsonl) end(io.Writer) error { return nil }

// NewDelimited is a sink writing a header line, then one line per row, fields separated by delimiter
// — ',' for csv, '\t' for tsv — and quoted per RFC 4180 where a field needs it. Each cell is
// encode.Text; a null is an empty field.
func NewDelimited(out io.Writer, columns []string, delimiter rune, opts Options) (Writer, error) {
	if !validDelimiter(delimiter) {
		return nil, fmt.Errorf("stream: %q cannot delimit fields", delimiter)
	}
	return newSink(out, columns, opts, &delimited{columns: columns, delimiter: delimiter})
}

func validDelimiter(r rune) bool {
	return r != 0 && r != '"' && r != '\r' && r != '\n' && r != utf8.RuneError && utf8.ValidRune(r)
}

type delimited struct {
	columns   []string
	delimiter rune
	headed    bool
}

func (f *delimited) writer(out io.Writer) *csv.Writer {
	w := csv.NewWriter(out)
	w.Comma = f.delimiter
	return w
}

func (f *delimited) header(w *csv.Writer) error {
	if f.headed {
		return nil
	}
	f.headed = true
	return w.Write(f.columns)
}

func (f *delimited) batch(out io.Writer, rows [][]any) error {
	w := f.writer(out)
	if err := f.header(w); err != nil {
		return err
	}
	fields := make([]string, len(f.columns))
	for _, r := range rows {
		for i, v := range r {
			fields[i], _ = encode.Text(v)
		}
		if err := w.Write(fields); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

// end heads an empty relation, so even no rows is a well-formed file.
func (f *delimited) end(out io.Writer) error {
	if f.headed {
		return nil
	}
	w := f.writer(out)
	if err := f.header(w); err != nil {
		return err
	}
	w.Flush()
	return w.Error()
}

// OTel is the OTel log sink's own settings.
type OTel struct {
	// Scope is the instrumentation scope the records are reported under.
	Scope string
	// Now stamps each record's observed time. Nil leaves it unset, for the collector to stamp.
	Now func() time.Time
}

// NewOTelLogs is a sink writing OTLP/JSON: each batch one ExportLogsServiceRequest on its own line,
// the form the collector's file receiver and an OTLP/HTTP JSON endpoint take. Each row is one log
// record: its body the row as a JSON object, its attributes one per column, typed — an integer
// that fits int64 an intValue, other numbers doubleValue, objects and arrays kvlist and array values.
func NewOTelLogs(out io.Writer, columns []string, opts Options, otel OTel) (Writer, error) {
	return newSink(out, columns, opts, &otelLogs{columns: columns, otel: otel})
}

type otelLogs struct {
	columns []string
	otel    OTel
}

func (f *otelLogs) batch(out io.Writer, rows [][]any) error {
	var b bytes.Buffer
	b.WriteString(`{"resourceLogs":[{"resource":{},"scopeLogs":[{"scope":{"name":`)
	if err := jsonString(&b, f.otel.Scope); err != nil {
		return err
	}
	b.WriteString(`},"logRecords":[`)
	for i, r := range rows {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('{')
		if f.otel.Now != nil {
			fmt.Fprintf(&b, `"observedTimeUnixNano":"%d",`, f.otel.Now().UnixNano())
		}
		var body bytes.Buffer
		if err := object(&body, f.columns, r); err != nil {
			return err
		}
		b.WriteString(`"body":{"stringValue":`)
		if err := jsonString(&b, body.String()); err != nil {
			return err
		}
		b.WriteString(`},"attributes":[`)
		for j, c := range f.columns {
			if j > 0 {
				b.WriteByte(',')
			}
			if err := keyValue(&b, c, r[j]); err != nil {
				return err
			}
		}
		b.WriteString(`]}`)
	}
	b.WriteString("]}]}]}\n")
	_, err := out.Write(b.Bytes())
	return err
}

func (f *otelLogs) end(io.Writer) error { return nil }

func jsonString(b *bytes.Buffer, s string) error {
	raw, err := encode.JSON(s)
	b.Write(raw)
	return err
}

func keyValue(b *bytes.Buffer, k string, v any) error {
	b.WriteString(`{"key":`)
	if err := jsonString(b, k); err != nil {
		return err
	}
	b.WriteString(`,"value":`)
	if err := anyValue(b, v); err != nil {
		return err
	}
	b.WriteByte('}')
	return nil
}

// anyValue writes v as an OTLP AnyValue. A null is the empty AnyValue.
func anyValue(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString(`{}`)
	case string:
		b.WriteString(`{"stringValue":`)
		if err := jsonString(b, x); err != nil {
			return err
		}
		b.WriteByte('}')
	case bool:
		fmt.Fprintf(b, `{"boolValue":%t}`, x)
	case map[string]any:
		b.WriteString(`{"kvlistValue":{"values":[`)
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := keyValue(b, k, x[k]); err != nil {
				return err
			}
		}
		b.WriteString(`]}}`)
	case []any:
		b.WriteString(`{"arrayValue":{"values":[`)
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := anyValue(b, e); err != nil {
				return err
			}
		}
		b.WriteString(`]}}`)
	default:
		// A number, or anything else encode renders: read back as JSON it is one of the cases above
		// or a number, and a number is an intValue where it fits int64.
		raw, err := encode.JSON(x)
		if err != nil {
			return err
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var back any
		if err := dec.Decode(&back); err != nil {
			return err
		}
		n, ok := back.(json.Number)
		if !ok {
			return anyValue(b, back)
		}
		if i, ok := encode.AsInt64(n); ok {
			// OTLP/JSON writes a 64-bit integer as a decimal string.
			fmt.Fprintf(b, `{"intValue":"%s"}`, strconv.FormatInt(i, 10))
			return nil
		}
		fmt.Fprintf(b, `{"doubleValue":%s}`, n)
	}
	return nil
}

// NewTable is a sink writing a text table, each cell encode.Text and each column as wide as its
// widest rendered cell. The sink holds a batch, not the relation, so each batch is its own table,
// sized to its own rows; a null is written null.
func NewTable(out io.Writer, columns []string, opts Options) (Writer, error) {
	return newSink(out, columns, opts, &table{columns: columns})
}

type table struct{ columns []string }

func (f *table) batch(out io.Writer, rows [][]any) error {
	cells := make([][]string, len(rows))
	width := make([]int, len(f.columns))
	for i, c := range f.columns {
		width[i] = utf8.RuneCountInString(c)
	}
	for r, row := range rows {
		cells[r] = make([]string, len(row))
		for i, v := range row {
			s, ok := encode.Text(v)
			if !ok {
				s = "null"
			}
			cells[r][i] = s
			width[i] = max(width[i], utf8.RuneCountInString(s))
		}
	}
	var b strings.Builder
	rule := func() {
		for _, w := range width {
			b.WriteByte('+')
			b.WriteString(strings.Repeat("-", w+2))
		}
		b.WriteString("+\n")
	}
	line := func(fields []string) {
		for i, s := range fields {
			b.WriteString("| ")
			b.WriteString(s)
			b.WriteString(strings.Repeat(" ", width[i]-utf8.RuneCountInString(s)+1))
		}
		b.WriteString("|\n")
	}
	rule()
	line(f.columns)
	rule()
	for _, r := range cells {
		line(r)
	}
	rule()
	_, err := io.WriteString(out, b.String())
	return err
}

func (f *table) end(io.Writer) error { return nil }
