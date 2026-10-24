package omnisdk

import (
	"context"
	"fmt"
	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/buffer"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
	"time"
)

// NewSleep plans a cancellable delay as an ordinary exchange. It performs no I/O.
func NewSleep(duration time.Duration) (Plan, error) {
	if duration <= 0 {
		return nil, fmt.Errorf("sleep duration must be positive")
	}
	spec := plan.NewExchangeSpec("sleep", nil, nil, func(map[string]any) facade.Operator {
		return sleepOperator{duration: duration}
	}, nil)
	return &cannedPlan{plan: plan.NewPlan([]plan.ExchangeSpec{spec}, nil, nil, nil, nil, nil)}, nil
}

type sleepOperator struct{ duration time.Duration }

func (s sleepOperator) Open(ctx context.Context) facade.Records {
	buf := buffer.NewBuffer(1, 2, 0)
	go func() {
		timer := time.NewTimer(s.duration)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			buf.Complete(ctx.Err())
		case <-timer.C:
			err := buf.Append(ctx, bind.NewDocRecord(map[string]any{}))
			buf.Complete(err)
		}
	}()
	return buf.Reader()
}
