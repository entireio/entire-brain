"""Entire Brain — Python client.

A brain runs on your machine and, until now, could only be reached by a program
willing to speak JSON-RPC or shell out to the CLI and parse its output. This is
the small amount of code that makes it an ordinary HTTP service instead.

    from entire_brain import Brain

    brain = Brain("http://127.0.0.1:7777", token="…")
    for fact in brain.facts():
        print(fact["text"])

    for hit in brain.search("retry policy"):
        print(hit["source"], hit["text"])

Start the server with:

    entire brain mcp --http 7777

It binds loopback and prints a bearer token. Nothing here works without one —
that is deliberate, and explained in the server's own documentation.

Standard library only. A memory client that drags in a dependency tree is a
client people vendor rather than install.
"""

from __future__ import annotations

import json
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Iterable

__all__ = ["Brain", "BrainError"]


class BrainError(RuntimeError):
    """A request to a brain failed.

    Carries the status code and the server's own message, because "it broke" is
    not an error report. A 401 here almost always means the token is missing or
    stale, and saying so beats making the caller guess.
    """

    def __init__(self, status: int, message: str, hint: str = "") -> None:
        self.status = status
        self.message = message
        self.hint = hint
        detail = f"brain request failed ({status}): {message}"
        if hint:
            detail += f"\n  {hint}"
        if status == 401:
            detail += "\n  Check the bearer token printed by `entire brain mcp --http`."
        super().__init__(detail)


class Brain:
    """A client for one brain over HTTP."""

    def __init__(self, base_url: str, token: str, timeout: float = 30.0) -> None:
        if not token:
            # Failing here rather than at the first request: an empty token is a
            # configuration mistake, and a 401 three calls later is a worse way
            # to find out about it.
            raise ValueError("a bearer token is required; `entire brain mcp --http` prints one")
        self.base_url = base_url.rstrip("/")
        self.token = token
        self.timeout = timeout

    def _get(self, path: str, **params: Any) -> dict[str, Any]:
        query = {k: v for k, v in params.items() if v is not None}
        url = f"{self.base_url}{path}"
        if query:
            url += "?" + urllib.parse.urlencode(query)
        request = urllib.request.Request(url, headers={"Authorization": f"Bearer {self.token}"})
        try:
            with urllib.request.urlopen(request, timeout=self.timeout) as response:
                return json.loads(response.read().decode("utf-8"))
        except urllib.error.HTTPError as error:
            body = error.read().decode("utf-8", "replace")
            try:
                parsed = json.loads(body)
                raise BrainError(error.code, parsed.get("error", body), parsed.get("hint", "")) from None
            except json.JSONDecodeError:
                raise BrainError(error.code, body.strip() or error.reason) from None
        except urllib.error.URLError as error:
            raise BrainError(0, f"could not reach {self.base_url}: {error.reason}") from None

    def status(self, branch: str | None = None) -> dict[str, Any]:
        """Fact counts for a branch: active, superseded, retracted, total."""
        return self._get("/v1/status", branch=branch)

    def facts(
        self, limit: int = 20, branch: str | None = None, include_all: bool = False
    ) -> list[dict[str, Any]]:
        """Durable facts, most useful first.

        Superseded and retracted facts are excluded unless include_all is set:
        a caller that renders these somewhere should not be shown things the
        brain no longer believes.
        """
        payload = self._get(
            "/v1/facts", limit=limit, branch=branch, all="true" if include_all else None
        )
        return payload.get("facts", [])

    def search(
        self, query: str, limit: int = 20, source: str | None = None, branch: str | None = None
    ) -> list[dict[str, Any]]:
        """Hybrid retrieval across facts, history and docs.

        source narrows the corpus ("fact", "history", "doc"); omitting it
        searches the default set.
        """
        if not query.strip():
            raise ValueError("query must not be empty")
        payload = self._get("/v1/search", q=query, limit=limit, source=source, branch=branch)
        return payload.get("results", [])

    def endpoints(self) -> Iterable[dict[str, str]]:
        """What this server exposes. Useful when a client is newer than a brain."""
        return self._get("/v1/").get("endpoints", [])
