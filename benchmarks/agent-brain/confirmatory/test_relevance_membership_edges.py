"""Synthetic eligibility boundaries without changing protocol attestations."""
import copy
import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("relevance_membership_edges", Path(__file__).with_name("relevance_dataset.py"))
relevance = importlib.util.module_from_spec(spec)
spec.loader.exec_module(relevance)

CUTOFF = "2026-09-18T00:00:00Z"


def fact(identifier, sessions, status="active"):
    return {"fact_id": identifier, "fact_sha256": "a" * 64,
            "status": status, "provenance_session_ids": sessions}


class MembershipEdges(unittest.TestCase):
    def membership(self, facts, dates):
        return {"schema_version": relevance.SOURCE_MEMBERSHIP_SCHEMA_VERSION,
                "facts": facts, "session_dates": dates}

    def test_every_provenance_session_must_precede_cutoff_and_be_included(self):
        membership = self.membership([
            fact("z-kept", ["before"]), fact("a-kept", ["before", "offset-before"]),
            fact("at-cutoff", ["at"]), fact("offset-at-cutoff", ["offset-at"]),
            fact("mixed-age", ["after", "before"]), fact("excluded", ["before", "excluded"]),
            fact("unknown", ["missing"]), fact("bad-date", ["bad"]),
            fact("non-string-date", ["number"]), fact("missing-provenance", []),
            fact("retired", ["before"], "retired"),
        ], {
            "before": "2026-09-17T23:59:59Z", "offset-before": "2026-09-18T01:59:59+02:00",
            "at": CUTOFF, "offset-at": "2026-09-18T02:00:00+02:00",
            "after": "2026-09-18T00:00:01Z", "excluded": "2026-01-01T00:00:00Z",
            "bad": "not-a-date", "number": 42,
        })
        before = copy.deepcopy(membership)
        result = relevance.active_eligible_fact_catalog(membership, CUTOFF, ["excluded"])
        self.assertEqual(result, [
            {"fact_id": "a-kept", "fact_sha256": "a" * 64},
            {"fact_id": "z-kept", "fact_sha256": "a" * 64},
        ])
        self.assertEqual(membership, before)

    def test_malformed_membership_and_provenance_fail_closed(self):
        valid = self.membership([fact("one", ["before"])], {"before": "2026-01-01T00:00:00Z"})
        malformed = []
        for key, value in (("schema_version", -1), ("facts", {}), ("session_dates", [])):
            m = copy.deepcopy(valid)
            m[key] = value
            malformed.append(m)
        for sessions in (["before", "before"], ["z", "a"], [""]):
            m = copy.deepcopy(valid)
            m["facts"][0]["provenance_session_ids"] = sessions
            malformed.append(m)
        for m in malformed:
            with self.subTest(membership=m), self.assertRaises(relevance.DatasetError):
                relevance.active_eligible_fact_catalog(m, CUTOFF, [])

    def test_duplicate_eligible_ids_and_bad_hashes_are_rejected(self):
        for facts in ([fact("one", ["before"]), fact("one", ["before"])],
                      [{**fact("one", ["before"]), "fact_sha256": "truncated"}]):
            with self.subTest(facts=facts), self.assertRaises(relevance.DatasetError):
                relevance.active_eligible_fact_catalog(
                    self.membership(facts, {"before": "2026-01-01T00:00:00Z"}), CUTOFF, [])


if __name__ == "__main__":
    unittest.main()
