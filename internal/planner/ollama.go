package planner

import (
	"context"
	"fmt"
	"strings"

	"otter/internal/model"
)

type OllamaPlanner struct {
	model model.Interface
}

func NewOllamaPlanner(model model.Interface) *OllamaPlanner {
	return &OllamaPlanner{model: model}
}

func (p *OllamaPlanner) Plan(_ context.Context, req Request) (Response, error) {
	prompt := buildPrompt(req.Task, req.Tools)
	output, err := p.model.Generate(prompt)
	if err != nil {
		return Response{}, err
	}
	return Response{RawJSON: output}, nil
}

func buildPrompt(task string, toolNames []string) string {
	return fmt.Sprintf(`You are Otter, a local system agent.
Return valid JSON only with this exact schema:
{"tool":"tool_name","input":{}}
If task cannot be completed safely, return:
{"error":"reason"}

Rules:
- Use only these tools: %s
- Never delete files
- Never use shell commands
- Never use network or external APIs
- Prefer safe read/list/summarize/write/move
- No markdown, no explanations
- Do not include any text before or after the JSON object

User task: %s`, strings.Join(toolNames, ", "), task)
}
