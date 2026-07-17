#!/usr/bin/env python3
"""agentfiles — a symlink-only manager for agent skills, prompts, and extensions.

One directory (~/.agentfiles by default), two JSON files, and symlinks. No compiled
program, no package manager, and it never runs an agent's plugin CLI. Agents
discover enabled assets by scanning their discovery directory for symlinks.

State (all under AGENTFILES_HOME, override with the AGENTFILES_HOME env var):
  agents.json    editable config: which agents exist and where each keeps its
                skills/prompts/extensions directories.
  registry.json  state: every source (git/local) and item, plus which agents
                each item is enabled for.
  sources/       git checkouts cloned here (manager-owned).
"""
from __future__ import annotations

import argparse
import contextlib
import datetime as dt
import fcntl
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path
from typing import NoReturn
from urllib.parse import urlparse

SCHEMA = 1

# Item "type" is singular (one skill); agents.json keys are plural (a dir holds many).
KINDS = {"skill": "skills", "prompt": "prompts", "extension": "extensions"}
TYPE_ALIASES = {
    "skill": "skill", "skills": "skill",
    "prompt": "prompt", "prompts": "prompt", "command": "prompt", "commands": "prompt",
    "extension": "extension", "extensions": "extension", "plugin": "extension", "plugins": "extension",
}
NAME_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]*$")
SKIP_DIRS = {".git", ".hg", ".svn", "node_modules", "vendor", "dist", "build", "target"}


def now() -> str:
    return dt.datetime.now(dt.timezone.utc).replace(microsecond=0).isoformat()


def die(msg: str) -> NoReturn:
    print(f"error: {msg}", file=sys.stderr)
    sys.exit(1)


def expand(path: str | Path) -> Path:
    return Path(os.path.expandvars(os.path.expanduser(str(path))))


def normalize_relative_path(value: str, label: str = "path") -> str:
    """Return a normalized relative path that cannot escape its root."""
    raw = value.strip()
    if not raw or raw == ".":
        return "."
    path = Path(raw)
    if path.is_absolute() or any(part == ".." for part in path.parts):
        die(f"{label} must stay inside the source: {value!r}")
    normalized = Path(*(part for part in path.parts if part not in ("", ".")))
    return normalized.as_posix() or "."


def ensure_within(root: Path, candidate: Path, label: str) -> Path:
    root = root.resolve()
    candidate = candidate.resolve()
    try:
        candidate.relative_to(root)
    except ValueError:
        die(f"{label} escapes {root}: {candidate}")
    return candidate


def canonical_type(value: str) -> str:
    try:
        return TYPE_ALIASES[value.lower()]
    except KeyError:
        die(f"unknown type {value!r}; use skill, prompt, or extension")


def validate_name(value: str, label: str = "name") -> str:
    if not NAME_RE.fullmatch(value):
        die(f"invalid {label} {value!r}; use letters, digits, dot, underscore, or dash")
    return value


def read_json(path: Path, default) -> dict:
    if not path.exists():
        return json.loads(json.dumps(default))
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        die(f"cannot read {path}: {exc}")
    return data if isinstance(data, dict) else die(f"{path} must be a JSON object")


def write_json(path: Path, data: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    payload = json.dumps(data, indent=2, sort_keys=True) + "\n"
    fd, tmp = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    tmp = Path(tmp)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            fh.write(payload)
            fh.flush()
            os.fsync(fh.fileno())
        os.replace(tmp, path)
        dir_fd = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(dir_fd)
        finally:
            os.close(dir_fd)
    finally:
        tmp.unlink(missing_ok=True)


class Store:
    def __init__(self, root: Path):
        self.root = root
        self.agents_path = root / "agents.json"
        self.registry_path = root / "registry.json"
        self.sources_dir = root / "sources"

    def init(self) -> None:
        self.root.mkdir(parents=True, exist_ok=True)
        self.sources_dir.mkdir(parents=True, exist_ok=True)
        # agents.json is created once, then only edited by the user.
        if not self.agents_path.exists():
            write_json(self.agents_path, {"agents": {}})
        if not self.registry_path.exists():
            write_json(self.registry_path, {"schema": SCHEMA, "sources": {}, "items": {}})

    def require_init(self) -> None:
        if not self.registry_path.exists():
            die(f"{self.root} is not initialized; run: agentfiles init")
        if not self.agents_path.exists():
            die(f"agents.json not found in {self.root}; copy agents.example.json there "
                f"(agentfiles never writes agents.json)")

    def agents(self) -> dict:
        self.require_init()
        data = read_json(self.agents_path, {"agents": {}})
        if not isinstance(data.get("agents"), dict):
            die("invalid agents.json: missing 'agents' object")
        for name, cfg in data["agents"].items():
            validate_name(name, "agent name")
            if not isinstance(cfg, dict):
                die(f"invalid agents.json: agent {name!r} must be an object")
            for kind in KINDS.values():
                paths = cfg.get(kind, [])
                if not isinstance(paths, list) or not all(isinstance(p, str) and p for p in paths):
                    die(f"invalid agents.json: {name}.{kind} must be a list of paths")
        return data

    def registry(self) -> dict:
        self.require_init()
        data = read_json(self.registry_path, {"schema": SCHEMA, "sources": {}, "items": {}})
        if data.get("schema") != SCHEMA:
            die(f"unsupported registry schema {data.get('schema')!r}; expected {SCHEMA}")
        if not isinstance(data.get("sources"), dict) or not isinstance(data.get("items"), dict):
            die("invalid registry.json: 'sources' and 'items' must be objects")
        for name, source in data["sources"].items():
            validate_name(name, "source name")
            if not isinstance(source, dict):
                die(f"invalid registry.json: source {name!r} must be an object")
        for slug, item in data["items"].items():
            validate_name(slug, "item slug")
            if not isinstance(item, dict):
                die(f"invalid registry.json: item {slug!r} must be an object")
        return data

    def save_registry(self, data: dict) -> None:
        write_json(self.registry_path, data)


# --- git / source helpers ----------------------------------------------------

def git(*args: str, cwd: Path | None = None, capture: bool = False,
        check: bool = True) -> str:
    cmd = ["git", "-c", "core.hooksPath=/dev/null"]
    if cwd is not None:
        cmd += ["-C", str(cwd)]
    cmd += list(args)
    try:
        res = subprocess.run(cmd, check=False, text=True,
                             stdout=subprocess.PIPE if capture else None,
                             stderr=subprocess.PIPE if capture else None)
    except FileNotFoundError:
        die("git is required for git sources")
    if res.returncode:
        if not check:
            return ""
        detail = (res.stderr or res.stdout or "").strip()
        die(f"git failed: {' '.join(cmd)}" + (f"\n{detail}" if detail else ""))
    return (res.stdout or "").strip()


def is_git(value: str) -> bool:
    if "://" in value or value.startswith(("git@", "github:", "gitlab:")):
        return True
    return value.endswith(".git") or ".git" in value


def normalize_git(value: str) -> str:
    if value.startswith("github:"):
        return f"https://github.com/{value[len('github:'):].strip('/')}.git"
    if value.startswith("gitlab:"):
        return f"https://gitlab.com/{value[len('gitlab:'):].strip('/')}.git"
    if "://" not in value and not value.startswith("git@") and value.count("/") == 1:
        return f"https://github.com/{value.strip('/')}.git"
    return value


def default_name(value: str) -> str:
    # ponytail: git sources default to org-repo (vercel-labs-skills), not just the repo basename
    v = value
    if v.startswith("github:"):
        v = f"https://github.com/{v[len('github:'):].strip('/')}.git"
    elif v.startswith("git@"):
        v = "https://" + v[4:].replace(":", "/", 1)
    if "://" in v:
        segs = [s for s in urlparse(v).path.split("/") if s]
        if segs:
            repo = segs[-1].removesuffix(".git")
            org = segs[-2] if len(segs) >= 2 else ""
            raw = f"{org}-{repo}" if org else repo
        else:
            raw = "source"
    else:
        raw = Path(value).name.removesuffix(".git") or "source"
    raw = re.sub(r"[^A-Za-z0-9._-]+", "-", raw) or "source"
    return raw[1:] if raw and not raw[0].isalnum() else raw


def source_root(store: Store, source: dict) -> Path:
    if source.get("type") == "git":
        checkout = Path(source.get("checkout", ""))
        if checkout.is_absolute():
            die(f"invalid managed checkout path: {checkout}")
        root = ensure_within(store.sources_dir, store.root / checkout, "managed checkout")
        if root.parent != store.sources_dir.resolve():
            die(f"managed checkout must be directly under {store.sources_dir}: {root}")
        return root
    if source.get("type") != "local" or not isinstance(source.get("path"), str):
        die(f"invalid source record: {source!r}")
    return expand(source["path"]).resolve()


def item_path(store: Store, source: dict, subpath: str) -> Path:
    root = source_root(store, source)
    normalized = normalize_relative_path(subpath, "item path")
    return ensure_within(root, root / normalized, "item path")


# --- symlink helpers ---------------------------------------------------------

def link_targets(store: Store, agents: dict, item: dict, agent: str) -> list[Path]:
    cfg = agents["agents"].get(agent)
    if cfg is None:
        die(f"unknown agent {agent!r}; edit agents.json")
    item_type = item.get("type")
    if item_type not in KINDS:
        die(f"unknown item type {item_type!r}")
    dirs = cfg.get(KINDS[item_type], [])
    if not dirs:
        die(f"agent {agent!r} has no {KINDS[item_type]} directory configured")
    return [expand(d) / item["slug"] for d in dirs]


def inspect_link_targets(agents: dict, item: dict,
                         agent: str) -> tuple[list[Path], str | None]:
    cfg = agents.get("agents", {}).get(agent)
    if cfg is None:
        return [], f"unknown agent {agent!r}"
    item_type = item.get("type")
    if item_type not in KINDS:
        return [], f"unknown item type {item_type!r}"
    dirs = cfg.get(KINDS[item_type], [])
    if not dirs:
        return [], f"agent {agent!r} has no {KINDS[item_type]} directory configured"
    slug = item.get("slug")
    if not isinstance(slug, str):
        return [], "item has no valid slug"
    return [expand(path) / slug for path in dirs], None


def points_to(link: Path, target: Path) -> bool:
    if not link.is_symlink():
        return False
    raw = os.readlink(link)
    actual = (link.parent / raw).resolve() if not Path(raw).is_absolute() else Path(raw).resolve()
    return actual == target.resolve()


def make_link(target: Path, source: Path) -> bool:
    if not source.exists():
        die(f"cannot link missing source: {source}")
    if target.is_symlink():
        if points_to(target, source):
            return False
        die(f"refusing to replace unmanaged path: {target}")
    if target.exists():
        die(f"refusing to replace non-symlink path: {target}")
    try:
        target.parent.mkdir(parents=True, exist_ok=True)
        os.symlink(os.path.relpath(source, target.parent), target)
    except OSError as exc:
        die(f"cannot create symlink {target}: {exc}")
    return True


def drop_link(target: Path, source: Path) -> bool:
    if not target.is_symlink() or not points_to(target, source):
        return False
    try:
        target.unlink()
    except OSError as exc:
        die(f"cannot remove symlink {target}: {exc}")
    return True


def preflight_links(store: Store, agents: dict, item: dict,
                    agent_names: list[str], source: Path,
                    enable: bool) -> list[tuple[str, Path]]:
    if enable and not source.exists():
        die(f"item source does not exist: {source}")
    operations: list[tuple[str, Path]] = []
    seen: set[tuple[str, Path]] = set()
    for agent in dict.fromkeys(agent_names):
        for target in link_targets(store, agents, item, agent):
            key = (agent, target)
            if key in seen:
                continue
            seen.add(key)
            if enable:
                if target.is_symlink() and points_to(target, source):
                    pass
                elif target.is_symlink():
                    die(f"refusing to replace unmanaged symlink: {target}")
                elif target.exists():
                    die(f"refusing to replace non-symlink path: {target}")
            operations.append((agent, target))
    return operations


# --- commands ----------------------------------------------------------------

def cmd_init(args, store: Store) -> None:
    store.init()
    print(f"initialized {store.root}")
    print(f"  agents:   {store.agents_path}")
    print(f"  registry: {store.registry_path}")


def cmd_agents(args, store: Store) -> None:
    data = store.agents()
    rows = []
    for agent, cfg in sorted(data["agents"].items()):
        for kind in ("skills", "prompts", "extensions"):
            for path in cfg.get(kind, []):
                rows.append([agent, kind, path])
    table(["AGENT", "KIND", "DIRECTORY"], rows)


def resolve_git_ref(root: Path, ref: str) -> str:
    candidates = (
        f"refs/remotes/origin/{ref}",
        f"refs/tags/{ref}",
        ref,
    )
    for candidate in candidates:
        commit = git("rev-parse", "--verify", f"{candidate}^{{commit}}",
                     cwd=root, capture=True, check=False)
        if commit:
            return commit
    die(f"git ref not found after fetch: {ref}")


def add_source(store: Store, registry: dict, name: str, value: str,
               ref: str | None) -> tuple[str, bool]:
    name = name or default_name(value)
    validate_name(name, "source name")
    local = expand(value)
    existing = registry["sources"].get(name)
    # A local directory always wins, even if it looks like a git URL pattern.
    if local.exists():
        if not local.is_dir():
            die(f"local source is not a directory: {local}")
        if ref:
            die("--ref is only valid for git sources")
        resolved = str(local.resolve())
        if existing:
            if existing.get("type") == "local" and existing.get("path") == resolved:
                return name, False
            die(f"source name {name!r} is already used by a different source")
        registry["sources"][name] = {"type": "local", "path": str(local.resolve()), "added_at": now()}
        return name, True
    if not is_git(value):
        die(f"not a local path or git source: {value}")
    url = normalize_git(value)
    wanted_ref = ref or ""
    if existing:
        if (existing.get("type") == "git" and existing.get("url") == url
                and existing.get("ref", "") == wanted_ref):
            root = source_root(store, existing)
            if not root.is_dir():
                die(f"registered source checkout is missing: {root}; run doctor")
            return name, False
        die(f"source name {name!r} is already used by a different source or ref")
    host = re.sub(r"[^A-Za-z0-9._-]+", "-", urlparse(url).netloc or "git")
    dest = store.sources_dir / f"{name}-{host}"
    dest.parent.mkdir(parents=True, exist_ok=True)
    if dest.exists() or dest.is_symlink():
        die(f"refusing to replace existing checkout path: {dest}")
    tmp = store.sources_dir / f".{name}.tmp-{os.getpid()}"
    shutil.rmtree(tmp, ignore_errors=True)
    try:
        clone_args = ["clone"]
        if not ref:
            clone_args += ["--depth", "1"]
        clone_args += [url, str(tmp)]
        git(*clone_args)
        if ref:
            git("fetch", "--prune", "--tags", "origin", cwd=tmp)
            git("checkout", "--detach", resolve_git_ref(tmp, ref), cwd=tmp)
        os.replace(tmp, dest)
    except OSError as exc:
        shutil.rmtree(tmp, ignore_errors=True)
        die(f"cannot install source checkout at {dest}: {exc}")
    except BaseException:
        shutil.rmtree(tmp, ignore_errors=True)
        raise
    registry["sources"][name] = {
        "type": "git", "url": url, "ref": wanted_ref,
        "checkout": str(dest.relative_to(store.root)),
        "added_at": now(),
    }
    return name, True


def cmd_add(args, store: Store) -> None:
    registry = store.registry()
    parts = list(args.parts)
    if args.type is None and parts[0].lower() in TYPE_ALIASES:
        # Leading kind: add skill name src subpath. An explicit --type makes
        # names such as "skill" unambiguous slugs.
        typ = canonical_type(parts[0])
        rest = parts[1:]
    else:                                       # kind via --type: add name src subpath
        typ = canonical_type(args.type or "skill")
        rest = parts
    if len(rest) not in (2, 3):
        die("usage: agentfiles add [<kind>] <slug> <source> [subpath]")
    if args.subpath is not None and len(rest) == 3:
        die("use either positional subpath or --subpath, not both")
    slug = validate_name(rest[0], "slug")
    if slug in registry["items"]:
        die(f"item {slug!r} already exists; remove it first")
    source_value = rest[1]
    subpath = normalize_relative_path(args.subpath if args.subpath is not None
                                      else (rest[2] if len(rest) == 3 else "."),
                                      "subpath")
    source_name, created_source = add_source(
        store, registry, args.source_name or "", source_value, args.ref)
    source = registry["sources"][source_name]
    try:
        path = item_path(store, source, subpath)
        if not path.exists():
            die(f"item path does not exist: {path}")
        if typ == "skill" and not (path.is_dir() and (path / "SKILL.md").is_file()):
            die(f"skill must be a directory containing SKILL.md: {path}")
    except BaseException:
        if created_source:
            registry["sources"].pop(source_name, None)
            if source.get("type") == "git":
                checkout = source_root(store, source)
                if checkout.is_dir():
                    shutil.rmtree(checkout)
        raise
    registry["items"][slug] = {
        "slug": slug, "type": typ, "source": source_name,
        "path": subpath, "enabled": [], "added_at": now(),
    }
    try:
        store.save_registry(registry)
        if args.agent:
            apply_enable(store, registry, store.agents(), slug, args.agent, True)
    except BaseException:
        registry["items"].pop(slug, None)
        if created_source:
            registry["sources"].pop(source_name, None)
            if source.get("type") == "git":
                checkout = source_root(store, source)
                if checkout.is_dir():
                    shutil.rmtree(checkout)
        try:
            store.save_registry(registry)
        except BaseException:
            pass
        raise
    print(f"added {slug} ({typ}) from source {source_name}" +
          (f" at {subpath}" if subpath != "." else ""))


def cmd_scan(args, store: Store) -> None:
    registry = store.registry()
    if args.slug not in registry["items"]:
        die(f"unknown item {args.slug!r}; run 'agentfiles add' first")
    item = registry["items"][args.slug]
    scan_root = item_path(store, registry["sources"][item["source"]], item["path"])
    rows = [[name, rel] for name, rel in discover_skills(scan_root, args.under)]
    if rows:
        table(["SKILL", "PATH"], rows)
    else:
        print("(none found)")


def discover_skills(root: Path, under: str | None = None) -> list[tuple[str, str]]:
    root = root.resolve()
    relative = normalize_relative_path(under or ".", "scan path")
    scan = ensure_within(root, root / relative, "scan path")
    if not scan.is_dir():
        die(f"scan path is not a directory: {scan}")
    found = []
    for cur, dirs, files in os.walk(scan):
        dirs[:] = sorted(d for d in dirs if d not in SKIP_DIRS and not d.startswith("."))
        if "SKILL.md" in files:
            d = Path(cur)
            found.append((d.name, str(d.relative_to(root))))
            dirs[:] = []
    return sorted(found)


def selected_agents(args, agents: dict, item: dict) -> list[str]:
    names = list(getattr(args, "agent", None) or []) + list(getattr(args, "agent_flag", None) or [])
    if args.all and names:
        die("use agent names or --all, not both")
    if args.all:
        if not args.enable:
            return sorted(set(item.get("enabled", [])))
        return sorted(a for a, c in agents["agents"].items() if c.get(KINDS[item["type"]]))
    if not names:
        die("pass an agent name or --all")
    return list(dict.fromkeys(names))


def apply_enable(store, registry, agents, slug, agents_list, enable):
    item = registry["items"][slug]
    source = item_path(store, registry["sources"][item["source"]], item["path"])
    operations = preflight_links(store, agents, item, agents_list, source, enable)
    old_enabled = list(item.get("enabled", []))
    created: list[Path] = []
    removed: list[Path] = []
    try:
        for agent, target in operations:
            changed = make_link(target, source) if enable else drop_link(target, source)
            if changed:
                (created if enable else removed).append(target)
            print(f"{'enabled ' if enable else 'disabled'} {agent:<12} {slug} -> {target}")
        enabled = set(old_enabled)
        enabled.update(agents_list) if enable else enabled.difference_update(agents_list)
        item["enabled"] = sorted(enabled)
        store.save_registry(registry)
    except BaseException:
        item["enabled"] = old_enabled
        for target in reversed(created):
            target.unlink(missing_ok=True)
        for target in removed:
            if not target.exists() and not target.is_symlink():
                target.parent.mkdir(parents=True, exist_ok=True)
                os.symlink(os.path.relpath(source, target.parent), target)
        raise


def cmd_enable_disable(args, store: Store, enable: bool) -> None:
    registry = store.registry()
    agents = store.agents()
    if args.slug not in registry["items"]:
        die(f"unknown item {args.slug!r}")
    chosen = selected_agents(args, agents, registry["items"][args.slug])
    apply_enable(store, registry, agents, args.slug, chosen, enable)


def cmd_update(args, store: Store) -> None:
    registry = store.registry()
    names = args.sources or sorted(registry["sources"])
    for name in names:
        source = registry["sources"].get(name)
        if source is None:
            die(f"unknown source {name!r}")
        if source["type"] != "git":
            print(f"skip    {name}: local source")
            continue
        root = source_root(store, source)
        if not root.is_dir():
            die(f"missing checkout for {name}: {root}")
        before = git("rev-parse", "HEAD^{commit}", cwd=root, capture=True)
        git("fetch", "--prune", "--tags", "origin", cwd=root)
        ref = source.get("ref")
        if ref:
            git("checkout", "--detach", resolve_git_ref(root, ref), cwd=root)
        else:
            git("pull", "--ff-only", cwd=root)
        after = git("rev-parse", "HEAD^{commit}", cwd=root, capture=True)
        print(f"{'updated' if before != after else 'current':<8} {name} ({after[:12]})")


def cmd_remove(args, store: Store) -> None:
    registry = store.registry()
    agents = store.agents()
    if args.slug not in registry["items"]:
        die(f"unknown item {args.slug!r}")

    item = registry["items"][args.slug]
    source = registry["sources"].get(item.get("source"))
    if source is None:
        die(f"item {args.slug!r} references an unknown source; run doctor")
    src = item_path(store, source, item["path"])
    operations = preflight_links(
        store, agents, item, list(item.get("enabled", [])), src, False)

    source_name = item["source"]
    drop_source = not any(
        slug != args.slug and other.get("source") == source_name
        for slug, other in registry["items"].items()
    )
    checkout: Path | None = None
    quarantine: Path | None = None
    if drop_source and source.get("type") == "git":
        checkout = source_root(store, source)
        quarantine = checkout.with_name(f".{checkout.name}.remove-{os.getpid()}")
        if quarantine.exists() or quarantine.is_symlink():
            die(f"stale removal path exists: {quarantine}")

    removed_links: list[Path] = []
    try:
        for agent, target in operations:
            if drop_link(target, src):
                removed_links.append(target)
                print(f"removed link {agent}/{item['slug']} -> {target}")

        if checkout is not None and checkout.exists():
            try:
                os.replace(checkout, quarantine)
            except OSError as exc:
                die(f"cannot stage source checkout for removal: {exc}")

        registry["items"].pop(args.slug)
        if drop_source:
            registry["sources"].pop(source_name, None)
        store.save_registry(registry)
    except BaseException:
        registry["items"][args.slug] = item
        if drop_source:
            registry["sources"][source_name] = source
        if quarantine is not None and quarantine.exists() and checkout is not None:
            os.replace(quarantine, checkout)
        for target in removed_links:
            if not target.exists() and not target.is_symlink():
                target.parent.mkdir(parents=True, exist_ok=True)
                os.symlink(os.path.relpath(src, target.parent), target)
        raise

    if quarantine is not None and quarantine.exists():
        try:
            shutil.rmtree(quarantine)
            print(f"removed source checkout {source_name}")
        except OSError as exc:
            print(f"warning: registry updated, but could not delete {quarantine}: {exc}",
                  file=sys.stderr)
    print(f"removed {args.slug}")


def cmd_list(args, store: Store) -> None:
    registry = store.registry()
    rows = [[k, str(v.get("type", "?")), str(v.get("source", "?")),
             ",".join(v.get("enabled", [])) or "-"]
            for k, v in sorted(registry["items"].items())]
    table(["SLUG", "TYPE", "SOURCE", "ENABLED"], rows)


def cmd_status(args, store: Store) -> None:
    cmd_list(args, store)
    registry = store.registry()
    agents = store.agents()
    print("\ndrift (filesystem vs registry):")
    problems = 0
    for slug, item in registry["items"].items():
        source_record = registry["sources"].get(item.get("source"))
        if source_record is None:
            print(f"  UNKNOWN SOURCE: {slug}")
            problems += 1
            continue
        source = item_path(store, source_record, item.get("path", "."))
        if not source.exists():
            print(f"  MISSING ITEM:   {slug} -> {source}")
            problems += 1
        for agent in item.get("enabled", []):
            targets, error = inspect_link_targets(agents, item, agent)
            if error:
                print(f"  BAD CONFIG:     {agent}/{slug}: {error}")
                problems += 1
                continue
            for target in targets:
                if not target.is_symlink():
                    print(f"  MISSING LINK:   {agent}/{slug} -> {target}")
                    problems += 1
                elif not points_to(target, source):
                    print(f"  WRONG LINK:     {agent}/{slug} -> {target}")
                    problems += 1
    print("  none" if not problems else f"  {problems} problem(s)")


def cmd_sync(args, store: Store) -> None:
    registry = store.registry()
    agents = store.agents()
    enable_ops: list[tuple[dict, Path, str, Path]] = []
    disable_ops: list[tuple[Path, Path]] = []

    for item in registry["items"].values():
        source_record = registry["sources"].get(item.get("source"))
        if source_record is None:
            die(f"item {item.get('slug', '?')!r} references an unknown source")
        source = item_path(store, source_record, item.get("path", "."))
        for agent, target in preflight_links(
                store, agents, item, list(item.get("enabled", [])), source, True):
            enable_ops.append((item, source, agent, target))
        for agent in set(agents["agents"]) - set(item.get("enabled", [])):
            if not agents["agents"][agent].get(KINDS[item["type"]]):
                continue
            for target in link_targets(store, agents, item, agent):
                disable_ops.append((target, source))

    created: list[Path] = []
    try:
        for item, source, agent, target in enable_ops:
            if make_link(target, source):
                created.append(target)
                print(f"linked   {agent:<12} {item['slug']} -> {target}")
    except BaseException:
        for target in reversed(created):
            target.unlink(missing_ok=True)
        raise

    removed = sum(1 for target, source in disable_ops if drop_link(target, source))
    print(f"synced {len(created)} new link(s), removed {removed} stale link(s)")


def cmd_doctor(args, store: Store) -> None:
    registry = store.registry()
    agents = store.agents()
    problems = 0

    for name, source in registry["sources"].items():
        root = source_root(store, source)
        if not root.is_dir():
            print(f"ERROR missing source {name}: {root}")
            problems += 1
        elif source.get("type") == "git" and not (root / ".git").exists():
            print(f"ERROR source {name} is not a git checkout: {root}")
            problems += 1

    for slug, item in registry["items"].items():
        if item.get("slug") != slug:
            print(f"ERROR {slug}: stored slug is {item.get('slug')!r}")
            problems += 1
        if item.get("type") not in KINDS:
            print(f"ERROR {slug}: unknown item type {item.get('type')!r}")
            problems += 1
            continue
        source = registry["sources"].get(item.get("source"))
        if source is None:
            print(f"ERROR {slug}: references unknown source {item.get('source')!r}")
            problems += 1
            continue
        root = item_path(store, source, item.get("path", "."))
        if not root.exists():
            print(f"ERROR missing item {slug}: {root}")
            problems += 1
        elif item["type"] == "skill" and not (root.is_dir() and (root / "SKILL.md").is_file()):
            print(f"ERROR invalid skill {slug}: {root} has no SKILL.md")
            problems += 1
        enabled = item.get("enabled", [])
        if not isinstance(enabled, list) or not all(isinstance(a, str) for a in enabled):
            print(f"ERROR {slug}: enabled must be a list of agent names")
            problems += 1
            continue
        if len(enabled) != len(set(enabled)):
            print(f"ERROR {slug}: duplicate enabled agents")
            problems += 1
        for agent in enabled:
            targets, error = inspect_link_targets(agents, item, agent)
            if error:
                print(f"ERROR {slug} for {agent}: {error}")
                problems += 1
                continue
            for target in targets:
                if not target.is_symlink() or not points_to(target, root):
                    print(f"ERROR broken link {slug} for {agent}: {target}")
                    problems += 1

    if problems:
        die(f"doctor found {problems} problem(s)")
    print(f"ok: {len(registry['sources'])} source(s), {len(registry['items'])} item(s)")

def table(headers: list[str], rows: list[list[str]]) -> None:
    if not rows:
        print("(none)")
        return
    widths = [len(h) for h in headers]
    for row in rows:
        for i, v in enumerate(row):
            widths[i] = max(widths[i], len(v))
    print("  ".join(h.ljust(widths[i]) for i, h in enumerate(headers)))
    print("  ".join("-" * w for w in widths))
    for row in rows:
        print("  ".join(v.ljust(widths[i]) for i, v in enumerate(row)))


# --- cli ---------------------------------------------------------------------

@contextlib.contextmanager
def locked(store: Store):
    store.root.mkdir(parents=True, exist_ok=True)
    lock = store.root / ".lock"
    fh = lock.open("a+")
    fcntl.flock(fh.fileno(), fcntl.LOCK_EX)
    try:
        yield
    finally:
        fcntl.flock(fh.fileno(), fcntl.LOCK_UN)
        fh.close()


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(prog="agentfiles", description=__doc__.splitlines()[0])
    p.add_argument("--home", default=os.environ.get("AGENTFILES_HOME", "~/.agentfiles"), help="manager directory (default ~/.agentfiles)")
    sub = p.add_subparsers(dest="command", required=True)

    sub.add_parser("init", help="initialize state and create agents.json if missing")
    sub.add_parser("list", help="list registered items")

    a = sub.add_parser("agents", help="list agent discovery directories")
    a_sub = a.add_subparsers(dest="agent_command", required=True)
    a_sub.add_parser("ls", aliases=["list"])

    add = sub.add_parser("add", help="register a source + item, optionally enable it")
    add.add_argument("parts", nargs="+",
                     help="[<kind>] <slug> <source> [subpath]  (kind: skill|prompt|extension; optional)")
    add.add_argument("--subpath", help="path within the source (default: root); alt. to positional subpath")
    add.add_argument("--type", help="kind when omitted as first arg (default: skill)")
    add.add_argument("--ref", help="git branch/tag/commit to check out")
    add.add_argument("--source-name", help="reuse/name the source explicitly")
    add.add_argument("--agent", action="append", help="enable for this agent after adding (repeatable)")

    scan = sub.add_parser("scan", help="list SKILL.md directories under an item")
    scan.add_argument("slug")
    scan.add_argument("--under", help="scan only this subpath")

    for c, en in (("enable", True), ("disable", False)):
        ep = sub.add_parser(c, help=f"{c} an item for selected agents")
        ep.add_argument("slug")
        ep.add_argument("agent", nargs="*", help="agent name(s) (or use --agent)")
        ep.add_argument("--agent", dest="agent_flag", action="append", help="agent name (repeatable)")
        ep.add_argument("--all", action="store_true", help="every compatible/enabled agent")
        ep.set_defaults(enable=en)

    up = sub.add_parser("update", help="fetch and check out latest git sources")
    up.add_argument("sources", nargs="*")

    sub.add_parser("remove", help="unlink everywhere and drop the source").add_argument("slug")
    sub.add_parser("status", help="list items and show filesystem drift")
    sub.add_parser("sync", help="make symlinks match the registry exactly")
    sub.add_parser("doctor", help="validate sources, items, and enabled links")
    return p


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    store = Store(expand(args.home))
    mutating = args.command not in ("list", "agents", "scan", "status", "doctor")
    if mutating:
        with locked(store):
            dispatch(args, store)
    else:
        dispatch(args, store)
    return 0


def dispatch(args, store: Store) -> None:
    cmd = args.command
    if cmd == "init":
        cmd_init(args, store)
    elif cmd == "list":
        cmd_list(args, store)
    elif cmd == "agents":
        cmd_agents(args, store)
    elif cmd == "add":
        cmd_add(args, store)
    elif cmd == "scan":
        cmd_scan(args, store)
    elif cmd in ("enable", "disable"):
        cmd_enable_disable(args, store, args.enable)
    elif cmd == "update":
        cmd_update(args, store)
    elif cmd == "remove":
        cmd_remove(args, store)
    elif cmd == "status":
        cmd_status(args, store)
    elif cmd == "sync":
        cmd_sync(args, store)
    elif cmd == "doctor":
        cmd_doctor(args, store)


if __name__ == "__main__":
    raise SystemExit(main())
