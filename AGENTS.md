# Project notes

<!-- project-knowledge-harness:agent-guidance -->

- Keep future work in [TODO.md](TODO.md), using the P1, P2, P3, P?, Done
  lanes and priority/effort tags. Capture substantial research in
  `backlog/<slug>.md` and link it from the TODO entry.
- Use the installed `project-knowledge-harness` skill's `add-todo.sh` and
  `todo-kanban.sh --validate-only TODO.md` helpers. These scripts belong to
  the skill; they are not bundled in this repository's `scripts/` directory.
- Distinguish implemented behavior, documented upstream capabilities,
  untested assumptions, and proposed interfaces in research notes.
- When an item ships, move it to Done with a date and mark its research
  note `Status: shipped`. Preserve the note as historical context.
- Record resolved, non-obvious debugging traps in `pitfalls/`, titled by
  symptom. Keep design questions in `backlog/` until they are investigated.
- These notes are maintainer metadata outside the MkDocs `docs/` tree.

<!-- project-knowledge-harness:agent-guidance (end) -->
