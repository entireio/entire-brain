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

# Every form a run: script can take. Matching only "run: |" left the single
# line form, the chomping forms and folded scalars unscanned — which is most of
# the run: keys in test.yml, and exactly where a new one would be added.
RUN_BLOCK = re.compile(
    r"^(?P<indent> +)run:[ \t]*(?:"
    r"(?P<inline>[^|>\n][^\n]*)\n"                       # run: echo ...
    r"|[|>][+-]?[ \t]*\n(?P<body>(?:(?P=indent) .*\n|[ \t]*\n)*)"  # run: | / |- / > / >-
    r")",
    re.M,
)


def run_script_lines(source):
    """Yield (line_number, text) for every line of every run: script."""
    for block in RUN_BLOCK.finditer(source):
        start = source[: block.start()].count("\n") + 1
        if block.group("inline"):
            yield start, block.group("inline")
        else:
            for offset, line in enumerate((block.group("body") or "").splitlines()):
                yield start + offset + 1, line


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
            for line_no, line in run_script_lines(source):
                if UNTRUSTED_CONTEXTS.search(line):
                    offenders.append(f"{workflow.name}:{line_no}: {line.strip()}")

        self.assertEqual(
            offenders,
            [],
            "Attacker-influenced GitHub Actions expressions must be passed through "
            "env: rather than interpolated into a run: script:\n  " + "\n  ".join(offenders),
        )

    def test_every_job_that_consumes_the_version_validates_its_shape(self) -> None:
        """A dispatched version reaches tag names, asset file names and release
        titles. Grepping for the error message proves nothing — this finds the
        guard in each job and runs its regex, so a weakened pattern fails here
        rather than in a release."""
        source = (WORKFLOWS / "release.yml").read_text()
        self.assertIn("VERSION_INPUT", source)

        # Split into jobs so a guard in one cannot vouch for the other.
        jobs = dict(re.findall(r"^  (\w[\w-]*):\n(.*?)(?=^  \w[\w-]*:\n|\Z)",
                               source, re.M | re.S))
        for job in ("build", "publish"):
            self.assertIn(job, jobs, f"release.yml has no {job} job")
            guards = re.findall(r'\[\[ ! "\$version" =~ (\S+) \]\]', jobs[job])
            self.assertEqual(
                len(guards), 1,
                f"the {job} job must validate the version exactly once; found {len(guards)}",
            )
            self._assert_regex_is_strict(guards[0], job)

    ACCEPT = ["v0.1.0", "v1.2.3", "v10.20.30",
              "v1.2.3-nightly.202609192059.abc1234", "v0.0.0-dev.f9838c0", "v0.1.0+build.5"]
    REJECT = ["0.1.0", "v1.2", "v1", "", "v1.2.3.4", "v1.2.3-",
              "v1.2.3; rm -rf /", "v1.2.3$(id)", "v1.2.3 && echo pwned",
              "v1.2.3`id`", "../../etc/passwd", "v1.2.3\nv9.9.9"]

    def _assert_regex_is_strict(self, pattern: str, job: str) -> None:
        """Run the workflow's own regex in bash, the way the workflow does."""
        script = (
            'shopt -s nocasematch 2>/dev/null; '
            f'if [[ "$1" =~ {pattern} ]]; then echo ACCEPT; else echo REJECT; fi'
        )
        def verdict(value: str) -> str:
            return subprocess.run(["bash", "-c", script, "bash", value],
                                  capture_output=True, text=True,
                                  timeout=30).stdout.strip()

        for good in self.ACCEPT:
            self.assertEqual(verdict(good), "ACCEPT",
                             f"{job}: version {good!r} should be accepted by {pattern}")
        for bad in self.REJECT:
            self.assertEqual(verdict(bad), "REJECT",
                             f"{job}: version {bad!r} MUST be rejected by {pattern}")


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
                  *checksums.txt)
                    {"echo 'deadbeef  some-other-file.tar.gz' > \"$out\"; exit 0"
                     if checksums_status == 2 else
                     f"exit {1 if checksums_status else 0}"} ;;
                  *releases/latest) echo '{{"tag_name": "v9.9.9"}}'; exit 0 ;;
                  *.tar.gz) cp "{tmp / 'asset.tar.gz'}" "$out"; exit 0 ;;
                esac
                exit 0
                """
            )
        )
        fake_curl.chmod(fake_curl.stat().st_mode | stat.S_IEXEC)

        # Stub `entire` too. The installer's success path runs
        # `entire plugin install --force`, which would repoint the developer's
        # real Brain plugin at this temp dir and then delete it on cleanup.
        # No test takes that path today; one added later must not break the host.
        fake_entire = fake_curl.parent / "entire"
        fake_entire.write_text("#!/usr/bin/env bash\nexit 0\n")
        fake_entire.chmod(fake_entire.stat().st_mode | stat.S_IEXEC)

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

    def test_a_checksums_file_missing_this_asset_says_so(self) -> None:
        """pipefail would kill the script at the grep assignment, making the
        dedicated error unreachable and leaving a curl | bash user with a bare
        exit 1 and no diagnostic."""
        result = self._run_with_fake_curl(checksums_status=2)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("no entry for", result.stderr,
                      f"expected a diagnostic, got stderr={result.stderr!r}")

    def test_nothing_is_installed_when_verification_is_impossible(self) -> None:
        result, install_dir = self._run_with_fake_curl(checksums_status=1, want_dir=True)
        landed = list(install_dir.iterdir()) if install_dir.exists() else []
        self.assertEqual(
            landed,
            [],
            f"the installer left {landed} behind despite failing verification:\n"
            f"stdout={result.stdout}\nstderr={result.stderr}",
        )


class InstallerRunsWhenPiped(unittest.TestCase):
    """The documented invocation is curl | bash, where $0 is "bash" and the
    script has no file to read itself back out of. Anything that reads $0 works
    when the script is run as a file and breaks the way it is actually used."""

    def test_help_works_when_the_script_is_piped_into_bash(self) -> None:
        result = subprocess.run(
            ["bash", "-s", "--", "--help"],
            input=INSTALLER.read_text(),
            capture_output=True,
            text=True,
            timeout=30,
        )
        self.assertEqual(
            result.returncode,
            0,
            f"--help failed when piped, which is how the README documents it:\n"
            f"stdout={result.stdout}\nstderr={result.stderr}",
        )
        self.assertIn("--nightly", result.stdout)
        self.assertNotIn("No such file", result.stderr)

    def test_the_script_does_not_read_itself_from_argv_zero(self) -> None:
        self.assertNotIn(
            '"$0"',
            INSTALLER.read_text(),
            'the script must not read $0: under curl | bash it is "bash", not a path',
        )


if __name__ == "__main__":
    unittest.main()
