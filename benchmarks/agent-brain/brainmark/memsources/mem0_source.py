"""`mem0` arm -- the named competitor, over the same session-A bytes.

Written against the mem0 OSS pip API (`from mem0 import Memory`), NOT the
managed platform: the managed service is a moving target that cannot be pinned,
and a benchmark that cannot be re-run is not evidence.

IMPORT-GUARDED. `mem0ai` is not a dependency of this repo and the offline test
suite must pass without it. The import happens inside `_make_client()`, so this
module always imports; only actually RUNNING the arm requires the package. A
missing package raises MemorySourceError at prep -- it never degrades to an
empty packet (that would score an infrastructure failure as "memory did not
help", which is the easiest way to fake this benchmark's headline).

mem0 is synchronous, so it is wrapped to satisfy the async duck-typed surface in
_competitor.py. The wrapper is the ONLY mem0-specific code here; retrieval,
budget, envelope, and pinning are the shared path every arm uses.

PROVIDER CONFIG BEYOND A MODEL NAME. A pin may carry `config_extra`, a dict
passed straight through into mem0's own provider config block, plus an optional
`vector_store` block. This exists because mem0's published defaults are not
always DEPLOYABLE: on an Azure AI Foundry resource `openai/gpt-4o-mini` returns
DeploymentNotFound and no embedding deployment exists at all, so the LLM needs
an `openai_base_url` and the embedder must be a local sentence-transformers
model with matching dims. The mechanism only ever gives mem0 MORE
configurability, never less, and it changes nothing for any other arm. Every
such deviation is FORCED, is recorded in COMPETITORS.md with its probe date,
and is echoed into the packet provenance -- see `_observed` below.

`${VAR}` inside a config_extra string is expanded from the environment, so a
tenant endpoint is named by reference and never committed. An unset variable is
a hard error: a mem0 arm that silently fell back to public OpenAI would be a
different service than the one the report names.
"""

from __future__ import annotations

import asyncio
import os
import re
from typing import Any

#: `${NAME}` -- deliberately NOT `$NAME`, so a literal `$` in a model id or a
#: password-like pin cannot be eaten by accident.
_ENV_REF = re.compile(r"\$\{([A-Za-z_][A-Za-z0-9_]*)\}")

from ._competitor import DEFAULT_RESULT_SOURCE, run_competitor, transcript_to_messages
from .base import MemoryPacket, MemorySourceError

ARM = "mem0"


def expand_env(value: Any, where: str) -> Any:
    """Expand `${VAR}` in strings, recursively through dicts and lists.

    Raises rather than substituting an empty string: a half-resolved endpoint
    would send mem0 somewhere other than the pinned deployment, which is an
    infrastructure failure scored as "memory did not help".
    """
    if isinstance(value, dict):
        return {k: expand_env(v, f"{where}.{k}") for k, v in value.items()}
    if isinstance(value, list):
        return [expand_env(v, f"{where}[{i}]") for i, v in enumerate(value)]
    if not isinstance(value, str):
        return value

    def sub(match: "re.Match[str]") -> str:
        name = match.group(1)
        resolved = os.environ.get(name)
        if not resolved:
            raise MemorySourceError(
                f"mem0 pin {where} references ${{{name}}} but {name} is unset or "
                "empty. Refusing to run the arm against an unpinned endpoint."
            )
        return resolved.rstrip("/") if where.endswith("url") else resolved

    return _ENV_REF.sub(sub, value)


def provider_block(pin: dict[str, Any], where: str) -> dict[str, Any]:
    """One mem0 provider block: `{"provider": ..., "config": {...}}`.

    `config_extra` is merged INTO the config dict beside the model name, which
    is how mem0 itself takes an endpoint (`openai_base_url`) or an embedding
    width (`embedding_dims`).
    """
    inner: dict[str, Any] = {"model": pin.get("model")}
    inner.update(expand_env(pin.get("config_extra") or {}, f"{where}.config_extra"))
    return {"provider": pin.get("provider", "openai"), "config": inner}


class Mem0Client:
    """Async adapter over mem0 OSS `Memory`, matching the harness client surface."""

    def __init__(self, pins: dict[str, Any] | None = None) -> None:
        self._pins = pins or {}
        self._memory: Any = None

    # -- context manager -------------------------------------------------
    async def __aenter__(self) -> "Mem0Client":
        self._memory = await asyncio.to_thread(self._make_client)
        return self

    async def __aexit__(self, *exc: Any) -> None:
        return None

    async def close(self) -> None:
        return None

    def _make_client(self) -> Any:
        try:
            from mem0 import Memory  # type: ignore[import-not-found]
        except ImportError as exc:
            raise MemorySourceError(
                "mem0 arm requires the `mem0ai` package "
                f"(pin {self._pins.get('version_pin', 'unpinned')}): pip install "
                "mem0ai==<pin>. Refusing to run the arm degraded."
            ) from exc

        config = self.mem0_config()
        return Memory.from_config(config) if config else Memory()

    def mem0_config(self) -> dict[str, Any]:
        """The `Memory.from_config` payload built from the pins. Pure; testable."""
        llm = self._pins.get("llm") or {}
        embedder = self._pins.get("embedder") or {}
        vector_store = self._pins.get("vector_store") or {}
        config: dict[str, Any] = {}
        if llm:
            config["llm"] = provider_block(llm, "llm")
        if embedder:
            config["embedder"] = provider_block(embedder, "embedder")
        if vector_store:
            config["vector_store"] = expand_env(vector_store, "vector_store")
        return config

    # -- duck-typed surface ----------------------------------------------
    async def add(
        self,
        messages: list[dict[str, str]],
        user_id: str,
        observation_date: str | None = None,
        timestamp: int | None = None,
        custom_instructions: str | None = None,
        metadata: dict | None = None,
    ) -> dict | None:
        details = dict(metadata or {})
        if observation_date is not None:
            details["observation_date"] = observation_date
        if timestamp is not None:
            details["timestamp"] = timestamp
        kwargs = {"user_id": user_id, "metadata": details}
        if custom_instructions is not None:
            import inspect
            if "custom_instructions" not in inspect.signature(self._memory.add).parameters:
                raise MemorySourceError("installed mem0 add API does not support custom_instructions")
            kwargs["custom_instructions"] = custom_instructions
        return await asyncio.to_thread(self._memory.add, messages, **kwargs)

    async def search(
        self,
        query: str,
        user_id: str,
        top_k: int = 200,
        rerank: bool = False,
        score_debug: bool = False,
    ) -> list[dict]:
        raw = await asyncio.to_thread(
            self._memory.search, query, user_id=user_id, limit=int(top_k)
        )
        # mem0 >=0.1.x returns {"results": [...]}; older builds returned a list.
        hits = raw.get("results", []) if isinstance(raw, dict) else (raw or [])
        out: list[dict] = []
        for hit in hits:
            if not isinstance(hit, dict):
                continue
            text = str(hit.get("memory") or hit.get("text") or "").strip()
            if not text:
                continue
            out.append({
                "memory": text,
                "score": float(hit.get("score") or 0.0),
                "id": str(hit.get("id") or ""),
                "created_at": hit.get("created_at"),
                # NOT `ARM`: the delivered packet must not name the condition.
                "source": DEFAULT_RESULT_SOURCE,
            })
        return out

    async def delete_user(self, user_id: str) -> bool:
        await asyncio.to_thread(self._memory.delete_all, user_id=user_id)
        return True

    async def get_user_profile(self, user_id: str) -> dict | None:
        return None


def observed_provenance(pins: dict[str, Any], top_k: int) -> dict[str, Any]:
    """What ACTUALLY ran: installed mem0 version + the resolved config.

    Import-guarded like the client: `"MISSING"` when mem0ai is absent, so this
    never turns an offline test run into an error. A version that disagrees
    with `version_pin` is reported, not corrected -- reconciling it is the
    reviewer's job and the report must be able to show the mismatch.
    """
    try:
        import mem0 as _mem0  # type: ignore[import-not-found]

        version = str(getattr(_mem0, "__version__", "unknown"))
    except ImportError:
        version = "MISSING"
    return {
        "mem0_version": version,
        "mem0_version_pin": pins.get("version_pin"),
        "mem0_version_matches_pin": version == str(pins.get("version_pin") or ""),
        "llm": pins.get("llm"),
        "embedder": pins.get("embedder"),
        "vector_store": pins.get("vector_store"),
        "forced_deviations": sorted(
            k for k in pins if k.endswith("_forced_deviation")
        ),
        "top_k": top_k,
    }


def build(
    query: str,
    max_bytes: int,
    top_k: int,
    transcript_bytes: bytes,
    user_id: str,
    pins: dict[str, Any] | None = None,
    observation_date: str | None = None,
    **_ignored,
) -> MemoryPacket:
    pins = pins or {}
    # OBSERVED, not declared. COMPETITORS.md requires every mem0 packet's
    # provenance to prove what actually ran -- the installed library version
    # beside the pinned one, and the resolved LLM/embedder/store -- so a report
    # states what ran rather than what was configured.
    pins = {**pins, "_observed": observed_provenance(pins, top_k)}
    return run_competitor(
        arm=ARM,
        client_factory=lambda: Mem0Client(pins),
        query=query,
        messages=transcript_to_messages(transcript_bytes),
        user_id=user_id,
        max_bytes=max_bytes,
        top_k=top_k,
        observation_date=observation_date,
        pins=pins,
    )
