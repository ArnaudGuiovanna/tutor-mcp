// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! Protocol fixtures, NOT native inference or live Tutor qualification. Input
//! schemas are taken from the captured real tools/list fixture. Receipt bodies
//! are synthetic examples of the Go handlers' actual response formats.
use super::*;
use crate::{
    catalog::ToolEntry,
    executor::{self, Preparation},
    journal::OperationRequest,
    session_store::{Completion, Owner},
};

const EPOCH: &str = "h07-test-epoch";
const TOOLS: [&str; 4] = [
    "get_conversation_context",
    "get_curriculum_snapshot",
    "start_learning_session",
    "get_next_activity",
];

fn payloads() -> Value {
    serde_json::from_str::<Value>(include_str!("../fixtures/h07-receipts.json")).unwrap()
        ["payloads"]
        .clone()
}
fn owner(account: &str) -> Owner {
    Owner {
        profile_id: "profile-test".into(),
        account_id: account.into(),
        tenant_id: "tenant-test".into(),
        server_id: "server-test".into(),
    }
}
fn entries() -> Vec<ToolEntry> {
    let captured: Value =
        serde_json::from_str(include_str!("../fixtures/catalog-local.json")).unwrap();
    captured["tools"]
        .as_array()
        .unwrap()
        .iter()
        .filter(|tool| TOOLS.contains(&tool["name"].as_str().unwrap()))
        .map(|tool| ToolEntry {
            server_id: "tutor".into(),
            name: tool["name"].as_str().unwrap().into(),
            description: tool["description"].as_str().unwrap().into(),
            input_schema: tool["inputSchema"].clone(),
            annotations: tool["annotations"].clone(),
        })
        .collect()
}
fn envelope(value: Value) -> Value {
    // tools/tools.go:jsonResult emits both forms. Structured data has priority.
    json!({"content":[{"type":"text","text":value.to_string()}],"structuredContent":value,"isError":false})
}

struct Fixture {
    store: SessionStore,
    catalog: ToolCatalog,
    host: ContentRef,
    turn: String,
    payloads: Value,
    _root: tempfile::TempDir,
}
impl Fixture {
    fn new() -> Self {
        let root = tempfile::tempdir().unwrap();
        let mut store =
            SessionStore::create(root.path(), &owner("account-test"), &"0".repeat(64)).unwrap();
        let turn = store
            .begin_turn("Demande synthétique de diagnostic.")
            .unwrap();
        let payloads = payloads();
        let host = json!({"conversationId":payloads["get_conversation_context"]["conversation_id"],
            "language":"fr","trainingContext":payloads["get_conversation_context"]});
        store
            .journal_mut()
            .append_event(
                "host-context",
                Some(&turn),
                "verified_host_context",
                &serde_json::to_vec(&host).unwrap(),
            )
            .unwrap();
        let host = store
            .journal()
            .content_ref(ContentKind::Event, "host-context")
            .unwrap();
        let mut catalog = ToolCatalog::new(EPOCH.into()).unwrap();
        let tools = entries();
        assert_eq!(tools.len(), 4);
        catalog.replace_server_tools(EPOCH, "tutor", tools).unwrap();
        Self {
            store,
            catalog,
            host,
            turn,
            payloads,
            _root: root,
        }
    }
    fn begin(&mut self) -> Workflow {
        Workflow::begin(&mut self.store, &self.turn, &self.host, EPOCH, Intent::Auto).unwrap()
    }
    fn with_sql_setup(sql: &str) -> Self {
        Self::new().reopen_with_sql(sql)
    }
    fn reopen_with_sql(self, sql: &str) -> Self {
        let Self {
            store,
            catalog,
            host,
            turn,
            payloads,
            _root,
        } = self;
        let id = store.id().to_owned();
        drop(store);
        // Only this fixture's private owner directory exists. A separate SQL
        // connection is used only while the exclusive journal is closed.
        let mut dirs = std::fs::read_dir(_root.path()).unwrap();
        let directory = dirs.next().unwrap().unwrap().path();
        assert!(dirs.next().is_none());
        let db = rusqlite::Connection::open(directory.join(format!("{id}.sqlite3"))).unwrap();
        db.execute_batch(sql).unwrap();
        drop(db);
        let store =
            SessionStore::load(_root.path(), &owner("account-test"), &id, &"0".repeat(64)).unwrap();
        Self {
            store,
            catalog,
            host,
            turn,
            payloads,
            _root,
        }
    }
    fn issue(&mut self, machine: &mut Workflow) -> AuthorizedStep {
        machine
            .authorize_next(&mut self.store, &self.catalog, &self.host, EPOCH)
            .unwrap()
    }
    fn finish_with(&mut self, step: AuthorizedStep, envelope: Value) -> ContentRef {
        let (call, permit) = step.into_parts();
        let id = call.operation_id().to_owned();
        assert!(matches!(
            executor::prepare(self.store.journal_mut(), &self.turn, call, Some(permit)).unwrap(),
            Preparation::Ready(_)
        ));
        // Simulated transport completion only. No executor.execute(), socket,
        // model or server is used by these fixtures.
        self.store.journal_mut().mark_dispatched(&id).unwrap();
        self.store
            .journal_mut()
            .complete_operation(&id, &serde_json::to_vec(&envelope).unwrap())
            .unwrap();
        self.store
            .journal()
            .content_ref(ContentKind::OperationResult, &id)
            .unwrap()
    }
    fn accept(&mut self, machine: &mut Workflow, source: &ContentRef) -> Result<()> {
        machine.accept_result(&mut self.store, &self.catalog, &self.host, EPOCH, source)
    }
    fn advance(&mut self, machine: &mut Workflow) -> ContentRef {
        let step = self.issue(machine);
        let body = self.payloads[step.call().name()].clone();
        let source = self.finish_with(step, envelope(body));
        self.accept(machine, &source).unwrap();
        source
    }
    fn reach(&mut self, machine: &mut Workflow, phase: Phase) {
        for _ in 0..4 {
            if machine.phase() == phase {
                return;
            }
            self.advance(machine);
        }
        assert_eq!(machine.phase(), phase);
    }
}

#[test]
fn real_catalog_and_server_receipts_reach_decision_without_enabling_assessment() {
    let mut f = Fixture::new();
    let mut machine = f.begin();
    let phases = [
        Phase::ContextConfirmed,
        Phase::CurriculumConfirmed,
        Phase::SessionOpen,
        Phase::DecisionConfirmed,
    ];
    for (name, next_phase) in TOOLS.into_iter().zip(phases) {
        let step = f.issue(&mut machine);
        assert_eq!(step.call().name(), name);
        let permission = step
            .call()
            .permission_params(f.store.id(), "permission-request");
        assert_eq!(permission["toolCall"]["title"], name);
        assert_eq!(permission["toolCall"]["rawInput"]["tool"], name);
        assert_eq!(permission["options"][0]["kind"], "allow_once");
        // Authorizing is not preparing or dispatching, even though all four
        // tools conservatively require WorkflowPermit in the existing executor.
        assert!(f
            .store
            .journal()
            .operation(step.call().operation_id())
            .unwrap()
            .is_none());
        let args = step.call().arguments();
        if name == "get_conversation_context" {
            assert_eq!(
                args,
                &json!({"conversation_id":machine.host.conversation_id})
            );
        } else {
            assert_eq!(args["domain_id"], "domain_h07");
            assert!(
                args.get("session_id").is_none(),
                "get_next_activity has no session parameter"
            );
        }
        let source = f.finish_with(step, envelope(f.payloads[name].clone()));
        f.accept(&mut machine, &source).unwrap();
        assert_eq!(machine.phase(), next_phase);
    }
    let decision = machine.decision(&f.store, &f.host, EPOCH).unwrap();
    assert_eq!(decision.decision_id(), "decision_h07");
    assert_eq!(decision.concept_key(), "Variables");
    assert_eq!(
        decision.contract()["competency"]["id"],
        "concept_immutable_1"
    );
    assert_eq!(decision.contract()["curriculum_version"], 7);
    assert_eq!(
        machine.context.as_ref().unwrap().value["revision"],
        "revision-opaque-a"
    );
    assert!(matches!(
        machine.authorize_next(&mut f.store, &f.catalog, &f.host, EPOCH),
        Err(Error::Phase)
    ));
    assert_eq!(machine.phase(), Phase::DecisionConfirmed);
}

#[test]
fn actual_executor_requires_permit_and_binds_it_to_one_operation() {
    let mut f = Fixture::new();
    let mut machine = f.begin();
    let (call, permit) = f.issue(&mut machine).into_parts();
    let id = call.operation_id().to_owned();
    assert!(matches!(
        executor::prepare(f.store.journal_mut(), &f.turn, call, None),
        Err(executor::Error::Preconditions)
    ));
    assert!(f.store.journal().operation(&id).unwrap().is_none());
    let mut fields = VerifiedFields::new(EPOCH.into(), f.store.journal()).unwrap();
    fields.bind_conversation(machine.host.conversation_uuid());
    let other = call_validation::validate_call(
        &f.catalog,
        EPOCH,
        "tutor/get_conversation_context",
        &json!({}),
        &fields,
        Uuid::new_v4(),
    )
    .unwrap();
    assert!(matches!(
        executor::prepare(f.store.journal_mut(), &f.turn, other, Some(permit)),
        Err(executor::Error::Preconditions)
    ));
}

#[test]
fn preconditions_retain_exact_sources_and_executor_phase_names() {
    let mut f = Fixture::new();
    let mut machine = f.begin();
    for expected in [
        "context_requested",
        "context_confirmed",
        "context_confirmed",
        "session_open",
    ] {
        let step = f.issue(&mut machine);
        let event_id = format!("{}:precondition", step.call().operation_id());
        let event = f.store.journal().event(&event_id).unwrap().unwrap();
        assert_eq!(event.kind, "pedagogical_precondition");
        let authority_fields: Value = serde_json::from_slice(&event.payload).unwrap();
        assert_eq!(authority_fields["authority_anchor_id"], machine.anchor.id());
        assert_eq!(
            authority_fields["authority_anchor_sha256"],
            json!(machine.anchor.sha256())
        );
        assert_eq!(event.turn_id.as_deref(), Some(f.turn.as_str()));
        let payload: Value = serde_json::from_slice(&event.payload).unwrap();
        assert_eq!(payload["phase"], expected);
        assert_eq!(payload["fingerprint"], step.call().fingerprint());
        assert_eq!(payload["sources"][0], proof(&f.host));
        assert_eq!(payload["acp_session_id"], f.store.id());
        let body = f.payloads[step.call().name()].clone();
        let source = f.finish_with(step, envelope(body));
        f.accept(&mut machine, &source).unwrap();
    }
}

#[test]
fn text_json_mcp_fallback_works_but_ambiguous_or_error_envelopes_do_not() {
    for mode in [
        "text",
        "error",
        "ambiguous",
        "structured-error",
        "too-large",
    ] {
        let mut f = Fixture::new();
        let mut machine = f.begin();
        let step = f.issue(&mut machine);
        let body = f.payloads["get_conversation_context"].clone();
        let mut result = json!({"content":[{"type":"text","text":body.to_string()}]});
        match mode {
            "error" => result["isError"] = json!(true),
            "ambiguous" => result["content"]
                .as_array_mut()
                .unwrap()
                .push(json!({"type":"text","text":body.to_string()})),
            "structured-error" => result["structuredContent"] = json!({"error":"not_selected"}),
            "too-large" => result["padding"] = json!("x".repeat(MAX_RECEIPT)),
            _ => (),
        }
        let source = f.finish_with(step, result);
        let accepted = f.accept(&mut machine, &source);
        assert_eq!(accepted.is_ok(), mode == "text", "{mode}");
        if mode != "text" {
            assert_eq!(machine.phase(), Phase::Stopped);
        }
    }
}

#[test]
fn context_rejects_changed_conversation_training_domain_revision_or_selection() {
    for key in [
        "conversation_id",
        "training_id",
        "domain_id",
        "revision",
        "selection_version",
    ] {
        let mut f = Fixture::new();
        let mut machine = f.begin();
        let step = f.issue(&mut machine);
        let mut body = f.payloads["get_conversation_context"].clone();
        body[key] = if key == "selection_version" {
            json!(5)
        } else {
            json!("foreign")
        };
        let source = f.finish_with(step, envelope(body));
        assert_eq!(f.accept(&mut machine, &source), Err(Error::Scope), "{key}");
        assert!(machine.context.is_none());
    }
}

#[test]
fn curriculum_rejects_wrong_domain_nonpositive_version_and_duplicate_keys() {
    for mode in [
        "domain",
        "zero-version",
        "string-version",
        "duplicate",
        "empty",
    ] {
        let mut f = Fixture::new();
        let mut machine = f.begin();
        f.advance(&mut machine);
        let step = f.issue(&mut machine);
        let mut body = f.payloads["get_curriculum_snapshot"].clone();
        match mode {
            "domain" => body["snapshot"]["domain_id"] = json!("foreign"),
            "zero-version" => body["snapshot"]["version"] = json!(0),
            "string-version" => body["snapshot"]["version"] = json!("7"),
            "duplicate" => {
                let c = body["snapshot"]["concepts"][0].clone();
                body["snapshot"]["concepts"].as_array_mut().unwrap().push(c);
            }
            _ => body["snapshot"]["concepts"] = json!([]),
        }
        let source = f.finish_with(step, envelope(body));
        assert!(f.accept(&mut machine, &source).is_err(), "{mode}");
        assert!(machine.curriculum.is_none());
    }
}

#[test]
fn session_receipt_requires_same_domain_matching_ids_and_open_status() {
    for (pointer, value) in [
        ("/session_id", json!("other")),
        ("/session/domain_id", json!("other")),
        ("/session/status", json!("closed")),
        ("/session/closed_at", json!("2026-10-30T09:00:00Z")),
    ] {
        let mut f = Fixture::new();
        let mut machine = f.begin();
        f.reach(&mut machine, Phase::CurriculumConfirmed);
        let step = f.issue(&mut machine);
        let mut body = f.payloads["start_learning_session"].clone();
        body["session"]["closed_at"] = Value::Null;
        *body.pointer_mut(pointer).unwrap() = value;
        let source = f.finish_with(step, envelope(body));
        assert!(f.accept(&mut machine, &source).is_err(), "{pointer}");
        assert!(machine.session.is_none());
    }
}

#[test]
fn decision_rejects_cross_linked_or_stale_fields_and_unsupported_protocols() {
    for (pointer, value) in [
        ("/domain_id", json!("foreign")),
        ("/session_id", json!("other-session")),
        ("/intent", json!("review")),
        ("/needs_domain_setup", json!(true)),
        ("/decision_id", json!("other-decision")),
        ("/activity/concept", json!("concept_immutable_1")),
        ("/activity/type", json!("CLOSE_SESSION")),
        ("/pedagogical_contract/target_concept", json!("other")),
        (
            "/pedagogical_contract/recommended_activity_type",
            json!("PRACTICE"),
        ),
        ("/pedagogical_contract/curriculum_version", json!(8)),
        ("/pedagogical_contract/competency/id", json!("foreign")),
        (
            "/pedagogical_contract/competency/outcomes/0/id",
            json!("invented"),
        ),
        (
            "/pedagogical_contract/policy_version",
            json!("future-policy"),
        ),
        (
            "/pedagogical_contract/learning_event_protocol",
            json!("future-protocol"),
        ),
        (
            "/pedagogical_contract/bkt_update_mode",
            json!("learning_opportunity"),
        ),
        (
            "/pedagogical_contract/constraints/must_collect",
            json!(["learner_answer"]),
        ),
        (
            "/pedagogical_contract/llm_discretion/cannot_mark_mastery_without_evidence",
            json!(false),
        ),
    ] {
        let mut f = Fixture::new();
        let mut machine = f.begin();
        f.reach(&mut machine, Phase::SessionOpen);
        let step = f.issue(&mut machine);
        let mut body = f.payloads["get_next_activity"].clone();
        *body.pointer_mut(pointer).unwrap() = value;
        let source = f.finish_with(step, envelope(body));
        assert!(f.accept(&mut machine, &source).is_err(), "{pointer}");
        assert!(machine.decision.is_none());
        assert_eq!(machine.phase(), Phase::Stopped);
    }
}

#[test]
fn receipt_for_a_different_operation_or_turn_cannot_advance() {
    for mode in [
        "wrong-id",
        "wrong-tool",
        "wrong-args",
        "wrong-turn",
        "wrong-key",
    ] {
        let mut f = Fixture::new();
        let mut machine = f.begin();
        let step = f.issue(&mut machine);
        let id = if mode == "wrong-id" {
            Uuid::new_v4().to_string()
        } else {
            step.call().operation_id().into()
        };
        let tool = if mode == "wrong-tool" {
            "tutor/get_pending_alerts"
        } else {
            step.call().tool_id()
        };
        let args = if mode == "wrong-args" {
            json!({"conversation_id":"foreign"})
        } else {
            step.call().arguments().clone()
        };
        f.store
            .journal_mut()
            .append_turn("other-turn", br#"{"user":"fixture"}"#)
            .unwrap();
        f.store
            .journal_mut()
            .prepare_operation(OperationRequest {
                id: &id,
                turn_id: Some(if mode == "wrong-turn" {
                    "other-turn"
                } else {
                    &f.turn
                }),
                tool,
                arguments: &args,
                idempotency_key: if mode == "wrong-key" {
                    Some("unexpected")
                } else {
                    step.call().idempotency_key()
                },
                mutating: true,
            })
            .unwrap();
        f.store.journal_mut().mark_dispatched(&id).unwrap();
        f.store
            .journal_mut()
            .complete_operation(
                &id,
                &serde_json::to_vec(&envelope(f.payloads["get_conversation_context"].clone()))
                    .unwrap(),
            )
            .unwrap();
        let source = f
            .store
            .journal()
            .content_ref(ContentKind::OperationResult, &id)
            .unwrap();
        assert_eq!(
            f.accept(&mut machine, &source),
            Err(Error::Receipt),
            "{mode}"
        );
    }
}

#[test]
fn model_event_or_operation_arguments_are_never_receipt_authority() {
    for kind in [ContentKind::Event, ContentKind::OperationArguments] {
        let mut f = Fixture::new();
        let mut machine = f.begin();
        let step = f.issue(&mut machine);
        let id = step.call().operation_id().to_owned();
        let source = if kind == ContentKind::Event {
            f.store
                .journal_mut()
                .append_event(
                    &id,
                    Some(&f.turn),
                    "harness_model_action",
                    &serde_json::to_vec(&envelope(f.payloads["get_conversation_context"].clone()))
                        .unwrap(),
                )
                .unwrap();
            f.store.journal().content_ref(kind, &id).unwrap()
        } else {
            f.finish_with(
                step,
                envelope(f.payloads["get_conversation_context"].clone()),
            );
            f.store.journal().content_ref(kind, &id).unwrap()
        };
        assert_eq!(f.accept(&mut machine, &source), Err(Error::Receipt));
    }
}

#[test]
fn model_text_cannot_create_host_context_authority() {
    let mut f = Fixture::new();
    let content = f.store.journal().resolve_content(&f.host).unwrap();
    f.store
        .journal_mut()
        .append_event(
            "model-host",
            Some(&f.turn),
            "harness_model_action",
            &content,
        )
        .unwrap();
    let source = f
        .store
        .journal()
        .content_ref(ContentKind::Event, "model-host")
        .unwrap();
    assert!(matches!(
        Workflow::begin(&mut f.store, &f.turn, &source, EPOCH, Intent::Auto),
        Err(Error::Scope)
    ));
}

#[test]
fn same_owner_other_acp_session_is_not_the_same_authority() {
    let mut first = Fixture::new();
    let mut second = Fixture::new();
    let mut machine = first.begin();
    let step = first.issue(&mut machine);
    let source = first.finish_with(
        step,
        envelope(first.payloads["get_conversation_context"].clone()),
    );
    assert_eq!(
        machine.accept_result(
            &mut second.store,
            &second.catalog,
            &first.host,
            EPOCH,
            &source
        ),
        Err(Error::Scope)
    );
    assert_eq!(machine.phase(), Phase::Stopped);
}

#[test]
fn foreign_owner_cannot_load_the_journal_or_supply_a_result() {
    let mut f = Fixture::new();
    let mut machine = f.begin();
    let step = f.issue(&mut machine);
    let id = step.call().operation_id().to_owned();
    let root = tempfile::tempdir().unwrap();
    let mut foreign =
        SessionStore::create(root.path(), &owner("foreign-account"), &"0".repeat(64)).unwrap();
    foreign
        .journal_mut()
        .append_turn(&f.turn, br#"{"user":"fixture"}"#)
        .unwrap();
    foreign
        .journal_mut()
        .prepare_operation(OperationRequest {
            id: &id,
            turn_id: Some(&f.turn),
            tool: step.call().tool_id(),
            arguments: step.call().arguments(),
            idempotency_key: None,
            mutating: true,
        })
        .unwrap();
    foreign.journal_mut().mark_dispatched(&id).unwrap();
    let bytes =
        serde_json::to_vec(&envelope(f.payloads["get_conversation_context"].clone())).unwrap();
    foreign
        .journal_mut()
        .complete_operation(&id, &bytes)
        .unwrap();
    let source = foreign
        .journal()
        .content_ref(ContentKind::OperationResult, &id)
        .unwrap();
    f.finish_with(step, serde_json::from_slice(&bytes).unwrap());
    assert_eq!(f.accept(&mut machine, &source), Err(Error::Receipt));
    assert!(SessionStore::load(
        f._root.path(),
        &owner("foreign-account"),
        f.store.id(),
        &"0".repeat(64)
    )
    .is_err());
}

#[test]
fn current_host_or_catalog_epoch_change_revokes_the_machine() {
    for mode in ["host", "epoch", "finished", "cancel"] {
        let mut f = Fixture::new();
        let mut machine = f.begin();
        let mut host = f.host.clone();
        let mut epoch = EPOCH;
        match mode {
            "host" => {
                let bytes = f.store.journal().resolve_content(&host).unwrap();
                f.store
                    .journal_mut()
                    .append_event(
                        "host-refreshed",
                        Some(&f.turn),
                        "verified_host_context",
                        &bytes,
                    )
                    .unwrap();
                host = f
                    .store
                    .journal()
                    .content_ref(ContentKind::Event, "host-refreshed")
                    .unwrap();
            }
            "epoch" => epoch = "new-catalog-epoch",
            "finished" => f
                .store
                .finish_turn(&f.turn, Completion::Interrupted)
                .unwrap(),
            _ => machine.stop(&mut f.store).unwrap(),
        }
        assert!(
            matches!(
                machine.authorize_next(&mut f.store, &f.catalog, &host, epoch),
                Err(Error::Scope)
            ),
            "{mode}"
        );
        assert_eq!(machine.phase(), Phase::Stopped);
    }
}

#[test]
fn schema_change_while_waiting_invalidates_the_receipt() {
    let mut f = Fixture::new();
    let mut machine = f.begin();
    let step = f.issue(&mut machine);
    let source = f.finish_with(
        step,
        envelope(f.payloads["get_conversation_context"].clone()),
    );
    let mut changed = entries();
    changed
        .iter_mut()
        .find(|entry| entry.name == "get_conversation_context")
        .unwrap()
        .input_schema["properties"]["new_field"] = json!({"type":"string"});
    f.catalog
        .replace_server_tools(EPOCH, "tutor", changed)
        .unwrap();
    assert_eq!(f.accept(&mut machine, &source), Err(Error::Scope));
}

#[test]
fn no_second_pending_call_and_no_fresh_keys_after_restart() {
    let mut f = Fixture::new();
    let mut machine = f.begin();
    let step = f.issue(&mut machine);
    assert!(matches!(
        machine.authorize_next(&mut f.store, &f.catalog, &f.host, EPOCH),
        Err(Error::Pending)
    ));
    let root = f._root.path().to_owned();
    let session_id = f.store.id().to_owned();
    let host = f.host.clone();
    let turn = f.turn.clone();
    let catalog = f.catalog;
    drop(step);
    drop(f.store);
    let mut reopened =
        SessionStore::load(&root, &owner("account-test"), &session_id, &"0".repeat(64)).unwrap();
    assert!(matches!(
        machine.authorize_next(&mut reopened, &catalog, &host, EPOCH),
        Err(Error::Scope)
    ));
    assert!(matches!(
        Workflow::begin(&mut reopened, &turn, &host, EPOCH, Intent::Auto),
        Err(Error::RecoveryRequired)
    ));
}

#[test]
fn unknown_mutation_blocks_new_authority_instead_of_creating_retry() {
    let mut f = Fixture::new();
    let mut machine = f.begin();
    let step = f.issue(&mut machine);
    let id = step.call().operation_id().to_owned();
    let (call, permit) = step.into_parts();
    executor::prepare(f.store.journal_mut(), &f.turn, call, Some(permit)).unwrap();
    f.store.journal_mut().mark_dispatched(&id).unwrap();
    f.store.journal_mut().mark_unknown(&id).unwrap();
    assert!(matches!(
        machine.authorize_next(&mut f.store, &f.catalog, &f.host, EPOCH),
        Err(Error::RecoveryRequired)
    ));
    assert_eq!(machine.phase(), Phase::Stopped);
}

#[test]
fn review_intent_is_host_owned_and_never_overridden_by_the_model() {
    let mut f = Fixture::new();
    let mut machine =
        Workflow::begin(&mut f.store, &f.turn, &f.host, EPOCH, Intent::Review).unwrap();
    f.reach(&mut machine, Phase::SessionOpen);
    let step = f.issue(&mut machine);
    assert_eq!(
        step.call().arguments(),
        &json!({"domain_id":"domain_h07","intent":"review"})
    );
    // The auto receipt is not acceptable for the current explicit review.
    let source = f.finish_with(step, envelope(f.payloads["get_next_activity"].clone()));
    assert_eq!(f.accept(&mut machine, &source), Err(Error::Scope));
}

#[test]
fn absent_duplicate_and_null_outcomes_are_not_confused() {
    for mode in ["missing", "duplicate", "null"] {
        let mut f = Fixture::new();
        let mut competency =
            f.payloads["get_curriculum_snapshot"]["snapshot"]["concepts"][0].clone();
        match mode {
            "missing" => {
                competency.as_object_mut().unwrap().remove("outcomes");
            }
            "duplicate" => {
                let outcome = competency["outcomes"][0].clone();
                competency["outcomes"].as_array_mut().unwrap().push(outcome);
            }
            _ => competency["outcomes"] = Value::Null,
        }
        f.payloads["get_curriculum_snapshot"]["snapshot"]["concepts"][0] = competency.clone();
        f.payloads["get_next_activity"]["pedagogical_contract"]["competency"] = competency;
        let mut machine = f.begin();
        f.reach(&mut machine, Phase::SessionOpen);
        let step = f.issue(&mut machine);
        let source = f.finish_with(step, envelope(f.payloads["get_next_activity"].clone()));
        assert_eq!(
            f.accept(&mut machine, &source).is_ok(),
            mode == "null",
            "{mode}"
        );
    }
}

#[test]
fn changed_hash_never_resolves_as_the_same_host_authority() {
    let mut f = Fixture::new();
    let mut machine = f.begin();
    // Reuse the public event ID in another same-owner session with DIFFERENT
    // content. ContentRef binding alone has no session UUID; instance/session
    // checks plus the hash/expected operation links must still refuse it.
    let root = tempfile::tempdir().unwrap();
    let mut foreign =
        SessionStore::create(root.path(), &owner("account-test"), &"0".repeat(64)).unwrap();
    let foreign_turn = foreign.begin_turn("Another synthetic session.").unwrap();
    let mut body: Value =
        serde_json::from_slice(&f.store.journal().resolve_content(&f.host).unwrap()).unwrap();
    body["trainingContext"]["revision"] = json!("different-revision");
    foreign
        .journal_mut()
        .append_event(
            "host-context",
            Some(&foreign_turn),
            "verified_host_context",
            &serde_json::to_vec(&body).unwrap(),
        )
        .unwrap();
    let foreign_ref = foreign
        .journal()
        .content_ref(ContentKind::Event, "host-context")
        .unwrap();
    assert_eq!(foreign_ref.id(), f.host.id());
    assert_ne!(foreign_ref.sha256(), f.host.sha256());
    assert!(matches!(
        machine.authorize_next(&mut f.store, &f.catalog, &foreign_ref, EPOCH),
        Err(Error::Scope)
    ));
}

fn terminal_envelope(name: &str) -> Value {
    serde_json::from_str::<Value>(include_str!("../fixtures/h07-receipts.json")).unwrap()
        ["terminal_envelopes"][name]
        .clone()
}

#[test]
fn rest_and_close_session_are_terminal_server_guidance_without_a_frozen_decision() {
    for (name, kind) in [
        ("REST", TerminalKind::Rest),
        ("CLOSE_SESSION", TerminalKind::CloseSession),
    ] {
        let mut f = Fixture::new();
        let mut machine = f.begin();
        f.reach(&mut machine, Phase::SessionOpen);
        let step = f.issue(&mut machine);
        let source = f.finish_with(step, terminal_envelope(name));
        f.accept(&mut machine, &source).unwrap();
        assert_eq!(machine.phase(), Phase::Terminal(kind));
        assert!(matches!(
            machine.decision(&f.store, &f.host, EPOCH),
            Err(Error::Phase)
        ));
        let receipt = machine.terminal(&f.store, &f.host, EPOCH).unwrap();
        assert_eq!(receipt.kind(), kind);
        assert_eq!(receipt.activity()["type"], name);
        assert_eq!(receipt.activity()["concept"], "");
        assert!(receipt.contract().get("decision_id").is_none());
        assert_eq!(receipt.source().id(), source.id());
        let before = f
            .store
            .journal()
            .event_refs(Some(&f.turn), None, 100)
            .unwrap()
            .len();
        assert!(matches!(
            machine.authorize_next(&mut f.store, &f.catalog, &f.host, EPOCH),
            Err(Error::Phase)
        ));
        assert_eq!(
            before,
            f.store
                .journal()
                .event_refs(Some(&f.turn), None, 100)
                .unwrap()
                .len()
        );
        // Even the server's literal instruction to record_session_close cannot
        // turn terminal guidance into a permit for a further mutation.
        assert_eq!(machine.phase(), Phase::Terminal(kind));
    }
}

#[test]
fn terminal_receipts_still_require_matching_scope_and_no_fabricated_decision() {
    for (pointer, value) in [
        ("/session_id", json!("foreign")),
        ("/domain_id", json!("foreign")),
        ("/intent", json!("review")),
        ("/decision_id", json!("fabricated")),
        (
            "/pedagogical_contract/recommended_activity_type",
            json!("PRACTICE"),
        ),
        (
            "/pedagogical_contract/bkt_update_mode",
            json!("learning_opportunity"),
        ),
    ] {
        let mut f = Fixture::new();
        let mut machine = f.begin();
        f.reach(&mut machine, Phase::SessionOpen);
        let step = f.issue(&mut machine);
        let mut body = terminal_envelope("REST")["structuredContent"].clone();
        *body.pointer_mut(pointer).unwrap() = value;
        let source = f.finish_with(step, envelope(body));
        assert!(f.accept(&mut machine, &source).is_err(), "{pointer}");
        assert!(machine.terminal.is_none());
    }
    for key in [
        "decision_id",
        "policy_version",
        "curriculum_version",
        "competency",
    ] {
        let mut f = Fixture::new();
        let mut machine = f.begin();
        f.reach(&mut machine, Phase::SessionOpen);
        let step = f.issue(&mut machine);
        let mut body = terminal_envelope("CLOSE_SESSION")["structuredContent"].clone();
        body["pedagogical_contract"][key] = json!("invented");
        let source = f.finish_with(step, envelope(body));
        assert_eq!(
            f.accept(&mut machine, &source),
            Err(Error::Contract),
            "{key}"
        );
    }
}

#[test]
fn review_without_reviewable_concept_stops_without_starting_an_assessment() {
    let mut f = Fixture::new();
    let mut machine =
        Workflow::begin(&mut f.store, &f.turn, &f.host, EPOCH, Intent::Review).unwrap();
    f.reach(&mut machine, Phase::SessionOpen);
    let step = f.issue(&mut machine);
    let mut body = terminal_envelope("REST")["structuredContent"].clone();
    body["intent"] = json!("review");
    body["intent_status"] = json!("no_reviewable_concept");
    body["pedagogical_contract"]["intent"] = json!("review_prior_material");
    let source = f.finish_with(step, envelope(body));
    f.accept(&mut machine, &source).unwrap();
    assert_eq!(machine.phase(), Phase::Terminal(TerminalKind::Rest));
    assert!(matches!(
        machine.authorize_next(&mut f.store, &f.catalog, &f.host, EPOCH),
        Err(Error::Phase)
    ));
}

// P1 regressions exercise the actual executor boundary with synthetic receipts.
// No model inference, network, or live Tutor server is used.

#[test]
fn p1_stopped_workflow_rejects_previously_issued_permit() {
    let mut f = Fixture::new();
    let mut machine = f.begin();
    let (call, permit) = f.issue(&mut machine).into_parts();
    machine.stop(&mut f.store).unwrap();
    let result = executor::prepare(f.store.journal_mut(), &f.turn, call, Some(permit));
    assert!(
        matches!(result, Err(executor::Error::Preconditions)),
        "P1: executor accepted a retired permit"
    );
}

#[test]
fn p1_finished_turn_rejects_previously_issued_permit() {
    let mut f = Fixture::new();
    let mut machine = f.begin();
    let (call, permit) = f.issue(&mut machine).into_parts();
    f.store
        .finish_turn(&f.turn, Completion::Interrupted)
        .unwrap();
    let result = executor::prepare(f.store.journal_mut(), &f.turn, call, Some(permit));
    assert!(
        matches!(result, Err(executor::Error::Preconditions)),
        "P1: executor accepted a finished turn"
    );
}

#[test]
fn p1_stopping_rejects_already_prepared_operation() {
    let mut f = Fixture::new();
    let mut machine = f.begin();
    let (call, permit) = f.issue(&mut machine).into_parts();
    let id = call.operation_id().to_owned();
    executor::prepare(f.store.journal_mut(), &f.turn, call, Some(permit)).unwrap();
    machine.stop(&mut f.store).unwrap();
    assert!(
        f.store.journal().operation(&id).unwrap().unwrap().state == OperationState::Rejected,
        "P1: operation is still Prepared after workflow stop"
    );
}

async fn assert_no_transport(f: &mut Fixture, prepared: executor::PreparedCall) {
    use crate::mcp_client;
    use std::sync::atomic::AtomicBool;
    struct CountingTransport {
        calls: usize,
        payload: Value,
    }
    impl mcp_client::Transport for CountingTransport {
        fn server_id(&self) -> &str {
            "tutor"
        }
        async fn call_tool(
            &mut self,
            _: &str,
            _: Value,
            _: Option<&str>,
            _: &AtomicBool,
        ) -> std::result::Result<Value, mcp_client::Error> {
            self.calls += 1;
            Ok(self.payload.clone())
        }
    }
    // A turn can finish or the workflow can retire independently of an ACP
    // cancellation flag. A legitimate old allow response is not fresh scope.
    let mut transport = CountingTransport {
        calls: 0,
        payload: envelope(f.payloads["get_conversation_context"].clone()),
    };
    let result = prepared
        .execute(
            f.store.journal_mut(),
            &f.catalog,
            EPOCH,
            &json!({"outcome":{"outcome":"selected","optionId":"allow"}}),
            &mut transport,
            &AtomicBool::new(false),
        )
        .await;
    assert!(result.is_err(), "revoked authority must fail dispatch");
    assert_eq!(
        transport.calls, 0,
        "P1: mock transport was called with retired authority"
    );
}

fn prepared_step(f: &mut Fixture, machine: &mut Workflow) -> executor::PreparedCall {
    let (call, permit) = f.issue(machine).into_parts();
    match executor::prepare(f.store.journal_mut(), &f.turn, call, Some(permit)).unwrap() {
        Preparation::Ready(prepared) => *prepared,
        _ => panic!("fixture expected a new prepared operation"),
    }
}

#[tokio::test]
async fn p1_prepared_dispatch_after_stop_makes_no_transport_call() {
    let mut f = Fixture::new();
    let mut machine = f.begin();
    let prepared = prepared_step(&mut f, &mut machine);
    machine.stop(&mut f.store).unwrap();
    assert_no_transport(&mut f, prepared).await;
}

#[test]
fn p1_dropping_workflow_rejects_issued_permit_and_reconstruction() {
    let mut f = Fixture::new();
    let mut machine = f.begin();
    let anchor = machine.anchor.clone();
    let (call, permit) = f.issue(&mut machine).into_parts();
    let id = call.operation_id().to_owned();
    drop(machine);
    assert!(matches!(
        executor::prepare(f.store.journal_mut(), &f.turn, call, Some(permit)),
        Err(executor::Error::Preconditions)
    ));
    assert!(f.store.journal().operation(&id).unwrap().is_none());
    assert!(WorkflowAuthority::new(f.store.journal_mut(), &f.turn, &anchor).is_err());
}

#[tokio::test]
async fn p1_dropping_workflow_before_dispatch_makes_no_transport_call() {
    let mut f = Fixture::new();
    let mut machine = f.begin();
    let prepared = prepared_step(&mut f, &mut machine);
    drop(machine);
    assert_no_transport(&mut f, prepared).await;
}

const FAIL_REVOCATION: &str = "CREATE TRIGGER fail_h07_revocation BEFORE INSERT ON events
    WHEN NEW.id LIKE '%:revoked' BEGIN SELECT RAISE(ABORT,'synthetic revocation failure'); END;";

#[tokio::test]
async fn p1_failed_durable_revocation_still_retires_prepared_authority() {
    let mut f = Fixture::with_sql_setup(FAIL_REVOCATION);
    let mut machine = f.begin();
    let prepared = prepared_step(&mut f, &mut machine);
    let id = machine.pending.as_ref().unwrap().operation.clone();
    assert_eq!(machine.stop(&mut f.store), Err(Error::Storage));
    assert_eq!(machine.phase(), Phase::Stopped);
    assert!(f
        .store
        .journal()
        .event(&format!("{}:revoked", machine.anchor.id()))
        .unwrap()
        .is_none());
    assert_eq!(
        f.store.journal().operation(&id).unwrap().unwrap().state,
        OperationState::Rejected
    );
    assert_no_transport(&mut f, prepared).await;
}

#[test]
fn p1_failed_durable_stop_is_not_hidden_by_receipt_or_scope_error() {
    for accept in [false, true] {
        let mut f = Fixture::with_sql_setup(FAIL_REVOCATION);
        let mut machine = f.begin();
        let (call, permit) = f.issue(&mut machine).into_parts();
        let id = call.operation_id().to_owned();
        if accept {
            let wrong_receipt = f.host.clone();
            assert_eq!(f.accept(&mut machine, &wrong_receipt), Err(Error::Storage));
        } else {
            assert!(matches!(
                machine.authorize_next(&mut f.store, &f.catalog, &f.host, "changed-epoch"),
                Err(Error::Storage)
            ));
        }
        assert_eq!(machine.phase(), Phase::Stopped);
        assert!(matches!(
            executor::prepare(f.store.journal_mut(), &f.turn, call, Some(permit)),
            Err(executor::Error::Preconditions)
        ));
        assert!(f.store.journal().operation(&id).unwrap().is_none());
    }
}

#[tokio::test]
async fn p1_failed_prepared_cleanup_retains_pending_identity_and_revokes_dispatch() {
    let mut f = Fixture::with_sql_setup(
        "CREATE TRIGGER fail_h07_cleanup BEFORE INSERT ON transitions
        WHEN NEW.to_state='rejected' BEGIN SELECT RAISE(ABORT,'synthetic cleanup failure'); END;",
    );
    let mut machine = f.begin();
    let prepared = prepared_step(&mut f, &mut machine);
    let id = machine.pending.as_ref().unwrap().operation.clone();
    for _ in 0..2 {
        assert_eq!(machine.stop(&mut f.store), Err(Error::Storage));
        assert_eq!(machine.phase(), Phase::Stopped);
        assert_eq!(machine.pending.as_ref().unwrap().operation, id);
        assert_eq!(
            f.store.journal().operation(&id).unwrap().unwrap().state,
            OperationState::Prepared
        );
        assert!(f
            .store
            .journal()
            .event(&format!("{}:revoked", machine.anchor.id()))
            .unwrap()
            .is_some());
    }
    assert_no_transport(&mut f, prepared).await;
}

#[test]
fn p1_stop_preserves_dispatched_unknown_and_completed_operations() {
    for state in [
        OperationState::Dispatched,
        OperationState::Unknown,
        OperationState::Completed,
    ] {
        let mut f = Fixture::new();
        let mut machine = f.begin();
        let _prepared = prepared_step(&mut f, &mut machine);
        let id = machine.pending.as_ref().unwrap().operation.clone();
        f.store.journal_mut().mark_dispatched(&id).unwrap();
        match state {
            OperationState::Unknown => f.store.journal_mut().mark_unknown(&id).unwrap(),
            OperationState::Completed => f
                .store
                .journal_mut()
                .complete_operation(&id, b"{}")
                .unwrap(),
            _ => (),
        }
        let before = f.store.journal().operation(&id).unwrap().unwrap();
        machine.stop(&mut f.store).unwrap();
        let after = f.store.journal().operation(&id).unwrap().unwrap();
        assert_eq!(after.state, before.state);
        assert_eq!(after.result, before.result);
        assert_eq!(machine.pending.as_ref().unwrap().operation, id);
    }
}

#[test]
fn p1_stop_never_rejects_a_different_prepared_operation() {
    for changed in ["turn", "tool", "arguments", "key"] {
        let mut f = Fixture::new();
        let mut machine = f.begin();
        let step = f.issue(&mut machine);
        let call = step.call();
        let id = call.operation_id().to_owned();
        f.store
            .journal_mut()
            .append_turn("other-turn", b"{}")
            .unwrap();
        let wrong_args = json!({"conversation_id":"other-conversation"});
        f.store
            .journal_mut()
            .prepare_operation(OperationRequest {
                id: &id,
                turn_id: Some(if changed == "turn" {
                    "other-turn"
                } else {
                    &f.turn
                }),
                tool: if changed == "tool" {
                    "tutor/other_tool"
                } else {
                    call.tool_id()
                },
                arguments: if changed == "arguments" {
                    &wrong_args
                } else {
                    call.arguments()
                },
                idempotency_key: if changed == "key" {
                    Some("foreign-key")
                } else {
                    call.idempotency_key()
                },
                mutating: true,
            })
            .unwrap();
        assert_eq!(machine.stop(&mut f.store), Err(Error::Scope), "{changed}");
        assert_eq!(
            f.store.journal().operation(&id).unwrap().unwrap().state,
            OperationState::Prepared,
            "{changed}"
        );
        assert_eq!(machine.pending.as_ref().unwrap().operation, id);
    }
}

async fn assert_post_dispatch_storage_failure(transport_fails: bool) {
    use crate::mcp_client;
    use std::sync::atomic::AtomicBool;

    struct PostDispatchTransport {
        calls: usize,
        key: Option<String>,
        reply: std::result::Result<Value, mcp_client::Error>,
    }
    impl mcp_client::Transport for PostDispatchTransport {
        fn server_id(&self) -> &str {
            "tutor"
        }
        async fn call_tool(
            &mut self,
            name: &str,
            arguments: Value,
            key: Option<&str>,
            _: &AtomicBool,
        ) -> std::result::Result<Value, mcp_client::Error> {
            self.calls += 1;
            assert_eq!(name, "start_learning_session");
            assert_eq!(arguments, json!({"domain_id":"domain_h07"}));
            assert_eq!(key, self.key.as_deref());
            self.reply.clone()
        }
    }

    // Do not fault the two preceding context reads. The completed-transition
    // fault rolls back the result INSERT as well as the operation update.
    let failed_states = if transport_fails {
        "'unknown'"
    } else {
        "'completed','unknown'"
    };
    let mut f = Fixture::with_sql_setup(&format!(
        "CREATE TRIGGER fail_h07_post_dispatch BEFORE INSERT ON transitions
         WHEN NEW.to_state IN ({failed_states}) AND
         (SELECT tool FROM operations WHERE id=NEW.operation_id)='tutor/start_learning_session'
         BEGIN SELECT RAISE(ABORT,'synthetic post-dispatch persistence failure'); END;"
    ));
    let mut machine = f.begin();
    f.reach(&mut machine, Phase::CurriculumConfirmed);
    let prepared = prepared_step(&mut f, &mut machine);
    let id = machine.pending.as_ref().unwrap().operation.clone();
    let original = f.store.journal().operation(&id).unwrap().unwrap();
    assert!(original.mutating);
    assert!(original.idempotency_key.is_some());
    let mut transport = PostDispatchTransport {
        calls: 0,
        key: original.idempotency_key.clone(),
        reply: if transport_fails {
            Err(mcp_client::Error::Call)
        } else {
            Ok(envelope(f.payloads["start_learning_session"].clone()))
        },
    };
    let result = prepared
        .execute(
            f.store.journal_mut(),
            &f.catalog,
            EPOCH,
            &json!({"outcome":{"outcome":"selected","optionId":"allow"}}),
            &mut transport,
            &AtomicBool::new(false),
        )
        .await;
    assert_eq!(
        result.err(),
        Some(if transport_fails {
            executor::Error::UnknownOutcome
        } else {
            executor::Error::Storage
        })
    );
    assert_eq!(transport.calls, 1);
    let dispatched = f.store.journal().operation(&id).unwrap().unwrap();
    assert_eq!(dispatched.state, OperationState::Dispatched);
    assert!(dispatched.result.is_none());
    assert!(f
        .store
        .journal()
        .content_ref(ContentKind::OperationResult, &id)
        .is_err());
    assert!(f.store.journal().has_unresolved_mutations().unwrap());

    machine.stop(&mut f.store).unwrap();
    assert_eq!(machine.phase(), Phase::Stopped);
    assert_eq!(machine.pending.as_ref().unwrap().operation, id);
    assert!(f.store.journal().operation(&id).unwrap().unwrap() == dispatched);
    assert!(f.store.journal().has_unresolved_mutations().unwrap());
    assert_eq!(
        f.store
            .journal()
            .transitions(&id)
            .unwrap()
            .iter()
            .map(|entry| entry.to)
            .collect::<Vec<_>>(),
        vec![OperationState::Prepared, OperationState::Dispatched]
    );

    // A fresh turn and verified host receipt avoid a false positive from the
    // stopped workflow or its existing anchor: unresolved IO is the blocker.
    f.store
        .finish_turn(&f.turn, Completion::Interrupted)
        .unwrap();
    let next_turn = f.store.begin_turn("Nouvelle demande synthétique.").unwrap();
    let host_bytes = f.store.journal().resolve_content(&f.host).unwrap();
    let next_host_id = format!("{next_turn}:host");
    f.store
        .journal_mut()
        .append_event(
            &next_host_id,
            Some(&next_turn),
            "verified_host_context",
            &host_bytes,
        )
        .unwrap();
    let next_host = f
        .store
        .journal()
        .content_ref(ContentKind::Event, &next_host_id)
        .unwrap();
    assert!(matches!(
        Workflow::begin(&mut f.store, &next_turn, &next_host, EPOCH, Intent::Auto),
        Err(Error::RecoveryRequired)
    ));
    assert!(f
        .store
        .journal()
        .event(&format!("{next_turn}:h07"))
        .unwrap()
        .is_none());
    assert_eq!(transport.calls, 1);

    drop(machine);
    // Closing releases the exclusive SQLite lock. Remove the injected fault
    // before reopening so recovery can durably record Dispatched -> Unknown.
    let mut f = f.reopen_with_sql("DROP TRIGGER fail_h07_post_dispatch;");
    assert_eq!(f.store.journal().recovered_unknown_count(), 1);
    let recovered = f.store.journal().operation(&id).unwrap().unwrap();
    assert_eq!(recovered.state, OperationState::Unknown);
    assert_eq!(recovered.turn_id, original.turn_id);
    assert_eq!(recovered.tool, original.tool);
    assert_eq!(recovered.canonical_arguments, original.canonical_arguments);
    assert_eq!(recovered.idempotency_key, original.idempotency_key);
    assert!(recovered.result.is_none());
    assert!(f.store.journal().has_unresolved_mutations().unwrap());
    assert_eq!(
        f.store
            .journal()
            .transitions(&id)
            .unwrap()
            .iter()
            .map(|entry| entry.to)
            .collect::<Vec<_>>(),
        vec![
            OperationState::Prepared,
            OperationState::Dispatched,
            OperationState::Unknown
        ]
    );
    assert!(matches!(
        Workflow::begin(&mut f.store, &next_turn, &next_host, EPOCH, Intent::Auto),
        Err(Error::RecoveryRequired)
    ));
    assert_eq!(
        transport.calls, 1,
        "recovery must not re-execute the transport"
    );
}

#[tokio::test]
async fn p1_transport_failure_and_failed_mark_unknown_remain_dispatched_until_reopen() {
    assert_post_dispatch_storage_failure(true).await;
}

#[tokio::test]
async fn p1_success_reply_and_failed_complete_and_unknown_recover_without_reexecution() {
    assert_post_dispatch_storage_failure(false).await;
}
