# Changelog

## 2026-04-25

- Bootstrapped local-first `otter` CLI + HTTP server with bearer auth.
- Added Ollama-backed planner integration and safe tool execution loop.
- Implemented safe tools: `list_files`, `read_file`, `summarize_files`.
- Added permission gating, tests, and install flow for `otter` command.
- Added path expansion support for `~` and `$HOME` in tool inputs.
- Added natural-language directory access management with persisted config (`~/.config/otter/config.json`).
- Improved notes intent handling (`access my notes`, latest-notes scanning fallback, and clearer Apple Notes guidance).
