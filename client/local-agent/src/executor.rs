// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! Durable, serial dispatch boundary. No model proposal is an authorization.
//! Writes additionally require a retained precondition receipt issued by the
//! pedagogical state machine; ordinary discovery can only inspect read tools.
//! Unknown outcomes require explicit reconciliation, never a new automatic call.

use crate::{
    call_validation::ValidatedCall,
    catalog::ToolCatalog,
    journal::{ContentKind, ContentRef, Journal, OperationRequest, OperationState},
    mcp_client::Transport,
};
use serde_json::Value;
use std::sync::atomic::{AtomicBool, Ordering};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Error {
    Preconditions,
    Storage,
    Denied,
    Cancelled,
    Stale,
    RecoveryRequired,
    UnknownOutcome,
}
impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            Self::Preconditions => "tool_preconditions_not_proven",
            Self::Storage => "tool_journal_unavailable",
            Self::Denied => "tool_permission_denied",
            Self::Cancelled => "tool_cancelled_before_dispatch",
            Self::Stale => "tool_binding_changed",
            Self::RecoveryRequired => "tool_reconciliation_required",
            Self::UnknownOutcome => "tool_outcome_unknown",
        })
    }
}
impl std::error::Error for Error {}

/// Constructed only from an immutable, code-issued journal event. The state
/// machine must check the underlying server/learner proofs BEFORE writing
/// pedagogical_precondition. Model notes must never use that reserved kind.
pub struct WorkflowPermit {
    journal_instance: uuid::Uuid,
    operation: String,
    fingerprint: String,
    turn: String,
}
impl WorkflowPermit {
    pub fn from_event(
        journal: &Journal,
        reference: &ContentRef,
        turn: &str,
        call: &ValidatedCall,
    ) -> Result<Self, Error> {
        call.check_journal(journal)
            .map_err(|_| Error::Preconditions)?;
        if reference.kind() != ContentKind::Event {
            return Err(Error::Preconditions);
        }
        let event = journal
            .event(reference.id())
            .map_err(|_| Error::Storage)?
            .ok_or(Error::Preconditions)?;
        if event.kind != "pedagogical_precondition" || event.turn_id.as_deref() != Some(turn) {
            return Err(Error::Preconditions);
        }
        let bytes = journal
            .resolve_content(reference)
            .map_err(|_| Error::Storage)?;
        let value: Value = serde_json::from_slice(&bytes).map_err(|_| Error::Preconditions)?;
        let expected_phase = match call.name() {
            "get_conversation_context" => "context_requested",
            "get_curriculum_snapshot" | "start_learning_session" => "context_confirmed",
            "get_next_activity" => "session_open",
            "prepare_assessment_attempt" => "activity_validated",
            "submit_assessment_attempt" => "response_committed",
            "record_interaction" => "evaluation_validated",
            "record_learning_event" => "feedback_delivered",
            "cancel_assessment_attempt" => "attempt_cancel_requested",
            "record_session_close" => "session_close_requested",
            "record_affect" => "affect_reported",
            "get_memory_state" | "get_metacognitive_mirror" => "coaching_context_requested",
            _ => return Err(Error::Preconditions),
        };
        if value.get("operation_id").and_then(Value::as_str) != Some(call.operation_id())
            || value.get("fingerprint").and_then(Value::as_str) != Some(call.fingerprint())
            || value.get("tool_id").and_then(Value::as_str) != Some(call.tool_id())
            || value.get("phase").and_then(Value::as_str) != Some(expected_phase)
        {
            return Err(Error::Preconditions);
        }
        Ok(Self {
            journal_instance: journal.instance_id(),
            operation: call.operation_id().into(),
            fingerprint: call.fingerprint().into(),
            turn: turn.into(),
        })
    }
}

pub struct PreparedCall {
    call: ValidatedCall,
    turn: String,
}
pub enum Preparation {
    Ready(PreparedCall),
    Completed(Execution),
}
pub struct Execution {
    pub result: Value,
    pub reference: ContentRef,
    pub replayed: bool,
}

pub fn prepare(
    journal: &mut Journal,
    turn: &str,
    call: ValidatedCall,
    permit: Option<WorkflowPermit>,
) -> Result<Preparation, Error> {
    call.check_journal(journal)
        .map_err(|_| Error::Preconditions)?;
    if call.effect().mutating()
        && !permit.is_some_and(|p| {
            p.journal_instance == journal.instance_id()
                && p.operation == call.operation_id()
                && p.fingerprint == call.fingerprint()
                && p.turn == turn
        })
    {
        return Err(Error::Preconditions);
    }
    if call.effect().mutating()
        && journal
            .has_unresolved_mutations()
            .map_err(|_| Error::Storage)?
    {
        return Err(Error::RecoveryRequired);
    }
    let op = journal
        .prepare_operation(OperationRequest {
            id: call.operation_id(),
            turn_id: Some(turn),
            tool: call.tool_id(),
            arguments: call.arguments(),
            idempotency_key: call.idempotency_key(),
            mutating: call.effect().mutating(),
        })
        .map_err(|_| Error::Storage)?;
    match op.state {
        OperationState::Prepared => Ok(Preparation::Ready(PreparedCall {
            call,
            turn: turn.into(),
        })),
        OperationState::Completed => {
            let reference = journal
                .content_ref(ContentKind::OperationResult, &op.id)
                .map_err(|_| Error::Storage)?;
            let bytes = journal
                .resolve_content(&reference)
                .map_err(|_| Error::Storage)?;
            let result = serde_json::from_slice(&bytes).map_err(|_| Error::Storage)?;
            Ok(Preparation::Completed(Execution {
                result,
                reference,
                replayed: true,
            }))
        }
        OperationState::Rejected => Err(Error::Denied),
        OperationState::Dispatched | OperationState::Unknown => Err(Error::RecoveryRequired),
    }
}

impl PreparedCall {
    pub fn operation_id(&self) -> &str {
        self.call.operation_id()
    }
    pub fn permission_params(&self, session: &str) -> Value {
        self.call
            .permission_params(session, self.call.operation_id())
    }

    /// `response` must be the result of the matching host ACP permission RPC,
    /// delivered by the event loop, never parsed from assistant text.
    pub async fn execute<T: Transport>(
        self,
        journal: &mut Journal,
        catalog: &ToolCatalog,
        binding: &str,
        response: &Value,
        transport: &mut T,
        cancel: &AtomicBool,
    ) -> Result<Execution, Error> {
        self.call
            .check_journal(journal)
            .map_err(|_| Error::Preconditions)?;
        if cancel.load(Ordering::Acquire) {
            journal
                .reject_prepared(self.call.operation_id())
                .map_err(|_| Error::Storage)?;
            return Err(Error::Cancelled);
        }
        let allowed = response.pointer("/outcome/outcome").and_then(Value::as_str)
            == Some("selected")
            && response
                .pointer("/outcome/optionId")
                .and_then(Value::as_str)
                == Some("allow");
        if !allowed {
            journal
                .reject_prepared(self.call.operation_id())
                .map_err(|_| Error::Storage)?;
            return Err(Error::Denied);
        }
        if self.call.check_current(catalog, binding).is_err() || transport.server_id() != "tutor" {
            journal
                .reject_prepared(self.call.operation_id())
                .map_err(|_| Error::Storage)?;
            return Err(Error::Stale);
        }
        if self.call.effect().mutating()
            && journal
                .has_unresolved_mutations()
                .map_err(|_| Error::Storage)?
        {
            return Err(Error::RecoveryRequired);
        }
        let recorded = journal
            .operation(self.call.operation_id())
            .map_err(|_| Error::Storage)?
            .ok_or(Error::Storage)?;
        if recorded.turn_id.as_deref() != Some(self.turn.as_str())
            || recorded.tool != self.call.tool_id()
            || recorded.mutating != self.call.effect().mutating()
            || recorded.idempotency_key.as_deref() != self.call.idempotency_key()
            || recorded.canonical_arguments
                != crate::journal::canonical_arguments(self.call.arguments())
                    .map_err(|_| Error::Storage)?
        {
            return Err(Error::Preconditions);
        }
        // The commit is the last boundary before IO. No request may be sent
        // if the durable Prepared→Dispatched transition fails.
        let permit = journal
            .mark_dispatched(self.call.operation_id())
            .map_err(|_| Error::Storage)?;
        let op = permit.into_operation();
        let args: Value =
            serde_json::from_slice(&op.canonical_arguments).map_err(|_| Error::Storage)?;
        let reply = transport
            .call_tool(
                self.call.name(),
                args,
                op.idempotency_key.as_deref(),
                cancel,
            )
            .await;
        let result = match reply {
            Ok(result) => result,
            Err(_) => {
                let _ = journal.mark_unknown(&op.id);
                return Err(Error::UnknownOutcome);
            }
        };
        let malformed =
            !result.is_object() || result.get("isError").is_some_and(|v| !v.is_boolean());
        let business_error = result.get("isError").and_then(Value::as_bool) == Some(true);
        if malformed || (op.mutating && business_error) {
            // Preserve the exact error without interpreting it as proof that
            // a write was rolled back. A later reconciler may inspect it.
            let bytes = serde_json::to_vec(&result).map_err(|_| Error::Storage)?;
            let stored = journal.append_event(
                &format!("{}:uncertain", op.id),
                Some(&self.turn),
                "mcp_uncertain_reply",
                &bytes,
            );
            let _ = journal.mark_unknown(&op.id);
            stored.map_err(|_| Error::Storage)?;
            return Err(Error::UnknownOutcome);
        }
        let bytes = serde_json::to_vec(&result).map_err(|_| Error::Storage)?;
        if journal.complete_operation(&op.id, &bytes).is_err() {
            let _ = journal.mark_unknown(&op.id);
            return Err(Error::Storage);
        }
        // If cancellation arrived just after a complete reply, retain that
        // receipt as completed: local cancellation cannot undo a server write.
        let reference = journal
            .content_ref(ContentKind::OperationResult, &op.id)
            .map_err(|_| Error::Storage)?;
        Ok(Execution {
            result,
            reference,
            replayed: false,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        call_validation::{validate_call, VerifiedFields},
        catalog::ToolEntry,
        journal::Binding,
        mcp_client,
    };
    use serde_json::json;
    struct FakeTransport {
        calls: usize,
        result: Result<Value, mcp_client::Error>,
    }
    impl Transport for FakeTransport {
        fn server_id(&self) -> &str {
            "tutor"
        }
        async fn call_tool(
            &mut self,
            _: &str,
            _: Value,
            _: Option<&str>,
            _: &AtomicBool,
        ) -> Result<Value, mcp_client::Error> {
            self.calls += 1;
            self.result.clone()
        }
    }
    fn setup() -> (tempfile::TempDir, Journal, ToolCatalog) {
        let root = tempfile::tempdir().unwrap();
        let mut journal = Journal::open(
            &root.path().join("journal.sqlite"),
            &Binding {
                profile_id: "p",
                account_id: "a",
                tenant_id: "t",
                server_id: "s",
            },
        )
        .unwrap();
        journal
            .append_turn("turn", br#"{"user":"public fixture"}"#)
            .unwrap();
        let mut catalog = ToolCatalog::new("epoch".into()).unwrap();
        catalog
            .replace_server_tools(
                "epoch",
                "tutor",
                ["get_pending_alerts", "get_next_activity"]
                    .into_iter()
                    .map(|name| ToolEntry {
                        server_id: "tutor".into(),
                        name: name.into(),
                        description: String::new(),
                        input_schema: json!({"type":"object","additionalProperties":false}),
                        annotations: Value::Null,
                    })
                    .collect(),
            )
            .unwrap();
        (root, journal, catalog)
    }
    fn call(
        catalog: &ToolCatalog,
        fields: &VerifiedFields,
        name: &str,
        id: uuid::Uuid,
    ) -> ValidatedCall {
        validate_call(
            catalog,
            "epoch",
            &format!("tutor/{name}"),
            &json!({}),
            fields,
            id,
        )
        .unwrap()
    }
    fn ready(preparation: Preparation) -> PreparedCall {
        match preparation {
            Preparation::Ready(call) => call,
            _ => panic!("expected fresh call"),
        }
    }
    fn allow() -> Value {
        json!({"outcome":{"outcome":"selected","optionId":"allow"}})
    }
    fn workflow(journal: &mut Journal, call: &ValidatedCall) -> WorkflowPermit {
        let id = format!("{}:precondition", call.operation_id());
        journal.append_event(&id,Some("turn"),"pedagogical_precondition",&serde_json::to_vec(&json!({
            "operation_id":call.operation_id(),"fingerprint":call.fingerprint(),"tool_id":call.tool_id(),"phase":"session_open",
        })).unwrap()).unwrap();
        WorkflowPermit::from_event(
            journal,
            &journal.content_ref(ContentKind::Event, &id).unwrap(),
            "turn",
            call,
        )
        .unwrap()
    }

    #[tokio::test]
    async fn permission_and_durable_state_precede_io_and_completed_calls_are_not_repeated() {
        let (_root, mut journal, catalog) = setup();
        let fields = VerifiedFields::new("epoch".into(), &journal).unwrap();
        let mut transport = FakeTransport {
            calls: 0,
            result: Ok(json!({"content":[],"structuredContent":{"alerts":[]}})),
        };
        let cancel = AtomicBool::new(false);
        let denied_id = uuid::Uuid::new_v4();
        let denied = ready(
            prepare(
                &mut journal,
                "turn",
                call(&catalog, &fields, "get_pending_alerts", denied_id),
                None,
            )
            .unwrap(),
        );
        assert!(matches!(
            denied
                .execute(
                    &mut journal,
                    &catalog,
                    "epoch",
                    &json!({"outcome":{"outcome":"selected","optionId":"reject"}}),
                    &mut transport,
                    &cancel
                )
                .await,
            Err(Error::Denied)
        ));
        assert_eq!(transport.calls, 0);
        assert_eq!(
            journal
                .operation(&denied_id.to_string())
                .unwrap()
                .unwrap()
                .state,
            OperationState::Rejected
        );
        let id = uuid::Uuid::new_v4();
        let prepared = ready(
            prepare(
                &mut journal,
                "turn",
                call(&catalog, &fields, "get_pending_alerts", id),
                None,
            )
            .unwrap(),
        );
        assert_eq!(
            journal.operation(&id.to_string()).unwrap().unwrap().state,
            OperationState::Prepared
        );
        let result = prepared
            .execute(
                &mut journal,
                &catalog,
                "epoch",
                &allow(),
                &mut transport,
                &cancel,
            )
            .await
            .unwrap();
        assert!(!result.replayed);
        assert_eq!(transport.calls, 1);
        let replay = prepare(
            &mut journal,
            "turn",
            call(&catalog, &fields, "get_pending_alerts", id),
            None,
        )
        .unwrap();
        let Preparation::Completed(replay) = replay else {
            panic!("completed operation must not execute again")
        };
        assert!(replay.replayed);
        assert_eq!(replay.result, result.result);
        assert_eq!(transport.calls, 1);
    }

    #[tokio::test]
    async fn mutation_needs_workflow_proof_and_ambiguous_result_blocks_further_writes() {
        let (_root, mut journal, catalog) = setup();
        let fields = VerifiedFields::new("epoch".into(), &journal).unwrap();
        let id = uuid::Uuid::new_v4();
        assert!(matches!(
            prepare(
                &mut journal,
                "turn",
                call(&catalog, &fields, "get_next_activity", id),
                None
            ),
            Err(Error::Preconditions)
        ));
        assert!(journal.operation(&id.to_string()).unwrap().is_none());
        let proposed = call(&catalog, &fields, "get_next_activity", id);
        let permit = workflow(&mut journal, &proposed);
        let prepared = ready(prepare(&mut journal, "turn", proposed, Some(permit)).unwrap());
        let mut transport = FakeTransport {
            calls: 0,
            result: Err(mcp_client::Error::Call),
        };
        assert!(matches!(
            prepared
                .execute(
                    &mut journal,
                    &catalog,
                    "epoch",
                    &allow(),
                    &mut transport,
                    &AtomicBool::new(false)
                )
                .await,
            Err(Error::UnknownOutcome)
        ));
        assert_eq!(transport.calls, 1);
        assert_eq!(
            journal.operation(&id.to_string()).unwrap().unwrap().state,
            OperationState::Unknown
        );
        assert!(journal.has_unresolved_mutations().unwrap());
        let next = call(&catalog, &fields, "get_next_activity", uuid::Uuid::new_v4());
        let next_permit = workflow(&mut journal, &next);
        assert!(matches!(
            prepare(&mut journal, "turn", next, Some(next_permit)),
            Err(Error::RecoveryRequired)
        ));
        assert_eq!(transport.calls, 1);
        assert!(prepare(
            &mut journal,
            "turn",
            call(
                &catalog,
                &fields,
                "get_pending_alerts",
                uuid::Uuid::new_v4()
            ),
            None
        )
        .is_ok());
    }

    #[tokio::test]
    async fn cancellation_stale_binding_and_failed_storage_never_dispatch() {
        for case in ["cancel", "stale", "storage"] {
            let (_root, mut journal, catalog) = setup();
            let fields = VerifiedFields::new("epoch".into(), &journal).unwrap();
            let id = uuid::Uuid::new_v4();
            let prepared = ready(
                prepare(
                    &mut journal,
                    "turn",
                    call(&catalog, &fields, "get_pending_alerts", id),
                    None,
                )
                .unwrap(),
            );
            let mut transport = FakeTransport {
                calls: 0,
                result: Ok(json!({"content":[]})),
            };
            if case == "storage" {
                journal.reject_prepared(&id.to_string()).unwrap();
            }
            let result = prepared
                .execute(
                    &mut journal,
                    &catalog,
                    if case == "stale" {
                        "new-epoch"
                    } else {
                        "epoch"
                    },
                    &allow(),
                    &mut transport,
                    &AtomicBool::new(case == "cancel"),
                )
                .await;
            assert!(result.is_err());
            assert_eq!(transport.calls, 0);
        }
    }

    #[tokio::test]
    async fn mutation_error_is_preserved_without_claiming_a_rollback() {
        let (_root, mut journal, catalog) = setup();
        let fields = VerifiedFields::new("epoch".into(), &journal).unwrap();
        let proposed = call(&catalog, &fields, "get_next_activity", uuid::Uuid::new_v4());
        let id = proposed.operation_id().to_owned();
        let permit = workflow(&mut journal, &proposed);
        let prepared = ready(prepare(&mut journal, "turn", proposed, Some(permit)).unwrap());
        let reply =
            json!({"isError":true,"content":[{"type":"text","text":"exact private failure"}]});
        let mut transport = FakeTransport {
            calls: 0,
            result: Ok(reply.clone()),
        };
        assert!(matches!(
            prepared
                .execute(
                    &mut journal,
                    &catalog,
                    "epoch",
                    &allow(),
                    &mut transport,
                    &AtomicBool::new(false)
                )
                .await,
            Err(Error::UnknownOutcome)
        ));
        assert_eq!(
            serde_json::from_slice::<Value>(
                &journal
                    .event(&format!("{id}:uncertain"))
                    .unwrap()
                    .unwrap()
                    .payload
            )
            .unwrap(),
            reply
        );
        assert_eq!(
            journal.operation(&id).unwrap().unwrap().state,
            OperationState::Unknown
        );
    }

    #[tokio::test]
    async fn already_prepared_write_is_blocked_after_an_earlier_call_becomes_unknown() {
        let (_root, mut journal, catalog) = setup();
        let fields = VerifiedFields::new("epoch".into(), &journal).unwrap();
        let a = call(&catalog, &fields, "get_next_activity", uuid::Uuid::new_v4());
        let a_permit = workflow(&mut journal, &a);
        let a = ready(prepare(&mut journal, "turn", a, Some(a_permit)).unwrap());
        let b = call(&catalog, &fields, "get_next_activity", uuid::Uuid::new_v4());
        let b_id = b.operation_id().to_owned();
        let b_permit = workflow(&mut journal, &b);
        let b = ready(prepare(&mut journal, "turn", b, Some(b_permit)).unwrap());
        let mut transport = FakeTransport {
            calls: 0,
            result: Err(mcp_client::Error::Call),
        };
        let cancel = AtomicBool::new(false);
        assert!(matches!(
            a.execute(
                &mut journal,
                &catalog,
                "epoch",
                &allow(),
                &mut transport,
                &cancel
            )
            .await,
            Err(Error::UnknownOutcome)
        ));
        assert!(matches!(
            b.execute(
                &mut journal,
                &catalog,
                "epoch",
                &allow(),
                &mut transport,
                &cancel
            )
            .await,
            Err(Error::RecoveryRequired)
        ));
        assert_eq!(transport.calls, 1);
        assert_eq!(
            journal.operation(&b_id).unwrap().unwrap().state,
            OperationState::Prepared
        );
    }

    #[tokio::test]
    async fn approval_cannot_dispatch_another_journals_operation_with_the_same_uuid() {
        let (_root, mut first, catalog) = setup();
        let (_other, mut second, _) = setup();
        let fields = VerifiedFields::new("epoch".into(), &first).unwrap();
        let id = uuid::Uuid::new_v4();
        let prepared = ready(
            prepare(
                &mut first,
                "turn",
                call(&catalog, &fields, "get_pending_alerts", id),
                None,
            )
            .unwrap(),
        );
        second
            .prepare_operation(OperationRequest {
                id: &id.to_string(),
                turn_id: Some("turn"),
                tool: "tutor/get_pending_alerts",
                arguments: &json!({"limit":99}),
                idempotency_key: None,
                mutating: false,
            })
            .unwrap();
        let mut transport = FakeTransport {
            calls: 0,
            result: Ok(json!({"content":[]})),
        };
        assert!(matches!(
            prepared
                .execute(
                    &mut second,
                    &catalog,
                    "epoch",
                    &allow(),
                    &mut transport,
                    &AtomicBool::new(false)
                )
                .await,
            Err(Error::Preconditions)
        ));
        assert_eq!(transport.calls, 0);
        assert_eq!(
            second.operation(&id.to_string()).unwrap().unwrap().state,
            OperationState::Prepared
        );
    }

    #[test]
    fn a_foreign_workflow_receipt_cannot_authorize_the_same_call_in_another_journal() {
        let (_root, mut first, catalog) = setup();
        let (_other, mut second, _) = setup();
        let first_fields = VerifiedFields::new("epoch".into(), &first).unwrap();
        let second_fields = VerifiedFields::new("epoch".into(), &second).unwrap();
        let id = uuid::Uuid::new_v4();
        let a = call(&catalog, &first_fields, "get_next_activity", id);
        let proof = workflow(&mut first, &a);
        let b = call(&catalog, &second_fields, "get_next_activity", id);
        assert!(matches!(
            prepare(&mut second, "turn", b, Some(proof)),
            Err(Error::Preconditions)
        ));
        assert!(second.operation(&id.to_string()).unwrap().is_none());
    }
}
