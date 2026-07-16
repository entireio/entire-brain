#!/usr/bin/env python3

from __future__ import annotations

import copy
import contextlib
import io
import json
import pathlib
import subprocess
import tempfile
import unittest

import profile_brief as profile


FAKE_BRAIN_BINARY = r'''#!/usr/bin/env python3
import json
import os
import pathlib
import sys


def stage(invoked=True, duration=10, inputs=1, outputs=1, errors=0):
    if not invoked:
        return {"invoked": False, "duration_ns": 0, "input_count": 0, "output_count": 0, "error_count": 0}
    return {
        "invoked": True,
        "duration_ns": duration,
        "input_count": inputs,
        "output_count": outputs,
        "error_count": errors,
    }


def make_profile(packet_bytes):
    counts = {
        "semantic_symbols": 1,
        "semantic_relations": 1,
        "semantic_neighbors": 1,
        "runtime_traces": 1,
        "test_roots": 1,
        "test_suggestions": 1,
        "history_matches": 1,
        "facts": 1,
        "facts_with_locus_drift": 0,
        "actions": 1,
        "likely_edit_files": 1,
        "likely_test_files": 1,
        "likely_files": 2,
        "patterns": 0,
        "consolidations": 0,
        "themes": 0,
        "guidance_items": 4,
        "warnings": 0,
    }
    query = {
        "ordinal": 1,
        "duration_ns": 3,
        "scanned_file_count": 2,
        "scanned_byte_count": 30,
        "match_count": 1,
        "truncated": False,
        "error_count": 0,
    }
    return {
        "schema_version": 1,
        "duration_unit": "nanoseconds",
        "total_brief": {"duration_ns": 200},
        "status_build_state": stage(duration=20),
        "semantic": {
            "context": stage(),
            "runtime_traces": stage(),
            "tests": stage(),
        },
        "history": {
            "index_load": stage(),
            "indexed_rank": stage(),
            "raw_fallback": {
                "invoked": True,
                "duration_ns": 4,
                "query_count": 1,
                "scanned_file_count": 2,
                "scanned_byte_count": 30,
                "match_count": 1,
                "truncation_count": 0,
                "error_count": 0,
                "queries": [query],
            },
        },
        "facts": {
            "load": stage(outputs=2),
            "vector_cache_load": stage(outputs=1),
            "embed": {
                "invoked": True,
                "duration_ns": 11,
                "query_call_count": 1,
                "fact_call_count": 1,
                "valid_vector_count": 2,
                "invalid_vector_count": 0,
            },
            "rank": stage(inputs=2),
            "cache_flush": stage(inputs=2, outputs=2),
        },
        "synthesis": {
            "likely_files": stage(),
            "action_checklist": stage(),
        },
        "knowledge": {
            "patterns": stage(outputs=0),
            "consolidations": stage(outputs=0),
            "themes": stage(outputs=0),
        },
        "packet": {
            "format": "json",
            "serialization": stage(),
            "byte_count": packet_bytes,
            "counts": counts,
        },
    }


if sys.argv[1:3] == ["status", "--json"]:
    print(json.dumps({"brain": {"path": str(pathlib.Path.cwd() / ".fake-brain")}}))
    raise SystemExit(0)

if sys.argv[1:2] != ["brief"]:
    raise SystemExit(64)

prompt = sys.argv[2]
if "FAIL_PRIVATE" in prompt:
    sys.stderr.write("PRIVATE_STDERR " + prompt + " " + os.getcwd())
    sys.stdout.write("PRIVATE_FAILED_PACKET " + prompt)
    raise SystemExit(7)

sidecar = pathlib.Path(sys.argv[sys.argv.index("--profile-json") + 1])
packet = (json.dumps({"private_prompt": prompt, "private_path": os.getcwd()}, sort_keys=True) + "\n").encode()
sidecar_value = make_profile(len(packet))
if "INVALID_PRIVATE" in prompt:
    sidecar_value["private_leak"] = prompt
if "MISMATCH_PRIVATE" in prompt:
    sidecar_value["packet"]["byte_count"] += 1
sidecar.write_text(json.dumps(sidecar_value))
sidecar.chmod(0o644 if "PUBLIC_MODE_PRIVATE" in prompt else 0o600)
sys.stdout.buffer.write(packet)
'''


def test_stage(invoked: bool = True, output_count: int = 1) -> dict[str, object]:
    if not invoked:
        return {
            "invoked": False,
            "duration_ns": 0,
            "input_count": 0,
            "output_count": 0,
            "error_count": 0,
        }
    return {
        "invoked": True,
        "duration_ns": 1,
        "input_count": 1,
        "output_count": output_count,
        "error_count": 0,
    }


def valid_profile(packet_bytes: int = 12) -> dict[str, object]:
    counts = {
        name: 0
        for name in (
            "semantic_symbols",
            "semantic_relations",
            "semantic_neighbors",
            "runtime_traces",
            "test_roots",
            "test_suggestions",
            "history_matches",
            "facts",
            "facts_with_locus_drift",
            "actions",
            "likely_edit_files",
            "likely_test_files",
            "likely_files",
            "patterns",
            "consolidations",
            "themes",
            "guidance_items",
            "warnings",
        )
    }
    return {
        "schema_version": 1,
        "duration_unit": "nanoseconds",
        "total_brief": {"duration_ns": 1},
        "status_build_state": test_stage(),
        "semantic": {name: test_stage() for name in ("context", "runtime_traces", "tests")},
        "history": {
            "index_load": test_stage(),
            "indexed_rank": test_stage(),
            "raw_fallback": {
                "invoked": False,
                "duration_ns": 0,
                "query_count": 0,
                "scanned_file_count": 0,
                "scanned_byte_count": 0,
                "match_count": 0,
                "truncation_count": 0,
                "error_count": 0,
                "queries": [],
            },
        },
        "facts": {
            "load": test_stage(output_count=0),
            "vector_cache_load": test_stage(invoked=False),
            "embed": {
                "invoked": False,
                "duration_ns": 0,
                "query_call_count": 0,
                "fact_call_count": 0,
                "valid_vector_count": 0,
                "invalid_vector_count": 0,
            },
            "rank": test_stage(),
            "cache_flush": test_stage(invoked=False),
        },
        "synthesis": {name: test_stage() for name in ("likely_files", "action_checklist")},
        "knowledge": {name: test_stage() for name in ("patterns", "consolidations", "themes")},
        "packet": {
            "format": "json",
            "serialization": test_stage(),
            "byte_count": packet_bytes,
            "counts": counts,
        },
    }


class BriefProfileCorpusTest(unittest.TestCase):
    def test_checked_in_inventory_is_exact_deduped_and_prompt_free(self) -> None:
        corpus = profile.load_verified_corpus()
        self.assertTrue(profile.verify_self_hash(corpus, "corpus_sha256"))
        self.assertEqual(corpus["selection"]["source_file_count"], 118)
        self.assertEqual(corpus["selection"]["deduplicated_task_count"], 114)
        self.assertEqual(corpus["selection"]["duplicates_removed"], 4)
        self.assertEqual(
            corpus["selection"]["unique_count_by_repo"],
            {"entire-brain": 25, "entire-cli": 68, "entire-db": 21},
        )
        self.assertEqual(len(corpus["tasks"]), 114)
        keys = {
            "logical_repo",
            "source_path",
            "task_sha256",
            "config_sha256",
            "prompt_sha256",
        }
        dedupe = set()
        for task in corpus["tasks"]:
            self.assertEqual(set(task), keys)
            self.assertFalse(pathlib.PurePosixPath(task["source_path"]).is_absolute())
            self.assertNotIn("..", pathlib.PurePosixPath(task["source_path"]).parts)
            self.assertRegex(task["task_sha256"], r"^[0-9a-f]{64}$")
            self.assertRegex(task["config_sha256"], r"^[0-9a-f]{64}$")
            self.assertRegex(task["prompt_sha256"], r"^[0-9a-f]{64}$")
            dedupe.add((task["logical_repo"], task["prompt_sha256"]))
        self.assertEqual(len(dedupe), 114)
        corpus_text = profile.DEFAULT_CORPUS.read_text()
        self.assertNotIn("Session export regression:", corpus_text)
        self.assertNotIn("Fix the security regression", corpus_text)

    def test_tampered_corpus_self_hash_and_config_hash_fail_closed(self) -> None:
        corpus = profile.load_verified_corpus()
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "corpus.json"
            tampered = copy.deepcopy(corpus)
            tampered["tasks"][0]["config_sha256"] = "0" * 64
            path.write_text(json.dumps(tampered))
            with self.assertRaises(profile.ProfileRunError):
                profile.load_verified_corpus(path)

            rehashed = profile.attach_self_hash(tampered, "corpus_sha256")
            path.write_text(json.dumps(rehashed))
            with self.assertRaises(profile.ProfileRunError):
                profile.load_verified_corpus(path)

    def test_schedule_is_deterministic_and_observations_are_consecutive(self) -> None:
        tasks = [
            {"logical_repo": "entire-brain", "task_sha256": "a" * 64},
            {"logical_repo": "entire-cli", "task_sha256": "b" * 64},
        ]
        first = profile.build_schedule(tasks)
        second = profile.build_schedule(tasks)
        self.assertEqual(first, second)
        self.assertEqual(
            [entry["observation_label"] for entry in first["entries"]],
            ["first_observation", "immediate_repeat"] * 2,
        )
        self.assertEqual([entry["sequence"] for entry in first["entries"]], list(range(4)))


class BriefProfileShapeTest(unittest.TestCase):
    def test_exact_numeric_privacy_shape_accepts_valid_profile(self) -> None:
        numeric = profile.validate_numeric_profile(valid_profile(), 12)
        self.assertNotIn("duration_unit", numeric)
        self.assertNotIn("format", numeric["packet"])
        self.assertEqual(profile.observed_vector_cache_state(numeric)["state"], "no_facts")

    def test_extra_private_field_string_or_aggregate_tamper_fails_closed(self) -> None:
        for mutate in (
            lambda value: value.update({"private_path": "/Users/private/repo"}),
            lambda value: value["packet"].update({"format": "/private/value"}),
            lambda value: value["history"]["raw_fallback"].update({"query_count": 1}),
            lambda value: value["facts"]["embed"].update({"valid_vector_count": 1}),
        ):
            value = valid_profile()
            mutate(value)
            with self.assertRaises(profile.ProfileShapeError):
                profile.validate_numeric_profile(value, 12)

    def test_duplicate_keys_nonfinite_numbers_and_oversized_integers_fail_closed(self) -> None:
        with self.assertRaises(profile.ProfileShapeError):
            profile._strict_sidecar_json(b'{"schema_version":1,"schema_version":1}')
        with self.assertRaises(profile.ProfileShapeError):
            profile._strict_sidecar_json(b'{"schema_version":NaN}')
        value = valid_profile()
        value["total_brief"]["duration_ns"] = 1 << 63
        with self.assertRaises(profile.ProfileShapeError):
            profile.validate_numeric_profile(value, 12)


class BriefProfileRunnerTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.temporary.name)
        self.binary = self.root / "fake-entire"
        self.binary.write_text(FAKE_BRAIN_BINARY)
        self.binary.chmod(0o700)

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def make_repo(self, logical_repo: str) -> pathlib.Path:
        repo = self.root / logical_repo
        repo.mkdir()
        subprocess.run(("git", "init", "-q"), cwd=repo, check=True)
        (repo / "tracked.txt").write_text("fixture\n")
        subprocess.run(("git", "add", "tracked.txt"), cwd=repo, check=True)
        subprocess.run(
            (
                "git",
                "-c",
                "user.name=Fixture",
                "-c",
                "user.email=fixture@example.invalid",
                "commit",
                "-qm",
                "fixture",
            ),
            cwd=repo,
            check=True,
        )
        brain = repo / ".fake-brain"
        brain.mkdir()
        (brain / "manifest.json").write_text(json.dumps({"schema_version": 1, "repo": logical_repo}))
        return repo

    @staticmethod
    def task(logical_repo: str, prompt_text: str) -> dict[str, str]:
        prompt_sha = profile.sha256_bytes(prompt_text.encode())
        return {
            "logical_repo": logical_repo,
            "task_sha256": profile.sha256_bytes((logical_repo + prompt_sha).encode()),
            "prompt_sha256": prompt_sha,
            "prompt": prompt_text,
        }

    @staticmethod
    def corpus(tasks: list[dict[str, str]]) -> dict[str, object]:
        return {
            "schema_version": 1,
            "corpus_sha256": profile.sha256_bytes(b"fixture-corpus"),
            "tasks": [
                {key: task[key] for key in ("logical_repo", "task_sha256", "prompt_sha256")}
                for task in tasks
            ],
        }

    def test_fake_binary_report_is_deterministic_private_and_complete(self) -> None:
        mappings = {
            "entire-brain": self.make_repo("entire-brain"),
            "entire-cli": self.make_repo("entire-cli"),
        }
        private_prompts = [
            "PRIVATE_PROMPT_ALPHA /Users/secret/repo",
            "PRIVATE_PROMPT_BETA secret@example.invalid",
        ]
        tasks = [
            self.task("entire-brain", private_prompts[0]),
            self.task("entire-cli", private_prompts[1]),
        ]
        corpus = self.corpus(tasks)
        first = profile.build_profile_report(corpus, tasks, self.binary, mappings, 5)
        second = profile.build_profile_report(corpus, tasks, self.binary, mappings, 5)
        self.assertEqual(first, second)
        self.assertTrue(profile.verify_self_hash(first, "report_sha256"))
        self.assertEqual(len(first["observations"]), 4)
        self.assertEqual(first["failures"], [])
        self.assertEqual(
            [row["observation_label"] for row in first["observations"]],
            ["first_observation", "immediate_repeat"] * 2,
        )
        for row in first["observations"]:
            self.assertEqual(row["status"], "ok")
            self.assertEqual(
                row["observed_fact_vector_cache"]["state"],
                "fact_embedding_with_existing_cache",
            )
            self.assertNotIn("duration_unit", row["numeric_profile"])
            self.assertNotIn("format", row["numeric_profile"]["packet"])
        retained = json.dumps(first, sort_keys=True)
        for private in [
            *private_prompts,
            *map(str, mappings.values()),
            str(self.binary),
            "private_prompt",
            "private_path",
        ]:
            self.assertNotIn(private, retained)

        output = self.root / "report.json"
        profile.write_private_json(output, first)
        self.assertEqual(output.stat().st_mode & 0o777, 0o600)
        self.assertEqual(json.loads(output.read_text()), first)

    def test_failure_hashes_stderr_and_retains_no_output_or_private_values(self) -> None:
        repo = self.make_repo("entire-db")
        prompt_text = "FAIL_PRIVATE user@example.invalid /Users/private/database"
        task = self.task("entire-db", prompt_text)
        report = profile.build_profile_report(
            self.corpus([task]), [task], self.binary, {"entire-db": repo}, 5
        )
        self.assertEqual(len(report["failures"]), 2)
        self.assertTrue(all(row["kind"] == "nonzero_exit" for row in report["failures"]))
        self.assertTrue(all(row["stderr_byte_count"] > 0 for row in report["failures"]))
        for observation in report["observations"]:
            self.assertEqual(observation["status"], "failed")
            self.assertNotIn("packet", observation)
            self.assertNotIn("numeric_profile", observation)
            self.assertRegex(observation["failure"]["stderr_sha256"], r"^[0-9a-f]{64}$")
        retained = json.dumps(report, sort_keys=True)
        for private in (prompt_text, str(repo), "PRIVATE_STDERR", "PRIVATE_FAILED_PACKET"):
            self.assertNotIn(private, retained)

    def test_invalid_or_packet_mismatched_sidecar_fails_without_retention(self) -> None:
        repo = self.make_repo("entire-brain")
        for prompt_text in (
            "INVALID_PRIVATE /Users/private/invalid",
            "MISMATCH_PRIVATE /Users/private/mismatch",
            "PUBLIC_MODE_PRIVATE /Users/private/mode",
        ):
            task = self.task("entire-brain", prompt_text)
            report = profile.build_profile_report(
                self.corpus([task]), [task], self.binary, {"entire-brain": repo}, 5
            )
            self.assertEqual(len(report["failures"]), 2)
            self.assertTrue(all(row["kind"] == "invalid_sidecar" for row in report["failures"]))
            retained = json.dumps(report, sort_keys=True)
            self.assertNotIn(prompt_text, retained)
            self.assertNotIn(str(repo), retained)

    def test_cli_smoke_runs_deterministic_prefix_and_marks_it_incomplete(self) -> None:
        repo = self.make_repo("entire-brain")
        output = self.root / "cli-report.json"
        stdout = io.StringIO()
        with contextlib.redirect_stdout(stdout):
            exit_code = profile.main(
                (
                    "--brain-bin",
                    str(self.binary),
                    "--repo",
                    "entire-brain=" + str(repo),
                    "--output",
                    str(output),
                    "--max-tasks",
                    "1",
                    "--timeout-seconds",
                    "5",
                )
            )
        self.assertEqual(exit_code, 0)
        report = json.loads(output.read_text())
        self.assertFalse(report["corpus"]["complete_corpus"])
        self.assertEqual(report["corpus"]["selected_task_count"], 1)
        self.assertEqual(len(report["observations"]), 2)
        self.assertTrue(profile.verify_self_hash(report, "report_sha256"))
        summary = json.loads(stdout.getvalue())
        self.assertEqual(summary["failures"], 0)


if __name__ == "__main__":
    unittest.main()
