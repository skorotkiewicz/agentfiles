#!/usr/bin/env python3
"""atctl: a symlink-only manager for agent skills, prompts, and extensions."""

from __future__ import annotations

import argparse
import contextlib
import datetime as dt
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path
from typing import Any, Iterable

try:
    import fcntl
except ImportError:  # pragma: no cover - Unix is the primary target.
    fcntl = None

SCHEMA = 1
KINDS = ("skills", "prompts", "extensions")
KIND_ALIASES = {
    "skill": "skills",
    "skills": "skills",
    "prompt": "prompts",
    "prompts": "prompts",
    "extension": "extensions",
    "extensions": "extensions",
    "plugin": "extensions",
    "plugins": "extensions",
    "command": "prompts",
    "commands": "prompts",
}
NAME_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]*$")
SKIP_SCAN_DIRS = {
    ".git",
    ".hg",
    ".svn",
    "node_modules",
    "vendor",
    "dist",
    "build",
    "target",
}

DEFAULT_AGENTS = {
    "schema": SCHEMA,
    "agents": {
        "pi": {
            "skills": ["~/.pi/agent/skills"],
            "prompts": ["~/.pi/agent/prompts"],
            "extensions": ["~/.pi/agent/extensions"],
        },
        "codex": {"skills": ["~/.codex/skills"]},
        "claude": {
            "skills": ["~/.claude/skills"],
            "prompts": ["~/.claude/commands"],
        },
        "opencode": {
            "skills": ["~/.config/opencode/skills"],
            "prompts": ["~/.config/opencode/commands"],
            "extensions": ["~/.config/opencode/plugins"],
        },
    },
}

DEFAULT_REGISTRY = {"schema": SCHEMA, "sources": {}, "items": {}}


class AtError(RuntimeError):
    pass


def now() -> str:
    return dt.datetime.now(dt.timezone.utc).replace(microsecond=0).isoformat()


def eprint(*args: object) -> None:
    print(*args, file=sys.stderr)


def expand(path: str | Path) -> Path:
    return Path(os.path.expandvars(os.path.expanduser(str(path))))


def canonical_kind(value: str) -> str:
    try:
        return KIND_ALIASES[value.lower()]
    except KeyError as exc:
        raise AtError(
            f"unknown kind {value!r}; choose skills, prompts, or extensions"
        ) from exc


def validate_name(value: str, label: str = "name") -> str:
    if not NAME_RE.fullmatch(value):
        raise AtError(
            f"invalid {label} {value!r}; use letters, digits, dot, underscore, or dash"
        )
    return value


def validate_target_name(value: str) -> str:
    if value in {"", ".", ".."} or Path(value).name != value or "/" in value:
        raise AtError(f"invalid target name {value!r}; it must be one filename")
    return value


def safe_relative(value: str) -> Path:
    path = Path(value)
    if path.is_absolute() or any(part == ".." for part in path.parts):
        raise AtError(f"item path must be relative and may not contain '..': {value!r}")
    return path


def read_json(path: Path, default: dict[str, Any] | None = None) -> dict[str, Any]:
    if not path.exists():
        if default is None:
            raise AtError(f"missing file: {path}")
        return json.loads(json.dumps(default))
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise AtError(f"cannot read JSON {path}: {exc}") from exc
    if not isinstance(data, dict):
        raise AtError(f"expected a JSON object in {path}")
    return data


def atomic_write_json(path: Path, data: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    payload = json.dumps(data, indent=2, sort_keys=True) + "\n"
    fd, temp_name = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    temp = Path(temp_name)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp, path)
    finally:
        with contextlib.suppress(FileNotFoundError):
            temp.unlink()


@contextlib.contextmanager
def root_lock(root: Path):
    root.mkdir(parents=True, exist_ok=True)
    lock_path = root / ".lock"
    with lock_path.open("a+") as handle:
        if fcntl is not None:
            fcntl.flock(handle.fileno(), fcntl.LOCK_EX)
        try:
            yield
        finally:
            if fcntl is not None:
                fcntl.flock(handle.fileno(), fcntl.LOCK_UN)


class Store:
    def __init__(self, root: Path):
        self.root = root
        self.agents_path = root / "agents.json"
        self.registry_path = root / "registry.json"
        self.sources_dir = root / "sources"

    def init(self) -> None:
        self.root.mkdir(parents=True, exist_ok=True)
        self.sources_dir.mkdir(parents=True, exist_ok=True)
        if not self.agents_path.exists():
            atomic_write_json(self.agents_path, DEFAULT_AGENTS)
        if not self.registry_path.exists():
            atomic_write_json(self.registry_path, DEFAULT_REGISTRY)

    def require_init(self) -> None:
        if not self.agents_path.exists() or not self.registry_path.exists():
            raise AtError(f"{self.root} is not initialized; run: atctl init")

    def agents(self) -> dict[str, Any]:
        self.require_init()
        data = read_json(self.agents_path)
        validate_agents(data)
        return data

    def registry(self) -> dict[str, Any]:
        self.require_init()
        data = read_json(self.registry_path)
        validate_registry(data)
        return data


def validate_agents(data: dict[str, Any]) -> None:
    if data.get("schema") != SCHEMA or not isinstance(data.get("agents"), dict):
        raise AtError("invalid agents.json schema")
    for agent, config in data["agents"].items():
        validate_name(agent, "agent name")
        if not isinstance(config, dict):
            raise AtError(f"agent {agent!r} must be an object")
        for kind, paths in config.items():
            if kind not in KINDS:
                raise AtError(f"agent {agent!r} has non-canonical kind {kind!r}")
            if not isinstance(paths, list) or not all(
                isinstance(p, str) and p for p in paths
            ):
                raise AtError(f"agent {agent!r} kind {kind!r} must be a list of paths")


def validate_registry(data: dict[str, Any]) -> None:
    if data.get("schema") != SCHEMA:
        raise AtError("invalid registry.json schema")
    if not isinstance(data.get("sources"), dict) or not isinstance(
        data.get("items"), dict
    ):
        raise AtError("registry.json must contain source and item objects")


def source_root(store: Store, source: dict[str, Any]) -> Path:
    if source["type"] == "git":
        return store.root / source["checkout"]
    return expand(source["path"])


def item_path(store: Store, registry: dict[str, Any], item: dict[str, Any]) -> Path:
    source = registry["sources"].get(item["source"])
    if source is None:
        raise AtError(f"item references missing source {item['source']!r}")
    root = source_root(store, source).resolve()
    candidate = (root / safe_relative(item["path"])).resolve()
    try:
        candidate.relative_to(root)
    except ValueError as exc:
        raise AtError(f"item path escapes source root: {item['path']!r}") from exc
    return candidate


def git(repo: Path | None, *args: str, capture: bool = False) -> str:
    command = ["git", "-c", "gc.auto=0", "-c", "core.hooksPath=/dev/null"]
    if repo is not None:
        command += ["-C", str(repo)]
    command += list(args)
    try:
        result = subprocess.run(
            command,
            check=True,
            text=True,
            stdout=subprocess.PIPE if capture else None,
            stderr=subprocess.PIPE if capture else None,
        )
    except FileNotFoundError as exc:
        raise AtError("git is required for Git sources") from exc
    except subprocess.CalledProcessError as exc:
        detail = (exc.stderr or exc.stdout or "").strip()
        raise AtError(
            f"git failed: {' '.join(command)}" + (f"\n{detail}" if detail else "")
        ) from exc
    return (result.stdout or "").strip()


def normalize_git_source(value: str) -> str:
    if value.startswith("github:"):
        repo = value.removeprefix("github:").strip("/")
        return f"https://github.com/{repo}.git"
    if "://" not in value and not value.startswith("git@") and value.count("/") == 1:
        return f"https://github.com/{value.strip('/')}.git"
    return value


def default_source_name(value: str) -> str:
    raw = value.rstrip("/").rsplit("/", 1)[-1]
    raw = raw.removesuffix(".git") or "source"
    raw = re.sub(r"[^A-Za-z0-9._-]+", "-", raw).strip("-.") or "source"
    if not raw[0].isalnum():
        raw = f"source-{raw}"
    return raw


def git_ref_kind(repo: Path, requested_ref: str | None) -> tuple[str, str, str]:
    if requested_ref:
        remote_ref = f"refs/remotes/origin/{requested_ref}"
        try:
            commit = git(
                repo, "rev-parse", "--verify", f"{remote_ref}^{{commit}}", capture=True
            )
            return requested_ref, "branch", commit
        except AtError:
            commit = git(
                repo,
                "rev-parse",
                "--verify",
                f"{requested_ref}^{{commit}}",
                capture=True,
            )
            return requested_ref, "fixed", commit

    branch = git(repo, "symbolic-ref", "--short", "HEAD", capture=True)
    commit = git(repo, "rev-parse", "HEAD^{commit}", capture=True)
    return branch, "branch", commit


def add_source(
    store: Store, registry: dict[str, Any], name: str, value: str, ref: str | None
) -> dict[str, Any]:
    validate_name(name, "source name")
    if name in registry["sources"]:
        raise AtError(f"source {name!r} already exists")

    local = expand(value)
    if local.exists():
        if not local.is_dir():
            raise AtError(f"local source must be a directory: {local}")
        source = {
            "type": "local",
            "path": str(local.resolve()),
            "added_at": now(),
        }
        registry["sources"][name] = source
        return source

    if value.startswith((".", "~", "/")):
        raise AtError(f"local source does not exist: {local}")

    url = normalize_git_source(value)
    destination = store.sources_dir / name
    if destination.exists() or destination.is_symlink():
        raise AtError(f"source checkout already exists: {destination}")
    store.sources_dir.mkdir(parents=True, exist_ok=True)
    temporary = store.sources_dir / f".{name}.tmp-{os.getpid()}"
    shutil.rmtree(temporary, ignore_errors=True)
    try:
        git(None, "clone", "--origin", "origin", "--", url, str(temporary))
        resolved_ref, ref_kind, commit = git_ref_kind(temporary, ref)
        git(temporary, "checkout", "--detach", commit)
        os.replace(temporary, destination)
    except Exception:
        shutil.rmtree(temporary, ignore_errors=True)
        raise

    source = {
        "type": "git",
        "url": url,
        "ref": resolved_ref,
        "ref_kind": ref_kind,
        "revision": commit,
        "checkout": str(destination.relative_to(store.root)),
        "added_at": now(),
        "updated_at": now(),
    }
    registry["sources"][name] = source
    return source


def restore_source(
    store: Store, name: str, source: dict[str, Any], dry_run: bool = False
) -> bool:
    root = source_root(store, source)
    if root.is_dir():
        return False
    if source["type"] != "git":
        raise AtError(f"missing local source {name!r}: {root}")
    if dry_run:
        print(f"restore  source     {name} -> {root}")
        return True

    root.parent.mkdir(parents=True, exist_ok=True)
    temporary = root.parent / f".{root.name}.restore-{os.getpid()}"
    shutil.rmtree(temporary, ignore_errors=True)
    try:
        git(None, "clone", "--origin", "origin", "--", source["url"], str(temporary))
        revision = source.get("revision")
        if revision:
            commit = git(
                temporary,
                "rev-parse",
                "--verify",
                f"{revision}^{{commit}}",
                capture=True,
            )
        elif source.get("ref_kind") == "branch":
            commit = git(
                temporary,
                "rev-parse",
                "--verify",
                f"refs/remotes/origin/{source['ref']}^{{commit}}",
                capture=True,
            )
        else:
            commit = git(
                temporary,
                "rev-parse",
                "--verify",
                f"{source['ref']}^{{commit}}",
                capture=True,
            )
        git(temporary, "checkout", "--detach", commit)
        os.replace(temporary, root)
    except Exception:
        shutil.rmtree(temporary, ignore_errors=True)
        raise
    print(f"restore  source     {name} -> {root}")
    return True


def update_source(store: Store, name: str, source: dict[str, Any]) -> tuple[str, str]:
    if source["type"] != "git":
        return "local", "local source; skipped"
    repo = source_root(store, source)
    if not repo.is_dir():
        raise AtError(f"missing checkout for source {name!r}: {repo}")
    dirty = git(repo, "status", "--porcelain", capture=True)
    if dirty:
        raise AtError(f"source {name!r} has local changes; refusing to update")
    before = git(repo, "rev-parse", "HEAD^{commit}", capture=True)
    git(repo, "fetch", "--prune", "--force", "--tags", "origin")
    ref = source["ref"]
    if source.get("ref_kind") == "branch":
        commit = git(
            repo,
            "rev-parse",
            "--verify",
            f"refs/remotes/origin/{ref}^{{commit}}",
            capture=True,
        )
    else:
        commit = git(repo, "rev-parse", "--verify", f"{ref}^{{commit}}", capture=True)
    git(repo, "checkout", "--detach", commit)
    source["revision"] = commit
    source["updated_at"] = now()
    return ("updated" if before != commit else "current"), commit[:12]


def parse_skill_name(skill_md: Path) -> str:
    try:
        text = skill_md.read_text(encoding="utf-8", errors="replace")
    except OSError:
        return skill_md.parent.name
    if text.startswith("---"):
        end = text.find("\n---", 3)
        if end != -1:
            frontmatter = text[3:end]
            match = re.search(r"(?m)^name\s*:\s*['\"]?([^'\"\n]+)", frontmatter)
            if match:
                candidate = match.group(1).strip()
                if NAME_RE.fullmatch(candidate):
                    return candidate
    return skill_md.parent.name


def discover_skills(root: Path, under: str | None = None) -> list[tuple[str, str]]:
    scan_root = root / safe_relative(under) if under else root
    if not scan_root.is_dir():
        raise AtError(f"scan path is not a directory: {scan_root}")
    found: list[tuple[str, str]] = []
    for current, dirs, files in os.walk(scan_root):
        dirs[:] = sorted(
            d for d in dirs if d not in SKIP_SCAN_DIRS and not d.startswith(".atctl-")
        )
        if "SKILL.md" in files:
            directory = Path(current)
            name = parse_skill_name(directory / "SKILL.md")
            try:
                validate_name(name, "skill name")
            except AtError:
                continue
            found.append((name, str(directory.relative_to(root))))
            dirs[:] = []
    found.sort(key=lambda pair: (pair[0], pair[1]))
    return found


def item_id(kind: str, name: str) -> str:
    return f"{kind[:-1]}:{name}"


def add_item(
    store: Store,
    registry: dict[str, Any],
    kind: str,
    name: str,
    source_name: str,
    relative_path: str,
    target_name: str | None = None,
    explicit_id: str | None = None,
) -> str:
    kind = canonical_kind(kind)
    validate_name(name, "item name")
    if source_name not in registry["sources"]:
        raise AtError(f"unknown source {source_name!r}")
    relative = safe_relative(relative_path)
    identifier = explicit_id or item_id(kind, name)
    validate_name(identifier.replace(":", "-"), "item id")
    if identifier in registry["items"]:
        existing = registry["items"][identifier]
        if existing["source"] == source_name and existing["path"] == str(relative):
            return identifier
        raise AtError(f"item {identifier!r} already exists")
    target = validate_target_name(target_name or relative.name or name)
    item = {
        "name": name,
        "kind": kind,
        "source": source_name,
        "path": str(relative),
        "target_name": target,
        "enabled": [],
        "added_at": now(),
    }
    path = item_path(store, {**registry, "items": {identifier: item}}, item)
    if not path.exists():
        raise AtError(f"item path does not exist: {path}")
    if kind == "skills" and (not path.is_dir() or not (path / "SKILL.md").is_file()):
        raise AtError(f"skill must be a directory containing SKILL.md: {path}")
    registry["items"][identifier] = item
    return identifier


def resolve_item_ids(registry: dict[str, Any], selectors: Iterable[str]) -> list[str]:
    resolved: list[str] = []
    for selector in selectors:
        if selector in registry["items"]:
            identifier = selector
        else:
            matches = [
                key
                for key, item in registry["items"].items()
                if item.get("name") == selector
            ]
            if not matches:
                raise AtError(f"unknown item {selector!r}")
            if len(matches) > 1:
                raise AtError(
                    f"ambiguous item {selector!r}; use one of: {', '.join(sorted(matches))}"
                )
            identifier = matches[0]
        if identifier not in resolved:
            resolved.append(identifier)
    return resolved


def agent_targets(agents: dict[str, Any], agent: str, kind: str) -> list[Path]:
    config = agents["agents"].get(agent)
    if config is None:
        raise AtError(
            f"unknown agent {agent!r}; edit agents.json or run 'atctl agent add'"
        )
    targets = config.get(kind, [])
    if not targets:
        raise AtError(f"agent {agent!r} has no target configured for {kind}")
    return [expand(path) for path in targets]


def symlink_points_to(link: Path, expected: Path) -> bool:
    if not link.is_symlink():
        return False
    raw = os.readlink(link)
    actual = (
        (link.parent / raw).resolve()
        if not Path(raw).is_absolute()
        else Path(raw).resolve()
    )
    return actual == expected.resolve()


def plan_links(
    store: Store,
    agents: dict[str, Any],
    registry: dict[str, Any],
    item_ids: Iterable[str],
    agent_names: Iterable[str],
    allow_missing: bool = False,
) -> list[tuple[str, str, Path, Path]]:
    plans: list[tuple[str, str, Path, Path]] = []
    for identifier in item_ids:
        item = registry["items"][identifier]
        source = item_path(store, registry, item)
        if not source.exists() and not allow_missing:
            raise AtError(f"missing item source for {identifier}: {source}")
        for agent in agent_names:
            for directory in agent_targets(agents, agent, item["kind"]):
                target = directory / item["target_name"]
                plans.append((identifier, agent, source, target))
    return plans


def enable_items(
    store: Store,
    agents: dict[str, Any],
    registry: dict[str, Any],
    item_ids: list[str],
    agent_names: list[str],
    dry_run: bool = False,
) -> list[str]:
    plans = plan_links(store, agents, registry, item_ids, agent_names)
    for identifier, agent, source, target in plans:
        if target.exists() or target.is_symlink():
            if not symlink_points_to(target, source):
                raise AtError(f"refusing to replace unmanaged path: {target}")
    actions: list[str] = []
    created: list[Path] = []
    try:
        for identifier, agent, source, target in plans:
            if target.is_symlink():
                actions.append(f"current  {agent:<10} {identifier} -> {target}")
                continue
            actions.append(f"enable   {agent:<10} {identifier} -> {target}")
            if not dry_run:
                target.parent.mkdir(parents=True, exist_ok=True)
                relative = os.path.relpath(source, target.parent)
                os.symlink(relative, target)
                created.append(target)
        if not dry_run:
            for identifier in item_ids:
                enabled = set(registry["items"][identifier].get("enabled", []))
                enabled.update(agent_names)
                registry["items"][identifier]["enabled"] = sorted(enabled)
    except Exception:
        for target in reversed(created):
            with contextlib.suppress(OSError):
                target.unlink()
        raise
    return actions


def disable_items(
    store: Store,
    agents: dict[str, Any],
    registry: dict[str, Any],
    item_ids: list[str],
    agent_names: list[str],
    dry_run: bool = False,
) -> list[str]:
    plans = plan_links(store, agents, registry, item_ids, agent_names)
    for identifier, agent, source, target in plans:
        if target.exists() or target.is_symlink():
            if not symlink_points_to(target, source):
                raise AtError(f"refusing to unlink unmanaged path: {target}")
    actions: list[str] = []
    removed: list[tuple[Path, str]] = []
    try:
        for identifier, agent, source, target in plans:
            if not target.is_symlink():
                actions.append(f"missing  {agent:<10} {identifier} -> {target}")
                continue
            actions.append(f"disable  {agent:<10} {identifier} -> {target}")
            if not dry_run:
                raw = os.readlink(target)
                target.unlink()
                removed.append((target, raw))
        if not dry_run:
            for identifier in item_ids:
                enabled = set(registry["items"][identifier].get("enabled", []))
                enabled.difference_update(agent_names)
                registry["items"][identifier]["enabled"] = sorted(enabled)
    except Exception:
        for target, raw in removed:
            with contextlib.suppress(OSError):
                target.parent.mkdir(parents=True, exist_ok=True)
                os.symlink(raw, target)
        raise
    return actions


def print_table(headers: list[str], rows: list[list[str]]) -> None:
    if not rows:
        print("(none)")
        return
    widths = [len(header) for header in headers]
    for row in rows:
        for index, value in enumerate(row):
            widths[index] = max(widths[index], len(value))
    print("  ".join(header.ljust(widths[i]) for i, header in enumerate(headers)))
    print("  ".join("-" * width for width in widths))
    for row in rows:
        print("  ".join(value.ljust(widths[i]) for i, value in enumerate(row)))


def cmd_init(args: argparse.Namespace, store: Store) -> None:
    with root_lock(store.root):
        store.init()
    print(f"initialized {store.root}")
    print(f"agents:   {store.agents_path}")
    print(f"registry: {store.registry_path}")


def cmd_agent(args: argparse.Namespace, store: Store) -> None:
    with root_lock(store.root):
        store.require_init()
        data = store.agents()
        if args.agent_command == "ls":
            rows = []
            for agent, config in sorted(data["agents"].items()):
                for kind in KINDS:
                    for path in config.get(kind, []):
                        rows.append([agent, kind, path])
            print_table(["AGENT", "KIND", "TARGET"], rows)
            return
        kind = canonical_kind(args.kind)
        validate_name(args.agent, "agent name")
        config = data["agents"].setdefault(args.agent, {})
        paths = config.setdefault(kind, [])
        if args.agent_command == "add":
            if args.path not in paths:
                paths.append(args.path)
                paths.sort()
                atomic_write_json(store.agents_path, data)
            print(f"added {args.agent} {kind}: {args.path}")
        elif args.agent_command == "rm":
            if args.path not in paths:
                raise AtError(f"target is not configured: {args.path}")
            paths.remove(args.path)
            if not paths:
                config.pop(kind, None)
            if not config:
                data["agents"].pop(args.agent, None)
            atomic_write_json(store.agents_path, data)
            print(f"removed {args.agent} {kind}: {args.path}")


def cmd_source(args: argparse.Namespace, store: Store) -> None:
    with root_lock(store.root):
        store.require_init()
        registry = store.registry()
        if args.source_command == "ls":
            rows = []
            for name, source in sorted(registry["sources"].items()):
                origin = source.get("url") or source.get("path", "")
                rev = source.get("revision", "")[:12]
                rows.append([name, source["type"], source.get("ref", ""), rev, origin])
            print_table(["SOURCE", "TYPE", "REF", "REVISION", "ORIGIN"], rows)
            return
        if args.source_command == "add":
            add_source(store, registry, args.name, args.source, args.ref)
            atomic_write_json(store.registry_path, registry)
            print(f"added source {args.name}")
            return
        if args.source_command == "rm":
            if args.name not in registry["sources"]:
                raise AtError(f"unknown source {args.name!r}")
            users = [
                key
                for key, item in registry["items"].items()
                if item["source"] == args.name
            ]
            if users:
                raise AtError(f"source is still used by: {', '.join(sorted(users))}")
            source = registry["sources"].pop(args.name)
            atomic_write_json(store.registry_path, registry)
            if args.delete_checkout and source["type"] == "git":
                checkout = source_root(store, source)
                if checkout.parent.resolve() != store.sources_dir.resolve():
                    raise AtError(
                        f"refusing to delete unexpected checkout path: {checkout}"
                    )
                shutil.rmtree(checkout)
            print(f"removed source {args.name}")


def cmd_scan(args: argparse.Namespace, store: Store) -> None:
    registry = store.registry()
    source = registry["sources"].get(args.source)
    if source is None:
        raise AtError(f"unknown source {args.source!r}")
    root = source_root(store, source)
    rows = [[name, path] for name, path in discover_skills(root, args.under)]
    print_table(["SKILL", "PATH"], rows)


def cmd_item(args: argparse.Namespace, store: Store) -> None:
    with root_lock(store.root):
        store.require_init()
        registry = store.registry()
        agents = store.agents()
        if args.item_command == "ls":
            rows = []
            for identifier, item in sorted(registry["items"].items()):
                rows.append(
                    [
                        identifier,
                        item["kind"],
                        item["source"],
                        item["path"],
                        ",".join(item.get("enabled", [])) or "-",
                    ]
                )
            print_table(["ITEM", "KIND", "SOURCE", "PATH", "ENABLED"], rows)
            return
        if args.item_command == "add":
            identifier = add_item(
                store,
                registry,
                args.kind,
                args.name,
                args.source,
                args.path,
                args.target_name,
                args.id,
            )
            if args.agent:
                actions = enable_items(
                    store, agents, registry, [identifier], args.agent, args.dry_run
                )
                for action in actions:
                    print(action)
            if not args.dry_run:
                atomic_write_json(store.registry_path, registry)
            print(f"registered {identifier}" + (" (dry run)" if args.dry_run else ""))
            return
        if args.item_command == "rm":
            ids = resolve_item_ids(registry, args.items)
            enabled = {
                identifier: registry["items"][identifier].get("enabled", [])
                for identifier in ids
            }
            active = {key: value for key, value in enabled.items() if value}
            if active and not args.disable:
                detail = "; ".join(
                    f"{key}: {','.join(value)}" for key, value in active.items()
                )
                raise AtError(f"items are enabled ({detail}); pass --disable first")
            if args.disable:
                for identifier, agent_names in active.items():
                    for action in disable_items(
                        store,
                        agents,
                        registry,
                        [identifier],
                        list(agent_names),
                        args.dry_run,
                    ):
                        print(action)
            if not args.dry_run:
                for identifier in ids:
                    registry["items"].pop(identifier)
                atomic_write_json(store.registry_path, registry)
            for identifier in ids:
                print(f"removed {identifier}" + (" (dry run)" if args.dry_run else ""))


def cmd_install(args: argparse.Namespace, store: Store) -> None:
    with root_lock(store.root):
        store.require_init()
        registry = store.registry()
        agents = store.agents()
        source_name = args.source_name or default_source_name(args.source)
        if source_name not in registry["sources"]:
            if args.dry_run:
                raise AtError(
                    "dry-run install requires the source to already be registered"
                )
            add_source(store, registry, source_name, args.source, args.ref)
        source = registry["sources"][source_name]
        discovered = discover_skills(source_root(store, source), args.under)
        if args.skill:
            wanted = set(args.skill)
            discovered = [pair for pair in discovered if pair[0] in wanted]
            missing = sorted(wanted - {name for name, _ in discovered})
            if missing:
                raise AtError(f"skills not found: {', '.join(missing)}")
        if not discovered:
            raise AtError("no skills found")
        identifiers = []
        for name, path in discovered:
            identifier = add_item(store, registry, "skills", name, source_name, path)
            identifiers.append(identifier)
        if args.agent:
            for action in enable_items(
                store, agents, registry, identifiers, args.agent, args.dry_run
            ):
                print(action)
        if not args.dry_run:
            atomic_write_json(store.registry_path, registry)
        print(
            f"registered {len(identifiers)} skill(s) from {source_name}"
            + (" (dry run)" if args.dry_run else "")
        )


def cmd_enable_disable(args: argparse.Namespace, store: Store, enable: bool) -> None:
    with root_lock(store.root):
        store.require_init()
        registry = store.registry()
        agents = store.agents()
        ids = resolve_item_ids(registry, args.items)
        if not args.agent and not args.all:
            raise AtError("pass at least one --agent or use --all")
        if args.agent and args.all:
            raise AtError("choose either --agent or --all")

        for identifier in ids:
            item = registry["items"][identifier]
            if args.all:
                if enable:
                    selected = sorted(
                        agent
                        for agent, config in agents["agents"].items()
                        if config.get(item["kind"])
                    )
                else:
                    selected = list(item.get("enabled", []))
            else:
                selected = args.agent
            if not selected:
                print(f"current  {'all':<10} {identifier}: no matching agents")
                continue
            actions = (
                enable_items(
                    store, agents, registry, [identifier], selected, args.dry_run
                )
                if enable
                else disable_items(
                    store, agents, registry, [identifier], selected, args.dry_run
                )
            )
            for action in actions:
                print(action)
        if not args.dry_run:
            atomic_write_json(store.registry_path, registry)


def cmd_update(args: argparse.Namespace, store: Store) -> None:
    with root_lock(store.root):
        store.require_init()
        registry = store.registry()
        names = args.sources or sorted(registry["sources"])
        for name in names:
            source = registry["sources"].get(name)
            if source is None:
                raise AtError(f"unknown source {name!r}")
            status, detail = update_source(store, name, source)
            print(f"{status:<8} {name}: {detail}")
        atomic_write_json(store.registry_path, registry)


def cmd_sync(args: argparse.Namespace, store: Store) -> None:
    with root_lock(store.root):
        store.require_init()
        registry = store.registry()
        agents = store.agents()
        restored = 0
        for source_name, source in sorted(registry["sources"].items()):
            restored += int(restore_source(store, source_name, source, args.dry_run))
        all_plans: list[tuple[str, str, Path, Path]] = []
        for identifier, item in registry["items"].items():
            enabled = item.get("enabled", [])
            if enabled:
                all_plans.extend(
                    plan_links(
                        store,
                        agents,
                        registry,
                        [identifier],
                        enabled,
                        allow_missing=args.dry_run,
                    )
                )
        for identifier, agent, source, target in all_plans:
            if target.exists() or target.is_symlink():
                if not symlink_points_to(target, source):
                    raise AtError(f"refusing to replace unmanaged path: {target}")
        count = 0
        for identifier, agent, source, target in all_plans:
            if target.is_symlink():
                continue
            print(f"enable   {agent:<10} {identifier} -> {target}")
            if not args.dry_run:
                target.parent.mkdir(parents=True, exist_ok=True)
                os.symlink(os.path.relpath(source, target.parent), target)
            count += 1
        print(
            f"synced {restored} source(s), {count} missing link(s)"
            + (" (dry run)" if args.dry_run else "")
        )


def cmd_status(args: argparse.Namespace, store: Store) -> None:
    registry = store.registry()
    rows = []
    for identifier, item in sorted(registry["items"].items()):
        rows.append(
            [
                identifier,
                item["source"],
                item["path"],
                ",".join(item.get("enabled", [])) or "-",
            ]
        )
    print_table(["ITEM", "SOURCE", "PATH", "ENABLED"], rows)


def cmd_doctor(args: argparse.Namespace, store: Store) -> None:
    registry = store.registry()
    agents = store.agents()
    problems: list[str] = []
    checks = 0
    for source_name, source in registry["sources"].items():
        checks += 1
        root = source_root(store, source)
        if not root.is_dir():
            problems.append(f"missing source {source_name}: {root}")
    for identifier, item in registry["items"].items():
        checks += 1
        try:
            source = item_path(store, registry, item)
            if not source.exists():
                problems.append(f"missing item {identifier}: {source}")
            for agent in item.get("enabled", []):
                for directory in agent_targets(agents, agent, item["kind"]):
                    checks += 1
                    target = directory / item["target_name"]
                    if not target.is_symlink():
                        problems.append(
                            f"missing link {identifier} for {agent}: {target}"
                        )
                    elif not symlink_points_to(target, source):
                        problems.append(
                            f"wrong link {identifier} for {agent}: {target}"
                        )
        except AtError as exc:
            problems.append(f"{identifier}: {exc}")
    if problems:
        for problem in problems:
            print(f"ERROR {problem}")
        raise AtError(f"doctor found {len(problems)} problem(s)")
    print(f"ok: {checks} checks")


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="atctl",
        description="Manage agent skills, prompts, and extensions with canonical sources and symlinks.",
    )
    parser.add_argument(
        "--home",
        default=os.environ.get("AT_HOME", "~/.at"),
        help="manager directory (default: ~/.at or AT_HOME)",
    )
    sub = parser.add_subparsers(dest="command", required=True)

    sub.add_parser("init", help="create agents.json, registry.json, and sources/")

    agent = sub.add_parser("agent", help="manage agent target directories")
    agent_sub = agent.add_subparsers(dest="agent_command", required=True)
    agent_sub.add_parser("ls", aliases=["list"])
    for command in ("add", "rm"):
        p = agent_sub.add_parser(command)
        p.add_argument("agent")
        p.add_argument("kind")
        p.add_argument("path")

    source = sub.add_parser("source", help="manage local and Git sources")
    source_sub = source.add_subparsers(dest="source_command", required=True)
    source_sub.add_parser("ls", aliases=["list"])
    p = source_sub.add_parser("add")
    p.add_argument("name")
    p.add_argument(
        "source", help="local directory, owner/repo, github:owner/repo, or Git URL"
    )
    p.add_argument("--ref", help="branch, tag, or commit")
    p = source_sub.add_parser("rm")
    p.add_argument("name")
    p.add_argument(
        "--delete-checkout",
        action="store_true",
        help="also delete a managed Git checkout",
    )

    scan = sub.add_parser(
        "scan", help="find SKILL.md directories in a registered source"
    )
    scan.add_argument("source")
    scan.add_argument("--under", help="scan only this relative directory")

    install = sub.add_parser(
        "install", help="register a source and its discovered skills"
    )
    install.add_argument("source")
    install.add_argument("--source-name")
    install.add_argument("--ref")
    install.add_argument("--under", help="scan only this relative directory")
    install.add_argument(
        "--skill", action="append", help="register only this skill; repeatable"
    )
    install.add_argument(
        "--agent", action="append", help="enable for this agent; repeatable"
    )
    install.add_argument("--dry-run", action="store_true")

    item = sub.add_parser("item", help="manage registered items")
    item_sub = item.add_subparsers(dest="item_command", required=True)
    item_sub.add_parser("ls", aliases=["list"])
    p = item_sub.add_parser("add")
    p.add_argument("kind")
    p.add_argument("name")
    p.add_argument("--source", required=True)
    p.add_argument("--path", required=True)
    p.add_argument("--target-name")
    p.add_argument("--id")
    p.add_argument("--agent", action="append")
    p.add_argument("--dry-run", action="store_true")
    p = item_sub.add_parser("rm")
    p.add_argument("items", nargs="+")
    p.add_argument("--disable", action="store_true")
    p.add_argument("--dry-run", action="store_true")

    for command in ("enable", "disable"):
        p = sub.add_parser(command, help=f"{command} items for selected agents")
        p.add_argument("items", nargs="+")
        p.add_argument("--agent", action="append")
        p.add_argument(
            "--all",
            action="store_true",
            help="apply to every compatible/currently enabled agent",
        )
        p.add_argument("--dry-run", action="store_true")

    update = sub.add_parser(
        "update", help="fetch and check out latest configured Git refs"
    )
    update.add_argument("sources", nargs="*")
    sync = sub.add_parser(
        "sync", help="restore missing links declared in registry.json"
    )
    sync.add_argument("--dry-run", action="store_true")
    sub.add_parser(
        "status", aliases=["ls"], help="show registered items and enablement"
    )
    sub.add_parser("doctor", help="validate sources, items, and enabled links")
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    store = Store(expand(args.home))
    try:
        if args.command == "init":
            cmd_init(args, store)
        elif args.command == "agent":
            cmd_agent(args, store)
        elif args.command == "source":
            cmd_source(args, store)
        elif args.command == "scan":
            cmd_scan(args, store)
        elif args.command == "item":
            cmd_item(args, store)
        elif args.command == "install":
            cmd_install(args, store)
        elif args.command == "enable":
            cmd_enable_disable(args, store, True)
        elif args.command == "disable":
            cmd_enable_disable(args, store, False)
        elif args.command == "update":
            cmd_update(args, store)
        elif args.command == "sync":
            cmd_sync(args, store)
        elif args.command in {"status", "ls"}:
            cmd_status(args, store)
        elif args.command == "doctor":
            cmd_doctor(args, store)
        else:  # pragma: no cover
            parser.error(f"unknown command: {args.command}")
    except AtError as exc:
        eprint(f"error: {exc}")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
