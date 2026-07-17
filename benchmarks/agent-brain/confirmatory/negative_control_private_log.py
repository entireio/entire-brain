#!/usr/bin/env python3
"""Private content-addressed raw-log storage for future negative controls.

This module is a storage primitive only.  It cannot run a candidate, invoke
Git or Go, create a worktree, access a model/provider, open a protected
population, or authorize benchmark execution.  The caller must provide an
already-created private root and a binary stream.  A canonical public receipt
is returned only after the exact raw bytes are durably published and checked.

There is deliberately no retention-deletion API and no CLI.  Cleanup,
expiration, executor integration, classification, and public receipt
publication remain separate gates.
"""

from __future__ import annotations

import contextlib
import dataclasses
import errno
import fcntl
import hashlib
import json
import os
import pathlib
import re
import secrets
import stat
from collections.abc import Iterator
from typing import Any, BinaryIO


MAX_PRIVATE_RAW_LOG_BYTES_PER_ARM = 16_777_216
MAX_PRIVATE_RAW_LOG_BYTES_TOTAL = 2_147_483_648
PRIVATE_ROOT_MODE = 0o700
PRIVATE_FILE_MODE = 0o600
STORE_DIRECTORY = "sha256"
LOCK_FILE = ".negative-control-private-log.lock"
READ_CHUNK_BYTES = 1_048_576
MAX_RECEIPT_BYTES = 256
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")

__all__ = [
    "LogLimits",
    "PrivateLogError",
    "RawLogReceipt",
    "check_raw_log",
    "parse_receipt",
    "write_raw_log",
]


class PrivateLogError(ValueError):
    """Raised when private-log storage or a public receipt fails closed."""


@dataclasses.dataclass(frozen=True)
class LogLimits:
    """Exact ceilings; tests may inject only stricter positive values."""

    max_bytes_per_arm: int = MAX_PRIVATE_RAW_LOG_BYTES_PER_ARM
    max_bytes_total: int = MAX_PRIVATE_RAW_LOG_BYTES_TOTAL


FROZEN_LIMITS = LogLimits()


@dataclasses.dataclass(frozen=True)
class RawLogReceipt:
    raw_log_sha256: str
    raw_log_byte_count: int

    def as_dict(self) -> dict[str, Any]:
        return {
            "raw_log_byte_count": self.raw_log_byte_count,
            "raw_log_sha256": self.raw_log_sha256,
        }

    def canonical_bytes(self) -> bytes:
        return _canonical_receipt_bytes(self.as_dict())


@dataclasses.dataclass(frozen=True)
class _StoreHandles:
    root_fd: int
    store_fd: int
    lock_fd: int


def _require(condition: bool, message: str) -> None:
    if not condition:
        raise PrivateLogError(message)


def _validate_limits(limits: LogLimits) -> None:
    _require(isinstance(limits, LogLimits), "private-log limits are invalid")
    _require(
        type(limits.max_bytes_per_arm) is int
        and 0 < limits.max_bytes_per_arm <= MAX_PRIVATE_RAW_LOG_BYTES_PER_ARM,
        "per-arm private-log limit may only be tightened",
    )
    _require(
        type(limits.max_bytes_total) is int
        and 0 < limits.max_bytes_total <= MAX_PRIVATE_RAW_LOG_BYTES_TOTAL,
        "total private-log limit may only be tightened",
    )
    _require(
        limits.max_bytes_per_arm <= limits.max_bytes_total,
        "private-log limit ordering is invalid",
    )


def _validate_private_directory(metadata: os.stat_result, label: str) -> None:
    _require(stat.S_ISDIR(metadata.st_mode), f"{label} must be a directory")
    _require(not stat.S_ISLNK(metadata.st_mode), f"{label} must not be a symlink")
    _require(metadata.st_uid == os.getuid(), f"{label} must be owned by the current user")
    _require(stat.S_IMODE(metadata.st_mode) == PRIVATE_ROOT_MODE, f"{label} mode must be 0700")


def _validate_private_file(metadata: os.stat_result, label: str, *, expected_size: int | None = None) -> None:
    _require(stat.S_ISREG(metadata.st_mode), f"{label} must be a regular file")
    _require(not stat.S_ISLNK(metadata.st_mode), f"{label} must not be a symlink")
    _require(metadata.st_uid == os.getuid(), f"{label} must be owned by the current user")
    _require(stat.S_IMODE(metadata.st_mode) == PRIVATE_FILE_MODE, f"{label} mode must be 0600")
    _require(metadata.st_nlink == 1, f"{label} must not be hard-linked")
    if expected_size is not None:
        _require(metadata.st_size == expected_size, f"{label} byte count differs")


def _directory_open_flags() -> int:
    nofollow = getattr(os, "O_NOFOLLOW", 0)
    directory = getattr(os, "O_DIRECTORY", 0)
    _require(nofollow != 0 and directory != 0, "secure directory opens are unavailable")
    return os.O_RDONLY | nofollow | directory | getattr(os, "O_CLOEXEC", 0)


def _regular_open_flags(*, write: bool, create: bool = False) -> int:
    nofollow = getattr(os, "O_NOFOLLOW", 0)
    _require(nofollow != 0, "secure file opens are unavailable")
    flags = (
        (os.O_WRONLY if write else os.O_RDONLY)
        | nofollow
        | getattr(os, "O_CLOEXEC", 0)
        | getattr(os, "O_NONBLOCK", 0)
    )
    if create:
        flags |= os.O_CREAT | os.O_EXCL
    return flags


def _close_quietly(descriptor: int) -> None:
    if descriptor < 0:
        return
    try:
        os.close(descriptor)
    except OSError:
        pass


def _open_lock(root_fd: int, *, create: bool) -> int:
    descriptor = -1
    created = False
    try:
        if create:
            try:
                descriptor = os.open(
                    LOCK_FILE,
                    os.O_RDWR
                    | os.O_CREAT
                    | os.O_EXCL
                    | getattr(os, "O_NOFOLLOW", 0)
                    | getattr(os, "O_CLOEXEC", 0)
                    | getattr(os, "O_NONBLOCK", 0),
                    PRIVATE_FILE_MODE,
                    dir_fd=root_fd,
                )
                created = True
            except FileExistsError:
                descriptor = os.open(
                    LOCK_FILE,
                    os.O_RDWR
                    | getattr(os, "O_NOFOLLOW", 0)
                    | getattr(os, "O_CLOEXEC", 0)
                    | getattr(os, "O_NONBLOCK", 0),
                    dir_fd=root_fd,
                )
        else:
            descriptor = os.open(
                LOCK_FILE,
                os.O_RDWR
                | getattr(os, "O_NOFOLLOW", 0)
                | getattr(os, "O_CLOEXEC", 0)
                | getattr(os, "O_NONBLOCK", 0),
                dir_fd=root_fd,
            )
        if created:
            os.fchmod(descriptor, PRIVATE_FILE_MODE)
        metadata = os.fstat(descriptor)
        _validate_private_file(metadata, "private-log lock", expected_size=0)
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise PrivateLogError("private-log lock is busy") from None
        except OSError as exc:
            if exc.errno in {errno.EACCES, errno.EAGAIN}:
                raise PrivateLogError("private-log lock is busy") from None
            raise
        return descriptor
    except PrivateLogError:
        _close_quietly(descriptor)
        raise
    except OSError:
        _close_quietly(descriptor)
        raise PrivateLogError("private-log lock could not be opened securely") from None


def _open_store_directory(root_fd: int, *, create: bool) -> int:
    created = False
    descriptor = -1
    try:
        if create:
            try:
                os.mkdir(STORE_DIRECTORY, PRIVATE_ROOT_MODE, dir_fd=root_fd)
                created = True
            except FileExistsError:
                pass
        descriptor = os.open(STORE_DIRECTORY, _directory_open_flags(), dir_fd=root_fd)
        if created:
            os.fchmod(descriptor, PRIVATE_ROOT_MODE)
        metadata = os.fstat(descriptor)
        scanned = os.stat(STORE_DIRECTORY, dir_fd=root_fd, follow_symlinks=False)
        _validate_private_directory(metadata, "private-log content store")
        _require(
            (metadata.st_dev, metadata.st_ino) == (scanned.st_dev, scanned.st_ino),
            "private-log content store changed while opening",
        )
        return descriptor
    except PrivateLogError:
        _close_quietly(descriptor)
        raise
    except OSError:
        _close_quietly(descriptor)
        raise PrivateLogError("private-log content store could not be opened securely") from None


@contextlib.contextmanager
def _open_private_store(private_root: pathlib.Path, *, create: bool) -> Iterator[_StoreHandles]:
    root_fd = -1
    lock_fd = -1
    store_fd = -1
    try:
        root = pathlib.Path(private_root)
        try:
            scanned = root.lstat()
            resolved = root.resolve(strict=True)
            resolved_metadata = resolved.lstat()
        except OSError:
            raise PrivateLogError("private-log root metadata check failed") from None
        _validate_private_directory(scanned, "private-log root")
        _validate_private_directory(resolved_metadata, "resolved private-log root")
        _require(
            (scanned.st_dev, scanned.st_ino) == (resolved_metadata.st_dev, resolved_metadata.st_ino),
            "private-log root does not resolve to itself",
        )
        try:
            root_fd = os.open(root, _directory_open_flags())
        except OSError:
            raise PrivateLogError("private-log root could not be opened securely") from None
        opened = os.fstat(root_fd)
        _validate_private_directory(opened, "opened private-log root")
        _require(
            (opened.st_dev, opened.st_ino) == (scanned.st_dev, scanned.st_ino),
            "private-log root changed while opening",
        )

        lock_fd = _open_lock(root_fd, create=create)
        store_fd = _open_store_directory(root_fd, create=create)
        try:
            root_entries = set(os.listdir(root_fd))
        except OSError:
            raise PrivateLogError("private-log root could not be enumerated securely") from None
        _require(
            root_entries == {LOCK_FILE, STORE_DIRECTORY},
            "private-log root contains an unexpected entry",
        )
        if create:
            _fsync_directory(root_fd)
        yield _StoreHandles(root_fd=root_fd, store_fd=store_fd, lock_fd=lock_fd)
    finally:
        _close_quietly(store_fd)
        if lock_fd >= 0:
            try:
                fcntl.flock(lock_fd, fcntl.LOCK_UN)
            except OSError:
                pass
        _close_quietly(lock_fd)
        _close_quietly(root_fd)


def _fsync_directory(descriptor: int) -> None:
    try:
        os.fsync(descriptor)
    except OSError as exc:
        if exc.errno not in {errno.EINVAL, errno.ENOTSUP}:
            raise PrivateLogError("private-log directory synchronization failed") from None


def _scan_store(store_fd: int, limits: LogLimits) -> tuple[dict[str, int], int]:
    try:
        names = sorted(os.listdir(store_fd))
    except OSError:
        raise PrivateLogError("private-log content store could not be enumerated") from None
    inventory: dict[str, int] = {}
    total = 0
    for name in names:
        _require(SHA256_RE.fullmatch(name) is not None, "private-log content store contains an invalid entry")
        try:
            metadata = os.stat(name, dir_fd=store_fd, follow_symlinks=False)
        except OSError:
            raise PrivateLogError("private-log content metadata check failed") from None
        _validate_private_file(metadata, "private raw-log file")
        _require(
            0 <= metadata.st_size <= limits.max_bytes_per_arm,
            "private raw-log file exceeds the per-arm ceiling",
        )
        total += metadata.st_size
        _require(total <= limits.max_bytes_total, "private raw-log store exceeds the total ceiling")
        inventory[name] = metadata.st_size
    return inventory, total


def _create_temporary_file(store_fd: int) -> tuple[int, str]:
    for _ in range(64):
        name = f".raw-log-{secrets.token_hex(16)}.tmp"
        try:
            descriptor = os.open(
                name,
                _regular_open_flags(write=True, create=True),
                PRIVATE_FILE_MODE,
                dir_fd=store_fd,
            )
        except FileExistsError:
            continue
        except OSError:
            raise PrivateLogError("private raw-log temporary file could not be created") from None
        try:
            os.fchmod(descriptor, PRIVATE_FILE_MODE)
            _validate_private_file(os.fstat(descriptor), "private raw-log temporary file", expected_size=0)
            return descriptor, name
        except BaseException:
            _close_quietly(descriptor)
            try:
                os.unlink(name, dir_fd=store_fd)
            except OSError:
                pass
            raise
    raise PrivateLogError("private raw-log temporary name allocation failed")


def _write_bounded_stream(
    descriptor: int,
    raw_stream: BinaryIO,
    *,
    limits: LogLimits,
) -> RawLogReceipt:
    digest = hashlib.sha256()
    byte_count = 0
    maximum_new_bytes = limits.max_bytes_per_arm
    while True:
        request_size = min(READ_CHUNK_BYTES, maximum_new_bytes - byte_count + 1)
        _require(request_size > 0, "private raw-log ceiling arithmetic failed")
        try:
            chunk = raw_stream.read(request_size)
        except Exception:
            raise PrivateLogError("private raw-log stream read failed") from None
        _require(isinstance(chunk, bytes), "private raw-log stream must yield bytes")
        if chunk == b"":
            break
        next_count = byte_count + len(chunk)
        _require(next_count <= limits.max_bytes_per_arm, "private raw log exceeds the per-arm ceiling")
        offset = 0
        try:
            while offset < len(chunk):
                written = os.write(descriptor, chunk[offset:])
                _require(written > 0, "private raw-log write made no progress")
                offset += written
        except PrivateLogError:
            raise
        except OSError:
            raise PrivateLogError("private raw-log write failed") from None
        digest.update(chunk)
        byte_count = next_count
    try:
        os.fsync(descriptor)
        metadata = os.fstat(descriptor)
    except OSError:
        raise PrivateLogError("private raw-log synchronization failed") from None
    _validate_private_file(metadata, "private raw-log temporary file", expected_size=byte_count)
    return RawLogReceipt(raw_log_sha256=digest.hexdigest(), raw_log_byte_count=byte_count)


def _metadata_identity(metadata: os.stat_result) -> tuple[int, ...]:
    return (
        metadata.st_dev,
        metadata.st_ino,
        metadata.st_mode,
        metadata.st_uid,
        metadata.st_nlink,
        metadata.st_size,
        metadata.st_mtime_ns,
        metadata.st_ctime_ns,
    )


def _verify_content_file(store_fd: int, receipt: RawLogReceipt, limits: LogLimits) -> None:
    _require(
        0 <= receipt.raw_log_byte_count <= limits.max_bytes_per_arm,
        "raw-log receipt exceeds the per-arm ceiling",
    )
    descriptor = -1
    try:
        descriptor = os.open(
            receipt.raw_log_sha256,
            _regular_open_flags(write=False),
            dir_fd=store_fd,
        )
        before = os.fstat(descriptor)
        _validate_private_file(
            before,
            "private raw-log file",
            expected_size=receipt.raw_log_byte_count,
        )
        digest = hashlib.sha256()
        byte_count = 0
        while True:
            chunk = os.read(descriptor, READ_CHUNK_BYTES)
            if chunk == b"":
                break
            digest.update(chunk)
            byte_count += len(chunk)
            _require(
                byte_count <= receipt.raw_log_byte_count,
                "private raw-log file changed while reading",
            )
        after = os.fstat(descriptor)
        _require(
            _metadata_identity(after) == _metadata_identity(before),
            "private raw-log file changed while reading",
        )
        _require(byte_count == receipt.raw_log_byte_count, "private raw-log byte count differs")
        _require(digest.hexdigest() == receipt.raw_log_sha256, "private raw-log SHA-256 differs")
    except PrivateLogError:
        raise
    except OSError:
        raise PrivateLogError("private raw-log file could not be read securely") from None
    finally:
        _close_quietly(descriptor)


def _unlink_temporary(store_fd: int, name: str | None) -> None:
    if name is None:
        return
    try:
        os.unlink(name, dir_fd=store_fd)
    except FileNotFoundError:
        pass
    except OSError:
        # A residual temporary entry makes the next exact store scan fail closed.
        pass


def write_raw_log(
    private_root: pathlib.Path,
    raw_stream: BinaryIO,
    *,
    limits: LogLimits = FROZEN_LIMITS,
) -> bytes:
    """Store exact raw bytes and return the canonical two-field receipt.

    Canonical receipt bytes are returned only after final-file verification.  The
    content store is summed from secure metadata under its owner-only lock;
    no caller-provided total is accepted.
    """

    _validate_limits(limits)
    with _open_private_store(pathlib.Path(private_root), create=True) as handles:
        inventory, existing_total = _scan_store(handles.store_fd, limits)
        descriptor, temporary_name = _create_temporary_file(handles.store_fd)
        receipt: RawLogReceipt | None = None
        try:
            receipt = _write_bounded_stream(
                descriptor,
                raw_stream,
                limits=limits,
            )
            _close_quietly(descriptor)
            descriptor = -1
            if receipt.raw_log_sha256 not in inventory:
                _require(
                    existing_total + receipt.raw_log_byte_count <= limits.max_bytes_total,
                    "private raw-log store would exceed the total ceiling",
                )
            published = False
            try:
                os.link(
                    temporary_name,
                    receipt.raw_log_sha256,
                    src_dir_fd=handles.store_fd,
                    dst_dir_fd=handles.store_fd,
                    follow_symlinks=False,
                )
                published = True
            except FileExistsError:
                pass
            except OSError:
                raise PrivateLogError("private raw-log atomic publication failed") from None

            try:
                os.unlink(temporary_name, dir_fd=handles.store_fd)
                temporary_name = None
            except OSError:
                if published:
                    try:
                        os.unlink(receipt.raw_log_sha256, dir_fd=handles.store_fd)
                    except OSError:
                        pass
                raise PrivateLogError("private raw-log temporary cleanup failed") from None

            _verify_content_file(handles.store_fd, receipt, limits)
            _fsync_directory(handles.store_fd)
            final_inventory, final_total = _scan_store(handles.store_fd, limits)
            _require(
                final_inventory.get(receipt.raw_log_sha256) == receipt.raw_log_byte_count,
                "private raw-log content address is absent",
            )
            _require(final_total <= limits.max_bytes_total, "private raw-log store exceeds the total ceiling")
            return receipt.canonical_bytes()
        finally:
            _close_quietly(descriptor)
            _unlink_temporary(handles.store_fd, temporary_name)


def _reject_duplicate_pairs(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    value: dict[str, Any] = {}
    for key, item in pairs:
        _require(key not in value, "raw-log receipt contains a duplicate object key")
        value[key] = item
    return value


def _canonical_receipt_bytes(value: dict[str, Any]) -> bytes:
    return (
        json.dumps(
            value,
            ensure_ascii=False,
            allow_nan=False,
            sort_keys=True,
            separators=(",", ":"),
        ).encode("utf-8")
        + b"\n"
    )


def parse_receipt(raw: bytes, *, limits: LogLimits = FROZEN_LIMITS) -> RawLogReceipt:
    """Validate exact receipt bytes without accessing private storage."""

    _validate_limits(limits)
    _require(isinstance(raw, bytes), "raw-log receipt must be bytes")
    _require(len(raw) <= MAX_RECEIPT_BYTES, "raw-log receipt exceeds its byte ceiling")
    try:
        value = json.loads(raw.decode("utf-8"), object_pairs_hook=_reject_duplicate_pairs)
    except PrivateLogError:
        raise
    except (UnicodeError, json.JSONDecodeError):
        raise PrivateLogError("raw-log receipt is not valid UTF-8 JSON") from None
    _require(isinstance(value, dict), "raw-log receipt root must be an object")
    _require(
        set(value) == {"raw_log_sha256", "raw_log_byte_count"},
        "raw-log receipt fields differ",
    )
    digest = value["raw_log_sha256"]
    byte_count = value["raw_log_byte_count"]
    _require(isinstance(digest, str) and SHA256_RE.fullmatch(digest) is not None, "raw-log receipt SHA-256 is invalid")
    _require(
        type(byte_count) is int and 0 <= byte_count <= limits.max_bytes_per_arm,
        "raw-log receipt byte count is invalid",
    )
    receipt = RawLogReceipt(raw_log_sha256=digest, raw_log_byte_count=byte_count)
    _require(raw == receipt.canonical_bytes(), "raw-log receipt bytes are not canonical")
    return receipt


def check_raw_log(
    private_root: pathlib.Path,
    receipt_raw: bytes,
    *,
    limits: LogLimits = FROZEN_LIMITS,
) -> RawLogReceipt:
    """Check exact public receipt bytes against one private content address."""

    receipt = parse_receipt(receipt_raw, limits=limits)
    with _open_private_store(pathlib.Path(private_root), create=False) as handles:
        inventory, total = _scan_store(handles.store_fd, limits)
        _require(total <= limits.max_bytes_total, "private raw-log store exceeds the total ceiling")
        _require(
            inventory.get(receipt.raw_log_sha256) == receipt.raw_log_byte_count,
            "raw-log receipt content address is absent",
        )
        _verify_content_file(handles.store_fd, receipt, limits)
    return receipt
