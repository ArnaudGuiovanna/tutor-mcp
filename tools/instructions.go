// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package tools

// ServerInstructions is returned in the MCP initialize result. Hosts that do
// not load the tutor_mcp prompt still learn the core loop from it, so it stays
// short: tool schemas and descriptions carry the details.
const ServerInstructions = `Tutor MCP keeps the learner model (mastery, retention, evidence) and chooses what to practise next; you write the content and talk to the learner in their language. Tool results are in English.

Learning loop:
1. Context: call get_learner_context (bound native conversations: prepare_pedagogical_turn). If a result says needs_domain_setup=true, break the learner's goal into concepts and call init_domain once. Never create a second domain for a subject that already exists; pass its domain_id.
2. Session: call start_learning_session and reuse its session_id until it is closed or expires.
3. Activity: call get_next_activity(domain_id). Follow pedagogical_contract (llm_instruction, constraints, allowed_variants); keep audit_rationale internal.
4. Present one activity. Before showing a graded task, freeze it with prepare_assessment_attempt(decision_id), then store the learner's answer with submit_assessment_attempt before grading.
5. Record: call record_interaction (with attempt_id when prepared) before the next activity; call record_learning_event after delivering feedback or instruction.
6. Close: record_affect, then record_session_close(session_id).

idempotency_key (key on training tools): use a fresh value for each logical mutation; reuse it only to retry the identical call after a transport failure.
The tutor_mcp prompt holds the full protocol.`
