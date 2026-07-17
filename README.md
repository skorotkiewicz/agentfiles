# agentfiles

A symlink-only manager for agent skills, prompts, and extensions. One script,
no build step, no package manager, and it never runs an agent's plugin CLI.
Symlinks are the whole activation mechanism — agents discover enabled assets
by scanning their own directories.

```bash
cp agentfiles.py ~/.local/bin/agentfiles        # that's it (needs python3 + git)
export PATH="$HOME/.local/bin:$PATH"
```

## Quick start

```bash
cp agents.example.json ~/.agentfiles/agents.json   # edit by hand; agentfiles never writes it
agentfiles init                                   # creates ~/.agentfiles (registry.json + sources/)
agentfiles add find-skills github:vercel-labs/skills skills/find-skills --agent pi
agentfiles add skill find-skills github:vercel-labs/skills skills/find-skills   # kind as 1st arg
agentfiles enable find-skills --all              # or every compatible agent
agentfiles list
agentfiles disable find-skills pi
agentfiles update                                # git pull everything
agentfiles status                                # list + drift check
agentfiles doctor
agentfiles remove find-skills
```

Configure agents by editing `~/.agentfiles/agents.json` directly (start from
`agents.example.json`). `agentfiles` **never writes** `agents.json` — it only
reads it to know where each agent discovers skills/prompts/extensions.
`agentfiles agents ls` views the current mapping (read-only).

## How it works

```
~/.agentfiles/
  agents.json    editable config: agents -> {skills:[...], prompts:[...], extensions:[...]}
  registry.json  state: sources + items + which agents each item is enabled for
  sources/       git checkouts (manager-owned)
```

`add` retains a local path or Git source and registers one **item** (not
enabled yet). `enable` symlinks the item into the configured directory of one
or more agents; `disable` unlinks it without deleting the source.
`update` pulls Git sources. `remove` unlinks everywhere and drops the source.

- Filesystem only — it never runs `codex plugin`, `claude plugin`, `npm`, etc.
- Enable refuses to overwrite an existing non-symlink path; disable only
  removes symlinks that point at the managed source.
- Mutations take a file lock; JSON writes are atomic.
- `AGENTFILES_HOME=/tmp/x agentfiles init` runs fully isolated.

## Commands

| command | what it does |
|---|---|
| `init` | create `~/.agentfiles`, `registry.json`, `sources/` |
| `agents ls` / `agents list` | view configured agents (read-only; `agentfiles` never writes `agents.json`) |
| `add [<kind>] <slug> <src> [subpath \| --subpath PATH] [--ref] [--agent ...]` | register a source + item, opt. enable |
| `scan <slug> [--under DIR]` | list `SKILL.md` directories under an item |
| `enable <slug> [agent...] [--agent ...] [--all]` | symlink for selected agents |
| `disable <slug> [agent...] [--agent ...] [--all]` | unlink for selected agents |
| `update [source...]` | fetch + check out latest Git sources |
| `remove <slug>` | unlink everywhere, drop the source |
| `list` | show registered items |
| `status` | items + filesystem drift |
| `sync` | make symlinks match the registry exactly |
| `doctor` | validate sources, items, enabled links |

`--type` accepts `skill/prompt/extension` and common aliases
(`skills`, `command`, `plugin`, ...). Git sources may be `owner/repo`,
`github:owner/repo`, a full URL, or `user@host:path`.
