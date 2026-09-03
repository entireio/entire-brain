from __future__ import annotations

import copy
import hashlib
import json
import os
import pathlib
import subprocess
import tempfile
import time
import unittest
from unittest import mock
from typing import Any, Sequence

import task_eligibility
import task_negative_control as negative
import relevance_dataset


# A `select {}` body does not hang under `go test`.  With every goroutine
# blocked and no timer pending, the Go runtime's deadlock detector aborts the
# binary in single-digit milliseconds ("fatal error: all goroutines are asleep
# - deadlock!"), so a command built from it exits almost immediately.  Sleeping
# leaves a timer pending, the runtime therefore keeps waiting, and nothing but
# the external timeout can end the run -- which is the property under test.
HANG_TEST_SOURCE = (
    "package hang\n"
    "\n"
    "import (\n"
    '\t"testing"\n'
    '\t"time"\n'
    ")\n"
    "\n"
    "func TestHang(t *testing.T) { time.Sleep(time.Hour) }\n"
)
# Same dependencies, same command shape, but it returns at once: timing this
# one measures everything the hanging run does except the hang.
CONTROL_TEST_SOURCE = (
    "package control\n\nimport \"testing\"\n\nfunc TestControl(t *testing.T) {}\n"
)
# The measured window has to clear the fully cached cost of `go test` on this
# machine by a wide margin, without letting a slow machine stretch the test.
HANG_TIMEOUT_MULTIPLE = 5.0
HANG_TIMEOUT_FLOOR_SECONDS = 0.5
HANG_TIMEOUT_CEILING_SECONDS = 5.0


class TaskNegativeControlTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temporary = tempfile.TemporaryDirectory()
        cls.repo = pathlib.Path(cls.temporary.name) / "repo"
        cls.repo.mkdir()
        cls.git("init", "-q")
        cls.git("config", "user.name", "Synthetic Reviewer")
        cls.git("config", "user.email", "synthetic@example.invalid")
        (cls.repo / "go.mod").write_text("module example.invalid/negative\n\ngo 1.23\n", encoding="utf-8")
        package = cls.repo / "pkg"
        package.mkdir()
        (package / "code.go").write_text("package pkg\n\nfunc Value() int { return 1 }\n", encoding="utf-8")
        (package / "code_test.go").write_text(
            "package pkg\n\nimport \"testing\"\n\nfunc TestValue(t *testing.T) { if Value() != 1 { t.Fail() } }\n",
            encoding="utf-8",
        )
        cls.commit("base", "2026-07-16T00:00:00+00:00")
        cls.base = cls.rev("HEAD")

        (package / "code.go").write_text("package pkg\n\nfunc Value() int { return 2 }\n", encoding="utf-8")
        (package / "code_test.go").write_text(
            "package pkg\n\nimport \"testing\"\n\nfunc TestValue(t *testing.T) { if Value() != 2 { t.Fail() } }\n",
            encoding="utf-8",
        )
        cls.commit("answer-bearing eligible change", "2026-07-16T00:01:00+00:00")

        (package / "extra.go").write_text("package pkg\n\nfunc Unused() int { return 3 }\n", encoding="utf-8")
        (package / "code_test.go").write_text(
            "package pkg\n\nimport (\n  \"os\"\n  \"testing\"\n)\n\n"
            "func TestValue(t *testing.T) { if Value() != 2 { t.Fail() } }\n"
            "func TestStable(t *testing.T) {\n"
            "  if _, err := os.Stat(\"sentinel\"); err == nil { t.Fatal(\"dirty worktree\") }\n"
            "  if err := os.WriteFile(\"sentinel\", []byte(\"created\"), 0o600); err != nil { t.Fatal(err) }\n"
            "}\n",
            encoding="utf-8",
        )
        cls.commit("answer-bearing surviving change", "2026-07-16T00:02:00+00:00")

        (package / "extra.go").unlink()
        (package / "code_test.go").write_text(
            "package pkg\n\nimport \"testing\"\n\nfunc TestValue(t *testing.T) { if Value() != 999 { t.Fail() } }\n",
            encoding="utf-8",
        )
        cls.commit("answer-bearing invalid baseline", "2026-07-16T00:03:00+00:00")
        cls.head = cls.rev("HEAD")
        cls.ledger = task_eligibility.scan_repository(
            cls.repo,
            base=cls.base,
            head=cls.head,
            repository_id="example.invalid/negative",
        )
        cls.receipt = negative.execute_receipt(
            cls.repo,
            cls.ledger,
            ledger_file_sha256=hashlib.sha256(b"synthetic-ledger-bytes").hexdigest(),
            timeout_seconds=30,
            generated_at="2026-07-16T01:00:00Z",
        )

    @classmethod
    def tearDownClass(cls) -> None:
        cls.temporary.cleanup()

    @classmethod
    def git(cls, *args: str, env: dict[str, str] | None = None) -> str:
        completed = subprocess.run(
            ["git", *args],
            cwd=cls.repo,
            check=True,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
        )
        return completed.stdout.strip()

    @classmethod
    def commit(cls, subject: str, timestamp: str) -> None:
        cls.git("add", "-A")
        environment = dict(os.environ)
        environment["GIT_AUTHOR_DATE"] = timestamp
        environment["GIT_COMMITTER_DATE"] = timestamp
        cls.git("commit", "-q", "-m", subject, env=environment)

    @classmethod
    def rev(cls, revision: str) -> str:
        return cls.git("rev-parse", revision)

    def test_reverse_patch_classifications_and_add_delete_handling(self) -> None:
        classifications = [result["classification"] for result in self.receipt["results"]]
        self.assertEqual(
            classifications,
            [
                "eligible_for_symptom_review",
                "negative_control_survived",
                "baseline_invalid_failure",
            ],
        )
        self.assertEqual(self.receipt["summary"]["candidate_count"], 3)
        self.assertEqual(
            self.receipt["summary"]["classification_counts"]["eligible_for_symptom_review"],
            1,
        )
        negative.validate_receipt(self.receipt)
        schema = json.loads(
            (
                pathlib.Path(negative.__file__).parent
                / "schemas"
                / "development-task-negative-control-v1.schema.json"
            ).read_text(encoding="utf-8")
        )
        self.assertEqual(relevance_dataset.validate_schema_instance(self.receipt, schema, "receipt"), [])

    def test_receipt_retains_commitments_not_paths_subjects_or_output(self) -> None:
        rendered = json.dumps(self.receipt, sort_keys=True)
        self.assertNotIn("pkg/code.go", rendered)
        self.assertNotIn("answer-bearing", rendered)
        self.assertNotIn("Value()", rendered)
        self.assertIn("test_targets_sha256", rendered)
        self.assertEqual(self.receipt["exposure"], task_eligibility.EXPOSURE)

    def test_resealed_semantic_tampering_fails_closed(self) -> None:
        receipt = copy.deepcopy(self.receipt)
        receipt["results"][0]["classification"] = "negative_control_survived"
        receipt["receipt_sha256"] = negative._self_hash(receipt)
        with self.assertRaisesRegex(negative.NegativeControlError, "classification differs"):
            negative.validate_receipt(receipt)

        receipt = copy.deepcopy(self.receipt)
        receipt["summary"]["candidate_count"] += 1
        receipt["receipt_sha256"] = negative._self_hash(receipt)
        with self.assertRaisesRegex(negative.NegativeControlError, "summary count differs"):
            negative.validate_receipt(receipt)

        receipt = copy.deepcopy(self.receipt)
        receipt["summary"]["classification_counts"]["baseline_invalid_timeout"] = False
        receipt["receipt_sha256"] = negative._self_hash(receipt)
        with self.assertRaisesRegex(negative.NegativeControlError, "classification summary values"):
            negative.validate_receipt(receipt)

        receipt = copy.deepcopy(self.receipt)
        receipt["repository_id"] = "invalid\nrepository"
        receipt["receipt_sha256"] = negative._self_hash(receipt)
        with self.assertRaisesRegex(negative.NegativeControlError, "repository_id is invalid"):
            negative.validate_receipt(receipt)

        receipt = copy.deepcopy(self.receipt)
        receipt["implementation"]["files"][0]["sha256"] = hashlib.sha256(b"other runner").hexdigest()
        receipt["implementation"]["aggregate_sha256"] = negative._canonical_hash(
            receipt["implementation"]["files"]
        )
        receipt["receipt_sha256"] = negative._self_hash(receipt)
        with self.assertRaisesRegex(negative.NegativeControlError, "current bytes"):
            negative.validate_receipt(receipt)

    def test_source_repository_drift_is_rejected_before_execution(self) -> None:
        ledger = copy.deepcopy(self.ledger)
        ledger["candidates"][0]["source_diff_sha256"] = hashlib.sha256(b"drift").hexdigest()
        ledger["ledger_sha256"] = task_eligibility._self_hash(ledger)
        with self.assertRaisesRegex(negative.NegativeControlError, "source diff differs"):
            negative.execute_receipt(
                self.repo,
                ledger,
                ledger_file_sha256=hashlib.sha256(b"drifted-ledger").hexdigest(),
                timeout_seconds=30,
                generated_at="2026-07-16T01:00:00Z",
            )

    def test_timeout_classification_is_not_eligible(self) -> None:
        baseline = {
            "status": "timeout",
            "exit_code": None,
            "output_sha256": hashlib.sha256(b"").hexdigest(),
            "output_byte_count": 0,
        }
        completed_failure = {
            "status": "completed",
            "exit_code": 1,
            "output_sha256": hashlib.sha256(b"failure").hexdigest(),
            "output_byte_count": 7,
        }
        self.assertEqual(negative._classification(baseline, completed_failure), "baseline_invalid_timeout")
        self.assertEqual(
            negative._classification(
                {**completed_failure, "exit_code": 0},
                {**baseline},
            ),
            "reversed_invalid_timeout",
        )

    def test_hanging_go_test_is_killed_by_the_only_timeout(self) -> None:
        captured: list[bytes] = []
        with tempfile.TemporaryDirectory() as temporary:
            repo = pathlib.Path(temporary)
            (repo / "go.mod").write_text("module example.invalid/hang\n\ngo 1.23\n", encoding="utf-8")
            (repo / "hang").mkdir()
            (repo / "hang" / "hang_test.go").write_text(HANG_TEST_SOURCE, encoding="utf-8")
            (repo / "control").mkdir()
            (repo / "control" / "control_test.go").write_text(CONTROL_TEST_SOURCE, encoding="utf-8")
            prebuilt = repo / "prebuilt"
            prebuilt.mkdir()
            state_root = repo / "state"
            negative._reset_execution_state(state_root)
            environment = negative._test_environment(state_root)
            go = negative._go_runtime()["binary"]

            # Compile both packages before anything is timed.  What is under
            # test is that a command which never finishes is killed by the
            # single external timeout, so the Go build must sit outside the
            # measured window: on a cold build cache the build alone takes tens
            # of seconds and on a warm one it takes milliseconds, and that
            # spread decided the result rather than the hang.
            for package in ("control", "hang"):
                built = negative._run(
                    [go, "test", "-c", "-o", str(prebuilt / package), f"./{package}"],
                    cwd=repo,
                    combined_output=True,
                    env=environment,
                )
                self.assertEqual(
                    built.returncode,
                    0,
                    f"pre-building ./{package} failed:\n"
                    + built.stdout.decode("utf-8", errors="replace"),
                )

            # Calibrate the window against this machine rather than a constant.
            started = time.monotonic()
            control = negative._run(
                [go, "test", *negative.TEST_FLAGS, "./control"],
                cwd=repo,
                combined_output=True,
                env=environment,
            )
            cached_seconds = time.monotonic() - started
            self.assertEqual(
                control.returncode,
                0,
                "cached control run failed:\n" + control.stdout.decode("utf-8", errors="replace"),
            )
            timeout_seconds = min(
                HANG_TIMEOUT_CEILING_SECONDS,
                max(HANG_TIMEOUT_FLOOR_SECONDS, HANG_TIMEOUT_MULTIPLE * cached_seconds),
            )

            started = time.monotonic()
            execution = negative._execution(
                [go, "test", *negative.TEST_FLAGS, "./hang"],
                repo,
                timeout_seconds,
                environment,
                output_sink=captured.append,
            )
            elapsed = time.monotonic() - started

        diagnosis = (
            f"`go test ./hang` should have been killed by the only timeout "
            f"({timeout_seconds:.3f}s, calibrated from a {cached_seconds:.3f}s cached "
            f"control run) but returned status={execution['status']!r} "
            f"exit_code={execution['exit_code']!r} after {elapsed:.3f}s. Command output:\n"
            + (captured[0].decode("utf-8", errors="replace") if captured else "<no output captured>")
        )
        self.assertEqual(execution["status"], "timeout", diagnosis)
        self.assertIsNone(execution["exit_code"], diagnosis)
        # The command was still running when the window closed, so the window
        # was actually spent -- it did not merely outlast a fast exit.
        self.assertGreaterEqual(elapsed, timeout_seconds, diagnosis)
        self.assertEqual(
            negative._classification(
                {
                    "status": "completed",
                    "exit_code": 0,
                    "output_sha256": hashlib.sha256(b"ok").hexdigest(),
                    "output_byte_count": 2,
                },
                execution,
            ),
            "reversed_invalid_timeout",
            diagnosis,
        )

    def test_isolated_execution_state_disables_the_go_telemetry_sidecar(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            repo = pathlib.Path(temporary)
            (repo / "go.mod").write_text(
                "module example.invalid/telemetry\n\ngo 1.23\n", encoding="utf-8"
            )
            state_root = repo / "state"
            negative._reset_execution_state(state_root)
            directories = negative._telemetry_directories(state_root)
            # Whichever of these the host's Go treats as os.UserConfigDir(),
            # it finds a mode file that says "off".
            for directory in directories:
                self.assertEqual((directory / "mode").read_text(encoding="utf-8").strip(), "off")
            environment = negative._test_environment(state_root)
            self.assertEqual(environment["XDG_CONFIG_HOME"], str(state_root / "xdg-config"))
            self.assertEqual(environment["HOME"], str(state_root / "home"))
            completed = negative._run(
                [negative._go_runtime()["binary"], "list", "-m", "-json"],
                cwd=repo,
                combined_output=True,
                env=environment,
            )
            self.assertEqual(
                completed.returncode,
                0,
                completed.stdout.decode("utf-8", errors="replace"),
            )
            # With the mode at "off" the toolchain neither opens a counter file
            # nor forks the upload sidecar, so nothing is left writing into the
            # execution state once the measured command has exited.
            for directory in directories:
                self.assertEqual(
                    sorted(entry.name for entry in directory.iterdir()),
                    ["mode"],
                    f"the Go toolchain wrote telemetry into {directory}",
                )

    def test_effective_environment_is_captured_before_execution(self) -> None:
        state_root = pathlib.Path(self.temporary.name) / "environment-state"
        negative._reset_execution_state(state_root)
        environment = negative._test_environment(state_root)
        completed = negative._run(
            ["go", "env", "-json", *negative.GO_ENV_KEYS],
            cwd=self.repo,
            env=environment,
        )
        self.assertEqual(completed.returncode, 0)
        self.assertEqual(
            self.receipt["environment"]["go_env_sha256"],
            hashlib.sha256(completed.stdout).hexdigest(),
        )
        self.assertEqual(
            self.receipt["environment"]["execution_overrides"],
            {
                "GOENV": "off",
                "GOFLAGS": "",
                "GOPROXY": "off",
                "GOSUMDB": "off",
                "GOTOOLCHAIN": "local",
                "GOVCS": "*:off",
                "GOWORK": "off",
            },
        )
        self.assertIn(f"go version {negative.GO_TOOLCHAIN}", self.receipt["environment"]["go_version"])
        self.assertEqual(
            self.receipt["environment"]["environment_manifest_sha256"],
            negative._environment_manifest_hash(environment),
        )
        self.assertEqual(self.receipt["environment"]["inherited_variable_count"], 0)
        self.assertEqual(self.receipt["environment"]["credential_like_variable_count"], 0)
        forbidden = ("TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "API_KEY", "AUTH_SOCK")
        self.assertFalse(any(any(marker in key.upper() for marker in forbidden) for key in environment))

    def test_pinned_toolchain_module_preflight_fails_closed(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            repo = pathlib.Path(temporary)
            (repo / "go.mod").write_text(
                "module example.invalid/future\n\ngo 99.0\n",
                encoding="utf-8",
            )
            state_root = repo / "state"
            negative._reset_execution_state(state_root)
            with self.assertRaisesRegex(negative.NegativeControlError, "pinned Go toolchain"):
                negative._verify_go_module(repo, negative._test_environment(state_root), 0)

    def test_cleanup_prunes_metadata_after_remove_failure(self) -> None:
        worktree = pathlib.Path(self.temporary.name) / "forced-cleanup"
        state_root = pathlib.Path(self.temporary.name) / "cleanup-state"
        negative._reset_execution_state(state_root)
        candidate = self.ledger["candidates"][0]
        negative._prepare_worktree(
            self.repo, worktree, candidate, 0, negative._test_environment(state_root)
        )
        original_run = negative._run

        def injected_remove_failure(
            args: Sequence[str], **kwargs: Any
        ) -> subprocess.CompletedProcess[bytes]:
            if list(args[:4]) == ["git", "worktree", "remove", "--force"]:
                return subprocess.CompletedProcess(args, 1, stdout=b"", stderr=b"injected")
            return original_run(args, **kwargs)

        with mock.patch.object(negative, "_run", side_effect=injected_remove_failure):
            negative._cleanup_worktree(self.repo, worktree)
        self.assertFalse(worktree.exists())
        self.assertNotIn(str(worktree), self.git("worktree", "list", "--porcelain"))

    def test_cleanup_rejects_physical_deletion_failure_and_path_aliases(self) -> None:
        worktree = pathlib.Path(self.temporary.name) / "forced-deletion-failure"
        state_root = pathlib.Path(self.temporary.name) / "deletion-state"
        negative._reset_execution_state(state_root)
        candidate = self.ledger["candidates"][0]
        negative._prepare_worktree(
            self.repo, worktree, candidate, 0, negative._test_environment(state_root)
        )
        original_run = negative._run

        def injected_remove_failure(
            args: Sequence[str], **kwargs: Any
        ) -> subprocess.CompletedProcess[bytes]:
            if list(args[:4]) == ["git", "worktree", "remove", "--force"]:
                return subprocess.CompletedProcess(args, 1, stdout=b"", stderr=b"injected")
            return original_run(args, **kwargs)

        with (
            mock.patch.object(negative, "_run", side_effect=injected_remove_failure),
            mock.patch.object(negative.shutil, "rmtree", return_value=None),
            self.assertRaisesRegex(negative.NegativeControlError, "cannot delete"),
        ):
            negative._cleanup_worktree(self.repo, worktree)

        negative._cleanup_worktree(self.repo, worktree)
        self.assertFalse(worktree.exists())

    def test_execution_state_reset_fails_if_prior_state_cannot_be_deleted(self) -> None:
        state_root = pathlib.Path(self.temporary.name) / "uncleanable-state"
        negative._reset_execution_state(state_root)
        sentinel = state_root / "home" / "sentinel"
        sentinel.write_text("prior arm", encoding="utf-8")
        with (
            mock.patch.object(negative.shutil, "rmtree", return_value=None),
            self.assertRaisesRegex(negative.NegativeControlError, "cannot reset"),
        ):
            negative._reset_execution_state(state_root)
        self.assertTrue(sentinel.exists())
        negative._reset_execution_state(state_root)
        self.assertFalse(sentinel.exists())


if __name__ == "__main__":
    unittest.main()
