package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/chzyer/readline"

	"otter/internal/agent"
	"otter/internal/audit"
	"otter/internal/config"
	"otter/internal/settings"
	"otter/internal/transport"
)

const (
	defaultOllamaURL       = "http://127.0.0.1:11434"
	chatOllamaCheckTimeout = 350 * time.Millisecond
	colorTitle             = "\x1b[1;36m"
	colorWater             = "\x1b[36m"
	colorReset             = "\x1b[0m"
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

		srv := transport.NewServer(cfg, func(task string) string {
			return agent.RunTaskWithMode(task, "http")
		})
		if err := srv.ListenAndServe(); err != nil {
			fmt.Fprintf(os.Stderr, "server error: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if args[0] == "chat" {
		orch, err := agent.NewOrchestratorForMode("chat")
		if err != nil {
			fmt.Fprintf(os.Stderr, "orchestrator init error: %v\n", err)
			os.Exit(1)
		}
		if err := runChatREPL(os.Stdin, os.Stdout, func(task string) string { return orch.RunWithMode(task, "chat") }); err != nil {
			fmt.Fprintf(os.Stderr, "chat error: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if args[0] == "runs" {
		if err := handleRunsCommand(os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "runs error: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if args[0] == "show" && len(args) >= 2 && args[1] == "run" {
		selector := "latest"
		if len(args) >= 3 {
			selector = args[2]
		}
		if err := handleShowRunCommand(selector, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "show run error: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if args[0] == "model" {
		if err := handleModelCommand(args[1:], os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "model error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	task := strings.TrimSpace(strings.Join(args, " "))
	if task == "" {
		fmt.Fprintln(os.Stderr, "task cannot be empty")
		os.Exit(2)
	}

	fmt.Println(agent.RunTaskWithMode(task, "cli"))
}

func printUsage() {
	bin := filepath.Base(os.Args[0])
	if bin == "" {
		bin = "otter"
	}
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintf(os.Stderr, "  %s \"task\"\n", bin)
	fmt.Fprintf(os.Stderr, "  %s serve\n", bin)
	fmt.Fprintf(os.Stderr, "  %s chat\n", bin)
	fmt.Fprintf(os.Stderr, "  %s model\n", bin)
	fmt.Fprintf(os.Stderr, "  %s model set <model_name>\n", bin)
	fmt.Fprintf(os.Stderr, "  %s model set chat <model_name>\n", bin)
	fmt.Fprintf(os.Stderr, "  %s model show\n", bin)
	fmt.Fprintf(os.Stderr, "  %s runs\n", bin)
	fmt.Fprintf(os.Stderr, "  %s show run <id|latest>\n", bin)
}

type chatTurn struct {
	user      string
	assistant string
}

type chatEditor interface {
	Readline() (string, error)
	Close() error
}

type scannerEditor struct {
	scanner *bufio.Scanner
}

func (s *scannerEditor) Readline() (string, error) {
	if !s.scanner.Scan() {
		return "", io.EOF
	}
	return s.scanner.Text(), nil
}

func (s *scannerEditor) Close() error { return nil }

func runChatREPL(in io.Reader, out io.Writer, run func(string) string) error {
	renderChatHeader(out)
	fmt.Fprintln(out, "Ollama: not checked yet")
	if status := quickOllamaStatus(strings.TrimSpace(os.Getenv("OTTER_OLLAMA_URL")), chatOllamaCheckTimeout); status != "available" {
		fmt.Fprintln(out, "Ollama: unavailable")
	}

	editor, err := newChatEditor(in, out)
	if err != nil {
		return err
	}
	defer editor.Close()
	return runChatLoop(editor, out, run)
}

func runChatLoop(editor chatEditor, out io.Writer, run func(string) string) error {
	history := make([]chatTurn, 0, 5)
	for {
		line, err := editor.Readline()
		if errors.Is(err, readline.ErrInterrupt) {
			fmt.Fprintln(out, "bye")
			return nil
		}
		if errors.Is(err, io.EOF) {
			fmt.Fprintln(out, "bye")
			return nil
		}
		if err != nil {
			return err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if isTerminalEscapeInput(line) {
			continue
		}
		if strings.HasPrefix(strings.ToLower(line), "/model") {
			reply := handleChatModelCommand(line)
			fmt.Fprintln(out, reply)
			history = appendChatTurn(history, chatTurn{user: line, assistant: reply})
			continue
		}

		switch strings.ToLower(line) {
		case "/exit":
			fmt.Fprintln(out, "bye")
			return nil
		case "/help":
			fmt.Fprintln(out, "Commands: /help, /access, /undo, /model, /exit")
			fmt.Fprintln(out, "Any other message runs through the same planner/executor pipeline.")
			continue
		case "/undo":
			reply := run("undo last move")
			fmt.Fprintln(out, reply)
			history = appendChatTurn(history, chatTurn{user: "/undo", assistant: reply})
			continue
		case "/access":
			reply := run("what directories can otter access?")
			fmt.Fprintln(out, reply)
			history = appendChatTurn(history, chatTurn{user: "/access", assistant: reply})
			continue
		}

		task := line
		if shouldInjectContext(line) && len(history) > 0 {
			task = line + "\n\nConversation context:\n" + renderChatContext(history)
		}
		reply := run(task)
		fmt.Fprintln(out, reply)
		history = appendChatTurn(history, chatTurn{user: line, assistant: reply})
	}
}

func newChatEditor(in io.Reader, out io.Writer) (chatEditor, error) {
	configPath, err := settings.ConfigPath()
	if err != nil {
		return nil, err
	}
	historyPath := filepath.Join(filepath.Dir(configPath), "chat_history.txt")
	_ = os.MkdirAll(filepath.Dir(historyPath), 0o755)

	if stdinFile, ok := in.(*os.File); ok {
		if stdoutFile, outOK := out.(*os.File); outOK {
			rl, err := readline.NewEx(&readline.Config{
				Prompt:          "> ",
				Stdin:           stdinFile,
				Stdout:          stdoutFile,
				HistoryFile:     historyPath,
				InterruptPrompt: "^C",
				EOFPrompt:       "exit",
			})
			if err == nil {
				return rl, nil
			}
		}
	}

	return &scannerEditor{scanner: bufio.NewScanner(in)}, nil
}

func renderChatHeader(out io.Writer) {
	title := "🦦 Otter — local-first ops"
	waterFinal := "  ~~~~~~~~"
	if chatColorsEnabled(out) {
		fmt.Fprintf(out, "%s%s%s\n", colorTitle, title, colorReset)
	} else {
		fmt.Fprintln(out, title)
	}
	fmt.Fprintln(out, "Hello, I’m Otter. What can I help you with?")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "   /\\_/\\")
	fmt.Fprintln(out, "  ( o.o )/")
	fmt.Fprintln(out, "   > ^ <")
	renderWaterLine(out, waterFinal)
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Try:")
	fmt.Fprintln(out, "- summarize files")
	fmt.Fprintln(out, "- organize or recover files")
	fmt.Fprintln(out, "- create safe plans")
	fmt.Fprintln(out, "- inspect recent runs")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Type /help for commands • /exit to quit")
}

func renderWaterLine(out io.Writer, final string) {
	colored := wrapWaterColor(out, final)
	if !shouldAnimateWater(out) {
		fmt.Fprintln(out, colored)
		return
	}
	frames := []string{
		"  ~~~~~~~~",
		"   ~~~~~~~~",
		"  ~  ~  ~~~",
		"   ~~ ~~ ~~",
		"  ~~~~~~~~",
	}
	for _, frame := range frames {
		fmt.Fprintf(out, "\r%s", wrapWaterColor(out, frame))
		time.Sleep(140 * time.Millisecond)
	}
	fmt.Fprintf(out, "\r%s\n", colored)
}

func wrapWaterColor(out io.Writer, line string) string {
	if !chatColorsEnabled(out) {
		return line
	}
	return colorWater + line + colorReset
}

func chatColorsEnabled(out io.Writer) bool {
	if strings.TrimSpace(os.Getenv("NO_COLOR")) != "" {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("TERM")), "dumb") {
		return false
	}
	return isTTYWriter(out)
}

func shouldAnimateWater(out io.Writer) bool {
	// Water animation is interactive-only and skipped for NO_COLOR/TERM=dumb or non-TTY output.
	return chatColorsEnabled(out) && isTTYWriter(out)
}

func isTTYWriter(out io.Writer) bool {
	file, ok := out.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}

func isTerminalEscapeInput(line string) bool {
	return strings.HasPrefix(line, "\x1b[")
}

func quickOllamaStatus(rawURL string, timeout time.Duration) string {
	client := &http.Client{Timeout: timeout}
	return quickOllamaStatusWithClient(rawURL, client)
}

func quickOllamaStatusWithClient(rawURL string, client *http.Client) string {
	names, err := listOllamaModelNamesWithClient(rawURL, client)
	if err != nil || len(names) == 0 {
		return "unavailable"
	}
	return "available"
}

func listOllamaModelNames(rawURL string, timeout time.Duration) ([]string, error) {
	client := &http.Client{Timeout: timeout}
	return listOllamaModelNamesWithClient(rawURL, client)
}

func listOllamaModelNamesWithClient(rawURL string, client *http.Client) ([]string, error) {
	baseURL := strings.TrimSpace(rawURL)
	if baseURL == "" {
		baseURL = defaultOllamaURL
	}
	baseURL = strings.TrimRight(baseURL, "/")

	req, err := http.NewRequest(http.MethodGet, baseURL+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("unexpected ollama status: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	var payload struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}

	names := make([]string, 0, len(payload.Models))
	seen := make(map[string]struct{}, len(payload.Models))
	for _, model := range payload.Models {
		name := strings.TrimSpace(model.Name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func shouldInjectContext(input string) bool {
	lower := strings.ToLower(input)
	phrases := []string{"that", "it", "same", "again", "them", "those", "previous", "last one"}
	for _, phrase := range phrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

func renderChatContext(history []chatTurn) string {
	builder := strings.Builder{}
	for _, turn := range history {
		builder.WriteString("User: ")
		builder.WriteString(turn.user)
		builder.WriteString("\n")
		builder.WriteString("Otter: ")
		builder.WriteString(turn.assistant)
		builder.WriteString("\n")
	}
	return strings.TrimSpace(builder.String())
}

func appendChatTurn(history []chatTurn, turn chatTurn) []chatTurn {
	history = append(history, turn)
	if len(history) > 5 {
		history = history[len(history)-5:]
	}
	return history
}

func handleModelCommand(args []string, out io.Writer) error {
	if len(args) == 0 || (len(args) == 1 && args[0] == "show") {
		cfg, err := settings.Load()
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		name, source := agent.ResolvePlannerModelName(cfg, os.Getenv("OTTER_MODEL"))
		chatName, chatSource := agent.ResolveChatModelName(cfg, os.Getenv("OTTER_MODEL"))
		fmt.Fprintf(out, "Main model: %s\n", name)
		fmt.Fprintf(out, "Main source: %s\n", source)
		fmt.Fprintf(out, "Chat model: %s\n", chatName)
		fmt.Fprintf(out, "Chat source: %s\n", chatSource)
		return nil
	}
	if len(args) == 1 && args[0] == "available" {
		models, err := listOllamaModelNames(strings.TrimSpace(os.Getenv("OTTER_OLLAMA_URL")), 800*time.Millisecond)
		if err != nil {
			return fmt.Errorf("list available models: %w", err)
		}
		if len(models) == 0 {
			fmt.Fprintln(out, "Available models: none found")
			return nil
		}
		fmt.Fprintln(out, "Available models:")
		for _, model := range models {
			fmt.Fprintf(out, "- %s\n", model)
		}
		return nil
	}

	if len(args) >= 2 && args[0] == "set" {
		if len(args) >= 3 && args[1] == "chat" {
			return setChatModel(strings.TrimSpace(strings.Join(args[2:], " ")), out)
		}
		return setModel(strings.TrimSpace(strings.Join(args[1:], " ")), out)
	}

	return fmt.Errorf("usage: otter model show | otter model available | otter model set <model_name> | otter model set chat <model_name>")
}

func setModel(modelName string, out io.Writer) error {
	if modelName == "" {
		return fmt.Errorf("model name is required")
	}

	cfg, err := settings.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	cfg.Model = modelName
	if err := settings.Save(cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	fmt.Fprintf(out, "Saved model in config: %s\n", modelName)
	fmt.Fprintf(out, "If needed, install it locally with: ollama pull %s\n", modelName)
	return nil
}

func setChatModel(modelName string, out io.Writer) error {
	if modelName == "" {
		return fmt.Errorf("chat model name is required")
	}
	cfg, err := settings.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	cfg.ChatModel = modelName
	if err := settings.Save(cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	fmt.Fprintf(out, "Saved chat model in config: %s\n", modelName)
	fmt.Fprintf(out, "If needed, install it locally with: ollama pull %s\n", modelName)
	return nil
}

func handleChatModelCommand(line string) string {
	trimmed := strings.TrimSpace(line)
	fields := strings.Fields(strings.TrimPrefix(trimmed, "/"))
	if len(fields) == 0 || strings.ToLower(fields[0]) != "model" {
		return "Model command error: usage /model, /model available, /model set <model_name>"
	}

	var output strings.Builder
	args := fields[1:]
	if len(args) == 0 {
		if err := handleModelCommand(nil, &output); err != nil {
			return "Model command error: " + err.Error()
		}
		models, err := listOllamaModelNames(strings.TrimSpace(os.Getenv("OTTER_OLLAMA_URL")), 800*time.Millisecond)
		if err != nil {
			output.WriteString("Available models: unavailable\n")
			output.WriteString("Tip: ensure Ollama is running (`ollama serve`) and reachable.")
			return strings.TrimSpace(output.String())
		}
		if len(models) == 0 {
			output.WriteString("Available models: none found")
			return strings.TrimSpace(output.String())
		}
		output.WriteString("Available models:\n")
		for _, model := range models {
			output.WriteString("- " + model + "\n")
		}
		return strings.TrimSpace(output.String())
	}

	if err := handleModelCommand(args, &output); err != nil {
		return "Model command error: " + err.Error()
	}
	return strings.TrimSpace(output.String())
}

func handleRunsCommand(out io.Writer) error {
	items, err := audit.ListRunSummaries(20)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		fmt.Fprintln(out, "No runs found.")
		return nil
	}
	for _, item := range items {
		fmt.Fprintf(out, "%s  %s  %s\n", item.ID, item.Status, item.Input)
	}
	return nil
}

func handleShowRunCommand(selector string, out io.Writer) error {
	runDir, err := audit.ResolveRunDirectory(selector)
	if err != nil {
		return err
	}

	inputBytes, _ := os.ReadFile(filepath.Join(runDir, "input.txt"))
	metaBytes, _ := os.ReadFile(filepath.Join(runDir, "metadata.json"))
	errorBytes, _ := os.ReadFile(filepath.Join(runDir, "errors.jsonl"))
	finalBytes, _ := os.ReadFile(filepath.Join(runDir, "final_output.md"))

	var meta map[string]any
	_ = json.Unmarshal(metaBytes, &meta)

	fmt.Fprintf(out, "Run: %s\n", runDir)
	fmt.Fprintf(out, "Input: %s\n", strings.TrimSpace(string(inputBytes)))
	if status, ok := meta["status"].(string); ok && strings.TrimSpace(status) != "" {
		fmt.Fprintf(out, "Status: %s\n", status)
	}
	if mode, ok := meta["mode"].(string); ok && strings.TrimSpace(mode) != "" {
		fmt.Fprintf(out, "Mode: %s\n", mode)
	}
	if modelName, ok := meta["model"].(string); ok && strings.TrimSpace(modelName) != "" {
		fmt.Fprintf(out, "Model: %s\n", modelName)
	}

	errorLines := strings.Split(strings.TrimSpace(string(errorBytes)), "\n")
	if len(strings.TrimSpace(string(errorBytes))) > 0 {
		fmt.Fprintln(out, "Errors:")
		count := 0
		for _, line := range errorLines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			fmt.Fprintf(out, "- %s\n", line)
			count++
			if count >= 5 {
				break
			}
		}
	} else {
		fmt.Fprintln(out, "Errors: none")
	}

	finalSnippet := strings.TrimSpace(string(finalBytes))
	if len(finalSnippet) > 500 {
		finalSnippet = finalSnippet[:500] + "...[truncated]"
	}
	if finalSnippet != "" {
		fmt.Fprintf(out, "Final output snippet:\n%s\n", finalSnippet)
	}
	fmt.Fprintf(out, "Files: %s\n", runDir)
	return nil
}
