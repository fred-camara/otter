# Changelog

## 2026-04-25

- Bootstrapped local-first `otter` CLI + HTTP server with bearer auth.
- Added Ollama-backed planner integration and safe tool execution loop.
- Implemented safe tools: `list_files`, `read_file`, `summarize_files`.
- Added permission gating, tests, and install flow for `otter` command.
- Added path expansion support for `~` and `$HOME` in tool inputs.
- Added natural-language directory access management with persisted config (`~/.config/otter/config.json`).
- Improved notes intent handling (`access my notes`, latest-notes scanning fallback, and clearer Apple Notes guidance).
- Added safe `write_file` tool (create-only default, explicit confirm required for overwrite).
- Added safe `move_file` tool (no overwrite, batch dry-run with explicit confirm to execute).
- Added composed CLI flow to read multiple files and write a synthesized output file.
- Added tests for permission failures, invalid paths, overwrite protection, and successful writes/moves.
- Added undo support for the last successful move batch.
- Improved organizer planning to preserve uniform subfolder structure when files already fit the target category.
- Added planner abstraction package (`internal/planner`) with context-based request/response interface and Ollama wrapper.
- Added mock planner support for tests without network/model dependencies.
- Added Recovery Mode dry-run planning (`recover ...`) with deterministic `recovery_plan.md` and `recovery_plan.json` outputs.
- Added optional log-assisted recovery signals (`/old/path -> /new/path`) with high-confidence reconstruction priority.
- Added minimal interactive REPL via `otter chat` with `/exit`, `/help`, `/undo`, and `/access` commands and in-memory history.
