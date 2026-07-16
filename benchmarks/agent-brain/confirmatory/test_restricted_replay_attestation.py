#!/usr/bin/env python3
"""Synthetic, offline tests for the restricted engine replay attestation."""

from __future__ import annotations

import copy
import datetime as dt
import base64
import json
import os
import pathlib
import shutil
import subprocess
import tempfile
import unittest
from unittest import mock

import restricted_replay_attestation as ATTEST


def statement() -> dict[str, object]:
    archive_hash = "a" * 64
    source_commit = "b" * 40
    return {
        "issued_at": "2026-07-16T12:05:00Z",
        "private_diagnostic": {
            "manifest_schema_version": 2,
            "manifest_sha256": ATTEST.PRIVATE_MANIFEST_SHA256,
            "manifest_size_bytes": 123,
        },
        "public_projection": {
            "manifest_schema_version": 4,
            "manifest_sha256": "c" * 64,
            "manifest_size_bytes": 456,
            "artifact_inventory_algorithm": ATTEST.PUBLIC_INVENTORY_ALGORITHM,
            "artifact_inventory_root_sha256": "d" * 64,
            "payload_file_count": 5,
            "payload_logical_bytes": 789,
        },
        "production_inputs": {
            "pin_set_id": "production-engine-v1",
            "pin_set_authority": "production",
            "pin_set_path": ATTEST.PIN_PATH,
            "pin_set_sha256": "e" * 64,
            "matrix_path": ATTEST.MATRIX_PATH,
            "matrix_sha256": "f" * 64,
        },
        "source_identity": {
            "repository": ATTEST.REPOSITORY,
            "git_object_format": "sha1",
            "commit_oid": source_commit,
            "tree_oid": "1" * 40,
            "worktree_state": "clean",
            "checker_lock_path": ATTEST.CHECKER_LOCK_REPO_PATH,
            "checker_lock_sha256": "2" * 64,
            "checker_lock_algorithm": ATTEST.CHECKER_LOCK_ALGORITHM,
            "checker_source_aggregate_sha256": "3" * 64,
            "analyzer_lock_path": ATTEST.ANALYZER_LOCK_REPO_PATH,
            "analyzer_lock_sha256": "4" * 64,
            "analyzer_lock_algorithm": ATTEST.CHECKER_LOCK_ALGORITHM,
            "analyzer_source_aggregate_sha256": "5" * 64,
        },
        "replay_result": {
            "operation": "offline_exact_byte_validation_v1",
            "private_validator": "check_protocol.validate_engine_verification/v2",
            "private_error_count": 0,
            "public_validator": "public_engine_evidence.validate_public_bundle/v4",
            "public_error_count": 0,
            "checker_lock_valid": True,
            "analyzer_lock_valid": True,
            "decision": "pass",
        },
        "public_archive": {
            "format": "tar_zstd",
            "root": ATTEST.PUBLIC_ROOT,
            "sha256": archive_hash,
            "size_bytes": 1000,
            "regular_file_count": 6,
            "directory_count": 2,
            "logical_bytes": 900,
            "tree_inventory_algorithm": ATTEST.TREE_INVENTORY_ALGORITHM,
            "tree_inventory_sha256": "6" * 64,
            "repository": ATTEST.REPOSITORY,
            "tag": "engine-public-v4-fixture",
            "asset_name": "engine-public-v4-fixture.tar.zst",
            "asset_url": "https://github.com/entireio/entire-brain/releases/download/engine-public-v4-fixture/engine-public-v4-fixture.tar.zst",
            "release_id": 11,
            "asset_id": 12,
            "release_immutable": True,
            "release_target_commitish": source_commit,
            "asset_api_digest": f"sha256:{archive_hash}",
            "verified_at": "2026-07-16T12:00:00Z",
        },
    }


class RestrictedReplayAttestationTest(unittest.TestCase):
    def setUp(self) -> None:
        if shutil.which("ssh-keygen") is None:
            self.skipTest("ssh-keygen is unavailable")
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = pathlib.Path(self.temp.name)
        self.key = self.root / "fixture-key"
        subprocess.run(
            ["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "", "-f", str(self.key)],
            check=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env={"LC_ALL": "C", "LANG": "C", "PATH": os.environ.get("PATH", "")},
        )
        public_fields = self.key.with_suffix(".pub").read_text(encoding="ascii").split()
        self.public_key_base64 = public_fields[1]
        self.principal = "engine-replay-fixture@example.invalid"
        self.root_id = "fixture-ed25519-v1"
        self.trust_roots = {
            "schema_version": 1,
            "profile": ATTEST.TRUST_PROFILE,
            "status": "approved",
            "signature_namespace": ATTEST.SIGNATURE_NAMESPACE,
            "roots": [
                {
                    "root_id": self.root_id,
                    "principal": self.principal,
                    "key_type": "ssh-ed25519",
                    "public_key_base64": self.public_key_base64,
                    "public_key_sha256": ATTEST.sha256_bytes(ATTEST.public_key_line(self.public_key_base64)),
                    "status": "active",
                    "not_before": "2026-07-16T00:00:00Z",
                    "not_after": "2026-07-17T00:00:00Z",
                    "revoked_at": None,
                }
            ],
        }
        self.envelope = ATTEST.sign_envelope(
            statement(),
            trust_root_id=self.root_id,
            signer_principal=self.principal,
            public_key_base64=self.public_key_base64,
            private_key=self.key,
        )

    def test_fixture_sign_and_verify_round_trip(self) -> None:
        self.assertEqual(ATTEST.validate_principal(self.principal), self.principal)
        self.assertEqual(ATTEST.validate_identifier(self.root_id), self.root_id)
        ATTEST.verify_envelope(
            self.envelope,
            self.trust_roots,
            verification_time=dt.datetime(2026, 7, 16, 13, tzinfo=dt.UTC),
        )
        self.assertTrue(self.envelope["signature"].startswith("-----BEGIN SSH SIGNATURE-----\n"))
        self.assertEqual(
            self.envelope["signed_payload_sha256"],
            ATTEST.sha256_bytes(ATTEST.signed_payload(self.envelope)),
        )

    def test_signature_rejects_mutated_statement_even_if_payload_hash_is_recomputed(self) -> None:
        changed = copy.deepcopy(self.envelope)
        changed["statement"]["public_projection"]["payload_logical_bytes"] += 1
        changed["signed_payload_sha256"] = ATTEST.sha256_bytes(ATTEST.signed_payload(changed))
        with self.assertRaisesRegex(ATTEST.AttestationError, "SSHSIG operation was rejected"):
            ATTEST.verify_envelope(changed, self.trust_roots)

    def test_production_verifier_ignores_ambient_path_ssh_keygen(self) -> None:
        fake_dir = self.root / "ambient-bin"
        fake_dir.mkdir()
        marker = self.root / "fake-ran"
        fake = fake_dir / "ssh-keygen"
        fake.write_text(f"#!/bin/sh\ntouch {marker}\nexit 0\n", encoding="utf-8")
        fake.chmod(0o755)
        forged = copy.deepcopy(self.envelope)
        forged["statement"]["public_projection"]["payload_logical_bytes"] += 1
        forged["signed_payload_sha256"] = ATTEST.sha256_bytes(ATTEST.signed_payload(forged))
        ambient = f"{fake_dir}{os.pathsep}{os.environ.get('PATH', '')}"
        with mock.patch.dict(os.environ, {"PATH": ambient}):
            self.assertNotEqual(ATTEST._resolve_system_ssh_keygen(), fake)
            with self.assertRaisesRegex(ATTEST.AttestationError, "SSHSIG operation was rejected"):
                ATTEST.verify_envelope(forged, self.trust_roots)
        self.assertFalse(marker.exists())

    def test_allowed_signers_principal_cannot_inject_alternate_key(self) -> None:
        alternate_key = self.root / "alternate-key"
        subprocess.run(
            ["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "", "-f", str(alternate_key)],
            check=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env={"LC_ALL": "C", "LANG": "C", "PATH": os.environ.get("PATH", "")},
        )
        alternate_public = alternate_key.with_suffix(".pub").read_text(encoding="ascii").split()[1]
        injected_principal = f"* ssh-ed25519 {alternate_public}"
        # Construct the exact formerly exploitable object: metadata and trust pin key A,
        # signed bytes use key B, and the principal injects wildcard + key-B columns.
        with mock.patch.object(ATTEST, "validate_principal", side_effect=lambda value, _label="": value):
            forged = ATTEST.sign_envelope(
                statement(),
                trust_root_id=self.root_id,
                signer_principal=injected_principal,
                public_key_base64=self.public_key_base64,
                private_key=alternate_key,
            )
        malicious_trust = copy.deepcopy(self.trust_roots)
        malicious_trust["roots"][0]["principal"] = injected_principal
        with self.assertRaisesRegex(ATTEST.AttestationError, "literal ASCII principal token"):
            ATTEST.verify_envelope(forged, malicious_trust)

        for value in ("a,b", "*", "a?", "a b", "a\\b", 'a"b', "-option"):
            with self.subTest(value=value), self.assertRaisesRegex(ATTEST.AttestationError, "literal ASCII principal token"):
                ATTEST.validate_principal(value)
        for value in ("*", "root id", ",root", "-root"):
            with self.subTest(root_id=value), self.assertRaisesRegex(ATTEST.AttestationError, "literal ASCII identifier"):
                ATTEST.validate_identifier(value)

    def test_system_ssh_keygen_candidate_rejects_writable_or_symlink_path(self) -> None:
        insecure = self.root / "insecure-ssh-keygen"
        insecure.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
        insecure.chmod(0o777)
        with self.assertRaisesRegex(ATTEST.AttestationError, "symlink|root-owned|group/world-writable"):
            ATTEST._validate_system_ssh_keygen_candidate(insecure)
        redirected = self.root / "redirected-ssh-keygen"
        redirected.symlink_to("/usr/bin/ssh-keygen")
        with self.assertRaisesRegex(ATTEST.AttestationError, "symlink"):
            ATTEST._validate_system_ssh_keygen_candidate(redirected)

    def test_pending_revoked_wrong_principal_and_wrong_key_fail_closed(self) -> None:
        pending = copy.deepcopy(self.trust_roots)
        pending["status"] = "pending_owner_authorization"
        pending["roots"] = []
        with self.assertRaisesRegex(ATTEST.AttestationError, "pending owner authorization"):
            ATTEST.verify_envelope(self.envelope, pending)

        revoked = copy.deepcopy(self.trust_roots)
        revoked["roots"][0]["status"] = "revoked"
        revoked["roots"][0]["revoked_at"] = "2026-07-16T12:04:00Z"
        with self.assertRaisesRegex(ATTEST.AttestationError, "not active"):
            ATTEST.verify_envelope(self.envelope, revoked)

        wrong_principal = copy.deepcopy(self.trust_roots)
        wrong_principal["roots"][0]["principal"] = "other@example.invalid"
        with self.assertRaisesRegex(ATTEST.AttestationError, "principal differs"):
            ATTEST.verify_envelope(self.envelope, wrong_principal)

        wrong_key = copy.deepcopy(self.trust_roots)
        wrong_key["roots"][0]["public_key_sha256"] = "0" * 64
        with self.assertRaisesRegex(ATTEST.AttestationError, "public key differs"):
            ATTEST.verify_envelope(self.envelope, wrong_key)

    def test_signature_armor_is_strict_and_bounded(self) -> None:
        for replacement in (
            "pending",
            self.envelope["signature"] + "\n",
            self.envelope["signature"].replace("\n", "\r\n"),
            self.envelope["signature"].replace("U", "!", 1),
        ):
            with self.subTest(replacement=replacement[:40]):
                changed = copy.deepcopy(self.envelope)
                changed["signature"] = replacement
                with self.assertRaises(ATTEST.AttestationError):
                    ATTEST.verify_envelope(changed, self.trust_roots)

    def test_statement_cross_bindings_and_time_are_fail_closed(self) -> None:
        mutations = (
            ("private hash", lambda value: value["private_diagnostic"].__setitem__("manifest_sha256", "0" * 64), "private diagnostic manifest commitment"),
            ("target", lambda value: value["public_archive"].__setitem__("release_target_commitish", "0" * 40), "target differs"),
            ("api digest", lambda value: value["public_archive"].__setitem__("asset_api_digest", "sha256:" + "0" * 64), "API digest differs"),
            ("time", lambda value: value.__setitem__("issued_at", "2026-07-16T11:59:59Z"), "issued before"),
        )
        for label, mutate, message in mutations:
            with self.subTest(label=label):
                changed = statement()
                mutate(changed)
                with self.assertRaisesRegex(ATTEST.AttestationError, message):
                    ATTEST.validate_statement(changed)

    def test_timestamp_precision_cannot_invert_release_ordering(self) -> None:
        changed = statement()
        changed["public_archive"]["verified_at"] = "2026-07-16T12:00:00.123456789Z"
        changed["issued_at"] = "2026-07-16T12:00:00.123456700Z"
        with self.assertRaisesRegex(ATTEST.AttestationError, "must be RFC3339"):
            ATTEST.validate_statement(changed)
        for value in ("2026-07-16T12:00:00.1234567Z", "2026-07-16T12:00:00-00:00"):
            with self.subTest(value=value), self.assertRaisesRegex(ATTEST.AttestationError, "must be RFC3339"):
                ATTEST._parse_time(value, "fixture time")

    def test_restricted_file_requires_regular_owner_mode_0600_and_no_duplicates(self) -> None:
        path = self.root / "attestation.json"
        path.write_bytes(ATTEST.canonical_json_bytes(self.envelope) + b"\n")
        path.chmod(0o600)
        self.assertEqual(ATTEST.load_attestation(path), self.envelope)

        path.chmod(0o644)
        with self.assertRaisesRegex(ATTEST.AttestationError, "mode 0600"):
            ATTEST.load_attestation(path)
        path.chmod(0o600)

        duplicate = self.root / "duplicate.json"
        duplicate.write_text('{"schema_version":1,"schema_version":1}\n', encoding="utf-8")
        duplicate.chmod(0o600)
        with self.assertRaisesRegex(ATTEST.AttestationError, "duplicate key"):
            ATTEST.load_attestation(duplicate)

        symlink = self.root / "redirected.json"
        symlink.symlink_to(path)
        with self.assertRaisesRegex(ATTEST.AttestationError, "regular file"):
            ATTEST.load_attestation(symlink)

        noncanonical = self.root / "pretty.json"
        noncanonical.write_text(json.dumps(self.envelope, indent=2) + "\n", encoding="utf-8")
        noncanonical.chmod(0o600)
        with self.assertRaisesRegex(ATTEST.AttestationError, "not canonical JSON"):
            ATTEST.load_attestation(noncanonical)

    def test_checked_in_trust_roots_are_pending_and_empty(self) -> None:
        roots = ATTEST.load_trust_roots()
        self.assertEqual(roots["status"], "pending_owner_authorization")
        self.assertEqual(roots["roots"], [])

    def test_canonical_payload_is_key_order_independent_and_float_rejected(self) -> None:
        left = {"b": 2, "a": 1}
        right = {"a": 1, "b": 2}
        self.assertEqual(ATTEST.canonical_json_bytes(left), ATTEST.canonical_json_bytes(right))
        with self.assertRaisesRegex(ATTEST.AttestationError, "float prohibited"):
            ATTEST.canonical_json_bytes({"invalid": float("nan")})
        with self.assertRaisesRegex(ATTEST.AttestationError, "float prohibited"):
            ATTEST.canonical_json_bytes({"invalid": 1.5})

    def test_json_complexity_failures_are_translated(self) -> None:
        malformed = (
            b'{"value":' + b"1" * 5000 + b"}",
            b'{"value":' + b"[" * 2000 + b"0" + b"]" * 2000 + b"}",
        )
        for raw in malformed:
            with self.subTest(size=len(raw)), self.assertRaisesRegex(ATTEST.AttestationError, "not valid UTF-8 JSON|nesting bound"):
                ATTEST._decode_json_object(raw, "fixture")

    def test_trust_root_rejects_wrong_malformed_and_trailing_ssh_wire_keys(self) -> None:
        def wire(algorithm: bytes, key: bytes, trailing: bytes = b"") -> str:
            raw = len(algorithm).to_bytes(4, "big") + algorithm + len(key).to_bytes(4, "big") + key + trailing
            return base64.b64encode(raw).decode("ascii")

        for label, encoded, message in (
            ("rsa", wire(b"ssh-rsa", b"x" * 32), "not ssh-ed25519"),
            ("short", wire(b"ssh-ed25519", b"x" * 31), "exactly 32"),
            ("trailing", wire(b"ssh-ed25519", b"x" * 32, b"x"), "trailing"),
            ("truncated", base64.b64encode(b"\x00\x00\x00").decode("ascii"), "truncated"),
        ):
            with self.subTest(label=label):
                with self.assertRaisesRegex(ATTEST.AttestationError, message):
                    ATTEST.public_key_line(encoded)

    def test_statement_integer_fields_reject_float_and_bool_aliases(self) -> None:
        mutations = (
            ("private schema float", ("private_diagnostic", "manifest_schema_version"), 2.0),
            ("projection count float", ("public_projection", "payload_file_count"), 5.0),
            ("projection count bool", ("public_projection", "payload_file_count"), True),
            ("private errors bool", ("replay_result", "private_error_count"), False),
            ("archive count float", ("public_archive", "regular_file_count"), 6.0),
        )
        for label, (section, field), replacement in mutations:
            with self.subTest(label=label):
                changed = statement()
                changed[section][field] = replacement
                with self.assertRaises(ATTEST.AttestationError):
                    ATTEST.validate_statement(changed)


if __name__ == "__main__":
    unittest.main()
