from __future__ import annotations

import ast
import errno
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

    def test_final_file_ceiling_is_incremental_and_dedup_aware(self) -> None:
        limits = private_log.LogLimits(
            max_bytes_per_arm=8,
            max_bytes_total=32,
            max_final_file_count=2,
        )
        with tempfile.TemporaryDirectory() as temporary:
            root = self.private_root(pathlib.Path(temporary))
            first_payload = b"one"
            second_payload = b"two"
            first = self.write(root, first_payload, limits=limits)
            self.write(root, second_payload, limits=limits)
            store = root / private_log.STORE_DIRECTORY
            self.assertEqual(len(list(store.iterdir())), 2)
            private_log.check_raw_log(root, first, limits=limits)

            duplicate = self.write(root, first_payload, limits=limits)
            self.assertEqual(duplicate, first)
            self.assertEqual(len(list(store.iterdir())), 2)

            third_payload = b"three"
            with self.assertRaisesRegex(private_log.PrivateLogError, "final-file ceiling"):
                self.write(root, third_payload, limits=limits)
            self.assertEqual(len(list(store.iterdir())), 2)

            third = store / hashlib.sha256(third_payload).hexdigest()
            third.write_bytes(third_payload)
            third.chmod(0o600)
            with self.assertRaisesRegex(private_log.PrivateLogError, "final-file ceiling"):
                private_log.check_raw_log(root, first, limits=limits)

    def test_incremental_enumeration_stops_before_stating_limit_plus_one(self) -> None:
        limits = private_log.LogLimits(
            max_bytes_per_arm=8,
            max_bytes_total=32,
            max_final_file_count=2,
        )

        class SyntheticEntry:
            def __init__(self, name: str) -> None:
                self.name = name

        entries = [SyntheticEntry(character * 64) for character in ("a", "b", "c")]
        scanner = mock.MagicMock()
        scanner.__enter__.return_value = iter(entries)
        scanner.__exit__.return_value = False
        with tempfile.TemporaryDirectory() as temporary:
            metadata_path = pathlib.Path(temporary) / "metadata"
            metadata_path.write_bytes(b"")
            metadata_path.chmod(0o600)
            metadata = metadata_path.stat()
            with (
                mock.patch.object(private_log.os, "scandir", return_value=scanner),
                mock.patch.object(private_log.os, "stat", return_value=metadata) as stat_call,
                self.assertRaisesRegex(private_log.PrivateLogError, "final-file ceiling"),
            ):
                private_log._scan_store(123, limits)
            self.assertEqual(stat_call.call_count, 2)

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
        self.assertEqual(private_log.FROZEN_LIMITS.max_final_file_count, 4096)
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
            private_log.LogLimits(
                max_final_file_count=private_log.MAX_PRIVATE_RAW_LOG_FINAL_FILE_COUNT + 1,
            ),
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

    def test_every_directory_sync_error_returns_no_receipt(self) -> None:
        for stage, fail_on_directory_sync, error_number in (
            ("root-setup", 1, errno.EINVAL),
            ("store-publication", 2, errno.ENOTSUP),
        ):
            with self.subTest(stage=stage), tempfile.TemporaryDirectory() as temporary:
                root = self.private_root(pathlib.Path(temporary))
                payload = b"directory-sync-proof"
                real_fsync = private_log.os.fsync
                real_fstat = private_log.os.fstat
                directory_sync_count = 0

                def fail_selected_directory_sync(descriptor: int) -> None:
                    nonlocal directory_sync_count
                    if stat.S_ISDIR(real_fstat(descriptor).st_mode):
                        directory_sync_count += 1
                        if directory_sync_count == fail_on_directory_sync:
                            raise OSError(error_number, "synthetic directory sync failure")
                    real_fsync(descriptor)

                receipt_raw: bytes | None = None
                with (
                    mock.patch.object(
                        private_log.os,
                        "fsync",
                        side_effect=fail_selected_directory_sync,
                    ),
                    self.assertRaisesRegex(
                        private_log.PrivateLogError,
                        "directory synchronization failed",
                    ),
                ):
                    receipt_raw = self.write(root, payload)
                self.assertIsNone(receipt_raw)

                store = root / private_log.STORE_DIRECTORY
                digest = hashlib.sha256(payload).hexdigest()
                if stage == "root-setup":
                    self.assertEqual(list(store.iterdir()), [])
                else:
                    content = store / digest
                    self.assertEqual(content.read_bytes(), payload)
                    self.assertEqual(content.stat().st_nlink, 1)
                    private_log.check_raw_log(root, self.write_receipt_for(payload))

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

    def test_failed_temp_and_rollback_unlinks_leave_two_links_no_receipt_and_fail_closed(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = self.private_root(pathlib.Path(temporary))
            payload = b"complete-two-link-state"

            def refuse_descriptor_relative_unlink(
                _path: str | bytes | os.PathLike[str] | os.PathLike[bytes],
                *,
                dir_fd: int | None = None,
            ) -> None:
                if dir_fd is not None:
                    raise OSError(errno.EIO, "synthetic unlink failure")
                raise AssertionError("unexpected non-descriptor-relative unlink")

            receipt_raw: bytes | None = None
            with (
                mock.patch.object(
                    private_log.os,
                    "unlink",
                    side_effect=refuse_descriptor_relative_unlink,
                ),
                self.assertRaisesRegex(
                    private_log.PrivateLogError,
                    "temporary cleanup failed",
                ),
            ):
                receipt_raw = self.write(root, payload)
            self.assertIsNone(receipt_raw)

            store = root / private_log.STORE_DIRECTORY
            entries = list(store.iterdir())
            self.assertEqual(len(entries), 2)
            digest = hashlib.sha256(payload).hexdigest()
            digest_path = store / digest
            temporary_paths = [path for path in entries if path.name != digest]
            self.assertEqual(len(temporary_paths), 1)
            self.assertTrue(temporary_paths[0].name.startswith(".raw-log-"))
            self.assertEqual(digest_path.read_bytes(), payload)
            self.assertEqual(temporary_paths[0].read_bytes(), payload)
            self.assertEqual(digest_path.stat().st_ino, temporary_paths[0].stat().st_ino)
            self.assertEqual(digest_path.stat().st_nlink, 2)
            with self.assertRaisesRegex(private_log.PrivateLogError, "invalid entry|hard-linked"):
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

    def test_digest_path_replacement_after_hashing_is_detected(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            parent = pathlib.Path(temporary)
            root = self.private_root(parent)
            payload = b"opened-content"
            receipt_raw = self.write(root, payload)
            digest = hashlib.sha256(payload).hexdigest()
            content = root / private_log.STORE_DIRECTORY / digest
            replacement = parent / "replacement"
            replacement.write_bytes(b"replaced-bytes")
            replacement.chmod(0o600)
            opened_original = parent / "opened-original"
            real_stat = private_log.os.stat
            real_replace = private_log.os.replace
            digest_stat_count = 0

            def replace_before_post_hash_path_stat(
                path: str | bytes | int | os.PathLike[str] | os.PathLike[bytes],
                *args: Any,
                **kwargs: Any,
            ) -> os.stat_result:
                nonlocal digest_stat_count
                if path == digest and kwargs.get("dir_fd") is not None:
                    digest_stat_count += 1
                    if digest_stat_count == 2:
                        real_replace(content, opened_original)
                        real_replace(replacement, content)
                return real_stat(path, *args, **kwargs)

            with (
                mock.patch.object(
                    private_log.os,
                    "stat",
                    side_effect=replace_before_post_hash_path_stat,
                ),
                self.assertRaisesRegex(
                    private_log.PrivateLogError,
                    "pathname no longer names the opened file",
                ),
            ):
                private_log.check_raw_log(root, receipt_raw)
            self.assertEqual(digest_stat_count, 2)
            self.assertEqual(opened_original.read_bytes(), payload)
            self.assertEqual(content.read_bytes(), b"replaced-bytes")

    def test_private_log_errors_never_echo_raw_bytes_or_paths(self) -> None:
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

    def test_base_exception_cancellation_propagates_and_is_outside_non_leaking_claim(self) -> None:
        secret = "CALLER_CANCELLATION_TEXT"
        cancellation = KeyboardInterrupt(secret)

        class CancellingStream:
            def read(self, _size: int) -> bytes:
                raise cancellation

        with tempfile.TemporaryDirectory() as temporary:
            root = self.private_root(pathlib.Path(temporary))
            with self.assertRaises(KeyboardInterrupt) as caught:
                private_log.write_raw_log(
                    root,
                    cast(BinaryIO, CancellingStream()),
                )
            self.assertIs(caught.exception, cancellation)
            self.assertEqual(caught.exception.args, (secret,))
            self.assertEqual(list((root / private_log.STORE_DIRECTORY).iterdir()), [])

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
