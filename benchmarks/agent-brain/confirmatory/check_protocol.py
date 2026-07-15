#!/usr/bin/env python3
"""Offline consistency and freeze-gate checks for the WS6 protocol artifacts."""

from __future__ import annotations

import argparse
import hashlib
import json
import pathlib
import re
import sys
from typing import Any


HERE = pathlib.Path(__file__).resolve().parent
REPO = HERE.parents[2]
C0701 = REPO / "benchmarks" / "agent-brain" / "mined-c0701"
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
STATES = ["unseen", "prompt_inspected", "retrieval_probed", "agent_run", "optimization_used"]
ARMS = ["lexical_handrolled", "model2vec_rrf", "embeddinggemma_rrf"]


def load(path: pathlib.Path) -> Any:
    with path.open(encoding="utf-8") as handle:
        return json.load(handle)


def digest(path: pathlib.Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def _error(errors: list[str], condition: bool, message: str) -> None:
    if not condition:
        errors.append(message)


def validate_inventory(inventory: dict[str, Any]) -> list[str]:
    errors: list[str] = []
    configs = sorted((C0701 / "dev").glob("*.json")) + sorted((C0701 / "holdout").glob("*.json"))
    patches = sorted((C0701 / "patches").glob("*-test.patch"))
    ledger = load(C0701 / "selection-ledger.json")
    ledger_by_short = {row["short_sha"]: row for row in ledger["tasks"]}
    actual_by_id: dict[str, tuple[pathlib.Path, dict[str, Any]]] = {}
    actual_by_short: dict[str, tuple[pathlib.Path, dict[str, Any]]] = {}
    for path in configs:
        task = load(path)
        task_id = task["id"]
        short = task["_mined_from_commit"][:9]
        _error(errors, task_id not in actual_by_id, f"duplicate task id in configs: {task_id}")
        _error(errors, short not in actual_by_short, f"duplicate fix commit in configs: {short}")
        actual_by_id[task_id] = (path, task)
        actual_by_short[short] = (path, task)

    inventory_tasks = inventory.get("tasks", [])
    inventory_by_id = {row.get("task_id"): row for row in inventory_tasks}
    _error(errors, inventory.get("schema_version") == 1, "inventory schema_version must be 1")
    _error(errors, inventory.get("state_order") == STATES, "inventory state order changed")
    _error(errors, len(inventory_by_id) == len(inventory_tasks), "inventory task ids are not unique")
    _error(errors, set(inventory_by_id) == set(actual_by_id), "inventory/config task-id sets differ")
    patch_shorts = {path.name.removeprefix("entire-cli-c0701-").removesuffix("-test.patch") for path in patches}
    _error(errors, patch_shorts == set(actual_by_short), "patch/config fix-commit sets differ")
    _error(errors, set(ledger_by_short) == set(actual_by_short), "ledger/config fix-commit sets differ")
    _error(errors, ledger.get("total") == len(actual_by_id), "ledger total is stale")
    _error(errors, ledger.get("dev_count") == sum(path.parent.name == "dev" for path in configs), "ledger dev_count is stale")
    _error(errors, ledger.get("holdout_count") == sum(path.parent.name == "holdout" for path in configs), "ledger holdout_count is stale")

    for task_id, row in inventory_by_id.items():
        if task_id not in actual_by_id:
            continue
        config_path, config = actual_by_id[task_id]
        artifacts = row.get("artifacts", {})
        patch_path = REPO / artifacts.get("patch_path", "missing")
        _error(errors, REPO / artifacts.get("config_path", "missing") == config_path, f"{task_id}: config path mismatch")
        _error(errors, artifacts.get("config_sha256") == digest(config_path), f"{task_id}: config hash mismatch")
        prompt_hash = hashlib.sha256(config["prompt"].encode()).hexdigest()
        _error(errors, artifacts.get("prompt_sha256") == prompt_hash, f"{task_id}: prompt hash mismatch")
        _error(errors, patch_path.is_file(), f"{task_id}: patch is missing")
        if patch_path.is_file():
            _error(errors, artifacts.get("patch_sha256") == digest(patch_path), f"{task_id}: patch hash mismatch")
        _error(errors, row.get("state") in STATES, f"{task_id}: invalid task state")
        _error(errors, bool(row.get("state_recorded_at")), f"{task_id}: state timestamp missing")
        _error(errors, bool(row.get("suite_references")), f"{task_id}: suite/source reference missing")
        _error(errors, row.get("ledger", {}).get("present") is True, f"{task_id}: ledger presence not recorded")
        if row.get("confirmatory_eligible"):
            _error(errors, row.get("state") == "unseen", f"{task_id}: exposed task marked confirmatory eligible")

    summary = inventory.get("summary", {})
    expected = {
        "config_files": len(configs),
        "unique_task_ids": len(actual_by_id),
        "patch_files": len(patches),
        "ledger_rows": len(ledger_by_short),
        "source_dev": sum(path.parent.name == "dev" for path in configs),
        "source_holdout": sum(path.parent.name == "holdout" for path in configs),
        "confirmatory_eligible": sum(bool(row.get("confirmatory_eligible")) for row in inventory_tasks),
    }
    _error(errors, summary == expected, "inventory summary does not match artifacts")
    _error(errors, not (C0701 / "dev" / "entire-cli-c0701-4dd458656.json").exists(), "duplicate 4dd458656 dev config remains")
    _error(errors, "d9df8fcca" in ledger_by_short, "d9df8fcca is absent from the ledger")
    return errors


def validate(freeze: bool = False) -> list[str]:
    errors: list[str] = []
    inventory = load(HERE / "task-inventory.json")
    protocol = load(HERE / "preregistration.json")
    matrix = load(HERE / "engine-matrix.json")
    dataset = load(HERE / "offline-relevance-dataset.json")
    gate = load(HERE / "go-no-go.json")
    errors.extend(validate_inventory(inventory))

    for schema_path in sorted((HERE / "schemas").glob("*.json")):
        schema = load(schema_path)
        _error(errors, schema.get("$schema") == "https://json-schema.org/draft/2020-12/schema", f"{schema_path.name}: wrong JSON Schema dialect")

    arms = matrix.get("arms", [])
    _error(errors, [arm.get("id") for arm in arms] == ARMS, "primary engine arms changed or reordered")
    namespaces = [arm.get("namespace") for arm in arms]
    _error(errors, len(set(namespaces)) == len(namespaces), "engine namespaces are not isolated")
    for arm in arms:
        _error(errors, arm.get("environment", {}).get("ENTIRE_BRAIN_FACTS_BM25") == "0", f"{arm.get('id')}: BM25 must be off")
        _error(errors, arm.get("effective_engine_required") == arm.get("id"), f"{arm.get('id')}: effective engine contract mismatch")
    _error(errors, "--no-semantic" in arms[0].get("cli_flags", []), "lexical arm must explicitly disable semantic ranking")
    _error(errors, arms[1].get("environment", {}).get("ENTIRE_BRAIN_EMBEDDER") == "", "Model2Vec arm must explicitly select bundled default")
    _error(errors, arms[2].get("environment", {}).get("ENTIRE_BRAIN_EMBEDDER") == "ollama", "EmbeddingGemma arm must explicitly select loopback embedder")

    _error(errors, protocol.get("holdout_opened") is False, "fresh holdout was opened during preparation")
    _error(errors, dataset.get("sealed_holdout", {}).get("opened_at") is None, "relevance holdout was opened during preparation")
    checks = gate.get("checks", [])
    _error(errors, len({item.get("id") for item in checks}) == len(checks), "go/no-go check ids are not unique")
    expected_decision = "go" if checks and all(item.get("status") == "pass" for item in checks) else "no_go"
    _error(errors, gate.get("decision") == expected_decision, "go/no-go decision does not match checklist")

    if freeze:
        _error(errors, protocol.get("status") == "frozen_unopened", "protocol is not frozen_unopened")
        _error(errors, all(value == "pass" for value in protocol.get("dependencies", {}).values()), "WS2-WS5 dependencies are pending")
        selection = protocol.get("development_selection", {})
        _error(errors, selection.get("threshold_passed") is True, "offline development threshold has not passed")
        _error(errors, selection.get("selected_k") is not None, "packet K is not frozen")
        _error(errors, selection.get("selected_aggregation_rule") is not None, "aggregation rule is not frozen")
        _error(errors, protocol.get("agent_design", {}).get("power", {}).get("completed") is True, "power calculation is pending")
        _error(errors, SHA256_RE.fullmatch(protocol.get("analyzer_sha256") or "") is not None, "analyzer hash is not frozen")
        budget = protocol.get("paid_budget", {})
        for key in ("model_id", "input_price_per_token", "cached_input_price_per_token", "output_price_per_token", "maximum_usd"):
            _error(errors, budget.get(key) is not None, f"paid budget field is pending: {key}")
        holdout = protocol.get("fresh_holdout", {})
        _error(errors, SHA256_RE.fullmatch(holdout.get("commitment_sha256") or "") is not None, "fresh holdout commitment is not frozen")
        _error(errors, dataset.get("status") == "frozen_unopened", "offline relevance dataset is not frozen_unopened")
        dev_items = dataset.get("development", {}).get("items", [])
        sealed = dataset.get("sealed_holdout", {})
        _error(errors, len(dev_items) >= protocol["offline_dataset"]["minimum_development_queries"], "too few development relevance queries")
        _error(errors, len({item.get("task_id") for item in dev_items}) >= protocol["offline_dataset"]["minimum_development_tasks"], "too few development relevance tasks")
        _error(errors, sealed.get("item_count", 0) >= protocol["offline_dataset"]["minimum_sealed_holdout_queries"], "too few sealed relevance queries")
        _error(errors, sealed.get("unique_task_count", 0) >= protocol["offline_dataset"]["minimum_sealed_holdout_tasks"], "too few sealed relevance tasks")
        _error(errors, SHA256_RE.fullmatch(sealed.get("commitment_sha256") or "") is not None, "relevance holdout commitment is missing")
        _error(errors, all(item.get("status") == "pass" for item in checks), "paid-run checklist is not all pass")
        _error(errors, gate.get("decision") == "go", "paid-run decision is not go")
        freeze_info = protocol.get("freeze", {})
        _error(errors, bool(freeze_info.get("frozen_at")), "freeze timestamp is missing")
        _error(errors, SHA256_RE.fullmatch(freeze_info.get("protocol_sha256") or "") is not None, "protocol freeze hash is missing")
    return errors


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--freeze", action="store_true", help="enforce final-freeze and paid-run gates")
    args = parser.parse_args()
    errors = validate(freeze=args.freeze)
    if errors:
        for error in errors:
            print(f"ERROR: {error}", file=sys.stderr)
        return 1
    mode = "freeze" if args.freeze else "preparation"
    print(f"WS6 {mode} checks passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
