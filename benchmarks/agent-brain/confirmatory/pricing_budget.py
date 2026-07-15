#!/usr/bin/env python3
"""Deterministic arithmetic for the confirmatory pricing and budget contract."""

from __future__ import annotations

import argparse
from decimal import Decimal, InvalidOperation, ROUND_CEILING, localcontext
import json
import pathlib
import re
from typing import Any


FORMULA_ID = "worst_case_token_envelope_v1"
TOKEN_KEYS = ("uncached_input", "cached_input", "output")
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


def effective_tokens_per_call(contract: dict[str, Any]) -> dict[str, int]:
    """Resolve the frozen per-call token envelope from either supported assumption policy."""
    assumptions = contract.get("token_assumptions", {})
    mode = assumptions.get("mode")
    if mode == "explicit_per_call_caps":
        caps = assumptions.get("explicit_per_call_caps", {})
        return {
            key: _token_count(caps.get(key), f"explicit_per_call_caps.{key}")
            for key in TOKEN_KEYS
        }
    if mode == "empirical_bound":
        empirical = assumptions.get("empirical_bound", {})
        observed = empirical.get("observed_tokens_per_call", {})
        multiplier = parse_decimal(
            empirical.get("safety_multiplier"),
            "empirical_bound.safety_multiplier",
            minimum=Decimal("1"),
        )
        result: dict[str, int] = {}
        for key in TOKEN_KEYS:
            observed_count = _token_count(
                observed.get(key), f"observed_tokens_per_call.{key}"
            )
            result[key] = int(
                (Decimal(observed_count) * multiplier).to_integral_value(
                    rounding=ROUND_CEILING
                )
            )
        return result
    raise ValueError("token_assumptions.mode must select an explicit or empirical envelope")


def calculate(contract: dict[str, Any]) -> dict[str, Any]:
    """Calculate the exact worst-case cost for all requested and reserve calls."""
    quote = contract.get("pricing_quote", {})
    unit = quote.get("tokens_per_price_unit")
    if not isinstance(unit, int) or isinstance(unit, bool) or unit <= 0:
        raise ValueError("pricing_quote.tokens_per_price_unit must be a positive integer")
    prices = quote.get("prices_usd_per_unit", {})
    parsed_prices = {
        key: parse_decimal(prices.get(key), f"prices_usd_per_unit.{key}")
        for key in TOKEN_KEYS
    }
    tokens = effective_tokens_per_call(contract)
    maximum_calls = contract.get("design", {}).get("maximum_calls_with_reserve")
    if not isinstance(maximum_calls, int) or isinstance(maximum_calls, bool) or maximum_calls <= 0:
        raise ValueError("design.maximum_calls_with_reserve must be a positive integer")

    required_precision = max(
        50,
        max(len(price.as_tuple().digits) for price in parsed_prices.values())
        + max(len(str(value)) for value in tokens.values())
        + len(str(maximum_calls))
        + 12,
    )
    with localcontext() as context:
        context.prec = required_precision
        denominator = Decimal(unit)
        line_items = {
            key: Decimal(tokens[key]) * parsed_prices[key] / denominator
            for key in TOKEN_KEYS
        }
        per_call = sum(line_items.values(), Decimal("0"))
        maximum = per_call * Decimal(maximum_calls)
    return {
        "formula_id": FORMULA_ID,
        "tokens_per_price_unit": unit,
        "effective_tokens_per_call": tokens,
        "per_call_usd_by_category": {
            key: decimal_string(line_items[key]) for key in TOKEN_KEYS
        },
        "per_call_maximum_usd": decimal_string(per_call),
        "maximum_calls": maximum_calls,
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
