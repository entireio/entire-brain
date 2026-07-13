#!/usr/bin/env python3
"""Verify a private Phase 0A archive against its public checksum manifest."""

from __future__ import annotations

import argparse
import hashlib
import json
import pathlib
import tarfile
from typing import Any


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_file(path: pathlib.Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def unique_member(archive: tarfile.TarFile, suffix: str) -> tarfile.TarInfo:
    matches = [member for member in archive.getmembers() if member.isfile() and member.name.endswith(suffix)]
    if len(matches) != 1:
        raise RuntimeError(f"expected one archive member ending {suffix!r}, found {len(matches)}")
    return matches[0]


def member_bytes(archive: tarfile.TarFile, member: tarfile.TarInfo) -> bytes:
    handle = archive.extractfile(member)
    if handle is None:
        raise RuntimeError(f"cannot read archive member: {member.name}")
    return handle.read()


def verify_suite(archive: tarfile.TarFile, suite: dict[str, Any]) -> dict[str, Any]:
    name = str(suite["suite"])
    records = member_bytes(archive, unique_member(archive, f"/{name}/records.ndjson"))
    record_count = sum(1 for line in records.splitlines() if line.strip())
    if record_count != int(suite["records"]):
        raise RuntimeError(f"{name}: record count {record_count} != {suite['records']}")
    if sha256_bytes(records) != suite["records_sha256"]:
        raise RuntimeError(f"{name}: records.ndjson SHA-256 mismatch")

    summary_field = "summary_sha256" if "summary_sha256" in suite else "prep_summary_sha256"
    summary_name = "summary.json" if summary_field == "summary_sha256" else "prep-summary.json"
    summary = member_bytes(archive, unique_member(archive, f"/{name}/{summary_name}"))
    if sha256_bytes(summary) != suite[summary_field]:
        raise RuntimeError(f"{name}: {summary_name} SHA-256 mismatch")
    return {"suite": name, "records": record_count, "verified": True}


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--manifest", type=pathlib.Path, required=True)
    parser.add_argument("--archive", type=pathlib.Path, required=True)
    args = parser.parse_args()

    manifest = json.loads(args.manifest.read_text())
    archive_path = args.archive.resolve()
    if archive_path.name != manifest["archive_basename"]:
        raise RuntimeError("archive basename does not match manifest")
    if archive_path.stat().st_size != int(manifest["archive_bytes"]):
        raise RuntimeError("archive size does not match manifest")
    if sha256_file(archive_path) != manifest["archive_sha256"]:
        raise RuntimeError("archive SHA-256 does not match manifest")

    verified_suites = []
    with tarfile.open(archive_path, "r:gz") as archive:
        roots = set()
        for member in archive.getmembers():
            name = member.name
            if name.startswith("./"):
                name = name[2:]
            name = name.strip("/")
            # Skip empty and tar metadata entries (e.g. a leading "." dir or a
            # pax_global_header some tar creators emit) so a valid archive is not
            # rejected over its layout; the archive_root match below is unchanged.
            if not name or name == "." or name == "pax_global_header":
                continue
            roots.add(name.split("/", 1)[0])
        if roots != {manifest["archive_root"]}:
            raise RuntimeError(f"archive roots {sorted(roots)} do not match manifest")
        for suite in manifest.get("suites", []):
            verified_suites.append(verify_suite(archive, suite))

    print(json.dumps({
        "archive": archive_path.name,
        "archive_sha256": manifest["archive_sha256"],
        "suites": verified_suites,
        "verified": True,
    }, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
