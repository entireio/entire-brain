#!/usr/bin/env python3
"""Deterministic arithmetic for the confirmatory pricing and budget contract."""

from __future__ import annotations

import argparse
from decimal import Decimal, InvalidOperation, ROUND_CEILING, localcontext
import json
import pathlib
import re
from typing import Any


FORMULA_ID = "worst_case_agent_invocation_token_envelope_v4"
TOKEN_KEYS = (
    "uncached_input",
    "cache_read_input",
    "cache_write_input",
    "visible_output",
    "reasoning_output",
)
DECIMAL_RE = re.compile(r"^(0|[1-9][0-9]*)(\.[0-9]+)?$")


def parse_decimal(value: Any, label: str, *, minimum: Decimal = Decimal("0")) -> Decimal:
    """Parse an exact, non-exponent decimal string without accepting JSON floats."""
    if not isinstance(value, str) or DECIMAL_RE.fullmatch(value) is None:
        raise ValueError(f"{label} must be a non-exponent decimal string")
    try:
        parsed = Decimal(value)
    except InvalidOperation as exc:  # pragma: no cover - guarded by the regular expression
        raise ValueError(f"{label} is not a valid decimal string") from exc
    if not parsed.is_finite() or parsed < minimum:
        raise ValueError(f"{label} must be at least {minimum}")
    return parsed


def decimal_string(value: Decimal) -> str:
    """Return a canonical, non-exponent decimal representation."""
    rendered = format(value, "f")
    if "." in rendered:
        rendered = rendered.rstrip("0").rstrip(".")
    return rendered or "0"


def _token_count(value: Any, label: str) -> int:
    if not isinstance(value, int) or isinstance(value, bool) or value < 0:
        raise ValueError(f"{label} must be a nonnegative integer")
    return value


def effective_tokens_per_agent_invocation(contract: dict[str, Any]) -> dict[str, int]:
    """Resolve the frozen per-agent-invocation token envelope."""
    assumptions = contract.get("token_assumptions", {})
    mode = assumptions.get("mode")
    if mode == "explicit_per_agent_invocation_caps":
        caps = assumptions.get("explicit_per_agent_invocation_caps", {})
        return {
            key: _token_count(
                caps.get(key), f"explicit_per_agent_invocation_caps.{key}"
            )
            for key in TOKEN_KEYS
        }
    if mode == "empirical_bound":
        empirical = assumptions.get("empirical_bound", {})
        observed = empirical.get("observed_tokens_per_agent_invocation", {})
        multiplier = parse_decimal(
            empirical.get("safety_multiplier"),
            "empirical_bound.safety_multiplier",
            minimum=Decimal("1"),
        )
        result: dict[str, int] = {}
        for key in TOKEN_KEYS:
            observed_count = _token_count(
                observed.get(key), f"observed_tokens_per_agent_invocation.{key}"
            )
            result[key] = int(
                (Decimal(observed_count) * multiplier).to_integral_value(
                    rounding=ROUND_CEILING
                )
            )
        return result
    raise ValueError(
        "token_assumptions.mode must select an explicit per-agent-invocation or empirical envelope"
    )


def maximum_agent_invocations(contract: dict[str, Any]) -> int:
    """Validate the zero-retry/replacement design and return its hard ceiling."""
    design = contract.get("design")
    if not isinstance(design, dict):
        raise ValueError("design must be an object")
    requested = design.get("requested_cells")
    if not isinstance(requested, int) or isinstance(requested, bool) or requested <= 0:
        raise ValueError("design.requested_cells must be a positive integer")
    for field in (
        "retry_agent_invocations",
        "replacement_cell_attempts",
        "reserve_cell_attempts",
    ):
        if design.get(field) != 0 or isinstance(design.get(field), bool):
            raise ValueError(f"design.{field} must be exactly zero")
    maximum = design.get("maximum_agent_invocations")
    if maximum != requested or isinstance(maximum, bool):
        raise ValueError(
            "design.maximum_agent_invocations must equal requested_cells"
        )
    return requested


def calculate(contract: dict[str, Any]) -> dict[str, Any]:
    """Calculate worst-case cost for the zero-retry agent-invocation ceiling."""
    quote = contract.get("pricing_quote", {})
    unit = quote.get("tokens_per_price_unit")
    if not isinstance(unit, int) or isinstance(unit, bool) or unit <= 0:
        raise ValueError("pricing_quote.tokens_per_price_unit must be a positive integer")
    prices = quote.get("prices_usd_per_unit", {})
    aliases = quote.get("price_aliases", {})
    parsed_prices: dict[str, Decimal] = {}
    for key in TOKEN_KEYS:
        direct = prices.get(key)
        alias = aliases.get(key) if isinstance(aliases, dict) else None
        if (direct is None) == (alias is None):
            raise ValueError(f"{key} must set exactly one direct price or alias")
        source = key if direct is not None else alias
        if source not in TOKEN_KEYS or prices.get(source) is None:
            raise ValueError(f"{key} alias must target a direct category price")
        parsed_prices[key] = parse_decimal(
            prices[source], f"prices_usd_per_unit.{source}"
        )
    tokens = effective_tokens_per_agent_invocation(contract)
    maximum_invocations = maximum_agent_invocations(contract)

    required_precision = max(
        50,
        max(len(price.as_tuple().digits) for price in parsed_prices.values())
        + max(len(str(value)) for value in tokens.values())
        + len(str(maximum_invocations))
        + 12,
    )
    with localcontext() as context:
        context.prec = required_precision
        denominator = Decimal(unit)
        line_items = {
            key: Decimal(tokens[key]) * parsed_prices[key] / denominator
            for key in TOKEN_KEYS
        }
        per_agent_invocation = sum(line_items.values(), Decimal("0"))
        maximum = per_agent_invocation * Decimal(maximum_invocations)
    return {
        "formula_id": FORMULA_ID,
        "tokens_per_price_unit": unit,
        "effective_tokens_per_agent_invocation": tokens,
        "per_agent_invocation_usd_by_category": {
            key: decimal_string(line_items[key]) for key in TOKEN_KEYS
        },
        "per_agent_invocation_maximum_usd": decimal_string(per_agent_invocation),
        "maximum_agent_invocations": maximum_invocations,
        "maximum_usd": decimal_string(maximum),
    }


def main() -> int:
    parser = argparse.ArgumentParser(description="Print the derived pricing-budget calculation")
    parser.add_argument("contract", type=pathlib.Path, help="path to pricing-budget.json")
    args = parser.parse_args()
    try:
        contract = json.loads(args.contract.read_text(encoding="utf-8"))
        result = calculate(contract)
    except (OSError, UnicodeDecodeError, json.JSONDecodeError, ValueError) as exc:
        parser.error(str(exc))
    print(json.dumps(result, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
