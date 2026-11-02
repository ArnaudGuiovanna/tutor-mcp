// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! Host-admitted business consultations. Shape validation is NOT admission.
//! No classifier, tool routing, UI acknowledgement, permission policy or retry.
//! A private dispatch ticket predates preparation; historical ContentRefs can
//! never be upgraded into a current ScopedReceipt. Scope remains opaque.

#[cfg(test)]
use crate::library::EntryProof;
use crate::{
    call_validation::{self, Effect, ValidatedCall, VerifiedFields},
    catalog::ToolCatalog,
    executor::{self, Preparation, PreparedCall, WorkflowAuthority, WorkflowPermit},
    journal::{ContentKind, ContentRef, OperationState},
    library::LibraryRequest,
    mcp_client::Transport,
    mcp_session::Scope,
    session_store::SessionStore,
};
use serde_json::{json, Value};
use std::sync::atomic::AtomicBool;
use uuid::Uuid;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum Error {
    Request,
    Scope,
    Receipt,
    Storage,
    ExistingOperation,
    Validation(call_validation::Error),
    Execution(executor::Error),
}
impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            Self::Request => "consultation_request_invalid",
            Self::Scope => "consultation_scope_changed",
            Self::Receipt => "consultation_receipt_invalid",
            Self::Storage => "consultation_storage_unavailable",
            Self::ExistingOperation => "consultation_operation_already_exists",
            Self::Validation(_) => "consultation_call_invalid",
            Self::Execution(error) => return std::fmt::Display::fmt(error, f),
        })
    }
}
impl std::error::Error for Error {}
impl From<executor::Error> for Error {
    fn from(error: executor::Error) -> Self {
        Self::Execution(error)
    }
}
impl From<call_validation::Error> for Error {
    fn from(error: call_validation::Error) -> Self {
        Self::Validation(error)
    }
}

/// Association with EXISTING host scope and store metadata, not a second
/// identity/epoch. No digest, owner/credential getter or serialization exists.
/// Root must supply the Scope built from this store's verified Owner/session/
/// conversation. Scope intentionally offers no API to infer those identifiers.
#[derive(Clone)]
struct Origin {
    scope: Scope,
    instance: Uuid,
    session: String,
    conversation: Uuid,
}
impl Origin {
    fn capture(store: &SessionStore, scope: &Scope) -> Result<Self, Error> {
        Ok(Self {
            scope: scope.clone(),
            instance: store.journal().instance_id(),
            session: store.id().to_owned(),
            conversation: store
                .conversation_binding()
                .map_err(|_| Error::Storage)?
                .ok_or(Error::Scope)?,
        })
    }
    fn check(&self, store: &SessionStore, current: &Scope) -> Result<(), Error> {
        if &self.scope != current
            || self.instance != store.journal().instance_id()
            || self.session != store.id()
            || store.conversation_binding().map_err(|_| Error::Storage)? != Some(self.conversation)
        {
            return Err(Error::Scope);
        }
        Ok(())
    }
}

enum Request {
    FirstPage(LibraryRequest),
    #[cfg(test)]
    Details(Box<EntryProof>),
}

/// ONE request explicitly admitted by trusted host code. Not Deserialize,
/// not constructible from a model proposal/event, and consumed by preparation.
/// Constructors do not resolve semantics: the caller must have resolved the
/// current positive user request and any display/choice evidence beforehand.
/// ACP approval is still mandatory; no extra user confirmation is introduced.
pub(crate) struct AdmittedConsultation {
    origin: Origin,
    turn: String,
    anchor: ContentRef,
    authority: WorkflowAuthority,
    request: Request,
}
impl AdmittedConsultation {
    pub(crate) fn admit_first_page(
        store: &mut SessionStore,
        current: &Scope,
        turn: &str,
        request: LibraryRequest,
    ) -> Result<Self, Error> {
        Self::admit(store, current, turn, Request::FirstPage(request))
    }

    /// `proof` resolves an exact retained row, never a model ID. The host must
    /// separately establish that THIS row is the user's intended reference;
    /// proof validity alone does not imply current acknowledged UI selection.
    #[cfg(test)]
    pub(crate) fn admit_details(
        store: &mut SessionStore,
        current: &Scope,
        turn: &str,
        proof: EntryProof,
    ) -> Result<Self, Error> {
        proof.check(store, current)?;
        Self::admit(store, current, turn, Request::Details(Box::new(proof)))
    }

    fn admit(
        store: &mut SessionStore,
        current: &Scope,
        turn: &str,
        request: Request,
    ) -> Result<Self, Error> {
        let origin = Origin::capture(store, current)?;
        // Verify an existing, unfinished stored user turn, not arbitrary prose
        // passed as a pseudo turn. Semantics remain the host admission gate.
        store.read_turn(turn).map_err(|_| Error::Request)?;
        if store
            .journal()
            .event(&format!("{turn}:finish"))
            .map_err(|_| Error::Storage)?
            .is_some()
        {
            return Err(Error::Request);
        }
        let id = format!("{}:consultation", Uuid::new_v4());
        let capability = match &request {
            Request::FirstPage(_) => "training_library_first_page",
            #[cfg(test)]
            Request::Details(_) => "training_details",
        };
        store
            .journal_mut()
            .append_event(
                &id,
                Some(turn),
                "capability_consultation_started",
                &serde_json::to_vec(&json!({"version":1,"capability":capability}))
                    .map_err(|_| Error::Storage)?,
            )
            .map_err(|_| Error::Storage)?;
        let anchor = store
            .journal()
            .content_ref(ContentKind::Event, &id)
            .map_err(|_| Error::Storage)?;
        let authority = WorkflowAuthority::new_consultation(store.journal_mut(), turn, &anchor)?;
        Ok(Self {
            origin,
            turn: turn.to_owned(),
            anchor,
            authority,
            request,
        })
    }

    pub(crate) fn prepare(
        self,
        store: &mut SessionStore,
        current: &Scope,
        catalog: &ToolCatalog,
        binding: &str,
        operation: Uuid,
    ) -> Result<PreparedConsultation, Error> {
        self.origin.check(store, current)?;
        // Build a new evidence set for this stage. In particular do NOT inherit
        // conversation_id/expected_selection from active training A for query B.
        let (name, proposal, fields, expected) = match &self.request {
            Request::FirstPage(request) => {
                let args = request.first_arguments();
                (
                    "list_training_library",
                    args.clone(),
                    VerifiedFields::new(binding.to_owned(), store.journal())?,
                    args,
                )
            }
            #[cfg(test)]
            Request::Details(proof) => {
                let fields = proof.verified_fields(store, current, binding)?;
                (
                    "get_training",
                    json!({}),
                    fields,
                    proof.arguments(store, current)?,
                )
            }
        };
        let call = call_validation::validate_call(
            catalog,
            binding,
            &format!("tutor/{name}"),
            &proposal,
            &fields,
            operation,
        )?;
        if call.effect() != Effect::MutationWithoutReplayContract || call.arguments() != &expected {
            return Err(Error::Request);
        }
        // No await between ticket minting and preparation: &mut store and the
        // journal's exclusive file lock prevent another local preparer racing.
        let ticket = DispatchTicket::mint(store, &self.origin, current, &self.turn, &call)?;
        let event_id = format!("{}:consultation-precondition", call.operation_id());
        let bytes = serde_json::to_vec(&json!({"version":1,
            "operation_id":call.operation_id(),"fingerprint":call.fingerprint(),
            "tool_id":call.tool_id(),"authority_anchor_id":self.anchor.id(),
            "authority_anchor_sha256":self.anchor.sha256()}))
        .map_err(|_| Error::Storage)?;
        store
            .journal_mut()
            .append_event(
                &event_id,
                Some(&self.turn),
                "capability_consultation_precondition",
                &bytes,
            )
            .map_err(|_| Error::Storage)?;
        let reference = store
            .journal()
            .content_ref(ContentKind::Event, &event_id)
            .map_err(|_| Error::Storage)?;
        let permit = WorkflowPermit::from_consultation_event(
            store.journal(),
            &reference,
            &self.turn,
            &call,
            &self.authority,
        )?;
        let preparation = executor::prepare(store.journal_mut(), &self.turn, call, Some(permit))?;
        match preparation {
            Preparation::Ready(prepared) => Ok(PreparedConsultation {
                prepared: *prepared,
                ticket,
                authority: self.authority,
            }),
            // Historical completion can never mint a new scoped receipt.
            Preparation::Completed(_) => Err(Error::ExistingOperation),
        }
    }
}

/// Never returned to orchestration; no constructor from historical content.
struct DispatchTicket {
    origin: Origin,
    operation: String,
    turn: String,
    tool: String,
    arguments: Value,
    canonical_arguments: Vec<u8>,
}
impl DispatchTicket {
    fn mint(
        store: &SessionStore,
        origin: &Origin,
        current: &Scope,
        turn: &str,
        call: &ValidatedCall,
    ) -> Result<Self, Error> {
        origin.check(store, current)?;
        call.check_journal(store.journal())?;
        if store
            .journal()
            .operation(call.operation_id())
            .map_err(|_| Error::Storage)?
            .is_some()
            || store
                .journal()
                .event(&format!("{}:validated-contract", call.operation_id()))
                .map_err(|_| Error::Storage)?
                .is_some()
            || store
                .journal()
                .event(&format!(
                    "{}:consultation-precondition",
                    call.operation_id()
                ))
                .map_err(|_| Error::Storage)?
                .is_some()
        {
            return Err(Error::ExistingOperation);
        }
        // Retired operation IDs also remain rejected by journal.prepare_operation.
        Ok(Self {
            origin: origin.clone(),
            operation: call.operation_id().to_owned(),
            turn: turn.to_owned(),
            tool: call.tool_id().to_owned(),
            arguments: call.arguments().clone(),
            canonical_arguments: crate::journal::canonical_arguments(call.arguments())
                .map_err(|_| Error::Storage)?,
        })
    }
    fn capture(
        self,
        store: &SessionStore,
        current: &Scope,
        execution: executor::Execution,
    ) -> Result<ScopedReceipt, Error> {
        self.origin.check(store, current)?;
        if execution.replayed
            || execution.reference.kind() != ContentKind::OperationResult
            || execution.reference.id() != self.operation
        {
            return Err(Error::Receipt);
        }
        let receipt = ScopedReceipt {
            origin: self.origin,
            operation: self.operation,
            turn: self.turn,
            tool: self.tool,
            arguments: self.arguments,
            canonical_arguments: self.canonical_arguments,
            source: execution.reference,
        };
        let retained = receipt.envelope(store, current)?;
        if retained != execution.result {
            return Err(Error::Receipt);
        }
        // isError=true is not success, even when transport/RPC returned Ok.
        call_validation::result_payload(&retained).map_err(|_| Error::Receipt)?;
        Ok(receipt)
    }
}

/// Owns the live consultation authority during permission wait AND dispatch.
/// Dropping this object/future retires the guard. Existing executor records an
/// interrupted sent mutation as Dispatched/Unknown; no automatic replay follows.
pub(crate) struct PreparedConsultation {
    prepared: PreparedCall,
    ticket: DispatchTicket,
    authority: WorkflowAuthority,
}
impl PreparedConsultation {
    pub(crate) fn operation_id(&self) -> &str {
        self.prepared.operation_id()
    }
    pub(crate) fn permission_params(&self, session: &str) -> Value {
        self.prepared.permission_params(session)
    }
    pub(crate) fn revoke(&self, store: &mut SessionStore) -> Result<(), Error> {
        self.authority
            .revoke(store.journal_mut())
            .map_err(Error::from)
    }
    // Preserve the existing executor pass-through API and its distinct host
    // scope, live catalog, matched approval, transport and cancellation inputs.
    #[allow(clippy::too_many_arguments)]
    pub(crate) async fn execute<T: Transport>(
        self,
        store: &mut SessionStore,
        current: &Scope,
        catalog: &ToolCatalog,
        binding: &str,
        matching_acp_response: &Value,
        transport: &mut T,
        cancel: &AtomicBool,
    ) -> Result<ScopedReceipt, Error> {
        let Self {
            prepared,
            ticket,
            authority,
        } = self;
        ticket.origin.check(store, current)?;
        let execution = prepared
            .execute(
                store.journal_mut(),
                catalog,
                binding,
                matching_acp_response,
                transport,
                cancel,
            )
            .await?;
        let receipt = ticket.capture(store, current, execution);
        drop(authority); // Explicitly retain the guard through dispatch/capture.
        receipt
    }
}

/// Sealed by ONE fresh completed executor dispatch with a structured non-isError
/// reply. Business payload validation remains the typed decoder's responsibility.
/// Clone duplicates the same proof; it cannot relabel origin/arguments. Neither
/// serialized nor reconstructed after reopen. It is provenance scoped for
/// projection, NOT permission to execute another operation or disclose all data.
#[derive(Clone)]
pub(crate) struct ScopedReceipt {
    origin: Origin,
    operation: String,
    turn: String,
    tool: String,
    arguments: Value,
    canonical_arguments: Vec<u8>,
    source: ContentRef,
}
impl ScopedReceipt {
    #[cfg(test)]
    pub(crate) fn source(&self) -> &ContentRef {
        &self.source
    }
    pub(crate) fn tool_id(&self) -> &str {
        &self.tool
    }
    pub(crate) fn arguments(&self) -> &Value {
        &self.arguments
    }
    fn envelope(&self, store: &SessionStore, current: &Scope) -> Result<Value, Error> {
        self.origin.check(store, current)?;
        let operation = store
            .journal()
            .operation(&self.operation)
            .map_err(|_| Error::Storage)?
            .ok_or(Error::Receipt)?;
        if operation.state != OperationState::Completed
            || operation.tool != self.tool
            || operation.turn_id.as_deref() != Some(self.turn.as_str())
            || !operation.mutating
            || operation.idempotency_key.is_some()
            || operation.canonical_arguments != self.canonical_arguments
            || self.source.id() != self.operation
            || self.source.kind() != ContentKind::OperationResult
        {
            return Err(Error::Receipt);
        }
        serde_json::from_slice(
            &store
                .journal()
                .resolve_content(&self.source)
                .map_err(|_| Error::Receipt)?,
        )
        .map_err(|_| Error::Receipt)
    }
    pub(crate) fn payload(&self, store: &SessionStore, current: &Scope) -> Result<Value, Error> {
        call_validation::result_payload(&self.envelope(store, current)?).map_err(|_| Error::Receipt)
    }
}

#[cfg(test)]
mod tests;
