# at — skill / prompt / extension shop

A tiny, filesystem-only manager for agent skills, prompts and extensions.
The "shop" lives at `~/.at` and is the center for everything; each agent gets
skills enabled by a plain `ln -s` into its own dir, and disabled by `unlink`.
It **never** runs agent plugin CLIs (no `codex plugin add`).

## Layout

```
~/.at/
  registry.json   # database: what is installed + per-agent enabled map + source
  agents.json     # structure: which agents exist + their skills/prompts/extensions dirs
  sources/<slug>/ # canonical source (git-cloned or local)
  link/<agent>/   # symlinks for the convenience "default" agent (optional)
```

`~/.agents` is **never written to** — only symlinks land there, and only because
you point an agent at a `~/.agents/...` dir in `agents.json`.

## State files

`registry.json` (source of truth):

```json
{
  "version": 1,
  "items": {
    "find-skills": {
      "type": "skill",
      "source": { "kind": "git", "url": "https://github.com/vercel-labs/skills", "path": "~/.at/sources/find-skills" },
      "enabled": { "default": true, "nano": true }
    }
  }
}
```

`agents.json` (you edit this — add your own agents):

```json
{
  "agents": {
    "default": { "skills": "~/.at/link/default/skills", "prompts": "~/.at/link/default/prompts", "extensions": "~/.at/link/default/extensions" },
    "nano":    { "skills": "~/Dev/Rust/nano-prototype/at/skills" },
    "codex":   { "skills": "~/.agents/codex/skills", "prompts": "~/.agents/codex/prompts" }
  }
}
```

The shop reads `agents.json` to know **where to link/unlink** for each agent.

## Commands

`add` installs a skill's source into `~/.at/sources/<slug>` and registers it in
`registry.json` — but does **not** link it. Linking is an explicit `enable` step,
so a freshly added skill stays unlinked until you enable it for an agent.

```sh
just init                          # create ~/.at/registry.json + agents.json
just add <git-url|path> [--type skill|prompt|extension] [--name slug]   # install source, not linked
just enable  <slug> [agent]        # ln -s for that agent
just disable <slug> [agent]        # unlink for that agent (source kept)
just remove  <slug>                # purge everywhere (unlink + drop source)
just remove  <slug> --agent X      # disable for one agent only
just update  [slug]               # git pull sources, re-link enabled
just list    [agent|all]
just status                        # show registry vs filesystem drift
just sync                          # force filesystem to match registry
just adopt                         # register already-linked assets non-destructively
just doctor                        # validate both JSON files
```

## Why symlinks (best practice)

- **Immutable source, mutable links** — a skill lives once in `sources/<slug>`;
  enabling for an agent is just a symlink. Disable = `unlink`, never delete source.
- **Per-agent, declarative** — the same skill can be enabled for `nano` but
  disabled for `codex`; `registry.json` records the truth, `sync` reconciles.
- **Agent-agnostic** — agents discover skills by scanning their dir for symlinks.
  No plugin-CLI coupling, works for any agent that reads a skills folder.
- **Portable updates** — `update` does `git pull` + re-link; the JSON keeps the
  repo URL so re-cloning/moving is trivial.
