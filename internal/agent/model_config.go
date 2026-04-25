package agent

import (
	"strings"

	"otter/internal/settings"
)

const DefaultPlannerModelName = defaultModelName

func ResolvePlannerModelName(cfg settings.Config, envModel string) (name string, source string) {
	if value := strings.TrimSpace(envModel); value != "" {
		return value, "environment variable OTTER_MODEL"
	}
	if value := strings.TrimSpace(cfg.Model); value != "" {
		return value, "config"
	}
	return DefaultPlannerModelName, "default"
}

func ResolveChatModelName(cfg settings.Config, envModel string) (name string, source string) {
	if value := strings.TrimSpace(cfg.ChatModel); value != "" {
		return value, "config chat_model"
	}
	return ResolvePlannerModelName(cfg, envModel)
}
