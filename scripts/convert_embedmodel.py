#!/usr/bin/env python3
"""Convert a Model2Vec static embedding model into the bundled `embedmodel.bin`.

This is a DEV-TIME tool, run manually (it needs network to fetch the model from
the HuggingFace Hub). Its output — `assets/embedmodel.bin` and the Go parity
golden — is committed, so the brain itself never touches the network: it embeds
the converted asset and does inference in pure Go (see internal/cli/embed.go).

Why Model2Vec / "potion": the model is a static token->vector table (PCA +
zipf-weighted distillation of a sentence-transformer). Inference is just
tokenize -> gather -> mean-pool -> L2-normalize, which is trivial and fast to
reimplement in pure Go with no cgo, no ONNX runtime, and no GPU — the only
backend that fits the brain's pure-Go, offline, single-static-binary shape.

Format (little-endian) of embedmodel.bin:
    magic    "EBM1"        4 bytes
    version  uint32        =1
    nameLen  uint16, name  utf8 model id (cache-invalidation key)
    dim      uint32
    vocab    uint32
    dtype    uint32        1 = int8 (per-column symmetric quantization)
    flags    uint32        bit0 lowercase, bit1 strip_accents, bit2 chinese
    unkID    uint32
    contLen  uint16, cont  utf8 WordPiece continuation prefix ("##")
    tokens   vocab x (uint16 len + utf8 bytes), in id order
    scales   dim x float32 (per-column dequant scale)
    matrix   vocab*dim int8, row-major
"""
import argparse
import json
import struct
import sys
from pathlib import Path

import numpy as np


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--model", default="minishlab/potion-base-8M")
    ap.add_argument("--out", default="assets/embedmodel.bin")
    ap.add_argument("--golden", default="internal/cli/testdata/embed_golden.json")
    args = ap.parse_args()

    from model2vec import StaticModel

    m = StaticModel.from_pretrained(args.model)
    E = np.asarray(m.embedding, dtype=np.float32)
    vocab_size, dim = E.shape
    spec = json.loads(m.tokenizer.to_str())
    model_spec = spec["model"]
    norm = spec.get("normalizer") or {}
    if model_spec.get("type") != "WordPiece":
        sys.exit(f"unsupported tokenizer type {model_spec.get('type')!r}; expected WordPiece")

    vocab = model_spec["vocab"]  # token -> id
    id_to_tok = [""] * vocab_size
    for tok, idx in vocab.items():
        if 0 <= idx < vocab_size:
            id_to_tok[idx] = tok
    unk = model_spec.get("unk_token", "[UNK]")
    unk_id = vocab.get(unk, 0)
    cont = model_spec.get("continuing_subword_prefix", "##")

    lowercase = bool(norm.get("lowercase", True))
    # BertNormalizer: strip_accents=null follows lowercase (true => strip).
    strip_accents = norm.get("strip_accents")
    if strip_accents is None:
        strip_accents = lowercase
    chinese = bool(norm.get("handle_chinese_chars", True))
    flags = (1 if lowercase else 0) | (2 if strip_accents else 0) | (4 if chinese else 0)

    # Per-column symmetric int8 quantization. PCA columns differ in scale, so a
    # per-column scale keeps relative error tiny (cosine parity > 0.999).
    col_max = np.max(np.abs(E), axis=0)
    scales = np.where(col_max > 0, col_max / 127.0, 1.0).astype(np.float32)
    q = np.clip(np.round(E / scales), -127, 127).astype(np.int8)

    name = args.model.encode("utf-8")
    cont_b = cont.encode("utf-8")
    out = bytearray()
    out += b"EBM1"
    out += struct.pack("<I", 1)
    out += struct.pack("<H", len(name)) + name
    out += struct.pack("<I", dim)
    out += struct.pack("<I", vocab_size)
    out += struct.pack("<I", 1)  # dtype int8
    out += struct.pack("<I", flags)
    out += struct.pack("<I", unk_id)
    out += struct.pack("<H", len(cont_b)) + cont_b
    for tok in id_to_tok:
        tb = tok.encode("utf-8")
        out += struct.pack("<H", len(tb)) + tb
    out += scales.tobytes()
    out += q.tobytes()

    outp = Path(args.out)
    outp.parent.mkdir(parents=True, exist_ok=True)
    outp.write_bytes(out)

    # Golden parity set: reference embeddings from the real model, so the Go
    # tokenizer + pooling can be validated by cosine against ground truth.
    texts = [
        "use spaces not tabs",
        "Indentation Convention!",
        "café RÉSUMÉ",
        "the cat sat on the mat",
        "authentication token validation",
        "how do I change the retry backoff",
        "facts are scoped to a branch",
        "",
    ]
    vecs = [m.encode([t])[0].astype(float).tolist() for t in texts]
    gp = Path(args.golden)
    gp.parent.mkdir(parents=True, exist_ok=True)
    gp.write_text(json.dumps({"model": args.model, "dim": dim, "texts": texts, "vectors": vecs}, indent=2))

    print(f"wrote {outp} ({len(out)/1e6:.1f} MB) vocab={vocab_size} dim={dim} "
          f"lowercase={lowercase} strip_accents={strip_accents} chinese={chinese}")
    print(f"wrote {gp} ({len(texts)} golden vectors)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
