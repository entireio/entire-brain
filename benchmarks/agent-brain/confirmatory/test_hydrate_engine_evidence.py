from __future__ import annotations

import copy
import hashlib
import importlib.util
import io
import json
import os
import pathlib
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest import mock

import public_engine_evidence as PUBLIC
import restricted_replay_attestation as ATTEST


HERE = pathlib.Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location(
    "hydrate_engine_evidence", HERE / "hydrate_engine_evidence.py"
)
assert SPEC and SPEC.loader
HYDRATE = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = HYDRATE
SPEC.loader.exec_module(HYDRATE)


ROOT = "run-fixture-v1"
MANIFEST_NAME = f"{ROOT}/engine-verification.json"
MANIFEST_BYTES = b'{"records":[],"schema_version":2}\n'
PAYLOADS = {
    MANIFEST_NAME: MANIFEST_BYTES,
    f"{ROOT}/artifacts/bin/entire-brain": b"fixture binary\n",
    f"{ROOT}/runs/lexical/result.json": b'{"ok":true}\n',
}


def digest(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def add_file(archive: tarfile.TarFile, name: str, data: bytes, *, mode: int = 0o644) -> None:
    member = tarfile.TarInfo(name)
    member.size = len(data)
    member.mode = mode
    archive.addfile(member, io.BytesIO(data))


def write_archive(
    path: pathlib.Path,
    *,
    payloads: dict[str, bytes] | None = None,
    extra_members: list[tuple[tarfile.TarInfo, bytes | None]] | None = None,
) -> None:
    with tarfile.open(path, "w:zst") as archive:
        root = tarfile.TarInfo(ROOT)
        root.type = tarfile.DIRTYPE
        root.mode = 0o755
        archive.addfile(root)
        for name, data in (PAYLOADS if payloads is None else payloads).items():
            add_file(archive, name, data, mode=0o755 if name.endswith("entire-brain") else 0o644)
        for member, data in extra_members or []:
            archive.addfile(member, None if data is None else io.BytesIO(data))


def make_contract(archive: pathlib.Path, *, payloads: dict[str, bytes] | None = None) -> dict:
    files = PAYLOADS if payloads is None else payloads
    return {
        "schema_version": 1,
        "storage": {
            "kind": "github_immutable_release_asset",
            "repository": "entireio/entire-brain-evidence",
            "tag": "engine-fixture-v1",
            "asset_name": "engine-fixture-v1.tar.zst",
            "asset_url": "https://github.com/entireio/entire-brain-evidence/releases/download/"
            "engine-fixture-v1/engine-fixture-v1.tar.zst",
            "asset_size_bytes": archive.stat().st_size,
            "asset_sha256": HYDRATE.sha256_file(archive),
            "publication_disposition": "regeneration_required",
            "privacy_review": "fail",
            "published": False,
            "release_id": None,
            "asset_id": None,
            "release_immutable": False,
            "release_target_commitish": None,
            "asset_api_digest": None,
            "verified_at": None,
        },
        "archive": {
            "format": "tar_zstd",
            "root": ROOT,
            "regular_file_count": len(files),
            "logical_bytes": sum(map(len, files.values())),
            "symlink_count": 0,
        },
        "evidence": {
            "manifest_path": MANIFEST_NAME,
            "manifest_sha256": digest(MANIFEST_BYTES),
            "recorded_artifact_root": "/fixture/source",
        },
        "recorded_external_inputs": {
            "repo_root": "/fixture/repository",
            "repo_key": "fixture/repository",
        },
        "hydration": {
            "repo_relative_parent": "benchmarks/agent-brain/confirmatory/engine-evidence"
        },
    }


class FakeResponse(io.BytesIO):
    def __init__(self, body: bytes, url: str = "https://example.test/evidence.tar.zst") -> None:
        super().__init__(body)
        self._url = url
        self.headers = {"Content-Length": str(len(body))}

    def geturl(self) -> str:
        return self._url

    def __enter__(self) -> "FakeResponse":
        return self

    def __exit__(self, *_args: object) -> None:
        self.close()


class EngineEvidenceHydratorTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.temp.name)
        self.archive = self.root / "fixture.tar.zst"
        write_archive(self.archive)
        self.contract_data = make_contract(self.archive)
        self.contract = self.root / "contract.json"
        self.write_contract()
        self.destination = self.root / "hydrated"

    def tearDown(self) -> None:
        self.temp.cleanup()

    def write_contract(self) -> None:
        self.contract.write_text(json.dumps(self.contract_data), encoding="utf-8")

    def assert_no_partial_publication(self) -> None:
        self.assertFalse(self.destination.exists())
        self.assertEqual(list(self.root.glob(".hydrated.hydrate-*")), [])

    def test_local_archive_hydrates_and_returns_manifest(self) -> None:
        manifest = HYDRATE.hydrate(
            self.contract,
            self.destination,
            archive_path=self.archive,
        )

        self.assertEqual(manifest, self.destination / MANIFEST_NAME)
        self.assertEqual(manifest.read_bytes(), MANIFEST_BYTES)
        self.assertEqual(
            (self.destination / ROOT / "artifacts/bin/entire-brain").stat().st_mode & 0o777,
            0o755,
        )
        self.assertEqual(list(self.root.glob(".hydrated.hydrate-*")), [])

    def test_download_url_hydrates_from_authenticated_bytes(self) -> None:
        body = self.archive.read_bytes()
        calls: list[tuple[str, int]] = []

        def urlopen(request: object, *, timeout: int) -> FakeResponse:
            calls.append((request.full_url, timeout))
            return FakeResponse(body)

        manifest = HYDRATE.hydrate(
            self.contract,
            self.destination,
            download_url="https://example.test/evidence.tar.zst",
            urlopen=urlopen,
        )

        self.assertTrue(manifest.is_file())
        self.assertEqual(calls, [("https://example.test/evidence.tar.zst", 60)])

    def test_release_asset_requires_complete_immutable_attestation(self) -> None:
        contract = HYDRATE.load_contract(self.contract)
        with self.assertRaisesRegex(HYDRATE.HydrationError, "not published and immutably attested"):
            HYDRATE.release_asset_url(contract)

    def test_release_asset_url_comes_from_complete_contract(self) -> None:
        storage = self.contract_data["storage"]
        storage.update(
            {
                "published": True,
                "publication_disposition": "approved",
                "privacy_review": "publishable",
                "release_id": 123,
                "asset_id": 456,
                "release_immutable": True,
                "release_target_commitish": "a" * 40,
                "asset_api_digest": f"sha256:{storage['asset_sha256']}",
                "verified_at": "2026-07-16T12:00:00Z",
            }
        )
        self.write_contract()
        contract = HYDRATE.load_contract(self.contract)
        self.assertEqual(
            HYDRATE.release_asset_url(contract),
            "https://github.com/entireio/entire-brain-evidence/releases/download/"
            "engine-fixture-v1/engine-fixture-v1.tar.zst",
        )

    def test_refuses_existing_destination_without_changing_it(self) -> None:
        self.destination.mkdir()
        sentinel = self.destination / "sentinel"
        sentinel.write_text("keep", encoding="utf-8")

        with self.assertRaisesRegex(HYDRATE.HydrationError, "destination already exists"):
            HYDRATE.hydrate(self.contract, self.destination, archive_path=self.archive)

        self.assertEqual(sentinel.read_text(encoding="utf-8"), "keep")

    def test_rejects_archive_size_before_extraction(self) -> None:
        self.contract_data["storage"]["asset_size_bytes"] += 1
        self.write_contract()

        with self.assertRaisesRegex(HYDRATE.HydrationError, "archive size"):
            HYDRATE.hydrate(self.contract, self.destination, archive_path=self.archive)

        self.assert_no_partial_publication()

    def test_rejects_archive_digest_before_extraction(self) -> None:
        self.contract_data["storage"]["asset_sha256"] = "0" * 64
        self.write_contract()

        with self.assertRaisesRegex(HYDRATE.HydrationError, "archive SHA-256"):
            HYDRATE.hydrate(self.contract, self.destination, archive_path=self.archive)

        self.assert_no_partial_publication()

    def test_rejects_manifest_digest_and_cleans_staging(self) -> None:
        self.contract_data["evidence"]["manifest_sha256"] = "0" * 64
        self.write_contract()

        with self.assertRaisesRegex(HYDRATE.HydrationError, "manifest SHA-256"):
            HYDRATE.hydrate(self.contract, self.destination, archive_path=self.archive)

        self.assert_no_partial_publication()

    def test_rejects_file_count_and_logical_byte_mismatches(self) -> None:
        for field, message in (
            ("regular_file_count", "regular files"),
            ("logical_bytes", "logical bytes"),
        ):
            with self.subTest(field=field):
                changed = copy.deepcopy(self.contract_data)
                changed["archive"][field] += 1
                self.contract.write_text(json.dumps(changed), encoding="utf-8")
                with self.assertRaisesRegex(HYDRATE.HydrationError, message):
                    HYDRATE.hydrate(self.contract, self.destination, archive_path=self.archive)
                self.assert_no_partial_publication()

    def test_rejects_unsafe_archive_entry_types_and_paths(self) -> None:
        cases: list[tuple[str, tarfile.TarInfo, bytes | None, str]] = []

        absolute = tarfile.TarInfo("/absolute")
        absolute.size = 1
        cases.append(("absolute", absolute, b"x", "absolute archive member"))

        traversal = tarfile.TarInfo(f"{ROOT}/../escape")
        traversal.size = 1
        cases.append(("traversal", traversal, b"x", "traversal"))

        second_root = tarfile.TarInfo("other-root/file")
        second_root.size = 1
        cases.append(("multiple roots", second_root, b"x", "unexpected roots"))

        symbolic = tarfile.TarInfo(f"{ROOT}/symbolic")
        symbolic.type = tarfile.SYMTYPE
        symbolic.linkname = "target"
        cases.append(("symbolic link", symbolic, None, "symbolic link"))

        hard = tarfile.TarInfo(f"{ROOT}/hard")
        hard.type = tarfile.LNKTYPE
        hard.linkname = MANIFEST_NAME
        cases.append(("hard link", hard, None, "hard link"))

        fifo = tarfile.TarInfo(f"{ROOT}/fifo")
        fifo.type = tarfile.FIFOTYPE
        cases.append(("fifo", fifo, None, "device or FIFO"))

        character = tarfile.TarInfo(f"{ROOT}/device")
        character.type = tarfile.CHRTYPE
        character.devmajor = 1
        character.devminor = 3
        cases.append(("device", character, None, "device or FIFO"))

        for label, member, data, message in cases:
            with self.subTest(label=label):
                archive = self.root / f"{label.replace(' ', '-')}.tar.zst"
                write_archive(archive, extra_members=[(member, data)])
                contract = make_contract(archive)
                contract["archive"]["regular_file_count"] += int(member.isfile())
                contract["archive"]["logical_bytes"] += member.size if member.isfile() else 0
                self.contract.write_text(json.dumps(contract), encoding="utf-8")
                with self.assertRaisesRegex(HYDRATE.HydrationError, message):
                    HYDRATE.hydrate(self.contract, self.destination, archive_path=archive)
                self.assert_no_partial_publication()

    def test_rejects_duplicate_member_paths(self) -> None:
        duplicate = tarfile.TarInfo(MANIFEST_NAME)
        duplicate.size = len(MANIFEST_BYTES)
        archive = self.root / "duplicate.tar.zst"
        write_archive(archive, extra_members=[(duplicate, MANIFEST_BYTES)])
        contract = make_contract(archive)
        contract["archive"]["regular_file_count"] += 1
        contract["archive"]["logical_bytes"] += len(MANIFEST_BYTES)
        self.contract.write_text(json.dumps(contract), encoding="utf-8")

        with self.assertRaisesRegex(HYDRATE.HydrationError, "duplicate archive member"):
            HYDRATE.hydrate(self.contract, self.destination, archive_path=archive)

        self.assert_no_partial_publication()

    def test_rejects_manifest_outside_archive_root(self) -> None:
        self.contract_data["evidence"]["manifest_path"] = "other/engine-verification.json"
        self.write_contract()

        with self.assertRaisesRegex(HYDRATE.HydrationError, "inside archive.root"):
            HYDRATE.hydrate(self.contract, self.destination, archive_path=self.archive)

    def test_rejects_non_https_or_credentialed_download_url(self) -> None:
        for url in (
            "http://example.test/evidence.tar.zst",
            "https://user:password@example.test/evidence.tar.zst",
        ):
            with self.subTest(url=url):
                with self.assertRaisesRegex(HYDRATE.HydrationError, "download URL must be HTTPS"):
                    HYDRATE.hydrate(
                        self.contract,
                        self.destination,
                        download_url=url,
                        urlopen=lambda *_args, **_kwargs: self.fail("network must not be opened"),
                    )
                self.assert_no_partial_publication()

    def test_rejects_duplicate_contract_keys(self) -> None:
        self.contract.write_text('{"schema_version":1,"schema_version":1}', encoding="utf-8")

        with self.assertRaisesRegex(HYDRATE.HydrationError, "duplicate key"):
            HYDRATE.load_contract(self.contract)


class PublicV4PackageHydrationTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = pathlib.Path(self.temp.name)
        self.bundle = self.root / "bundle"
        (self.bundle / "arms").mkdir(parents=True)
        for relative in HYDRATE.PUBLIC_SOURCE_PATHS:
            path = self.bundle.joinpath(*pathlib.PurePosixPath(relative).parts)
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(json.dumps({"fixture": relative}, sort_keys=True) + "\n", encoding="utf-8")

    def test_deterministic_package_has_exact_tree_and_hydrates_same_bytes(self) -> None:
        first_path = self.root / "first.tar.zst"
        second_path = self.root / "second.tar.zst"
        first = HYDRATE.package_public_v4(self.bundle, first_path)
        second = HYDRATE.package_public_v4(self.bundle, second_path)

        self.assertEqual(first.archive_sha256, second.archive_sha256)
        self.assertEqual(first_path.read_bytes(), second_path.read_bytes())
        self.assertEqual(first.regular_file_count, 6)
        self.assertEqual(first.directory_count, 2)
        self.assertEqual(first.payload_file_count, 5)
        entries = HYDRATE.inspect_public_archive(first_path)
        self.assertEqual(tuple(item.path for item in entries), HYDRATE.PUBLIC_ARCHIVE_PATHS)
        self.assertEqual(HYDRATE.public_tree_inventory_sha256(entries), first.tree_inventory_sha256)

        destination = self.root / "hydrated"
        manifest = HYDRATE.extract_public_archive(
            first_path,
            destination,
            expected_size=first.archive_size_bytes,
            expected_sha256=first.archive_sha256,
            expected_tree_sha256=first.tree_inventory_sha256,
        )
        self.assertEqual(manifest, destination / HYDRATE.PUBLIC_MANIFEST_PATH)
        for relative in HYDRATE.PUBLIC_SOURCE_PATHS:
            expected = self.bundle.joinpath(*pathlib.PurePosixPath(relative).parts).read_bytes()
            actual = (destination / HYDRATE.PUBLIC_ARCHIVE_ROOT).joinpath(
                *pathlib.PurePosixPath(relative).parts
            ).read_bytes()
            self.assertEqual(actual, expected)

        drifted = destination / HYDRATE.PUBLIC_ARCHIVE_ROOT / "temporal-projection.json"
        drifted.chmod(0o600)
        with self.assertRaisesRegex(HYDRATE.HydrationError, "file mode differs"):
            HYDRATE.verify_public_tree(
                destination / HYDRATE.PUBLIC_ARCHIVE_ROOT,
                first.tree_inventory_sha256,
            )

    def test_captured_public_snapshot_cannot_be_substituted_after_capture(self) -> None:
        archive = self.root / "public.tar.zst"
        package = HYDRATE.package_public_v4(self.bundle, archive)
        hydrated = self.root / "hydrated"
        HYDRATE.extract_public_archive(
            archive,
            hydrated,
            expected_size=package.archive_size_bytes,
            expected_sha256=package.archive_sha256,
            expected_tree_sha256=package.tree_inventory_sha256,
        )
        public_root = hydrated / HYDRATE.PUBLIC_ARCHIVE_ROOT
        payloads, _entries = HYDRATE.capture_public_tree(public_root, package.tree_inventory_sha256)
        original = payloads[str(HYDRATE.PUBLIC_MANIFEST_PATH)]
        (public_root / HYDRATE.PUBLIC_MANIFEST_NAME).write_bytes(b'{"substituted":true}\n')
        snapshot_parent = self.root / "snapshot"
        snapshot_parent.mkdir(mode=0o700)
        snapshot_manifest = HYDRATE._materialize_public_snapshot(payloads, snapshot_parent)
        self.assertEqual(snapshot_manifest.read_bytes(), original)
        self.assertNotEqual(snapshot_manifest.read_bytes(), (public_root / HYDRATE.PUBLIC_MANIFEST_NAME).read_bytes())

    def test_package_rejects_extra_missing_link_special_and_case_collision(self) -> None:
        cases: list[tuple[str, callable]] = [
            ("extra", lambda: (self.bundle / "extra.json").write_text("{}\n", encoding="utf-8")),
            ("missing", lambda: (self.bundle / "temporal-projection.json").unlink()),
            ("symlink", lambda: self._replace_with_symlink("temporal-projection.json")),
        ]
        for label, mutate in cases:
            with self.subTest(label=label):
                with tempfile.TemporaryDirectory() as raw:
                    copied = pathlib.Path(raw) / "bundle"
                    shutil.copytree(self.bundle, copied)
                    original = self.bundle
                    self.bundle = copied
                    try:
                        mutate()
                        with self.assertRaises(HYDRATE.HydrationError):
                            HYDRATE.package_public_v4(self.bundle, pathlib.Path(raw) / "out.tar.zst")
                    finally:
                        self.bundle = original

    def test_casefold_and_unicode_collision_key_is_filesystem_independent(self) -> None:
        self.assertEqual(HYDRATE._public_path_key("public-v4-v1/arms"), HYDRATE._public_path_key("public-v4-v1/ARMS"))
        entries = (
            HYDRATE.PublicTreeEntry("directory", "public-v4-v1/ARMS", 0o755, "0" * 64, 0),
            HYDRATE.PublicTreeEntry("directory", "public-v4-v1/arms", 0o755, "0" * 64, 0),
        )
        with self.assertRaisesRegex(HYDRATE.HydrationError, "collision"):
            HYDRATE.public_tree_inventory_sha256(entries)

    def _replace_with_symlink(self, relative: str) -> None:
        target = self.bundle / relative
        other = self.bundle / "arms/lexical_handrolled.json"
        target.unlink()
        target.symlink_to(other)

    def test_inspector_rejects_noncanonical_pax_mode_and_link(self) -> None:
        cases = ("pax", "mode", "link")
        for case in cases:
            with self.subTest(case=case):
                path = self.root / f"{case}.tar.zst"
                with tarfile.open(path, "w:zst") as archive:
                    for index, name in enumerate(HYDRATE.PUBLIC_ARCHIVE_PATHS):
                        directory = index < 2
                        member = tarfile.TarInfo(name)
                        member.type = tarfile.DIRTYPE if directory else tarfile.REGTYPE
                        member.mode = 0o755 if directory else 0o644
                        member.uid = member.gid = 0
                        member.uname = member.gname = ""
                        member.mtime = 0
                        body = b"" if directory else b"{}\n"
                        member.size = len(body)
                        if case == "pax" and name.endswith("temporal-projection.json"):
                            member.pax_headers = {"comment": "forbidden"}
                        if case == "mode" and name.endswith("temporal-projection.json"):
                            member.mode = 0o600
                        if case == "link" and name.endswith("temporal-projection.json"):
                            member.type = tarfile.SYMTYPE
                            member.linkname = "arms/lexical_handrolled.json"
                            member.size = 0
                        archive.addfile(member, None if directory or member.issym() else io.BytesIO(body))
                with self.assertRaises(HYDRATE.HydrationError):
                    HYDRATE.inspect_public_archive(path)

    def test_inspector_rejects_repeated_slash_and_dot_member_spellings(self) -> None:
        for label, replacement in (
            ("repeated slash", "public-v4-v1//arms/embeddinggemma_rrf.json"),
            ("dot component", "public-v4-v1/./arms/embeddinggemma_rrf.json"),
        ):
            with self.subTest(label=label):
                path = self.root / f"{label.replace(' ', '-')}.tar.zst"
                with tarfile.open(path, "w:zst") as archive:
                    for index, name in enumerate(HYDRATE.PUBLIC_ARCHIVE_PATHS):
                        if name == "public-v4-v1/arms/embeddinggemma_rrf.json":
                            name = replacement
                        directory = index < 2
                        member = tarfile.TarInfo(name)
                        member.type = tarfile.DIRTYPE if directory else tarfile.REGTYPE
                        member.mode = 0o755 if directory else 0o644
                        member.uid = member.gid = 0
                        member.uname = member.gname = ""
                        member.mtime = 0
                        body = b"" if directory else b"{}\n"
                        member.size = len(body)
                        archive.addfile(member, None if directory else io.BytesIO(body))
                with self.assertRaisesRegex(HYDRATE.HydrationError, "spelling is noncanonical"):
                    HYDRATE.inspect_public_archive(path)

    def test_extract_authenticates_staged_bytes_and_leaves_no_partial_tree(self) -> None:
        archive = self.root / "public.tar.zst"
        package = HYDRATE.package_public_v4(self.bundle, archive)
        destination = self.root / "hydrated"
        with self.assertRaisesRegex(HYDRATE.HydrationError, "SHA-256 differs"):
            HYDRATE.extract_public_archive(
                archive,
                destination,
                expected_size=package.archive_size_bytes,
                expected_sha256="0" * 64,
                expected_tree_sha256=package.tree_inventory_sha256,
            )
        self.assertFalse((destination / HYDRATE.PUBLIC_ARCHIVE_ROOT).exists())
        self.assertEqual(list(destination.glob(".public-v4.hydrate-*")), [])

    def test_restricted_hydration_requires_local_owner_0600_and_is_atomic(self) -> None:
        source = self.root / "attestation.json"
        # Structure validation happens after the authenticated copy.  This test
        # exercises the local-file contract with a deliberately invalid body.
        source.write_text("{}\n", encoding="utf-8")
        source.chmod(0o644)
        destination = self.root / "hydrated"
        with self.assertRaisesRegex(HYDRATE.HydrationError, "mode 0600"):
            HYDRATE.hydrate_restricted_attestation(
                source,
                destination,
                expected_size=source.stat().st_size,
                expected_sha256=HYDRATE.sha256_file(source),
            )
        self.assertFalse((destination / "restricted").exists())


class StorageV2ContractTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = pathlib.Path(self.temp.name)
        canonical = HERE / "engine-evidence-storage.json"
        self.contract_data = json.loads(canonical.read_text(encoding="utf-8"))
        self.contract = self.root / "contract.json"
        self.write_contract()

    def write_contract(self) -> None:
        self.contract.write_text(json.dumps(self.contract_data, sort_keys=True) + "\n", encoding="utf-8")

    def test_canonical_pending_contract_is_structurally_valid_but_not_approved(self) -> None:
        loaded = HYDRATE.load_storage_contract_v2(self.contract, verify_external_locks=False)
        self.assertEqual(loaded["contract_status"], "pending_owner_authorization")
        self.assertEqual(loaded["expected_bindings"]["replay_result"]["decision"], "pending")

    def test_full_hydration_and_legacy_paths_are_frozen(self) -> None:
        self.contract_data["hydration"]["repo_relative_parent"] = ".engine-evidence"
        self.write_contract()
        with self.assertRaisesRegex(HYDRATE.HydrationError, "hydration paths changed"):
            HYDRATE.load_storage_contract_v2(self.contract, verify_external_locks=False)

        self.contract_data = json.loads((HERE / "engine-evidence-storage.json").read_text(encoding="utf-8"))
        self.contract_data["legacy_v1"]["descriptor_path"] = "engine-evidence-storage-legacy-v1.json"
        self.write_contract()
        with self.assertRaisesRegex(HYDRATE.HydrationError, "legacy diagnostic binding changed"):
            HYDRATE.load_storage_contract_v2(self.contract, verify_external_locks=False)

    def test_pending_status_rejects_pass_published_nonnull_and_bool_integer_aliases(self) -> None:
        mutations = (
            ("pass", lambda value: value["expected_bindings"]["replay_result"].__setitem__("decision", "pass"), "pending replay result"),
            ("published", lambda value: value["public"]["storage"].__setitem__("published", True), "booleans must be false"),
            ("archive hash", lambda value: value["public"]["storage"].__setitem__("asset_sha256", "a" * 64), "owner fields must be null"),
            ("payload count bool", lambda value: value["public"]["evidence"].__setitem__("payload_file_count", True), "integer 5"),
            ("directory count float", lambda value: value["public"]["archive"].__setitem__("directory_count", 2.0), "integer 2"),
        )
        for label, mutate, message in mutations:
            with self.subTest(label=label):
                changed = copy.deepcopy(self.contract_data)
                mutate(changed)
                self.contract.write_text(json.dumps(changed) + "\n", encoding="utf-8")
                with self.assertRaisesRegex(HYDRATE.HydrationError, message):
                    HYDRATE.load_storage_contract_v2(self.contract, verify_external_locks=False)

    def test_approved_contract_rejects_any_pending_null(self) -> None:
        self.contract_data["contract_status"] = "approved"
        self.write_contract()
        with self.assertRaisesRegex(HYDRATE.HydrationError, "approved storage contract contains null"):
            HYDRATE.load_storage_contract_v2(self.contract, verify_external_locks=False)

    def test_production_verification_rejects_callable_injection_before_io(self) -> None:
        with self.assertRaisesRegex(HYDRATE.HydrationError, "does not accept injected"):
            HYDRATE.verify_hydrated_storage_v2(
                self.contract,
                matrix={},
                pins={},
                attestation_verifier=lambda _envelope, _roots: None,
                verify_external_locks=True,
            )

    def test_duplicate_keys_and_legacy_v1_cannot_be_interpreted_as_v2(self) -> None:
        self.contract.write_text('{"schema_version":2,"schema_version":2}\n', encoding="utf-8")
        with self.assertRaisesRegex(HYDRATE.HydrationError, "duplicate key"):
            HYDRATE.load_storage_contract_v2(self.contract, verify_external_locks=False)
        with self.assertRaisesRegex(HYDRATE.HydrationError, "must contain exactly"):
            HYDRATE.load_storage_contract_v2(HERE / "engine-evidence-storage-legacy-v1.json", verify_external_locks=False)

    def test_json_complexity_failures_are_translated(self) -> None:
        malformed = (
            b'{"value":' + b"1" * 5000 + b"}",
            b'{"value":' + b"[" * 2000 + b"0" + b"]" * 2000 + b"}",
        )
        for raw in malformed:
            with self.subTest(size=len(raw)), self.assertRaisesRegex(HYDRATE.HydrationError, "not valid UTF-8 JSON|nesting bound"):
                HYDRATE._decode_storage_json(raw, "fixture")

    def test_rfc3339_parser_rejects_permissive_iso8601_variants(self) -> None:
        for value in (
            "2026-07-16 12:10:00+00:00",
            "2026-07-16T12:10:00+0000",
            "2026-07-16T12:10:00",
            "2026-07-16T12:10:00+24:00",
            "2026-07-16T12:10:00.123456789Z",
            "2026-07-16T12:10:00-00:00",
        ):
            with self.subTest(value=value):
                with self.assertRaisesRegex(HYDRATE.HydrationError, "must be RFC3339"):
                    HYDRATE._rfc3339(value, "fixture timestamp")


class ApprovedTwoRootHydrationTest(unittest.TestCase):
    def setUp(self) -> None:
        if shutil.which("ssh-keygen") is None:
            self.skipTest("ssh-keygen is unavailable")
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.repo = pathlib.Path(self.temp.name) / "repo"
        self.here = self.repo / "benchmarks/agent-brain/confirmatory"
        self.here.mkdir(parents=True)
        self.bundle = pathlib.Path(self.temp.name) / "bundle"
        (self.bundle / "arms").mkdir(parents=True)
        for relative in HYDRATE.PUBLIC_SOURCE_PATHS:
            if relative == HYDRATE.PUBLIC_MANIFEST_NAME:
                continue
            path = self.bundle.joinpath(*pathlib.PurePosixPath(relative).parts)
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(json.dumps({"fixture": relative}, sort_keys=True) + "\n", encoding="utf-8")
        payload_bytes = sum(
            self.bundle.joinpath(*pathlib.PurePosixPath(relative).parts).stat().st_size
            for relative in HYDRATE.PUBLIC_SOURCE_PATHS
            if relative != HYDRATE.PUBLIC_MANIFEST_NAME
        )
        self.inventory_root = "7" * 64
        (self.bundle / HYDRATE.PUBLIC_MANIFEST_NAME).write_text(
            json.dumps(
                {
                    "schema_version": 4,
                    "artifact_inventory": {
                        "algorithm": ATTEST.PUBLIC_INVENTORY_ALGORITHM,
                        "root_sha256": self.inventory_root,
                        "file_count": 5,
                        "logical_bytes": payload_bytes,
                    },
                },
                sort_keys=True,
                separators=(",", ":"),
            )
            + "\n",
            encoding="utf-8",
        )
        self.archive = pathlib.Path(self.temp.name) / "public.tar.zst"
        self.package = HYDRATE.package_public_v4(self.bundle, self.archive)
        self._build_signed_contract()

    def _build_signed_contract(self) -> None:
        key = pathlib.Path(self.temp.name) / "fixture-key"
        subprocess.run(
            ["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "", "-f", str(key)],
            check=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env={"LC_ALL": "C", "LANG": "C", "PATH": os.environ.get("PATH", "")},
        )
        public_key = key.with_suffix(".pub").read_text(encoding="ascii").split()[1]
        principal = "two-root-fixture@example.invalid"
        root_id = "two-root-fixture-ed25519"
        trust = {
            "schema_version": 1,
            "profile": ATTEST.TRUST_PROFILE,
            "status": "approved",
            "signature_namespace": ATTEST.SIGNATURE_NAMESPACE,
            "roots": [
                {
                    "root_id": root_id,
                    "principal": principal,
                    "key_type": "ssh-ed25519",
                    "public_key_base64": public_key,
                    "public_key_sha256": ATTEST.sha256_bytes(ATTEST.public_key_line(public_key)),
                    "status": "active",
                    "not_before": "2026-07-16T00:00:00Z",
                    "not_after": "2026-07-17T00:00:00Z",
                    "revoked_at": None,
                }
            ],
        }
        (self.here / "engine-replay-trust-roots.json").write_bytes(ATTEST.canonical_json_bytes(trust) + b"\n")

        contract = json.loads((HERE / "engine-evidence-storage.json").read_text(encoding="utf-8"))
        contract["contract_status"] = "approved"
        source_commit = "b" * 40
        tag = "engine-public-v4-fixture"
        asset_name = "engine-public-v4-fixture.tar.zst"
        url = f"https://github.com/entireio/entire-brain/releases/download/{tag}/{asset_name}"
        public_storage = contract["public"]["storage"]
        public_storage.update(
            {
                "tag": tag,
                "asset_name": asset_name,
                "asset_url": url,
                "asset_size_bytes": self.package.archive_size_bytes,
                "asset_sha256": self.package.archive_sha256,
                "publication_disposition": "approved",
                "privacy_review": "publishable",
                "published": True,
                "release_id": 11,
                "asset_id": 12,
                "release_immutable": True,
                "release_target_commitish": source_commit,
                "asset_api_digest": f"sha256:{self.package.archive_sha256}",
                "verified_at": "2026-07-16T12:00:00Z",
            }
        )
        contract["public"]["archive"].update(
            {
                "logical_bytes": self.package.logical_bytes,
                "tree_inventory_sha256": self.package.tree_inventory_sha256,
            }
        )
        public_evidence = contract["public"]["evidence"]
        public_evidence.update(
            {
                "manifest_sha256": self.package.manifest_sha256,
                "manifest_size_bytes": self.package.manifest_size_bytes,
                "artifact_inventory_root_sha256": self.inventory_root,
                "payload_logical_bytes": self.package.payload_logical_bytes,
            }
        )
        contract["restricted"]["storage"].update(
            {
                "provider_id": "fixture-provider",
                "object_id": "fixture-object",
                "version_id": "fixture-version-1",
                "retention_status": "approved",
                "access_control_status": "approved",
                "immutable": True,
                "verified_at": "2026-07-16T12:10:00Z",
            }
        )
        bindings = contract["expected_bindings"]
        bindings["issued_at"] = "2026-07-16T12:05:00Z"
        bindings["private_diagnostic"]["manifest_size_bytes"] = 123
        bindings["public_projection"].update(
            {
                "manifest_sha256": self.package.manifest_sha256,
                "manifest_size_bytes": self.package.manifest_size_bytes,
                "artifact_inventory_root_sha256": self.inventory_root,
                "payload_logical_bytes": self.package.payload_logical_bytes,
            }
        )
        bindings["source_identity"].update(
            {"commit_oid": source_commit, "tree_oid": "c" * 40, "worktree_state": "clean"}
        )
        bindings["replay_result"].update(
            {
                "private_error_count": 0,
                "public_error_count": 0,
                "checker_lock_valid": True,
                "analyzer_lock_valid": True,
                "decision": "pass",
            }
        )
        bindings["public_archive"].update(
            {
                "sha256": self.package.archive_sha256,
                "size_bytes": self.package.archive_size_bytes,
                "logical_bytes": self.package.logical_bytes,
                "tree_inventory_sha256": self.package.tree_inventory_sha256,
                "tag": tag,
                "asset_name": asset_name,
                "asset_url": url,
                "release_id": 11,
                "asset_id": 12,
                "release_immutable": True,
                "release_target_commitish": source_commit,
                "asset_api_digest": f"sha256:{self.package.archive_sha256}",
                "verified_at": "2026-07-16T12:00:00Z",
            }
        )
        envelope = ATTEST.sign_envelope(
            bindings,
            trust_root_id=root_id,
            signer_principal=principal,
            public_key_base64=public_key,
            private_key=key,
        )
        self.attestation = pathlib.Path(self.temp.name) / "attestation.json"
        self.attestation.write_bytes(ATTEST.canonical_json_bytes(envelope) + b"\n")
        self.attestation.chmod(0o600)
        contract["restricted"]["attestation"].update(
            {
                "sha256": HYDRATE.sha256_file(self.attestation),
                "size_bytes": self.attestation.stat().st_size,
                "signed_payload_sha256": envelope["signed_payload_sha256"],
                "trust_root_id": root_id,
                "signer_principal": principal,
                "public_key_sha256": envelope["public_key_sha256"],
            }
        )
        self.contract = self.here / "fixture-storage-v2.json"
        self.contract.write_text(json.dumps(contract, sort_keys=True) + "\n", encoding="utf-8")

    def test_real_sshsig_two_root_hydration_is_atomic_and_verifies(self) -> None:
        corrupted = pathlib.Path(self.temp.name) / "corrupted-attestation.json"
        corrupted.write_bytes(self.attestation.read_bytes() + b" ")
        corrupted.chmod(0o600)
        destination = self.repo / HYDRATE.STORAGE_HYDRATION_PARENT
        with self.assertRaisesRegex(HYDRATE.HydrationError, "size differs"):
            HYDRATE.hydrate_storage_v2(
                self.contract,
                self.archive,
                corrupted,
                repo=self.repo,
                matrix={},
                pins={},
                verify_external_locks=False,
            )
        self.assertFalse(destination.exists())
        self.assertEqual(list(destination.parent.glob(".engine-evidence.hydrate-*")), [])

        with mock.patch.object(PUBLIC, "validate_public_bundle", return_value=[]):
            manifest = HYDRATE.hydrate_storage_v2(
                self.contract,
                self.archive,
                self.attestation,
                repo=self.repo,
                matrix={},
                pins={},
                verify_external_locks=False,
            )
        self.assertTrue(manifest.is_file())
        hydrated_attestation = destination / HYDRATE.RESTRICTED_ATTESTATION_PATH
        self.assertEqual(stat.S_IMODE(hydrated_attestation.stat().st_mode), 0o600)
        self.assertTrue((destination / HYDRATE.PUBLIC_ARCHIVE_ROOT).is_dir())

    def test_approved_contract_rejects_public_projection_cross_binding_drift(self) -> None:
        changed = json.loads(self.contract.read_text(encoding="utf-8"))
        changed["public"]["evidence"]["manifest_sha256"] = "0" * 64
        self.contract.write_text(json.dumps(changed, sort_keys=True) + "\n", encoding="utf-8")
        with self.assertRaisesRegex(HYDRATE.HydrationError, "differs from signed projection"):
            HYDRATE.load_storage_contract_v2(
                self.contract,
                repo=self.repo,
                verify_external_locks=False,
            )


if __name__ == "__main__":
    unittest.main()
