"""Locked v2 confirmatory analysis for the agent-brain benchmark.

The authoritative claim is an intersection-union test: retrieved memory must
clear a frozen practical superiority floor for end-to-end elapsed time,
normalized billed model cost, and task-normalized code quality.  Every
executed attempt enters every endpoint.  Missing, imbalanced, or unauthenticated
measurements make analysis fail closed.

Version-1 schemas remain in the repository for exploratory evidence migration,
but this module emits only ``agent-brain-confirmatory-analysis/v2``.
"""

from __future__ import annotations

import argparse
from decimal import Decimal, InvalidOperation, localcontext
import hashlib
import json
import math
import pathlib
import re
import sys
from collections import defaultdict
from typing import Any, Iterable, Sequence

if __package__ in (None, ""):
    sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))
    from analysis.common import is_executed_run, load_records
    from analysis.evidence import (
        ANALYZER_AGGREGATE_ALGORITHM,
        ANALYZER_RUNTIME_SOURCE_PATHS,
        analyzer_aggregate_sha256,
        current_analyzer_records,
        verify_bundle,
    )
else:
    from .common import is_executed_run, load_records
    from .evidence import (
        ANALYZER_AGGREGATE_ALGORITHM,
        ANALYZER_RUNTIME_SOURCE_PATHS,
        analyzer_aggregate_sha256,
        current_analyzer_records,
        verify_bundle,
    )


REPORT_SCHEMA = "agent-brain-confirmatory-analysis/v2"
SUCCESS_CONTRACT_SCHEMA = "agent-brain-joint-superiority-contract/v2"
BILLING_SCHEMA = "agent-brain-billing-usage/v2"
QUALITY_SCHEMA = "agent-brain-code-quality/v2"
PROVIDER_INVOCATION_SCHEMA = "agent-brain-provider-invocation-state/v1"
PROVIDER_INVOCATIONS_OBSERVED = "provider_invocations_observed"
STRUCTURAL_ZERO_NO_PROVIDER_INVOCATION = "structural_zero_no_provider_invocation"
PROVIDER_PATH_ENTERED_USAGE_UNKNOWN = "provider_path_entered_usage_unknown"
FROZEN_RUNNER_IDENTITY_SCHEMA = "agent-brain-frozen-runner-identity/v1"
EXECUTION_IDENTITY_SCHEMA = "agent-brain-cell-execution-identity/v1"
FROZEN_RUNNER_IDENTITY_FIELDS = (
    "schema",
    "provider",
    "runner_id",
    "runner_version",
    "agent_id",
    "agent_cli",
    "agent_cli_version",
    "requested_model_id",
    "resolved_model_id",
    "effort",
    "schedule_sha256",
    "identity_sha256",
)
EXECUTION_IDENTITY_FIELDS = (
    "schema",
    "provider",
    "runner_id",
    "runner_version",
    "agent_id",
    "agent_cli",
    "agent_cli_version",
    "requested_model_id",
    "resolved_model_id",
    "resolved_model_attestation",
    "effort",
    "schedule_sha256",
    "price_quote_sha256",
    "frozen_runner_identity_sha256",
    "identity_sha256",
)
PRIMARY_ARMS = ("no_memory", "placebo_packet", "retrieved_memory")
BASELINE_ARM = "no_memory"
OPTIMIZED_ARM = "retrieved_memory"
PLACEBO_ARM = "placebo_packet"
DEFAULT_TASKS = 24
DEFAULT_REPETITIONS = 4
DEFAULT_RESAMPLES = 10_000
DEFAULT_SEED = 607_152_026
ALPHA = 0.05
CONFIDENCE = 0.95
PRICE_CATEGORIES = (
    "uncached_input",
    "cache_read_input",
    "cache_write_input",
    "visible_output",
    "reasoning_output",
)
RAW_USAGE_CATEGORIES = (
    "input_tokens",
    "cache_read_input_tokens",
    "cache_write_input_tokens",
    "output_tokens",
    "reasoning_tokens",
)
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
DECIMAL_RE = re.compile(r"^(0|[1-9][0-9]*)(\.[0-9]+)?$")


class AnalysisInputError(ValueError):
    """Raised when a suite cannot safely enter confirmatory analysis."""


def _stable_json_sha256(value: Any) -> str:
    payload = json.dumps(
        value,
        ensure_ascii=False,
        allow_nan=False,
        sort_keys=True,
        separators=(",", ":"),
    )
    return hashlib.sha256(payload.encode("utf-8")).hexdigest()


def _self_hash(value: dict[str, Any], field: str) -> str:
    return _stable_json_sha256({key: item for key, item in value.items() if key != field})


def _nested(record: dict[str, Any], *path: str) -> Any:
    value: Any = record
    for key in path:
        if not isinstance(value, dict):
            return None
        value = value.get(key)
    return value


def _finite_number(value: Any, label: str, *, positive: bool = False) -> float:
    if (
        isinstance(value, bool)
        or not isinstance(value, (int, float))
        or not math.isfinite(float(value))
        or (positive and float(value) <= 0.0)
    ):
        qualifier = "finite positive" if positive else "finite"
        raise AnalysisInputError(f"{label} must be a {qualifier} number, got {value!r}")
    return float(value)


def _nonnegative_int(value: Any, label: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or value < 0:
        raise AnalysisInputError(f"{label} must be a nonnegative integer, got {value!r}")
    return value


def _decimal(value: Any, label: str) -> Decimal:
    if not isinstance(value, str) or DECIMAL_RE.fullmatch(value) is None:
        raise AnalysisInputError(f"{label} must be a non-exponent nonnegative decimal string")
    try:
        parsed = Decimal(value)
    except InvalidOperation as exc:  # pragma: no cover - guarded by the regex
        raise AnalysisInputError(f"{label} is not a valid decimal") from exc
    if not parsed.is_finite() or parsed < 0:
        raise AnalysisInputError(f"{label} must be finite and nonnegative")
    return parsed


def _decimal_string(value: Decimal) -> str:
    rendered = format(value, "f")
    if "." in rendered:
        rendered = rendered.rstrip("0").rstrip(".")
    return rendered or "0"


def _quantile(sorted_values: Sequence[float], probability: float) -> float:
    if not sorted_values:
        raise ValueError("cannot take a quantile of an empty sample")
    if not 0.0 <= probability <= 1.0:
        raise ValueError("quantile probability must be in [0, 1]")
    position = (len(sorted_values) - 1) * probability
    lower = math.floor(position)
    upper = math.ceil(position)
    if lower == upper:
        return float(sorted_values[lower])
    fraction = position - lower
    return float(sorted_values[lower] * (1.0 - fraction) + sorted_values[upper] * fraction)


def _bootstrap_indices(task_count: int, resamples: int, seed: int) -> list[tuple[int, ...]]:
    if task_count < 1:
        raise AnalysisInputError("at least one task cluster is required")
    if isinstance(resamples, bool) or not isinstance(resamples, int) or resamples < 1:
        raise AnalysisInputError("bootstrap resamples must be a positive integer")
    if isinstance(seed, bool) or not isinstance(seed, int):
        raise AnalysisInputError("bootstrap seed must be an integer")
    ceiling = 1 << 256
    limit = ceiling - (ceiling % task_count)
    domain = b"agent-brain-task-cluster-bootstrap-v1\0" + str(seed).encode("ascii") + b"\0"
    required = resamples * task_count
    counter = 0
    draws: list[int] = []
    while len(draws) < required:
        value = int.from_bytes(
            hashlib.sha256(domain + counter.to_bytes(16, "big")).digest(), "big"
        )
        counter += 1
        if value < limit:
            draws.append(value % task_count)
    return [
        tuple(draws[offset : offset + task_count])
        for offset in range(0, required, task_count)
    ]


def _bootstrap_means(values: Sequence[float], samples: Sequence[Sequence[int]]) -> list[float]:
    denominator = len(values)
    return [sum(values[index] for index in draw) / denominator for draw in samples]


def _one_sided_greater_p(
    bootstrap: Sequence[float], point: float, null_value: float
) -> float:
    distance = point - null_value
    exceedances = sum((sample - point) >= distance for sample in bootstrap)
    return (exceedances + 1.0) / (len(bootstrap) + 1.0)


def _one_sided_less_p(bootstrap: Sequence[float], point: float, null_value: float) -> float:
    distance = null_value - point
    exceedances = sum((point - sample) >= distance for sample in bootstrap)
    return (exceedances + 1.0) / (len(bootstrap) + 1.0)


def _two_sided_p(bootstrap: Sequence[float], point: float, null_value: float) -> float:
    distance = abs(point - null_value)
    exceedances = sum(abs(sample - point) >= distance for sample in bootstrap)
    return (exceedances + 1.0) / (len(bootstrap) + 1.0)


def holm_adjust(raw_p_values: dict[str, float], alpha: float = ALPHA) -> dict[str, dict[str, Any]]:
    """Retained for explicitly diagnostic v1 endpoint families."""
    ordered = sorted(raw_p_values.items(), key=lambda item: (item[1], item[0]))
    running_adjusted = 0.0
    rejection_open = True
    result: dict[str, dict[str, Any]] = {}
    for index, (name, raw) in enumerate(ordered):
        if isinstance(raw, bool) or not isinstance(raw, (int, float)) or not 0 <= raw <= 1:
            raise ValueError(f"invalid p-value for {name}: {raw!r}")
        multiplier = len(ordered) - index
        threshold = alpha / multiplier
        running_adjusted = max(running_adjusted, multiplier * raw)
        rejected = rejection_open and raw <= threshold
        rejection_open = rejection_open and rejected
        result[name] = {
            "raw_p_value": raw,
            "holm_adjusted_p_value": min(1.0, running_adjusted),
            "holm_step": index + 1,
            "holm_alpha_threshold": threshold,
            "holm_rejected_at_family_alpha": rejected,
        }
    return result


def normalize_billing_usage(
    raw: dict[str, Any], semantics: dict[str, Any], *, label: str = "billing usage"
) -> dict[str, int]:
    """Return mutually exclusive categories without double-counting inclusive totals."""
    if not isinstance(raw, dict) or set(raw) != set(RAW_USAGE_CATEGORIES):
        raise AnalysisInputError(
            f"{label}.raw must contain exactly {', '.join(RAW_USAGE_CATEGORIES)}"
        )
    counts = {key: _nonnegative_int(raw.get(key), f"{label}.raw.{key}") for key in raw}
    if not isinstance(semantics, dict) or set(semantics) != {
        "input_tokens_includes",
        "output_tokens_includes",
        "counter_absence_means_zero",
    }:
        raise AnalysisInputError(f"{label}.semantics must explicitly name included subcategories")
    absence = semantics.get("counter_absence_means_zero")
    if not isinstance(absence, dict) or set(absence) != {
        "cache_read_input",
        "cache_write_input",
        "reasoning_output",
    } or any(not isinstance(value, bool) for value in absence.values()):
        raise AnalysisInputError(f"{label}: counter-absence semantics are incomplete")
    input_includes = semantics.get("input_tokens_includes")
    output_includes = semantics.get("output_tokens_includes")
    allowed_input = {"cache_read_input", "cache_write_input"}
    if (
        not isinstance(input_includes, list)
        or len(input_includes) != len(set(input_includes))
        or set(input_includes) - allowed_input
    ):
        raise AnalysisInputError(f"{label}: invalid input_tokens_includes")
    included_input = sum(counts[f"{category}_tokens"] for category in input_includes)
    if included_input > counts["input_tokens"]:
        raise AnalysisInputError(f"{label}: included cache input exceeds input total")
    uncached = counts["input_tokens"] - included_input
    if output_includes == ["reasoning_output"]:
        if counts["reasoning_tokens"] > counts["output_tokens"]:
            raise AnalysisInputError(f"{label}: reasoning exceeds inclusive output total")
        output = counts["output_tokens"] - counts["reasoning_tokens"]
    elif output_includes == []:
        output = counts["output_tokens"]
    else:
        raise AnalysisInputError(f"{label}: invalid output_tokens_includes")
    return {
        "uncached_input": uncached,
        "cache_read_input": counts["cache_read_input_tokens"],
        "cache_write_input": counts["cache_write_input_tokens"],
        "visible_output": output,
        "reasoning_output": counts["reasoning_tokens"],
    }


def _validate_price_quote(quote: Any) -> dict[str, Any]:
    if not isinstance(quote, dict):
        raise AnalysisInputError("a frozen price quote object is required")
    # The canonical protocol stores the quote inside pricing-budget.json. Tests
    # and sealed suites may pass the exact nested quote directly.
    if isinstance(quote.get("pricing_quote"), dict):
        quote = quote["pricing_quote"]
    if quote.get("schema") != "agent-brain-price-quote/v2":
        raise AnalysisInputError("price quote schema must be agent-brain-price-quote/v2")
    if quote.get("status") != "pinned":
        raise AnalysisInputError("price quote must be pinned")
    if quote.get("currency") != "USD":
        raise AnalysisInputError("price quote currency must be USD")
    unit = quote.get("tokens_per_price_unit")
    if isinstance(unit, bool) or not isinstance(unit, int) or unit <= 0:
        raise AnalysisInputError("price quote token unit must be a positive integer")
    semantics = quote.get("usage_semantics")
    if not isinstance(semantics, dict) or set(semantics) != {
        "input_tokens_includes",
        "output_tokens_includes",
        "counter_absence_means_zero",
    }:
        raise AnalysisInputError("price quote usage semantics are incomplete")
    input_includes = semantics.get("input_tokens_includes")
    output_includes = semantics.get("output_tokens_includes")
    absence = semantics.get("counter_absence_means_zero")
    if (
        not isinstance(input_includes, list)
        or len(input_includes) != len(set(input_includes))
        or set(input_includes) - {"cache_read_input", "cache_write_input"}
        or output_includes not in ([], ["reasoning_output"])
        or not isinstance(absence, dict)
        or set(absence)
        != {"cache_read_input", "cache_write_input", "reasoning_output"}
        or any(not isinstance(value, bool) for value in absence.values())
    ):
        raise AnalysisInputError("price quote usage semantics are invalid")
    prices = quote.get("prices_usd_per_unit")
    aliases = quote.get("price_aliases")
    if not isinstance(prices, dict) or set(prices) != set(PRICE_CATEGORIES):
        raise AnalysisInputError(
            f"price quote must contain exactly {', '.join(PRICE_CATEGORIES)} prices"
        )
    if not isinstance(aliases, dict) or set(aliases) != set(PRICE_CATEGORIES):
        raise AnalysisInputError("price quote must explicitly bind every category alias")
    for category in PRICE_CATEGORIES:
        price = prices[category]
        alias = aliases[category]
        if (price is None) == (alias is None):
            raise AnalysisInputError(
                f"price quote {category} must set exactly one direct price or alias"
            )
        if price is not None:
            _decimal(price, f"price quote {category}")
        if alias is not None:
            if alias not in PRICE_CATEGORIES or alias == category:
                raise AnalysisInputError(f"price quote {category} alias is invalid")
            if prices.get(alias) is None:
                raise AnalysisInputError(f"price quote {category} alias must target a direct price")
    expected_hash = _self_hash(quote, "quote_sha256")
    if quote.get("quote_sha256") != expected_hash:
        raise AnalysisInputError("price quote self-hash mismatch")
    return quote


def _resolved_prices(quote: dict[str, Any]) -> dict[str, Decimal]:
    prices = quote["prices_usd_per_unit"]
    aliases = quote["price_aliases"]
    return {
        category: _decimal(
            prices[category] if prices[category] is not None else prices[aliases[category]],
            f"price {category}",
        )
        for category in PRICE_CATEGORIES
    }


def _validate_frozen_runner_identity(identity: Any) -> dict[str, Any]:
    if not isinstance(identity, dict) or set(identity) != set(FROZEN_RUNNER_IDENTITY_FIELDS):
        raise AnalysisInputError("frozen runner identity fields are incomplete")
    if identity.get("schema") != FROZEN_RUNNER_IDENTITY_SCHEMA:
        raise AnalysisInputError("frozen runner identity schema changed")
    for field in FROZEN_RUNNER_IDENTITY_FIELDS:
        if field == "schema":
            continue
        value = identity.get(field)
        if not isinstance(value, str) or not value:
            raise AnalysisInputError(f"frozen runner identity {field} is required")
    if identity.get("agent_id") != identity.get("agent_cli"):
        raise AnalysisInputError("frozen agent and CLI identities differ")
    if not SHA256_RE.fullmatch(identity["schedule_sha256"]):
        raise AnalysisInputError("frozen runner schedule identity is invalid")
    if identity.get("identity_sha256") != _self_hash(identity, "identity_sha256"):
        raise AnalysisInputError("frozen runner identity self-hash mismatch")
    return identity


def _validate_cell_execution_identity(
    record: dict[str, Any],
    frozen: dict[str, Any],
    quote: dict[str, Any],
    *,
    provider_state: str,
) -> None:
    run_id = str(record.get("run_id") or "unknown")
    agent_info = record.get("agent_info")
    identity = agent_info.get("execution_identity") if isinstance(agent_info, dict) else None
    if not isinstance(identity, dict) or set(identity) != set(EXECUTION_IDENTITY_FIELDS):
        raise AnalysisInputError(f"{run_id}: full cell execution identity is required")
    if identity.get("schema") != EXECUTION_IDENTITY_SCHEMA:
        raise AnalysisInputError(f"{run_id}: cell execution identity schema changed")
    if identity.get("identity_sha256") != _self_hash(identity, "identity_sha256"):
        raise AnalysisInputError(f"{run_id}: cell execution identity self-hash mismatch")
    expected = {
        "provider": frozen["provider"],
        "runner_id": frozen["runner_id"],
        "runner_version": frozen["runner_version"],
        "agent_id": frozen["agent_id"],
        "agent_cli": frozen["agent_cli"],
        "agent_cli_version": frozen["agent_cli_version"],
        "requested_model_id": frozen["requested_model_id"],
        "resolved_model_id": frozen["resolved_model_id"],
        "effort": frozen["effort"],
        "schedule_sha256": frozen["schedule_sha256"],
        "price_quote_sha256": quote["quote_sha256"],
        "frozen_runner_identity_sha256": frozen["identity_sha256"],
    }
    for field, value in expected.items():
        if identity.get(field) != value:
            raise AnalysisInputError(f"{run_id}: cell execution identity mismatch: {field}")
    expected_attestation = (
        "frozen_expected_no_agent_invocation"
        if provider_state == STRUCTURAL_ZERO_NO_PROVIDER_INVOCATION
        else "agent_cli_reported"
    )
    if identity.get("resolved_model_attestation") != expected_attestation:
        raise AnalysisInputError(f"{run_id}: resolved model attestation is inconsistent")
    runner = record.get("runner")
    if runner != {
        "id": frozen["runner_id"],
        "agent": frozen["agent_id"],
        "model": frozen["requested_model_id"],
        "effort": frozen["effort"],
    }:
        raise AnalysisInputError(f"{run_id}: record runner/model/effort differs from frozen identity")
    if provider_state == PROVIDER_INVOCATIONS_OBSERVED:
        if agent_info.get("resolved_model") != frozen["resolved_model_id"]:
            raise AnalysisInputError(f"{run_id}: observed resolved model differs from frozen identity")
        attempts = agent_info.get("attempts")
        if not isinstance(attempts, list) or any(
            not isinstance(attempt, dict)
            or attempt.get("resolved_model") != frozen["resolved_model_id"]
            for attempt in attempts
        ):
            raise AnalysisInputError(f"{run_id}: attempt resolved model differs from frozen identity")


def _validate_success_contract(contract: Any) -> dict[str, Any]:
    if not isinstance(contract, dict) or contract.get("schema") != SUCCESS_CONTRACT_SCHEMA:
        raise AnalysisInputError(f"success contract schema must be {SUCCESS_CONTRACT_SCHEMA}")
    if contract.get("primary_contrast") != "retrieved_memory_vs_no_memory":
        raise AnalysisInputError("success contract primary contrast changed")
    if contract.get("intersection_union_alpha") != ALPHA:
        raise AnalysisInputError("success contract intersection-union alpha changed")
    endpoints = contract.get("endpoints")
    if not isinstance(endpoints, dict) or set(endpoints) != {
        "elapsed_time",
        "normalized_cost",
        "code_quality",
    }:
        raise AnalysisInputError("success contract must define exactly three co-primary endpoints")
    required = {
        "elapsed_time": ("lower", "paired_task_geometric_mean_ratio", "practical_ratio_max"),
        "normalized_cost": (
            "lower",
            "ratio_of_equal_task_weighted_task_arm_mean_costs",
            "practical_ratio_max",
        ),
        "code_quality": ("higher", "paired_task_mean_difference", "practical_difference_min"),
    }
    for name, (direction, estimand, floor_key) in required.items():
        endpoint = endpoints.get(name)
        if not isinstance(endpoint, dict):
            raise AnalysisInputError(f"success contract {name} must be an object")
        if endpoint.get("direction") != direction or endpoint.get("estimand") != estimand:
            raise AnalysisInputError(f"success contract {name} estimand or direction changed")
        floor = _finite_number(endpoint.get(floor_key), f"success contract {name}.{floor_key}")
        if name != "code_quality" and not 0 < floor < 1:
            raise AnalysisInputError(f"success contract {name} ratio floor must be in (0,1)")
        if name == "code_quality" and not 0 < floor <= 1:
            raise AnalysisInputError("success contract quality floor must be in (0,1]")
        if endpoint.get("floor_status") not in {"provisional", "frozen_approved"}:
            raise AnalysisInputError(f"success contract {name} floor_status is invalid")
    quality_measurement = contract.get("code_quality_measurement")
    if quality_measurement != {
        "schema": QUALITY_SCHEMA,
        "rubric": "task_relative_output_outcome_patch_focus_v2",
        "included_components": ["outcome", "patch_focus"],
        "normalization_denominator_points": 75,
        "excluded_components": [
            "validation_discipline",
            "runtime_efficiency",
            "brain_use",
        ],
        "critical_failure_forces_zero": True,
    }:
        raise AnalysisInputError("success contract code-quality measurement changed")
    timeout = contract.get("timeout_policy")
    if not isinstance(timeout, dict):
        raise AnalysisInputError("success contract timeout_policy is required")
    if timeout.get("policy") != "retain_measured_end_to_end_elapsed_no_component_cap_substitution":
        raise AnalysisInputError("unsupported timeout policy")
    if (
        timeout.get("elapsed_field") != "timing.end_to_end_user_visible_wall_seconds"
        or timeout.get("substitute_component_timeout_limit") is not False
        or timeout.get("provider_retry_limit") != 0
    ):
        raise AnalysisInputError("timeout/retry observation contract changed")
    _finite_number(
        timeout.get("agent_timeout_limit_seconds"),
        "agent timeout limit",
        positive=True,
    )
    if timeout.get("status") not in {"provisional", "frozen_approved"}:
        raise AnalysisInputError("timeout policy status is invalid")
    expected_hash = _self_hash(contract, "contract_sha256")
    if contract.get("contract_sha256") != expected_hash:
        raise AnalysisInputError("success contract self-hash mismatch")
    return contract


def _normalized_cost(
    billing: Any, quote: dict[str, Any], *, run_id: str
) -> tuple[float, str, dict[str, int]]:
    label = f"{run_id}: billing_v2"
    if not isinstance(billing, dict) or billing.get("schema") != BILLING_SCHEMA:
        raise AnalysisInputError(f"{label} is required")
    if billing.get("price_quote_sha256") != quote.get("quote_sha256"):
        raise AnalysisInputError(f"{label} price quote hash mismatch")
    raw = billing.get("raw")
    semantics = billing.get("semantics")
    if semantics != quote.get("usage_semantics"):
        raise AnalysisInputError(f"{label} usage semantics differ from frozen quote")
    exclusive = billing.get("exclusive")
    derived = normalize_billing_usage(raw, semantics, label=label)
    if not isinstance(exclusive, dict) or set(exclusive) != set(PRICE_CATEGORIES):
        raise AnalysisInputError(f"{label}.exclusive is missing a cost category")
    recorded = {
        key: _nonnegative_int(exclusive.get(key), f"{label}.exclusive.{key}")
        for key in PRICE_CATEGORIES
    }
    if recorded != derived:
        raise AnalysisInputError(f"{label} exclusive categories do not match raw inclusion semantics")
    prices = _resolved_prices(quote)
    with localcontext() as context:
        context.prec = 60
        cost = sum(
            (Decimal(recorded[key]) * prices[key] for key in PRICE_CATEGORIES),
            Decimal("0"),
        ) / Decimal(quote["tokens_per_price_unit"])
    if cost < 0:  # pragma: no cover - prices and token counters are nonnegative
        raise AnalysisInputError(f"{label} normalized billed cost must be nonnegative")
    rendered = _decimal_string(cost)
    recorded_cost = billing.get("normalized_cost_usd")
    if recorded_cost is not None and recorded_cost != rendered:
        raise AnalysisInputError(f"{label} normalized cost is stale or tampered")
    return float(cost), rendered, recorded


def _validated_attempt_aggregate_cost(
    record: dict[str, Any], quote: dict[str, Any], *, run_id: str
) -> tuple[float, str, dict[str, int]]:
    """Reconcile provider attempts or an authenticated pre-provider structural zero."""
    agent_info = record.get("agent_info")
    if not isinstance(agent_info, dict):
        raise AnalysisInputError(f"{run_id}: agent_info is required")
    attempts = agent_info.get("attempts")
    if not isinstance(attempts, list):
        raise AnalysisInputError(f"{run_id}: attempt-level billing evidence must be an array")
    provider = agent_info.get("provider_invocation")
    if not isinstance(provider, dict) or provider.get("schema") != PROVIDER_INVOCATION_SCHEMA:
        raise AnalysisInputError(f"{run_id}: authenticated provider invocation state is required")
    integrity = agent_info.get("billing_integrity")
    if not isinstance(integrity, dict) or integrity.get("schema") != "agent-brain-attempt-billing-integrity/v1":
        raise AnalysisInputError(f"{run_id}: attempt billing integrity record is required")

    if provider.get("state") == STRUCTURAL_ZERO_NO_PROVIDER_INVOCATION:
        if (
            record.get("agent_ran") is not False
            or provider.get("invocation_count") != 0
            or provider.get("attestation") != "harness_control_flow_run_agent_not_entered"
            or provider.get("reason")
            not in {
                "treatment_retrieval_or_delivery_timeout",
                "treatment_retrieval_or_delivery_failure",
            }
            or attempts != []
            or agent_info.get("returncode") is not None
        ):
            raise AnalysisInputError(f"{run_id}: structural-zero provider attestation is inconsistent")
        if (
            integrity.get("required") is not True
            or integrity.get("passed") is not True
            or integrity.get("attempt_count") != 0
            or integrity.get("complete_attempts") != 0
            or integrity.get("incomplete_attempts") != []
            or integrity.get("aggregate_present") is not True
            or integrity.get("aggregation") != STRUCTURAL_ZERO_NO_PROVIDER_INVOCATION
        ):
            raise AnalysisInputError(f"{run_id}: structural-zero billing integrity did not pass")
        aggregate_report = _nested(agent_info, "usage", "usage_report")
        if aggregate_report != {
            "complete": True,
            "parser": STRUCTURAL_ZERO_NO_PROVIDER_INVOCATION,
            "accounting_basis": STRUCTURAL_ZERO_NO_PROVIDER_INVOCATION,
            "source_events": 0,
            "attempt_count": 0,
            "complete_attempts": [],
            "incomplete_attempts": [],
            "error": None,
        }:
            raise AnalysisInputError(f"{run_id}: structural-zero usage report is inconsistent")
        aggregate = _nested(agent_info, "usage", "billing_v2")
        cost, cost_text, exclusive = _normalized_cost(aggregate, quote, run_id=run_id)
        assert isinstance(aggregate, dict)
        if (
            cost != 0.0
            or any(value != 0 for value in aggregate.get("raw", {}).values())
            or any(value != 0 for value in exclusive.values())
        ):
            raise AnalysisInputError(f"{run_id}: structural-zero billing must contain only zeros")
        return cost, cost_text, exclusive

    if provider.get("state") != PROVIDER_INVOCATIONS_OBSERVED:
        raise AnalysisInputError(f"{run_id}: provider path entry or usage is ambiguous")
    if (
        record.get("agent_ran") is not True
        or provider.get("invocation_count") != len(attempts)
        or provider.get("attestation") != "retained_attempt_ledger"
        or provider.get("reason") is not None
    ):
        raise AnalysisInputError(f"{run_id}: provider invocation ledger attestation is inconsistent")
    if len(attempts) != 1:
        raise AnalysisInputError(f"{run_id}: confirmatory provider retry limit is zero")
    if (
        integrity.get("required") is not True
        or integrity.get("passed") is not True
        or integrity.get("attempt_count") != len(attempts)
        or integrity.get("complete_attempts") != len(attempts)
        or integrity.get("incomplete_attempts") != []
        or integrity.get("aggregate_present") is not True
        or integrity.get("aggregation")
        != "sum_mutually_exclusive_categories_across_isolated_invocations"
    ):
        raise AnalysisInputError(f"{run_id}: attempt billing integrity did not pass")
    raw_sums = {key: 0 for key in RAW_USAGE_CATEGORIES}
    exclusive_sums = {key: 0 for key in PRICE_CATEGORIES}
    for index, attempt in enumerate(attempts, 1):
        if not isinstance(attempt, dict) or attempt.get("attempt") != index:
            raise AnalysisInputError(f"{run_id}: attempt billing evidence is out of order")
        usage = attempt.get("usage")
        if not isinstance(usage, dict):
            raise AnalysisInputError(f"{run_id}: attempt {index} usage is required")
        report = usage.get("usage_report")
        if not isinstance(report, dict) or report.get("complete") is not True:
            raise AnalysisInputError(f"{run_id}: attempt {index} provider usage is incomplete")
        billing = usage.get("billing_v2")
        _normalized_cost(billing, quote, run_id=f"{run_id}/attempt-{index}")
        assert isinstance(billing, dict)
        for key in RAW_USAGE_CATEGORIES:
            raw_sums[key] += _nonnegative_int(
                billing["raw"].get(key), f"{run_id}/attempt-{index}: raw.{key}"
            )
        for key in PRICE_CATEGORIES:
            exclusive_sums[key] += _nonnegative_int(
                billing["exclusive"].get(key),
                f"{run_id}/attempt-{index}: exclusive.{key}",
            )
    aggregate = _nested(agent_info, "usage", "billing_v2")
    cost, cost_text, aggregate_exclusive = _normalized_cost(
        aggregate, quote, run_id=run_id
    )
    assert isinstance(aggregate, dict)
    if aggregate.get("raw") != raw_sums or aggregate_exclusive != exclusive_sums:
        raise AnalysisInputError(f"{run_id}: aggregate billing does not equal attempt sum")
    aggregate_report = _nested(agent_info, "usage", "usage_report")
    if (
        not isinstance(aggregate_report, dict)
        or aggregate_report.get("complete") is not True
        or aggregate_report.get("accounting_basis")
        != "sum_per_isolated_provider_invocation_attempt_total"
        or aggregate_report.get("attempt_count") != len(attempts)
        or aggregate_report.get("incomplete_attempts") != []
    ):
        raise AnalysisInputError(f"{run_id}: aggregate provider usage report is incomplete")
    return cost, cost_text, aggregate_exclusive


def _runner_id(record: dict[str, Any]) -> str | None:
    runner = record.get("runner")
    value = runner.get("id") if isinstance(runner, dict) else None
    return value if isinstance(value, str) and value else None


def _schedule_cell_key(value: dict[str, Any]) -> tuple[str, str, str, int] | None:
    runner = value.get("runner")
    runner_id = runner.get("id") if isinstance(runner, dict) else None
    repetition = value.get("repetition")
    if (
        not isinstance(value.get("task_id"), str)
        or not isinstance(runner_id, str)
        or not isinstance(value.get("condition"), str)
        or isinstance(repetition, bool)
        or not isinstance(repetition, int)
    ):
        return None
    return (value["task_id"], runner_id, value["condition"], repetition)


def _validate_expected_schedule(
    records: Sequence[dict[str, Any]], expected_schedule_cells: Sequence[dict[str, Any]]
) -> None:
    expected_by_run: dict[str, tuple[str, str, str, int]] = {}
    for index, cell in enumerate(expected_schedule_cells):
        run_id = cell.get("run_id") if isinstance(cell, dict) else None
        key = _schedule_cell_key(cell) if isinstance(cell, dict) else None
        if not isinstance(run_id, str) or not run_id or key is None:
            raise AnalysisInputError(f"schedule cell {index} has an invalid identity")
        if run_id in expected_by_run:
            raise AnalysisInputError(f"duplicate schedule run_id: {run_id}")
        expected_by_run[run_id] = key
    actual_by_run: dict[str, tuple[str, str, str, int]] = {}
    for index, record in enumerate(records):
        run_id = record.get("run_id") if isinstance(record, dict) else None
        key = _schedule_cell_key(record) if isinstance(record, dict) else None
        if not isinstance(run_id, str) or not run_id or key is None:
            raise AnalysisInputError(f"record {index} has an invalid schedule identity")
        if run_id in actual_by_run:
            raise AnalysisInputError(f"duplicate run_id: {run_id}")
        actual_by_run[run_id] = key
    missing = sorted(set(expected_by_run) - set(actual_by_run))
    extra = sorted(set(actual_by_run) - set(expected_by_run))
    if missing or extra:
        raise AnalysisInputError(
            f"records do not match verified schedule: missing={missing[:5]}, extra={extra[:5]}"
        )
    mismatched = sorted(
        run_id for run_id in expected_by_run if expected_by_run[run_id] != actual_by_run[run_id]
    )
    if mismatched:
        raise AnalysisInputError(f"record identities disagree with schedule: {mismatched[:5]}")


def _prepare_cells(
    records: Sequence[dict[str, Any]],
    *,
    expected_task_ids: Sequence[str],
    expected_runner_id: str,
    repetitions: int,
    expected_schedule_cells: Sequence[dict[str, Any]] | None,
    success_contract: dict[str, Any],
    price_quote: dict[str, Any],
    runner_identity: dict[str, Any],
) -> tuple[list[dict[str, Any]], dict[str, Any]]:
    task_ids = list(expected_task_ids)
    if not task_ids or any(not isinstance(item, str) or not item for item in task_ids):
        raise AnalysisInputError("expected_task_ids must be a non-empty sequence of task IDs")
    if len(task_ids) != len(set(task_ids)):
        raise AnalysisInputError("expected_task_ids contains duplicates")
    if not isinstance(expected_runner_id, str) or not expected_runner_id:
        raise AnalysisInputError("expected_runner_id is required")
    if isinstance(repetitions, bool) or not isinstance(repetitions, int) or repetitions < 1:
        raise AnalysisInputError("repetitions must be a positive integer")
    if expected_schedule_cells is not None:
        _validate_expected_schedule(records, expected_schedule_cells)
    expected_count = len(task_ids) * len(PRIMARY_ARMS) * repetitions
    if len(records) != expected_count:
        raise AnalysisInputError(f"expected {expected_count} primary cells, found {len(records)}")

    expected_tasks = set(task_ids)
    seen_run_ids: set[str] = set()
    seen_cells: set[tuple[str, str, int]] = set()
    observed_tasks: set[str] = set()
    observed_arms: set[str] = set()
    condition_to_arm: dict[str, str] = {}
    arm_to_condition: dict[str, str] = {}
    missing_validation_scored_incorrect = 0
    critical_quality_zeroes = 0
    timed_out_attempts = 0
    structural_zero_attempts = 0
    cells: list[dict[str, Any]] = []
    timeout_policy = success_contract["timeout_policy"]
    frozen_agent_timeout = float(timeout_policy["agent_timeout_limit_seconds"])

    for index, record in enumerate(records):
        prefix = f"record[{index}]"
        if not isinstance(record, dict):
            raise AnalysisInputError(f"{prefix} must be an object")
        run_id = record.get("run_id")
        if not isinstance(run_id, str) or not run_id:
            raise AnalysisInputError(f"{prefix}.run_id is required")
        if run_id in seen_run_ids:
            raise AnalysisInputError(f"duplicate run_id: {run_id}")
        seen_run_ids.add(run_id)
        if record.get("treatment_started") is not True or not is_executed_run(record):
            raise AnalysisInputError(
                f"{run_id}: cell did not explicitly start the causal treatment interval"
            )
        if record.get("analysis_excluded"):
            raise AnalysisInputError(f"{run_id}: analysis-excluded attempts cannot fill a requested cell")

        task_id = record.get("task_id")
        if not isinstance(task_id, str) or task_id not in expected_tasks:
            raise AnalysisInputError(f"{run_id}: unexpected or missing task_id {task_id!r}")
        if _runner_id(record) != expected_runner_id:
            raise AnalysisInputError(f"{run_id}: runner does not match {expected_runner_id!r}")
        condition = record.get("condition")
        repetition = record.get("repetition")
        if not isinstance(condition, str) or not condition:
            raise AnalysisInputError(f"{run_id}: condition is required")
        if isinstance(repetition, bool) or not isinstance(repetition, int) or not 1 <= repetition <= repetitions:
            raise AnalysisInputError(f"{run_id}: invalid repetition {repetition!r}")
        treatment = record.get("treatment")
        if not isinstance(treatment, dict):
            raise AnalysisInputError(f"{run_id}: explicit treatment metadata is required")
        arm = treatment.get("arm")
        if arm not in PRIMARY_ARMS:
            raise AnalysisInputError(f"{run_id}: non-primary or invalid treatment arm {arm!r}")
        if treatment.get("confirmatory_eligible") is not True:
            raise AnalysisInputError(f"{run_id}: treatment is not confirmatory eligible")
        if treatment.get("query_source") != "user_query" or record.get("retrieval_query_source") != "user_query":
            raise AnalysisInputError(f"{run_id}: primary treatments must use the user_query source")
        if condition_to_arm.setdefault(condition, arm) != arm:
            raise AnalysisInputError(f"condition {condition!r} maps to multiple treatment arms")
        if arm_to_condition.setdefault(arm, condition) != condition:
            raise AnalysisInputError(f"treatment arm {arm!r} maps to multiple conditions")
        cell_key = (task_id, arm, repetition)
        if cell_key in seen_cells:
            raise AnalysisInputError(f"duplicate task/arm/repetition cell: {cell_key}")
        seen_cells.add(cell_key)
        observed_tasks.add(task_id)
        observed_arms.add(arm)

        validation = record.get("validation")
        validation_ok = validation.get("ok") if isinstance(validation, dict) else None
        if not isinstance(validation_ok, bool):
            if not record.get("error"):
                raise AnalysisInputError(f"{run_id}: validation.ok must be boolean")
            validation_ok = False
            missing_validation_scored_incorrect += 1
        agent_info = record.get("agent_info")
        provider = agent_info.get("provider_invocation") if isinstance(agent_info, dict) else None
        return_code = agent_info.get("returncode") if isinstance(agent_info, dict) else None
        provider_state = provider.get("state") if isinstance(provider, dict) else None
        if provider_state == STRUCTURAL_ZERO_NO_PROVIDER_INVOCATION:
            if record.get("agent_ran") is not False or return_code is not None:
                raise AnalysisInputError(
                    f"{run_id}: no-provider structural zero requires agent_ran=false and null returncode"
                )
        elif provider_state == PROVIDER_PATH_ENTERED_USAGE_UNKNOWN:
            raise AnalysisInputError(f"{run_id}: provider path entry or usage is ambiguous")
        elif isinstance(return_code, bool) or not isinstance(return_code, int):
            raise AnalysisInputError(f"{run_id}: launched agent returncode must be an integer")
        _validate_cell_execution_identity(
            record,
            runner_identity,
            price_quote,
            provider_state=provider_state,
        )

        timing = record.get("timing")
        if not isinstance(timing, dict):
            raise AnalysisInputError(f"{run_id}: timing is required")
        if timing.get("primary") != "end_to_end_user_visible_wall_seconds":
            raise AnalysisInputError(f"{run_id}: primary timing field changed")
        if (
            timing.get("pre_treatment_setup_included_in_primary") is not False
            or timing.get("treatment_retrieval_included_in_primary") is not True
            or timing.get("hidden_validation_included_in_primary") is not False
        ):
            raise AnalysisInputError(f"{run_id}: primary timing boundary flags changed")
        elapsed = _finite_number(
            timing.get("end_to_end_user_visible_wall_seconds"),
            f"{run_id}: end_to_end_user_visible_wall_seconds",
            positive=True,
        )
        timed_out = timing.get("timeout_occurred")
        if not isinstance(timed_out, bool):
            raise AnalysisInputError(f"{run_id}: timeout_occurred must be boolean")
        recorded_agent_limit = _finite_number(
            timing.get("agent_timeout_limit_seconds"),
            f"{run_id}: agent_timeout_limit_seconds",
            positive=True,
        )
        if recorded_agent_limit != frozen_agent_timeout:
            raise AnalysisInputError(
                f"{run_id}: agent timeout limit differs from frozen policy"
            )
        if timed_out:
            if timing.get("timeout_stage") not in {
                "treatment_retrieval_or_delivery",
                "agent_execution",
            }:
                raise AnalysisInputError(f"{run_id}: timeout stage is missing or invalid")
            _finite_number(
                timing.get("timeout_component_limit_seconds"),
                f"{run_id}: timeout_component_limit_seconds",
                positive=True,
            )
            timed_out_attempts += 1
        else:
            if timing.get("timeout_stage") is not None:
                raise AnalysisInputError(f"{run_id}: non-timeout cell records a timeout stage")
            if timing.get("timeout_component_limit_seconds") is not None:
                raise AnalysisInputError(
                    f"{run_id}: non-timeout cell records a component timeout limit"
                )

        cost, cost_text, exclusive_usage = _validated_attempt_aggregate_cost(
            record, price_quote, run_id=run_id
        )
        if isinstance(provider, dict) and provider.get("state") == STRUCTURAL_ZERO_NO_PROVIDER_INVOCATION:
            structural_zero_attempts += 1
        raw_total_tokens = _nested(record, "agent_info", "usage", "total_tokens")
        if raw_total_tokens is not None:
            _finite_number(raw_total_tokens, f"{run_id}: raw total_tokens")

        quality = record.get("code_quality")
        if not isinstance(quality, dict) or quality.get("schema") != QUALITY_SCHEMA:
            raise AnalysisInputError(f"{run_id}: {QUALITY_SCHEMA} record is required")
        raw_quality = _finite_number(
            quality.get("task_normalized_score"), f"{run_id}: task_normalized_score"
        )
        if not 0.0 <= raw_quality <= 1.0:
            raise AnalysisInputError(f"{run_id}: task_normalized_score must be in [0,1]")
        declared_critical = quality.get("critical_failure")
        reasons = quality.get("critical_failure_reasons")
        if not isinstance(declared_critical, bool):
            raise AnalysisInputError(f"{run_id}: critical_failure must be boolean")
        if not isinstance(reasons, list) or any(not isinstance(item, str) or not item for item in reasons):
            raise AnalysisInputError(f"{run_id}: critical_failure_reasons must be strings")
        if len(reasons) != len(set(reasons)):
            raise AnalysisInputError(f"{run_id}: critical_failure_reasons contains duplicates")
        if declared_critical != bool(reasons):
            raise AnalysisInputError(
                f"{run_id}: critical_failure must equal whether critical_failure_reasons is non-empty"
            )
        forced_critical = bool(not validation_ok or return_code != 0 or record.get("error"))
        if forced_critical and not declared_critical:
            raise AnalysisInputError(
                f"{run_id}: validation, return-code, or record error requires a declared critical failure"
            )
        critical = declared_critical
        if critical and raw_quality != 0.0:
            raise AnalysisInputError(
                f"{run_id}: critical failures must record task_normalized_score as zero"
            )
        effective_quality = 0.0 if critical else raw_quality
        if critical:
            critical_quality_zeroes += 1

        api_seconds = _nested(record, "timing", "agent_reported_api_seconds")
        if api_seconds is not None:
            api_seconds = _finite_number(
                api_seconds, f"{run_id}: agent_reported_api_seconds", positive=True
            )
        harness_seconds = _nested(record, "timing", "harness_agent_interval_wall_seconds")
        if harness_seconds is not None:
            harness_seconds = _finite_number(
                harness_seconds,
                f"{run_id}: harness_agent_interval_wall_seconds",
                positive=True,
            )
        cells.append(
            {
                "run_id": run_id,
                "task_id": task_id,
                "arm": arm,
                "repetition": repetition,
                "correct": 1.0 if validation_ok else 0.0,
                "elapsed_time": elapsed,
                "normalized_cost": cost,
                "normalized_cost_usd": cost_text,
                "quality": effective_quality,
                "raw_quality": raw_quality,
                "quality_critical": critical,
                "timed_out": timed_out,
                "exclusive_usage": exclusive_usage,
                "raw_total_tokens": float(raw_total_tokens) if raw_total_tokens is not None else None,
                "harness_agent_seconds": harness_seconds,
                "agent_api_seconds": api_seconds,
            }
        )

    if observed_tasks != expected_tasks:
        raise AnalysisInputError(f"task set is incomplete: missing={sorted(expected_tasks - observed_tasks)}")
    if observed_arms != set(PRIMARY_ARMS):
        raise AnalysisInputError("primary treatment set mismatch")
    if len(condition_to_arm) != len(PRIMARY_ARMS):
        raise AnalysisInputError("conditions and primary treatment arms must have a one-to-one mapping")
    expected_repetitions = set(range(1, repetitions + 1))
    grouped: dict[tuple[str, str], set[int]] = defaultdict(set)
    for cell in cells:
        grouped[(cell["task_id"], cell["arm"])].add(cell["repetition"])
    for task_id in sorted(expected_tasks):
        for arm in PRIMARY_ARMS:
            if grouped.get((task_id, arm), set()) != expected_repetitions:
                raise AnalysisInputError(f"unbalanced cell {task_id}/{arm}")
    cells.sort(key=lambda cell: (cell["task_id"], cell["arm"], cell["repetition"]))
    return cells, {
        "valid_executed_attempts": len(cells),
        "expected_attempts": expected_count,
        "missing_validation_scored_incorrect": missing_validation_scored_incorrect,
        "critical_quality_zeroes": critical_quality_zeroes,
        "timed_out_attempts": timed_out_attempts,
        "structural_zero_no_provider_attempts": structural_zero_attempts,
        "frozen_runner_identity_sha256": runner_identity["identity_sha256"],
        "condition_to_treatment_arm": dict(sorted(condition_to_arm.items())),
    }


def _task_arm_means(
    cells: Sequence[dict[str, Any]], task_ids: Sequence[str], field: str
) -> dict[str, dict[str, float]]:
    values: dict[tuple[str, str], list[float]] = defaultdict(list)
    for cell in cells:
        values[(cell["task_id"], cell["arm"])].append(float(cell[field]))
    return {
        task_id: {
            arm: sum(values[(task_id, arm)]) / len(values[(task_id, arm)])
            for arm in PRIMARY_ARMS
        }
        for task_id in task_ids
    }


def _placebo_ratio_diagnostic(
    means: dict[str, dict[str, float]], task_ids: Sequence[str], samples: Sequence[Sequence[int]]
) -> dict[str, Any]:
    values = [math.log(means[task][PLACEBO_ARM] / means[task][BASELINE_ARM]) for task in task_ids]
    point = sum(values) / len(values)
    bootstrap = sorted(_bootstrap_means(values, samples))
    return {
        "comparison": "placebo_packet_vs_no_memory",
        "role": "diagnostic_not_in_joint_verdict",
        "n_task_clusters": len(task_ids),
        "paired_task_geometric_mean_ratio": math.exp(point),
        "two_sided_percentile_ci": [
            math.exp(_quantile(bootstrap, ALPHA / 2)),
            math.exp(_quantile(bootstrap, 1 - ALPHA / 2)),
        ],
        "bootstrap_p_value_two_sided_log_ratio_zero": _two_sided_p(bootstrap, point, 0.0),
    }


def _ratio_endpoint(
    means: dict[str, dict[str, float]],
    task_ids: Sequence[str],
    samples: Sequence[Sequence[int]],
    *,
    practical_ratio_max: float,
    source_field: str,
    estimand: str,
) -> dict[str, Any]:
    values = [math.log(means[task][OPTIMIZED_ARM] / means[task][BASELINE_ARM]) for task in task_ids]
    point = sum(values) / len(values)
    bootstrap = sorted(_bootstrap_means(values, samples))
    upper_log = _quantile(bootstrap, 1.0 - ALPHA)
    p_value = _one_sided_less_p(bootstrap, point, math.log(practical_ratio_max))
    return {
        "status": "evaluated",
        "comparison": "retrieved_memory_vs_no_memory",
        "source_field": source_field,
        "estimand": estimand,
        "all_executed_attempts": True,
        "repetition_aggregation": "arithmetic_mean_within_task_arm_before_contrast",
        "cluster_unit": "task",
        "n_task_clusters": len(task_ids),
        "paired_task_geometric_mean_ratio": math.exp(point),
        "one_sided_confidence": CONFIDENCE,
        "one_sided_percentile_upper_bound": math.exp(upper_log),
        "practical_ratio_max": practical_ratio_max,
        "bootstrap_p_value_one_sided_at_practical_floor": p_value,
        "clears_practical_floor": bool(
            math.exp(upper_log) < practical_ratio_max and p_value <= ALPHA
        ),
        "placebo_diagnostic": _placebo_ratio_diagnostic(means, task_ids, samples),
    }


def _equal_task_weighted_cost_ratio(
    means: dict[str, dict[str, float]],
    task_ids: Sequence[str],
    indices: Sequence[int],
    numerator_arm: str,
    *,
    allow_zero_denominator: bool = False,
) -> float | None:
    numerator = sum(means[task_ids[index]][numerator_arm] for index in indices)
    denominator = sum(means[task_ids[index]][BASELINE_ARM] for index in indices)
    if denominator <= 0.0:
        if allow_zero_denominator:
            return None
        raise AnalysisInputError(
            "normalized-cost full-sample no_memory denominator must be positive"
        )
    return numerator / denominator


def _cost_ratio_endpoint(
    means: dict[str, dict[str, float]],
    task_ids: Sequence[str],
    samples: Sequence[Sequence[int]],
    *,
    practical_ratio_max: float,
) -> dict[str, Any]:
    """Zero-safe ratio of equally weighted task-arm mean billed costs."""
    complete = tuple(range(len(task_ids)))
    point = _equal_task_weighted_cost_ratio(means, task_ids, complete, OPTIMIZED_ARM)
    assert point is not None
    raw_bootstrap = [
        _equal_task_weighted_cost_ratio(
            means,
            task_ids,
            draw,
            OPTIMIZED_ARM,
            allow_zero_denominator=True,
        )
        for draw in samples
    ]
    zero_denominator_draws = sum(value is None for value in raw_bootstrap)
    bootstrap = sorted(float(value) for value in raw_bootstrap if value is not None)
    if zero_denominator_draws:
        finite_upper = _quantile(bootstrap, 1.0 - ALPHA) if bootstrap else point
        upper = max(1.0, finite_upper)
        p_value = 1.0
    else:
        upper = _quantile(bootstrap, 1.0 - ALPHA)
        p_value = _one_sided_less_p(bootstrap, point, practical_ratio_max)
    placebo_point = _equal_task_weighted_cost_ratio(means, task_ids, complete, PLACEBO_ARM)
    assert placebo_point is not None
    raw_placebo_bootstrap = [
        _equal_task_weighted_cost_ratio(
            means,
            task_ids,
            draw,
            PLACEBO_ARM,
            allow_zero_denominator=True,
        )
        for draw in samples
    ]
    placebo_zero_denominator_draws = sum(value is None for value in raw_placebo_bootstrap)
    placebo_bootstrap = sorted(
        float(value) for value in raw_placebo_bootstrap if value is not None
    )
    placebo_lower = _quantile(placebo_bootstrap, ALPHA / 2) if placebo_bootstrap else placebo_point
    placebo_upper = (
        _quantile(placebo_bootstrap, 1 - ALPHA / 2)
        if placebo_bootstrap
        else placebo_point
    )
    if placebo_zero_denominator_draws:
        placebo_upper = max(1.0, placebo_upper)
    return {
        "status": "evaluated",
        "comparison": "retrieved_memory_vs_no_memory",
        "source_field": "derived_from_agent_info.usage.billing_v2_and_frozen_price_quote",
        "estimand": "ratio_of_equal_task_weighted_task_arm_mean_costs",
        "all_executed_attempts": True,
        "repetition_aggregation": "arithmetic_mean_within_task_arm_before_contrast",
        "task_aggregation": "equal_weight_arithmetic_mean_before_ratio",
        "cluster_unit": "task",
        "n_task_clusters": len(task_ids),
        "equal_task_weighted_mean_ratio": point,
        "one_sided_confidence": CONFIDENCE,
        "one_sided_percentile_upper_bound": upper,
        "bootstrap_zero_denominator_draws": zero_denominator_draws,
        "bootstrap_zero_denominator_policy": (
            "force_nonclear_upper_bound_at_least_one_and_p_value_one"
        ),
        "practical_ratio_max": practical_ratio_max,
        "bootstrap_p_value_one_sided_at_practical_floor": p_value,
        "clears_practical_floor": bool(upper < practical_ratio_max and p_value <= ALPHA),
        "placebo_diagnostic": {
            "comparison": "placebo_packet_vs_no_memory",
            "role": "diagnostic_not_in_joint_verdict",
            "n_task_clusters": len(task_ids),
            "equal_task_weighted_mean_ratio": placebo_point,
            "two_sided_percentile_ci": [
                placebo_lower,
                placebo_upper,
            ],
            "bootstrap_p_value_two_sided_ratio_one": (
                1.0
                if placebo_zero_denominator_draws
                else _two_sided_p(placebo_bootstrap, placebo_point, 1.0)
            ),
            "bootstrap_zero_denominator_draws": placebo_zero_denominator_draws,
        },
    }


def _quality_endpoint(
    means: dict[str, dict[str, float]],
    task_ids: Sequence[str],
    samples: Sequence[Sequence[int]],
    *,
    practical_difference_min: float,
) -> dict[str, Any]:
    differences = [means[task][OPTIMIZED_ARM] - means[task][BASELINE_ARM] for task in task_ids]
    point = sum(differences) / len(differences)
    bootstrap = sorted(_bootstrap_means(differences, samples))
    lower = _quantile(bootstrap, ALPHA)
    p_value = _one_sided_greater_p(bootstrap, point, practical_difference_min)
    placebo_values = [means[task][PLACEBO_ARM] - means[task][BASELINE_ARM] for task in task_ids]
    placebo_point = sum(placebo_values) / len(placebo_values)
    placebo_bootstrap = sorted(_bootstrap_means(placebo_values, samples))
    return {
        "status": "evaluated",
        "comparison": "retrieved_memory_vs_no_memory",
        "source_field": "code_quality.task_normalized_score_with_critical_failure_zero",
        "estimand": "mean_across_tasks(retrieved_task_mean_quality - no_memory_task_mean_quality)",
        "all_executed_attempts": True,
        "repetition_aggregation": "arithmetic_mean_within_task_arm_before_contrast",
        "cluster_unit": "task",
        "n_task_clusters": len(task_ids),
        "paired_task_mean_difference": point,
        "one_sided_confidence": CONFIDENCE,
        "one_sided_percentile_lower_bound": lower,
        "practical_difference_min": practical_difference_min,
        "bootstrap_p_value_one_sided_at_practical_floor": p_value,
        "clears_practical_floor": bool(lower > practical_difference_min and p_value <= ALPHA),
        "placebo_diagnostic": {
            "comparison": "placebo_packet_vs_no_memory",
            "role": "diagnostic_not_in_joint_verdict",
            "n_task_clusters": len(task_ids),
            "paired_task_mean_difference": placebo_point,
            "two_sided_percentile_ci": [
                _quantile(placebo_bootstrap, ALPHA / 2),
                _quantile(placebo_bootstrap, 1 - ALPHA / 2),
            ],
            "bootstrap_p_value_two_sided_difference_zero": _two_sided_p(
                placebo_bootstrap, placebo_point, 0.0
            ),
        },
    }


def analyze_records(
    records: Sequence[dict[str, Any]],
    *,
    expected_task_ids: Sequence[str],
    expected_runner_id: str,
    success_contract: dict[str, Any],
    price_quote: dict[str, Any],
    runner_identity: dict[str, Any],
    repetitions: int = DEFAULT_REPETITIONS,
    resamples: int = DEFAULT_RESAMPLES,
    seed: int = DEFAULT_SEED,
    expected_schedule_cells: Sequence[dict[str, Any]] | None = None,
    suite_integrity: dict[str, Any] | None = None,
) -> dict[str, Any]:
    """Analyze a complete balanced v2 record set without success-only filtering."""
    contract = _validate_success_contract(success_contract)
    quote = _validate_price_quote(price_quote)
    frozen_runner = _validate_frozen_runner_identity(runner_identity)
    if frozen_runner["runner_id"] != expected_runner_id:
        raise AnalysisInputError("expected runner ID differs from frozen runner identity")
    task_ids = sorted(expected_task_ids)
    cells, cell_integrity = _prepare_cells(
        records,
        expected_task_ids=task_ids,
        expected_runner_id=expected_runner_id,
        repetitions=repetitions,
        expected_schedule_cells=expected_schedule_cells,
        success_contract=contract,
        price_quote=quote,
        runner_identity=frozen_runner,
    )
    samples = _bootstrap_indices(len(task_ids), resamples, seed)
    floors = contract["endpoints"]
    elapsed = _ratio_endpoint(
        _task_arm_means(cells, task_ids, "elapsed_time"),
        task_ids,
        samples,
        practical_ratio_max=float(floors["elapsed_time"]["practical_ratio_max"]),
        source_field="timing.end_to_end_user_visible_wall_seconds",
        estimand=(
            "geometric_mean_across_tasks(retrieved_mean_end_to_end_elapsed_all_executed / "
            "no_memory_mean_end_to_end_elapsed_all_executed)"
        ),
    )
    cost = _cost_ratio_endpoint(
        _task_arm_means(cells, task_ids, "normalized_cost"),
        task_ids,
        samples,
        practical_ratio_max=float(floors["normalized_cost"]["practical_ratio_max"]),
    )
    quality = _quality_endpoint(
        _task_arm_means(cells, task_ids, "quality"),
        task_ids,
        samples,
        practical_difference_min=float(floors["code_quality"]["practical_difference_min"]),
    )
    endpoints = {"elapsed_time": elapsed, "normalized_cost": cost, "code_quality": quality}
    contract_frozen = bool(
        contract.get("status") == "frozen_approved"
        and all(item.get("floor_status") == "frozen_approved" for item in floors.values())
        and contract["timeout_policy"].get("status") == "frozen_approved"
    )
    all_clear = all(endpoint["clears_practical_floor"] for endpoint in endpoints.values())

    arm_summary: dict[str, dict[str, Any]] = {}
    for arm in PRIMARY_ARMS:
        arm_cells = [cell for cell in cells if cell["arm"] == arm]
        usage_totals = {
            category: sum(cell["exclusive_usage"][category] for cell in arm_cells)
            for category in PRICE_CATEGORIES
        }
        raw_token_values = [cell["raw_total_tokens"] for cell in arm_cells if cell["raw_total_tokens"] is not None]
        api_values = [cell["agent_api_seconds"] for cell in arm_cells if cell["agent_api_seconds"] is not None]
        harness_values = [cell["harness_agent_seconds"] for cell in arm_cells if cell["harness_agent_seconds"] is not None]
        arm_summary[arm] = {
            "executed_attempts": len(arm_cells),
            "validation_pass_rate": sum(cell["correct"] for cell in arm_cells) / len(arm_cells),
            "critical_quality_zeroes": sum(cell["quality_critical"] for cell in arm_cells),
            "mean_task_normalized_quality": sum(cell["quality"] for cell in arm_cells) / len(arm_cells),
            "mean_end_to_end_user_visible_wall_seconds": sum(cell["elapsed_time"] for cell in arm_cells) / len(arm_cells),
            "mean_normalized_cost_usd": sum(cell["normalized_cost"] for cell in arm_cells) / len(arm_cells),
            "timed_out_attempts": sum(cell["timed_out"] for cell in arm_cells),
            "exclusive_billed_tokens_total": usage_totals,
            "raw_total_token_measurements": len(raw_token_values),
            "raw_arithmetic_mean_total_tokens": (
                sum(raw_token_values) / len(raw_token_values) if raw_token_values else None
            ),
            "harness_agent_interval_measurements": len(harness_values),
            "raw_arithmetic_mean_harness_agent_interval_seconds": (
                sum(harness_values) / len(harness_values) if harness_values else None
            ),
            "agent_reported_api_measurements": len(api_values),
            "raw_arithmetic_mean_agent_reported_api_seconds": (
                sum(api_values) / len(api_values) if api_values else None
            ),
        }

    analyzer_path = pathlib.Path(__file__).resolve()
    identity_records = current_analyzer_records(analyzer_path.parent)
    identity_sha = analyzer_aggregate_sha256(identity_records)
    return {
        "schema": REPORT_SCHEMA,
        "status": "complete",
        "analyzer_entrypoint_sha256": hashlib.sha256(analyzer_path.read_bytes()).hexdigest(),
        "analyzer_identity": {
            "algorithm": ANALYZER_AGGREGATE_ALGORITHM,
            "current_sha256": identity_sha,
            "source_paths": list(ANALYZER_RUNTIME_SOURCE_PATHS),
            "matched": None,
            "note": "library analysis is not bound to a suite or frozen expected analyzer hash",
        },
        "integrity": {"passed": True, **(suite_integrity or {}), **cell_integrity},
        "design": {
            "task_clusters": len(task_ids),
            "runner_id": expected_runner_id,
            "primary_treatments": list(PRIMARY_ARMS),
            "primary_contrast": "retrieved_memory_vs_no_memory",
            "diagnostic_contrasts": ["placebo_packet_vs_no_memory"],
            "repetitions_per_treatment": repetitions,
            "requested_primary_cells": len(task_ids) * len(PRIMARY_ARMS) * repetitions,
            "attempt_policy": "all_executed_attempts_no_success_only_endpoints",
            "cluster_unit": "task_not_repetition",
        },
        "bootstrap": {
            "resamples": resamples,
            "seed": seed,
            "cluster_unit": "task",
            "index_generator": "sha256_counter_rejection_v1",
            "one_sided_confidence": CONFIDENCE,
            "finite_resample_p_value_correction": "(exceedances + 1) / (resamples + 1)",
        },
        "contracts": {
            "success_contract_sha256": contract["contract_sha256"],
            "success_contract_status": contract["status"],
            "price_quote_sha256": quote["quote_sha256"],
            "frozen_runner_identity_sha256": frozen_runner["identity_sha256"],
            "price_quote_currency": quote["currency"],
            "cost_categories": list(PRICE_CATEGORIES),
            "usage_semantics": quote["usage_semantics"],
            "attempt_aggregation": (
                "sum_per_isolated_provider_invocation_attempt_total_or_authenticated_structural_zero"
            ),
            "provider_retry_limit": contract["timeout_policy"]["provider_retry_limit"],
            "code_quality_schema": QUALITY_SCHEMA,
            "code_quality_rubric": contract["code_quality_measurement"]["rubric"],
            "timeout_policy": contract["timeout_policy"]["policy"],
        },
        "arm_summary": arm_summary,
        "primary_endpoints": endpoints,
        "joint_superiority": {
            "method": "intersection_union_all_three_component_nulls_must_be_rejected",
            "family_alpha": ALPHA,
            "multiplicity": (
                "no_across_endpoint_adjustment_required_for_intersection_union_test; "
                "each component uses one-sided alpha=0.05"
            ),
            "success_contract_frozen": contract_frozen,
            "all_practical_floors_cleared": all_clear,
            "passed": bool(contract_frozen and all_clear),
            "status": (
                "pass"
                if contract_frozen and all_clear
                else "fail_one_or_more_endpoints"
                if contract_frozen
                else "not_eligible_contract_not_frozen"
            ),
            "required_endpoints": ["elapsed_time", "normalized_cost", "code_quality"],
        },
        "diagnostics": {
            "validation_and_pass_rates_are_not_a_success_gate": True,
            "raw_usage_and_timing_retained": True,
            "successful_attempt_only_metrics_computed": False,
            "provider_api_time_is_diagnostic_only_and_never_substituted": True,
            "timed_out_elapsed_uses_measured_end_to_end_interval": True,
            "all_provider_retry_attempts_included_in_cost": True,
            "structural_zero_requires_authenticated_no_provider_invocation": True,
            "generic_total_tokens_used_for_cost_categories": False,
        },
    }


def _load_json(path: pathlib.Path) -> dict[str, Any]:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise AnalysisInputError(f"cannot read {path}: {exc}") from exc
    if not isinstance(value, dict):
        raise AnalysisInputError(f"{path} must contain a JSON object")
    return value


def _assert_execution_order(schedule: dict[str, Any], state: dict[str, Any]) -> None:
    cells = schedule.get("cells")
    if not isinstance(cells, list):
        raise AnalysisInputError("verified schedule has no cells array")
    planned = [cell.get("run_id") for cell in cells if isinstance(cell, dict)]
    if len(planned) != len(cells) or any(not isinstance(run_id, str) for run_id in planned):
        raise AnalysisInputError("verified schedule contains an invalid run ID")
    if state.get("schedule_sha256") != schedule.get("schedule_sha256"):
        raise AnalysisInputError("schedule-state hash does not match schedule")
    if state.get("planned_cell_count") != len(planned) or state.get("recorded_cell_count") != len(planned):
        raise AnalysisInputError("schedule-state cell counts are incomplete")
    if not isinstance(state.get("deviations"), list) or state["deviations"]:
        raise AnalysisInputError("execution schedule contains deviations")
    if state.get("actual_started_order") != planned or state.get("actual_finished_order") != planned:
        raise AnalysisInputError("actual execution order does not match the frozen schedule")


def analyze_verified_suite(
    suite_dir: pathlib.Path | str,
    *,
    expected_analyzer_sha256: str,
    success_contract: dict[str, Any],
    price_quote: dict[str, Any],
    runner_identity: dict[str, Any],
    expected_task_count: int = DEFAULT_TASKS,
    repetitions: int = DEFAULT_REPETITIONS,
    resamples: int = DEFAULT_RESAMPLES,
    seed: int = DEFAULT_SEED,
) -> dict[str, Any]:
    """Verify a suite and analyze it without loading holdout task metadata."""
    root = pathlib.Path(suite_dir).resolve()
    verification = verify_bundle(root)
    if not verification.get("ok"):
        raise AnalysisInputError(f"evidence verification failed: {verification.get('errors', [])[:5]}")
    manifest = _load_json(root / "evidence-manifest.json")
    if not isinstance(expected_analyzer_sha256, str) or not SHA256_RE.fullmatch(expected_analyzer_sha256):
        raise AnalysisInputError("a frozen lowercase analyzer SHA256 is required")
    analyzer = manifest.get("analyzer")
    if not isinstance(analyzer, dict) or analyzer.get("algorithm") != ANALYZER_AGGREGATE_ALGORITHM:
        raise AnalysisInputError("suite analyzer identity is missing or unsupported")
    suite_sha = analyzer.get("aggregate_sha256")
    current_sha = analyzer_aggregate_sha256(
        current_analyzer_records(pathlib.Path(__file__).resolve().parent)
    )
    if suite_sha != expected_analyzer_sha256 or current_sha != expected_analyzer_sha256:
        raise AnalysisInputError("suite/current analyzer does not match the frozen expected analyzer")
    harness = manifest.get("harness")
    if not isinstance(harness, dict) or harness.get("confirmatory_eligible") is not True:
        raise AnalysisInputError("suite harness is not confirmatory eligible")
    schedule = _load_json(root / "schedule.json")
    state = _load_json(root / "schedule-state.json")
    expected_schedule_hash = _stable_json_sha256(
        {key: value for key, value in schedule.items() if key != "schedule_sha256"}
    )
    if schedule.get("schedule_sha256") != expected_schedule_hash:
        raise AnalysisInputError("schedule self-hash is invalid")
    _assert_execution_order(schedule, state)
    frozen_runner = _validate_frozen_runner_identity(runner_identity)
    if frozen_runner["schedule_sha256"] != schedule.get("schedule_sha256"):
        raise AnalysisInputError("frozen runner schedule identity does not match schedule.json")
    cells = schedule["cells"]
    scheduled_tasks = sorted(
        {cell.get("task_id") for cell in cells if isinstance(cell, dict) and isinstance(cell.get("task_id"), str)}
    )
    runners = sorted(
        {
            runner.get("id")
            for cell in cells
            if isinstance(cell, dict)
            for runner in [cell.get("runner")]
            if isinstance(runner, dict) and isinstance(runner.get("id"), str)
        }
    )
    if isinstance(expected_task_count, bool) or not isinstance(expected_task_count, int) or expected_task_count < 1:
        raise AnalysisInputError("expected_task_count must be a positive integer")
    if len(scheduled_tasks) != expected_task_count:
        raise AnalysisInputError(f"expected {expected_task_count} scheduled tasks, found {len(scheduled_tasks)}")
    if len(runners) != 1:
        raise AnalysisInputError(f"confirmatory design requires exactly one runner, found {runners}")
    if schedule.get("repetitions") != repetitions:
        raise AnalysisInputError("schedule repetitions do not match the frozen design")
    requested = manifest.get("requested_cells")
    expected_count = len(scheduled_tasks) * len(PRIMARY_ARMS) * repetitions
    if not isinstance(requested, dict) or requested.get("count") != expected_count:
        raise AnalysisInputError("manifest requested-cell count does not match the design")
    requested_schedule = requested.get("schedule")
    if (
        not isinstance(requested_schedule, dict)
        or requested_schedule.get("schedule_sha256") != schedule.get("schedule_sha256")
        or requested_schedule.get("cell_count") != expected_count
    ):
        raise AnalysisInputError("manifest schedule identity does not match schedule.json")
    report = analyze_records(
        load_records(root),
        expected_task_ids=scheduled_tasks,
        expected_runner_id=runners[0],
        success_contract=success_contract,
        price_quote=price_quote,
        runner_identity=frozen_runner,
        repetitions=repetitions,
        resamples=resamples,
        seed=seed,
        expected_schedule_cells=cells,
        suite_integrity={
            "evidence_bundle_verified": True,
            "suite_id": manifest.get("suite_id"),
            "suite_identity_sha256": manifest.get("identity_sha256"),
            "schedule_sha256": schedule.get("schedule_sha256"),
            "schedule_deviations": 0,
            "holdout_metadata_loaded": False,
        },
    )
    report["analyzer_identity"] = {
        "algorithm": ANALYZER_AGGREGATE_ALGORITHM,
        "expected_sha256": expected_analyzer_sha256,
        "suite_sha256": suite_sha,
        "current_sha256": current_sha,
        "source_paths": list(ANALYZER_RUNTIME_SOURCE_PATHS),
        "matched": True,
    }
    return report


def load_analyzer_lock(path: pathlib.Path | str) -> str:
    lock = _load_json(pathlib.Path(path))
    if lock.get("schema_version") != 1 or lock.get("algorithm") != ANALYZER_AGGREGATE_ALGORITHM:
        raise AnalysisInputError("analyzer lock schema or algorithm is unsupported")
    files = lock.get("files")
    if not isinstance(files, list):
        raise AnalysisInputError("analyzer lock files must be an array")
    paths = [item.get("path") if isinstance(item, dict) else None for item in files]
    if paths != list(ANALYZER_RUNTIME_SOURCE_PATHS):
        raise AnalysisInputError("analyzer lock runtime source set changed or is out of order")
    records = [
        {"source_path": item.get("path"), "sha256": item.get("sha256")}
        for item in files
        if isinstance(item, dict)
    ]
    if len(records) != len(files) or any(
        not isinstance(item["sha256"], str) or not SHA256_RE.fullmatch(item["sha256"])
        for item in records
    ):
        raise AnalysisInputError("analyzer lock contains an invalid content hash")
    aggregate = analyzer_aggregate_sha256(records)
    if lock.get("aggregate_sha256") != aggregate:
        raise AnalysisInputError("analyzer lock aggregate hash mismatch")
    return aggregate


def _write_report(path: pathlib.Path | None, report: dict[str, Any]) -> None:
    rendered = json.dumps(report, indent=2, sort_keys=True) + "\n"
    if path is None:
        sys.stdout.write(rendered)
    else:
        path.write_text(rendered, encoding="utf-8")


def main(argv: Iterable[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Analyze a verified agent-brain v2 confirmatory suite")
    parser.add_argument("suite", type=pathlib.Path)
    parser.add_argument("--success-contract", type=pathlib.Path, required=True)
    parser.add_argument("--price-quote", type=pathlib.Path, required=True)
    parser.add_argument("--runner-identity", type=pathlib.Path, required=True)
    parser.add_argument("--output", type=pathlib.Path)
    parser.add_argument("--expected-tasks", type=int, default=DEFAULT_TASKS)
    parser.add_argument("--repetitions", type=int, default=DEFAULT_REPETITIONS)
    parser.add_argument("--resamples", type=int, default=DEFAULT_RESAMPLES)
    parser.add_argument("--seed", type=int, default=DEFAULT_SEED)
    group = parser.add_mutually_exclusive_group()
    group.add_argument("--analyzer-sha256")
    group.add_argument("--analyzer-lock", type=pathlib.Path)
    args = parser.parse_args(list(argv) if argv is not None else None)
    try:
        lock_path = args.analyzer_lock or pathlib.Path(__file__).resolve().parents[1] / "confirmatory" / "analyzer-lock.json"
        expected_sha = args.analyzer_sha256 or load_analyzer_lock(lock_path)
        report = analyze_verified_suite(
            args.suite,
            expected_analyzer_sha256=expected_sha,
            success_contract=_load_json(args.success_contract),
            price_quote=_load_json(args.price_quote),
            runner_identity=_load_json(args.runner_identity),
            expected_task_count=args.expected_tasks,
            repetitions=args.repetitions,
            resamples=args.resamples,
            seed=args.seed,
        )
        _write_report(args.output, report)
    except (AnalysisInputError, OSError, json.JSONDecodeError) as exc:
        parser.exit(2, f"confirmatory analysis refused: {exc}\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
