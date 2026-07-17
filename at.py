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


def die(msg: str) -> "NoReturn":  # type: ignore[name-defined]
    print(f"error: {msg}", file=sys.stderr)
    sys.exit(1)


def expand(path: str | Path) -> Path:
    return Path(os.path.expandvars(os.path.expanduser(str(path))))


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
        # NOTE: agents.json is hand-edited by the user; agentfiles never writes it.
        if not self.registry_path.exists():
            write_json(self.registry_path, {"schema": SCHEMA, "sources": {}, "items": {}})
        if not self.agents_path.exists():
            die(f"agents.json not found in {self.root}; copy agents.example.json there "
                f"(agentfiles never writes agents.json)")

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
        return data

    def registry(self) -> dict:
        self.require_init()
        data = read_json(self.registry_path, {"schema": SCHEMA, "sources": {}, "items": {}})
        data.setdefault("schema", SCHEMA)
        data.setdefault("sources", {})
        data.setdefault("items", {})
        return data

    # Disabled: agentfiles never writes agents.json (hand-edited by the user).
    def save_agents(self, data: dict) -> None:
        write_json(self.agents_path, data)
    ###

    def save_registry(self, data: dict) -> None:
        write_json(self.registry_path, data)


# --- git / source helpers ----------------------------------------------------

def git(*args: str, cwd: Path | None = None, capture: bool = False) -> str:
    cmd = ["git", "-c", "core.hooksPath=/dev/null"]
    if cwd is not None:
        cmd += ["-C", str(cwd)]
    cmd += list(args)
    try:
        res = subprocess.run(cmd, check=True, text=True,
                             stdout=subprocess.PIPE if capture else None,
                             stderr=subprocess.PIPE if capture else None)
    except FileNotFoundError:
        die("git is required for git sources")
    except subprocess.CalledProcessError as exc:
        detail = (exc.stderr or exc.stdout or "").strip()
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
    raw = re.sub(r"[^A-Za-z0-9._-]+", "-", Path(value).name.removesuffix(".git")) or "source"
    return raw[1:] if raw and not raw[0].isalnum() else raw


def source_root(store: Store, source: dict) -> Path:
    if source["type"] == "git":
        return (store.root / source["checkout"]).resolve()
    return expand(source["path"])


def item_path(store: Store, source: dict, subpath: str) -> Path:
    return (source_root(store, source) / subpath).resolve()


# --- symlink helpers ---------------------------------------------------------

def link_targets(store: Store, agents: dict, item: dict, agent: str) -> list[Path]:
    cfg = agents["agents"].get(agent)
    if not cfg:
        die(f"unknown agent {agent!r}; edit agents.json")
    dirs = cfg.get(KINDS[item["type"]], [])
    if not dirs:
        die(f"agent {agent!r} has no {KINDS[item['type']]} directory configured")
    return [expand(d) / item["slug"] for d in dirs]


def points_to(link: Path, target: Path) -> bool:
    if not link.is_symlink():
        return False
    raw = os.readlink(link)
    actual = (link.parent / raw).resolve() if not Path(raw).is_absolute() else Path(raw).resolve()
    return actual == target.resolve()


def make_link(target: Path, source: Path) -> bool:
    if target.is_symlink():
        if points_to(target, source):
            return False
        die(f"refusing to replace unmanaged path: {target}")
    if target.exists():
        die(f"refusing to replace non-symlink path: {target}")
    target.parent.mkdir(parents=True, exist_ok=True)
    os.symlink(os.path.relpath(source, target.parent), target)
    return True


def drop_link(target: Path, source: Path) -> None:
    if not target.is_symlink() or not points_to(target, source):
        return
    target.unlink()


# --- commands ----------------------------------------------------------------

def cmd_init(args, store: Store) -> None:
    store.init()
    print(f"initialized {store.root}")
    print(f"  agents:   {store.agents_path}")
    print(f"  registry: {store.registry_path}")


def cmd_agents(args, store: Store) -> None:
    data = store.agents()
    if args.agent_command == "ls":
        rows = []
        for agent, cfg in sorted(data["agents"].items()):
            for kind in ("skills", "prompts", "extensions"):
                for p in cfg.get(kind, []):
                    rows.append([agent, kind, p])
        table(["AGENT", "KIND", "DIRECTORY"], rows)
        return
    # Disabled: agentfiles never writes agents.json (hand-edited by the user).
    validate_name(args.agent, "agent")
    kind = KINDS[canonical_type(args.kind)]
    cfg = data["agents"].setdefault(args.agent, {})
    paths = cfg.setdefault(kind, [])
    if args.agent_command == "add":
        if args.path not in paths:
            paths.append(args.path)
            paths.sort()
            store.save_agents(data)
        print(f"agent {args.agent}: {kind} -> {args.path}")
    else:
        if args.path not in paths:
            die(f"path not configured: {args.path}")
        paths.remove(args.path)
        if not paths:
            cfg.pop(kind, None)
        if not cfg:
            data["agents"].pop(args.agent, None)
        store.save_agents(data)
        print(f"agent {args.agent}: removed {kind} -> {args.path}")
    ###
    die("agentfiles never writes agents.json; edit it by hand (see agents.example.json)")


def add_source(store: Store, registry: dict, name: str, value: str, ref: str | None) -> str:
    name = name or default_name(value)
    validate_name(name, "source name")
    local = expand(value)
    # A local directory always wins, even if it looks like a git URL pattern.
    if local.exists():
        if name in registry["sources"] and registry["sources"][name].get("path") == str(local.resolve()):
            return name
        registry["sources"][name] = {"type": "local", "path": str(local.resolve()), "added_at": now()}
        return name
    if not is_git(value):
        die(f"not a local path or git source: {value}")
    url = normalize_git(value)
    if name in registry["sources"] and registry["sources"][name].get("url") == url:
        return name
    dest = store.sources_dir / name
    if dest.exists():
        die(f"source checkout already exists: {dest}")
    dest.parent.mkdir(parents=True, exist_ok=True)
    tmp = store.sources_dir / f".{name}.tmp-{os.getpid()}"
    shutil.rmtree(tmp, ignore_errors=True)
    try:
        git("clone", "--depth", "1", url, str(tmp))
        if ref:
            git("checkout", ref, cwd=tmp)
        os.replace(tmp, dest)
    except SystemExit:
        shutil.rmtree(tmp, ignore_errors=True)
        raise
    registry["sources"][name] = {
        "type": "git", "url": url, "ref": ref or "",
        "checkout": str(dest.relative_to(store.root)),
        "added_at": now(),
    }
    return name


def cmd_add(args, store: Store) -> None:
    registry = store.registry()
    parts = args.parts
    if parts[0].lower() in TYPE_ALIASES:        # leading kind: add skill name src subpath
        typ = canonical_type(parts[0])
        rest = parts[1:]
    else:                                       # kind via --type: add name src subpath
        typ = canonical_type(args.type)
        rest = parts
    if len(rest) < 2:
        die("usage: at add [<kind>] <slug> <source> [subpath]")
    slug = validate_name(rest[0], "slug")
    source = rest[1]
    subpath = (args.subpath or (rest[2] if len(rest) > 2 else ".")).strip("/")
    source_name = add_source(store, registry, args.source_name or "", source, args.ref)
    source = registry["sources"][source_name]
    path = item_path(store, source, subpath)
    if not path.exists():
        die(f"item path does not exist: {path}")
    if typ == "skill" and not (path.is_dir() and (path / "SKILL.md").is_file()):
        die(f"skill must be a directory containing SKILL.md: {path}")
    registry["items"][slug] = {
        "slug": slug, "type": typ, "source": source_name,
        "path": subpath, "enabled": [], "added_at": now(),
    }
    store.save_registry(registry)
    print(f"added {slug} ({typ}) from source {source_name}" + (f" at {subpath}" if subpath != "." else ""))
    if args.agent:
        apply_enable(store, registry, store.agents(), slug, args.agent, True)


def cmd_scan(args, store: Store) -> None:
    registry = store.registry()
    if args.slug not in registry["items"]:
        die(f"unknown item {args.slug!r}; run 'at add' first")
    item = registry["items"][args.slug]
    root = source_root(store, registry["sources"][item["source"]])
    scan_root = (root / item["path"] if item["path"] != "." else root)
    rows = [[name, rel] for name, rel in discover_skills(scan_root, args.under)]
    table(["SKILL", "PATH"], rows)
    if not rows:
        print("(none found)")


def discover_skills(root: Path, under: str | None = None) -> list[tuple[str, str]]:
    scan = (root / under) if under else root
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
    if args.all:
        if not args.enable:
            return list(item["enabled"])
        return sorted(a for a, c in agents["agents"].items() if c.get(KINDS[item["type"]]))
    if not names:
        die("pass an agent name or --all")
    return names


def apply_enable(store, registry, agents, slug, agents_list, enable):
    item = registry["items"][slug]
    source = item_path(store, registry["sources"][item["source"]], item["path"])
    for agent in agents_list:
        for target in link_targets(store, agents, item, agent):
            if enable:
                make_link(target, source)
                print(f"enabled  {agent:<12} {slug} -> {target}")
            else:
                drop_link(target, source)
                print(f"disabled {agent:<12} {slug} -> {target}")
    enabled = set(item["enabled"])
    enabled.update(agents_list) if enable else enabled.difference_update(agents_list)
    item["enabled"] = sorted(enabled)
    store.save_registry(registry)


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
        git("fetch", "--prune", "origin", cwd=root)
        ref = source.get("ref")
        if ref:
            git("checkout", ref, cwd=root)
        else:
            git("pull", "--ff-only", cwd=root)
        after = git("rev-parse", "HEAD^{commit}", cwd=root, capture=True)
        print(f"{'updated' if before != after else 'current':<8} {name} ({after[:12]})")


def cmd_remove(args, store: Store) -> None:
    registry = store.registry()
    agents = store.agents()
    if args.slug not in registry["items"]:
        die(f"unknown item {args.slug!r}")
    item = registry["items"].pop(args.slug)
    source = registry["sources"].get(item["source"])
    src = item_path(store, source, item["path"]) if source else None
    for agent in item["enabled"]:
        for target in link_targets(store, agents, item, agent):
            if src:
                drop_link(target, src)
            print(f"removed link {agent}/{item['slug']}")
    # drop the source too if nothing else uses it
    if source and not any(i["source"] == item["source"] for i in registry["items"].values()):
        registry["sources"].pop(item["source"], None)
        if source["type"] == "git" and (store.root / source["checkout"]).exists():
            shutil.rmtree(store.root / source["checkout"])
            print(f"removed source checkout {item['source']}")
    store.save_registry(registry)
    print(f"removed {args.slug}")
    # if source and not any(i["source"] == item["source"] for i in registry["items"].values()):
    #     registry["sources"].pop(item["source"], None)
    #     if source["type"] == "git":
    #         checkout = (store.root / source["checkout"]).resolve()
    #         # ponytail: only ever delete a manager-owned clone directly under sources/
    #         if checkout.is_dir() and checkout.parent == store.sources_dir.resolve():
    #             shutil.rmtree(checkout)
    #             print(f"removed source checkout {item['source']}")
    # store.save_registry(registry)
    # print(f"removed {args.slug}")



def cmd_list(args, store: Store) -> None:
    registry = store.registry()
    rows = [[k, v["type"], v["source"], ",".join(v.get("enabled", [])) or "-"]
            for k, v in sorted(registry["items"].items())]
    table(["SLUG", "TYPE", "SOURCE", "ENABLED"], rows)


def cmd_status(args, store: Store) -> None:
    cmd_list(args, store)
    registry = store.registry()
    agents = store.agents()
    print("\ndrift (filesystem vs registry):")
    problems = 0
    for slug, item in registry["items"].items():
        source = item_path(store, registry["sources"][item["source"]], item["path"])
        for agent in item["enabled"]:
            for target in link_targets(store, agents, item, agent):
                if not target.is_symlink():
                    print(f"  MISSING LINK: {agent}/{slug}")
                    problems += 1
                elif not points_to(target, source):
                    print(f"  WRONG LINK:   {agent}/{slug}")
                    problems += 1
    print("  none" if not problems else f"  {problems} problem(s)")


def cmd_sync(args, store: Store) -> None:
    registry = store.registry()
    agents = store.agents()
    links = 0
    for item in registry["items"].values():
        source = item_path(store, registry["sources"][item["source"]], item["path"])
        for agent in item["enabled"]:
            for target in link_targets(store, agents, item, agent):
                if make_link(target, source):
                    links += 1
        for agent in set(agents["agents"]) - set(item["enabled"]):
            if not agents["agents"][agent].get(KINDS[item["type"]]):
                continue  # agent has no directory for this kind; nothing to unlink
            for target in link_targets(store, agents, item, agent):
                drop_link(target, source)
    print(f"synced {links} link(s) to registry")


def cmd_doctor(args, store: Store) -> None:
    registry = store.registry()
    agents = store.agents()
    problems = 0
    for name, source in registry["sources"].items():
        if not source_root(store, source).is_dir():
            print(f"ERROR missing source {name}")
            problems += 1
    for slug, item in registry["items"].items():
        source = registry["sources"].get(item["source"])
        if source is None:
            print(f"ERROR {slug}: references unknown source")
            problems += 1
            continue
        root = item_path(store, source, item["path"])
        if not root.exists():
            print(f"ERROR missing item {slug}: {root}")
            problems += 1
        for agent in item["enabled"]:
            for target in link_targets(store, agents, item, agent):
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

    sub.add_parser("init", help="create ~/.agentfiles, agents.json, registry.json, sources/")
    sub.add_parser("list", help="list registered items")

    a = sub.add_parser("agents", help="manage agent discovery directories")
    a_sub = a.add_subparsers(dest="agent_command", required=True)
    a_sub.add_parser("ls", aliases=["list"])
    # Disabled: agentfiles never writes agents.json (hand-edited by the user).
    for c in ("add", "rm"):
        ap = a_sub.add_parser(c)
        ap.add_argument("kind", help="skills | prompts | extensions")
        ap.add_argument("path", help="discovery directory")
        ap.add_argument("--agent", required=True, help="agent name")
    ###

    add = sub.add_parser("add", help="register a source + item, optionally enable it")
    add.add_argument("parts", nargs="+",
                     help="[<kind>] <slug> <source> [subpath]  (kind: skill|prompt|extension; optional)")
    add.add_argument("--subpath", help="path within the source (default: root); alt. to positional subpath")
    add.add_argument("--type", default="skill", help="default kind when omitted as first arg")
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
