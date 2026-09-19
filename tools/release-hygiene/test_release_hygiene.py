"""Regression tests for the release path.

Both of these encode a defect the agent review found on the pull request that
introduced the release workflow and the standalone installer. They are written
to fail if either defect is reintroduced, not to describe the fix.
"""

from __future__ import annotations

import os
import re
import shutil
import stat
import subprocess
import tempfile
import textwrap
import unittest
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
WORKFLOWS = REPO / ".github" / "workflows"
INSTALLER = REPO / "scripts" / "get-brain.sh"

# A run: block, and the indented body that belongs to it.
RUN_BLOCK = re.compile(r"^(?P<indent> +)run: \|\s*\n(?P<body>(?:(?P=indent) .*\n|[ \t]*\n)*)", re.M)


# Contexts whose value someone outside the repository can influence. These are
# the ones that become code when interpolated into a shell script. Values
# GitHub computes itself — github.sha, runner.temp, a fixed matrix entry — are
# not attacker-controlled and are left alone deliberately: a check that flagged
# them too would be noise, and noise gets switched off.
UNTRUSTED_CONTEXTS = re.compile(
    r"\$\{\{\s*(inputs\.|github\.event\.|github\.head_ref|github\.ref_name)"
)


class WorkflowExpressionHygiene(unittest.TestCase):
    """A ${{ }} expression inside run: is substituted as text before the shell
    parses the script, so any value an attacker controls becomes code. Untrusted
    input must reach the shell through env:, where it is an ordinary string."""

    def test_no_untrusted_expression_interpolated_into_a_run_block(self) -> None:
        offenders: list[str] = []
        for workflow in sorted(WORKFLOWS.glob("*.yml")):
            source = workflow.read_text()
            for block in RUN_BLOCK.finditer(source):
                start = source[: block.start()].count("\n") + 1
                for offset, line in enumerate(block.group("body").splitlines()):
                    if UNTRUSTED_CONTEXTS.search(line):
                        offenders.append(f"{workflow.name}:{start + offset + 1}: {line.strip()}")

        self.assertEqual(
            offenders,
            [],
            "Attacker-influenced GitHub Actions expressions must be passed through "
            "env: rather than interpolated into a run: script:\n  " + "\n  ".join(offenders),
        )

    def test_release_version_shape_is_validated(self) -> None:
        """A dispatched version reaches tag names, file names and release titles,
        so the workflow pins its shape instead of trusting the input."""
        source = (WORKFLOWS / "release.yml").read_text()
        self.assertIn("VERSION_INPUT", source)
        self.assertRegex(
            source,
            r"refusing to (build|publish) an unrecognised version",
            "release.yml no longer rejects a version that is not vX.Y.Z",
        )


class InstallerFailsClosed(unittest.TestCase):
    """The README and SECURITY.md both promise the downloaded archive is verified
    against the release checksums. Every path that would install without
    verifying must therefore be fatal."""

    def test_the_script_has_no_install_without_verification_path(self) -> None:
        source = INSTALLER.read_text()
        self.assertNotIn(
            "installing without verification",
            source,
            "the installer must not proceed when checksums.txt cannot be fetched",
        )
        self.assertNotIn(
            "skipping checksum verification",
            source,
            "the installer must not proceed when it has no tool to hash with",
        )

    def _run_with_fake_curl(self, checksums_status: int, want_dir: bool = False):
        """Run the installer with a curl stub on PATH that serves a tarball but
        answers the checksums.txt request with the given status."""
        tmp = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, tmp, ignore_errors=True)

        payload = tmp / "payload"
        payload.mkdir()
        (payload / "entire-brain").write_text("#!/bin/sh\necho fake\n")
        subprocess.run(
            ["tar", "-czf", str(tmp / "asset.tar.gz"), "-C", str(payload), "entire-brain"],
            check=True,
        )

        fake_curl = tmp / "bin" / "curl"
        fake_curl.parent.mkdir()
        fake_curl.write_text(
            textwrap.dedent(
                f"""\
                #!/usr/bin/env bash
                # Serve the release archive, fail the checksums request.
                out=""
                url=""
                while [ $# -gt 0 ]; do
                  case "$1" in
                    -o) out="$2"; shift 2 ;;
                    -*) shift ;;
                    *) url="$1"; shift ;;
                  esac
                done
                case "$url" in
                  *checksums.txt) exit {1 if checksums_status else 0} ;;
                  *releases/latest) echo '{{"tag_name": "v9.9.9"}}'; exit 0 ;;
                  *.tar.gz) cp "{tmp / 'asset.tar.gz'}" "$out"; exit 0 ;;
                esac
                exit 0
                """
            )
        )
        fake_curl.chmod(fake_curl.stat().st_mode | stat.S_IEXEC)

        env = dict(os.environ)
        env["PATH"] = f"{fake_curl.parent}:{env['PATH']}"
        env["ENTIRE_BRAIN_INSTALL_DIR"] = str(tmp / "install")

        result = subprocess.run(
            ["bash", str(INSTALLER), "--version", "v9.9.9"],
            capture_output=True,
            text=True,
            env=env,
            timeout=60,
        )
        return (result, Path(env["ENTIRE_BRAIN_INSTALL_DIR"])) if want_dir else result

    def test_unfetchable_checksums_aborts_the_install(self) -> None:
        result = self._run_with_fake_curl(checksums_status=1)
        self.assertNotEqual(
            result.returncode,
            0,
            "the installer exited 0 despite being unable to verify the download:\n"
            f"stdout={result.stdout}\nstderr={result.stderr}",
        )
        self.assertIn("checksums.txt", result.stderr)

    def test_nothing_is_installed_when_verification_is_impossible(self) -> None:
        result, install_dir = self._run_with_fake_curl(checksums_status=1, want_dir=True)
        landed = list(install_dir.iterdir()) if install_dir.exists() else []
        self.assertEqual(
            landed,
            [],
            f"the installer left {landed} behind despite failing verification:\n"
            f"stdout={result.stdout}\nstderr={result.stderr}",
        )


if __name__ == "__main__":
    unittest.main()
