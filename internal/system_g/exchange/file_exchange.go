package exchange

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/stackql-labs/omnisdk/internal/system_g/buffer"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/record"
	"github.com/stackql-labs/omnisdk/internal/system_g/value"
)

var (
	_ facade.Exchange = &fileExchange{}
)

type fileExchange struct {
	id          int64
	emits       []facade.Beta
	receives    []facade.Beta
	defaultPath string
	flags       string
	formClass   facade.FormClass
}

func NewFileExchange(
	id int64,
	defaultPath string,
	flags string,
	formClass facade.FormClass,
) facade.Exchange {
	return &fileExchange{
		id:          id,
		defaultPath: defaultPath,
		flags:       flags,
		formClass:   formClass,
		emits:       []facade.Beta{},
		receives:    []facade.Beta{},
	}
}

func (e *fileExchange) AddEmit(b facade.Beta) {
	e.emits = append(e.emits, b)
}

func (e *fileExchange) AddReceive(b facade.Beta) {
	e.receives = append(e.receives, b)
}

func (e *fileExchange) Receives() []facade.Beta {
	return e.receives
}

func (e *fileExchange) getReaderCount() int {
	if len(e.receives) == 0 {
		return 1
	}
	return len(e.receives)
}

func (e *fileExchange) Emits() []facade.Beta {
	return e.emits
}

func (e *fileExchange) WriteTo(w io.Writer) (int64, error) {
	var total int64
	n, err := w.Write([]byte(fmt.Sprintf("File Exchange id = %d\n", e.id)))
	total += int64(n)
	if err != nil {
		return total, err
	}
	for _, b := range e.emits {
		n, err := b.WriteTo(w)
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func (e *fileExchange) Open(ctx context.Context) facade.Records {
	buf := buffer.NewBuffer(e.getReaderCount(), 1024, 0)
	go func() {
		var cerr error
		defer func() { buf.Complete(cerr) }()
		f, err := os.Open(e.defaultPath)
		if err != nil {
			cerr = err
			return
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			rec := record.NewRecord(map[string]facade.Value{
				facade.AnonymousPayload: value.NewBytesValue([]byte(sc.Text())),
			})
			if err := buf.Append(ctx, rec); err != nil {
				if !errors.Is(err, buffer.ErrAllReadersClosed) {
					cerr = err // real failure; all-readers-closed is a clean early stop
				}
				return
			}
		}
		cerr = sc.Err()
	}()
	return buf.Reader()
}
