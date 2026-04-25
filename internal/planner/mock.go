package planner

import "context"

type MockPlanner struct {
	Response Response
	Err      error
	PlanFunc func(ctx context.Context, req Request) (Response, error)
}

func (m MockPlanner) Plan(ctx context.Context, req Request) (Response, error) {
	if m.PlanFunc != nil {
		return m.PlanFunc(ctx, req)
	}
	if m.Err != nil {
		return Response{}, m.Err
	}
	return m.Response, nil
}
