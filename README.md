# Otter

<p align="center">
  <img src="logo.png" alt="Otter logo" width="180">
</p>

Local-first macOS CLI for small personal ops tasks: files, summaries, recovery plans, and local model-assisted workflows.

Otter runs on your machine, uses Ollama for planning and summaries, and keeps safety checks in the executor. It can help with common file tasks, but it is intentionally conservative about writes and moves.

## What You Can Do With It

Use Otter from the command line:

```bash
otter "list files in ~/Downloads"
otter "summarize this file CV_Frederic_Camara_2025.pdf"
otter "organize my downloads"
otter "recover file structure in ~/Downloads/audio"
otter "undo last move"
```

Or start chat mode:

```bash
otter chat
```

The useful things today are:

- summarize text files and PDFs when text extraction works
- list and read files inside allowed directories
- move files with dry-run and confirmation behavior
- undo the last successful move batch
- create recovery plans for messy folders
- manage which directories Otter can access
- inspect previous runs through local audit logs
- choose which local Ollama model Otter uses

## Quick Start

1. Install and start Ollama.

```bash
ollama serve
ollama pull qwen2.5-coder:14b
```

2. Set basic environment variables.

```bash
export OTTER_TOKEN="replace-with-a-long-random-token"
export OTTER_HOST="127.0.0.1" # optional
export OTTER_PORT="8080"       # optional
export OTTER_ALLOWED_DIRS="$HOME/Downloads,$HOME/Documents" # optional
export OTTER_MODEL="qwen2.5-coder:14b" # optional
export OTTER_OLLAMA_URL="http://127.0.0.1:11434" # optional
```

If `OTTER_ALLOWED_DIRS` is not set and no directories are saved in config, Otter defaults to the current directory.

3. Build and install the local command.

```bash
make install-user
```

If `otter` is not found, add `~/.local/bin` to your shell path:

```bash
export PATH="$HOME/.local/bin:$PATH"
```

For zsh:

```bash
grep -qxF 'export PATH="$HOME/.local/bin:$PATH"' ~/.zshrc || echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.zshrc
source ~/.zshrc
```

4. Try a first command.

```bash
otter "list files in ~/Downloads"
```

5. Try chat mode.

```bash
otter chat
```

## Common Usage Examples

### Organize Files

```bash
otter "organize my downloads"
otter "organize my music files in ~/Downloads/audio"
```

Batch moves start as dry-runs. If the plan looks right, re-run with explicit confirmation:

```bash
otter "organize my downloads confirm=true"
```

### Summarize Files

```bash
otter "summarize ~/notes/today.md"
otter "summarize this file CV_Frederic_Camara_2025.pdf"
otter "summarize files ~/notes/today.md and ~/notes/todo.md"
```

Bare filenames can be resolved from directories Otter already has access to.

### Undo the Last Move

```bash
otter "undo last move"
```

Undo uses the persisted move history. It is meant for the most recent successful move batch.

### Recovery Planning

Recovery mode helps create a plan when a folder structure was organized incorrectly.

```bash
otter "recover file structure in ~/Downloads/audio"
otter "create recovery plan for ~/Downloads/audio"
```

Recovery is dry-run only. It writes these plan files into the recovery root:

```text
recovery_plan.md
recovery_plan.json
```

If you have partial move logs, you can include them in the task:

```bash
otter "create recovery plan for ~/Downloads/audio /old/path/file.wav -> /new/path/file.wav"
```

Log entries are treated as high-confidence hints. Files Otter cannot place confidently are labeled for review.

### Access Control

Otter only works inside allowed directories. You can manage access with natural-language commands:

```bash
otter "give otter access to Desktop and Documents"
otter "allow access to ~/Work and ~/Projects"
otter "access my notes"
otter "what directories can otter access?"
```

Access rules are saved in `~/.config/otter/config.json`, unless `OTTER_CONFIG_FILE` is set. `OTTER_ALLOWED_DIRS` is merged in at runtime.

Note files are read from folders (`.md` and `.txt`). Direct Apple Notes database access is not implemented.

## Chat Mode

Start it with:

```bash
otter chat
```

Chat mode is useful when you want to do several small tasks in a row without retyping `otter "..."` each time. It uses the same planner, tools, safety checks, permissions, and audit logging as normal CLI task mode.

Commands inside chat:

```text
/help    show chat commands
/access  show accessible directories
/undo    undo the last move
/model   show model config and locally available Ollama models
/exit    quit
```

Anything else is treated like a normal Otter task.

## Audit Logs

Every user request gets a local audit bundle. This includes CLI tasks, HTTP `/run` requests, and each chat message.

Audit logs are stored here by default:

```text
~/.config/otter/runs/
```

Each run directory contains useful debugging files such as:

```text
input.txt
metadata.json
planner_request.json
planner_response_raw.txt
planner_response_parsed.json
tool_calls.jsonl
final_output.md
errors.jsonl
```

Use these commands to inspect recent runs:

```bash
otter runs
otter show run latest
otter show run <run_id>
```

Audit logging is best-effort. If writing audit files fails, the task should continue. Logs are local artifacts and should not be committed.

## Safety Behavior

Current tool scope:

- `list_files`
- `read_file`
- `summarize_files`
- `write_file`
- `move_file`

Rules to expect:

- directory access is enforced before tools run
- writes are create-only by default
- overwrites require `overwrite=true` and `confirm=true`
- batch moves return a dry-run first unless `confirm=true`
- move operations do not overwrite existing files
- the last successful move batch can be undone
- recovery planning does not move files
- delete, arbitrary shell commands, and external API calls are not available tools

The model suggests actions. The executor enforces these rules.

## Model Usage

Otter uses Ollama locally. The default planner model is:

```text
qwen2.5-coder:14b
```

Show the current model config:

```bash
otter model
otter model show
```

Set the main planner model in persisted config:

```bash
otter model set qwen3.5:latest
```

Set a chat-specific model:

```bash
otter model set chat llama3.1:8b
```

List locally available Ollama models:

```bash
otter model available
```

Model precedence:

- `OTTER_MODEL` overrides the main configured model at runtime
- `chat_model` in config is used by `otter chat` when set
- otherwise chat falls back to the main model
- Otter does not pull or validate models when you set them

If needed, pull the model yourself:

```bash
ollama pull qwen3.5:latest
```

## Commands Reference

```bash
otter "task"
```

Run one task through the local planner and executor.

```bash
otter chat
```

Start the interactive chat REPL.

```bash
otter serve
```

Start the local HTTP server.

```bash
otter model
otter model show
otter model available
otter model set <model_name>
otter model set chat <model_name>
```

Inspect or update local model configuration.

```bash
otter runs
otter show run latest
otter show run <run_id>
```

Inspect local audit logs.

## HTTP Usage

Start the server:

```bash
export OTTER_TOKEN="replace-with-a-long-random-token"
otter serve
```

Send a task:

```bash
curl -sS -X POST http://127.0.0.1:8080/run \
  -H "Authorization: Bearer $OTTER_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"task":"list files in ~/Downloads"}'
```

Health check:

```bash
curl -sS http://127.0.0.1:8080/healthz
```

Server config:

- `OTTER_TOKEN` is required for `otter serve`
- `OTTER_HOST` defaults to `127.0.0.1`
- `OTTER_PORT` defaults to `8080`
- `PORT` also works and takes precedence over `OTTER_PORT`

## Limitations

- macOS is the main target right now.
- The planner expects a local Ollama server.
- PDF summaries depend on text extraction quality.
- Apple Notes app database access is not implemented.
- Recovery mode creates plans; it does not execute recovery moves yet.
- Chat context is in-memory and intentionally short.
- No database, GUI, cloud sync, or external API integrations are included.

## Tests

Run the test suite:

```bash
make test
```

Equivalent Go command:

```bash
go test ./...
```

## License

MIT. See [LICENSE](LICENSE).
