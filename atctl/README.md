# atctl

A tiny, dependency-free manager for agent **skills**, **prompts**, and **extensions**.

It keeps one canonical copy of every item and only creates or removes filesystem symlinks in agent directories. It never runs agent-specific installers such as `codex plugin add`, `claude plugin install`, or `opencode plugin`.

The executable is named `atctl` because Unix already has an unrelated command named `at`.

## Layout

```text
~/.at/
├── agents.json       # editable agent → kind → target-directory map
├── registry.json     # sources, items, and per-agent enabled state
├── sources/          # managed Git checkouts
└── .lock             # prevents concurrent state edits
```

Local sources may live anywhere, including an existing `~/.at/catalog`, `~/.at/skills`, or a normal Git working tree.

## Install

```bash
just install
atctl init
```

There are no Python packages to install; `atctl.py` uses only the standard library and Git for remote sources.

## Quick start

Install every discovered skill from a GitHub repository and enable it for Pi and Codex:

```bash
atctl install skills badlogic/pi-skills --agent pi --agent codex
```

Temporarily disable one skill for Codex without deleting its source or registration:

```bash
atctl disable browser-tools --agent codex
atctl enable browser-tools --agent codex

# Or disable it everywhere it is currently enabled:
atctl disable browser-tools --all
```

See current state and verify every link:

```bash
atctl status
atctl doctor
```

Update all managed Git sources. Symlinks keep pointing at the same canonical directories:

```bash
atctl update
```

## Existing local collection

Suppose your canonical files already look like this:

```text
~/.at/catalog/
├── skills/shredder/SKILL.md
├── prompts/review.md
└── extensions/context-tree.ts
```

Register them without copying anything:

```bash
atctl source add personal ~/.at/catalog
atctl item add skill shredder \
  --source personal \
  --path skills/shredder \
  --agent pi \
  --agent codex

atctl item add prompt review \
  --source personal \
  --path prompts/review.md \
  --agent pi

atctl item add extension context-tree \
  --source personal \
  --path extensions/context-tree.ts \
  --agent pi
```

For a directory containing many `SKILL.md` files:

```bash
atctl source add personal ~/.at/catalog
atctl scan personal --under skills
atctl install skills ~/.at/catalog skills --source-name personal --agent pi
```

## Agent targets

`atctl init` creates practical defaults for Pi, Codex, Claude Code, and OpenCode. The database is intentionally generic: add any private or future agent by editing `~/.at/agents.json` or using the CLI.

```bash
atctl agent add my-agent skills ~/.my-agent/skills
atctl agent add my-agent prompts ~/.my-agent/prompts
atctl agent add my-agent extensions ~/.my-agent/extensions
atctl agent ls
```

Each kind accepts multiple target directories. Enabling an item creates a link in every configured target for that agent and kind.

## Sources and items

A **source** is either:

- a local directory, which `atctl` never modifies; or
- a Git repository cloned to `~/.at/sources/<name>`.

An **item** points to one file or directory inside a source and has a kind, stable ID, target filename, and enabled-agent list.

```bash
atctl source add my-skills owner/repo --ref main
atctl scan my-skills
atctl item add skill my-skill --source my-skills --path skills/my-skill
atctl enable skill:my-skill --agent pi
```

GitHub shorthand, `github:owner/repo`, normal Git URLs, SSH URLs, branches, tags, and commits are accepted.

## Safety model

- JSON writes use a temporary file, `fsync`, and atomic rename.
- Mutations take an advisory lock at `~/.at/.lock`.
- Links are relative, so moving a home-directory tree is less fragile.
- Existing normal files, directories, and unrelated symlinks are never overwritten.
- Disable/remove only unlinks a path when it still resolves to the registered canonical item.
- Git is invoked directly with argument arrays, never through a shell.
- Git updates refuse dirty managed checkouts and use detached checkouts of the configured branch/tag/commit.
- `sync` restores missing managed Git checkouts at the recorded revision, then restores declared links; it does not delete unknown files.
- `--dry-run` is available on install, item add/remove, enable/disable, and sync.

## Commands

```text
atctl init
atctl agent  add|rm|ls
atctl source add|rm|ls
atctl scan SOURCE [--under PATH]
atctl install [KIND] SOURCE [PATH] [--agent AGENT] [--skill NAME]
atctl item   add|rm|ls
atctl enable ITEM... (--agent AGENT | --all)
atctl disable ITEM... (--agent AGENT | --all)
atctl update [SOURCE...]
atctl sync [--dry-run]
atctl status
atctl doctor
```

Item selectors may be a full ID such as `skill:shredder` or a unique short name such as `shredder`.

## Why this shape

The important package-manager properties are deliberately separated:

1. `agents.json` is editable configuration: where each agent discovers each kind of artifact.
2. `registry.json` is persistent state: source provenance, checked-out revision, item paths, and enablement.
3. Canonical storage is separate from activation.
4. Activation is declarative and reversible rather than destructive.
5. A fresh machine can restore managed Git checkouts and links with `atctl sync`; local sources must still exist at their recorded paths.

Vercel's `skills` CLI also recommends a canonical copy with agent-facing symlinks, but its lifecycle centers on add/remove/update. `atctl` adds the missing reversible enabled/disabled layer and generalizes it to prompts and extensions.

## Upstream references

- Vercel Skills CLI: <https://github.com/vercel-labs/skills>
- Agent Skills specification: <https://agentskills.io/>
- Codex skill locations: <https://developers.openai.com/codex/build-skills>
- Claude Code skills: <https://code.claude.com/docs/en/skills>
- OpenCode skills: <https://opencode.ai/docs/skills/>
- OpenCode commands: <https://opencode.ai/docs/commands/>
- OpenCode plugins: <https://opencode.ai/docs/plugins/>
- XDG Base Directory specification: <https://specifications.freedesktop.org/basedir/>

## Install a source subpath

```bash
atctl install skills https://github.com/vercel-labs/skills/ skills/find-skills --agent pi
```

```bash
atctl install prompts owner/repo prompts --agent pi
atctl install extensions ~/.asas/something --agent pi
```

`KIND` accepts `skills`, `prompts`, or `extensions`. `PATH` may select one file or a directory inside the source. For compatibility, omitting `KIND` still performs a skill install. `--under` remains available as the equivalent path flag.
