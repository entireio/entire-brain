#!/usr/bin/env python3
"""Static and synthetic tests for the non-executing run-storage contract."""

from __future__ import annotations

import ast
import copy
import hashlib
import io
import json
import pathlib
import unicodedata
import unittest
from typing import Any
from unittest import mock

import negative_control_run_storage_contract_v1 as storage


HERE = pathlib.Path(__file__).parent
ARTIFACT_PATH = HERE / "negative-control-run-storage-contract-v1.json"
E0_PATH = HERE / "development-task-negative-control-execution-contract-v1.json"
SCHEMA_PATH = HERE / "schemas/negative-control-run-storage-contract-v1.schema.json"
JOURNAL_SCHEMA_PATH = HERE / "schemas/negative-control-run-journal-event-v1.schema.json"
RESERVATION_SCHEMA_PATH = (
    HERE / "schemas/negative-control-capacity-reservation-receipt-v1.schema.json"
)
CLEANUP_SCHEMA_PATH = HERE / "schemas/negative-control-cleanup-attestation-v1.schema.json"


def _load(path: pathlib.Path) -> dict[str, Any]:
    value = json.loads(path.read_text(encoding="utf-8"))
    if type(value) is not dict:
        raise AssertionError(f"{path.name} is not an object")
    return value


def _canonical_hash(value: Any) -> str:
    raw = json.dumps(
        value,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
        allow_nan=False,
    ).encode("utf-8")
    return hashlib.sha256(raw).hexdigest()


def _all_leaf_values(value: Any) -> list[Any]:
    if type(value) is dict:
        return [leaf for child in value.values() for leaf in _all_leaf_values(child)]
    if type(value) is list:
        return [leaf for child in value for leaf in _all_leaf_values(child)]
    return [value]


def _values_for_key(value: Any, key: str) -> list[Any]:
    found: list[Any] = []
    if type(value) is dict:
        for child_key, child in value.items():
            if child_key == key:
                found.append(child)
            found.extend(_values_for_key(child, key))
    elif type(value) is list:
        for child in value:
            found.extend(_values_for_key(child, key))
    return found


class NegativeControlRunStorageContractV1Test(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.artifact = _load(ARTIFACT_PATH)
        cls.e0 = _load(E0_PATH)
        cls.schema = _load(SCHEMA_PATH)
        cls.journal_schema = _load(JOURNAL_SCHEMA_PATH)
        cls.reservation_schema = _load(RESERVATION_SCHEMA_PATH)
        cls.cleanup_schema = _load(CLEANUP_SCHEMA_PATH)

    def test_runtime_evidence_schemas_are_non_authorizing(self) -> None:
        for schema in (
            self.journal_schema,
            self.reservation_schema,
            self.cleanup_schema,
        ):
            authority = schema["properties"]["authority"]["const"]
            self.assertIs(authority["execution_authority"], False)
            self.assertIn("not_authorized", authority["benchmark_execution"])
            self.assertIn("not_authorized", authority["candidate_execution"])
            self.assertIn("not_authorized", authority["model_provider_execution"])
            self.assertIn("not_authorized", authority["paid_execution"])

    def test_checked_artifact_is_exact_deterministic_rebuild(self) -> None:
        raw = ARTIFACT_PATH.read_bytes()
        self.assertEqual(
            hashlib.sha256(raw).hexdigest(),
            "a1effd9ea44f5e13bffc516d1854cdad52936d2afbcbad7534b28b7ed81da671",
        )
        self.assertEqual(raw, storage._render(self.artifact))
        rebuilt = storage.build_contract()
        self.assertEqual(rebuilt, self.artifact)
        self.assertEqual(storage.check_contract(), self.artifact)
        self.assertEqual(
            hashlib.sha256(SCHEMA_PATH.read_bytes()).hexdigest(),
            storage.CHECKED_CONTRACT_SCHEMA_SHA256,
        )

    def test_contract_self_hash_is_exact(self) -> None:
        self.assertEqual(
            self.artifact["contract_sha256"], storage._self_hash(self.artifact)
        )
        self.assertEqual(
            self.artifact["contract_sha256"],
            "a563d6f7ed1deaeb40676dd567f2d23fbd7f827ca2719aeb83f7e4973af4f852",
        )
        self.assertNotEqual(self.artifact["contract_sha256"], "0" * 64)

    def test_contract_authority_and_every_runtime_binding_are_inert(self) -> None:
        self.assertEqual(self.artifact["status"], storage.STATUS)
        self.assertEqual(self.artifact["execution_status"], storage.EXECUTION_STATUS)
        self.assertEqual(self.artifact["authority"], storage.AUTHORITY)
        self.assertEqual(self.artifact["runtime_bindings"], storage.RUNTIME_BINDINGS)
        self.assertTrue(storage.RUNTIME_BINDINGS)
        self.assertTrue(all(value is None for value in storage.RUNTIME_BINDINGS.values()))
        self.assertIs(storage.AUTHORITY["execution_authority"], False)
        self.assertIs(storage.AUTHORITY["atomic_consumption"], False)
        self.assertIs(storage.AUTHORITY["owner_approval"], False)
        for operation in (
            "benchmark_execution",
            "cache_staging",
            "candidate_execution",
            "capacity_reservation",
            "cleanup",
            "filesystem_mutation",
            "journal_persistence",
            "model_provider_execution",
            "network_access",
            "paid_execution",
            "recovery",
            "run_storage_mutation",
            "worktree_creation",
        ):
            self.assertEqual(storage.AUTHORITY[operation], "forbidden", operation)
        self.assertEqual(self.artifact["residual_gates"], storage.RESIDUAL_GATES)
        self.assertIn(
            "successor_owner_approval_and_trust_roots_binding_exact_storage_contract",
            storage.RESIDUAL_GATES,
        )
        self.assertIn(
            "checked_prefix_items_schema_auditor_for_attempt_and_classification_schemas",
            storage.RESIDUAL_GATES,
        )

    def test_frozen_protocol_and_cross_component_projection_are_exact(self) -> None:
        self.assertEqual(
            self.artifact["storage_protocol"],
            storage._storage_protocol(self.e0["state_machine"]),
        )
        self.assertEqual(
            self.artifact["cross_component_invariants"],
            storage._cross_component_invariants(),
        )
        self.assertEqual(
            self.schema["properties"]["storage_protocol"]["const"],
            self.artifact["storage_protocol"],
        )
        self.assertEqual(
            self.schema["properties"]["runtime_bindings"]["const"],
            storage.RUNTIME_BINDINGS,
        )
        boundary = self.artifact["cross_component_invariants"][
            "legacy_schema_audit_boundary"
        ]
        self.assertEqual(
            boundary["checked_draft_validator_unsupported_keyword"], "prefixItems"
        )
        self.assertEqual(
            boundary["exact_raw_bound_schemas"],
            [
                "negative-control-attempt-observations-v1.schema.json",
                "negative-control-classification-receipt-v1.schema.json",
            ],
        )
        self.assertIs(boundary["silent_meta_audit_skip"], False)
        self.assertEqual(
            boundary["execution_effect"],
            "forbidden_until_checked_prefix_items_auditor_exists",
        )

    def test_genesis_is_the_atomic_approval_cas_record(self) -> None:
        all_of = self.journal_schema["allOf"]
        genesis = next(
            rule["then"]["properties"]
            for rule in all_of
            if rule["if"]["properties"]["phase"].get("const") == "GENESIS"
        )
        self.assertEqual(genesis["record_ordinal"]["const"], 0)
        self.assertEqual(
            genesis["event_kind"]["const"],
            "atomic_approval_consumption_and_journal_genesis",
        )
        self.assertEqual(
            genesis["external_effect"]["const"]["effect_kind"],
            "atomic_approval_consumption",
        )
        self.assertEqual(genesis["previous_record_sha256"]["type"], "null")
        self.assertEqual(genesis["prepared_record_sha256"]["type"], "null")
        self.assertEqual(
            genesis["state"]["const"],
            {
                "after": "authority_and_inputs_verified",
                "before": "contract_checked_execution_forbidden",
                "scope": "run",
            },
        )
        protocol = storage._storage_protocol(self.e0["state_machine"])
        approval = protocol["approval_and_genesis"]
        self.assertEqual(
            approval["atomic_record"],
            "single_use_approval_cas_claim_is_same_durable_record_as_journal_genesis",
        )
        self.assertEqual(
            approval["first_external_effect"], "atomic_consumption_genesis_commit"
        )
        self.assertEqual(
            approval["pre_consumption_work"], "read_only_in_memory_only_no_started_run"
        )
        self.assertIn("adapter_forbidden", approval["failure_rule"])
        binding_fields = self.journal_schema["properties"]["bindings"]["required"]
        self.assertIn("approval_file_sha256", binding_fields)
        self.assertIn("approval_self_sha256", binding_fields)
        self.assertIn("signed_payload_sha256", binding_fields)
        self.assertNotIn("approval_sha256", binding_fields)

    def test_owner_v1_cannot_authorize_the_successor_storage_contract(self) -> None:
        owner = _load(HERE / "negative-control-owner-approval-verifier-contract-v1.json")
        binding = owner["execution_contract_binding"]
        self.assertEqual(binding["contract_sha256"], storage.CHECKED_E0_SHA256)
        self.assertEqual(
            binding["state_machine_sha256"], storage.CHECKED_E0_STATE_MACHINE_SHA256
        )
        self.assertNotEqual(
            binding["contract_sha256"], self.artifact["contract_sha256"]
        )
        self.assertEqual(
            self.artifact["cross_component_invariants"]["approval_consumption"][
                "owner_v1_binding_effect"
            ],
            "cannot_authorize_successor_storage_contract",
        )

    def test_e0_state_machine_is_preserved_exactly_with_separate_overlay(self) -> None:
        protocol = storage._storage_protocol(self.e0["state_machine"])
        self.assertEqual(protocol["e0_state_machine"], self.e0["state_machine"])
        self.assertEqual(
            protocol["e0_state_machine_sha256"], storage.CHECKED_E0_STATE_MACHINE_SHA256
        )
        self.assertEqual(
            _canonical_hash(protocol["e0_state_machine"]),
            storage.CHECKED_E0_STATE_MACHINE_SHA256,
        )
        overlay = protocol["lifecycle_overlay"]
        self.assertEqual(
            overlay["initial_state_persistence"],
            "logical_only_until_atomic_consumption_genesis",
        )
        self.assertEqual(
            overlay["recovery_rule"],
            "started_nonterminal_run_latches_no_receipt_before_cleanup_and_never_resumes",
        )
        self.assertEqual(
            overlay["cursor_rule"],
            "plus_one_only_after_matching_attempt_cleanup_committed",
        )
        self.assertEqual(
            overlay["aggregate_receipt_guard"],
            "cursor_248_ceilings_passed_run_cleanup_committed_reservation_release_committed_latch_false",
        )
        self.assertIs(overlay["release_before_receipt"], True)
        self.assertEqual(
            overlay["receipt_forbidden_latch"], "persistent_monotonic_false_to_true"
        )

    def test_prepared_committed_schema_pairing_is_explicit(self) -> None:
        all_of = self.journal_schema["allOf"]
        prepared = next(
            rule["then"]["properties"]
            for rule in all_of
            if rule["if"]["properties"]["phase"].get("const") == "PREPARED"
        )
        committed = next(
            rule["then"]["properties"]
            for rule in all_of
            if rule["if"]["properties"]["phase"].get("const") == "COMMITTED"
        )
        self.assertEqual(prepared["prepared_record_sha256"]["type"], "null")
        self.assertEqual(
            committed["prepared_record_sha256"]["$ref"], "#/$defs/sha256"
        )
        phases = self.journal_schema["properties"]["phase"]["enum"]
        self.assertEqual(phases, ["GENESIS", "PREPARED", "COMMITTED"])

    def test_journal_has_exact_immutable_hash_chain_ceilings(self) -> None:
        self.assertEqual(_values_for_key(self.artifact, "max_record_count"), [20_000])
        self.assertEqual(_values_for_key(self.artifact, "max_record_bytes"), [65_536])
        self.assertEqual(
            _values_for_key(self.artifact, "max_total_journal_bytes"),
            [67_108_864],
        )
        self.assertEqual(
            self.journal_schema["properties"]["record_ordinal"]["maximum"],
            19_999,
        )
        leaves = _all_leaf_values(self.artifact)
        self.assertIn("contiguous_zero_based", leaves)
        self.assertIn("immutable_canonical_o_excl_sha256", leaves)
        self.assertIn(
            "prepared_record_durable_then_effect_then_verify_then_committed_record_durable",
            leaves,
        )
        protocol = storage._storage_protocol(self.e0["state_machine"])["durability"]
        self.assertEqual(protocol["record_encoding"], "canonical_bytes_only")
        self.assertEqual(
            protocol["publication_rule"], "immutable_exclusive_create_no_replace"
        )
        self.assertEqual(
            protocol["stable_control_root"],
            "0700_current_uid_nonsymlink_pinned_descriptor_outside_disposable_run_root",
        )

    def test_capacity_contract_does_not_fabricate_an_eight_gib_hold(self) -> None:
        budget = self.reservation_schema["properties"]["budget"]["const"]
        self.assertEqual(budget["minimum_free_disk_before_staging_bytes"], 17_179_869_184)
        self.assertEqual(budget["max_total_staging_bytes"], 8_589_934_592)
        self.assertEqual(budget["minimum_free_disk_reserve_bytes"], 8_589_934_592)
        self.assertEqual(
            budget["minimum_free_disk_before_staging_bytes"],
            budget["max_total_staging_bytes"]
            + budget["minimum_free_disk_reserve_bytes"],
        )
        reservation = self.reservation_schema["properties"]["reservation"]
        props = reservation["properties"]
        self.assertNotIn("held_physical_bytes", props)
        for name in (
            "adapter_implementation_sha256",
            "accounting_profile",
            "created_at",
            "mechanism_profile",
            "physical_allocation_attestation_sha256",
            "post_effect_free_bytes",
            "reservation_id",
            "reservation_remaining_bytes",
            "staging_actual_bytes",
        ):
            self.assertEqual(props[name]["type"], "null", name)
        self.assertEqual(
            props["independent_hold_requires_preflight_bytes"]["const"],
            25_769_803_776,
        )
        self.assertEqual(props["state"]["const"], "absent_mechanism_unresolved")
        self.assertEqual(
            self.reservation_schema["properties"]["receipt_sha256"]["type"],
            "null",
        )
        observation_props = self.reservation_schema["properties"]["observation"][
            "properties"
        ]
        self.assertTrue(observation_props)
        self.assertTrue(
            all(field_schema.get("type") == "null" for field_schema in observation_props.values())
        )
        binding_props = self.reservation_schema["properties"]["bindings"][
            "properties"
        ]
        for name in (
            "atomic_consumption_genesis_sha256",
            "journal_head_sha256",
            "run_id",
            "run_identity_sha256",
            "run_storage_contract_sha256",
        ):
            self.assertEqual(binding_props[name]["type"], "null", name)
        self.assertEqual(
            self.reservation_schema["properties"]["status"]["const"],
            "capacity_reservation_receipt_shape_frozen_mechanism_unresolved_execution_forbidden",
        )
        capacity = storage._storage_protocol(self.e0["state_machine"])["capacity"]
        self.assertEqual(capacity["minimum_preflight_bytes"], 17_179_869_184)
        self.assertEqual(capacity["max_staging_bytes"], 8_589_934_592)
        self.assertEqual(capacity["minimum_residual_bytes"], 8_589_934_592)
        self.assertEqual(capacity["independent_hold_preflight_bytes"], 25_769_803_776)
        self.assertIsNone(capacity["reservation_mechanism"])
        self.assertEqual(capacity["status"], "research_pending_no_reservation_claim")

    def test_cleanup_schema_freezes_secure_path_boundary(self) -> None:
        cleanup = self.cleanup_schema["properties"]["cleanup"]["properties"]
        self.assertIs(cleanup["descriptor_relative"]["const"], True)
        self.assertIs(cleanup["nofollow_every_component"]["const"], True)
        self.assertIs(cleanup["same_device"]["const"], True)
        self.assertIs(cleanup["postorder"]["const"], True)
        policy = cleanup["path_policy"]["const"]
        self.assertEqual(policy["absolute_paths"], "forbidden")
        self.assertEqual(policy["dot_or_dot_dot_component"], "forbidden")
        self.assertEqual(policy["backslash"], "forbidden")
        self.assertEqual(policy["nul"], "forbidden")
        self.assertEqual(policy["control_unicode_category_c"], "forbidden")
        self.assertEqual(policy["casefold_nfc_dot_dot_namedfork"], "forbidden")
        self.assertEqual(policy["ancestor_conflicts"], "forbidden")
        self.assertEqual(policy["portable_collisions"], "forbidden")
        protocol = storage._storage_protocol(self.e0["state_machine"])
        cleanup_policy = protocol["cleanup"]
        self.assertIs(cleanup_policy["control_root_deleted_with_run_root"], False)
        self.assertEqual(
            cleanup_policy["order"], "descriptor_relative_postorder_descendants_only"
        )
        self.assertIs(
            cleanup_policy["same_device_reverified_at_every_descent_and_before_delete"],
            True,
        )
        self.assertEqual(
            cleanup_policy["target"],
            "disposable_run_root_descendants_never_control_root",
        )
        status_rule = self.cleanup_schema["allOf"][0]
        success_pairs = status_rule["then"]["properties"]["journal"]["oneOf"]
        self.assertEqual(
            [
                {
                    key: schema["const"]
                    for key, schema in pair["properties"].items()
                }
                for pair in success_pairs
            ],
            [
                {
                    "receipt_forbidden_latch": False,
                    "recovery_mode": "normal_cleanup",
                    "terminal_state": "reservation_released",
                },
                {
                    "receipt_forbidden_latch": True,
                    "recovery_mode": "restart_cleanup_no_resume",
                    "terminal_state": "cleaned_no_receipt",
                },
            ],
        )
        failure_journal = status_rule["else"]["properties"]["journal"]["properties"]
        self.assertIs(failure_journal["receipt_forbidden_latch"]["const"], True)
        self.assertEqual(
            failure_journal["terminal_state"]["const"], "cleanup_failed_latched"
        )

    def test_namedfork_rejection_applies_to_every_runtime_path_boundary(self) -> None:
        policy = storage._storage_protocol(self.e0["state_machine"])["path_policy"]
        self.assertEqual(policy["forbidden_component_casefold"], "..namedfork")
        self.assertIs(policy["descriptor_relative_only"], True)
        self.assertIs(policy["serialized_absolute_locator"], False)
        self.assertEqual(
            policy["boundaries"],
            [
                "approval_cas",
                "cache",
                "control_root",
                "journal",
                "lock",
                "run_root",
                "staging",
                "cleanup",
                "worktree",
                "reversal",
                "every_derived_path",
            ],
        )
        self.assertIn("casefold_dotdot_namedfork", policy["forbidden"])
        hostile_components = (
            "..namedfork",
            "..NamedFork",
            "..NAMEDFORK",
            "..namedfor\N{KELVIN SIGN}",
        )
        for component in hostile_components:
            with self.subTest(component=component):
                self.assertEqual(
                    unicodedata.normalize("NFC", component).casefold(),
                    "..namedfork",
                )

    def test_source_has_no_runtime_or_execution_surface(self) -> None:
        source = pathlib.Path(storage.__file__).read_text(encoding="utf-8")
        tree = ast.parse(source)
        imported: set[str] = set()
        for node in ast.walk(tree):
            if isinstance(node, ast.Import):
                imported.update(alias.name.split(".", 1)[0] for alias in node.names)
            elif isinstance(node, ast.ImportFrom) and node.module:
                imported.add(node.module.split(".", 1)[0])
        self.assertTrue(
            imported.isdisjoint(
                {
                    "fcntl",
                    "http",
                    "requests",
                    "socket",
                    "subprocess",
                    "urllib",
                }
            )
        )
        self.assertNotIn("Popen(", source)
        self.assertNotIn("run_candidate", source)
        self.assertNotIn("reserve_capacity", source)
        self.assertNotIn("stage_cache", source)
        function_names = {
            node.name
            for node in ast.walk(tree)
            if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef))
        }
        for fragment in (
            "consume",
            "reserve",
            "stage",
            "recover",
            "cleanup",
            "candidate",
            "worktree",
        ):
            self.assertFalse(
                any(fragment in name for name in function_names),
                f"unexpected runtime function fragment {fragment!r}",
            )

    def test_cli_surface_is_exactly_build_and_check(self) -> None:
        with mock.patch.object(storage, "_write_atomic") as write:
            with mock.patch.object(storage, "build_contract", return_value=self.artifact):
                self.assertEqual(
                    storage.main(["build", "--output", "/synthetic/contract.json"]),
                    0,
                )
        write.assert_called_once()

        summary = {
            "authority": {"execution_authority": False},
            "execution_status": storage.EXECUTION_STATUS,
            "profile": storage.PROFILE,
            "status": storage.STATUS,
        }
        with mock.patch.object(storage, "check_contract", return_value=summary):
            with mock.patch("sys.stdout", new=io.StringIO()):
                self.assertEqual(storage.main(["check"]), 0)
        for command in ("consume", "reserve", "stage", "recover", "cleanup", "run"):
            with self.subTest(command=command), mock.patch(
                "sys.stderr", new=io.StringIO()
            ), self.assertRaises(SystemExit):
                storage.main([command])

    def test_resealed_status_runtime_pin_and_protocol_mutations_fail_closed(self) -> None:
        dependencies = storage._load_dependencies()
        cases: list[tuple[str, Any]] = [
            (
                "status",
                lambda value: value.__setitem__("status", "authorized"),
            ),
            (
                "runtime",
                lambda value: value["runtime_bindings"].__setitem__(
                    "capacity_reservation_receipt_sha256", "a" * 64
                ),
            ),
            (
                "authority",
                lambda value: value["authority"].__setitem__(
                    "filesystem_mutation", "allowed"
                ),
            ),
            (
                "component-pin",
                lambda value: value["component_bindings"][0].__setitem__(
                    "artifact_sha256", "a" * 64
                ),
            ),
            (
                "e0-transition",
                lambda value: value["storage_protocol"]["e0_state_machine"][
                    "run_journal"
                ].__setitem__("success_guard", "receipt_before_cleanup"),
            ),
            (
                "journal-limit",
                lambda value: value["storage_protocol"]["durability"].__setitem__(
                    "max_record_count", 20_001
                ),
            ),
            (
                "capacity-mechanism",
                lambda value: value["storage_protocol"]["capacity"].__setitem__(
                    "reservation_mechanism", "unreviewed"
                ),
            ),
            (
                "namedfork-policy",
                lambda value: value["storage_protocol"]["path_policy"].__setitem__(
                    "forbidden_component_casefold", "none"
                ),
            ),
            (
                "path-boundary",
                lambda value: value["storage_protocol"]["path_policy"][
                    "boundaries"
                ].remove("cache"),
            ),
            (
                "recovery-resume",
                lambda value: value["storage_protocol"]["lifecycle_overlay"].__setitem__(
                    "recovery_rule", "resume_after_restart"
                ),
            ),
            (
                "receipt-before-release",
                lambda value: value["storage_protocol"]["lifecycle_overlay"].__setitem__(
                    "release_before_receipt", False
                ),
            ),
        ]
        for label, mutate in cases:
            value = copy.deepcopy(self.artifact)
            mutate(value)
            value["contract_sha256"] = storage._self_hash(value)
            with self.subTest(label=label), self.assertRaises(
                storage.RunStorageContractError
            ):
                storage._validate_contract(value, dependencies=dependencies)


if __name__ == "__main__":
    unittest.main()
