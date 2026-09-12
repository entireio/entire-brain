"""Agent-backend registry. One import point for the rest of BrainMark.

    from brainmark import agents
    adapter = agents.get_adapter("codex")          # or None -> backends.json default
    result  = adapter.run(prompt=..., worktree=..., model=..., timeout=...,
                          out_dir=..., env=..., patch_collector=...)

Backends are a REPORTED FACTOR, never pooled: two backends on the same pairs are
two cells, not one bigger n (devenv memory: "cross-cell comparison is
confounded"). `default_backend` in backends.json is codex per plan 0.5.
"""

from __future__ import annotations

import pathlib
from typing import Any

if __package__ in (None, ""):  # pragma: no cover
    import sys

    sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))
    from brainmark.agents.base import (  # type: ignore[no-redef]
        AgentAdapter, AgentBackendError, AgentResult, compute_usd, load_backends,
    )
    from brainmark.agents.claude_adapter import ClaudeAdapter  # type: ignore[no-redef]
    from brainmark.agents.codex_adapter import CodexAdapter  # type: ignore[no-redef]
else:
    from .base import AgentAdapter, AgentBackendError, AgentResult, compute_usd, load_backends
    from .claude_adapter import ClaudeAdapter
    from .codex_adapter import CodexAdapter

ADAPTERS: dict[str, type[AgentAdapter]] = {
    "codex": CodexAdapter,
    "claude": ClaudeAdapter,
}

__all__ = [
    "ADAPTERS", "AgentAdapter", "AgentBackendError", "AgentResult",
    "compute_usd", "get_adapter", "load_backends", "resolve_backend_name", "resolve_model",
]


def resolve_backend_name(name: str | None = None,
                         backends: dict[str, Any] | None = None) -> str:
    backends = backends if backends is not None else load_backends()
    chosen = name or backends.get("default_backend")
    if chosen not in ADAPTERS:
        raise AgentBackendError(
            f"unknown backend {chosen!r}; known: {sorted(ADAPTERS)}"
        )
    if chosen not in (backends.get("backends") or {}):
        raise AgentBackendError(f"backend {chosen!r} has no pin block in backends.json")
    return str(chosen)


def get_adapter(name: str | None = None, backends: dict[str, Any] | None = None,
                backends_path: str | pathlib.Path | None = None) -> AgentAdapter:
    backends = backends if backends is not None else load_backends(backends_path)
    chosen = resolve_backend_name(name, backends)
    spec = dict((backends.get("backends") or {})[chosen])
    spec["_backends_sha256"] = backends.get("_backends_sha256")
    prices = {k: v for k, v in (backends.get("prices") or {}).items()
              if isinstance(v, dict)}
    return ADAPTERS[chosen](spec, prices)


def resolve_model(backend: str, role: str = "primary",
                  backends: dict[str, Any] | None = None) -> str:
    """Model for a backend by ROLE, so tiers are named once, in backends.json.

    role: "primary" (the confirmatory cell) | "generality" (the second model
    that makes the generality claim) | "alternate".
    """
    backends = backends if backends is not None else load_backends()
    models = ((backends.get("backends") or {}).get(backend) or {}).get("models") or {}
    model = models.get(role)
    if not model:
        raise AgentBackendError(
            f"backend {backend!r} has no model for role {role!r}; have {sorted(models)}"
        )
    return str(model)
