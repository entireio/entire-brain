# Candidate-distillation quantitative evaluator

Prepare complete source evidence with the CLI before labeling:

```sh
entire-brain facts distill-quality corpus /path/to/repo \
  --out /private/fresh-development-corpus --split-seed PINNED-SEED \
  --partition development --json
```

Optional `--branch` and `--session` filters select whole sessions. The export
includes every normalized visible turn in that scope, including sessions with
no admitted candidates. It retains exact source-line anchors and candidate
membership, redacts public text, and writes real source locations separately
in `private-source-map.jsonl`. Keep the output outside the repository and
Brain; all files are private local evaluation artifacts.

This command produces an **unset label template**, not reference labels or a
scoreable quantitative artifact. Complete source labels, paired extraction
outputs, and retrieval judgments must be supplied separately. For confirmation,
pass `--partition confirmation --prior-development-inventory FILE`; the exporter
excludes known family identities and exact duplicate source digests. The
operator must also account for related families that lack shared identifiers,
freeze the split before judgments, and retain the preconfirmation seal.

To prepare isolated extraction stores from the same selected canonical source:

```sh
entire-brain facts distill-quality snapshot /path/to/repo \
  --out /private/fresh-paired-snapshot --branch BRANCH --json
```

The private snapshot contains separate `legacy` and `candidate` plugin roots,
identical source-only manifests and transcripts, and the same taxonomy. It
copies no active facts, proposals, or caches. Its scope digest binds transcript
bytes and session authority metadata. The aggregate raw-source limit is 1 GiB;
narrow the branch/session scope when it is exceeded.

For each arm, set `ENTIRE_PLUGIN_CONFIG_DIR`, `ENTIRE_PLUGIN_DATA_DIR`,
`ENTIRE_PLUGIN_STATE_DIR`, and `ENTIRE_PLUGIN_CACHE_DIR` to the paths in its
snapshot manifest, then run the ordinary distill command with that pipeline.
First verify `distill --dry-run --json` reports the arm's `brain_dir`. Use a
fresh copy of the empty arm for every cold repetition. The snapshot is private
source material and has neither reference labels nor release approval.

The existing `facts eval` arms can inspect each resulting store with the same
task file. `facts eval-compare` intentionally treats different Brain manifests
as diagnostic unless its release identity requirements are met; do not bypass
that check to claim a paired release result. Bind common source/task identities
and both treatment artifacts separately in the quantitative evidence below.

Run `python3 benchmarks/agent-brain/candidate_distillation_eval.py EVIDENCE.json`.
It accepts a single strict JSON artifact with schema
`candidate-distillation-quantitative-evidence/v1` and writes a deterministic
JSON report. `pass` or `fail` means the supplied fixed corpus was scoreable;
the report deliberately cannot approve a plan phase.

The artifact must predeclare `split_seed`, `source_cutoff`, `strata`, both
`corpus_sizes`, every provider repetition, evaluator metric version, bootstrap
method/seed/10,000 resamples, and a recomputable numeric development-only power calculation
with its required independent-family count. It must contain exactly disjoint
`development` and `confirmation` partitions. A session family, continued
session, re-export, or duplicate source must appear in only one partition.

Each partition supplies exactly `corpus_sizes[role]` `source_sessions` and the
artifact supplies a `preconfirmation_seal` that binds canonical SHA-256
digests of the contract and each partition's source roster only; labels and
outputs are deliberately excluded from that pre-judgment commitment. A separate
`scored_partition_digests` map binds the complete post-judgment inputs. Every session supplies immutable
`source_content` plus its matching `source_content_sha256`; content hashes and
family identities must be disjoint across partitions, including re-exports.
Every
session supplies fully labeled `reference_facts` (which may be empty for a
fully labeled negative session; otherwise unique `id`; set
`authority_class: true` for the 98% subset), an `expected_repetitions` map, and
complete `candidate` and `legacy` outputs for every provider/repetition. Each
output lists `admitted_span_fact_ids` and `emitted_facts`; every admitted span
must name a labeled reference and be identical across provider repetitions.
Confirmation also supplies paired `retrieval_tasks` with a
common `family_id` and finite candidate/legacy `recall`, `precision`, and
`useful_per_1k` values. Supply exactly one pre-aggregated paired row per task
family; multiple unweighted rows are rejected so variable family sizes cannot
alter the estimand.

The scorer uses all labeled reference facts across the whole partition as the span/extraction denominator. Empty negative sessions remain in the precision denominator: unsupported emitted outputs are false positives. It does not drop missing provider output, and fails closed on zero denominators,
incomplete paired coverage, partition/family overlap, or insufficient
independent source or retrieval families. It computes fixed-corpus 95%/98%
span gates and four paired, family-cluster bootstrap lower bounds: recall and
retrieval recall must be at least -0.02; durable precision and useful-per-1k
must be at least zero. A report has no self-attested phase-pass field; its
`acceptance_decision` is always `evaluation_only_not_phase_gate` and it does
not replace panel judgments or human approval.

The digest receipt proves that the scorer used the frozen bytes. It does not
prove who sealed them or when confirmation labels were viewed. Retain the seal
in the independent evaluation ledger and obtain the required panel/human
decision separately; this local tool must never be used as release acceptance.
