#!/usr/bin/env python3
"""Contracts for the synthetic component canary; no model calls."""
import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

SPEC = importlib.util.spec_from_file_location("memory_integrity", Path(__file__).with_name("memory_integrity.py"))
mi = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(mi)


class IntegrityTests(unittest.TestCase):
    def setUp(self):
        self.data = mi.load_fixture(mi.FIXTURE)
        self.sources = {s["id"]: s for s in self.data["sources"]}

    def test_reader_projection_excludes_labels_and_arm(self):
        case = self.data["cases"][0]
        request = mi.request_for(case, [])
        altered = {**case, "expected_choice": "reuse", "misleading_facts": [], "relevant_sources": []}
        self.assertEqual(request, mi.request_for(altered, []))
        self.assertEqual(set(request), {"instruction", "task", "branch", "choices", "memory", "request_sha256"})

    def test_real_session_fixture_keeps_its_kind_and_rejects_unknown_kinds(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "fixture.json"
            data = {**self.data, "kind": "session-grounded-integrity-pilot"}
            mi.write_json(path, data)
            self.assertEqual(mi.load_fixture(path)["kind"], "session-grounded-integrity-pilot")
            mi.write_json(path, {**data, "kind": "unknown"})
            with self.assertRaisesRegex(ValueError, "schema 1"):
                mi.load_fixture(path)

    def test_sources_expand_even_if_raw_query_misses(self):
        fact = self.data["facts"][0]
        packet, _ = mi.make_packet("facts_with_sources", "main", [fact], [], self.sources, 4096)
        self.assertEqual([i["id"] for i in packet], ["source:payment-decision", "fact:payment-global"])
        self.assertIn("NEW purchase", packet[0]["text"])

    def test_raw_backfill_recovers_omitted_fact_and_later_correction(self):
        for case_id, sid in [("omission", "clock-constraint"), ("correction", "retry-correction")]:
            case = next(c for c in self.data["cases"] if c["id"] == case_id)
            packet, _ = mi.make_packet("facts_with_sources", "main", [],
                                      [mi.source_item(self.sources[sid])], self.sources, 4096)
            self.assertTrue(mi.retrieval_metrics(case, packet)["all_required_evidence"])

    def test_branch_filter_applies_before_source_expansion(self):
        fact = next(f for f in self.data["facts"] if f["id"] == "schema-mis-scoped")
        packet, _ = mi.make_packet("facts_with_sources", "main", [fact],
                                  [mi.source_item(self.sources["schema-experiment"])], self.sources, 4096)
        self.assertEqual([i["id"] for i in packet], ["fact:schema-mis-scoped"])

    def test_byte_budget_never_truncates_qualifier_or_splits_group(self):
        source = {"id": "s", "text": "ಕನ್ನಡ exception: only for retries"}
        fact = {"id": "f", "text": "always reuse"}
        full_size = len(mi.encoded([source, fact]))
        packet, omitted = mi.pack([[source, fact]], full_size - 1)
        self.assertEqual(packet, [])
        self.assertEqual(omitted, 1)
        self.assertEqual(mi.pack([[source, fact]], full_size)[0], [source, fact])

    def test_duplicate_evidence_charged_once(self):
        source = {"id": "s", "text": "evidence"}
        a, b = {"id": "a"}, {"id": "b"}
        packet, _ = mi.pack([[source, a], [source, b]], 4096)
        self.assertEqual(packet, [source, a, b])

    def test_reserve_recovers_correction_and_keeps_older_history(self):
        fact = next(f for f in self.data["facts"] if "retry-correction" not in f["source_ids"]
                    and "retry-original" in f["source_ids"])
        raw = [mi.source_item(self.sources[s]) for s in ("retry-original", "retry-correction")]
        baseline, _ = mi.make_packet("facts_with_sources", "main", [fact], raw, self.sources, 512)
        candidate, _ = mi.make_packet("facts_with_sources", "main", [fact], raw, self.sources, 512, 50)
        self.assertNotIn("source:retry-correction", [i["id"] for i in baseline])
        self.assertEqual({i["id"] for i in candidate}, {i["id"] for i in raw})
        self.assertLessEqual(len(mi.encoded(candidate)), 512)

    def test_reserve_preserves_older_fact_and_source_when_they_fit(self):
        fact = self.data["facts"][0]
        old = self.sources[fact["source_ids"][0]]
        recent = mi.source_item({**old, "id": "new", "timestamp": "2026-09-01T00:00:00Z", "text": "New note."})
        expected = [recent, mi.source_item(old), mi.fact_item(fact)]
        budget = len(mi.encoded(expected))
        packet, _ = mi.make_packet("facts_with_sources", "main", [fact], [recent], self.sources, budget, 50)
        self.assertEqual({i["id"] for i in packet}, {recent["id"], "source:"+old["id"], "fact:"+fact["id"]})
        self.assertEqual(len(mi.encoded(packet)), budget)

    def test_reserve_never_pulls_unretrieved_or_other_branch_history(self):
        raw = [mi.source_item(self.sources["schema-experiment"])]
        packet, _ = mi.make_packet("facts_with_sources", "main", [], raw, self.sources, 4096, 50)
        self.assertEqual(packet, [])

    def test_reserve_rounds_up_whole_passage_but_skips_oversized_hit(self):
        huge = {"id":"huge", "timestamp":"2026-09-03T00:00:00Z", "text":"x"*1000}
        fits = {"id":"fits", "timestamp":"2026-09-02T00:00:00Z", "text":"whole qualifier"}
        budget = len(mi.encoded([fits]))
        self.assertEqual(mi.reserve_recent([huge, fits], budget, 50), [fits])
        self.assertEqual(mi.reserve_recent([huge], budget, 100), [])

    def test_reserve_compares_instants_and_keeps_rank_for_equal_timestamps(self):
        a = {"id":"a", "timestamp":"2026-09-01T10:00:00+02:00"}
        b = {"id":"b", "timestamp":"2026-09-01T09:00:00Z"}
        c = {"id":"c", "timestamp":"2026-09-01T09:00:00+00:00"}
        self.assertEqual(mi.reserve_recent([a,b,c], 4096, 100), [b,c,a])

    def test_unknown_dates_get_normal_backfill_not_recent_priority(self):
        raw = [{"id":"missing"}, {"id":"invalid", "timestamp":"yesterday"},
               {"id":"naive", "timestamp":"2026-09-01"}, {"id":"null", "timestamp":None}]
        self.assertEqual(mi.reserve_recent(raw, 4096, 50), [])
        raw = [{**i, "branch":"main"} for i in raw]
        packet, _ = mi.make_packet("facts_with_sources", "main", [], raw, {}, 4096, 50)
        self.assertEqual(packet, raw)

    def test_reserve_deduplicates_and_releases_unused_capacity(self):
        source = mi.source_item(self.sources["payment-decision"])
        fact = self.data["facts"][0]
        packet, _ = mi.make_packet("facts_with_sources", "main", [fact], [source,source], self.sources, 4096, 50)
        self.assertEqual(packet, [source,mi.fact_item(fact)])

    def test_zero_reserve_and_control_arms_preserve_packets(self):
        fact = self.data["facts"][0]
        raw = [mi.source_item(self.sources[fact["source_ids"][0]])]
        for arm in mi.ARMS:
            baseline = mi.make_packet(arm,"main",[fact],raw,self.sources,4096)
            self.assertEqual(baseline,mi.make_packet(arm,"main",[fact],raw,self.sources,4096,0))
            if arm != "facts_with_sources":
                self.assertEqual(baseline,mi.make_packet(arm,"main",[fact],raw,self.sources,4096,50))

    def test_invalid_reserve_rejected(self):
        for percent in (-1,101):
            with self.assertRaisesRegex(ValueError,"between 0 and 100"):
                mi.make_packet("no_memory","main",[],[],{},512,percent)

    def test_retrieved_poison_does_not_count_as_source_evidence(self):
        case = self.data["cases"][0]
        metrics = mi.retrieval_metrics(case, [mi.fact_item(self.data["facts"][0])])
        self.assertEqual(metrics["misleading_claims_delivered"], 1)
        self.assertEqual(metrics["evidence_recall"], 0)

    def test_empty_memory_is_not_a_correct_answer_measurement(self):
        packet, _ = mi.make_packet("no_memory", "main", [], [], self.sources, 4096)
        metrics = mi.retrieval_metrics(self.data["cases"][0], packet)
        self.assertEqual(metrics["evidence_recall"], 0)
        self.assertNotIn("correct", metrics)

    def score_fixture(self, root):
        cases = self.data["cases"]
        requests, rows, answers = [], [], []
        for case in cases:
            for arm in mi.ARMS:
                packet = [] if arm == "no_memory" else [mi.source_item(self.sources[s]) for s in case["relevant_sources"]]
                request = mi.request_for(case, packet)
                requests.append(request)
                rows.append({"case_id": case["id"], "arm": arm, "request_sha256": request["request_sha256"]})
        unique = {r["request_sha256"]: r for r in requests}
        for key, request in unique.items():
            case = next(c for c in cases if c["query"] == request["task"])
            answers.append({"request_sha256": key, "choice": case["expected_choice"], "citations": [i["id"] for i in request["memory"]]})
        request_bytes = b"\n".join(mi.encoded(r) for r in requests)
        mi.write_json(root / "retrieval-report.json", {"fixture_sha256": mi.digest(self.data), "rows": rows,
                                                     "requests_sha256": hashlib.sha256(request_bytes).hexdigest()})
        (root / "requests.jsonl").write_bytes(request_bytes)
        response_path = root / "responses.jsonl"
        response_path.write_bytes(b"\n".join(mi.encoded(r) for r in answers))
        return response_path, answers

    def test_scorer_rejects_missing_responses_and_packet_tampering(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            response_path, answers = self.score_fixture(root)
            self.assertEqual(len(mi.score(mi.FIXTURE, root, response_path, "unit-test-stub")["rows"]), 32)
            response_path.write_bytes(b"\n".join(mi.encoded(r) for r in answers[:-1]))
            with self.assertRaisesRegex(ValueError, "cover every"):
                mi.score(mi.FIXTURE, root, response_path, "unit-test-stub")
            response_path, _ = self.score_fixture(root)
            path = root / "requests.jsonl"
            path.write_text(path.read_text(encoding="utf-8").replace("idempotency", "altered"), encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "digest mismatch"):
                mi.score(mi.FIXTURE, root, response_path, "unit-test-stub")

    def test_scorer_records_fabricated_citation_separately(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            response_path, answers = self.score_fixture(root)
            answers[0]["citations"] = ["source:not-delivered"]
            response_path.write_bytes(b"\n".join(mi.encoded(r) for r in answers))
            scored = mi.score(mi.FIXTURE, root, response_path, "unit-test-stub")
            self.assertEqual(scored["rows"][0]["invalid_citations"], ["source:not-delivered"])
            self.assertFalse(scored["rows"][0]["required_evidence_cited"])

    def test_scorer_rejects_dropped_case_arm(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            response_path, _ = self.score_fixture(root)
            path = root / "retrieval-report.json"
            report = json.loads(path.read_text(encoding="utf-8"))
            report["rows"].pop()
            mi.write_json(path, report)
            with self.assertRaisesRegex(ValueError, "case/arm"):
                mi.score(mi.FIXTURE, root, response_path, "unit-test-stub")


if __name__ == "__main__":
    unittest.main()
