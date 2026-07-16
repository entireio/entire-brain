from __future__ import annotations

import copy
import hashlib
import importlib.util
import io
import json
import pathlib
import sys
import tarfile
import tempfile
import unittest


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


if __name__ == "__main__":
    unittest.main()
