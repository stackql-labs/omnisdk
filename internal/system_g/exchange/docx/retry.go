package docx

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/retry"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
)

func validateRetry(settings aot.HTTPRetry) error {
	if settings.Attempts() < 1 {
		return fmt.Errorf("docx: retry max_attempts must be positive")
	}
	if settings.InitialDelay() < 0 || settings.MaxDelay() < 0 || settings.Multiplier() < 0 {
		return fmt.Errorf("docx: retry delays and multiplier must be non-negative")
	}
	switch settings.Algorithm() {
	case "", "exponential", "constant":
	default:
		return fmt.Errorf("docx: unsupported retry algorithm %q", settings.Algorithm())
	}
	for _, status := range settings.Statuses() {
		if status < 100 || status > 599 {
			return fmt.Errorf("docx: invalid retry status %d", status)
		}
	}
	return nil
}

type documentBackoff struct{ settings aot.HTTPRetry }

func (b documentBackoff) Delay(attempt facade.Attempt) time.Duration {
	base, cap := b.settings.InitialDelay(), b.settings.MaxDelay()
	if base <= 0 {
		base = 250 * time.Millisecond
	}
	if cap <= 0 {
		cap = 30 * time.Second
	}
	factor := b.settings.Multiplier()
	if factor == 0 {
		factor = 2
	}
	delay := float64(base)
	if b.settings.Algorithm() != "constant" {
		delay *= math.Pow(factor, float64(attempt.Index))
	}
	if delay >= float64(cap) {
		return cap
	}
	return time.Duration(delay)
}

type documentRetryOp struct {
	facade.Operator
	settings aot.HTTPRetry
	method   string
}

func withRetrySettings(op facade.Operator, settings aot.HTTPRetry, method string) facade.Operator {
	if settings == nil {
		return op
	}
	return documentRetryOp{Operator: op, settings: settings, method: method}
}
func (o documentRetryOp) Open(ctx context.Context) facade.Records {
	methods := o.settings.Methods()
	allowed := len(methods) == 0
	for _, method := range methods {
		if strings.EqualFold(method, o.method) {
			allowed = true
		}
	}
	attempts := o.settings.Attempts()
	if !allowed {
		attempts = 1
	}
	var classifier retry.Classifier
	if statuses := o.settings.Statuses(); len(statuses) > 0 {
		classifier = retry.NewStatusClassifier(statuses...)
	}
	policy := retry.Override(retry.From(ctx), retry.Config{Tries: attempts, Classifier: classifier, Backoff: documentBackoff{o.settings}})
	return o.Operator.Open(retry.WithPolicy(ctx, policy))
}
