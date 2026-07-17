#!/usr/bin/env python3
"""Synthetic, offline tests for the negative-control owner-approval verifier."""

from __future__ import annotations

import argparse
import base64
import contextlib
import copy
import datetime as dt
import io
import importlib.util
import json
import marshal
import os
import pathlib
import signal
import stat
import struct
import subprocess
import sys
import tempfile
import unittest
from collections.abc import Callable
from typing import Any
from unittest import mock

import negative_control_owner_approval_v1 as approval


HERE = pathlib.Path(__file__).parent


def _utc(value: dt.datetime) -> str:
    return value.astimezone(dt.UTC).replace(microsecond=0).strftime(
        "%Y-%m-%dT%H:%M:%SZ"
    )


def _reseal_trust_roots(value: dict[str, Any]) -> None:
    value["trust_roots_sha256"] = None
    value["trust_roots_sha256"] = approval._field_self_hash(
        value, "trust_roots_sha256"
    )


def _reseal_contract(value: dict[str, Any]) -> None:
    value["verifier_contract_sha256"] = None
    value["verifier_contract_sha256"] = approval._field_self_hash(
        value, "verifier_contract_sha256"
    )


def _reseal_envelope(value: dict[str, Any]) -> None:
    statement = value["statement"]
    statement["run_identity_sha256"] = approval._canonical_hash(
        statement["run_identity"]
    )
    value["proof"]["signed_payload_sha256"] = approval._sha256(
        approval.signed_payload(statement)
    )
    value["approval_sha256"] = None
    value["approval_sha256"] = approval._field_self_hash(value, "approval_sha256")


def _approval_raw(value: dict[str, Any]) -> bytes:
    return approval._canonical_bytes(value) + b"\n"


def _armor(raw: bytes) -> str:
    encoded = base64.b64encode(raw).decode("ascii")
    lines = [
        encoded[index : index + approval.SSHSIG_ARMOR_WIDTH]
        for index in range(0, len(encoded), approval.SSHSIG_ARMOR_WIDTH)
    ]
    return (
        "-----BEGIN SSH SIGNATURE-----\n"
        + "\n".join(lines)
        + "\n-----END SSH SIGNATURE-----"
    )


def _unarmor(value: str) -> bytes:
    lines = value.split("\n")
    return base64.b64decode("".join(lines[1:-1]), validate=True)


class SyntheticApprovalFixture:
    """An ephemeral owner key and a self-consistent, approved test fixture."""

    temporary: tempfile.TemporaryDirectory[str]
    fixture_root: pathlib.Path
    private_key: pathlib.Path
    public_key_base64: str
    public_key_blob: bytes
    public_key_line: bytes
    public_key_sha256: str
    principal: str
    root_id: str
    trust_roots: dict[str, Any]
    verifier_contract: dict[str, Any]
    approval_schema: dict[str, Any]
    report_schema: dict[str, Any]
    verifier_schema: dict[str, Any]
    envelope: dict[str, Any]

    @classmethod
    def setUpClass(cls) -> None:
        super().setUpClass()  # type: ignore[misc]
        cls.temporary = tempfile.TemporaryDirectory(
            prefix="negative-control-owner-approval-tests-"
        )
        cls.fixture_root = pathlib.Path(cls.temporary.name)
        cls.private_key = cls.fixture_root / "fixture-owner-ed25519"
        subprocess.run(
            [
                str(approval.SSH_KEYGEN_PATH),
                "-q",
                "-t",
                "ed25519",
                "-N",
                "",
                "-C",
                "",
                "-f",
                str(cls.private_key),
            ],
            check=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env={"LANG": "C", "LC_ALL": "C", "PATH": "/usr/bin"},
        )
        public_fields = cls.private_key.with_suffix(".pub").read_text(
            encoding="ascii"
        ).split()
        if public_fields[0] != "ssh-ed25519" or len(public_fields) < 2:
            raise AssertionError("fixture ssh-keygen did not produce an Ed25519 key")
        cls.public_key_base64 = public_fields[1]
        (
            cls.public_key_blob,
            cls.public_key_line,
            cls.public_key_sha256,
        ) = approval._public_key(
            cls.public_key_base64, label="ephemeral fixture public key"
        )
        cls.principal = "entire-brain-negative-control-owner-test-fixture"
        cls.root_id = "ephemeral-test-owner-v1"

        now = approval._utc_now()
        root_before = now - dt.timedelta(days=1)
        root_after = now + dt.timedelta(days=1)
        cls.trust_roots = {
            "canonical_json_profile": approval.CANONICAL_ARTIFACT_PROFILE,
            "profile": approval.TRUST_PROFILE,
            "roots": [
                {
                    "key_type": "ssh-ed25519",
                    "not_after": _utc(root_after),
                    "not_before": _utc(root_before),
                    "principal": cls.principal,
                    "public_key_base64": cls.public_key_base64,
                    "public_key_sha256": cls.public_key_sha256,
                    "purpose": "negative_control_execution_approval_v1",
                    "revoked_at": None,
                    "root_id": cls.root_id,
                    "status": "active",
                }
            ],
            "schema_version": 1,
            "signature_namespace": approval.SIGNATURE_NAMESPACE,
            "status": "approved",
            "trust_roots_sha256": None,
        }
        _reseal_trust_roots(cls.trust_roots)

        dependencies = approval._load_dependencies()
        cls.approval_schema = dependencies["approval_schema"]
        cls.report_schema = dependencies["report_schema"]
        cls.verifier_schema = dependencies["verifier_schema"]
        cls.verifier_contract = approval.build_verifier_contract()
        cls.verifier_contract["status"] = approval.APPROVED_STATUS
        cls.verifier_contract["authority"]["trust_root_status"] = "approved"
        cls.verifier_contract["trust_roots_binding"].update(
            {
                "artifact_sha256": approval._sha256(
                    approval._render(cls.trust_roots)
                ),
                "root_count": 1,
                "status": "approved",
                "trust_roots_sha256": cls.trust_roots["trust_roots_sha256"],
            }
        )
        _reseal_contract(cls.verifier_contract)

        issued = now - dt.timedelta(seconds=10)
        not_before = now - dt.timedelta(seconds=5)
        expires = now + dt.timedelta(seconds=600)
        run_identity = {
            "attempt_count": 248,
            "candidate_bindings_sha256": approval.CHECKED_CANDIDATE_BINDINGS_SHA256,
            "candidate_count": 62,
            "repository_count": 3,
            "run_id": "2" * 64,
            "run_nonce_sha256": "3" * 64,
            "schedule_sha256": approval.CHECKED_SCHEDULE_SHA256,
        }
        statement = {
            "action": "approve_exact_development_negative_control_run_subject_to_all_residual_gates_and_atomic_consumption",
            "approval_effect": copy.deepcopy(approval.APPROVAL_EFFECT),
            "approval_id": "1" * 64,
            "approval_scope": "exact_e0_contract_one_development_run_v1",
            "execution_contract": approval._expected_statement_execution_binding(),
            "execution_status_after_verification": approval.AFTER_VERIFICATION_STATUS,
            "expires_at": _utc(expires),
            "issued_at": _utc(issued),
            "not_before": _utc(not_before),
            "profile": approval.STATEMENT_PROFILE,
            "prohibitions": copy.deepcopy(approval.PROHIBITIONS),
            "protocol": copy.deepcopy(approval.PROTOCOL),
            "run_identity": run_identity,
            "run_identity_sha256": approval._canonical_hash(run_identity),
            "schema_version": 1,
            "signature_namespace": approval.SIGNATURE_NAMESPACE,
            "signature_scheme": approval.SIGNATURE_SCHEME,
            "signer_principal": cls.principal,
            "signer_public_key_sha256": cls.public_key_sha256,
            "single_use": copy.deepcopy(approval.SINGLE_USE),
            "trust_root_id": cls.root_id,
            "verifier_contract": {
                "artifact_file": approval.VERIFIER_CONTRACT_PATH.name,
                "artifact_sha256": approval._sha256(
                    approval._render(cls.verifier_contract)
                ),
                "profile": approval.VERIFIER_PROFILE,
                "status": approval.APPROVED_STATUS,
                "verifier_contract_sha256": cls.verifier_contract[
                    "verifier_contract_sha256"
                ],
            },
        }
        payload = approval.signed_payload(statement)
        signed = subprocess.run(
            [
                str(approval.SSH_KEYGEN_PATH),
                "-Y",
                "sign",
                "-f",
                str(cls.private_key),
                "-n",
                approval.SIGNATURE_NAMESPACE,
            ],
            check=True,
            input=payload,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env={"LANG": "C", "LC_ALL": "C", "PATH": "/usr/bin"},
        )
        signature_armor = signed.stdout.decode("ascii").removesuffix("\n")
        cls.envelope = {
            "approval_sha256": None,
            "profile": approval.APPROVAL_PROFILE,
            "proof": {
                "scheme": approval.SIGNATURE_SCHEME,
                "signature_armor": signature_armor,
                "signed_payload_sha256": approval._sha256(payload),
            },
            "schema_version": 1,
            "statement": statement,
        }
        cls.envelope["approval_sha256"] = approval._field_self_hash(
            cls.envelope, "approval_sha256"
        )

    @classmethod
    def tearDownClass(cls) -> None:
        cls.temporary.cleanup()
        super().tearDownClass()  # type: ignore[misc]

    def fresh_envelope(self) -> dict[str, Any]:
        return copy.deepcopy(self.envelope)

    def verify(self, value: dict[str, Any]) -> dict[str, Any]:
        return approval._verify_approval_with_dependencies(
            value,
            approval_raw=_approval_raw(value),
            trust_roots=copy.deepcopy(self.trust_roots),
            verifier_contract=copy.deepcopy(self.verifier_contract),
            approval_schema=self.approval_schema,
            report_schema=self.report_schema,
            verifier_schema=self.verifier_schema,
        )


class CheckedVerifierContractTest(unittest.TestCase):
    def test_checked_contract_is_a_deterministic_exact_build(self) -> None:
        checked, checked_raw = approval._read_json(
            approval.VERIFIER_CONTRACT_PATH,
            maximum=approval.MAX_ARTIFACT_BYTES,
            label="checked owner-approval verifier contract",
            canonical_profile=approval.CANONICAL_ARTIFACT_PROFILE,
        )
        first = approval.build_verifier_contract()
        second = approval.build_verifier_contract()
        self.assertEqual(first, second)
        self.assertEqual(checked, first)
        self.assertEqual(checked_raw, approval._render(first))
        self.assertEqual(approval.check_verifier_contract(), first)
        self.assertFalse(first["authority"]["execution_authority"])
        self.assertFalse(first["authority"]["atomic_consumption"])
        self.assertEqual(first["authority"]["paid_execution"], "forbidden")
        self.assertEqual(first["authority"]["model_provider_execution"], "forbidden")
        self.assertEqual(first["status"], approval.PENDING_STATUS)
        self.assertEqual(first["trust_roots_binding"]["root_count"], 0)

    def test_build_cli_only_writes_the_non_authorizing_checked_projection(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_name:
            output = pathlib.Path(temporary_name) / "built.json"
            self.assertEqual(approval.main(["build", "--output", str(output)]), 0)
            self.assertEqual(output.read_bytes(), approval._render(approval.build_verifier_contract()))
            self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o644)

    def test_cli_surface_is_exactly_build_check_and_verify(self) -> None:
        parser = approval._parser()
        subparsers = [
            action
            for action in parser._actions
            if isinstance(action, argparse._SubParsersAction)
        ]
        self.assertEqual(len(subparsers), 1)
        self.assertEqual(set(subparsers[0].choices), {"build", "check", "verify"})
        self.assertEqual(
            vars(parser.parse_args(["build", "--output", "out.json"])),
            {"command": "build", "output": pathlib.Path("out.json")},
        )
        self.assertEqual(
            vars(parser.parse_args(["check"])),
            {"artifact": approval.VERIFIER_CONTRACT_PATH, "command": "check"},
        )
        self.assertEqual(
            vars(parser.parse_args(["verify", "approval.json"])),
            {"approval": pathlib.Path("approval.json"), "command": "verify"},
        )
        for argv in (
            [],
            ["sign"],
            ["keygen"],
            ["authorize"],
            ["run"],
            ["verify"],
            ["verify", "approval.json", "--trust-roots", "roots.json"],
            ["verify", "approval.json", "--time", "2026-07-17T00:00:00Z"],
            ["check", "one.json", "two.json"],
        ):
            with self.subTest(argv=argv), contextlib.redirect_stderr(io.StringIO()):
                with self.assertRaises(SystemExit):
                    parser.parse_args(argv)

    def test_pending_roots_reject_verify_before_approval_read_or_subprocess(self) -> None:
        approval_path = pathlib.Path(tempfile.gettempdir()) / "must-not-be-read.json"
        original_read_json = approval._read_json
        attempted_approval_reads: list[pathlib.Path] = []

        def guarded_read_json(path: pathlib.Path, **kwargs: Any) -> Any:
            if path == approval_path:
                attempted_approval_reads.append(path)
                raise AssertionError("pending verification read the approval")
            return original_read_json(path, **kwargs)

        stderr = io.StringIO()
        with (
            mock.patch.object(approval, "_read_json", side_effect=guarded_read_json),
            mock.patch.object(
                approval.subprocess,
                "Popen",
                side_effect=AssertionError("pending verification started a subprocess"),
            ) as popen,
            contextlib.redirect_stderr(stderr),
            self.assertRaises(SystemExit) as raised,
        ):
            approval.main(["verify", str(approval_path)])
        self.assertEqual(raised.exception.code, 2)
        self.assertIn("approval verifier trust root is pending", stderr.getvalue())
        self.assertEqual(attempted_approval_reads, [])
        popen.assert_not_called()

    def test_schema_validator_and_verifier_implementation_are_exactly_pinned(self) -> None:
        identity = approval._draft_validator_identity()
        self.assertEqual(
            identity,
            {
                "artifact_file": "draft202012.py",
                "artifact_sha256": approval.CHECKED_DRAFT202012_SHA256,
                "size_bytes": approval.CHECKED_DRAFT202012_SIZE,
            },
        )
        with (
            mock.patch.object(approval, "CHECKED_DRAFT202012_SHA256", "a" * 64),
            self.assertRaisesRegex(
                approval.ApprovalVerificationError, "validator source hash differs"
            ),
        ):
            approval._draft_validator_identity()

        dependencies = approval._load_dependencies()
        changed = approval.build_verifier_contract()
        changed["implementation"]["builder"]["artifact_sha256"] = "a" * 64
        _reseal_contract(changed)
        with self.assertRaisesRegex(
            approval.ApprovalVerificationError,
            "implementation binding differs",
        ):
            approval.validate_verifier_contract(
                changed,
                schema=dependencies["verifier_schema"],
            )

    def test_forged_timestamp_valid_validator_bytecode_is_ignored(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_name:
            temporary = pathlib.Path(temporary_name).resolve(strict=True)
            source = temporary / "draft202012.py"
            source.write_text("MARKER = 'checked-source-was-imported'\n", encoding="utf-8")
            fixed_mtime = 1_700_000_000
            os.utime(source, (fixed_mtime, fixed_mtime))
            metadata = source.stat()
            forged_code = compile(
                "MARKER = 'forged-timestamp-cache-loaded'\n",
                str(source),
                "exec",
            )
            cache = (
                temporary
                / "__pycache__"
                / f"draft202012.{sys.implementation.cache_tag}.pyc"
            )
            cache.parent.mkdir()
            cache.write_bytes(
                importlib.util.MAGIC_NUMBER
                + struct.pack(
                    "<III",
                    0,
                    int(metadata.st_mtime),
                    metadata.st_size,
                )
                + marshal.dumps(forged_code)
            )
            base_environment = {
                "LANG": "C",
                "LC_ALL": "C",
                "PATH": "/usr/bin:/bin",
                "PYTHONDONTWRITEBYTECODE": "1",
            }
            control = subprocess.run(
                [
                    sys.executable,
                    "-c",
                    "import draft202012; print(draft202012.MARKER)",
                ],
                check=True,
                cwd=temporary,
                env={**base_environment, "PYTHONPATH": str(temporary)},
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
            )
            self.assertEqual(control.stdout.strip(), "forged-timestamp-cache-loaded")

            validation = subprocess.run(
                [
                    sys.executable,
                    "-c",
                    (
                        "import negative_control_owner_approval_v1 as owner;"
                        "owner._validate_schema({}, "
                        "{'$schema':'https://json-schema.org/draft/2020-12/schema',"
                        "'$id':'fixture','type':'object','additionalProperties':False},"
                        "schema_name='fixture',label='fixture');"
                        "print(owner._draft_validator_identity()['artifact_sha256'])"
                    ),
                ],
                check=True,
                cwd=temporary,
                env={
                    **base_environment,
                    "PYTHONPATH": os.pathsep.join((str(temporary), str(HERE))),
                },
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
            )
            self.assertEqual(
                validation.stdout.strip(),
                approval.CHECKED_DRAFT202012_SHA256,
            )


class SyntheticApprovalVerificationTest(SyntheticApprovalFixture, unittest.TestCase):
    def test_valid_fixture_verifies_but_never_authorizes_any_execution(self) -> None:
        report = self.verify(self.fresh_envelope())
        self.assertEqual(report["signature_verification"], "valid")
        self.assertEqual(
            report["status"],
            "owner_approval_signature_verified_execution_forbidden",
        )
        self.assertEqual(report["execution_status"], approval.AFTER_VERIFICATION_STATUS)
        self.assertFalse(report["execution_authority"])
        self.assertFalse(report["atomic_consumption"])
        self.assertEqual(report["candidate_execution"], "forbidden")
        self.assertEqual(report["model_provider_execution"], "forbidden")
        self.assertEqual(report["paid_execution"], "forbidden")
        self.assertEqual(report["replay_status"], "unconsumed_no_atomic_replay_store")
        self.assertEqual(
            report["run_binding_status"],
            "signed_statement_verified_not_bound_to_executor",
        )
        self.assertEqual(report["run_id"], "2" * 64)
        self.assertEqual(
            report["report_sha256"],
            approval._field_self_hash(report, "report_sha256"),
        )

    def test_injected_fixture_seam_still_binds_exact_approval_raw_bytes(self) -> None:
        with (
            mock.patch.object(
                approval,
                "_run_sshsig_verify",
                side_effect=AssertionError("raw-byte drift reached SSHSIG"),
            ) as verify,
            self.assertRaisesRegex(
                approval.ApprovalVerificationError,
                "raw bytes differ from the canonical envelope",
            ),
        ):
            approval._verify_approval_with_dependencies(
                self.fresh_envelope(),
                approval_raw=b"{}\n",
                trust_roots=copy.deepcopy(self.trust_roots),
                verifier_contract=copy.deepcopy(self.verifier_contract),
                approval_schema=self.approval_schema,
                report_schema=self.report_schema,
                verifier_schema=self.verifier_schema,
            )
        verify.assert_not_called()

    def test_signed_payload_has_the_exact_domain_and_canonical_statement(self) -> None:
        statement = self.envelope["statement"]
        payload = approval.signed_payload(statement)
        self.assertEqual(
            payload,
            b"entire-brain/negative-control-execution-approval/v1\0"
            + approval._canonical_bytes(statement),
        )
        self.assertEqual(
            self.envelope["proof"]["signed_payload_sha256"],
            approval._sha256(payload),
        )

    def test_semantic_tampering_fails_before_sshsig_subprocess(self) -> None:
        mutators: dict[str, Callable[[dict[str, Any]], None]] = {
            "action": lambda value: value["statement"].__setitem__("action", "approve"),
            "execution-contract": lambda value: value["statement"][
                "execution_contract"
            ].__setitem__("schedule_sha256", "a" * 64),
            "protocol": lambda value: value["statement"]["protocol"].__setitem__(
                "max_concurrency", 2
            ),
            "prohibitions": lambda value: value["statement"]["prohibitions"].pop(),
            "approval-effect": lambda value: value["statement"][
                "approval_effect"
            ].__setitem__("signature_verification_alone", "execution_authority"),
            "single-use": lambda value: value["statement"]["single_use"].__setitem__(
                "max_consumptions", 2
            ),
            "verifier-binding": lambda value: value["statement"][
                "verifier_contract"
            ].__setitem__("artifact_sha256", "a" * 64),
            "run-count": lambda value: value["statement"]["run_identity"].__setitem__(
                "attempt_count", 247
            ),
            "principal-injection": lambda value: value["statement"].__setitem__(
                "signer_principal", "entire-brain-negative-control-owner-x y"
            ),
            "namespace": lambda value: value["statement"].__setitem__(
                "signature_namespace", "other"
            ),
        }
        for label, mutate in mutators.items():
            changed = self.fresh_envelope()
            mutate(changed)
            _reseal_envelope(changed)
            with (
                self.subTest(label=label),
                mock.patch.object(
                    approval,
                    "_run_sshsig_verify",
                    side_effect=AssertionError("semantic drift reached SSHSIG"),
                ) as verify,
                self.assertRaises(approval.ApprovalVerificationError),
            ):
                self.verify(changed)
            verify.assert_not_called()

    def test_each_nested_hash_and_contract_cross_binding_is_enforced(self) -> None:
        changed = self.fresh_envelope()
        changed["statement"]["run_identity"]["run_id"] = "4" * 64
        changed["approval_sha256"] = approval._field_self_hash(
            changed, "approval_sha256"
        )
        with self.assertRaisesRegex(
            approval.ApprovalVerificationError, "run identity self hash differs"
        ):
            self.verify(changed)

        changed = self.fresh_envelope()
        changed["statement"]["run_identity"]["run_id"] = "4" * 64
        changed["statement"]["run_identity_sha256"] = approval._canonical_hash(
            changed["statement"]["run_identity"]
        )
        changed["approval_sha256"] = approval._field_self_hash(
            changed, "approval_sha256"
        )
        with self.assertRaisesRegex(
            approval.ApprovalVerificationError, "signed payload hash differs"
        ):
            self.verify(changed)

        changed = self.fresh_envelope()
        changed["proof"]["signed_payload_sha256"] = "a" * 64
        with self.assertRaisesRegex(
            approval.ApprovalVerificationError, "self hash differs"
        ):
            self.verify(changed)

        changed_trust = copy.deepcopy(self.trust_roots)
        changed_trust["roots"][0]["root_id"] = "other-owner"
        _reseal_trust_roots(changed_trust)
        with (
            mock.patch.object(
                approval,
                "_run_sshsig_verify",
                side_effect=AssertionError("unbound roots reached SSHSIG"),
            ) as verify,
            self.assertRaisesRegex(
                approval.ApprovalVerificationError,
                "verifier trust binding differs",
            ),
        ):
            approval._verify_approval_with_dependencies(
                self.envelope,
                approval_raw=_approval_raw(self.envelope),
                trust_roots=changed_trust,
                verifier_contract=self.verifier_contract,
                approval_schema=self.approval_schema,
                report_schema=self.report_schema,
                verifier_schema=self.verifier_schema,
            )
        verify.assert_not_called()

    def test_timestamp_shape_order_activation_and_lifetime_tampering(self) -> None:
        issued = approval._parse_time(
            self.envelope["statement"]["issued_at"], label="fixture issued_at"
        )
        cases: dict[str, tuple[str, str, str]] = {
            "fractional": (
                _utc(issued),
                _utc(issued + dt.timedelta(seconds=1)),
                _utc(issued + dt.timedelta(seconds=2)).replace("Z", ".0Z"),
            ),
            "reversed": (
                _utc(issued),
                _utc(issued - dt.timedelta(seconds=1)),
                _utc(issued + dt.timedelta(seconds=10)),
            ),
            "activation-delay": (
                _utc(issued),
                _utc(issued + dt.timedelta(seconds=61)),
                _utc(issued + dt.timedelta(seconds=62)),
            ),
            "lifetime": (
                _utc(issued),
                _utc(issued + dt.timedelta(seconds=1)),
                _utc(
                    issued
                    + dt.timedelta(
                        seconds=approval.MAX_APPROVAL_LIFETIME_SECONDS + 2
                    )
                ),
            ),
        }
        for label, (issued_at, not_before, expires_at) in cases.items():
            changed = self.fresh_envelope()
            changed["statement"].update(
                {
                    "expires_at": expires_at,
                    "issued_at": issued_at,
                    "not_before": not_before,
                }
            )
            _reseal_envelope(changed)
            with self.subTest(label=label), self.assertRaises(
                approval.ApprovalVerificationError
            ):
                approval.validate_approval_envelope(
                    changed,
                    schema=self.approval_schema,
                    verifier_contract=self.verifier_contract,
                )

    def test_future_and_expired_approvals_fail_before_sshsig(self) -> None:
        statement = self.envelope["statement"]
        times = (
            approval._parse_time(statement["not_before"], label="not_before")
            - dt.timedelta(seconds=1),
            approval._parse_time(statement["expires_at"], label="expires_at"),
        )
        for current in times:
            with (
                self.subTest(current=current),
                mock.patch.object(approval, "_utc_now", return_value=current),
                mock.patch.object(
                    approval,
                    "_run_sshsig_verify",
                    side_effect=AssertionError("inactive approval reached SSHSIG"),
                ) as verify,
                self.assertRaisesRegex(
                    approval.ApprovalVerificationError,
                    "not active at verification time",
                ),
            ):
                self.verify(self.fresh_envelope())
            verify.assert_not_called()

    def test_root_status_identity_key_and_window_tampering_fail_closed(self) -> None:
        current = approval._utc_now()
        cases: dict[str, Callable[[dict[str, Any]], None]] = {
            "pending": lambda value: value.update(
                {"roots": [], "status": "pending_owner_authorization"}
            ),
            "revoked": lambda value: value["roots"][0].update(
                {"revoked_at": _utc(current), "status": "revoked"}
            ),
            "principal": lambda value: value["roots"][0].__setitem__(
                "principal", "entire-brain-negative-control-owner-other"
            ),
            "key-hash": lambda value: value["roots"][0].__setitem__(
                "public_key_sha256", "a" * 64
            ),
            "approval-window": lambda value: value["roots"][0].__setitem__(
                "not_after",
                _utc(
                    approval._parse_time(
                        self.envelope["statement"]["expires_at"],
                        label="fixture expires_at",
                    )
                    - dt.timedelta(seconds=1)
                ),
            ),
        }
        for label, mutate in cases.items():
            changed = copy.deepcopy(self.trust_roots)
            mutate(changed)
            _reseal_trust_roots(changed)
            with self.subTest(label=label), self.assertRaises(
                approval.ApprovalVerificationError
            ):
                validated = approval._validate_trust_roots(changed)
                approval._select_trust_root(
                    self.envelope,
                    validated,
                    current=current,
                )

        duplicate = copy.deepcopy(self.trust_roots)
        duplicate["roots"].append(copy.deepcopy(duplicate["roots"][0]))
        _reseal_trust_roots(duplicate)
        with self.assertRaisesRegex(
            approval.ApprovalVerificationError, "duplicate"
        ):
            approval._validate_trust_roots(duplicate)

    def test_signature_binds_every_semantically_free_run_identifier(self) -> None:
        changed = self.fresh_envelope()
        changed["statement"]["run_identity"]["run_id"] = "4" * 64
        _reseal_envelope(changed)
        with self.assertRaisesRegex(
            approval.ApprovalVerificationError,
            "offline SSHSIG verification was rejected",
        ):
            self.verify(changed)

    def test_signature_bit_flip_is_rejected_by_offline_sshsig(self) -> None:
        changed = self.fresh_envelope()
        raw = bytearray(_unarmor(changed["proof"]["signature_armor"]))
        raw[-1] ^= 1
        changed["proof"]["signature_armor"] = _armor(bytes(raw))
        changed["approval_sha256"] = None
        changed["approval_sha256"] = approval._field_self_hash(
            changed, "approval_sha256"
        )
        with self.assertRaisesRegex(
            approval.ApprovalVerificationError,
            "offline SSHSIG verification was rejected",
        ):
            self.verify(changed)

    def test_sshsig_wire_and_armor_are_parsed_strictly_before_subprocess(self) -> None:
        valid_armor = self.envelope["proof"]["signature_armor"]
        valid_raw = _unarmor(valid_armor)
        namespace = approval.SIGNATURE_NAMESPACE.encode("ascii")
        wrong_namespace = b"x" + namespace[1:]
        variants = {
            "trailing-newline": valid_armor + "\n",
            "crlf": valid_armor.replace("\n", "\r\n"),
            "noncanonical-width": valid_armor.replace("\n", "", 1),
            "wrong-version": _armor(
                valid_raw[:6] + struct.pack(">I", 2) + valid_raw[10:]
            ),
            "wrong-namespace": _armor(
                valid_raw.replace(namespace, wrong_namespace, 1)
            ),
            "trailing-wire-byte": _armor(valid_raw + b"\0"),
        }
        for label, variant in variants.items():
            with self.subTest(label=label), self.assertRaises(
                approval.ApprovalVerificationError
            ):
                approval._decode_sshsig_armor(
                    variant,
                    expected_public_key_blob=self.public_key_blob,
                )
        with self.assertRaisesRegex(
            approval.ApprovalVerificationError, "public key differs"
        ):
            approval._decode_sshsig_armor(
                valid_armor,
                expected_public_key_blob=b"wrong",
            )


class JsonAndPathSafetyTest(SyntheticApprovalFixture, unittest.TestCase):
    def test_json_parser_rejects_duplicate_float_constant_surrogate_and_bounds(self) -> None:
        malformed = {
            "duplicate": b'{"x":1,"x":2}',
            "float": b'{"x":1.0}',
            "constant": b'{"x":NaN}',
            "surrogate": b'{"x":"\\ud800"}',
            "integer": b'{"x":' + b"1" * (approval.MAX_INTEGER_DIGITS + 1) + b"}",
            "depth": b'{"x":' + b"[" * 34 + b"0" + b"]" * 34 + b"}",
        }
        for label, raw in malformed.items():
            with self.subTest(label=label), self.assertRaises(
                approval.ApprovalVerificationError
            ):
                approval._parse_json(raw, label=label)
        with (
            mock.patch.object(approval, "MAX_JSON_NODES", 2),
            self.assertRaisesRegex(
                approval.ApprovalVerificationError, "node ceiling"
            ),
        ):
            approval._parse_json(b'{"x":1,"y":2}', label="nodes")

    def test_signed_envelope_file_must_be_compact_canonical_private_and_single_link(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_name:
            # macOS exposes /var as a symlink to /private/var. Resolve the
            # system-created temporary directory so this test exercises the
            # approval file itself rather than intentionally forbidden
            # symlink traversal in an ancestor.
            root = pathlib.Path(temporary_name).resolve(strict=True)
            path = root / "approval.json"
            path.write_bytes(_approval_raw(self.envelope))
            path.chmod(0o600)
            parsed, raw = approval._read_json(
                path,
                maximum=approval.MAX_APPROVAL_BYTES,
                label="fixture approval",
                canonical_profile=approval.CANONICAL_SIGNED_PROFILE,
                require_owner_mode=0o600,
                require_single_link=True,
            )
            self.assertEqual(parsed, self.envelope)
            self.assertEqual(raw, _approval_raw(self.envelope))

            pretty = root / "pretty.json"
            pretty.write_bytes(approval._render(self.envelope))
            pretty.chmod(0o600)
            with self.assertRaisesRegex(
                approval.ApprovalVerificationError, "not canonical"
            ):
                approval._read_json(
                    pretty,
                    maximum=approval.MAX_APPROVAL_BYTES,
                    label="pretty approval",
                    canonical_profile=approval.CANONICAL_SIGNED_PROFILE,
                    require_owner_mode=0o600,
                    require_single_link=True,
                )

            public_mode = root / "public-mode.json"
            public_mode.write_bytes(_approval_raw(self.envelope))
            public_mode.chmod(0o644)
            with self.assertRaisesRegex(
                approval.ApprovalVerificationError, "mode differs"
            ):
                approval._read_json(
                    public_mode,
                    maximum=approval.MAX_APPROVAL_BYTES,
                    label="public-mode approval",
                    canonical_profile=approval.CANONICAL_SIGNED_PROFILE,
                    require_owner_mode=0o600,
                    require_single_link=True,
                )

            hardlink = root / "approval-hardlink.json"
            os.link(path, hardlink)
            with self.assertRaisesRegex(
                approval.ApprovalVerificationError, "link count differs"
            ):
                approval._read_json(
                    hardlink,
                    maximum=approval.MAX_APPROVAL_BYTES,
                    label="hard-linked approval",
                    canonical_profile=approval.CANONICAL_SIGNED_PROFILE,
                    require_owner_mode=0o600,
                    require_single_link=True,
                )

    def test_secure_reader_rejects_symlink_traversal_nonregular_and_oversize(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_name:
            root = pathlib.Path(temporary_name).resolve(strict=True)
            regular = root / "regular"
            regular.write_bytes(b"{}\n")
            redirected = root / "redirected"
            redirected.symlink_to(regular)
            fifo = root / "fifo"
            os.mkfifo(fifo)
            child = root / "child"
            child.mkdir()
            cases = {
                "symlink": redirected,
                "traversal": child / ".." / "regular",
                "fifo": fifo,
            }
            for label, path in cases.items():
                with self.subTest(label=label), self.assertRaises(
                    approval.ApprovalVerificationError
                ):
                    approval._read_bounded(path, maximum=16, label=label)
            with self.assertRaisesRegex(
                approval.ApprovalVerificationError, "byte ceiling"
            ):
                approval._read_bounded(regular, maximum=2, label="oversize")


class SshsigSubprocessContractTest(SyntheticApprovalFixture, unittest.TestCase):
    def test_subprocess_has_exact_binary_argv_environment_io_timeout_and_private_files(self) -> None:
        observed: dict[str, Any] = {}

        class FakeProcess:
            pid = 4242
            returncode = 0

            def __init__(self, command: list[str], **kwargs: Any) -> None:
                observed["command"] = command
                observed["kwargs"] = kwargs
                observed["cwd_mode"] = stat.S_IMODE(
                    pathlib.Path(kwargs["cwd"]).stat().st_mode
                )
                allowed = pathlib.Path(command[4])
                signature_path = pathlib.Path(command[10])
                observed["allowed"] = allowed.read_bytes()
                observed["allowed_mode"] = stat.S_IMODE(allowed.stat().st_mode)
                observed["signature"] = signature_path.read_bytes()
                observed["signature_mode"] = stat.S_IMODE(signature_path.stat().st_mode)

            def communicate(
                self, *, input: bytes | None = None, timeout: int | None = None
            ) -> tuple[bytes, bytes]:
                observed["input"] = input
                observed["timeout"] = timeout
                return b"", b""

        payload = b"synthetic signed payload"
        armor = self.envelope["proof"]["signature_armor"]
        identity = {"sha256": "stable"}
        with (
            mock.patch.object(
                approval, "_system_ssh_keygen_identity", return_value=identity
            ) as identity_check,
            mock.patch.object(approval.subprocess, "Popen", FakeProcess),
        ):
            approval._run_sshsig_verify(
                payload=payload,
                signature_armor=armor,
                principal=self.principal,
                key_line=self.public_key_line,
            )
        self.assertEqual(identity_check.call_count, 2)
        self.assertEqual(
            observed["command"],
            [
                "/usr/bin/ssh-keygen",
                "-Y",
                "verify",
                "-f",
                observed["command"][4],
                "-I",
                self.principal,
                "-n",
                approval.SIGNATURE_NAMESPACE,
                "-s",
                observed["command"][10],
            ],
        )
        kwargs = observed["kwargs"]
        self.assertEqual(kwargs["env"], {"LANG": "C", "LC_ALL": "C", "PATH": "/usr/bin"})
        self.assertFalse(kwargs["shell"])
        self.assertIs(kwargs["stdin"], subprocess.PIPE)
        self.assertIs(kwargs["stdout"], subprocess.DEVNULL)
        self.assertIs(kwargs["stderr"], subprocess.DEVNULL)
        self.assertTrue(kwargs["start_new_session"])
        self.assertEqual(observed["cwd_mode"], 0o700)
        self.assertEqual(observed["allowed_mode"], 0o600)
        self.assertEqual(observed["signature_mode"], 0o600)
        self.assertEqual(
            observed["allowed"],
            self.principal.encode("ascii") + b" " + self.public_key_line,
        )
        self.assertEqual(observed["signature"], armor.encode("ascii") + b"\n")
        self.assertEqual(observed["input"], payload)
        self.assertEqual(observed["timeout"], approval.SSH_TIMEOUT_SECONDS)

    def test_timeout_kills_the_process_group_and_fails_closed(self) -> None:
        process = mock.Mock()
        process.pid = 4242
        process.communicate.side_effect = [
            subprocess.TimeoutExpired(cmd="ssh-keygen", timeout=approval.SSH_TIMEOUT_SECONDS),
            (b"", b""),
        ]
        with (
            mock.patch.object(
                approval, "_system_ssh_keygen_identity", return_value={"stable": True}
            ),
            mock.patch.object(approval.subprocess, "Popen", return_value=process),
            mock.patch.object(approval.os, "killpg") as killpg,
            self.assertRaisesRegex(
                approval.ApprovalVerificationError, "verification timed out"
            ),
        ):
            approval._run_sshsig_verify(
                payload=b"payload",
                signature_armor=self.envelope["proof"]["signature_armor"],
                principal=self.principal,
                key_line=self.public_key_line,
            )
        killpg.assert_called_once_with(4242, signal.SIGKILL)
        self.assertEqual(process.communicate.call_count, 2)

    def test_process_start_failure_is_translated_and_never_falls_back(self) -> None:
        with (
            mock.patch.object(
                approval, "_system_ssh_keygen_identity", return_value={"stable": True}
            ),
            mock.patch.object(
                approval.subprocess, "Popen", side_effect=OSError("synthetic")
            ) as popen,
            self.assertRaisesRegex(
                approval.ApprovalVerificationError, "could not start"
            ),
        ):
            approval._run_sshsig_verify(
                payload=b"payload",
                signature_armor=self.envelope["proof"]["signature_armor"],
                principal=self.principal,
                key_line=self.public_key_line,
            )
        self.assertEqual(popen.call_count, 1)
        self.assertEqual(popen.call_args.args[0][0], "/usr/bin/ssh-keygen")


if __name__ == "__main__":
    unittest.main()
