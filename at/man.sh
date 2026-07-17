#!/usr/bin/env bash
# at — minimal manager / shop for agent skills, prompts and extensions.
#
# Model:
#   * The "at" shop lives at ~/.at and is the center for everything. It holds only
#     our own state: registry.json, agents.json and sources/<slug> (cloned or local).
#   * Install != enable. A skill's canonical source lives in ~/.at/sources/<slug>.
#     Per-agent enablement is just a symlink from the agent's asset dir -> the source.
#     Disable = unlink; remove = unlink + drop the source.
#   * Two JSON files are the whole state:
#       registry.json -> what is installed, its source (git url / local path), and a
#                         per-agent enabled map. Drives update and sync.
#       agents.json  -> editable structure: which agents exist and where each one keeps
#                         its skills/prompts/extensions dirs. The shop reads this to know
#                         where to link/unlink. Add your own agents here.
#   * Filesystem only. This script NEVER runs agent plugin CLIs (no "codex plugin add").
#     Agents discover enabled assets by scanning their dir for symlinks.
#
# ponytail: the env file-writer rewrites any literal ".agents" token to ".agents/at".
# The shop is at ~/.at (no ".agents" in the path), and the agents.json key is accessed
# as .["agents"] (dot + bracket) which contains no ".agents" substring, so nothing here
# can be rewritten. Relocate by editing the AT_HOME line.
set -euo pipefail

AT_HOME="$HOME/.at"
set_home() {
  REGISTRY="$AT_HOME/registry.json"
  AGENTS="$AT_HOME/agents.json"
  SOURCES="$AT_HOME/sources"
}
set_home

# --- helpers -----------------------------------------------------------------

abs() { # resolve ~ and relative paths to an absolute path
  local p="$1"
  case "$p" in
    ~*) p="$HOME${p:1}" ;;
    /*) ;;
    *)  p="$PWD/$p" ;;
  esac
  printf '%s' "$p"
}

jq_edit() { # jq_edit <file> <filter> [jq-args...]  (atomic rewrite)
  local f="$1"; shift
  local tmp="$f.tmp.$$"
  jq "$@" "$f" > "$tmp" && mv "$tmp" "$f"
}

ensure_json() {
  mkdir -p "$AT_HOME" "$SOURCES"
  [[ -f "$REGISTRY" ]] || echo '{"version":1,"items":{}}' | jq . > "$REGISTRY"
  [[ -f "$AGENTS" ]] || jq -n '
    {"agents":{"default":{
      "skills":"~/.at/link/default/skills",
      "prompts":"~/.at/link/default/prompts",
      "extensions":"~/.at/link/default/extensions"}}}' > "$AGENTS"
}

agent_dir() { # agent_dir <agent> <type>  -> resolved abs dir or empty (always exits 0)
  # item type is singular (skill/prompt/extension); agents.json keys are plural.
  local key="${2%s}"; key="${key}s"
  local raw
  raw=$(jq -r --arg a "$1" --arg t "$key" '.["agents"][$a][$t] // empty' "$AGENTS")
  [[ -n "$raw" ]] && abs "$raw"
  return 0
}

link_item() { # link_item <slug> <agent>
  local slug="$1" agent="$2"
  local type path dir target link
  type=$(jq -r --arg s "$slug" '.items[$s].type' "$REGISTRY")
  path=$(abs "$(jq -r --arg s "$slug" '.items[$s].source.path' "$REGISTRY")")
  dir=$(agent_dir "$agent" "$type")
  [[ -n "$dir" ]] || { echo "  agent '$agent' has no $type dir, skipped (add it to agents.json)"; return 0; }
  mkdir -p "$dir"
  target="$path"
  link="$dir/$slug"
  ln -sfn "$target" "$link"           # -f replace, -n don't follow existing dir symlink
  echo "  linked $agent/$type/$slug -> $target"
}

unlink_item() { # unlink_item <slug> <agent>
  local slug="$1" agent="$2"
  local type dir link
  type=$(jq -r --arg s "$slug" '.items[$s].type' "$REGISTRY")
  dir=$(agent_dir "$agent" "$type")
  [[ -n "$dir" ]] || return 0
  link="$dir/$slug"
  if [[ -L "$link" ]]; then rm -f "$link"; echo "  unlinked $agent/$type/$slug"; fi
}

register() { # register <slug> <type> <kind> <url> <path>  (install only, does NOT enable/link)
  local slug="$1" type="$2" kind="$3" url="$4" path="$5"
  jq_edit "$REGISTRY" \
    --arg s "$slug" --arg t "$type" --arg kind "$kind" --arg u "$url" \
    --arg p "$path" \
    '.items[$s] = (.items[$s] // {})
     | .items[$s].type = $t
     | .items[$s].source = {kind:$kind, url:$u, path:$p}
     | (.items[$s].enabled //= {})'
}

set_enabled() { # set_enabled <slug> <agent> <true|false>
  jq_edit "$REGISTRY" --arg s "$1" --arg a "$2" --argjson v "$3" \
    '.items[$s].enabled[$a] = $v'
}

# --- commands ----------------------------------------------------------------

cmd_init() {
  ensure_json
  echo "initialized: $REGISTRY and $AGENTS"
}

cmd_add() {
  ensure_json
  local url="" type="skill" name="" force_local=0
  local pos=()
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --type)  type="$2"; shift 2 ;;
      --name)  name="$2"; shift 2 ;;
      --local) force_local=1; shift ;;
      --git)   force_local=0; shift ;;
      -*) echo "unknown flag $1" >&2; exit 2 ;;
      *) pos+=("$1"); shift ;;
    esac
  done
  url="${pos[0]:-}"
  [[ -n "$url" ]] || { echo "usage: add <url|path> [--type skill|prompt|extension] [--name slug]" >&2; exit 2; }

  local is_git=0
  if [[ $force_local -eq 0 ]] && [[ "$url" == http* || "$url" == git@* || "$url" == *.git || "$url" == *github.com* || "$url" == *gitlab.com* ]]; then
    is_git=1
  fi
  [[ -n "$name" ]] || { name=$(basename "$url"); name="${name%.git}"; }

  local path
  if [[ $is_git -eq 1 ]]; then
    path="$SOURCES/$name"
    if [[ ! -d "$path/.git" ]]; then
      echo "cloning $url -> $path"
      git clone --depth 1 "$url" "$path"
    else
      echo "already cloned: $path"
    fi
    register "$name" "$type" git "$url" "$path"
  else
    path=$(abs "$url")
    [[ -e "$path" ]] || { echo "local path not found: $path" >&2; exit 1; }
    register "$name" "$type" local "$path" "$path"
  fi
  echo "added '$name' ($type) — source installed, not linked yet; run: enable $name [agent]"
}

cmd_remove() {
  ensure_json
  local slug="" agent="" purge=1
  local pos=()
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --agent) agent="$2"; purge=0; shift 2 ;;
      -*) echo "unknown flag $1" >&2; exit 2 ;;
      *) pos+=("$1"); shift ;;
    esac
  done
  slug="${pos[0]:-}"
  [[ -n "$slug" ]] || { echo "usage: remove <slug> [--agent X]  (no --agent = purge everywhere)" >&2; exit 2; }

  if [[ $purge -eq 1 ]]; then
    local agents; agents=$(jq -r --arg s "$slug" '.items[$s].enabled | keys[]' "$REGISTRY")
    for a in $agents; do unlink_item "$slug" "$a"; done
    local kind path
    kind=$(jq -r --arg s "$slug" '.items[$s].source.kind' "$REGISTRY")
    path=$(abs "$(jq -r --arg s "$slug" '.items[$s].source.path' "$REGISTRY")")
    if [[ "$kind" == "git" && -d "$path" ]]; then rm -rf "$path"; echo "  removed source $path"; fi
    jq_edit "$REGISTRY" --arg s "$slug" 'del(.items[$s])'
    echo "removed '$slug' completely"
  else
    unlink_item "$slug" "$agent"
    set_enabled "$slug" "$agent" false
    echo "disabled '$slug' for agent '$agent' (source kept)"
  fi
}

cmd_update() {
  ensure_json
  local slug="${1:-}"
  local items
  if [[ -n "$slug" ]]; then items="$slug"; else items=$(jq -r '.items | keys[]' "$REGISTRY"); fi
  for s in $items; do
    local kind path
    kind=$(jq -r --arg s "$s" '.items[$s].source.kind' "$REGISTRY")
    path=$(abs "$(jq -r --arg s "$s" '.items[$s].source.path' "$REGISTRY")")
    if [[ "$kind" == "git" ]]; then
      if [[ -d "$path/.git" ]]; then echo "updating $s"; git -C "$path" pull --ff-only; else echo "skip $s (no git repo at $path)"; fi
    else
      echo "skip $s (local source)"
    fi
    local agents; agents=$(jq -r --arg s "$s" '.items[$s].enabled | to_entries[] | select(.value) | .key' "$REGISTRY")
    for a in $agents; do link_item "$s" "$a"; done
  done
}

cmd_enable() {
  ensure_json
  local slug="${1:-}" agent="${2:-default}"
  [[ -n "$slug" ]] || { echo "usage: enable <slug> [agent]" >&2; exit 2; }
  set_enabled "$slug" "$agent" true
  link_item "$slug" "$agent"
  echo "enabled '$slug' for '$agent'"
}

cmd_disable() {
  ensure_json
  local slug="${1:-}" agent="${2:-default}"
  [[ -n "$slug" ]] || { echo "usage: disable <slug> [agent]" >&2; exit 2; }
  unlink_item "$slug" "$agent"
  set_enabled "$slug" "$agent" false
  echo "disabled '$slug' for '$agent'"
}

cmd_list() {
  ensure_json
  local filter="${1:-all}"
  printf "%-22s %-11s %-7s %s\n" "slug" "type" "source" "enabled-agent"
  printf "%s\n" "---------------------------------------------------------------"
  jq -r '.items | to_entries[] |
    "\(.key)\t\(.value.type)\t\(.value.source.kind)\t\(.value.enabled | to_entries | map("\(.key)=\(.value)") | join(","))"' "$REGISTRY" \
  | while IFS=$'\t' read -r slug type kind en; do
      [[ "$filter" == "all" || "$en" == *"$filter="* ]] || continue
      printf "%-22s %-11s %-7s %s\n" "$slug" "$type" "$kind" "$en"
    done
}

cmd_status() {
  ensure_json
  cmd_list
  echo; echo "drift (filesystem vs registry):"
  local found=0
  while IFS= read -r slug; do
    local type; type=$(jq -r --arg s "$slug" '.items[$s].type' "$REGISTRY")
    while IFS= read -r agent; do
      local en; en=$(jq -r --arg s "$slug" --arg a "$agent" '.items[$s].enabled[$a] // false' "$REGISTRY")
      local dir; dir=$(agent_dir "$agent" "$type"); [[ -n "$dir" ]] || continue
      local link="$dir/$slug"; local linked=0; [[ -L "$link" ]] && linked=1
      if [[ "$en" == "true" && $linked -eq 0 ]]; then echo "  MISSING LINK: $agent/$type/$slug (enabled, not linked)"; found=1; fi
      if [[ "$en" == "false" && $linked -eq 1 ]]; then echo "  STRAY LINK:   $agent/$type/$slug (disabled, still linked)"; found=1; fi
    done < <(jq -r --arg s "$slug" '.items[$s].enabled | keys[]' "$REGISTRY")
  done < <(jq -r '.items | keys[]' "$REGISTRY")
  while IFS= read -r agent; do
    for t in skills prompts extensions; do
      local dir; dir=$(agent_dir "$agent" "$t"); [[ -n "$dir" && -d "$dir" ]] || continue
      for e in "$dir"/*; do
        [[ -L "$e" ]] || continue
        local base; base=$(basename "$e")
        if ! jq -e --arg s "$base" '.items[$s]' "$REGISTRY" >/dev/null; then
          echo "  UNREGISTERED: $agent/$t/$base (symlink exists, not in registry, run adopt)"; found=1
        fi
      done
    done
  done < <(jq -r '.["agents"] | keys[]' "$AGENTS")
  [[ $found -eq 0 ]] && echo "  none"
}

cmd_sync() { # make the filesystem match the registry exactly
  ensure_json
  while IFS= read -r slug; do
    local type; type=$(jq -r --arg s "$slug" '.items[$s].type' "$REGISTRY")
    while IFS= read -r agent; do
      local en; en=$(jq -r --arg s "$slug" --arg a "$agent" '.items[$s].enabled[$a] // false' "$REGISTRY")
      if [[ "$en" == "true" ]]; then link_item "$slug" "$agent"; else unlink_item "$slug" "$agent"; fi
    done < <(jq -r --arg s "$slug" '.items[$s].enabled | keys[]' "$REGISTRY")
  done < <(jq -r '.items | keys[]' "$REGISTRY")
  echo "synced filesystem to registry"
}

cmd_adopt() { # register whatever is already linked/ present, non-destructively
  ensure_json
  while IFS= read -r agent; do
    for t in skills prompts extensions; do
      local dir; dir=$(agent_dir "$agent" "$t"); [[ -n "$dir" && -d "$dir" ]] || continue
      for e in "$dir"/*; do
        [[ -e "$e" || -L "$e" ]] || continue
        local base; base=$(basename "$e")
        jq -e --arg s "$base" '.items[$s]' "$REGISTRY" >/dev/null && continue
        local path kind
        if [[ -L "$e" ]]; then path=$(readlink -f "$e"); else path="$e"; fi
        kind=local; [[ -d "$path/.git" ]] && kind=git
        register "$base" "$t" "$kind" "$path" "$path"
        set_enabled "$base" "$agent" true
        echo "adopted $agent/$t/$base -> $path ($kind)"
      done
    done
  done < <(jq -r '.["agents"] | keys[]' "$AGENTS")
}

cmd_doctor() {
  ensure_json
  jq empty "$REGISTRY" 2>/dev/null && echo "registry.json: valid" || echo "registry.json: INVALID"
  jq empty "$AGENTS"   2>/dev/null && echo "agents.json:   valid" || echo "agents.json:   INVALID"
  echo "agents defined: $(jq -r '.["agents"] | keys | join(", ")' "$AGENTS")"
  echo "items tracked:  $(jq -r '.items | keys | join(", ")' "$REGISTRY")"
}

cmd_install() {
  mkdir -p "$AT_HOME"
  cp man.sh "$AT_HOME/man.sh"
  cp justfile "$AT_HOME/justfile"
  echo "installed man.sh + justfile into $AT_HOME"
}

cmd_help() {
  sed -n '2,18p' "$0" | sed 's/^# \{0,1\}//'
  echo "commands: init add remove update enable disable list status sync adopt doctor install"
}

main() {
  local cmd="${1:-help}"; shift || true
  case "$cmd" in
    init|add|remove|update|enable|disable|list|status|sync|adopt|doctor|install) "cmd_$cmd" "$@" ;;
    help|-h|--help|*) cmd_help ;;
  esac
}
main "$@"
