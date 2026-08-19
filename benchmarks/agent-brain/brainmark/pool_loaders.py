#!/usr/bin/env python3
"""HF SWE-bench pool loaders -- download ONCE, then deterministic and OFFLINE.

Phase 0.1 of the NeurIPS upgrade needs a much larger instance pool than the 303
instances in `graphmark/agentic-swebench/tasks/*.json`. The pools live on the
Hugging Face Hub. This module is the ONLY place that talks to the Hub, and it
does so exactly once per pool:

    download  ->  <pools.cache_dir>/<pool>.json          normalized instances
                  <pools.cache_dir>/<pool>.meta.json     revision + sha256 + counts

Every later step -- mining, sealing, testing -- reads the cached JSON with the
standard library alone. `datasets` is IMPORT-GUARDED: it is needed to download
and for nothing else, so the test suite (and any offline re-mine) runs without
it installed.

DETERMINISM CONTRACT
  * Pools are always loaded at a PINNED REVISION. `registry[pool].revision` may
    be null in config.json, in which case the sha is resolved from the Hub API
    at download time and written into `<pool>.meta.json`; from there it flows
    into `candidates/INDEX.json`. A null pin therefore means "not yet frozen",
    never "whatever is current at mine time" -- mining never contacts the Hub.
  * The cached JSON is written with sorted keys and instances sorted by
    instance_id, so re-downloading the same revision is byte-identical.
  * Pool merge order is `(priority, pool_name)` from the registry, never CLI
    argument order, so `--pool a --pool b` and `--pool b --pool a` agree.

LICENSE NOTE (0.10 artifact audit)
  SWE-bench_Multimodal is present in the registry but `enabled: false`. Its HF
  card carries no license field (checked 2026-08-18: `cardData.license` is
  null on princeton-nlp/SWE-bench, _Verified and _Multimodal alike; only
  SWE-bench/SWE-bench_Multilingual declares `mit`). NeurIPS D&B desk-checks
  dataset licensing, so Multimodal stays out until the license audit clears it.
  Flip `pools.registry.swe_bench_multimodal.enabled` to true to include it.

Usage:
    python3 pool_loaders.py --status                    # what is cached, at what sha
    python3 pool_loaders.py --download                  # every enabled pool
    python3 pool_loaders.py --download --pool swe_bench_verified
    python3 pool_loaders.py --sizes                     # Hub byte sizes, no download
"""

from __future__ import annotations

import argparse
import json
import pathlib
import sys

if __package__ in (None, ""):
    sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent.parent))
    from brainmark import _harness  # type: ignore[no-redef]
else:
    from . import _harness

LOADER_VERSION = 1

# Fields carried from an HF row into the normalized instance. Order is fixed so
# the normalized record is stable; missing fields are simply absent.
_STR_FIELDS = (
    "repo",
    "instance_id",
    "base_commit",
    "patch",
    "test_patch",
    "problem_statement",
    "hints_text",
    "created_at",
    "version",
    "environment_setup_commit",
    "language",
    "image_assets",
)
# SWE-bench ships these as JSON-encoded strings; graphmark's task JSONs ship
# them already decoded. Normalize to lists so both look identical downstream.
_LIST_FIELDS = ("FAIL_TO_PASS", "PASS_TO_PASS")

_HF_API = "https://huggingface.co/api/datasets/"
_HF_SIZE_API = "https://datasets-server.huggingface.co/size?dataset="


class PoolError(RuntimeError):
    """A pool could not be loaded. Never silently degrades to an empty pool."""


# --------------------------------------------------------------------------
# registry
# --------------------------------------------------------------------------


def registry(config: dict) -> dict[str, dict]:
    pools = config.get("pools") or {}
    reg = pools.get("registry") or {}
    if not isinstance(reg, dict):
        raise PoolError("config.pools.registry must be an object")
    return reg


def cache_dir(config: dict) -> pathlib.Path:
    pools = config.get("pools") or {}
    raw = str(pools.get("cache_dir") or "pools")
    path = pathlib.Path(raw)
    return path if path.is_absolute() else _harness.BRAINMARK_DIR / path


def enabled_pools(config: dict) -> list[str]:
    """Enabled pool names in deterministic merge order."""
    reg = registry(config)
    names = [name for name, spec in reg.items() if spec.get("enabled")]
    return sort_pools(config, names)


def sort_pools(config: dict, names: list[str]) -> list[str]:
    """(priority, name) -- CLI order must never change a mining result."""
    reg = registry(config)
    unknown = sorted(n for n in names if n not in reg)
    if unknown:
        raise PoolError(
            f"unknown pool(s) {unknown}; known: {sorted(reg)}"
        )
    return sorted(set(names), key=lambda n: (int(reg[n].get("priority", 1000)), n))


def pool_paths(config: dict, name: str) -> tuple[pathlib.Path, pathlib.Path]:
    root = cache_dir(config)
    return root / f"{name}.json", root / f"{name}.meta.json"


# --------------------------------------------------------------------------
# normalization (pure, no network, no `datasets`)
# --------------------------------------------------------------------------


def normalize_row(row: dict, pool: str) -> dict | None:
    """One HF row -> one graphmark-shaped instance, or None if unusable.

    Unusable == no instance_id / repo / base_commit / patch. Such a row cannot
    be paired or graded, and dropping it here keeps the miner's gates honest
    (a missing patch would otherwise look like "no shared files").
    """
    out: dict = {}
    for key in _STR_FIELDS:
        if key not in row:
            continue
        value = row[key]
        if value is None:
            continue
        out[key] = value if isinstance(value, str) else json.dumps(
            value, ensure_ascii=False, sort_keys=True
        )
    for key in _LIST_FIELDS:
        if key not in row:
            continue
        out[key] = _as_list(row[key])
    for required in ("instance_id", "repo", "base_commit", "patch"):
        if not out.get(required):
            return None
    out["pool"] = pool
    return out


def _as_list(value) -> list[str]:
    if isinstance(value, list):
        return [str(v) for v in value]
    if isinstance(value, str):
        text = value.strip()
        if text.startswith("["):
            try:
                parsed = json.loads(text)
            except json.JSONDecodeError:
                return [value]
            if isinstance(parsed, list):
                return [str(v) for v in parsed]
        return [value] if text else []
    if value is None:
        return []
    return [str(value)]


def normalize_rows(rows, pool: str) -> list[dict]:
    """Deduplicate by instance_id (first wins) and sort. Order-independent."""
    seen: dict[str, dict] = {}
    for row in rows:
        inst = normalize_row(dict(row), pool)
        if inst is None:
            continue
        seen.setdefault(inst["instance_id"], inst)
    return [seen[iid] for iid in sorted(seen)]


def pool_document(pool: str, spec: dict, revision: str, instances: list[dict]) -> dict:
    return {
        "schema_version": LOADER_VERSION,
        "pool": pool,
        "dataset_id": str(spec["hf_id"]),
        "split": str(spec.get("split") or "test"),
        "revision": revision,
        "count": len(instances),
        "instances": instances,
    }


# --------------------------------------------------------------------------
# download (the ONLY networked path)
# --------------------------------------------------------------------------


def _datasets_module():
    """Import-guarded `datasets`. Absent => a loud, actionable error."""
    try:
        import datasets  # type: ignore
    except ImportError as exc:  # pragma: no cover - environment dependent
        raise PoolError(
            "the `datasets` package is required to DOWNLOAD a pool (it is not "
            "needed to read one). Install it into the interpreter you run this "
            "with:  python3 -m pip install 'datasets>=2.19'"
        ) from exc
    return datasets


def resolve_revision(hf_id: str, timeout: int = 30) -> str:
    """Current Hub commit sha for a dataset repo. Called only at download."""
    import urllib.request

    url = _HF_API + hf_id
    with urllib.request.urlopen(url, timeout=timeout) as resp:  # noqa: S310
        payload = json.loads(resp.read().decode("utf-8"))
    sha = payload.get("sha")
    if not isinstance(sha, str) or not sha:
        raise PoolError(f"Hub returned no revision sha for {hf_id}")
    return sha


def hub_size(hf_id: str, timeout: int = 30) -> dict:
    """Bytes + rows from the datasets-server, so a download can be budgeted."""
    import urllib.request

    try:
        with urllib.request.urlopen(_HF_SIZE_API + hf_id, timeout=timeout) as resp:  # noqa: S310
            payload = json.loads(resp.read().decode("utf-8"))
    except Exception as exc:  # noqa: BLE001 - reported, never fatal
        return {"dataset": hf_id, "error": str(exc)}
    size = (payload.get("size") or {}).get("dataset") or {}
    return {
        "dataset": hf_id,
        "num_rows": size.get("num_rows"),
        "num_bytes": size.get("num_bytes_original_files"),
        "error": None if size else json.dumps(payload)[:200],
    }


def download_pool(config: dict, name: str, force: bool = False) -> dict:
    """Fetch one pool at its pinned revision into the local cache.

    Idempotent: an existing cache at the same revision is left alone unless
    `force`. Returns the meta document.
    """
    spec = registry(config)[name]
    data_path, meta_path = pool_paths(config, name)
    pin = spec.get("revision")
    revision = str(pin) if pin else resolve_revision(str(spec["hf_id"]))

    if meta_path.is_file() and not force:
        meta = json.loads(meta_path.read_text(encoding="utf-8"))
        if meta.get("revision") == revision and data_path.is_file():
            meta["action"] = "cached"
            return meta

    datasets = _datasets_module()
    ds = datasets.load_dataset(
        str(spec["hf_id"]),
        split=str(spec.get("split") or "test"),
        revision=revision,
    )
    instances = normalize_rows((dict(r) for r in ds), name)
    doc = pool_document(name, spec, revision, instances)

    data_path.parent.mkdir(parents=True, exist_ok=True)
    data_path.write_text(_harness.pretty_json(doc), encoding="utf-8")
    meta = {
        "schema_version": LOADER_VERSION,
        "pool": name,
        "dataset_id": doc["dataset_id"],
        "split": doc["split"],
        "revision": revision,
        "revision_pinned_in_config": bool(pin),
        "count": len(instances),
        "rows_seen": ds.num_rows,
        "rows_dropped": int(ds.num_rows) - len(instances),
        "data_file": data_path.name,
        "data_sha256": _harness.sha256_file(data_path),
        "loader_sha256": _harness.sha256_file(pathlib.Path(__file__)),
    }
    meta_path.write_text(_harness.pretty_json(meta), encoding="utf-8")
    out = dict(meta)
    out["action"] = "downloaded"
    return out


# --------------------------------------------------------------------------
# offline read
# --------------------------------------------------------------------------


def load_pool(config: dict, name: str) -> tuple[dict[str, dict], dict]:
    """Read one cached pool. Standard library only; never contacts the Hub."""
    data_path, meta_path = pool_paths(config, name)
    if not data_path.is_file():
        raise PoolError(
            f"pool '{name}' is not cached at {data_path}. Download it once:\n"
            f"    python3 {pathlib.Path(__file__).name} --download --pool {name}"
        )
    doc = json.loads(data_path.read_text(encoding="utf-8"))
    instances = doc.get("instances")
    if not isinstance(instances, list):
        raise PoolError(f"pool '{name}' cache at {data_path} has no instance list")

    pool: dict[str, dict] = {}
    for inst in instances:
        iid = inst.get("instance_id")
        if isinstance(iid, str) and iid and iid not in pool:
            pool[iid] = inst

    meta = {}
    if meta_path.is_file():
        meta = json.loads(meta_path.read_text(encoding="utf-8"))
    provenance = {
        "kind": "hf_pool",
        "pool": name,
        "path": str(data_path),
        "sha256": _harness.sha256_file(data_path),
        "dataset_id": doc.get("dataset_id"),
        "revision": doc.get("revision") or meta.get("revision"),
        "split": doc.get("split"),
        "instances_in_file": len(instances),
    }
    return pool, provenance


def load_pools(config: dict, names: list[str]) -> tuple[dict[str, dict], list[dict]]:
    """Merge several cached pools in (priority, name) order; first wins.

    First-wins with a fixed order is what makes a merged pool deterministic:
    SWE-bench_Verified (priority 0) supersedes the same instance_id in the full
    SWE-bench dump, and the choice does not depend on how the flags were typed.
    """
    merged: dict[str, dict] = {}
    provenance: list[dict] = []
    for name in sort_pools(config, names):
        pool, prov = load_pool(config, name)
        added = 0
        for iid in sorted(pool):
            if iid not in merged:
                merged[iid] = pool[iid]
                added += 1
        prov = dict(prov)
        prov["instances_new"] = added
        provenance.append(prov)
    return merged, provenance


def pool_revisions(provenance: list[dict]) -> dict[str, dict]:
    """The `pool_revisions` block written into candidates/INDEX.json."""
    out: dict[str, dict] = {}
    for prov in provenance:
        if prov.get("kind") != "hf_pool":
            continue
        out[str(prov.get("pool"))] = {
            "dataset_id": prov.get("dataset_id"),
            "revision": prov.get("revision"),
            "split": prov.get("split"),
            "sha256": prov.get("sha256"),
            "instances_in_file": prov.get("instances_in_file"),
            "instances_new": prov.get("instances_new"),
        }
    return dict(sorted(out.items()))


# --------------------------------------------------------------------------
# CLI
# --------------------------------------------------------------------------


def _status(config: dict) -> str:
    reg = registry(config)
    lines = ["BrainMark HF pools", "=" * 68, f"cache: {cache_dir(config)}", ""]
    lines.append(f"{'pool':<26} {'on':>3} {'pri':>3} {'rows':>7}  revision")
    lines.append("-" * 78)
    for name in sorted(reg, key=lambda n: (int(reg[n].get("priority", 1000)), n)):
        spec = reg[name]
        data_path, meta_path = pool_paths(config, name)
        rows = "-"
        rev = str(spec.get("revision") or "(unpinned)")
        if meta_path.is_file():
            meta = json.loads(meta_path.read_text(encoding="utf-8"))
            rows = str(meta.get("count"))
            rev = str(meta.get("revision"))
        elif not data_path.is_file():
            rev += "  NOT CACHED"
        lines.append(
            f"{name:<26} {'yes' if spec.get('enabled') else ' NO':>3} "
            f"{int(spec.get('priority', 1000)):>3} {rows:>7}  {rev}"
        )
        reason = spec.get("_disabled_reason")
        if reason and not spec.get("enabled"):
            lines.append(f"{'':<26} reason: {reason}")
    return "\n".join(lines)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="BrainMark HF pool loader.")
    parser.add_argument("--config", default=None)
    parser.add_argument("--pool", action="append", default=None,
                        help="pool name (repeatable); default = every enabled pool")
    parser.add_argument("--download", action="store_true")
    parser.add_argument("--force", action="store_true", help="re-download even if cached")
    parser.add_argument("--sizes", action="store_true", help="Hub byte sizes, no download")
    parser.add_argument("--status", action="store_true")
    args = parser.parse_args(argv)

    config = _harness.load_config(args.config)
    names = sort_pools(config, args.pool) if args.pool else enabled_pools(config)

    if args.sizes:
        reg = registry(config)
        total = 0
        for name in sort_pools(config, list(reg)):
            info = hub_size(str(reg[name]["hf_id"]))
            nbytes = info.get("num_bytes") or 0
            total += int(nbytes)
            print(f"{name:<26} {str(info.get('num_rows')):>7} rows  "
                  f"{nbytes / 1e6:>9.1f} MB  {info.get('error') or ''}")
        print(f"{'TOTAL':<26} {'':>7}       {total / 1e6:>9.1f} MB")
        return 0

    if args.download:
        for name in names:
            meta = download_pool(config, name, force=args.force)
            print(f"{name:<26} {meta['action']:<11} rev={meta['revision']} "
                  f"n={meta['count']}")
        return 0

    print(_status(config))
    if not args.status:
        print("\n(use --download to fetch, --sizes to budget, --status to silence this)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
