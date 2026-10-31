// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! Durable, serial dispatch boundary. No model proposal is an authorization.
//! Writes additionally require a retained precondition receipt issued by the
//! pedagogical state machine; ordinary discovery can only inspect read tools.
//! Unknown outcomes require explicit reconciliation, never a new automatic call.

use crate::{
    call_validation::{Effect, ValidatedCall},
    catalog::ToolCatalog,
    journal::{ContentKind, ContentRef, Journal, JournalError, OperationRequest, OperationState},
    mcp_client::Transport,
};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use std::sync::{
    atomic::{AtomicBool, Ordering},
    Arc, Mutex,
};

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

fn open_turn(journal: &Journal, turn: &str) -> Result<(), Error> {
    if journal.turn(turn).map_err(|_| Error::Storage)?.is_none()
        || journal
            .event(&format!("{turn}:finish"))
            .map_err(|_| Error::Storage)?
            .is_some()
    {
        return Err(Error::Preconditions);
    }
    Ok(())
}

struct AuthorityState {
    instance: uuid::Uuid,
    turn: String,
    anchor: ContentRef,
    claim: ContentRef,
    revoked: Mutex<bool>,
}
impl AuthorityState {
    fn revocation_id(&self) -> String {
        format!("{}:revoked", self.anchor.id())
    }
    fn check_scope(&self, journal: &Journal, turn: &str) -> Result<(), Error> {
        if self.instance != journal.instance_id() || self.turn != turn {
            return Err(Error::Preconditions);
        }
        open_turn(journal, turn)?;
        journal
            .resolve_content(&self.anchor)
            .map_err(|_| Error::Preconditions)?;
        journal
            .resolve_content(&self.claim)
            .map_err(|_| Error::Preconditions)?;
        if journal
            .event(&self.revocation_id())
            .map_err(|_| Error::Storage)?
            .is_some()
        {
            return Err(Error::Preconditions);
        }
        Ok(())
    }
    fn check(&self, journal: &Journal, turn: &str) -> Result<(), Error> {
        let revoked = self.revoked.lock().map_err(|_| Error::Preconditions)?;
        if *revoked {
            return Err(Error::Preconditions);
        }
        self.check_scope(journal, turn)
    }
    fn retire(&self) {
        // A poisoned guard also denies dispatch. Preserve revocation even if
        // a previous owner panicked while holding this short synchronous lock.
        *self.revoked.lock().unwrap_or_else(|e| e.into_inner()) = true;
    }
}

/// Owned by the workflow for ONE open ACP turn. Permits share its guard but
/// cannot keep the authority alive after the workflow is dropped. Durable
/// workflow receipts may outlive this authority and be read by a later turn.
pub(crate) struct WorkflowAuthority {
    state: Arc<AuthorityState>,
}
impl WorkflowAuthority {
    pub(crate) fn new(
        journal: &mut Journal,
        turn: &str,
        anchor: &ContentRef,
    ) -> Result<Self, Error> {
        if anchor.kind() != ContentKind::Event {
            return Err(Error::Preconditions);
        }
        let event = journal
            .event(anchor.id())
            .map_err(|_| Error::Storage)?
            .ok_or(Error::Preconditions)?;
        if event.kind != "pedagogical_workflow_started" || event.turn_id.as_deref() != Some(turn) {
            return Err(Error::Preconditions);
        }
        open_turn(journal, turn)?;
        journal
            .resolve_content(anchor)
            .map_err(|_| Error::Preconditions)?;
        let claim_id = format!("{}:authority", anchor.id());
        for id in [&claim_id, &format!("{}:revoked", anchor.id())] {
            if journal.event(id).map_err(|_| Error::Storage)?.is_some() {
                return Err(Error::Preconditions);
            }
        }
        // Only the first constructor can claim this anchor. Dropping an
        // authority or reopening the journal cannot manufacture a fresh guard
        // for previously issued permits, even if stop could not be persisted.
        let claim_bytes = serde_json::to_vec(&json!({"version":1,"turn_id":turn,
            "anchor_id":anchor.id(),"anchor_sha256":anchor.sha256()}))
        .map_err(|_| Error::Storage)?;
        journal
            .append_event(
                &claim_id,
                Some(turn),
                "pedagogical_authority_created",
                &claim_bytes,
            )
            .map_err(|_| Error::Storage)?;
        let claim = journal
            .content_ref(ContentKind::Event, &claim_id)
            .map_err(|_| Error::Storage)?;
        let state = Arc::new(AuthorityState {
            instance: journal.instance_id(),
            turn: turn.into(),
            anchor: anchor.clone(),
            claim,
            revoked: Mutex::new(false),
        });
        state.check(journal, turn)?;
        Ok(Self { state })
    }
    pub(crate) fn revoke(&self, journal: &mut Journal) -> Result<(), Error> {
        // Retire first: storage failure must never leave old permits usable.
        self.state.retire();
        if journal.instance_id() != self.state.instance {
            return Err(Error::Preconditions);
        }
        journal
            .resolve_content(&self.state.anchor)
            .map_err(|_| Error::Preconditions)?;
        let bytes = serde_json::to_vec(&json!({
            "version": 1,
            "turn_id": self.state.turn,
            "anchor_id": self.state.anchor.id(),
            "anchor_sha256": self.state.anchor.sha256(),
        }))
        .map_err(|_| Error::Storage)?;
        journal
            .append_event(
                &self.state.revocation_id(),
                Some(&self.state.turn),
                "pedagogical_authority_revoked",
                &bytes,
            )
            .map_err(|_| Error::Storage)?;
        Ok(())
    }
}
impl Drop for WorkflowAuthority {
    fn drop(&mut self) {
        self.state.retire();
    }
}

/// Constructed only from an immutable, code-issued journal event. The state
/// machine must check the underlying server/learner proofs BEFORE writing
/// pedagogical_precondition. Model notes must never use that reserved kind.
pub struct WorkflowPermit {
    journal_instance: uuid::Uuid,
    operation: String,
    fingerprint: String,
    turn: String,
    precondition: ContentRef,
    authority: Arc<AuthorityState>,
}
impl WorkflowPermit {
    pub(crate) fn from_event(
        journal: &Journal,
        reference: &ContentRef,
        turn: &str,
        call: &ValidatedCall,
        authority: &WorkflowAuthority,
    ) -> Result<Self, Error> {
        authority.state.check(journal, turn)?;
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
            || value.get("authority_anchor_id").and_then(Value::as_str)
                != Some(authority.state.anchor.id())
            || value.get("authority_anchor_sha256") != Some(&json!(authority.state.anchor.sha256()))
        {
            return Err(Error::Preconditions);
        }
        Ok(Self {
            journal_instance: journal.instance_id(),
            operation: call.operation_id().into(),
            fingerprint: call.fingerprint().into(),
            turn: turn.into(),
            precondition: reference.clone(),
            authority: Arc::clone(&authority.state),
        })
    }

    fn check_identity(
        &self,
        journal: &Journal,
        turn: &str,
        call: &ValidatedCall,
    ) -> Result<(), Error> {
        if self.journal_instance != journal.instance_id()
            || self.operation != call.operation_id()
            || self.fingerprint != call.fingerprint()
            || self.turn != turn
        {
            return Err(Error::Preconditions);
        }
        journal
            .resolve_content(&self.precondition)
            .map_err(|_| Error::Preconditions)?;
        Ok(())
    }
    fn check(&self, journal: &Journal, turn: &str, call: &ValidatedCall) -> Result<(), Error> {
        self.check_identity(journal, turn, call)?;
        self.authority.check(journal, turn)
    }
    fn admit(
        &self,
        journal: &mut Journal,
        turn: &str,
        call: &ValidatedCall,
    ) -> Result<crate::journal::DispatchPermit, Error> {
        self.check_identity(journal, turn, call)?;
        let revoked = self
            .authority
            .revoked
            .lock()
            .map_err(|_| Error::Preconditions)?;
        if *revoked {
            return Err(Error::Preconditions);
        }
        self.authority.check_scope(journal, turn)?;
        // Revocation and admission have a definite order. No lock survives
        // this synchronous function or is held during the transport await.
        journal
            .mark_dispatched(call.operation_id())
            .map_err(|_| Error::Storage)
    }
}

pub struct PreparedCall {
    call: ValidatedCall,
    turn: String,
    contract: ContentRef,
    permit: Option<WorkflowPermit>,
}
pub enum Preparation {
    Ready(Box<PreparedCall>),
    Completed(Execution),
}
pub struct Execution {
    pub result: Value,
    pub reference: ContentRef,
    pub replayed: bool,
}

const CONTRACT_KIND: &str = "validated_tool_contract";

/// Version 1 receipts live at `<operation UUID>:validated-contract` in the
/// owner-bound journal. They retain the exact schema (as JSON, not its original
/// wire formatting), canonical argument bytes, effect/key mechanism and source
/// turn. They prove what this validator accepted before dispatch, NOT ACP
/// consent, pedagogical authority, server success, key retention or safe replay.
///
/// The journal verifies its stable owner on open; its ContentRef binds evidence
/// to that owner. Connection epochs, the epoch-dependent call fingerprint and
/// journal.instance_id() are deliberately absent: they are in-memory guards,
/// not durable identities. Reopening still requires fresh in-memory validation
/// and permission. Receipt IDs are journal-local, not portable capabilities.
fn contract_id(operation: &str) -> String {
    format!("{operation}:validated-contract")
}

fn contract_bytes(journal: &Journal, turn: &str, call: &ValidatedCall) -> Result<Vec<u8>, Error> {
    let turn_ref = journal
        .content_ref(ContentKind::Turn, turn)
        .map_err(|_| Error::Storage)?;
    let turn_bytes = journal
        .resolve_content(&turn_ref)
        .map_err(|_| Error::Storage)?;
    let arguments =
        crate::journal::canonical_arguments(call.arguments()).map_err(|_| Error::Storage)?;
    let canonical_arguments = String::from_utf8(arguments).map_err(|_| Error::Storage)?;
    let (effect, mechanism, key) = match call.effect() {
        Effect::ReadOnly => ("read_only", "none", None),
        Effect::MutationWithoutReplayContract => ("mutation_without_replay_contract", "none", None),
        Effect::MiddlewareKey => (
            "mutation",
            "params._meta.idempotency_key",
            call.idempotency_key(),
        ),
        Effect::LearningEventKey => (
            "mutation",
            "params.arguments.event_key",
            call.arguments().get("event_key").and_then(Value::as_str),
        ),
    };
    serde_json::to_vec(&json!({
        "version": 1,
        "operation_id": call.operation_id(),
        "turn_id": turn,
        "turn_sha256": format!("{:x}", Sha256::digest(&turn_bytes)),
        "tool_id": call.tool_id(),
        "tool_name": call.name(),
        "input_schema": call.input_schema(),
        // A string preserves the journal's exact canonical bytes, including
        // arbitrary-precision numbers, without another JSON normalization.
        "canonical_arguments": canonical_arguments,
        "effect": effect,
        "idempotency": {"mechanism": mechanism, "key": key},
    }))
    .map_err(|_| Error::Storage)
}

fn contract_error(error: JournalError) -> Error {
    match error {
        JournalError::Conflict | JournalError::BindingMismatch => Error::Preconditions,
        JournalError::RetiredId => Error::RecoveryRequired,
        _ => Error::Storage,
    }
}

fn persist_contract(
    journal: &mut Journal,
    turn: &str,
    call: &ValidatedCall,
) -> Result<ContentRef, Error> {
    let id = contract_id(call.operation_id());
    if journal
        .operation(call.operation_id())
        .map_err(contract_error)?
        .is_some()
        && journal.event(&id).map_err(contract_error)?.is_none()
    {
        // Never manufacture an old contract from a new model proposal or a
        // refreshed schema. This includes legacy Prepared/Completed records.
        return Err(Error::RecoveryRequired);
    }
    let bytes = contract_bytes(journal, turn, call)?;
    journal
        .append_event(&id, Some(turn), CONTRACT_KIND, &bytes)
        .map_err(contract_error)?;
    let reference = journal
        .content_ref(ContentKind::Event, &id)
        .map_err(contract_error)?;
    verify_contract(journal, turn, call, &reference)?;
    Ok(reference)
}

fn verify_contract(
    journal: &Journal,
    turn: &str,
    call: &ValidatedCall,
    reference: &ContentRef,
) -> Result<(), Error> {
    if reference.kind() != ContentKind::Event || reference.id() != contract_id(call.operation_id())
    {
        return Err(Error::Preconditions);
    }
    let event = journal
        .event(reference.id())
        .map_err(contract_error)?
        .ok_or(Error::RecoveryRequired)?;
    if event.kind != CONTRACT_KIND
        || event.turn_id.as_deref() != Some(turn)
        || journal.resolve_content(reference).map_err(contract_error)?
            != contract_bytes(journal, turn, call)?
    {
        return Err(Error::Preconditions);
    }
    Ok(())
}

pub fn prepare(
    journal: &mut Journal,
    turn: &str,
    call: ValidatedCall,
    permit: Option<WorkflowPermit>,
) -> Result<Preparation, Error> {
    call.check_journal(journal)
        .map_err(|_| Error::Preconditions)?;
    open_turn(journal, turn)?;
    if call.effect().mutating() && permit.is_none() {
        return Err(Error::Preconditions);
    }
    if let Some(permit) = &permit {
        permit.check(journal, turn, &call)?;
    }
    if call.effect().mutating()
        && journal
            .has_unresolved_mutations()
            .map_err(|_| Error::Storage)?
    {
        return Err(Error::RecoveryRequired);
    }
    // Commit the immutable contract FIRST. A failure after this commit may
    // leave an orphan receipt, which only an identical preparation can reuse.
    // Neither receipt nor Prepared alone is a dispatch/approval permit.
    let contract = persist_contract(journal, turn, &call)?;
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
        OperationState::Prepared => Ok(Preparation::Ready(Box::new(PreparedCall {
            call,
            turn: turn.into(),
            contract,
            permit,
        }))),
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
    fn reject_if_prepared(&self, journal: &mut Journal) -> Result<(), Error> {
        if journal
            .operation(self.call.operation_id())
            .map_err(|_| Error::Storage)?
            .is_some_and(|op| op.state == OperationState::Prepared)
        {
            journal
                .reject_prepared(self.call.operation_id())
                .map_err(|_| Error::Storage)?;
        }
        Ok(())
    }

    fn check_live(&self, journal: &Journal) -> Result<(), Error> {
        open_turn(journal, &self.turn)?;
        if let Some(permit) = &self.permit {
            permit.check(journal, &self.turn, &self.call)?;
        }
        Ok(())
    }

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
        if let Err(error) = self.check_live(journal) {
            self.reject_if_prepared(journal)?;
            return Err(error);
        }
        if cancel.load(Ordering::Acquire) {
            self.reject_if_prepared(journal)?;
            return Err(Error::Cancelled);
        }
        let allowed = response.pointer("/outcome/outcome").and_then(Value::as_str)
            == Some("selected")
            && response
                .pointer("/outcome/optionId")
                .and_then(Value::as_str)
                == Some("allow");
        if !allowed {
            self.reject_if_prepared(journal)?;
            return Err(Error::Denied);
        }
        if self.call.check_current(catalog, binding).is_err() || transport.server_id() != "tutor" {
            self.reject_if_prepared(journal)?;
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
        verify_contract(journal, &self.turn, &self.call, &self.contract)?;
        // The commit is the last boundary before IO. No request may be sent
        // if the durable Prepared→Dispatched transition fails.
        if cancel.load(Ordering::Acquire) {
            self.reject_if_prepared(journal)?;
            return Err(Error::Cancelled);
        }
        let admission = match &self.permit {
            Some(permit) => permit.admit(journal, &self.turn, &self.call),
            None => open_turn(journal, &self.turn).and_then(|()| {
                journal
                    .mark_dispatched(self.call.operation_id())
                    .map_err(|_| Error::Storage)
            }),
        };
        let permit = match admission {
            Ok(permit) => permit,
            Err(error) => {
                self.reject_if_prepared(journal)?;
                return Err(error);
            }
        };
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
            Preparation::Ready(call) => *call,
            _ => panic!("expected fresh call"),
        }
    }
    fn allow() -> Value {
        json!({"outcome":{"outcome":"selected","optionId":"allow"}})
    }
    fn workflow(
        journal: &mut Journal,
        call: &ValidatedCall,
    ) -> (WorkflowAuthority, WorkflowPermit) {
        workflow_phase(journal, call, "session_open")
    }
    fn workflow_phase(
        journal: &mut Journal,
        call: &ValidatedCall,
        phase: &str,
    ) -> (WorkflowAuthority, WorkflowPermit) {
        let anchor_id = format!("{}:workflow", call.operation_id());
        journal
            .append_event(
                &anchor_id,
                Some("turn"),
                "pedagogical_workflow_started",
                b"{\"version\":1}",
            )
            .unwrap();
        let anchor = journal.content_ref(ContentKind::Event, &anchor_id).unwrap();
        let authority = WorkflowAuthority::new(journal, "turn", &anchor).unwrap();
        let id = format!("{}:precondition", call.operation_id());
        journal.append_event(&id,Some("turn"),"pedagogical_precondition",&serde_json::to_vec(&json!({
            "operation_id":call.operation_id(),"fingerprint":call.fingerprint(),"tool_id":call.tool_id(),"phase":phase,
            "authority_anchor_id":anchor.id(),"authority_anchor_sha256":anchor.sha256(),
        })).unwrap()).unwrap();
        let permit = WorkflowPermit::from_event(
            journal,
            &journal.content_ref(ContentKind::Event, &id).unwrap(),
            "turn",
            call,
            &authority,
        )
        .unwrap();
        (authority, permit)
    }

    fn reopen(root: &tempfile::TempDir) -> Journal {
        Journal::open(
            &root.path().join("journal.sqlite"),
            &Binding {
                profile_id: "p",
                account_id: "a",
                tenant_id: "t",
                server_id: "s",
            },
        )
        .unwrap()
    }

    #[tokio::test]
    async fn failed_revocation_still_retires_permits_and_cannot_create_a_fresh_guard() {
        let (root, journal, catalog) = setup();
        drop(journal);
        let db = rusqlite::Connection::open(root.path().join("journal.sqlite")).unwrap();
        db.execute_batch("CREATE TRIGGER fail_revocation BEFORE INSERT ON events WHEN NEW.kind='pedagogical_authority_revoked' BEGIN SELECT RAISE(ABORT,'injected revocation failure'); END;").unwrap();
        drop(db);
        let mut journal = reopen(&root);
        let fields = VerifiedFields::new("epoch".into(), &journal).unwrap();
        let proposed = call(&catalog, &fields, "get_next_activity", uuid::Uuid::new_v4());
        let id = proposed.operation_id().to_owned();
        let (authority, permit) = workflow(&mut journal, &proposed);
        let anchor = authority.state.anchor.clone();
        let prepared = ready(prepare(&mut journal, "turn", proposed, Some(permit)).unwrap());
        assert!(matches!(
            authority.revoke(&mut journal),
            Err(Error::Storage)
        ));
        assert!(journal
            .event(&authority.state.revocation_id())
            .unwrap()
            .is_none());
        let mut transport = FakeTransport {
            calls: 0,
            result: Ok(json!({"content":[]})),
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
            Err(Error::Preconditions)
        ));
        assert_eq!(transport.calls, 0);
        assert_eq!(
            journal.operation(&id).unwrap().unwrap().state,
            OperationState::Rejected
        );
        drop(authority);
        assert!(WorkflowAuthority::new(&mut journal, "turn", &anchor).is_err());
        drop(journal);
        let mut journal = reopen(&root);
        assert!(WorkflowAuthority::new(&mut journal, "turn", &anchor).is_err());
        assert_eq!(
            journal.operation(&id).unwrap().unwrap().state,
            OperationState::Rejected
        );
    }

    #[test]
    fn another_live_workflow_cannot_replace_a_retired_permits_authority() {
        let (_root, mut journal, catalog) = setup();
        let fields = VerifiedFields::new("epoch".into(), &journal).unwrap();
        let first = call(&catalog, &fields, "get_next_activity", uuid::Uuid::new_v4());
        let (first_authority, first_permit) = workflow(&mut journal, &first);
        let source = first_permit.precondition.clone();
        drop(first_authority);
        let second = call(&catalog, &fields, "get_next_activity", uuid::Uuid::new_v4());
        let (second_authority, _second_permit) = workflow(&mut journal, &second);
        assert!(matches!(
            WorkflowPermit::from_event(&journal, &source, "turn", &first, &second_authority),
            Err(Error::Preconditions)
        ));
    }

    #[tokio::test]
    async fn a_finished_turn_also_retires_prepared_reads() {
        let (_root, mut journal, catalog) = setup();
        let fields = VerifiedFields::new("epoch".into(), &journal).unwrap();
        let proposed = call(
            &catalog,
            &fields,
            "get_pending_alerts",
            uuid::Uuid::new_v4(),
        );
        let id = proposed.operation_id().to_owned();
        let prepared = ready(prepare(&mut journal, "turn", proposed, None).unwrap());
        journal
            .append_event("turn:finish", Some("turn"), "turn_finished", b"{}")
            .unwrap();
        let mut transport = FakeTransport {
            calls: 0,
            result: Ok(json!({"content":[]})),
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
            Err(Error::Preconditions)
        ));
        assert_eq!(transport.calls, 0);
        assert_eq!(
            journal.operation(&id).unwrap().unwrap().state,
            OperationState::Rejected
        );
        let another = call(
            &catalog,
            &fields,
            "get_pending_alerts",
            uuid::Uuid::new_v4(),
        );
        assert!(matches!(
            prepare(&mut journal, "turn", another, None),
            Err(Error::Preconditions)
        ));
    }

    #[test]
    fn revocation_after_dispatch_does_not_rewrite_an_unknown_server_outcome() {
        use std::{sync::mpsc, time::Duration};
        struct InFlightTransport {
            entered: mpsc::Sender<()>,
            retired: mpsc::Receiver<()>,
            calls: usize,
        }
        impl Transport for InFlightTransport {
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
                self.entered.send(()).unwrap();
                self.retired.recv_timeout(Duration::from_secs(30)).unwrap();
                Err(mcp_client::Error::Call)
            }
        }
        let (_root, mut journal, catalog) = setup();
        let fields = VerifiedFields::new("epoch".into(), &journal).unwrap();
        let proposed = call(&catalog, &fields, "get_next_activity", uuid::Uuid::new_v4());
        let id = proposed.operation_id().to_owned();
        let (authority, permit) = workflow(&mut journal, &proposed);
        let prepared = ready(prepare(&mut journal, "turn", proposed, Some(permit)).unwrap());
        let (entered_tx, entered_rx) = mpsc::channel();
        let (retired_tx, retired_rx) = mpsc::channel();
        let mut transport = InFlightTransport {
            entered: entered_tx,
            retired: retired_rx,
            calls: 0,
        };
        let state = Arc::clone(&authority.state);
        let runtime = tokio::runtime::Builder::new_current_thread()
            .build()
            .unwrap();
        std::thread::scope(|scope| {
            scope.spawn(move || {
                // Transport is entered only after admission commits. Retire
                // while the result is still unknown, with no timing/sleeps.
                entered_rx.recv_timeout(Duration::from_secs(30)).unwrap();
                state.retire();
                retired_tx.send(()).unwrap();
            });
            let result = runtime.block_on(prepared.execute(
                &mut journal,
                &catalog,
                "epoch",
                &allow(),
                &mut transport,
                &AtomicBool::new(false),
            ));
            assert!(matches!(result, Err(Error::UnknownOutcome)));
        });
        assert_eq!(transport.calls, 1);
        authority.revoke(&mut journal).unwrap();
        assert_eq!(
            journal.operation(&id).unwrap().unwrap().state,
            OperationState::Unknown
        );
        assert!(journal.has_unresolved_mutations().unwrap());
    }

    #[tokio::test]
    async fn revocation_between_initial_check_and_final_admission_prevents_io() {
        struct RetiringTransport {
            state: Arc<AuthorityState>,
            calls: usize,
        }
        impl Transport for RetiringTransport {
            fn server_id(&self) -> &str {
                // execute checked live authority already, but has not yet
                // admitted the dispatch. No sleeps or scheduler race needed.
                self.state.retire();
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
                Ok(json!({"content":[]}))
            }
        }
        let (_root, mut journal, catalog) = setup();
        let fields = VerifiedFields::new("epoch".into(), &journal).unwrap();
        let proposed = call(&catalog, &fields, "get_next_activity", uuid::Uuid::new_v4());
        let id = proposed.operation_id().to_owned();
        let (authority, permit) = workflow(&mut journal, &proposed);
        let prepared = ready(prepare(&mut journal, "turn", proposed, Some(permit)).unwrap());
        let mut transport = RetiringTransport {
            state: Arc::clone(&authority.state),
            calls: 0,
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
            Err(Error::Preconditions)
        ));
        assert_eq!(transport.calls, 0);
        assert_eq!(
            journal.operation(&id).unwrap().unwrap().state,
            OperationState::Rejected
        );
        assert!(journal
            .transitions(&id)
            .unwrap()
            .iter()
            .all(|transition| transition.to != OperationState::Dispatched));
    }

    #[test]
    fn contract_receipt_retains_exact_schema_arguments_keys_and_source_links() {
        for (name, phase, mechanism) in [
            ("get_pending_alerts", None, "none"),
            (
                "get_next_activity",
                Some("session_open"),
                "params._meta.idempotency_key",
            ),
            (
                "record_learning_event",
                Some("feedback_delivered"),
                "params.arguments.event_key",
            ),
            (
                "get_memory_state",
                Some("coaching_context_requested"),
                "none",
            ),
        ] {
            let (_root, mut journal, mut catalog) = setup();
            let schema = json!({
                "type":"object", "additionalProperties":false,
                "$defs":{"payload":{"type":"object","properties":{
                    "z":{"type":"integer"},"a":{"type":"array","items":{"enum":["é","雪"]}}
                },"required":["z","a"],"additionalProperties":false}},
                "properties":{"payload":{"$ref":"#/$defs/payload"},"mode":{"enum":["coach","quiz"]},
                    "event_key":{"type":"string"}}, "required":["payload","mode"]
            });
            catalog
                .replace_server_tools(
                    "epoch",
                    "tutor",
                    vec![ToolEntry {
                        server_id: "tutor".into(),
                        name: name.into(),
                        description: String::new(),
                        input_schema: schema.clone(),
                        annotations: Value::Null,
                    }],
                )
                .unwrap();
            let fields = VerifiedFields::new("epoch".into(), &journal).unwrap();
            let proposal: Value = serde_json::from_str(
                r#"{"payload":{"z":9007199254740993,"a":["é","雪"]},"mode":"coach"}"#,
            )
            .unwrap();
            let id = uuid::Uuid::new_v4();
            let call = validate_call(
                &catalog,
                "epoch",
                &format!("tutor/{name}"),
                &proposal,
                &fields,
                id,
            )
            .unwrap();
            let (_authority, permit) = phase
                .map(|p| workflow_phase(&mut journal, &call, p))
                .unzip();
            let prepared = ready(prepare(&mut journal, "turn", call, permit).unwrap());
            let event = journal
                .event(&contract_id(&id.to_string()))
                .unwrap()
                .unwrap();
            let saved: Value = serde_json::from_slice(&event.payload).unwrap();
            let op = journal.operation(&id.to_string()).unwrap().unwrap();
            assert_eq!(event.kind, CONTRACT_KIND);
            assert_eq!(event.turn_id.as_deref(), Some("turn"));
            assert_eq!(saved["version"], 1);
            assert_eq!(saved["operation_id"], id.to_string());
            assert_eq!(saved["turn_id"], "turn");
            assert_eq!(
                saved["turn_sha256"],
                format!(
                    "{:x}",
                    Sha256::digest(journal.turn("turn").unwrap().unwrap().payload)
                )
            );
            assert_eq!(saved["tool_id"], op.tool);
            assert_eq!(saved["tool_name"], name);
            assert_eq!(saved["input_schema"], schema);
            let canonical = saved["canonical_arguments"].as_str().unwrap();
            assert_eq!(canonical.as_bytes(), op.canonical_arguments);
            assert!(canonical.contains("9007199254740993"));
            assert!(canonical.contains("é"));
            assert_eq!(saved["idempotency"]["mechanism"], mechanism);
            assert_eq!(
                saved["idempotency"]["key"],
                if mechanism == "none" {
                    Value::Null
                } else {
                    json!(id.to_string())
                }
            );
            assert_eq!(
                op.idempotency_key.as_deref(),
                (mechanism == "params._meta.idempotency_key")
                    .then(|| id.to_string())
                    .as_deref()
            );
            if name == "record_learning_event" {
                let args: Value = serde_json::from_str(canonical).unwrap();
                assert_eq!(args["event_key"], id.to_string());
            }
            assert_eq!(
                saved["effect"],
                if name == "get_pending_alerts" {
                    "read_only"
                } else if name == "get_memory_state" {
                    "mutation_without_replay_contract"
                } else {
                    "mutation"
                }
            );
            assert!(saved.get("binding").is_none());
            assert!(saved.get("journal_instance").is_none());
            assert!(saved.get("fingerprint").is_none());
            assert_eq!(
                journal.resolve_content(&prepared.contract).unwrap(),
                event.payload
            );
            assert_eq!(op.state, OperationState::Prepared);
            assert_eq!(journal.transitions(&id.to_string()).unwrap().len(), 1);
        }
    }

    #[tokio::test]
    async fn identical_contract_survives_reopen_and_new_connection_without_reusing_authority() {
        let (root, mut journal, catalog) = setup();
        let fields = VerifiedFields::new("epoch".into(), &journal).unwrap();
        let id = uuid::Uuid::new_v4();
        let old = ready(
            prepare(
                &mut journal,
                "turn",
                call(&catalog, &fields, "get_pending_alerts", id),
                None,
            )
            .unwrap(),
        );
        let receipt = journal.event(old.contract.id()).unwrap().unwrap();
        let repeated = ready(
            prepare(
                &mut journal,
                "turn",
                call(&catalog, &fields, "get_pending_alerts", id),
                None,
            )
            .unwrap(),
        );
        assert_eq!(repeated.contract.sha256(), old.contract.sha256());
        drop(journal);
        let mut journal = reopen(&root);
        let mut fresh = ToolCatalog::new("new-connection".into()).unwrap();
        fresh
            .replace_server_tools(
                "new-connection",
                "tutor",
                vec![catalog
                    .resolve("epoch", "tutor/get_pending_alerts")
                    .unwrap()
                    .clone()],
            )
            .unwrap();
        let mut transport = FakeTransport {
            calls: 0,
            result: Ok(json!({"content":[]})),
        };
        assert!(matches!(
            old.execute(
                &mut journal,
                &fresh,
                "new-connection",
                &allow(),
                &mut transport,
                &AtomicBool::new(false)
            )
            .await,
            Err(Error::Preconditions)
        ));
        assert_eq!(transport.calls, 0);
        let fields = VerifiedFields::new("new-connection".into(), &journal).unwrap();
        let call = validate_call(
            &fresh,
            "new-connection",
            "tutor/get_pending_alerts",
            &json!({}),
            &fields,
            id,
        )
        .unwrap();
        let current = ready(prepare(&mut journal, "turn", call, None).unwrap());
        let retained = journal.event(current.contract.id()).unwrap().unwrap();
        assert_eq!(receipt.sequence, retained.sequence);
        assert_eq!(receipt.payload, retained.payload);
        assert_eq!(journal.transitions(&id.to_string()).unwrap().len(), 1);
        current
            .execute(
                &mut journal,
                &fresh,
                "new-connection",
                &allow(),
                &mut transport,
                &AtomicBool::new(false),
            )
            .await
            .unwrap();
        assert_eq!(transport.calls, 1);
    }

    #[test]
    fn conflicting_receipt_metadata_or_contract_cannot_be_replaced() {
        let (_root, mut original, catalog) = setup();
        let fields = VerifiedFields::new("epoch".into(), &original).unwrap();
        let id = uuid::Uuid::new_v4();
        let original_call = ready(
            prepare(
                &mut original,
                "turn",
                call(&catalog, &fields, "get_pending_alerts", id),
                None,
            )
            .unwrap(),
        );
        let bytes = original.resolve_content(&original_call.contract).unwrap();
        for field in [
            "operation_id",
            "turn_id",
            "turn_sha256",
            "tool_id",
            "input_schema",
            "canonical_arguments",
            "idempotency",
            "effect",
            "kind",
            "event_turn",
        ] {
            let (_other, mut journal, _) = setup();
            journal.append_turn("other-turn", b"other input").unwrap();
            let mut payload: Value = serde_json::from_slice(&bytes).unwrap();
            if !matches!(field, "kind" | "event_turn") {
                payload[field] = json!("contradictory retained value");
            }
            let payload = serde_json::to_vec(&payload).unwrap();
            let kind = if field == "kind" {
                "model_note"
            } else {
                CONTRACT_KIND
            };
            let turn = if field == "event_turn" {
                "other-turn"
            } else {
                "turn"
            };
            journal
                .append_event(&contract_id(&id.to_string()), Some(turn), kind, &payload)
                .unwrap();
            let fields = VerifiedFields::new("epoch".into(), &journal).unwrap();
            assert!(
                matches!(
                    prepare(
                        &mut journal,
                        "turn",
                        call(&catalog, &fields, "get_pending_alerts", id),
                        None
                    ),
                    Err(Error::Preconditions)
                ),
                "{field}"
            );
            assert!(
                journal.operation(&id.to_string()).unwrap().is_none(),
                "{field}"
            );
            assert_eq!(
                journal
                    .event(&contract_id(&id.to_string()))
                    .unwrap()
                    .unwrap()
                    .payload,
                payload
            );
        }
    }

    #[test]
    fn changed_live_schema_cannot_rewrite_an_existing_validated_contract() {
        let (_root, mut journal, mut catalog) = setup();
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
        let original = journal.resolve_content(&prepared.contract).unwrap();
        let mut entry = catalog
            .resolve("epoch", "tutor/get_pending_alerts")
            .unwrap()
            .clone();
        // The same arguments remain valid; schema equivalence is insufficient.
        entry.input_schema["description"] = json!("Changed live contract");
        catalog
            .replace_server_tools("epoch", "tutor", vec![entry])
            .unwrap();
        assert!(matches!(
            prepare(
                &mut journal,
                "turn",
                call(&catalog, &fields, "get_pending_alerts", id),
                None
            ),
            Err(Error::Preconditions)
        ));
        assert_eq!(
            journal.resolve_content(&prepared.contract).unwrap(),
            original
        );
        assert_eq!(
            journal.operation(&id.to_string()).unwrap().unwrap().state,
            OperationState::Prepared
        );
    }

    #[tokio::test]
    async fn receipt_and_operation_persistence_failures_cannot_dispatch() {
        for table in ["events", "operations"] {
            let (root, journal, catalog) = setup();
            drop(journal);
            let db = rusqlite::Connection::open(root.path().join("journal.sqlite")).unwrap();
            let condition = if table == "events" {
                "WHEN NEW.kind='validated_tool_contract'"
            } else {
                ""
            };
            db.execute_batch(&format!("CREATE TRIGGER deny_contract_test BEFORE INSERT ON {table} {condition} BEGIN SELECT RAISE(ABORT,'injected persistence failure'); END;")).unwrap();
            drop(db);
            let mut journal = reopen(&root);
            let fields = VerifiedFields::new("epoch".into(), &journal).unwrap();
            let id = uuid::Uuid::new_v4();
            let mut transport = FakeTransport {
                calls: 0,
                result: Ok(json!({"content":[]})),
            };
            match prepare(
                &mut journal,
                "turn",
                call(&catalog, &fields, "get_pending_alerts", id),
                None,
            ) {
                Err(Error::Storage) => (),
                Ok(Preparation::Ready(prepared)) => {
                    let _ = prepared
                        .execute(
                            &mut journal,
                            &catalog,
                            "epoch",
                            &allow(),
                            &mut transport,
                            &AtomicBool::new(false),
                        )
                        .await;
                    panic!("a failed persistence must not yield a dispatchable call");
                }
                _ => panic!("expected storage failure"),
            }
            assert_eq!(transport.calls, 0);
            assert!(journal.operation(&id.to_string()).unwrap().is_none());
            let orphan = journal.event(&contract_id(&id.to_string())).unwrap();
            assert_eq!(orphan.is_some(), table == "operations");
            drop(journal);
            let db = rusqlite::Connection::open(root.path().join("journal.sqlite")).unwrap();
            db.execute_batch("DROP TRIGGER deny_contract_test;")
                .unwrap();
            drop(db);
            let mut journal = reopen(&root);
            let fields = VerifiedFields::new("epoch".into(), &journal).unwrap();
            let prepared = ready(
                prepare(
                    &mut journal,
                    "turn",
                    call(&catalog, &fields, "get_pending_alerts", id),
                    None,
                )
                .unwrap(),
            );
            if let Some(orphan) = orphan {
                assert_eq!(
                    journal
                        .event(prepared.contract.id())
                        .unwrap()
                        .unwrap()
                        .sequence,
                    orphan.sequence
                );
                assert_eq!(
                    journal.resolve_content(&prepared.contract).unwrap(),
                    orphan.payload
                );
            }
            prepared
                .execute(
                    &mut journal,
                    &catalog,
                    "epoch",
                    &allow(),
                    &mut transport,
                    &AtomicBool::new(false),
                )
                .await
                .unwrap();
            assert_eq!(transport.calls, 1);
        }
    }

    #[test]
    fn operations_without_original_receipts_are_not_backfilled() {
        for state in [
            OperationState::Prepared,
            OperationState::Completed,
            OperationState::Unknown,
        ] {
            let (_root, mut journal, catalog) = setup();
            let fields = VerifiedFields::new("epoch".into(), &journal).unwrap();
            let id = uuid::Uuid::new_v4();
            journal
                .prepare_operation(OperationRequest {
                    id: &id.to_string(),
                    turn_id: Some("turn"),
                    tool: "tutor/get_pending_alerts",
                    arguments: &json!({}),
                    idempotency_key: None,
                    mutating: false,
                })
                .unwrap();
            if state != OperationState::Prepared {
                journal.mark_dispatched(&id.to_string()).unwrap();
                if state == OperationState::Completed {
                    journal
                        .complete_operation(&id.to_string(), br#"{"content":[]}"#)
                        .unwrap();
                } else {
                    journal.mark_unknown(&id.to_string()).unwrap();
                }
            }
            assert!(matches!(
                prepare(
                    &mut journal,
                    "turn",
                    call(&catalog, &fields, "get_pending_alerts", id),
                    None
                ),
                Err(Error::RecoveryRequired)
            ));
            assert!(journal
                .event(&contract_id(&id.to_string()))
                .unwrap()
                .is_none());
            assert_eq!(
                journal.operation(&id.to_string()).unwrap().unwrap().state,
                state
            );
        }
    }

    #[tokio::test]
    async fn execute_requires_its_own_exact_contract_reference() {
        let (_root, mut journal, catalog) = setup();
        let fields = VerifiedFields::new("epoch".into(), &journal).unwrap();
        let id = uuid::Uuid::new_v4();
        let mut first = ready(
            prepare(
                &mut journal,
                "turn",
                call(&catalog, &fields, "get_pending_alerts", id),
                None,
            )
            .unwrap(),
        );
        let second = ready(
            prepare(
                &mut journal,
                "turn",
                call(
                    &catalog,
                    &fields,
                    "get_pending_alerts",
                    uuid::Uuid::new_v4(),
                ),
                None,
            )
            .unwrap(),
        );
        first.contract = second.contract;
        let mut transport = FakeTransport {
            calls: 0,
            result: Ok(json!({"content":[]})),
        };
        assert!(matches!(
            first
                .execute(
                    &mut journal,
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
            journal.operation(&id.to_string()).unwrap().unwrap().state,
            OperationState::Prepared
        );
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
        let (_permit_authority, permit) = workflow(&mut journal, &proposed);
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
        let (_next_permit_authority, next_permit) = workflow(&mut journal, &next);
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
        let (_permit_authority, permit) = workflow(&mut journal, &proposed);
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
        let (_a_permit_authority, a_permit) = workflow(&mut journal, &a);
        let a = ready(prepare(&mut journal, "turn", a, Some(a_permit)).unwrap());
        let b = call(&catalog, &fields, "get_next_activity", uuid::Uuid::new_v4());
        let b_id = b.operation_id().to_owned();
        let (_b_permit_authority, b_permit) = workflow(&mut journal, &b);
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
        let (_proof_authority, proof) = workflow(&mut first, &a);
        let b = call(&catalog, &second_fields, "get_next_activity", id);
        assert!(matches!(
            prepare(&mut second, "turn", b, Some(proof)),
            Err(Error::Preconditions)
        ));
        assert!(second.operation(&id.to_string()).unwrap().is_none());
    }
}
