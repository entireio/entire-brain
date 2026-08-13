#!/usr/bin/env python3
"""Offline JSON Schema Draft 2020-12 validator for benchmark contracts.

The benchmark lane cannot download validators at verification time.  This
module implements every assertion/applicator keyword used by its checked-in
schemas and resolves both local and cross-file references from an explicit
registry.  Unsupported assertion keywords fail closed instead of being
silently ignored.
"""

from __future__ import annotations

import datetime as dt
import math
import re
from dataclasses import dataclass
from typing import Any, Mapping, Sequence, cast
from urllib.parse import urldefrag


DIALECT = "https://json-schema.org/draft/2020-12/schema"
ANNOTATIONS = {"$schema", "$id", "$defs", "title", "description"}
SUPPORTED = ANNOTATIONS | {
    "$ref",
    "type",
    "const",
    "enum",
    "required",
    "properties",
    "additionalProperties",
    "allOf",
    "anyOf",
    "oneOf",
    "not",
    "if",
    "then",
    "else",
    "items",
    "minItems",
    "maxItems",
    "uniqueItems",
    "minLength",
    "maxLength",
    "pattern",
    "format",
    "minimum",
    "maximum",
    "exclusiveMinimum",
    "exclusiveMaximum",
}
RFC3339 = re.compile(
    r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$"
)


class SchemaError(ValueError):
    """Raised when an instance or the schema vocabulary fails closed."""


@dataclass(frozen=True)
class SchemaDocument:
    name: str
    schema: Mapping[str, Any]


def _json_equal(left: Any, right: Any) -> bool:
    if left is None or right is None:
        return left is right
    if isinstance(left, bool) or isinstance(right, bool):
        return type(left) is type(right) and left == right
    if type(left) is not type(right):
        if type(left) in {int, float} and type(right) in {int, float}:
            return float(left) == float(right)
        return False
    if isinstance(left, dict):
        return set(left) == set(right) and all(
            _json_equal(left[key], right[key]) for key in left
        )
    if isinstance(left, list):
        return len(left) == len(right) and all(
            _json_equal(a, b) for a, b in zip(left, right)
        )
    return left == right


def _type_matches(instance: Any, expected: str) -> bool:
    return {
        "object": isinstance(instance, dict),
        "array": isinstance(instance, list),
        "string": isinstance(instance, str),
        "integer": type(instance) is int,
        "number": type(instance) in {int, float}
        and math.isfinite(float(instance)),
        "boolean": isinstance(instance, bool),
        "null": instance is None,
    }.get(expected, False)


def _pointer(document: Mapping[str, Any], fragment: str) -> Any:
    if fragment in {"", "/"}:
        return document
    if not fragment.startswith("/"):
        raise SchemaError(f"unsupported non-pointer schema fragment #{fragment}")
    current: Any = document
    for raw in fragment[1:].split("/"):
        token = raw.replace("~1", "/").replace("~0", "~")
        if not isinstance(current, dict) or token not in current:
            raise SchemaError(f"unresolved JSON pointer token {token!r}")
        current = current[token]
    return current


class Validator:
    """Validate instances against an explicit Draft 2020-12 schema registry."""

    def __init__(self, documents: Sequence[SchemaDocument]):
        self._documents: dict[str, SchemaDocument] = {}
        for document in documents:
            schema = document.schema
            if schema.get("$schema") != DIALECT:
                raise SchemaError(f"{document.name}: wrong JSON Schema dialect")
            self._register(document.name, document)
            self._register(document.name.rsplit("/", 1)[-1], document)
            identifier = schema.get("$id")
            if isinstance(identifier, str):
                self._register(identifier, document)
        # Audit the complete checked-in schema graph before validating an
        # instance.  Runtime-only traversal would otherwise leave dormant
        # $defs and unselected conditional branches unchecked.
        for document in documents:
            errors = self._audit_schema(document.schema, document, "$")
            if errors:
                raise SchemaError(f"{document.name}: " + "; ".join(errors[:20]))

    def _register(self, key: str, document: SchemaDocument) -> None:
        previous = self._documents.get(key)
        if previous is not None and previous is not document:
            raise SchemaError(f"duplicate schema registry key {key!r}")
        self._documents[key] = document

    def _audit_schema(
        self,
        rule: Any,
        document: SchemaDocument,
        path: str,
    ) -> list[str]:
        if isinstance(rule, bool):
            return []
        if not isinstance(rule, dict):
            return [f"{path}: schema node is not an object or boolean"]
        errors: list[str] = []
        unknown = set(rule) - SUPPORTED
        if unknown:
            errors.append(f"{path}: unsupported schema keywords {sorted(unknown)}")

        for keyword in ("$schema", "$id", "title", "description", "$ref"):
            if keyword in rule and not isinstance(rule[keyword], str):
                errors.append(f"{path}.{keyword}: must be a string")
        reference = rule.get("$ref")
        if isinstance(reference, str):
            try:
                self._resolve(reference, document)
            except SchemaError as exc:
                errors.append(f"{path}.$ref: {exc}")

        expected = rule.get("type")
        json_types = {
            "object",
            "array",
            "string",
            "integer",
            "number",
            "boolean",
            "null",
        }
        if expected is not None:
            choices = expected if isinstance(expected, list) else [expected]
            if (
                not choices
                or not all(isinstance(item, str) and item in json_types for item in choices)
                or len(choices) != len(set(choices))
            ):
                errors.append(f"{path}.type: invalid or duplicate JSON type")
        enum = rule.get("enum")
        if "enum" in rule and (not isinstance(enum, list) or not enum):
            errors.append(f"{path}.enum: must be a nonempty array")
        required = rule.get("required")
        if "required" in rule and (
            not isinstance(required, list)
            or not all(isinstance(item, str) for item in required)
            or len(required) != len(set(required))
        ):
            errors.append(f"{path}.required: must contain unique strings")

        definitions = rule.get("$defs", {})
        if "$defs" in rule and not isinstance(definitions, dict):
            errors.append(f"{path}.$defs: must be an object")
        elif isinstance(definitions, dict):
            for name, child in definitions.items():
                errors.extend(
                    self._audit_schema(child, document, f"{path}.$defs.{name}")
                )
        properties = rule.get("properties", {})
        if "properties" in rule and not isinstance(properties, dict):
            errors.append(f"{path}.properties: must be an object")
        elif isinstance(properties, dict):
            for name, child in properties.items():
                errors.extend(
                    self._audit_schema(child, document, f"{path}.properties.{name}")
                )
        additional = rule.get("additionalProperties")
        if "additionalProperties" in rule:
            if not isinstance(additional, (dict, bool)):
                errors.append(
                    f"{path}.additionalProperties: must be an object or boolean"
                )
            else:
                errors.extend(
                    self._audit_schema(
                        additional, document, f"{path}.additionalProperties"
                    )
                )

        for keyword in ("allOf", "anyOf", "oneOf"):
            if keyword not in rule:
                continue
            choices = rule[keyword]
            if not isinstance(choices, list) or not choices:
                errors.append(f"{path}.{keyword}: must be a nonempty array")
                continue
            for index, child in enumerate(choices):
                errors.extend(
                    self._audit_schema(
                        child, document, f"{path}.{keyword}[{index}]"
                    )
                )
        for keyword in ("not", "if", "then", "else", "items"):
            if keyword in rule:
                errors.extend(
                    self._audit_schema(rule[keyword], document, f"{path}.{keyword}")
                )

        for keyword in ("minItems", "maxItems", "minLength", "maxLength"):
            if keyword in rule and (
                type(rule[keyword]) is not int or rule[keyword] < 0
            ):
                errors.append(f"{path}.{keyword}: must be a nonnegative integer")
        if "uniqueItems" in rule and not isinstance(rule["uniqueItems"], bool):
            errors.append(f"{path}.uniqueItems: must be boolean")
        pattern = rule.get("pattern")
        if "pattern" in rule:
            if not isinstance(pattern, str):
                errors.append(f"{path}.pattern: must be a string")
            else:
                try:
                    re.compile(pattern)
                except re.error as exc:
                    errors.append(f"{path}.pattern: invalid regular expression: {exc}")
        if "format" in rule and rule["format"] != "date-time":
            errors.append(f"{path}.format: unsupported format assertion")
        for keyword in (
            "minimum",
            "maximum",
            "exclusiveMinimum",
            "exclusiveMaximum",
        ):
            if keyword in rule and not (
                type(rule[keyword]) in {int, float}
                and math.isfinite(float(rule[keyword]))
            ):
                errors.append(f"{path}.{keyword}: must be a finite number")
        return errors

    def validate(self, instance: Any, schema_name: str, *, label: str) -> None:
        document = self._documents.get(schema_name)
        if document is None:
            raise SchemaError(f"unregistered schema {schema_name!r}")
        errors = self._walk(instance, document.schema, document, "$")
        if errors:
            raise SchemaError(f"{label}: " + "; ".join(errors[:20]))

    def _resolve(
        self, reference: str, current: SchemaDocument
    ) -> tuple[Any, SchemaDocument]:
        target_name, fragment = urldefrag(reference)
        target = current if not target_name else self._documents.get(target_name)
        if target is None:
            target = self._documents.get(target_name.rsplit("/", 1)[-1])
        if target is None:
            raise SchemaError(f"unresolved schema reference {reference!r}")
        return _pointer(target.schema, fragment), target

    def _walk(
        self,
        instance: Any,
        rule: Any,
        document: SchemaDocument,
        path: str,
    ) -> list[str]:
        if rule is True:
            return []
        if rule is False:
            return [f"{path}: false schema rejects the instance"]
        if not isinstance(rule, dict):
            return [f"{path}: schema node is not an object or boolean"]
        unknown = set(rule) - SUPPORTED
        if unknown:
            return [f"{path}: unsupported schema keywords {sorted(unknown)}"]

        errors: list[str] = []
        reference = rule.get("$ref")
        if isinstance(reference, str):
            try:
                target, target_document = self._resolve(reference, document)
                errors.extend(self._walk(instance, target, target_document, path))
            except SchemaError as exc:
                errors.append(f"{path}: {exc}")

        expected = rule.get("type")
        if expected is not None:
            choices = expected if isinstance(expected, list) else [expected]
            if not choices or not all(isinstance(item, str) for item in choices):
                errors.append(f"{path}: schema type declaration is invalid")
            elif not any(_type_matches(instance, item) for item in choices):
                errors.append(f"{path}: wrong JSON type")
                return errors

        if "const" in rule and not _json_equal(instance, rule["const"]):
            errors.append(f"{path}: value differs from const")
        if "enum" in rule:
            enum = rule["enum"]
            if not isinstance(enum, list) or not any(
                _json_equal(instance, item) for item in enum
            ):
                errors.append(f"{path}: value is outside enum")

        for keyword in ("allOf", "anyOf", "oneOf"):
            if keyword not in rule:
                continue
            choices = rule[keyword]
            if not isinstance(choices, list) or not choices:
                errors.append(f"{path}: {keyword} must be a nonempty array")
                continue
            results = [self._walk(instance, item, document, path) for item in choices]
            matches = sum(not result for result in results)
            if keyword == "allOf":
                for result in results:
                    errors.extend(result)
            elif keyword == "anyOf" and matches == 0:
                errors.append(f"{path}: no anyOf branch matched")
            elif keyword == "oneOf" and matches != 1:
                errors.append(f"{path}: expected exactly one oneOf match, got {matches}")

        if "not" in rule and not self._walk(instance, rule["not"], document, path):
            errors.append(f"{path}: prohibited not-schema matched")
        if "if" in rule:
            condition_matches = not self._walk(instance, rule["if"], document, path)
            branch = "then" if condition_matches else "else"
            if branch in rule:
                errors.extend(self._walk(instance, rule[branch], document, path))

        if isinstance(instance, dict):
            required = rule.get("required", [])
            if isinstance(required, list):
                for field in required:
                    if field not in instance:
                        errors.append(f"{path}: missing required field {field!r}")
            properties = rule.get("properties", {})
            if not isinstance(properties, dict):
                errors.append(f"{path}: properties is not an object")
                properties = {}
            for field, child in properties.items():
                if field in instance:
                    errors.extend(
                        self._walk(instance[field], child, document, f"{path}.{field}")
                    )
            extras = set(instance) - set(properties)
            additional = rule.get("additionalProperties", True)
            if additional is False:
                errors.extend(
                    f"{path}: additional property {field!r} is prohibited"
                    for field in sorted(extras)
                )
            elif isinstance(additional, (dict, bool)):
                for field in sorted(extras):
                    errors.extend(
                        self._walk(
                            instance[field], additional, document, f"{path}.{field}"
                        )
                    )

        if isinstance(instance, list):
            minimum = rule.get("minItems")
            maximum = rule.get("maxItems")
            if isinstance(minimum, int) and len(instance) < minimum:
                errors.append(f"{path}: shorter than minItems")
            if isinstance(maximum, int) and len(instance) > maximum:
                errors.append(f"{path}: longer than maxItems")
            if rule.get("uniqueItems") is True:
                if any(
                    _json_equal(instance[left], instance[right])
                    for left in range(len(instance))
                    for right in range(left + 1, len(instance))
                ):
                    errors.append(f"{path}: uniqueItems violated")
            item_rule = rule.get("items")
            if item_rule is not None:
                for index, item in enumerate(instance):
                    errors.extend(
                        self._walk(item, item_rule, document, f"{path}[{index}]")
                    )

        if isinstance(instance, str):
            minimum = rule.get("minLength")
            maximum = rule.get("maxLength")
            if isinstance(minimum, int) and len(instance) < minimum:
                errors.append(f"{path}: shorter than minLength")
            if isinstance(maximum, int) and len(instance) > maximum:
                errors.append(f"{path}: longer than maxLength")
            pattern = rule.get("pattern")
            if isinstance(pattern, str) and re.search(pattern, instance) is None:
                errors.append(f"{path}: string does not match pattern")
            if rule.get("format") == "date-time":
                try:
                    if RFC3339.fullmatch(instance) is None:
                        raise ValueError
                    dt.datetime.fromisoformat(instance.replace("Z", "+00:00"))
                except ValueError:
                    errors.append(f"{path}: invalid RFC3339 date-time")

        if type(instance) in {int, float}:
            numeric_instance = cast(int | float, instance)
            if not math.isfinite(float(numeric_instance)):
                return errors
            comparisons = (
                ("minimum", lambda a, b: a >= b),
                ("maximum", lambda a, b: a <= b),
                ("exclusiveMinimum", lambda a, b: a > b),
                ("exclusiveMaximum", lambda a, b: a < b),
            )
            for keyword, predicate in comparisons:
                boundary = rule.get(keyword)
                if type(boundary) in {int, float} and not predicate(
                    numeric_instance, boundary
                ):
                    errors.append(f"{path}: violates {keyword}")
        return errors
