# Otter

Local-first personal ops agent scaffold for macOS.

This milestone includes:
- Go project scaffold
- CLI task command: `otter "task"`
- local HTTP endpoint: `POST /run`
- bearer token authentication
- local Ollama planner (`qwen2.5-coder:14b` by default)
- safe tool loop (`list_files`, `read_file`, `summarize_files`)
- permission gate (safe tools only in this milestone)

## Quick start

1. Set environment variables:

```bash
export OTTER_TOKEN="replace-with-a-long-random-token"
export OTTER_HOST="127.0.0.1" # optional
export OTTER_PORT="8080"       # optional
export OTTER_ALLOWED_DIRS="$HOME/Downloads,$HOME/notes" # optional (default: current directory)
export OTTER_MODEL="qwen2.5-coder:14b" # optional
export OTTER_OLLAMA_URL="http://127.0.0.1:11434" # optional
```

2. Build and install local commands:

```bash
make install-user
```

If `otter` is not found, add `~/.local/bin` to your shell PATH:

```bash
export PATH="$HOME/.local/bin:$PATH"
```

To make it persistent for zsh:

```bash
grep -qxF 'export PATH="$HOME/.local/bin:$PATH"' ~/.zshrc || echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.zshrc
source ~/.zshrc
```

3. Start server:

```bash
otter serve
```

4. Call from terminal:

```bash
curl -sS -X POST http://127.0.0.1:8080/run \
  -H "Authorization: Bearer $OTTER_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"task":"organize my downloads"}'
```

5. Use CLI task mode:

```bash
otter "list files in $HOME/Downloads"
otter "summarize files $HOME/notes/today.md and $HOME/notes/todo.md"
otter "give otter access to Desktop and Documents"
otter "what directories can otter access?"
```

`~` and `$HOME` paths are supported in tool inputs.

## Commands

- `otter "task"` sends tasks to the local model planner.
- `otter serve` starts the local HTTP server.

## Directory access configuration

Otter now supports natural-language access management in CLI prompts:

- `otter "give otter access to Desktop"`
- `otter "allow access to ~/Work and ~/Projects"`
- `otter "what directories can otter access?"`
- `otter "help"` for quick usage guidance

Access rules are persisted in `~/.config/otter/config.json` (or `OTTER_CONFIG_FILE`).
If `OTTER_ALLOWED_DIRS` is set, it is merged with persisted access for that run.

## Current safety scope

- Allowed tools: `list_files`, `read_file`, `summarize_files`
- Blocked in this milestone: `write_file`, `move_file`
- Forbidden: delete, arbitrary shell, external API calls

## Tests

```bash
make test
```
