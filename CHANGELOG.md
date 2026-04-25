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
