import copy
import unittest

from evaluate_learning_policies import InvalidStudy, analyze, assignments, assigned_arm, load_json


class PolicyEvaluationTest(unittest.TestCase):
    def setUp(self):
        self.protocol = {"version": 1, "experiment_id": "test-study", "assignment_salt": "x"*32,
                         "registered_at": "2026-01-01T00:00:00Z", "arms": {"baseline": "simple-v1", "candidate": "runtime-v4"},
                         "followup_min_hours": 168, "followup_max_hours": 192,
                         "task_families": ["generated-heldout"], "endpoints": ["retention", "transfer"]}
        self.ids = {arm: next(str(i) for i in range(1000) if assigned_arm(self.protocol, str(i)) == arm)
                    for arm in self.protocol["arms"]}
        self.roster = assignments(self.protocol, self.ids.values(), "2026-01-02T00:00:00Z")

    def outcome(self, arm="candidate", endpoint="retention", passed=True):
        return {"participant_id": self.ids[arm], "endpoint": endpoint,
                "attempt_id": arm+endpoint, "task_family": "generated-heldout",
                "last_exposure_at": "2026-01-03T00:00:00Z", "submitted_at": "2026-01-10T00:00:00Z",
                "adjudicated_at": "2026-01-11T00:00:00Z", "adjudication_id": "j-"+arm+endpoint,
                "evaluation_method": "external_service", "trusted_evaluation": True,
                "hints_requested": 0, "passed": passed}

    def test_missing_outcomes_remain_visible_in_population_bounds(self):
        report = analyze(self.protocol, self.roster, [self.outcome()])
        result = report["endpoints"]["retention"]
        self.assertEqual(result["arms"]["candidate"]["observed_success_rate"], 1)
        self.assertIsNone(result["arms"]["baseline"]["observed_success_rate"])
        self.assertEqual(result["arms"]["baseline"]["missing_or_ineligible"], 1)
        self.assertEqual(result["all_assigned_difference_bounds"], [0, 1])
        self.assertIsNone(result["observed_rate_difference"])
        self.assertFalse(report["causal_claim"])

    def test_comparison_and_calibration_use_committed_predictions(self):
        row = self.outcome()
        row.update(predicted_probability=.8, predicted_at="2026-01-09T00:00:00Z")
        report = analyze(self.protocol, self.roster, [row, self.outcome("baseline", passed=False)])
        result = report["endpoints"]["retention"]
        self.assertEqual(result["observed_rate_difference"], 1)
        self.assertAlmostEqual(result["arms"]["candidate"]["brier_score"], .04)

    def test_assistance_trust_and_delay_exclusions_do_not_disappear(self):
        for field, value, reason in [("hints_requested", 1, "assisted_response"),
                                     ("trusted_evaluation", False, "untrusted_evaluation"),
                                     ("submitted_at", "2026-01-03T01:00:00Z", "outside_followup_window")]:
            row = self.outcome(); row[field] = value
            result = analyze(self.protocol, self.roster, [row])["endpoints"]["retention"]["arms"]["candidate"]
            self.assertEqual(result["excluded"], {reason: 1})
            self.assertEqual(result["all_assigned_rate_bounds"], [0, 1])

    def test_rejects_selection_duplicates_and_chronology(self):
        row = self.outcome()
        with self.assertRaises(InvalidStudy):
            analyze(self.protocol, self.roster, [row, row])
        roster = copy.deepcopy(self.roster); roster[0]["policy_version"] = "post-hoc"
        with self.assertRaises(InvalidStudy):
            analyze(self.protocol, roster, [])
        for change in [{"passed": 1}, {"submitted_at": "2026-01-01T00:00:00Z"},
                       {"predicted_probability": .9, "predicted_at": row["submitted_at"]}]:
            with self.assertRaises(InvalidStudy):
                analyze(self.protocol, self.roster, [{**row, **change}])
        with self.assertRaises(InvalidStudy):
            load_json('{"passed":true,"passed":false}')


if __name__ == "__main__":
    unittest.main()
