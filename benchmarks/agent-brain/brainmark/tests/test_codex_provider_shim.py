"""The distill provider shim, EXECUTED -- not just generated.

entire-brain's distill runs `codex exec --ignore-user-config` with no provider
config of its own, so on an unaided machine it authenticates against public
OpenAI instead of the pinned Azure deployment. That failure is SILENT: distill
succeeds and writes facts from the wrong model on the wrong account.

So the shim is not asserted as a string here. A fake `codex` records the argv it
is handed, the shim is really run, and the recorded argv is what is checked --
including the branch that must NOT inject, because a shim that rewrote a
measured BrainMark session would be an arm-asymmetric change to the thing under
measurement.
"""

from __future__ import annotations

import os
import pathlib
import stat
import subprocess
import sys
import tempfile
import textwrap
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))

from brainmark import agents  # noqa: E402
from brainmark.agents import codex_provider_shim as shim  # noqa: E402
from brainmark.agents.codex_adapter import CodexAdapter  # noqa: E402
from brainmark.memsources import full_brain  # noqa: E402

AZURE = {
    "base_url": "https://example-resource.services.ai.azure.com/openai/v1",
    "api_key": "sk-test-not-real",
    "api_key_env": "AZURE_AI_API_KEY",
    "provider_key": "azure",
    "wire_api": "responses",
}

FAKE_CODEX = textwrap.dedent("""\
    #!/usr/bin/env bash
    # Records the argv it was handed, one per line, then exits 0.
    : > "$SHIM_TEST_ARGV"
    for a in "$@"; do printf '%s\\n' "$a" >> "$SHIM_TEST_ARGV"; done
    printf '%s\\n' "KEY=${AZURE_AI_API_KEY:-UNSET}" >> "$SHIM_TEST_ARGV"
    printf '%s\\n' "MARKER=${BRAINMARK_CODEX_SHIM:-UNSET}" >> "$SHIM_TEST_ARGV"
""")


class ProviderShimTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        root = pathlib.Path(self.tmp.name)

        self.realdir = root / "realbin"
        self.realdir.mkdir()
        self.real = self.realdir / "codex"
        self.real.write_text(FAKE_CODEX, encoding="utf-8")
        self.real.chmod(self.real.stat().st_mode | stat.S_IXUSR)

        self.shim_dir = root / "shim"
        self.argv_log = root / "argv.txt"
        self.env = {"PATH": f"{self.realdir}{os.pathsep}/usr/bin:/bin",
                    "SHIM_TEST_ARGV": str(self.argv_log)}

    def tearDown(self) -> None:
        self.tmp.cleanup()

    # -- helpers ---------------------------------------------------------
    def _install(self) -> tuple[dict[str, str], dict]:
        return shim.install(self.shim_dir, AZURE, self.env)

    def _run(self, env: dict[str, str], *argv: str) -> list[str]:
        subprocess.run([str(self.shim_dir / "codex"), *argv],
                       check=True, capture_output=True, env=env)
        return self.argv_log.read_text(encoding="utf-8").splitlines()

    # -- the injecting branch --------------------------------------------
    def test_an_unconfigured_exec_gets_the_pinned_provider(self):
        """distill's shape: `codex exec --ignore-user-config <prompt>`."""
        env, prov = self._install()
        recorded = self._run(env, "exec", "--ignore-user-config", "PROMPT")
        joined = " ".join(recorded)

        self.assertEqual(recorded[0], "exec", "the subcommand must survive")
        self.assertIn('model_provider="azure"', joined)
        self.assertIn(f'model_providers.azure.base_url="{AZURE["base_url"]}"', joined)
        self.assertIn('model_providers.azure.env_key="AZURE_AI_API_KEY"', joined)
        self.assertIn('model_providers.azure.wire_api="responses"', joined)
        # The caller's own flags and prompt are preserved, and the prompt stays last.
        self.assertIn("--ignore-user-config", recorded)
        self.assertEqual(recorded[recorded.index("PROMPT")], "PROMPT")

        # The credential reaches the child even though the harness env sanitizer
        # strips it -- that is the point of exporting it from the shim.
        self.assertIn(f"KEY={AZURE['api_key']}", recorded)
        self.assertIn("MARKER=1", recorded)
        self.assertEqual(prov["real_codex"], str(self.real.resolve()))

    def test_the_injected_config_equals_what_a_measured_session_uses(self):
        """ONE source of truth: the shim must not drift from the adapter.

        If these diverge, distill runs against a different endpoint than the
        sessions whose transcript it is distilling -- and nothing would fail.
        """
        env, _ = self._install()
        recorded = self._run(env, "exec", "PROMPT")

        adapter = CodexAdapter(agents.load_backends()["backends"]["codex"], {})
        expected = [a for a in adapter.azure_config_args(AZURE) if a != "--config"]
        for setting in expected:
            self.assertIn(setting, recorded, f"shim omits {setting}")

    # -- the pass-through branches ---------------------------------------
    def test_a_caller_that_configures_a_provider_is_untouched(self):
        """Every measured BrainMark session takes this branch.

        `CodexAdapter.build_command()` always passes model_provider=, so the
        shim must be a no-op for the sessions under measurement -- otherwise it
        is a change to the thing being measured.
        """
        env, _ = self._install()
        argv = ["exec", "--config", 'model_provider="azure"',
                "--config", 'model_providers.azure.base_url="https://caller/v1"',
                "PROMPT"]
        recorded = self._run(env, *argv)
        self.assertEqual(recorded[:len(argv)], argv, "argv was rewritten")
        self.assertEqual(recorded.count('model_provider="azure"'), 1,
                         "the provider was injected a SECOND time")
        self.assertNotIn(f'model_providers.azure.base_url="{AZURE["base_url"]}"',
                         recorded, "the shim overrode the caller's endpoint")
        self.assertIn("MARKER=UNSET", recorded, "the injecting branch ran anyway")

    def test_non_exec_subcommands_pass_through(self):
        env, _ = self._install()
        for argv in (["--version"], ["login", "status"], []):
            with self.subTest(argv=argv):
                recorded = self._run(env, *argv)
                self.assertEqual([r for r in recorded if not r.startswith(("KEY=", "MARKER="))],
                                 argv)
                self.assertIn("MARKER=UNSET", recorded)

    # -- safety ----------------------------------------------------------
    def test_the_shim_never_resolves_to_itself(self):
        """Self-resolution would fork-bomb, and a hang is not a test failure."""
        env, _ = self._install()
        # PATH now has the shim dir FIRST; find_real_codex must skip it.
        self.assertEqual(shim.find_real_codex(self.shim_dir, env),
                         self.real.resolve())

    def test_a_missing_codex_is_a_loud_error(self):
        with self.assertRaises(agents.AgentBackendError):
            shim.find_real_codex(self.shim_dir, {"PATH": str(self.shim_dir)})

    def test_install_puts_the_shim_first_on_path(self):
        env, prov = self._install()
        self.assertEqual(env["PATH"].split(os.pathsep)[0], str(self.shim_dir))
        self.assertEqual(prov["shim_path"], str(self.shim_dir / "codex"))
        self.assertEqual(len(prov["shim_sha256"]), 64)
        self.assertIn("distill.go:117", prov["why"])
        # The caller's env must not be mutated in place.
        self.assertNotIn(str(self.shim_dir), self.env["PATH"])

    def test_the_shim_is_executable(self):
        shim.write_shim(self.shim_dir, AZURE, env=self.env)
        self.assertTrue(os.access(self.shim_dir / "codex", os.X_OK))


class DistillShimWiringTest(unittest.TestCase):
    """full_brain installs the shim for codex distills and for nothing else."""

    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.tmp.name)

    def tearDown(self) -> None:
        self.tmp.cleanup()

    def test_a_non_codex_distill_pin_installs_nothing(self):
        env = {"PATH": "/usr/bin"}
        out_env, prov = full_brain.install_distill_provider_shim(
            self.root, {"agent": "claude-code", "model": "m"}, env)
        self.assertIsNone(prov)
        self.assertEqual(out_env, env)
        self.assertFalse((self.root / "codex-shim").exists())

    def test_a_codex_distill_pin_installs_the_shim(self):
        realdir = self.root / "realbin"
        realdir.mkdir()
        real = realdir / "codex"
        real.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
        real.chmod(real.stat().st_mode | stat.S_IXUSR)
        env = {
            "PATH": f"{realdir}{os.pathsep}/usr/bin",
            "AZURE_AI_ENDPOINT": "https://example-resource.services.ai.azure.com",
            "AZURE_AI_API_KEY": "sk-test-not-real",
        }

        out_env, prov = full_brain.install_distill_provider_shim(
            self.root, {"agent": "codex", "model": "gpt-5.6-sol"}, env)

        self.assertIsNotNone(prov)
        self.assertEqual(out_env["PATH"].split(os.pathsep)[0],
                         str(self.root / "codex-shim"))
        # Built from backends.json, so it carries the Foundry URL shape and no
        # api-version -- the same pin the measured sessions run on.
        self.assertTrue(prov["base_url"].endswith("/openai/v1"), prov["base_url"])
        self.assertNotIn("api-version",
                         (self.root / "codex-shim" / "codex").read_text(encoding="utf-8"))


if __name__ == "__main__":
    unittest.main()
