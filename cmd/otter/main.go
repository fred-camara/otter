package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"otter/internal/agent"
	"otter/internal/config"
	"otter/internal/transport"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		printUsage()
		os.Exit(2)
	}

	if args[0] == "serve" {
		cfg, err := config.LoadFromEnv()
		if err != nil {
			fmt.Fprintf(os.Stderr, "config error: %v\n", err)
			os.Exit(1)
		}

		srv := transport.NewServer(cfg, agent.RunTask)
		if err := srv.ListenAndServe(); err != nil {
			fmt.Fprintf(os.Stderr, "server error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	task := strings.TrimSpace(strings.Join(args, " "))
	if task == "" {
		fmt.Fprintln(os.Stderr, "task cannot be empty")
		os.Exit(2)
	}

	fmt.Println(agent.RunTask(task))
}

func printUsage() {
	bin := filepath.Base(os.Args[0])
	if bin == "" {
		bin = "otter"
	}
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintf(os.Stderr, "  %s \"task\"\n", bin)
	fmt.Fprintf(os.Stderr, "  %s serve\n", bin)
}
