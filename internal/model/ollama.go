package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const defaultBaseURL = "http://127.0.0.1:11434"

type Ollama struct {
	baseURL    string
	model      string
	httpClient *http.Client
}

type generateRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
	Format string `json:"format,omitempty"`
}

type generateResponse struct {
	Response string `json:"response"`
}

func NewOllama(modelName, baseURL string) *Ollama {
	normalizedBaseURL := strings.TrimSpace(baseURL)
	if normalizedBaseURL == "" {
		normalizedBaseURL = defaultBaseURL
	}

	return &Ollama{
		baseURL: strings.TrimRight(normalizedBaseURL, "/"),
		model:   strings.TrimSpace(modelName),
		httpClient: &http.Client{
			Timeout: 90 * time.Second,
		},
	}
}

func (o *Ollama) Generate(prompt string) (string, error) {
	body, err := json.Marshal(generateRequest{
		Model:  o.model,
		Prompt: prompt,
		Stream: false,
		Format: "json",
	})
	if err != nil {
		return "", fmt.Errorf("marshal generate request: %w", err)
	}

	request, err := http.NewRequest(http.MethodPost, o.baseURL+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build generate request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := o.httpClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("call ollama: %w", err)
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("read ollama response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ollama returned status %d: %s", response.StatusCode, strings.TrimSpace(string(responseBody)))
	}

	var parsed generateResponse
	if err := json.Unmarshal(responseBody, &parsed); err != nil {
		return "", fmt.Errorf("parse ollama response: %w", err)
	}

	return strings.TrimSpace(parsed.Response), nil
}
