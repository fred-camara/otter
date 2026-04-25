package model

type Interface interface {
	Generate(prompt string) (string, error)
}
