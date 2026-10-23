package omnisdk_test

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
	"github.com/stackql-labs/omnisdk/pkg/query"
)

type completionFunc func(context.Context, string, string, map[string]any, omnisdk.Row) (omnisdk.Row, error)

func (f completionFunc) Complete(ctx context.Context, address, verb string, inputs map[string]any, row omnisdk.Row) (omnisdk.Row, error) {
	return f(ctx, address, verb, inputs, row)
}

func TestMutationCompletionRunsBeforeReturningWithoutRepeatingEffect(t *testing.T) {
	q := mustMutation(t, query.NewInsert(users("t"), query.NewAssignment("UserName", query.NewLiteral("carol"))), nil, []query.Predicate{regionEq()}, []query.Output{query.NewOutput("UserName", query.NewColumn("t", "UserName"))})
	var completed atomic.Int32
	rows, made, err := tryQueryWith(t, q, func(args *omnisdk.Args) {
		args.MutationCompletion = completionFunc(func(ctx context.Context, address, verb string, inputs map[string]any, row omnisdk.Row) (omnisdk.Row, error) {
			completed.Add(1)
			if ctx.Err() != nil || verb != "insert" || row["UserName"] != "carol" || inputs["UserName"] != "carol" {
				t.Errorf("completion input: address=%s verb=%s inputs=%v row=%v", address, verb, inputs, row)
			}
			return omnisdk.Row{"UserName": "finished"}, nil
		})
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Load() != 1 || !reflect.DeepEqual(rows, []string{"UserName=finished"}) || !reflect.DeepEqual(made, []string{"CreateUser|us-east-1|carol"}) {
		t.Fatalf("completed=%d rows=%v calls=%v", completed.Load(), rows, made)
	}
}

func TestMutationCompletionFailurePreservesUnknownOutcomeAndCause(t *testing.T) {
	sentinel := errors.New("completion read failed")
	q := mustMutation(t, query.NewInsert(users("t"), query.NewAssignment("UserName", query.NewLiteral("carol"))), nil, []query.Predicate{regionEq()}, nil)
	_, made, err := tryQueryWith(t, q, func(args *omnisdk.Args) {
		args.MutationCompletion = completionFunc(func(context.Context, string, string, map[string]any, omnisdk.Row) (omnisdk.Row, error) {
			return nil, sentinel
		})
	}, nil)
	if !errors.Is(err, sentinel) || !errors.Is(err, omnisdk.ErrOutcomeUnknown) || !reflect.DeepEqual(made, []string{"CreateUser|us-east-1|carol"}) {
		t.Fatalf("calls=%v err=%v", made, err)
	}
}
