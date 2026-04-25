package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

type Tool interface {
	Name() string
	Description() string
	Execute(input json.RawMessage) (string, error)
}

type Registry struct {
	tools map[string]Tool
}

func NewRegistry(registered ...Tool) *Registry {
	index := make(map[string]Tool, len(registered))
	for _, tool := range registered {
		index[tool.Name()] = tool
	}
	return &Registry{tools: index}
}

func (r *Registry) Execute(toolName string, input json.RawMessage) (string, error) {
	tool, ok := r.tools[toolName]
	if !ok {
		return "", fmt.Errorf("unknown tool: %s", toolName)
	}
	return tool.Execute(input)
}

func (r *Registry) Names() ([]string, error) {
	if len(r.tools) == 0 {
		return nil, errors.New("no tools registered")
	}

	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}
