package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

const (
	defaultHost = "127.0.0.1"
	defaultPort = "8080"
)

type Config struct {
	Host  string
	Port  string
	Token string
}

func LoadFromEnv() (Config, error) {
	host := firstNonEmpty(os.Getenv("OTTER_HOST"), defaultHost)
	port := firstNonEmpty(os.Getenv("PORT"), os.Getenv("OTTER_PORT"), defaultPort)
	token := strings.TrimSpace(os.Getenv("OTTER_TOKEN"))
	if token == "" {
		return Config{}, errors.New("OTTER_TOKEN must be set")
	}

	return Config{
		Host:  host,
		Port:  port,
		Token: token,
	}, nil
}

func (c Config) Address() string {
	return fmt.Sprintf("%s:%s", c.Host, c.Port)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
