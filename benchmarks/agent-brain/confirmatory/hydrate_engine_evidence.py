#!/usr/bin/env python3
"""Safely hydrate the pinned three-engine evidence archive.

All expected identities and sizes come from ``engine-evidence-storage.json``.
Callers may choose where the bytes come from, but cannot weaken those checks.
The destination is published only after the complete archive and extracted tree
have been validated.
"""

from __future__ import annotations

import argparse
import contextlib
import ctypes
import errno
import hashlib
import json
import ntpath
import os
import pathlib
import shutil
import stat
import sys
import tarfile
import tempfile
import urllib.parse
import urllib.request
from dataclasses import dataclass
from typing import Any, BinaryIO, Callable, Iterator, Mapping, Sequence


HERE = pathlib.Path(__file__).resolve().parent
REPO = HERE.parents[2]
DEFAULT_CONTRACT_PATH = HERE / "engine-evidence-storage.json"
COPY_CHUNK_BYTES = 1024 * 1024
DOWNLOAD_TIMEOUT_SECONDS = 60
RENAME_EXCL = 0x00000004  # Darwin renamex_np(2)
RENAME_NOREPLACE = 0x00000001  # Linux renameat2(2)
AT_FDCWD = -100


class HydrationError(RuntimeError):
    """Raised when evidence hydration cannot be established safely."""


@dataclass(frozen=True)
class StorageContract:
    asset_size_bytes: int
    asset_sha256: str
    archive_format: str
    archive_root: str
    regular_file_count: int
    logical_bytes: int
    manifest_path: pathlib.PurePosixPath
    manifest_sha256: str
    repository: str
    release_tag: str
    asset_name: str
    asset_url: str
    publication_disposition: str
    privacy_review: str
    published: bool
    release_id: int | None
    asset_id: int | None
    release_immutable: bool
    release_target_commitish: str | None
    asset_api_digest: str | None
    verified_at: str | None
    repo_relative_parent: pathlib.PurePosixPath

    @property
    def release_ready(self) -> bool:
        return (
            self.publication_disposition == "approved"
            and self.privacy_review == "publishable"
            and self.published
            and self.release_immutable
            and _is_int(self.release_id)
            and self.release_id > 0
            and _is_int(self.asset_id)
            and self.asset_id > 0
            and isinstance(self.release_target_commitish, str)
            and len(self.release_target_commitish) == 40
            and self.asset_api_digest == f"sha256:{self.asset_sha256}"
            and isinstance(self.verified_at, str)
            and bool(self.verified_at.strip())
        )


@dataclass(frozen=True)
class ArchiveMember:
    name: str
    parts: tuple[str, ...]
    is_file: bool
    size: int
    executable: bool


@dataclass(frozen=True)
class ArchiveInventory:
    members: tuple[ArchiveMember, ...]
    regular_file_count: int
    logical_bytes: int


def _require(condition: bool, message: str) -> None:
    if not condition:
        raise HydrationError(message)


def _is_int(value: Any) -> bool:
    return type(value) is int


def _sha256(value: Any, field: str) -> str:
    _require(
        isinstance(value, str)
        and len(value) == 64
        and value == value.lower()
        and all(character in "0123456789abcdef" for character in value),
        f"{field} must be a lowercase SHA-256 hex digest",
    )
    return value


def _mapping(value: Any, field: str) -> Mapping[str, Any]:
    _require(isinstance(value, dict), f"{field} must be an object")
    return value


def _positive_int(value: Any, field: str) -> int:
    _require(_is_int(value) and value > 0, f"{field} must be a positive integer")
    return value


def _nonempty_string(value: Any, field: str) -> str:
    _require(isinstance(value, str) and bool(value.strip()), f"{field} must be a non-empty string")
    return value.strip()


def _safe_relative_path(value: Any, field: str) -> pathlib.PurePosixPath:
    text = _nonempty_string(value, field)
    _require("\\" not in text and not ntpath.splitdrive(text)[0], f"{field} must use a relative POSIX path")
    path = pathlib.PurePosixPath(text)
    _require(not path.is_absolute(), f"{field} must be relative")
    _require(
        all(part not in {"", ".", ".."} for part in path.parts),
        f"{field} must not contain empty, dot, or parent components",
    )
    return path


def _object_without_duplicate_keys(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise HydrationError(f"storage contract contains duplicate key {key!r}")
        result[key] = value
    return result


def load_contract(path: pathlib.Path) -> StorageContract:
    """Load the fail-closed subset needed to authenticate and place the asset."""

    try:
        metadata = path.lstat()
    except OSError as exc:
        raise HydrationError(f"storage contract cannot be opened: {exc}") from exc
    _require(stat.S_ISREG(metadata.st_mode) and not path.is_symlink(), "storage contract must be one regular file")
    try:
        raw = json.loads(path.read_text(encoding="utf-8"), object_pairs_hook=_object_without_duplicate_keys)
    except HydrationError:
        raise
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise HydrationError(f"storage contract is not valid UTF-8 JSON: {exc}") from exc

    contract = _mapping(raw, "storage contract")
    _require(contract.get("schema_version") == 1, "unsupported engine evidence storage contract schema")
    storage = _mapping(contract.get("storage"), "storage")
    archive = _mapping(contract.get("archive"), "archive")
    evidence = _mapping(contract.get("evidence"), "evidence")
    hydration = _mapping(contract.get("hydration"), "hydration")

    _require(
        storage.get("kind") == "github_immutable_release_asset",
        "storage.kind must be github_immutable_release_asset",
    )
    repository = _nonempty_string(storage.get("repository"), "storage.repository")
    repository_parts = repository.split("/")
    _require(
        len(repository_parts) == 2 and all(part not in {"", ".", ".."} for part in repository_parts),
        "storage.repository must be an owner/repository pair",
    )
    release_tag = _nonempty_string(storage.get("tag"), "storage.tag")
    asset_name = _nonempty_string(storage.get("asset_name"), "storage.asset_name")
    _require(
        asset_name == pathlib.PurePosixPath(asset_name).name and "\\" not in asset_name,
        "storage.asset_name must be a basename",
    )
    asset_url = _nonempty_string(storage.get("asset_url"), "storage.asset_url")
    _validate_download_url(asset_url)
    publication_disposition = storage.get("publication_disposition")
    privacy_review = storage.get("privacy_review")
    _require(
        publication_disposition in {"regeneration_required", "approved"},
        "storage.publication_disposition is invalid",
    )
    _require(privacy_review in {"fail", "publishable"}, "storage.privacy_review is invalid")
    published = storage.get("published")
    release_immutable = storage.get("release_immutable")
    _require(type(published) is bool, "storage.published must be a boolean")
    _require(type(release_immutable) is bool, "storage.release_immutable must be a boolean")
    release_id = storage.get("release_id")
    asset_id = storage.get("asset_id")
    _require(release_id is None or (_is_int(release_id) and release_id > 0), "storage.release_id is invalid")
    _require(asset_id is None or (_is_int(asset_id) and asset_id > 0), "storage.asset_id is invalid")
    release_target_commitish = storage.get("release_target_commitish")
    _require(
        release_target_commitish is None
        or (
            isinstance(release_target_commitish, str)
            and len(release_target_commitish) == 40
            and all(character in "0123456789abcdef" for character in release_target_commitish)
        ),
        "storage.release_target_commitish is invalid",
    )
    asset_api_digest = storage.get("asset_api_digest")
    _require(
        asset_api_digest is None
        or (
            isinstance(asset_api_digest, str)
            and asset_api_digest.startswith("sha256:")
            and len(asset_api_digest) == 71
            and all(character in "0123456789abcdef" for character in asset_api_digest[7:])
        ),
        "storage.asset_api_digest is invalid",
    )
    verified_at = storage.get("verified_at")
    _require(
        verified_at is None or (isinstance(verified_at, str) and bool(verified_at.strip())),
        "storage.verified_at is invalid",
    )

    asset_size_bytes = _positive_int(storage.get("asset_size_bytes"), "storage.asset_size_bytes")
    asset_sha256 = _sha256(storage.get("asset_sha256"), "storage.asset_sha256")
    _require(archive.get("format") == "tar_zstd", "archive.format must be tar_zstd")
    archive_root = _nonempty_string(archive.get("root"), "archive.root")
    _require(
        archive_root == pathlib.PurePosixPath(archive_root).name
        and archive_root not in {".", ".."}
        and "\\" not in archive_root,
        "archive.root must be one safe path component",
    )
    regular_file_count = _positive_int(archive.get("regular_file_count"), "archive.regular_file_count")
    logical_bytes = _positive_int(archive.get("logical_bytes"), "archive.logical_bytes")
    _require(archive.get("symlink_count") == 0, "archive.symlink_count must be zero")

    manifest_path = _safe_relative_path(evidence.get("manifest_path"), "evidence.manifest_path")
    _require(
        manifest_path.parts[0] == archive_root,
        "evidence.manifest_path must be inside archive.root",
    )
    manifest_sha256 = _sha256(evidence.get("manifest_sha256"), "evidence.manifest_sha256")
    _nonempty_string(evidence.get("recorded_artifact_root"), "evidence.recorded_artifact_root")
    repo_relative_parent = _safe_relative_path(
        hydration.get("repo_relative_parent"), "hydration.repo_relative_parent"
    )

    loaded = StorageContract(
        asset_size_bytes=asset_size_bytes,
        asset_sha256=asset_sha256,
        archive_format="tar_zstd",
        archive_root=archive_root,
        regular_file_count=regular_file_count,
        logical_bytes=logical_bytes,
        manifest_path=manifest_path,
        manifest_sha256=manifest_sha256,
        repository=repository,
        release_tag=release_tag,
        asset_name=asset_name,
        asset_url=asset_url,
        publication_disposition=publication_disposition,
        privacy_review=privacy_review,
        published=published,
        release_id=release_id,
        asset_id=asset_id,
        release_immutable=release_immutable,
        release_target_commitish=release_target_commitish,
        asset_api_digest=asset_api_digest,
        verified_at=verified_at,
        repo_relative_parent=repo_relative_parent,
    )
    _require(asset_url == _derived_release_asset_url(loaded), "storage.asset_url does not match repository, tag, and asset")
    return loaded


def sha256_file(path: pathlib.Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(COPY_CHUNK_BYTES), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _copy_authenticated_stream(
    source: BinaryIO,
    destination: pathlib.Path,
    *,
    expected_size: int,
    expected_sha256: str,
) -> None:
    digest = hashlib.sha256()
    written = 0
    try:
        with destination.open("xb") as output:
            while True:
                chunk = source.read(COPY_CHUNK_BYTES)
                if not chunk:
                    break
                _require(isinstance(chunk, bytes), "archive source returned non-byte data")
                written += len(chunk)
                _require(written <= expected_size, "archive exceeds storage.asset_size_bytes")
                output.write(chunk)
                digest.update(chunk)
            output.flush()
            os.fsync(output.fileno())
    except HydrationError:
        raise
    except OSError as exc:
        raise HydrationError(f"archive could not be staged: {exc}") from exc
    _require(written == expected_size, f"archive size is {written}, expected {expected_size}")
    _require(digest.hexdigest() == expected_sha256, "archive SHA-256 does not match storage contract")


@contextlib.contextmanager
def _open_local_archive(path: pathlib.Path) -> Iterator[BinaryIO]:
    flags = os.O_RDONLY
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        descriptor = os.open(path, flags)
    except OSError as exc:
        raise HydrationError(f"local archive cannot be opened safely: {exc}") from exc
    try:
        metadata = os.fstat(descriptor)
        _require(stat.S_ISREG(metadata.st_mode), "local archive must be a regular file")
        with os.fdopen(descriptor, "rb", closefd=False) as handle:
            yield handle
    finally:
        os.close(descriptor)


def _derived_release_asset_url(contract: StorageContract) -> str:
    owner, repository = contract.repository.split("/", 1)
    return (
        f"https://github.com/{urllib.parse.quote(owner, safe='')}/"
        f"{urllib.parse.quote(repository, safe='')}/releases/download/"
        f"{urllib.parse.quote(contract.release_tag, safe='')}/"
        f"{urllib.parse.quote(contract.asset_name, safe='')}"
    )


def release_asset_url(contract: StorageContract) -> str:
    _require(contract.release_ready, "contracted release asset is not published and immutably attested")
    return contract.asset_url


def _validate_download_url(url: str) -> None:
    parsed = urllib.parse.urlparse(url)
    _require(
        parsed.scheme == "https" and bool(parsed.netloc) and parsed.username is None and parsed.password is None,
        "download URL must be HTTPS and must not contain credentials",
    )


def _download_archive(
    url: str,
    destination: pathlib.Path,
    contract: StorageContract,
    *,
    urlopen: Callable[..., Any] = urllib.request.urlopen,
) -> None:
    _validate_download_url(url)
    request = urllib.request.Request(
        url,
        headers={"Accept": "application/octet-stream", "User-Agent": "entire-brain-evidence-hydrator/1"},
    )
    try:
        response_context = urlopen(request, timeout=DOWNLOAD_TIMEOUT_SECONDS)
        with response_context as response:
            final_url = response.geturl()
            _validate_download_url(final_url)
            content_length = response.headers.get("Content-Length")
            if content_length is not None:
                try:
                    parsed_length = int(content_length)
                except ValueError as exc:
                    raise HydrationError("download Content-Length is not an integer") from exc
                _require(
                    parsed_length == contract.asset_size_bytes,
                    "download Content-Length does not match storage.asset_size_bytes",
                )
            _copy_authenticated_stream(
                response,
                destination,
                expected_size=contract.asset_size_bytes,
                expected_sha256=contract.asset_sha256,
            )
    except HydrationError:
        raise
    except Exception as exc:
        raise HydrationError(f"archive download failed: {exc}") from exc


def _member_parts(name: str) -> tuple[str, ...]:
    _require(isinstance(name, str) and bool(name), "archive member has an empty name")
    _require("\\" not in name and not ntpath.splitdrive(name)[0], f"unsafe archive member path: {name!r}")
    _require(not name.startswith("/"), f"absolute archive member path: {name!r}")
    stripped = name[:-1] if name.endswith("/") else name
    _require(bool(stripped), f"unsafe archive member path: {name!r}")
    parts = tuple(stripped.split("/"))
    _require(
        all(part not in {"", ".", ".."} for part in parts),
        f"archive member contains traversal or ambiguous components: {name!r}",
    )
    return parts


def _open_tar(path: pathlib.Path) -> tarfile.TarFile:
    try:
        return tarfile.open(path, mode="r:zst")
    except (tarfile.TarError, OSError, ValueError) as exc:
        raise HydrationError(f"archive is not a readable tar_zstd file: {exc}") from exc


def inspect_archive(path: pathlib.Path, contract: StorageContract) -> ArchiveInventory:
    members: list[ArchiveMember] = []
    names: set[str] = set()
    regular_paths: set[tuple[str, ...]] = set()
    logical_bytes = 0
    max_members = contract.regular_file_count * 4 + 128
    with _open_tar(path) as archive:
        try:
            for index, member in enumerate(archive, 1):
                _require(index <= max_members, "archive contains implausibly many metadata entries")
                parts = _member_parts(member.name)
                normalized = "/".join(parts)
                _require(normalized not in names, f"duplicate archive member path: {normalized}")
                names.add(normalized)
                _require(parts[0] == contract.archive_root, "archive contains multiple or unexpected roots")
                _require(not member.issym(), f"archive contains a symbolic link: {normalized}")
                _require(not member.islnk(), f"archive contains a hard link: {normalized}")
                _require(not member.isdev() and not member.isfifo(), f"archive contains a device or FIFO: {normalized}")
                _require(member.isfile() or member.isdir(), f"archive contains an unsupported entry: {normalized}")
                _require(not getattr(member, "sparse", None), f"archive contains a sparse file: {normalized}")
                if len(parts) == 1:
                    _require(member.isdir(), "archive root entry must be a directory")
                if member.isdir():
                    _require(member.size == 0, f"archive directory has a non-zero size: {normalized}")
                else:
                    _require(member.size >= 0, f"archive file has a negative size: {normalized}")
                    logical_bytes += member.size
                    _require(
                        logical_bytes <= contract.logical_bytes,
                        "archive logical bytes exceed storage contract",
                    )
                    regular_paths.add(parts)
                members.append(
                    ArchiveMember(
                        name=normalized,
                        parts=parts,
                        is_file=member.isfile(),
                        size=member.size,
                        executable=bool(member.mode & 0o111),
                    )
                )
        except (tarfile.TarError, OSError) as exc:
            raise HydrationError(f"archive metadata cannot be read safely: {exc}") from exc

    for path_parts in regular_paths:
        for length in range(1, len(path_parts)):
            _require(
                path_parts[:length] not in regular_paths,
                f"archive file is also a parent path: {'/'.join(path_parts[:length])}",
            )
    regular_file_count = len(regular_paths)
    _require(
        regular_file_count == contract.regular_file_count,
        f"archive has {regular_file_count} regular files, expected {contract.regular_file_count}",
    )
    _require(
        logical_bytes == contract.logical_bytes,
        f"archive has {logical_bytes} logical bytes, expected {contract.logical_bytes}",
    )
    _require(
        contract.manifest_path.parts in regular_paths,
        "archive does not contain the contracted evidence manifest as a regular file",
    )
    return ArchiveInventory(tuple(members), regular_file_count, logical_bytes)


def _write_member(archive: tarfile.TarFile, member: tarfile.TarInfo, destination: pathlib.Path) -> None:
    source = archive.extractfile(member)
    _require(source is not None, f"archive file cannot be read: {member.name}")
    remaining = member.size
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        descriptor = os.open(destination, flags, 0o600)
        with os.fdopen(descriptor, "wb") as output:
            while remaining:
                chunk = source.read(min(COPY_CHUNK_BYTES, remaining))
                _require(bool(chunk), f"archive file ended early: {member.name}")
                output.write(chunk)
                remaining -= len(chunk)
            _require(source.read(1) == b"", f"archive file exceeds declared size: {member.name}")
            output.flush()
            os.fsync(output.fileno())
        destination.chmod(0o755 if member.mode & 0o111 else 0o644)
    except HydrationError:
        raise
    except OSError as exc:
        raise HydrationError(f"archive member could not be written safely: {member.name}: {exc}") from exc
    finally:
        source.close()


def extract_archive(path: pathlib.Path, destination: pathlib.Path, contract: StorageContract) -> None:
    """Extract manually into a private staging directory after a full scan."""

    destination.mkdir(mode=0o700)
    with _open_tar(path) as archive:
        try:
            for member in archive:
                parts = _member_parts(member.name)
                target = destination.joinpath(*parts)
                if member.isdir():
                    target.mkdir(mode=0o700, parents=True, exist_ok=True)
                else:
                    target.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
                    _write_member(archive, member, target)
        except HydrationError:
            raise
        except (tarfile.TarError, OSError) as exc:
            raise HydrationError(f"archive extraction failed: {exc}") from exc

    for current, directories, _files in os.walk(destination, topdown=False, followlinks=False):
        for directory in directories:
            pathlib.Path(current, directory).chmod(0o755)
    destination.chmod(0o755)


def verify_extracted_tree(destination: pathlib.Path, contract: StorageContract) -> None:
    roots = list(destination.iterdir())
    _require(
        len(roots) == 1 and roots[0].name == contract.archive_root and roots[0].is_dir() and not roots[0].is_symlink(),
        "hydrated evidence does not contain exactly the contracted root directory",
    )
    regular_file_count = 0
    logical_bytes = 0
    for current, directories, files in os.walk(destination, followlinks=False):
        current_path = pathlib.Path(current)
        for name in directories:
            path = current_path / name
            metadata = path.lstat()
            _require(stat.S_ISDIR(metadata.st_mode), f"hydrated tree contains a non-directory: {path}")
        for name in files:
            path = current_path / name
            metadata = path.lstat()
            _require(stat.S_ISREG(metadata.st_mode), f"hydrated tree contains a non-regular file: {path}")
            regular_file_count += 1
            logical_bytes += metadata.st_size
    _require(
        regular_file_count == contract.regular_file_count,
        "hydrated regular-file count does not match storage contract",
    )
    _require(logical_bytes == contract.logical_bytes, "hydrated logical byte count does not match storage contract")
    manifest = destination.joinpath(*contract.manifest_path.parts)
    try:
        metadata = manifest.lstat()
    except OSError as exc:
        raise HydrationError(f"hydrated evidence manifest is missing: {exc}") from exc
    _require(stat.S_ISREG(metadata.st_mode), "hydrated evidence manifest must be a regular file")
    _require(sha256_file(manifest) == contract.manifest_sha256, "hydrated evidence manifest SHA-256 mismatch")


def _fsync_directory(path: pathlib.Path) -> None:
    try:
        descriptor = os.open(path, os.O_RDONLY)
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
    except OSError as exc:
        if exc.errno not in {errno.EINVAL, errno.ENOTSUP}:
            raise HydrationError(f"cannot synchronize directory {path}: {exc}") from exc


def _rename_noreplace(source: pathlib.Path, destination: pathlib.Path) -> None:
    """Publish a directory atomically without an overwrite race where supported."""

    libc = ctypes.CDLL(None, use_errno=True)
    source_bytes = os.fsencode(source)
    destination_bytes = os.fsencode(destination)
    result: int | None = None
    if sys.platform == "darwin" and hasattr(libc, "renamex_np"):
        function = libc.renamex_np
        function.argtypes = [ctypes.c_char_p, ctypes.c_char_p, ctypes.c_uint]
        function.restype = ctypes.c_int
        result = function(source_bytes, destination_bytes, RENAME_EXCL)
    elif sys.platform.startswith("linux") and hasattr(libc, "renameat2"):
        function = libc.renameat2
        function.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint]
        function.restype = ctypes.c_int
        result = function(AT_FDCWD, source_bytes, AT_FDCWD, destination_bytes, RENAME_NOREPLACE)
    if result is not None:
        if result == 0:
            return
        error = ctypes.get_errno()
        if error in {errno.EEXIST, errno.ENOTEMPTY}:
            raise HydrationError(f"destination already exists: {destination}")
        raise HydrationError(f"atomic evidence publication failed: {os.strerror(error)}")

    # Python has no portable no-replace directory rename. The destination is
    # checked immediately before rename on unsupported platforms; production
    # Darwin and Linux paths use the atomic primitives above.
    _require(not os.path.lexists(destination), f"destination already exists: {destination}")
    try:
        os.rename(source, destination)
    except OSError as exc:
        raise HydrationError(f"atomic evidence publication failed: {exc}") from exc


def hydrate(
    contract_path: pathlib.Path,
    destination: pathlib.Path,
    *,
    archive_path: pathlib.Path | None = None,
    download_url: str | None = None,
    release_asset: bool = False,
    urlopen: Callable[..., Any] = urllib.request.urlopen,
) -> pathlib.Path:
    """Authenticate, safely extract, validate, and atomically publish evidence."""

    selected_sources = sum((archive_path is not None, download_url is not None, release_asset))
    _require(selected_sources == 1, "select exactly one archive source")
    contract = load_contract(contract_path)
    destination = destination.expanduser().absolute()
    _require(not os.path.lexists(destination), f"destination already exists: {destination}")
    try:
        destination.parent.mkdir(parents=True, exist_ok=True)
    except OSError as exc:
        raise HydrationError(f"destination parent cannot be created: {exc}") from exc
    _require(destination.parent.is_dir(), "destination parent is not a directory")

    stage = pathlib.Path(tempfile.mkdtemp(prefix=f".{destination.name}.hydrate-", dir=destination.parent))
    staged_archive = stage / contract.asset_name
    staged_tree = stage / "tree"
    try:
        if archive_path is not None:
            with _open_local_archive(archive_path.expanduser()) as source:
                _copy_authenticated_stream(
                    source,
                    staged_archive,
                    expected_size=contract.asset_size_bytes,
                    expected_sha256=contract.asset_sha256,
                )
        else:
            url = release_asset_url(contract) if release_asset else download_url
            assert url is not None
            _download_archive(url, staged_archive, contract, urlopen=urlopen)

        inspect_archive(staged_archive, contract)
        extract_archive(staged_archive, staged_tree, contract)
        verify_extracted_tree(staged_tree, contract)
        _fsync_directory(staged_tree)
        _rename_noreplace(staged_tree, destination)
        _fsync_directory(destination.parent)
    finally:
        shutil.rmtree(stage, ignore_errors=True)
    return destination.joinpath(*contract.manifest_path.parts)


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--contract", type=pathlib.Path, default=DEFAULT_CONTRACT_PATH)
    parser.add_argument(
        "--destination",
        type=pathlib.Path,
        help="atomic destination parent; defaults to hydration.repo_relative_parent from the contract",
    )
    sources = parser.add_mutually_exclusive_group(required=True)
    sources.add_argument("--archive", type=pathlib.Path, help="local copy of the pinned archive")
    sources.add_argument("--url", help="HTTPS location serving the pinned archive bytes")
    sources.add_argument("--release", action="store_true", help="download the contracted GitHub release asset")
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    parser = _parser()
    args = parser.parse_args(argv)
    try:
        contract = load_contract(args.contract)
        destination = args.destination or REPO.joinpath(*contract.repo_relative_parent.parts)
        manifest = hydrate(
            args.contract,
            destination,
            archive_path=args.archive,
            download_url=args.url,
            release_asset=args.release,
        )
    except HydrationError as exc:
        parser.error(str(exc))
    print(manifest)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
