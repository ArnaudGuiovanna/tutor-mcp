// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! H07 lot 1: dormant, bounded receipt machine, not a tool dispatcher.
//! No model, transport, UI, clock, or automatic recovery is called here.
//! Effects are code-issued journal events and rejection of owned Prepared
//! operations when stopped. The caller still needs
//! executor::prepare and the exact host ACP permission before any MCP IO.
//! Authorities have private fields and no Deserialize constructor. They can
//! only be obtained from completed, expected operations in a SessionStore
//! already opened for its verified Owner. Training revision is NOT a numeric
//! curriculum version; a concept's engine key is NOT its stable curriculum ID.

use crate::{
    call_validation::{self, ValidatedCall, VerifiedFields},
    catalog::ToolCatalog,
    executor::{self, WorkflowAuthority, WorkflowPermit},
    host_context::HostContext,
    journal::{canonical_arguments, ContentKind, ContentRef, Journal, OperationState},
    session_store::SessionStore,
};
use serde_json::{json, Value};
use std::collections::HashSet;
use uuid::Uuid;

const MAX_RECEIPT: usize = 1024 * 1024;
const MAX_CONCEPTS: usize = 512;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Error {
    Scope,
    Phase,
    Pending,
    Receipt,
    Contract,
    Storage,
    RecoveryRequired,
}
impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            Self::Scope => "pedagogical_scope_changed",
            Self::Phase => "pedagogical_phase_unavailable",
            Self::Pending => "pedagogical_operation_pending",
            Self::Receipt => "pedagogical_receipt_invalid",
            Self::Contract => "pedagogical_contract_unsupported",
            Self::Storage => "pedagogical_journal_unavailable",
            Self::RecoveryRequired => "pedagogical_reconciliation_required",
        })
    }
}
impl std::error::Error for Error {}
type Result<T> = std::result::Result<T, Error>;

/// Supplied by the host's validated current study request, never model output.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Intent {
    Auto,
    Review,
}
impl Intent {
    fn wire(self) -> &'static str {
        match self {
            Self::Auto => "auto",
            Self::Review => "review",
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Phase {
    ContextRequested,
    ContextConfirmed,
    CurriculumConfirmed,
    SessionOpen,
    DecisionConfirmed,
    Terminal(TerminalKind),
    Stopped,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum TerminalKind {
    Rest,
    CloseSession,
}

/// Server guidance without a persisted cognitive decision. This receipt never
/// authorizes prepare/submit/evaluation or record_session_close. CLOSE_SESSION
/// is a recommendation to end the session, not proof it has been closed.
pub struct TerminalReceipt {
    source: ContentRef,
    kind: TerminalKind,
    activity: Value,
    contract: Value,
}
impl TerminalReceipt {
    pub fn source(&self) -> &ContentRef {
        &self.source
    }
    pub fn kind(&self) -> TerminalKind {
        self.kind
    }
    pub fn activity(&self) -> &Value {
        &self.activity
    }
    pub fn contract(&self) -> &Value {
        &self.contract
    }
}

/// Private result identity; the corresponding complete MCP envelope remains
/// in the journal. A bare result reference is never an execution permission.
pub struct ContextReceipt {
    source: ContentRef,
    value: Value,
}
pub struct CurriculumReceipt {
    source: ContentRef,
    version: u64,
    concepts: Vec<Value>,
}
pub struct SessionReceipt {
    source: ContentRef,
    id: String,
}
pub struct DecisionReceipt {
    source: ContentRef,
    id: String,
    concept_key: String,
    activity_type: String,
    contract: Value,
}
impl DecisionReceipt {
    pub fn source(&self) -> &ContentRef {
        &self.source
    }
    pub fn decision_id(&self) -> &str {
        &self.id
    }
    pub fn concept_key(&self) -> &str {
        &self.concept_key
    }
    pub fn activity_type(&self) -> &str {
        &self.activity_type
    }
    /// Includes exact constraints/outcomes; validating a generated quiz against
    /// them belongs to lot 2. DecisionConfirmed is NOT activity_validated.
    pub fn contract(&self) -> &Value {
        &self.contract
    }
}

pub struct AuthorizedStep {
    call: ValidatedCall,
    permit: WorkflowPermit,
}
impl AuthorizedStep {
    pub fn call(&self) -> &ValidatedCall {
        &self.call
    }
    pub fn into_parts(self) -> (ValidatedCall, WorkflowPermit) {
        (self.call, self.permit)
    }
}
struct Pending {
    operation: String,
    tool: String,
    arguments: Vec<u8>,
    key: Option<String>,
    schema: Value,
    precondition: ContentRef,
}

pub struct Workflow {
    instance: Uuid,
    acp_session: String,
    turn: String,
    epoch: String,
    host: HostContext,
    host_source: ContentRef,
    session_header: ContentRef,
    anchor: ContentRef,
    authority: WorkflowAuthority,
    intent: Intent,
    phase: Phase,
    context: Option<ContextReceipt>,
    curriculum: Option<CurriculumReceipt>,
    session: Option<SessionReceipt>,
    decision: Option<DecisionReceipt>,
    terminal: Option<TerminalReceipt>,
    pending: Option<Pending>,
}

impl Workflow {
    pub fn begin(
        store: &mut SessionStore,
        turn: &str,
        host_source: &ContentRef,
        epoch: &str,
        intent: Intent,
    ) -> Result<Self> {
        let journal = store.journal();
        if !bounded(epoch, 4096) || !bounded(turn, 128) {
            return Err(Error::Scope);
        }
        require_open_turn(journal, turn)?;
        if journal
            .has_unresolved_mutations()
            .map_err(|_| Error::Storage)?
        {
            return Err(Error::RecoveryRequired);
        }
        if host_source.kind() != ContentKind::Event {
            return Err(Error::Scope);
        }
        let event = journal
            .event(host_source.id())
            .map_err(|_| Error::Storage)?
            .ok_or(Error::Scope)?;
        if event.kind != "verified_host_context" || event.turn_id.as_deref() != Some(turn) {
            return Err(Error::Scope);
        }
        let bytes = journal
            .resolve_content(host_source)
            .map_err(|_| Error::Scope)?;
        if bytes.len() > 64 * 1024 {
            return Err(Error::Scope);
        }
        let host: HostContext = serde_json::from_slice(&bytes).map_err(|_| Error::Scope)?;
        host.validate().map_err(|_| Error::Scope)?;
        for key in ["training_id", "domain_id", "revision"] {
            text(&host.training_context, key).map_err(|_| Error::Scope)?;
        }
        let header = journal
            .content_ref(ContentKind::Event, "session-header")
            .map_err(|_| Error::Scope)?;
        let session: Value =
            serde_json::from_slice(&journal.resolve_content(&header).map_err(|_| Error::Scope)?)
                .map_err(|_| Error::Scope)?;
        if session["version"] != 1 || session["session_id"] != store.id() {
            return Err(Error::Scope);
        }
        let anchor_id = format!("{turn}:h07");
        // No fresh operation keys after a crash or a second constructor. Lot 1
        // has no reconciliation/recovery implementation and fails closed.
        if journal
            .event(&anchor_id)
            .map_err(|_| Error::Storage)?
            .is_some()
        {
            return Err(Error::RecoveryRequired);
        }
        let instance = journal.instance_id();
        let acp_session = store.id().to_owned();
        let payload = json!({"version":1,"session_id":acp_session,"intent":intent.wire(),
            "host":proof(host_source),"session_header":proof(&header),"catalog_epoch":epoch});
        store
            .journal_mut()
            .append_event(
                &anchor_id,
                Some(turn),
                "pedagogical_workflow_started",
                &serde_json::to_vec(&payload).map_err(|_| Error::Storage)?,
            )
            .map_err(|_| Error::Storage)?;
        let anchor = store
            .journal()
            .content_ref(ContentKind::Event, &anchor_id)
            .map_err(|_| Error::Storage)?;
        let authority =
            WorkflowAuthority::new(store.journal_mut(), turn, &anchor).map_err(authority_error)?;
        Ok(Self {
            instance,
            acp_session,
            turn: turn.into(),
            epoch: epoch.into(),
            host,
            host_source: host_source.clone(),
            session_header: header,
            anchor,
            authority,
            intent,
            phase: Phase::ContextRequested,
            context: None,
            curriculum: None,
            session: None,
            decision: None,
            terminal: None,
            pending: None,
        })
    }

    pub fn phase(&self) -> Phase {
        self.phase
    }
    pub fn stop(&mut self, store: &mut SessionStore) -> Result<()> {
        self.phase = Phase::Stopped;
        // revoke retires the shared memory guard BEFORE attempting journal IO.
        // Still attempt cleanup if persistence fails; neither error may reopen
        // this workflow or make an already-issued permit dispatchable again.
        let revoked = self
            .authority
            .revoke(store.journal_mut())
            .map_err(authority_error);
        let cleaned = self.reject_pending(store);
        match (revoked, cleaned) {
            (Err(Error::Storage), _) | (_, Err(Error::Storage)) => Err(Error::Storage),
            (Err(error), _) | (_, Err(error)) => Err(error),
            (Ok(()), Ok(())) => Ok(()),
        }
    }

    fn reject_pending(&mut self, store: &mut SessionStore) -> Result<()> {
        if self.instance != store.journal().instance_id() || self.acp_session != store.id() {
            return Err(Error::Scope);
        }
        let Some(pending) = self.pending.as_ref() else {
            return Ok(());
        };
        if let Some(operation) = store
            .journal()
            .operation(&pending.operation)
            .map_err(|_| Error::Storage)?
        {
            if operation.turn_id.as_deref() != Some(self.turn.as_str())
                || operation.tool != pending.tool
                || operation.canonical_arguments != pending.arguments
                || operation.idempotency_key != pending.key
            {
                return Err(Error::Scope);
            }
            match operation.state {
                OperationState::Prepared => store
                    .journal_mut()
                    .reject_prepared(&pending.operation)
                    .map_err(|_| Error::Storage)?,
                OperationState::Rejected => {}
                // Keep the pending identity for reconciliation. Never rewrite
                // a dispatched/unknown outcome or an already-completed result.
                OperationState::Dispatched
                | OperationState::Unknown
                | OperationState::Completed => return Ok(()),
            }
        }
        self.pending = None;
        Ok(())
    }

    fn check_scope(&self, store: &SessionStore, host: &ContentRef, epoch: &str) -> Result<()> {
        let journal = store.journal();
        if self.phase == Phase::Stopped
            || self.instance != journal.instance_id()
            || self.acp_session != store.id()
            || self.epoch != epoch
            || &self.host_source != host
        {
            return Err(Error::Scope);
        }
        require_open_turn(journal, &self.turn)?;
        for source in self.sources() {
            journal.resolve_content(source).map_err(|_| Error::Scope)?;
        }
        if journal
            .has_unresolved_mutations()
            .map_err(|_| Error::Storage)?
        {
            return Err(Error::RecoveryRequired);
        }
        Ok(())
    }

    fn sources(&self) -> Vec<&ContentRef> {
        let mut refs = vec![&self.host_source, &self.session_header, &self.anchor];
        if let Some(v) = &self.context {
            refs.push(&v.source);
        }
        if let Some(v) = &self.curriculum {
            refs.push(&v.source);
        }
        if let Some(v) = &self.session {
            refs.push(&v.source);
        }
        if let Some(v) = &self.decision {
            refs.push(&v.source);
        }
        if let Some(v) = &self.terminal {
            refs.push(&v.source);
        }
        refs
    }

    /// This is the integration seam with the existing executor. It validates
    /// the real tools/list schema, writes its reserved precondition, and issues
    /// WorkflowPermit::from_event. It neither prepares nor dispatches a call.
    /// No arbitrary tool/arguments can be supplied by a model or the caller.
    pub fn authorize_next(
        &mut self,
        store: &mut SessionStore,
        catalog: &ToolCatalog,
        current_host: &ContentRef,
        epoch: &str,
    ) -> Result<AuthorizedStep> {
        let result = self.authorize(store, catalog, current_host, epoch);
        if result
            .as_ref()
            .is_err_and(|e| !matches!(e, Error::Pending | Error::Phase))
        {
            // Preserve the triggering validation error, except that a failed
            // durable stop must surface as Storage, never as a mere rejection.
            if let Err(Error::Storage) = self.stop(store) {
                return Err(Error::Storage);
            }
        }
        result
    }

    fn authorize(
        &mut self,
        store: &mut SessionStore,
        catalog: &ToolCatalog,
        current_host: &ContentRef,
        epoch: &str,
    ) -> Result<AuthorizedStep> {
        self.check_scope(store, current_host, epoch)?;
        if self.pending.is_some() {
            return Err(Error::Pending);
        }
        let (name, phase) = match self.phase {
            Phase::ContextRequested => ("get_conversation_context", "context_requested"),
            Phase::ContextConfirmed => ("get_curriculum_snapshot", "context_confirmed"),
            Phase::CurriculumConfirmed => ("start_learning_session", "context_confirmed"),
            Phase::SessionOpen => ("get_next_activity", "session_open"),
            _ => return Err(Error::Phase),
        };
        let journal = store.journal();
        let mut fields = VerifiedFields::new(epoch.into(), journal).map_err(|_| Error::Scope)?;
        if self.phase == Phase::ContextRequested {
            fields.bind_conversation(self.host.conversation_uuid());
        } else {
            let context = self.context.as_ref().ok_or(Error::Phase)?;
            fields
                .bind_result(journal, &context.source, &[("domain_id", "/domain_id")])
                .map_err(|_| Error::Receipt)?;
        }
        let arguments = if self.phase == Phase::SessionOpen {
            json!({"intent":self.intent.wire()})
        } else {
            json!({})
        };
        let tool = format!("tutor/{name}");
        let call = call_validation::validate_call(
            catalog,
            epoch,
            &tool,
            &arguments,
            &fields,
            Uuid::new_v4(),
        )
        .map_err(|_| Error::Contract)?;
        let schema = catalog
            .resolve(epoch, &tool)
            .map_err(|_| Error::Contract)?
            .input_schema
            .clone();
        let canonical = canonical_arguments(call.arguments()).map_err(|_| Error::Contract)?;
        let event_id = format!("{}:precondition", call.operation_id());
        let payload = json!({"operation_id":call.operation_id(),"fingerprint":call.fingerprint(),
            "tool_id":call.tool_id(),"phase":phase,"h07_version":1,"acp_session_id":self.acp_session,
            "authority_anchor_id":self.anchor.id(),"authority_anchor_sha256":self.anchor.sha256(),
            "conversation_id":self.host.conversation_id,
            "sources":self.sources().into_iter().map(proof).collect::<Vec<_>>()});
        store
            .journal_mut()
            .append_event(
                &event_id,
                Some(&self.turn),
                "pedagogical_precondition",
                &serde_json::to_vec(&payload).map_err(|_| Error::Storage)?,
            )
            .map_err(|_| Error::Storage)?;
        let reference = store
            .journal()
            .content_ref(ContentKind::Event, &event_id)
            .map_err(|_| Error::Storage)?;
        let permit = WorkflowPermit::from_event(
            store.journal(),
            &reference,
            &self.turn,
            &call,
            &self.authority,
        )
        .map_err(|error| match error {
            executor::Error::Storage => Error::Storage,
            _ => Error::Receipt,
        })?;
        self.pending = Some(Pending {
            operation: call.operation_id().into(),
            tool,
            arguments: canonical,
            key: call.idempotency_key().map(str::to_owned),
            schema,
            precondition: reference,
        });
        Ok(AuthorizedStep { call, permit })
    }

    /// Only the exact completed operation for the outstanding step can advance
    /// state. Any rejected result stops the workflow; no inferred success or
    /// silent fresh key/retry follows. Receipt schemas are private parsers.
    pub fn accept_result(
        &mut self,
        store: &mut SessionStore,
        catalog: &ToolCatalog,
        current_host: &ContentRef,
        epoch: &str,
        source: &ContentRef,
    ) -> Result<()> {
        let result = self.accept(store, catalog, current_host, epoch, source);
        if result.is_err() {
            if let Err(Error::Storage) = self.stop(store) {
                return Err(Error::Storage);
            }
        }
        result
    }

    fn accept(
        &mut self,
        store: &SessionStore,
        catalog: &ToolCatalog,
        host: &ContentRef,
        epoch: &str,
        source: &ContentRef,
    ) -> Result<()> {
        self.check_scope(store, host, epoch)?;
        let journal = store.journal();
        let pending = self.pending.as_ref().ok_or(Error::Phase)?;
        if source.kind() != ContentKind::OperationResult || source.id() != pending.operation {
            return Err(Error::Receipt);
        }
        let entry = catalog
            .resolve(epoch, &pending.tool)
            .map_err(|_| Error::Scope)?;
        if entry.input_schema != pending.schema {
            return Err(Error::Scope);
        }
        journal
            .resolve_content(&pending.precondition)
            .map_err(|_| Error::Receipt)?;
        let operation = journal
            .operation(source.id())
            .map_err(|_| Error::Storage)?
            .ok_or(Error::Receipt)?;
        if operation.state != OperationState::Completed
            || operation.turn_id.as_deref() != Some(self.turn.as_str())
            || operation.tool != pending.tool
            || operation.canonical_arguments != pending.arguments
            || operation.idempotency_key != pending.key
            || !operation.mutating
        {
            return Err(Error::Receipt);
        }
        let bytes = journal
            .resolve_content(source)
            .map_err(|_| Error::Receipt)?;
        if bytes.len() > MAX_RECEIPT {
            return Err(Error::Receipt);
        }
        let envelope: Value = serde_json::from_slice(&bytes).map_err(|_| Error::Receipt)?;
        let payload = call_validation::result_payload(&envelope).map_err(|_| Error::Receipt)?;
        match self.phase {
            Phase::ContextRequested => {
                for key in ["conversation_id", "training_id", "domain_id", "revision"] {
                    if text(&payload, key)? != text(&self.host.training_context, key)? {
                        return Err(Error::Scope);
                    }
                }
                if payload["selection_version"].as_u64().is_none()
                    || payload["selection_version"]
                        != self.host.training_context["selection_version"]
                {
                    return Err(Error::Scope);
                }
                self.context = Some(ContextReceipt {
                    source: source.clone(),
                    value: payload,
                });
                self.phase = Phase::ContextConfirmed;
            }
            Phase::ContextConfirmed => {
                let snapshot = &payload["snapshot"];
                if text(snapshot, "domain_id")? != self.domain()? {
                    return Err(Error::Scope);
                }
                let version = positive(snapshot, "version")?;
                let concepts = snapshot["concepts"].as_array().ok_or(Error::Receipt)?;
                if concepts.is_empty() || concepts.len() > MAX_CONCEPTS {
                    return Err(Error::Contract);
                }
                let mut ids = HashSet::new();
                let mut keys = HashSet::new();
                for concept in concepts {
                    if !ids.insert(text(concept, "id")?)
                        || !keys.insert(text(concept, "key")?)
                        || !matches!(text(concept, "status")?, "active" | "retired")
                    {
                        return Err(Error::Receipt);
                    }
                }
                self.curriculum = Some(CurriculumReceipt {
                    source: source.clone(),
                    version,
                    concepts: concepts.clone(),
                });
                self.phase = Phase::CurriculumConfirmed;
            }
            Phase::CurriculumConfirmed => {
                let session = &payload["session"];
                let id = text(&payload, "session_id")?;
                if text(session, "id")? != id
                    || text(session, "domain_id")? != self.domain()?
                    || text(session, "status")? != "open"
                    || session.get("closed_at").is_some_and(|v| !v.is_null())
                {
                    return Err(Error::Scope);
                }
                self.session = Some(SessionReceipt {
                    source: source.clone(),
                    id: id.into(),
                });
                self.phase = Phase::SessionOpen;
            }
            Phase::SessionOpen => {
                let terminal_kind = match payload.pointer("/activity/type").and_then(Value::as_str)
                {
                    Some("REST") => Some(TerminalKind::Rest),
                    Some("CLOSE_SESSION") => Some(TerminalKind::CloseSession),
                    _ => None,
                };
                if let Some(kind) = terminal_kind {
                    self.terminal = Some(self.read_terminal(source, &payload, kind)?);
                    self.phase = Phase::Terminal(kind);
                } else {
                    self.decision = Some(self.read_decision(source, &payload)?);
                    self.phase = Phase::DecisionConfirmed;
                }
            }
            _ => return Err(Error::Phase),
        }
        self.pending = None;
        Ok(())
    }

    fn domain(&self) -> Result<&str> {
        text(
            &self.context.as_ref().ok_or(Error::Phase)?.value,
            "domain_id",
        )
    }

    fn check_activity_scope(&self, payload: &Value) -> Result<()> {
        if payload["needs_domain_setup"] != false {
            return Err(Error::Contract);
        }
        let session = self.session.as_ref().ok_or(Error::Phase)?;
        if text(payload, "domain_id")? != self.domain()?
            || text(payload, "session_id")? != session.id
            || text(payload, "intent")? != self.intent.wire()
        {
            return Err(Error::Scope);
        }
        Ok(())
    }

    fn read_terminal(
        &self,
        source: &ContentRef,
        payload: &Value,
        kind: TerminalKind,
    ) -> Result<TerminalReceipt> {
        self.check_activity_scope(payload)?;
        let activity = &payload["activity"];
        let contract = &payload["pedagogical_contract"];
        let expected = match kind {
            TerminalKind::Rest => "REST",
            TerminalKind::CloseSession => "CLOSE_SESSION",
        };
        if text(activity, "type")? != expected
            || text(contract, "recommended_activity_type")? != expected
        {
            return Err(Error::Scope);
        }
        // tools/pedagogical_decision.go skips non-cognitive activities. The
        // top-level map contains decision_id:""; omitempty contract fields
        // contain no decision/policy/version/competency. Never fabricate them.
        if payload.get("decision_id").and_then(Value::as_str) != Some("")
            || [
                "decision_id",
                "policy_version",
                "curriculum_version",
                "competency",
            ]
            .iter()
            .any(|key| contract.get(key).is_some())
        {
            return Err(Error::Contract);
        }
        let concept = activity
            .get("concept")
            .and_then(Value::as_str)
            .ok_or(Error::Receipt)?;
        if concept.is_empty() {
            if contract.get("target_concept").is_some() {
                return Err(Error::Scope);
            }
        } else if !bounded(concept, 4096) || text(contract, "target_concept")? != concept {
            return Err(Error::Scope);
        }
        if text(contract, "learning_event_protocol")? != "response-feedback-v1"
            || text(contract, "bkt_update_mode")? != "observation_only"
            || contract["llm_discretion"]["cannot_mark_mastery_without_evidence"] != true
        {
            return Err(Error::Contract);
        }
        text(contract, "intent")?;
        strings(&contract["constraints"]["must_collect"], 64)?;
        strings(&contract["constraints"]["avoid"], 64)?;
        strings(&contract["allowed_variants"], 64)?;
        for key in [
            "can_change_format",
            "can_request_clarification",
            "can_propose_negotiation",
        ] {
            if contract["llm_discretion"][key].as_bool().is_none() {
                return Err(Error::Receipt);
            }
        }
        Ok(TerminalReceipt {
            source: source.clone(),
            kind,
            activity: activity.clone(),
            contract: contract.clone(),
        })
    }

    fn read_decision(&self, source: &ContentRef, payload: &Value) -> Result<DecisionReceipt> {
        self.check_activity_scope(payload)?;
        let curriculum = self.curriculum.as_ref().ok_or(Error::Phase)?;
        let activity = &payload["activity"];
        let contract = &payload["pedagogical_contract"];
        let id = text(payload, "decision_id")?;
        let concept_key = text(activity, "concept")?;
        let activity_type = text(activity, "type")?;
        let expected_bkt = match activity_type {
            "NEW_CONCEPT"
            | "PRACTICE"
            | "RECALL_EXERCISE"
            | "DEBUGGING_CASE"
            | "DEBUG_MISCONCEPTION"
            | "FEYNMAN_PROMPT" => "learning_opportunity",
            "DIAGNOSTIC_ASSESSMENT" | "MASTERY_CHALLENGE" | "TRANSFER_PROBE" => "observation_only",
            _ => return Err(Error::Contract),
        };
        if text(contract, "decision_id")? != id
            || text(contract, "target_concept")? != concept_key
            || text(contract, "recommended_activity_type")? != activity_type
            || positive(contract, "curriculum_version")? != curriculum.version
        {
            return Err(Error::Scope);
        }
        if text(contract, "learning_event_protocol")? != "response-feedback-v1"
            || text(contract, "policy_version")? != "2026-10-parameters-v6"
            || text(contract, "bkt_update_mode")? != expected_bkt
        {
            return Err(Error::Contract);
        }
        let competency = &contract["competency"];
        let matching = curriculum
            .concepts
            .iter()
            .find(|value| value["key"].as_str() == Some(concept_key))
            .ok_or(Error::Scope)?;
        if matching != competency || text(competency, "status")? != "active" {
            return Err(Error::Scope);
        }
        let mut outcomes = HashSet::new();
        // Go may serialize a nil slice as null; that means no outcomes, never
        // a fabricated outcome ID. Non-null values must be an actual array.
        let raw_outcomes = competency.get("outcomes").ok_or(Error::Receipt)?;
        if !raw_outcomes.is_null() {
            let values = raw_outcomes.as_array().ok_or(Error::Receipt)?;
            if values.len() > 256 {
                return Err(Error::Receipt);
            }
            for outcome in values {
                if !outcomes.insert(text(outcome, "id")?) {
                    return Err(Error::Receipt);
                }
                if !outcome
                    .get("statement")
                    .and_then(Value::as_str)
                    .is_some_and(|s| {
                        !s.trim().is_empty() && s.len() <= 16 * 1024 && !s.contains('\0')
                    })
                {
                    return Err(Error::Receipt);
                }
            }
        }
        let constraints = &contract["constraints"];
        let must = strings(&constraints["must_collect"], 64)?;
        strings(&constraints["avoid"], 64)?;
        if !must.contains(&"learner_answer") || !must.contains(&"rubric_score") {
            return Err(Error::Contract);
        }
        if activity_type == "DIAGNOSTIC_ASSESSMENT"
            && !must.contains(&"cold_response_before_feedback")
        {
            return Err(Error::Contract);
        }
        if activity_type == "TRANSFER_PROBE"
            && (!must.contains(&"transfer_dimension") || !must.contains(&"transfer_score"))
        {
            return Err(Error::Contract);
        }
        strings(&contract["allowed_variants"], 64)?;
        for key in [
            "can_change_format",
            "can_request_clarification",
            "can_propose_negotiation",
        ] {
            if contract["llm_discretion"][key].as_bool().is_none() {
                return Err(Error::Receipt);
            }
        }
        if contract["llm_discretion"]["cannot_mark_mastery_without_evidence"] != true {
            return Err(Error::Contract);
        }
        Ok(DecisionReceipt {
            source: source.clone(),
            id: id.into(),
            concept_key: concept_key.into(),
            activity_type: activity_type.into(),
            contract: contract.clone(),
        })
    }

    pub fn decision<'a>(
        &'a self,
        store: &SessionStore,
        current_host: &ContentRef,
        epoch: &str,
    ) -> Result<&'a DecisionReceipt> {
        self.check_scope(store, current_host, epoch)?;
        self.decision.as_ref().ok_or(Error::Phase)
    }

    pub fn terminal<'a>(
        &'a self,
        store: &SessionStore,
        current_host: &ContentRef,
        epoch: &str,
    ) -> Result<&'a TerminalReceipt> {
        self.check_scope(store, current_host, epoch)?;
        self.terminal.as_ref().ok_or(Error::Phase)
    }
}

fn authority_error(error: executor::Error) -> Error {
    match error {
        executor::Error::Storage => Error::Storage,
        _ => Error::Scope,
    }
}

fn bounded(value: &str, max: usize) -> bool {
    !value.is_empty()
        && value.trim() == value
        && value.len() <= max
        && !value.chars().any(char::is_control)
}
fn text<'a>(value: &'a Value, key: &str) -> Result<&'a str> {
    value
        .get(key)
        .and_then(Value::as_str)
        .filter(|v| bounded(v, 4096))
        .ok_or(Error::Receipt)
}
fn positive(value: &Value, key: &str) -> Result<u64> {
    value
        .get(key)
        .and_then(Value::as_u64)
        .filter(|&v| v > 0)
        .ok_or(Error::Receipt)
}
fn strings(value: &Value, limit: usize) -> Result<Vec<&str>> {
    let values = value
        .as_array()
        .filter(|v| v.len() <= limit)
        .ok_or(Error::Receipt)?;
    values
        .iter()
        .map(|v| {
            v.as_str()
                .filter(|s| bounded(s, 4096))
                .ok_or(Error::Receipt)
        })
        .collect()
}
fn require_open_turn(journal: &Journal, turn: &str) -> Result<()> {
    if journal.turn(turn).map_err(|_| Error::Storage)?.is_none()
        || journal
            .event(&format!("{turn}:finish"))
            .map_err(|_| Error::Storage)?
            .is_some()
    {
        return Err(Error::Scope);
    }
    Ok(())
}
fn proof(source: &ContentRef) -> Value {
    let kind = match source.kind() {
        ContentKind::Turn => "turn",
        ContentKind::Event => "event",
        ContentKind::OperationArguments => "operation_arguments",
        ContentKind::OperationResult => "operation_result",
    };
    json!({"kind":kind,"id":source.id(),"sha256":source.sha256().iter().map(|b| format!("{b:02x}")).collect::<String>()})
}

#[cfg(test)]
#[path = "pedagogical_tests.rs"]
mod tests;
