# agentfiles

`agentfiles` is a local-first manager for agent skills, prompts, and extensions. It keeps one desired-state database and content-addressed library under `~/.agents/agentfiles`, then exposes only the enabled assets to each agent.

The useful distinction is:

- **install** retains an asset in the central library;
- **enable** creates an agent-facing view;
- **disable** removes that view without losing the asset or its history;
- **remove** disables it everywhere and moves managed data to recoverable trash.

This is an early preview. It is intentionally a small, dependency-free Go CLI rather than another package registry.

## Build

Go 1.23 or newer is required. [`just`](https://just.systems/) is optional but convenient:

```bash
just check
just build
./dist/agentfiles help
```

Install from this checkout with either:

```bash
just install
# or
go install ./cmd/agentfiles
```

## Quick start

Install one skill from a Git repository and enable it for Claude Code and Codex:

```bash
agentfiles add vercel-labs/skills \
  --select find-skills \
  --agent claude \
  --agent codex
```

Temporarily disable it only for Codex, then enable it again:

```bash
agentfiles disable find-skills --agent codex
agentfiles enable find-skills --agent codex
```

Update from the tracked source, inspect state, and roll back if needed:

```bash
agentfiles update find-skills --dry-run
agentfiles update find-skills
agentfiles list
agentfiles rollback find-skills
```

Flags may appear before or after positional arguments. `--agent` is repeatable and also accepts comma-separated names. `claude`, `gemini`, and `copilot` are aliases for their canonical profile names.

## Commands

| Command | Purpose |
| --- | --- |
| `add` / `install` | Validate and snapshot a local or Git source, then optionally enable it |
| `enable` / `on` | Expose retained assets to selected agents |
| `disable` / `off` | Remove selected agent views; with no `--agent`, disable everywhere |
| `update` / `upgrade` | Refresh selected assets, or all assets when none are named |
| `rollback` | Switch a filesystem asset to a retained revision |
| `remove` / `rm` | Disable, uninstall native instances, and move managed data to trash |
| `list` / `ls` | Show desired state; supports `--kind`, `--agent`, and `--json` |
| `status` | Show desired state plus drift findings |
| `agents` | Show built-in/custom profiles, detection, and target paths |
| `doctor` | Check links, current pointers, revision hashes, conflicts, and lock state |
| `migrate skills` | Adopt an existing skills directory; dry-run unless `--apply` is present |

All mutating lifecycle commands support `--dry-run` except migration, where dry-run is already the default. Machine-readable commands support `--json`.

## Skills

The default `add` kind is `skill`. Sources may be local paths, Git URLs, or GitHub `owner/repo` shorthand.

```bash
# One local skill
agentfiles add ./my-skill --agent cursor

# Select several skills from a repository
agentfiles add owner/repo \
  --select code-review \
  --select release-notes \
  --agent claude-code

# Install every discovered skill, but do not enable any yet
agentfiles add owner/repo --all
```

`SKILL.md` frontmatter is checked against the Agent Skills naming and description rules. Package scripts are copied but never run during installation. Package symlinks and unsupported file types are rejected.

## Prompts

Prompts are single files because prompt formats and filename suffixes are agent-specific:

```bash
agentfiles add prompt ./prompts/review.md \
  --name review \
  --agent claude-code
```

Codex Markdown prompts can still be managed:

```bash
agentfiles add prompt ./prompts/review.md --agent codex
```

The CLI warns because Codex custom prompts are deprecated in favor of skills. Gemini command files must be `.toml`; a Markdown prompt cannot be enabled for that adapter without converting it first.

## Extensions and plugins

Extension formats are not portable. `agentfiles` therefore has two modes:

1. **Native** delegates install/enable/disable/update/uninstall to Codex, Claude Code, or Gemini CLI.
2. **Filesystem** snapshots a file or directory and manages an agent-facing link, currently useful for OpenCode plugins and custom profiles.

Examples:

```bash
# Claude Code marketplace plugin
agentfiles add extension formatter@your-marketplace \
  --native --name formatter --agent claude

# Gemini extension from Git, pinned to a ref
agentfiles add extension https://github.com/example/workspace-extension \
  --native --name workspace-extension --ref v2 --agent gemini

# Filesystem OpenCode plugin
agentfiles add extension ./plugins/audit.ts \
  --filesystem --name audit --agent opencode
```

Codex plugin identifiers should be `plugin@marketplace`; targeted updates need the marketplace portion. Codex has no native disabled-plugin state, so disable maps to `codex plugin remove` and enable maps to `codex plugin add`.

Native managers retain ownership of their caches, dependency data, and prompts. Their operations may be interactive and cannot be made fully transactional across several vendors.

## Migrating an existing `~/.agents/skills`

Migration understands Vercel Skills CLI's `.skill-lock.json` when available and retains its Git source metadata. It also safely replaces old per-agent symlinks that point at the originals.

Preview first:

```bash
agentfiles migrate skills \
  --from ~/.agents/skills \
  --agent universal \
  --agent codex \
  --agent claude-code
```

Then apply the same plan:

```bash
agentfiles migrate skills \
  --from ~/.agents/skills \
  --agent universal \
  --agent codex \
  --agent claude-code \
  --apply
```

Include `universal` when tools should continue discovering the official shared `~/.agents/skills` view. Original directories move to timestamped migration trash only after snapshots are installed. Foreign links and unmanaged target files are refused rather than overwritten.

Skills without lock metadata become immutable `snapshot` sources. They can be enabled, disabled, removed, and rolled back, but `update` asks you to add them again from a refreshable local or Git source.

## Storage model

By default:

```text
~/.agents/agentfiles/
├── state.json
├── config.json                 # optional custom agent profiles
├── library/<kind>/<name>/current
├── revisions/<kind>/<name>/<sha256-prefix>/
├── trash/<timestamp>/
└── .lock                       # present only during a mutation
```

Set `AGENTFILES_HOME` to use another absolute manager root. `HOME`, `XDG_CONFIG_HOME`, `CODEX_HOME`, and `CLAUDE_CONFIG_DIR` are honored when resolving adapters.

New manager directories are private to the current user (`0700`) and `state.json` is written as `0600`.

Agent-facing entries are generated links, not canonical data. `agentfiles` removes only links that point to the exact managed asset; it refuses regular files, directories, and foreign symlinks. If multiple profiles resolve to the same physical directory, the link is retained until the last profile is disabled.

## Built-in adapters

| Profile | Skills | Prompts | Extensions |
| --- | --- | --- | --- |
| `codex` | `~/.codex/skills` | `~/.codex/prompts/*.md` | native Codex plugins |
| `claude-code` | `~/.claude/skills` | `~/.claude/commands/*.md` | native Claude plugins |
| `cursor` | `~/.cursor/skills` | — | — |
| `gemini-cli` | `~/.gemini/skills` | `~/.gemini/commands/*.toml` | native Gemini extensions |
| `opencode` | `${XDG_CONFIG_HOME}/opencode/skills` | `commands/*.md` | filesystem plugin files |
| `github-copilot` | `~/.copilot/skills` | — | — |
| `universal` | `~/.agents/skills` | — | — |

Codex officially documents `~/.agents/skills` as its shared user location. The isolated `codex` profile uses the compatibility global path also used by Vercel's Skills CLI; choose `universal` when you want the documented shared path.

## Custom agents

Add profiles in `~/.agents/agentfiles/config.json`:

```json
{
  "agents": {
    "roo": {
      "displayName": "Roo Code",
      "skillsDir": "~/.roo/skills"
    },
    "my-agent": {
      "displayName": "My Agent",
      "executable": "my-agent",
      "skillsDir": "~/.my-agent/skills",
      "promptsDir": "~/.my-agent/commands",
      "promptSuffix": ".md",
      "extensionsDir": "~/.my-agent/plugins",
      "extensionShape": "file"
    }
  }
}
```

Custom paths may use `~`, `$HOME`, or `${HOME}`. `extensionShape` is `file` or `directory`.

## Safety and recovery

- Desired state is replaced atomically under an exclusive operation lock.
- Filesystem revisions are immutable, content-addressed snapshots.
- Multi-link mutations roll back completed link changes on failure.
- Updates validate every filesystem source before switching revisions.
- `remove` moves manager-owned content to timestamped trash instead of deleting it permanently.
- `doctor` verifies revision content hashes and reports drift without repairing anything.
- Git repositories are cloned, but install never executes bundled code.

Native extension actions are best effort because an external CLI owns the actual state. If one vendor command fails after another completed, inspect with that vendor's list command and run `agentfiles doctor`.

Some agents load changes immediately; others require a new session, `/reload-plugins`, or restart.

## Why a CLI and a Justfile?

The `Justfile` is useful for repository recipes: formatting, tests, vetting, building, installation, and running the tool. Lifecycle management itself needs validated state, locks, rollback, JSON output, and safe link ownership checks, so it belongs in the CLI rather than a growing collection of shell recipes.

Useful development recipes:

```bash
just --list
just fmt
just vet
just test
just test-race
just check
just build 0.1.0
just run agents
```

See [docs/design.md](docs/design.md) for the research and design tradeoffs.
