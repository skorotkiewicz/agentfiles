# Design notes and research

This document records why `agentfiles` separates portable skills from adapter-specific prompts and extensions.

## What is actually portable

The [Agent Skills specification](https://agentskills.io/specification) defines a directory containing `SKILL.md`, required `name` and `description` frontmatter, and optional `scripts/`, `references/`, and `assets/`. It also specifies naming limits and recommends progressive loading. `agentfiles` validates the portable required fields, preserves every regular file in the package, and never runs scripts while installing.

Prompt and extension formats are not covered by that standard:

- [Codex skills](https://learn.chatgpt.com/docs/build-skills) use the open skill format and document symlink support. Codex also has a native per-skill disable setting, while [custom prompts are deprecated](https://learn.chatgpt.com/docs/custom-prompts).
- [Claude Code skills](https://code.claude.com/docs/en/slash-commands) support `skillOverrides`, but plugin skills have a separate lifecycle. Claude's [plugin reference](https://code.claude.com/docs/en/plugins-reference) exposes native install, enable, disable, update, and uninstall commands.
- [Gemini CLI skills](https://geminicli.com/docs/cli/using-agent-skills/) have native install/link/enable/disable operations and several discovery tiers. [Gemini extensions](https://geminicli.com/docs/extensions/reference/) are copied into a vendor store and have their own lifecycle.
- Vercel's [Skills CLI](https://github.com/vercel-labs/skills) already demonstrates the useful canonical-copy-plus-agent-symlink model and maintains a broad path compatibility table.

The result is an explicit boundary: skills use one portable storage model; prompt files use suffix-aware filesystem adapters; extensions either use a filesystem adapter or delegate to a supported native manager.

## Desired state, retained content, generated views

The model has three layers:

```text
state.json ──selects──> immutable revision snapshots
    │
    └──enabledFor──> generated agent-facing symlinks
```

Disabling removes only the generated view. It does not mutate a downloaded skill, edit its frontmatter, or erase update metadata. This gives every agent the same lifecycle semantics even where vendors use different config files.

Vendor-native skill toggles were deliberately not edited. Safely merging TOML/JSON settings owned by several rapidly changing CLIs would enlarge the blast radius and produce different semantics per agent. A generated-view approach is inspectable with `ls -l`, easy to recover, and works for agents with no native toggle API. Native extensions are different: their managers own dependency caches, manifests, authentication, and persistent data, so delegating is safer than copying those stores.

## Why content-addressed revisions

An update is staged as a new SHA-256-derived revision and only then becomes `current`. Agent links always point to the stable `current` indirection, so updating or rolling back does not require rewriting every agent directory.

This also makes drift checkable. `doctor` hashes each retained revision, verifies `current`, verifies enabled links, and reports manager-looking orphan links. Revision IDs and asset names are validated before they are used as paths.

## Ownership rules

The manager follows four filesystem rules:

1. Never replace a regular file or directory in an agent target.
2. Never replace or remove a symlink unless its resolved target is exactly the expected manager-owned `current` path.
3. Treat target directories that resolve through parent symlinks as the same physical view and retain a shared link until its final consumer is disabled.
4. During migration only, adopt an old symlink when it resolves exactly to the source skill being migrated; retain enough information to restore it if migration fails.

Package-internal symlinks are rejected. This avoids snapshots that escape their source directory or silently depend on mutable external files.

## Atomicity boundary

Filesystem state changes are guarded by a single exclusive lock. State is written to a temporary file, flushed, and atomically renamed. Link creation and `current` switches also use rename-based publication. Completed filesystem changes are reconciled back to the cloned pre-operation state when a later step fails.

There is no honest way to promise the same atomicity across Codex, Claude, and Gemini subprocesses. Native operations are therefore reported as best effort, and errors say when a previous vendor update may already have completed.

## Removal and migration

Permanent deletion is a poor default for a package manager that owns user-authored prompts. `remove` moves library data, revisions, and asset metadata to a timestamped trash directory. Migration similarly retains original directories in migration trash after establishing managed snapshots and links.

Migration is dry-run by default and reads Vercel's `.skill-lock.json` opportunistically. Tracked skills retain a refreshable Git source. Untracked skills are marked as snapshots instead of pretending they can be updated.

## Role of `just`

`just` is the repository task runner, not the lifecycle engine. It gives contributors memorable recipes over `gofmt`, `go vet`, `go test`, the race detector, and version-stamped builds. Keeping state transitions in Go provides structured validation, portable path handling, JSON output, testable rollback, and much clearer failure behavior than shell recipes would.
