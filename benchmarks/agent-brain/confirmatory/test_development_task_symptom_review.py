from __future__ import annotations

import copy
import json
import os
import pathlib
import subprocess
import tempfile
import unittest
from unittest import mock

import development_task_symptom_review as review
import relevance_dataset


class HistoricalIdentityBoundaryTest(unittest.TestCase):
    def test_current_validator_refuses_changed_implementation_identity(self):
        root = pathlib.Path(review.__file__).parent
        negative, _ = review._load_json(root / "development-task-negative-control-v1.json")
        changed = copy.deepcopy(negative["implementation"])
        changed["python_version"] = "0.0.0-synthetic-drift"
        with mock.patch.object(review.task_negative_control, "_implementation_identity", return_value=changed):
            with self.assertRaisesRegex(review.task_negative_control.NegativeControlError,
                                        "implementation identity differs"):
                review.task_negative_control.validate_receipt(negative)


class DevelopmentTaskSymptomReviewTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.root = pathlib.Path(review.__file__).parent
        cls.review, _ = review._load_json(cls.root / "development-task-symptom-review-v1.json")
        cls.ledger, cls.ledger_raw = review._load_json(cls.root / "development-task-eligibility-scan-v1.json")
        cls.negative, cls.negative_raw = review._load_json(cls.root / "development-task-negative-control-v1.json")

    def setUp(self):
        # Exercise the historical review's semantics with its declared runtime
        # identities; production validation of current bytes is tested separately.
        for module, identity in ((review, self.review["implementation"]),
                                 (review.task_negative_control, self.negative["implementation"])):
            patcher = mock.patch.object(module, "_implementation_identity", return_value=identity)
            patcher.start()
            self.addCleanup(patcher.stop)

    def validate(self, value: dict[str, object]) -> None:
        review.validate_review(
            value,
            ledger=self.ledger,
            ledger_raw=self.ledger_raw,
            negative=self.negative,
            negative_raw=self.negative_raw,
        )

    @staticmethod
    def reseal(value: dict[str, object]) -> None:
        value["ledger_sha256"] = review._self_hash(value)

    @staticmethod
    def reseal_record(record: dict[str, object]) -> None:
        record["review_record_sha256"] = review._review_record_hash(record)

    def test_historical_review_fixture_validates_and_matches_schema(self) -> None:
        self.validate(self.review)
        schema = json.loads(
            (self.root / "schemas" / "development-task-symptom-review-v1.schema.json").read_text(encoding="utf-8")
        )
        self.assertEqual(relevance_dataset.validate_schema_instance(self.review, schema, "review"), [])

    def test_exact_negative_control_selection_and_non_authority(self) -> None:
        selected = self.review["selection"]["selected_first_parent_positions"]
        self.assertEqual(selected, [1, 2, 6, 8, 9, 10, 11, 14, 17, 18, 20, 21, 25, 26, 27, 29, 31])
        excluded = self.review["selection"]["excluded_first_parent_positions"]
        self.assertEqual(excluded["negative_control_survived"], [12])
        self.assertEqual(excluded["baseline_invalid_failure"], [3, 19, 22, 28])
        self.assertEqual(excluded["baseline_invalid_timeout"], [])
        self.assertEqual(excluded["reversed_invalid_timeout"], [13])
        self.assertEqual(self.review["summary"]["selected_count"], 17)
        self.assertEqual(
            self.review["summary"]["accepted_count"] + self.review["summary"]["rejected_count"],
            17,
        )
        self.assertEqual(self.review["summary"]["authoritative_population_member_count"], 0)
        self.assertEqual(self.review["authority"], review.AUTHORITY)

    def test_every_exact_prompt_is_statically_clean_and_independently_bound(self) -> None:
        prompts: set[str] = set()
        draft_reviewer = next(item for item in self.review["reviewers"] if item["role"] == "draft_author")
        auditor = next(item for item in self.review["reviewers"] if item["role"] == "independent_read_only_auditor")
        self.assertNotEqual(draft_reviewer["reviewer_id"], auditor["reviewer_id"])
        for item in self.review["items"]:
            self.assertEqual(review._prompt_static_findings(item["prompt"]), [])
            self.assertEqual(item["prompt_sha256"], review._prompt_hash(item["prompt"]))
            self.assertNotIn(item["prompt_sha256"], prompts)
            prompts.add(item["prompt_sha256"])
            self.assertEqual(item["draft_review"]["prompt_sha256"], item["prompt_sha256"])
            self.assertEqual(item["independent_review"]["prompt_sha256"], item["prompt_sha256"])
            self.assertEqual(item["independent_review"]["reviewer_id"], auditor["reviewer_id"])
            self.assertEqual(item["source_history"]["source_session_decision"], "unresolved_no_owner_source_session_receipt")
            self.assertEqual(
                item["relationships"]["authoritative_overlap_decision"],
                "unresolved_owner_hmac_and_source_session_receipts_absent",
            )

    def test_resealed_prompt_leakage_fails_closed(self) -> None:
        value = copy.deepcopy(self.review)
        item = value["items"][0]
        item["prompt"] = item["prompt"] + " Use io.ReadAll in the event parser."
        item["prompt_sha256"] = review._prompt_hash(item["prompt"])
        for name in ("draft_review", "independent_review"):
            item[name]["prompt_sha256"] = item["prompt_sha256"]
            self.reseal_record(item[name])
        self.reseal(value)
        with self.assertRaisesRegex(review.SymptomReviewError, "prompt static leakage"):
            self.validate(value)

    def test_resealed_authority_and_source_session_overclaims_fail_closed(self) -> None:
        value = copy.deepcopy(self.review)
        value["authority"]["owner_hmac_receipts"] = "verified"
        self.reseal(value)
        with self.assertRaisesRegex(review.SymptomReviewError, "authority"):
            self.validate(value)

        value = copy.deepcopy(self.review)
        value["items"][0]["relationships"]["source_session_overlap_decision"] = "no_overlap"
        self.reseal(value)
        with self.assertRaisesRegex(review.SymptomReviewError, "overclaims authoritative overlap"):
            self.validate(value)

    def test_resealed_binding_and_review_tampering_fails_closed(self) -> None:
        value = copy.deepcopy(self.review)
        value["items"][0]["commit_oid"] = "1" * 40
        self.reseal(value)
        with self.assertRaisesRegex(review.SymptomReviewError, "commit_oid differs"):
            self.validate(value)

        value = copy.deepcopy(self.review)
        record = value["items"][0]["independent_review"]
        record["checks"]["no_patch_leakage"] = False
        self.reseal_record(record)
        self.reseal(value)
        with self.assertRaisesRegex(review.SymptomReviewError, "accepted review contains a failed check"):
            self.validate(value)

        value = copy.deepcopy(self.review)
        value["items"][0]["source_history"]["source_session_reason"] = "A long but invented source-session conclusion that should not be accepted by the validator."
        self.reseal(value)
        with self.assertRaisesRegex(review.SymptomReviewError, "source-session reason differs"):
            self.validate(value)

    def test_resealed_candidate_identity_and_commitment_tampering_fails_closed(self) -> None:
        for field, replacement in (
            ("candidate_ref", "candidate:tampered"),
            ("parent_oid", "2" * 40),
            ("tree_oid", "3" * 40),
        ):
            with self.subTest(field=field):
                value = copy.deepcopy(self.review)
                value["items"][0][field] = replacement
                self.reseal(value)
                with self.assertRaisesRegex(review.SymptomReviewError, rf"{field} differs"):
                    self.validate(value)

        for field in (
            "source_paths_sha256",
            "test_paths_sha256",
            "source_diff_sha256",
            "test_diff_sha256",
            "test_command_sha256",
            "baseline_output_sha256",
            "reversed_source_output_sha256",
        ):
            with self.subTest(field=field):
                value = copy.deepcopy(self.review)
                value["items"][0]["commitments"][field] = "4" * 64
                self.reseal(value)
                with self.assertRaisesRegex(review.SymptomReviewError, "commitments differ"):
                    self.validate(value)

    def test_resealed_selection_and_reviewer_tampering_fails_closed(self) -> None:
        value = copy.deepcopy(self.review)
        value["selection"]["selected_first_parent_positions"] = value["selection"]["selected_first_parent_positions"][:-1]
        self.reseal(value)
        with self.assertRaisesRegex(review.SymptomReviewError, "selected positions differ"):
            self.validate(value)

        value = copy.deepcopy(self.review)
        value["selection"]["excluded_first_parent_positions"]["negative_control_survived"] = []
        self.reseal(value)
        with self.assertRaisesRegex(review.SymptomReviewError, "excluded positions differ"):
            self.validate(value)

        value = copy.deepcopy(self.review)
        value["reviewers"][0]["role"] = "independent_read_only_auditor"
        value["reviewers"][0]["independence"] = "did_not_edit_draft_or_source"
        self.reseal(value)
        with self.assertRaisesRegex(review.SymptomReviewError, "reviewer roles differ"):
            self.validate(value)

        value = copy.deepcopy(self.review)
        value["reviewers"][1]["identity_attestation"] = "cryptographically_verified"
        self.reseal(value)
        with self.assertRaisesRegex(review.SymptomReviewError, "identity_attestation differs"):
            self.validate(value)

        value = copy.deepcopy(self.review)
        value["reviewers"][0]["reviewer_id"] = "/root/review_negative_control/other"
        self.reseal(value)
        with self.assertRaisesRegex(review.SymptomReviewError, "reviewer_id differs"):
            self.validate(value)

        value = copy.deepcopy(self.review)
        value["reviewers"][0]["tool_identity"] = ""
        self.reseal(value)
        with self.assertRaisesRegex(review.SymptomReviewError, "tool_identity is invalid"):
            self.validate(value)

        value = copy.deepcopy(self.review)
        value["reviewers"][1]["independence"] = "edited_the_draft"
        self.reseal(value)
        with self.assertRaisesRegex(review.SymptomReviewError, "independence differs"):
            self.validate(value)

    def test_input_binding_tampering_fails_closed(self) -> None:
        cases = (
            ("eligibility_ledger_path", "other-eligibility.json", "eligibility path differs"),
            ("eligibility_ledger_sha256", "2" * 64, "eligibility self hash differs"),
            ("eligibility_ledger_file_sha256", "2" * 64, "eligibility file hash differs"),
            ("negative_control_path", "other-negative.json", "negative-control path differs"),
            ("negative_control_receipt_sha256", "2" * 64, "negative-control self hash differs"),
            ("negative_control_file_sha256", "2" * 64, "negative-control file hash differs"),
        )
        for field, replacement, error in cases:
            with self.subTest(field=field):
                value = copy.deepcopy(self.review)
                value["inputs"][field] = replacement
                self.reseal(value)
                with self.assertRaisesRegex(review.SymptomReviewError, error):
                    self.validate(value)

    def test_root_and_relationship_binding_tampering_fails_closed(self) -> None:
        root_cases = (
            ("profile", "other_profile", "profile/status differs"),
            ("status", "authoritative", "profile/status differs"),
            ("exposure", "public", "exposure differs"),
            ("repository_id", "other/repository", "repository differs"),
            ("base_oid", "5" * 40, "range differs"),
            ("head_oid", "6" * 40, "range differs"),
        )
        for field, replacement, error in root_cases:
            with self.subTest(field=field):
                value = copy.deepcopy(self.review)
                value[field] = replacement
                self.reseal(value)
                with self.assertRaisesRegex(review.SymptomReviewError, error):
                    self.validate(value)

        value = copy.deepcopy(self.review)
        value["implementation"]["aggregate_sha256"] = "7" * 64
        self.reseal(value)
        with self.assertRaisesRegex(review.SymptomReviewError, "implementation identity differs"):
            self.validate(value)

        value = copy.deepcopy(self.review)
        value["review_policy"]["name"] = "other_policy"
        self.reseal(value)
        with self.assertRaisesRegex(review.SymptomReviewError, "review policy differs"):
            self.validate(value)

        relationship_cases = (
            ("fix_identity_sha256", "8" * 64, "fix identity differs"),
            ("fix_overlap_decision", "source_commit_overlap_requires_exclusion", "accepted disjoint state"),
            ("family_commitment_sha256", "8" * 64, "family commitment differs"),
            ("prompt_overlap_decision", "exact_duplicate_rejected", "prompt overlap differs"),
            ("patch_overlap_decision", "exact_patch_duplicate_rejected", "patch overlap differs"),
            ("authoritative_overlap_decision", "resolved", "overclaims authoritative overlap"),
        )
        for field, replacement, error in relationship_cases:
            with self.subTest(field=field):
                value = copy.deepcopy(self.review)
                value["items"][0]["relationships"][field] = replacement
                self.reseal(value)
                with self.assertRaisesRegex(review.SymptomReviewError, error):
                    self.validate(value)

    def test_repository_backed_validation_rejects_an_unbound_repository(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            repo = pathlib.Path(temporary)
            self.git(repo, "init", "-q", "-b", "main")
            self.git(repo, "config", "user.name", "Synthetic Reviewer")
            self.git(repo, "config", "user.email", "synthetic@example.invalid")
            (repo / "unbound.txt").write_text("unbound\n", encoding="utf-8")
            self.git(repo, "add", "unbound.txt")
            self.git(repo, "commit", "-q", "-m", "unbound")
            with self.assertRaises(review.SymptomReviewError):
                review.validate_review(
                    self.review,
                    ledger=self.ledger,
                    ledger_raw=self.ledger_raw,
                    negative=self.negative,
                    negative_raw=self.negative_raw,
                    source_repo=repo,
                )

    def test_repository_backed_validation_compares_every_source_history(self) -> None:
        items_by_position = {item["first_parent_position"]: item for item in self.review["items"]}

        def source_history(_repo: pathlib.Path, candidate: dict[str, object]) -> tuple[dict[str, object], set[str]]:
            position = candidate["first_parent_position"]
            commit_oid = candidate["commit_oid"]
            assert isinstance(position, int)
            assert isinstance(commit_oid, str)
            item = items_by_position[position]
            return copy.deepcopy(item["source_history"]), {commit_oid}

        with (
            mock.patch.object(review.task_negative_control, "_validate_repository_binding") as repository_binding,
            mock.patch.object(review.task_negative_control, "_candidate_inputs") as candidate_inputs,
            mock.patch.object(review, "_source_history", side_effect=source_history) as source_histories,
        ):
            review.validate_review(
                self.review,
                ledger=self.ledger,
                ledger_raw=self.ledger_raw,
                negative=self.negative,
                negative_raw=self.negative_raw,
                source_repo=pathlib.Path("/synthetic/bound-repository"),
            )
            repository_binding.assert_called_once()
            self.assertEqual(candidate_inputs.call_count, len(self.ledger["candidates"]))
            self.assertEqual(source_histories.call_count, len(self.review["items"]))

            value = copy.deepcopy(self.review)
            value["items"][0]["source_history"]["source_commit_count"] += 1
            self.reseal(value)
            with self.assertRaisesRegex(review.SymptomReviewError, "source history differs from repository"):
                review.validate_review(
                    value,
                    ledger=self.ledger,
                    ledger_raw=self.ledger_raw,
                    negative=self.negative,
                    negative_raw=self.negative_raw,
                    source_repo=pathlib.Path("/synthetic/bound-repository"),
                )

    def test_source_history_uses_feature_lineage_without_transcript_access(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            repo = pathlib.Path(temporary)
            self.git(repo, "init", "-q", "-b", "main")
            self.git(repo, "config", "user.name", "Synthetic Reviewer")
            self.git(repo, "config", "user.email", "synthetic@example.invalid")
            (repo / "base.txt").write_text("base\n", encoding="utf-8")
            self.git(repo, "add", "base.txt")
            self.git(repo, "commit", "-q", "-m", "base")
            self.git(repo, "checkout", "-q", "-b", "feature")
            (repo / "feature.txt").write_text("feature\n", encoding="utf-8")
            self.git(repo, "add", "feature.txt")
            self.git(
                repo,
                "commit",
                "-q",
                "-m",
                "feature symptom",
                "-m",
                "Entire-Checkpoint: 01KXDSZGRJRHEE2N68N467Y9G6",
            )
            self.git(repo, "checkout", "-q", "main")
            parent = self.git(repo, "rev-parse", "HEAD").strip()
            self.git(repo, "merge", "-q", "--no-ff", "feature", "-m", "merge feature")
            commit = self.git(repo, "rev-parse", "HEAD").strip()
            history, commits = review._source_history(
                repo,
                {"commit_oid": commit, "parent_oid": parent, "merge_parent_count": 2},
            )
            self.assertEqual(history["source_commit_count"], 1)
            self.assertEqual(history["checkpoint_trailer_commit_count"], 1)
            self.assertEqual(history["checkpoint_trailer_coverage"], "complete")
            self.assertEqual(len(commits), 1)
            self.assertEqual(history["source_session_decision"], "unresolved_no_owner_source_session_receipt")

    @staticmethod
    def git(repo: pathlib.Path, *args: str) -> str:
        environment = dict(os.environ)
        environment.update(
            {
                "GIT_AUTHOR_DATE": "2026-07-16T00:00:00+00:00",
                "GIT_COMMITTER_DATE": "2026-07-16T00:00:00+00:00",
            }
        )
        completed = subprocess.run(
            ["git", *args],
            cwd=repo,
            env=environment,
            check=True,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
        return completed.stdout


if __name__ == "__main__":
    unittest.main()
