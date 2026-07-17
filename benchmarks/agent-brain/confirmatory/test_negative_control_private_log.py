from __future__ import annotations

import ast
import fcntl
import hashlib
import io
import json
import os
import pathlib
import stat
import tempfile
import unittest
from unittest import mock
from typing import Any, BinaryIO, cast

import draft202012
import negative_control_private_log as private_log


class NegativeControlPrivateLogTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.root = pathlib.Path(private_log.__file__).parent
        cls.schema_path = (
            cls.root / "schemas" / "negative-control-private-log-receipt-v1.schema.json"
        )
        schema = json.loads(cls.schema_path.read_text(encoding="utf-8"))
        cls.validator = draft202012.Validator(
            [draft202012.SchemaDocument(cls.schema_path.name, schema)]
        )

    def private_root(self, parent: pathlib.Path, name: str = "private") -> pathlib.Path:
        root = parent / name
        root.mkdir(mode=0o700)
        root.chmod(0o700)
        return root

    def write(
        self,
        root: pathlib.Path,
        payload: bytes,
        *,
        limits: private_log.LogLimits = private_log.FROZEN_LIMITS,
    ) -> bytes:
        return private_log.write_raw_log(root, io.BytesIO(payload), limits=limits)

    def receipt_value(self, raw: bytes) -> dict[str, Any]:
        value = json.loads(raw)
        self.assertIsInstance(value, dict)
        return value

    def test_exact_private_storage_and_two_field_public_receipt(self) -> None:
        payload = b"HOST=synthetic-host\x00SECRET=not-redacted\xff\n"
        with tempfile.TemporaryDirectory() as temporary:
            parent = pathlib.Path(temporary)
            root = self.private_root(parent, "private-path-must-not-leak")
            receipt_raw = self.write(root, payload)
            digest = hashlib.sha256(payload).hexdigest()
            expected = (
                json.dumps(
                    {
                        "raw_log_byte_count": len(payload),
                        "raw_log_sha256": digest,
                    },
                    sort_keys=True,
                    separators=(",", ":"),
                ).encode("utf-8")
                + b"\n"
            )
            self.assertEqual(receipt_raw, expected)
            receipt = self.receipt_value(receipt_raw)
            self.assertEqual(set(receipt), {"raw_log_sha256", "raw_log_byte_count"})
            self.assertNotIn(b"private-path-must-not-leak", receipt_raw)
            self.assertNotIn(b"synthetic-host", receipt_raw)
            self.assertNotIn(b"SECRET", receipt_raw)
            self.assertNotIn(b"redact", receipt_raw.lower())

            store = root / private_log.STORE_DIRECTORY
            content = store / digest
            self.assertEqual(content.read_bytes(), payload)
            for path, mode in (
                (root, 0o700),
                (store, 0o700),
                (root / private_log.LOCK_FILE, 0o600),
                (content, 0o600),
            ):
                metadata = path.lstat()
                self.assertEqual(stat.S_IMODE(metadata.st_mode), mode)
                self.assertFalse(path.is_symlink())
            self.assertTrue(content.is_file())
            self.assertEqual(content.stat().st_nlink, 1)
            checked = private_log.check_raw_log(root, receipt_raw)
            self.assertEqual(checked.raw_log_sha256, digest)
            self.assertEqual(checked.raw_log_byte_count, len(payload))

    def test_empty_raw_log_is_content_addressed_and_checked(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = self.private_root(pathlib.Path(temporary))
            receipt_raw = self.write(root, b"")
            receipt = private_log.check_raw_log(root, receipt_raw)
            self.assertEqual(receipt.raw_log_byte_count, 0)
            self.assertEqual(receipt.raw_log_sha256, hashlib.sha256(b"").hexdigest())
            self.assertEqual((root / "sha256" / receipt.raw_log_sha256).read_bytes(), b"")

    def test_schema_and_runtime_accept_the_same_exact_receipt_shape(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = self.private_root(pathlib.Path(temporary))
            receipt_raw = self.write(root, b"synthetic")
            value = self.receipt_value(receipt_raw)
            self.validator.validate(value, self.schema_path.name, label="private raw-log receipt")

            schema_invalid = [
                {**value, "path": "/private/raw"},
                {**value, "raw_log_byte_count": -1},
                {**value, "raw_log_byte_count": private_log.MAX_PRIVATE_RAW_LOG_BYTES_PER_ARM + 1},
                {**value, "raw_log_byte_count": True},
                {**value, "raw_log_sha256": "a" * 63},
            ]
            for candidate in schema_invalid:
                with self.subTest(candidate=candidate):
                    with self.assertRaises(draft202012.SchemaError):
                        self.validator.validate(
                            candidate,
                            self.schema_path.name,
                            label="invalid private raw-log receipt",
                        )

    def test_receipt_checker_rejects_noncanonical_and_ambiguous_bytes(self) -> None:
        digest = hashlib.sha256(b"synthetic").hexdigest()
        value = {"raw_log_byte_count": 9, "raw_log_sha256": digest}
        canonical = private_log.RawLogReceipt(digest, 9).canonical_bytes()
        invalid = [
            json.dumps(value, indent=2, sort_keys=True).encode() + b"\n",
            canonical + b"\n",
            (
                b'{"raw_log_sha256":"'
                + digest.encode()
                + b'","raw_log_byte_count":9}\n'
            ),
            (
                b'{"raw_log_byte_count":9,"raw_log_byte_count":9,"raw_log_sha256":"'
                + digest.encode()
                + b'"}\n'
            ),
            b'{"raw_log_byte_count":9,"raw_log_sha256":"not-a-hash"}\n',
            b"[]\n",
            b"\xff",
        ]
        for raw in invalid:
            with self.subTest(raw=raw[:32]):
                with self.assertRaises(private_log.PrivateLogError):
                    private_log.parse_receipt(raw)

    def test_private_root_requires_non_symlink_owner_directory_mode_0700(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            parent = pathlib.Path(temporary)

            wrong_mode = self.private_root(parent, "wrong-mode")
            wrong_mode.chmod(0o755)
            with self.assertRaisesRegex(private_log.PrivateLogError, "mode must be 0700"):
                self.write(wrong_mode, b"x")

            regular = parent / "regular-file"
            regular.write_bytes(b"")
            regular.chmod(0o700)
            with self.assertRaisesRegex(private_log.PrivateLogError, "must be a directory"):
                self.write(regular, b"x")

            target = self.private_root(parent, "target")
            symlink = parent / "root-symlink"
            symlink.symlink_to(target, target_is_directory=True)
            with self.assertRaisesRegex(private_log.PrivateLogError, "must be a directory"):
                self.write(symlink, b"x")

            owner = self.private_root(parent, "owner")
            with (
                mock.patch.object(private_log.os, "getuid", return_value=os.getuid() + 1),
                self.assertRaisesRegex(private_log.PrivateLogError, "owned by the current user"),
            ):
                self.write(owner, b"x")

    def test_store_and_lock_reject_symlink_mode_hardlink_and_extra_entries(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            parent = pathlib.Path(temporary)

            root = self.private_root(parent, "store-symlink")
            outside = self.private_root(parent, "outside")
            (root / private_log.STORE_DIRECTORY).symlink_to(outside, target_is_directory=True)
            with self.assertRaises(private_log.PrivateLogError):
                self.write(root, b"x")

            root = self.private_root(parent, "lock-symlink")
            target = parent / "lock-target"
            target.write_bytes(b"")
            target.chmod(0o600)
            (root / private_log.LOCK_FILE).symlink_to(target)
            with self.assertRaises(private_log.PrivateLogError):
                self.write(root, b"x")

            root = self.private_root(parent, "wrong-store-mode")
            (root / private_log.STORE_DIRECTORY).mkdir(mode=0o755)
            (root / private_log.STORE_DIRECTORY).chmod(0o755)
            with self.assertRaisesRegex(private_log.PrivateLogError, "mode must be 0700"):
                self.write(root, b"x")

            root = self.private_root(parent, "extra-entry")
            (root / "unexpected").write_bytes(b"x")
            with self.assertRaisesRegex(private_log.PrivateLogError, "unexpected entry"):
                self.write(root, b"x")

            root = self.private_root(parent, "hardlinked-lock")
            self.write(root, b"first")
            lock_alias = parent / "lock-alias"
            os.link(root / private_log.LOCK_FILE, lock_alias)
            with self.assertRaisesRegex(private_log.PrivateLogError, "hard-linked"):
                private_log.check_raw_log(root, self.write_receipt_for(b"first"))

    def write_receipt_for(self, payload: bytes) -> bytes:
        return private_log.RawLogReceipt(
            raw_log_sha256=hashlib.sha256(payload).hexdigest(),
            raw_log_byte_count=len(payload),
        ).canonical_bytes()

    def test_raw_file_requires_regular_non_symlink_non_hardlink_mode_0600(self) -> None:
        cases = ("mode", "hardlink", "symlink")
        for case in cases:
            with self.subTest(case=case), tempfile.TemporaryDirectory() as temporary:
                parent = pathlib.Path(temporary)
                root = self.private_root(parent)
                payload = b"synthetic"
                receipt_raw = self.write(root, payload)
                digest = hashlib.sha256(payload).hexdigest()
                content = root / private_log.STORE_DIRECTORY / digest
                if case == "mode":
                    content.chmod(0o644)
                elif case == "hardlink":
                    os.link(content, root / private_log.STORE_DIRECTORY / ("f" * 64))
                else:
                    content.unlink()
                    content.symlink_to(parent / "missing")
                with self.assertRaises(private_log.PrivateLogError):
                    private_log.check_raw_log(root, receipt_raw)

    def test_existing_content_address_is_never_overwritten_with_different_bytes(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = self.private_root(pathlib.Path(temporary))
            self.write(root, b"initialize")
            payload = b"expected exact bytes"
            digest = hashlib.sha256(payload).hexdigest()
            target = root / private_log.STORE_DIRECTORY / digest
            wrong = b"X" * len(payload)
            target.write_bytes(wrong)
            target.chmod(0o600)
            with self.assertRaisesRegex(private_log.PrivateLogError, "SHA-256 differs"):
                self.write(root, payload)
            self.assertEqual(target.read_bytes(), wrong)

    def test_identical_content_deduplicates_without_replacement(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = self.private_root(pathlib.Path(temporary))
            payload = b"same exact raw bytes"
            first = self.write(root, payload)
            content = root / private_log.STORE_DIRECTORY / hashlib.sha256(payload).hexdigest()
            first_inode = content.stat().st_ino
            second = self.write(root, payload)
            self.assertEqual(first, second)
            self.assertEqual(content.stat().st_ino, first_inode)
            self.assertEqual(content.stat().st_nlink, 1)
            self.assertEqual(len(list((root / private_log.STORE_DIRECTORY).iterdir())), 1)

    def test_per_arm_ceiling_is_fail_closed_with_tightened_limit(self) -> None:
        limits = private_log.LogLimits(max_bytes_per_arm=4, max_bytes_total=20)
        with tempfile.TemporaryDirectory() as temporary:
            root = self.private_root(pathlib.Path(temporary))
            receipt_raw = self.write(root, b"1234", limits=limits)
            private_log.check_raw_log(root, receipt_raw, limits=limits)
            with self.assertRaisesRegex(private_log.PrivateLogError, "per-arm ceiling"):
                self.write(root, b"12345", limits=limits)
            self.assertEqual(len(list((root / private_log.STORE_DIRECTORY).iterdir())), 1)

    def test_total_ceiling_uses_actual_locked_store_sum(self) -> None:
        limits = private_log.LogLimits(max_bytes_per_arm=8, max_bytes_total=10)
        with tempfile.TemporaryDirectory() as temporary:
            root = self.private_root(pathlib.Path(temporary))
            first = self.write(root, b"123456", limits=limits)
            private_log.check_raw_log(root, first, limits=limits)
            with self.assertRaisesRegex(private_log.PrivateLogError, "total ceiling"):
                self.write(root, b"abcde", limits=limits)
            store = root / private_log.STORE_DIRECTORY
            self.assertEqual(sum(path.stat().st_size for path in store.iterdir()), 6)

            tighter = private_log.LogLimits(max_bytes_per_arm=5, max_bytes_total=5)
            with self.assertRaisesRegex(private_log.PrivateLogError, "byte count|ceiling"):
                private_log.check_raw_log(root, first, limits=tighter)

    def test_full_store_allows_exact_duplicate_but_rejects_new_content(self) -> None:
        limits = private_log.LogLimits(max_bytes_per_arm=6, max_bytes_total=6)
        with tempfile.TemporaryDirectory() as temporary:
            root = self.private_root(pathlib.Path(temporary))
            payload = b"123456"
            first = self.write(root, payload, limits=limits)
            duplicate = self.write(root, payload, limits=limits)
            self.assertEqual(duplicate, first)
            with self.assertRaisesRegex(private_log.PrivateLogError, "total ceiling"):
                self.write(root, b"abcde", limits=limits)
            store = root / private_log.STORE_DIRECTORY
            self.assertEqual([path.name for path in store.iterdir()], [hashlib.sha256(payload).hexdigest()])

    def test_fifo_and_directory_entries_fail_without_untrusted_blocking_open(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            parent = pathlib.Path(temporary)
            fifo_root = self.private_root(parent, "fifo-lock")
            os.mkfifo(fifo_root / private_log.LOCK_FILE, mode=0o600)
            with self.assertRaisesRegex(private_log.PrivateLogError, "regular file"):
                self.write(fifo_root, b"x")

            for kind in ("fifo", "directory"):
                with self.subTest(kind=kind):
                    root = self.private_root(parent, f"digest-{kind}")
                    receipt = self.write(root, b"first")
                    entry = root / private_log.STORE_DIRECTORY / ("f" * 64)
                    if kind == "fifo":
                        os.mkfifo(entry, mode=0o600)
                    else:
                        entry.mkdir(mode=0o700)
                    with self.assertRaisesRegex(private_log.PrivateLogError, "regular file"):
                        private_log.check_raw_log(root, receipt)

    def test_busy_owner_lock_fails_closed_without_waiting(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = self.private_root(pathlib.Path(temporary))
            receipt = self.write(root, b"locked")
            descriptor = os.open(
                root / private_log.LOCK_FILE,
                os.O_RDWR | getattr(os, "O_NONBLOCK", 0),
            )
            try:
                fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
                with self.assertRaisesRegex(private_log.PrivateLogError, "lock is busy"):
                    private_log.check_raw_log(root, receipt)
            finally:
                fcntl.flock(descriptor, fcntl.LOCK_UN)
                os.close(descriptor)

    def test_limits_can_only_tighten_the_frozen_16_mib_and_2_gib_values(self) -> None:
        self.assertEqual(private_log.FROZEN_LIMITS.max_bytes_per_arm, 16 * 1024 * 1024)
        self.assertEqual(private_log.FROZEN_LIMITS.max_bytes_total, 2 * 1024 * 1024 * 1024)
        invalid = [
            private_log.LogLimits(
                max_bytes_per_arm=private_log.MAX_PRIVATE_RAW_LOG_BYTES_PER_ARM + 1,
                max_bytes_total=private_log.MAX_PRIVATE_RAW_LOG_BYTES_TOTAL,
            ),
            private_log.LogLimits(
                max_bytes_per_arm=1,
                max_bytes_total=private_log.MAX_PRIVATE_RAW_LOG_BYTES_TOTAL + 1,
            ),
            private_log.LogLimits(max_bytes_per_arm=2, max_bytes_total=1),
        ]
        with tempfile.TemporaryDirectory() as temporary:
            root = self.private_root(pathlib.Path(temporary))
            for limits in invalid:
                with self.subTest(limits=limits), self.assertRaises(private_log.PrivateLogError):
                    self.write(root, b"x", limits=limits)

    def test_interruption_before_atomic_link_leaves_no_final_or_receipt(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = self.private_root(pathlib.Path(temporary))
            receipt_raw: bytes | None = None
            with (
                mock.patch.object(private_log.os, "link", side_effect=KeyboardInterrupt),
                self.assertRaises(KeyboardInterrupt),
            ):
                receipt_raw = self.write(root, b"synthetic")
            self.assertIsNone(receipt_raw)
            self.assertEqual(list((root / private_log.STORE_DIRECTORY).iterdir()), [])

    def test_interruption_after_link_returns_no_receipt_and_leaves_only_complete_final(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = self.private_root(pathlib.Path(temporary))
            payload = b"complete-before-public-receipt"
            original_fsync = private_log._fsync_directory
            calls = 0

            def interrupt_second_directory_sync(descriptor: int) -> None:
                nonlocal calls
                calls += 1
                if calls == 2:
                    raise KeyboardInterrupt
                original_fsync(descriptor)

            receipt_raw: bytes | None = None
            with (
                mock.patch.object(
                    private_log,
                    "_fsync_directory",
                    side_effect=interrupt_second_directory_sync,
                ),
                self.assertRaises(KeyboardInterrupt),
            ):
                receipt_raw = self.write(root, payload)
            self.assertIsNone(receipt_raw)
            digest = hashlib.sha256(payload).hexdigest()
            content = root / private_log.STORE_DIRECTORY / digest
            self.assertEqual(content.read_bytes(), payload)
            self.assertEqual(content.stat().st_nlink, 1)
            private_log.check_raw_log(root, self.write_receipt_for(payload))

    def test_orphan_temporary_entry_blocks_future_use_pending_cleanup_gate(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = self.private_root(pathlib.Path(temporary))
            receipt = self.write(root, b"first")
            orphan = root / private_log.STORE_DIRECTORY / ".raw-log-orphan.tmp"
            orphan.write_bytes(b"partial")
            orphan.chmod(0o600)
            with self.assertRaisesRegex(private_log.PrivateLogError, "invalid entry"):
                private_log.check_raw_log(root, receipt)
            with self.assertRaisesRegex(private_log.PrivateLogError, "invalid entry"):
                self.write(root, b"second")
            self.assertTrue(orphan.exists())

    def test_mutation_is_detected_and_errors_never_echo_raw_bytes_or_paths(self) -> None:
        secret = b"ULTRA_SECRET_RAW_VALUE"

        class ExplodingStream:
            def read(self, _size: int) -> bytes:
                raise RuntimeError(secret.decode())

        with tempfile.TemporaryDirectory() as temporary:
            parent = pathlib.Path(temporary)
            root = self.private_root(parent, "SECRET_PRIVATE_PATH")
            for operation in (
                lambda: private_log.write_raw_log(root, cast(BinaryIO, ExplodingStream())),
                lambda: private_log.parse_receipt(
                    b'{"raw_log_byte_count":1,"raw_log_sha256":"ULTRA_SECRET_RAW_VALUE"}\n'
                ),
            ):
                with self.assertRaises(private_log.PrivateLogError) as caught:
                    operation()
                message = str(caught.exception)
                self.assertNotIn(secret.decode(), message)
                self.assertNotIn("SECRET_PRIVATE_PATH", message)

            payload = b"same-size-original"
            receipt_raw = self.write(root, payload)
            digest = hashlib.sha256(payload).hexdigest()
            content = root / private_log.STORE_DIRECTORY / digest
            content.write_bytes(b"same-size-mutated!")
            content.chmod(0o600)
            with self.assertRaises(private_log.PrivateLogError) as caught:
                private_log.check_raw_log(root, receipt_raw)
            self.assertNotIn("same-size-mutated", str(caught.exception))

    def test_module_has_no_execution_network_provider_or_retention_surface(self) -> None:
        source_path = pathlib.Path(private_log.__file__)
        tree = ast.parse(source_path.read_text(encoding="utf-8"))
        imported: set[str] = set()
        for node in ast.walk(tree):
            if isinstance(node, ast.Import):
                imported.update(alias.name.split(".")[0] for alias in node.names)
            elif isinstance(node, ast.ImportFrom) and node.module:
                imported.add(node.module.split(".")[0])
        self.assertTrue(
            imported.isdisjoint(
                {"subprocess", "socket", "urllib", "http", "requests", "shutil"}
            )
        )
        self.assertEqual(
            set(private_log.__all__),
            {
                "LogLimits",
                "PrivateLogError",
                "RawLogReceipt",
                "check_raw_log",
                "parse_receipt",
                "write_raw_log",
            },
        )
        self.assertFalse(hasattr(private_log, "main"))


if __name__ == "__main__":
    unittest.main()
