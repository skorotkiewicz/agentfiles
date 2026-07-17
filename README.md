# at

A symlink-only manager for agent skills, prompts, and extensions. One script,
no build step, no package manager, and it never runs an agent's plugin CLI.
Symlinks are the whole activation mechanism — agents discover enabled assets
by scanning their own directories.

```bash
cp at.py ~/.local/bin/at        # that's it (needs python3 + git)
export PATH="$HOME/.local/bin:$PATH"
```

## Quick start

```bash
at init                                           # creates ~/.at (agents.json + registry.json + sources/)
at agents add skills ~/.pi/skills --agent pi      # register an agent's discovery dir
at add skill find-skills github:vercel-labs/skills skills/find-skills --agent pi
at add skill find-skills github:vercel-labs/skills skills/find-skills   # kind as 1st arg
at enable find-skills --all              # or every compatible agent
at list
at disable find-skills pi
at update                                # git pull everything
at status                               # list + drift check
at doctor
at remove find-skills
```

Configure agents with `at agents add/rm` (writes `agents.json`):

```bash
at agents add prompts ~/.pi/commands --agent pi
at agents rm   skills  ~/.pi/skills   --agent pi
at agents ls
```

`agents.json` lists each agent and where it keeps its `skills` / `prompts` /
`extensions` directories. `at` reads it to know where to place symlinks.

## How it works

```
~/.at/
  agents.json    config: agents -> {skills:[...], prompts:[...], extensions:[...]}
  registry.json  state: sources + items + which agents each item is enabled for
  sources/       git checkouts (manager-owned)
```

`add` retains a local path or Git source and registers one **item** (not
enabled yet). `enable` symlinks the item into the configured directory of one
or more agents; `disable` unlinks it without deleting the source.
`update` pulls Git sources. `remove` unlinks everywhere and drops the source.

- Filesystem only — it never runs `codex plugin`, `claude plugin`, `npm`, etc.
- Mutations take a file lock; JSON writes are atomic.
- `AT_HOME=/tmp/x at init` runs fully isolated.

## Commands

| command | what it does |
|---|---|
| `init` | create `~/.at`, `agents.json`, `registry.json`, `sources/` |
| `agents ls` / `agents list` | view configured agents |
| `agents add <kind> <dir> --agent <name>` | register an agent's discovery dir |
| `agents rm <kind> <dir> --agent <name>` | forget an agent's discovery dir |
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
