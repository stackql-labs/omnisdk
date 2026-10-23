package docx

import (
	"context"
	"errors"
	"fmt"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
	"net/http"
	"strings"
)

func authenticationTarget(target string, inputs map[string]any) string {
	for key, value := range inputs {
		target = strings.ReplaceAll(target, "{"+key+"}", fmt.Sprint(value))
	}
	return target
}

type authenticationSpec struct {
	plan.ExchangeSpec
	method, url string
}

func (s authenticationSpec) Make(bound map[string]any) facade.Operator {
	return authenticationOp{Operator: s.ExchangeSpec.Make(bound), method: s.method, url: s.url}
}

type authenticationOp struct {
	facade.Operator
	method, url string
}

func (o authenticationOp) Open(ctx context.Context) facade.Records {
	return authenticationRows{Records: o.Operator.Open(ctx), method: o.method, url: o.url}
}

type authenticationRows struct {
	facade.Records
	method, url string
}

func (r authenticationRows) Err() error {
	err := r.Records.Err()
	var response facade.HTTPStatusError
	if errors.As(err, &response) {
		return authenticationError{cause: err, response: response, method: r.method, url: r.url}
	}
	return err
}

type authenticationError struct {
	cause       error
	response    facade.HTTPStatusError
	method, url string
}

func (e authenticationError) Error() string {
	method := strings.ToLower(e.method)
	if len(method) > 0 {
		method = strings.ToUpper(method[:1]) + method[1:]
	}
	return fmt.Sprintf("%s %q: oauth2: cannot fetch token: %d %s\nResponse: %s", method, e.url, e.response.StatusCode(), strings.ToUpper(http.StatusText(e.response.StatusCode())), e.response.ResponseBody())
}
func (e authenticationError) Unwrap() error               { return e.cause }
func (e authenticationError) AuthenticationFailure() bool { return true }

func (s authenticationSpec) Flatten() facade.Transform {
	return authenticationFlatten{Transform: s.ExchangeSpec.Flatten(), method: s.method, url: s.url}
}

type authenticationFlatten struct {
	facade.Transform
	method, url string
}

func (t authenticationFlatten) Apply(page facade.Page) (facade.Record, error) {
	result, err := t.Transform.Apply(page)
	var response facade.HTTPStatusError
	if errors.As(err, &response) {
		return nil, authenticationError{cause: err, response: response, method: t.method, url: t.url}
	}
	return result, err
}
