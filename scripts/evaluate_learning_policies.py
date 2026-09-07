#!/usr/bin/env python3
"""Assign participants and describe preregistered delayed learning outcomes.

Inputs are operator-supplied exports, not an independent verification of the
learning process. This tool never tunes or deploys runtime policy parameters.
"""
import argparse
import hashlib
import json
import math
from datetime import datetime, timezone
from pathlib import Path


class InvalidStudy(ValueError):
    pass


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise InvalidStudy("duplicate JSON key")
        result[key] = value
    return result


def load_json(raw):
    return json.loads(raw, object_pairs_hook=unique_object,
                      parse_constant=lambda _: (_ for _ in ()).throw(InvalidStudy("non-finite JSON")))


def instant(value):
    if not isinstance(value, str):
        raise InvalidStudy("timestamp must be an ISO 8601 string with timezone")
    try:
        result = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as exc:
        raise InvalidStudy("invalid timestamp") from exc
    if result.tzinfo is None:
        raise InvalidStudy("timestamp requires timezone")
    return result.astimezone(timezone.utc)


def label(value):
    return isinstance(value, str) and 0 < len(value) <= 255 and value.strip() == value


def number(value):
    return type(value) in (int, float) and math.isfinite(value)


def validate_protocol(protocol):
    fields = {"version", "experiment_id", "assignment_salt", "registered_at", "arms",
              "followup_min_hours", "followup_max_hours", "task_families", "endpoints"}
    if not isinstance(protocol, dict) or set(protocol) != fields or type(protocol["version"]) is not int or protocol["version"] != 1:
        raise InvalidStudy("unsupported protocol")
    if not label(protocol["experiment_id"]) or not label(protocol["assignment_salt"]) or len(protocol["assignment_salt"]) < 32:
        raise InvalidStudy("experiment ID and an allocation salt of at least 32 characters are required")
    instant(protocol["registered_at"])
    if not isinstance(protocol["arms"], dict) or set(protocol["arms"]) != {"baseline", "candidate"} or not all(label(v) for v in protocol["arms"].values()) or len(set(protocol["arms"].values())) != 2:
        raise InvalidStudy("two distinct frozen policy versions are required")
    low, high = protocol["followup_min_hours"], protocol["followup_max_hours"]
    if not number(low) or not number(high) or not 0 < low <= high <= 8760:
        raise InvalidStudy("invalid fixed follow-up window")
    for name, allowed in [("task_families", None), ("endpoints", {"retention", "transfer"})]:
        values = protocol[name]
        if not isinstance(values, list) or not values or not all(label(v) for v in values) or len(set(values)) != len(values) or (allowed and not set(values) <= allowed):
            raise InvalidStudy("invalid protocol families or endpoints")
    return protocol


def protocol_hash(protocol):
    canonical = json.dumps(protocol, sort_keys=True, separators=(",", ":"), ensure_ascii=False)
    return hashlib.sha256(canonical.encode()).hexdigest()


def assigned_arm(protocol, participant_id):
    if not label(participant_id):
        raise InvalidStudy("invalid participant pseudonym")
    raw = json.dumps([protocol["experiment_id"], protocol["assignment_salt"], participant_id], separators=(",", ":"))
    return ("baseline", "candidate")[hashlib.sha256(raw.encode()).digest()[0] & 1]


def assignments(protocol, participants, assigned_at):
    validate_protocol(protocol)
    if instant(assigned_at) < instant(protocol["registered_at"]):
        raise InvalidStudy("assignment predates protocol registration")
    seen = set()
    rows = []
    for participant_id in participants:
        if participant_id in seen:
            raise InvalidStudy("duplicate participant")
        seen.add(participant_id)
        arm = assigned_arm(protocol, participant_id)
        rows.append({"participant_id": participant_id, "arm": arm,
                     "policy_version": protocol["arms"][arm], "assigned_at": assigned_at,
                     "protocol_hash": protocol_hash(protocol)})
    return rows


def wilson(successes, count):
    if count == 0:
        return None
    z = 1.959963984540054
    p = successes / count
    denominator = 1 + z * z / count
    center = (p + z * z / (2 * count)) / denominator
    half = z * math.sqrt(p * (1-p) / count + z*z / (4*count*count)) / denominator
    return [max(0, center-half), min(1, center+half)]


def analyze(protocol, enrolled, outcomes):
    validate_protocol(protocol)
    roster = {}
    for row in enrolled:
        if set(row) != {"participant_id", "arm", "policy_version", "assigned_at", "protocol_hash"}:
            raise InvalidStudy("invalid assignment fields")
        participant_id = row["participant_id"]
        if not label(participant_id) or participant_id in roster:
            raise InvalidStudy("invalid or duplicate participant")
        arm = assigned_arm(protocol, participant_id)
        if row["arm"] != arm or row["policy_version"] != protocol["arms"][arm] or row["protocol_hash"] != protocol_hash(protocol):
            raise InvalidStudy("assignment changed after registration")
        if instant(row["assigned_at"]) < instant(protocol["registered_at"]):
            raise InvalidStudy("assignment predates registration")
        roster[participant_id] = row
    if not roster:
        raise InvalidStudy("empty enrollment roster")
    buckets = {(endpoint, arm): {"assigned": sum(r["arm"] == arm for r in roster.values()),
                                "observed": 0, "passed": 0, "excluded": {}, "predictions": []}
               for endpoint in protocol["endpoints"] for arm in protocol["arms"]}
    seen, attempts, adjudications = set(), set(), set()
    required = {"participant_id", "endpoint", "attempt_id", "task_family", "last_exposure_at",
                "submitted_at", "adjudicated_at", "adjudication_id", "evaluation_method",
                "trusted_evaluation", "hints_requested", "passed"}
    for row in outcomes:
        if not isinstance(row, dict) or not required <= row.keys() or not row.keys() <= required | {"predicted_probability", "predicted_at"}:
            raise InvalidStudy("invalid outcome fields")
        participant_id, endpoint = row["participant_id"], row["endpoint"]
        if participant_id not in roster or endpoint not in protocol["endpoints"]:
            raise InvalidStudy("outcome outside the registered population or endpoints")
        key = (participant_id, endpoint)
        if key in seen or row["attempt_id"] in attempts or row["adjudication_id"] in adjudications:
            raise InvalidStudy("duplicate outcome, attempt or adjudication")
        if not label(row["attempt_id"]) or not label(row["adjudication_id"]):
            raise InvalidStudy("assessment provenance is required")
        seen.add(key); attempts.add(row["attempt_id"]); adjudications.add(row["adjudication_id"])
        if type(row["passed"]) is not bool or type(row["trusted_evaluation"]) is not bool or type(row["hints_requested"]) is not int or row["hints_requested"] < 0:
            raise InvalidStudy("outcome flags must have exact types")
        exposure, submitted, adjudicated = (instant(row[f]) for f in ("last_exposure_at", "submitted_at", "adjudicated_at"))
        if not instant(roster[participant_id]["assigned_at"]) <= exposure <= submitted <= adjudicated:
            raise InvalidStudy("outcome chronology contradicts assignment/exposure/submission")
        prediction = row.get("predicted_probability")
        if ("predicted_probability" in row) != ("predicted_at" in row):
            raise InvalidStudy("prediction and its timestamp are required together")
        if "predicted_probability" in row and (not number(prediction) or not 0 <= prediction <= 1 or not instant(roster[participant_id]["assigned_at"]) <= instant(row["predicted_at"]) < submitted):
            raise InvalidStudy("prediction must be finite, bounded and recorded before the response")
        delay = (submitted - exposure).total_seconds() / 3600
        reason = None
        if not row["trusted_evaluation"] or row["evaluation_method"] not in {"external_service", "human_review"}:
            reason = "untrusted_evaluation"
        elif row["hints_requested"]:
            reason = "assisted_response"
        elif row["task_family"] not in protocol["task_families"]:
            reason = "unregistered_task_family"
        elif not protocol["followup_min_hours"] <= delay <= protocol["followup_max_hours"]:
            reason = "outside_followup_window"
        bucket = buckets[(endpoint, roster[participant_id]["arm"])]
        if reason:
            bucket["excluded"][reason] = bucket["excluded"].get(reason, 0) + 1
            continue
        bucket["observed"] += 1
        bucket["passed"] += row["passed"]
        if prediction is not None:
            bucket["predictions"].append((prediction, int(row["passed"])))
    results = {}
    for endpoint in protocol["endpoints"]:
        arms = {}
        for arm in protocol["arms"]:
            bucket = buckets[(endpoint, arm)]
            total, observed, passed = (bucket[k] for k in ("assigned", "observed", "passed"))
            predictions = bucket.pop("predictions")
            arms[arm] = {**bucket, "missing_or_ineligible": total-observed,
                         "coverage": observed/total if total else None,
                         "observed_success_rate": passed/observed if observed else None,
                         "observed_rate_wilson_95": wilson(passed, observed),
                         "all_assigned_rate_bounds": [passed/total, (passed+total-observed)/total] if total else None,
                         "calibration_count": len(predictions),
                         "brier_score": sum((p-y)**2 for p,y in predictions)/len(predictions) if predictions else None}
        baseline, candidate = arms["baseline"], arms["candidate"]
        rates = [candidate["observed_success_rate"], baseline["observed_success_rate"]]
        bounds = [candidate["all_assigned_rate_bounds"], baseline["all_assigned_rate_bounds"]]
        results[endpoint] = {"arms": arms,
            "observed_rate_difference": rates[0]-rates[1] if None not in rates else None,
            "all_assigned_difference_bounds": [bounds[0][0]-bounds[1][1], bounds[0][1]-bounds[1][0]] if all(bounds) else None}
    return {"experiment_id": protocol["experiment_id"], "protocol_hash": protocol_hash(protocol),
            "unit": "participant", "source_integrity": "operator_supplied_exports_not_verified",
            "causal_claim": False, "automatic_policy_change": False, "endpoints": results}


def read_lines(path):
    with Path(path).open() as handle:
        for raw in handle:
            if raw.strip():
                if len(raw) > 16384:
                    raise InvalidStudy("oversized input row")
                yield load_json(raw)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("operation", choices=["assign", "analyze"])
    parser.add_argument("--protocol", required=True)
    parser.add_argument("--participants", help="one participant pseudonym per line, for assign")
    parser.add_argument("--assigned-at", help="fixed assignment timestamp, for assign")
    parser.add_argument("--assignments", help="frozen assignment JSONL, for analyze")
    parser.add_argument("--outcomes", help="independently assessed outcome JSONL, for analyze")
    args = parser.parse_args()
    try:
        protocol = validate_protocol(load_json(Path(args.protocol).read_text()))
        if args.operation == "assign":
            if not args.participants or not args.assigned_at:
                parser.error("assign requires --participants and --assigned-at")
            rows = assignments(protocol, Path(args.participants).read_text().splitlines(), args.assigned_at)
            for row in rows:
                print(json.dumps(row, sort_keys=True))
        else:
            if not args.assignments or not args.outcomes:
                parser.error("analyze requires --assignments and --outcomes")
            print(json.dumps(analyze(protocol, read_lines(args.assignments), read_lines(args.outcomes)), indent=2, allow_nan=False))
    except (InvalidStudy, ValueError, TypeError, KeyError, OSError):
        parser.exit(2, "Invalid study input; check the registered protocol, exact field types and event chronology.\n")


if __name__ == "__main__":
    main()
