package omnisdk

import "context"

type emptyPlan struct{}

func (emptyPlan) Open(ctx context.Context) (Rows, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return emptyRows{}, nil
}

type emptyRows struct{}

func (emptyRows) Next() bool   { return false }
func (emptyRows) Row() Row     { return nil }
func (emptyRows) Err() error   { return nil }
func (emptyRows) Close() error { return nil }
