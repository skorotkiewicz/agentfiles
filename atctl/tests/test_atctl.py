from __future__ import annotations

import contextlib
import importlib.util
import io
import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace

ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location("atctl_module", ROOT / "atctl.py")
assert SPEC and SPEC.loader
atctl = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(atctl)


class AtctlTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.base = Path(self.temp.name)
        self.home = self.base / "at"
        self.agent_dir = self.base / "agent-skills"
        self.catalog = self.base / "catalog"
        skill = self.catalog / "skills" / "demo"
        skill.mkdir(parents=True)
        (skill / "SKILL.md").write_text(
            "---\nname: demo\ndescription: demo\n---\n", encoding="utf-8"
        )

    def tearDown(self) -> None:
        self.temp.cleanup()

    def run_cli(self, *args: str, ok: bool = True) -> SimpleNamespace:
        stdout = io.StringIO()
        stderr = io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            returncode = atctl.main(["--home", str(self.home), *args])
        result = SimpleNamespace(
            returncode=returncode,
            stdout=stdout.getvalue(),
            stderr=stderr.getvalue(),
        )
        if ok and result.returncode != 0:
            self.fail(
                f"command failed: {args}\nstdout={result.stdout}\nstderr={result.stderr}"
            )
        return result

    def bootstrap(self) -> None:
        self.run_cli("init")
        agents_path = self.home / "agents.json"
        agents = json.loads(agents_path.read_text())
        agents["agents"] = {"test": {"skills": [str(self.agent_dir)]}}
        agents_path.write_text(json.dumps(agents), encoding="utf-8")
        self.run_cli("source", "add", "local", str(self.catalog))
        self.run_cli(
            "item", "add", "skill", "demo", "--source", "local", "--path", "skills/demo"
        )

    def test_enable_disable_roundtrip(self) -> None:
        self.bootstrap()
        self.run_cli("enable", "demo", "--agent", "test")
        link = self.agent_dir / "demo"
        self.assertTrue(link.is_symlink())
        self.assertEqual(link.resolve(), self.catalog / "skills" / "demo")
        self.run_cli("doctor")
        self.run_cli("disable", "demo", "--agent", "test")
        self.assertFalse(link.exists())
        registry = json.loads((self.home / "registry.json").read_text())
        self.assertEqual(registry["items"]["skill:demo"]["enabled"], [])

    def test_refuses_to_replace_real_file(self) -> None:
        self.bootstrap()
        self.agent_dir.mkdir(parents=True)
        (self.agent_dir / "demo").write_text("mine", encoding="utf-8")
        result = self.run_cli("enable", "demo", "--agent", "test", ok=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("unmanaged path", result.stderr)
        self.assertEqual((self.agent_dir / "demo").read_text(), "mine")

    def test_sync_restores_missing_link(self) -> None:
        self.bootstrap()
        self.run_cli("enable", "demo", "--agent", "test")
        link = self.agent_dir / "demo"
        link.unlink()
        self.run_cli("sync")
        self.assertTrue(link.is_symlink())

    def test_install_discovers_skill(self) -> None:
        self.run_cli("init")
        self.run_cli("install", str(self.catalog), "--source-name", "catalog")
        registry = json.loads((self.home / "registry.json").read_text())
        self.assertIn("skill:demo", registry["items"])

    def test_disable_all(self) -> None:
        self.bootstrap()
        self.run_cli("enable", "demo", "--agent", "test")
        self.run_cli("disable", "demo", "--all")
        self.assertFalse((self.agent_dir / "demo").exists())

    def test_git_source_update(self) -> None:
        upstream = self.base / "upstream"
        remote = self.base / "remote.git"
        skill = upstream / "skills" / "git-demo"
        skill.mkdir(parents=True)
        (skill / "SKILL.md").write_text(
            "---\nname: git-demo\ndescription: demo\n---\n", encoding="utf-8"
        )
        subprocess.run(["git", "-C", str(upstream), "init", "-q"], check=True)
        subprocess.run(
            ["git", "-C", str(upstream), "config", "user.email", "test@example.com"],
            check=True,
        )
        subprocess.run(
            ["git", "-C", str(upstream), "config", "user.name", "test"], check=True
        )
        subprocess.run(["git", "-C", str(upstream), "add", "."], check=True)
        subprocess.run(["git", "-C", str(upstream), "commit", "-qm", "one"], check=True)
        subprocess.run(["git", "-C", str(upstream), "branch", "-M", "main"], check=True)
        subprocess.run(
            ["git", "clone", "-q", "--bare", str(upstream), str(remote)], check=True
        )
        subprocess.run(
            ["git", "-C", str(remote), "symbolic-ref", "HEAD", "refs/heads/main"],
            check=True,
        )

        self.run_cli("init")
        self.run_cli("install", f"file://{remote}", "--source-name", "remote")
        registry_path = self.home / "registry.json"
        before = json.loads(registry_path.read_text())["sources"]["remote"]["revision"]

        (skill / "SKILL.md").write_text(
            "---\nname: git-demo\ndescription: updated\n---\n", encoding="utf-8"
        )
        subprocess.run(["git", "-C", str(upstream), "add", "."], check=True)
        subprocess.run(["git", "-C", str(upstream), "commit", "-qm", "two"], check=True)
        subprocess.run(
            ["git", "-C", str(upstream), "remote", "add", "origin", str(remote)],
            check=True,
        )
        subprocess.run(
            ["git", "-C", str(upstream), "push", "-q", "origin", "main"], check=True
        )

        self.run_cli("update", "remote")
        registry = json.loads(registry_path.read_text())
        after = registry["sources"]["remote"]["revision"]
        self.assertNotEqual(before, after)

        checkout = self.home / registry["sources"]["remote"]["checkout"]
        subprocess.run(["rm", "-rf", str(checkout)], check=True)
        dry = self.run_cli("sync", "--dry-run")
        self.assertIn("restore", dry.stdout)
        self.assertFalse(checkout.exists())
        self.run_cli("sync")
        self.assertTrue((checkout / "skills" / "git-demo" / "SKILL.md").is_file())
        restored = subprocess.run(
            ["git", "-C", str(checkout), "rev-parse", "HEAD"],
            check=True,
            text=True,
            stdout=subprocess.PIPE,
        ).stdout.strip()
        self.assertEqual(restored, after)

    def test_disable_refuses_wrong_symlink(self) -> None:
        self.bootstrap()
        self.run_cli("enable", "demo", "--agent", "test")
        link = self.agent_dir / "demo"
        link.unlink()
        other = self.base / "other"
        other.mkdir()
        os.symlink(other, link)
        result = self.run_cli("disable", "demo", "--agent", "test", ok=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertTrue(link.is_symlink())
        self.assertEqual(link.resolve(), other)


if __name__ == "__main__":
    unittest.main()
