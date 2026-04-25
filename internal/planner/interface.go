package planner

import "context"

type Request struct {
	Task  string
	Tools []string
}

type Response struct {
	RawJSON string
}

type Planner interface {
	Plan(ctx context.Context, req Request) (Response, error)
}
