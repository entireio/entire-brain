import hashlib
import sys
import tempfile
import unittest
from pathlib import Path


SCRIPT_DIR = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(SCRIPT_DIR))

import prepare  # noqa: E402
import run_other  # noqa: E402
import run_shard  # noqa: E402


class CoverageProducerTests(unittest.TestCase):
    def test_coverage_build_evidence_requires_exact_enabled_flag(self):
        prepare.require_coverage_build("binary:\n\tbuild\t-cover=true\n")
        for info in ("", "build -cover=false", "build -cover=trueish", "path -cover=true"):
            with self.subTest(info=info), self.assertRaisesRegex(RuntimeError, "cover"):
                prepare.require_coverage_build(info)

    def test_shard_command_writes_one_absolute_profile(self):
        with tempfile.TemporaryDirectory() as directory:
            profile = Path(directory).resolve() / "coverage" / "shard-003-process-00.out"
            arguments = run_shard.shard_command_arguments(
                "example/heavy", Path(directory) / "heavy.test", "20m", "^TestA$", None, profile
            )
        self.assertEqual(
            [argument for argument in arguments if argument.startswith("-test.coverprofile=")],
            [f"-test.coverprofile={profile}"],
        )
        self.assertIn("-test.timeout=20m", arguments)

    def test_profile_metadata_requires_contained_atomic_profile(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory).resolve() / "result"
            profile = output / "coverage" / "other.out"
            profile.parent.mkdir(parents=True)
            content = b"mode: atomic\nexample/pkg/file.go:1.1,1.2 1 1\n"
            profile.write_bytes(content)
            expected = ("coverage/other.out", hashlib.sha256(content).hexdigest())
            self.assertEqual(run_shard.coverage_profile_metadata(output, profile), expected)
            self.assertEqual(run_other.coverage_profile_metadata(output, profile), expected)

            profile.write_text("mode: set\nexample/pkg/file.go:1.1,1.2 1 1\n", encoding="utf-8")
            for metadata in (
                run_shard.coverage_profile_metadata,
                run_other.coverage_profile_metadata,
            ):
                with self.subTest(metadata=metadata.__module__), self.assertRaisesRegex(
                    RuntimeError, "mode header"
                ):
                    metadata(output, profile)

            outside = Path(directory).resolve() / "outside.out"
            outside.write_bytes(content)
            with self.assertRaisesRegex(ValueError, "escaped"):
                run_shard.coverage_profile_metadata(output, outside)


if __name__ == "__main__":
    unittest.main()
