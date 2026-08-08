#!/usr/bin/env python3
"""Fail when publication-unsafe local identifiers return to tracked files."""

from __future__ import annotations

import pathlib
import re
import subprocess
import unittest


HERE = pathlib.Path(__file__).resolve().parent
REPO = HERE.parents[1]


def joined(*parts: bytes) -> bytes:
    return b"".join(parts).lower()


BANNED: tuple[tuple[bytes, str], ...] = (
    (joined(b"/Users/", b"thomi"), "private developer home path"),
    (joined(b"/Users/", b"suhaan"), "private contributor home path"),
    (joined(b"/Users/", b"georgf"), "private upstream checkout path"),
    (joined(b"Users-", b"thomi"), "private username encoded in a path"),
    (joined(b"suhaan", b"thayyil"), "private contributor identifier"),
    (joined(b"Stefan ", b"Haubold"), "private evidence author identity"),
    (joined(b"[redacted-name] ", b"Dohmke"), "partially redacted author identity"),
    (joined(b"peyton", b"montei"), "private contributor identifier"),
    (joined(b"dvy", b"dra"), "private contributor identifier"),
    (joined(b"/Users/", b"Victor"), "private contributor home path"),
    (joined(b"\\Users\\", b"Victor"), "private contributor home path"),
    (joined(b"/private/var/", b"folders/"), "host-specific temporary path"),
    (joined(b"eu-staging", b".api.entire.io"), "internal service endpoint"),
    (joined(b"aws-us-east-2", b".api.entire.io"), "internal service endpoint"),
    (joined(b"aws-eu-central-1", b".api.entire.io"), "internal service endpoint"),
    (joined(b"aws-eu-west-1", b".api.entire.io"), "internal service endpoint"),
    (joined(b"claude/diff-less-", b"brain"), "private held-branch name"),
    (joined(b"review-", b"cutover"), "private cross-repository branch name"),
)

BANNED_REGEX: tuple[tuple[re.Pattern[bytes], str], ...] = (
    (
        re.compile(joined(b"\\baws-(?:us|eu|ap)-", b"[a-z0-9-]+\\b")),
        "internal cell identifier",
    ),
    (
        re.compile(joined(b"\\bcell-(?:us|eu|ap)-", b"[a-z0-9-]+\\.entire\\.io\\b")),
        "internal cell endpoint",
    ),
    (
        re.compile(joined(b"\\bcore[.-](?:us|eu)", b"\\.entire\\.io\\b")),
        "internal core endpoint",
    ),
    (
        re.compile(joined(b"\\b(?:us|eu)\\.auth", b"\\.entire\\.io\\b")),
        "internal authentication endpoint",
    ),
)


class PublicationSafetyTest(unittest.TestCase):
    def test_tracked_files_do_not_contain_audited_private_identifiers(self) -> None:
        proc = subprocess.run(
            ["git", "ls-files", "-z"],
            cwd=REPO,
            check=True,
            stdout=subprocess.PIPE,
        )
        findings: list[str] = []
        for raw_path in proc.stdout.split(b"\0"):
            if not raw_path:
                continue
            relative = pathlib.Path(raw_path.decode("utf-8", errors="surrogateescape"))
            data = (REPO / relative).read_bytes().lower()
            for needle, reason in BANNED:
                if needle in data:
                    findings.append(f"{relative}: {reason}")
            for pattern, reason in BANNED_REGEX:
                if pattern.search(data):
                    findings.append(f"{relative}: {reason}")
        self.assertEqual(findings, [], "publication-safety findings:\n" + "\n".join(findings))


if __name__ == "__main__":
    unittest.main()
