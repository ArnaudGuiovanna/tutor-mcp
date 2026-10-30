// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! Structural boundary before ACP approval. Model proposals cannot supply
//! identities/versions without matching retained server evidence. This module
//! does not establish pedagogical preconditions or grant permission to execute.

use crate::catalog::ToolCatalog;
use crate::journal::{ContentKind, ContentRef, Journal, OperationState};
use serde_json::{json, Map, Value};
use sha2::{Digest, Sha256};
use std::collections::BTreeMap;
use std::fmt;

const MAX_ARGUMENT_BYTES: usize = 128 * 1024;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Error {
    StaleBinding,
    UnknownTool,
    UnsupportedContract,
    InvalidSchema,
    InvalidArguments,
    UnverifiedField,
    ConflictingField,
    InvalidEvidence,
    Storage,
}
impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            Self::StaleBinding => "tool_binding_changed",
            Self::UnknownTool => "tool_not_in_live_catalog",
            Self::UnsupportedContract => "tool_contract_not_supported",
            Self::InvalidSchema => "tool_schema_not_supported",
            Self::InvalidArguments => "tool_arguments_invalid",
            Self::UnverifiedField => "tool_field_needs_verified_evidence",
            Self::ConflictingField => "tool_field_conflicts_with_verified_evidence",
            Self::InvalidEvidence => "tool_evidence_invalid",
            Self::Storage => "tool_evidence_unavailable",
        })
    }
}
impl std::error::Error for Error {}

/// No Deserialize implementation: this object is constructed by the harness,
/// not from a model reply. Values originate in completed MCP operations in
/// this session's journal. Rebinding invalidates the entire field set.
pub struct VerifiedFields {
    journal_instance: uuid::Uuid,
    binding: String,
    fields: BTreeMap<String, Value>,
}
impl VerifiedFields {
    pub fn names(&self) -> Vec<String> {
        self.fields.keys().cloned().collect()
    }

    pub fn bind_host_context(
        &mut self,
        journal: &Journal,
        reference: &ContentRef,
        turn: &str,
        conversation: uuid::Uuid,
    ) -> Result<(), Error> {
        if journal.instance_id() != self.journal_instance || reference.kind() != ContentKind::Event
        {
            return Err(Error::InvalidEvidence);
        }
        let event = journal
            .event(reference.id())
            .map_err(|_| Error::Storage)?
            .ok_or(Error::InvalidEvidence)?;
        if event.kind != "verified_host_context" || event.turn_id.as_deref() != Some(turn) {
            return Err(Error::InvalidEvidence);
        }
        let bytes = journal
            .resolve_content(reference)
            .map_err(|_| Error::Storage)?;
        let context: crate::host_context::HostContext =
            serde_json::from_slice(&bytes).map_err(|_| Error::InvalidEvidence)?;
        context.validate().map_err(|_| Error::InvalidEvidence)?;
        if context.conversation_uuid() != conversation {
            return Err(Error::InvalidEvidence);
        }
        self.bind_conversation(conversation);
        for name in ["training_id", "domain_id", "revision"] {
            let value = &context.training_context[name];
            if value.as_str().is_some_and(|s| !s.is_empty()) {
                self.fields.insert(name.into(), value.clone());
            }
        }
        self.fields.insert(
            "expected_selection".into(),
            context.training_context["selection_version"].clone(),
        );
        Ok(())
    }
    pub fn new(binding: String, journal: &Journal) -> Result<Self, Error> {
        if binding.is_empty() || binding.len() > 4096 {
            return Err(Error::StaleBinding);
        }
        Ok(Self {
            journal_instance: journal.instance_id(),
            binding,
            fields: BTreeMap::new(),
        })
    }

    /// The desktop owns conversation UUIDs; they are not proposed by Qwen.
    pub fn bind_conversation(&mut self, conversation: uuid::Uuid) {
        self.fields
            .insert("conversation_id".into(), json!(conversation.to_string()));
    }

    /// Pointer is relative to the server's structured payload. Preserve the
    /// original MCP envelope in the journal; never duplicate content/text in
    /// the model projection. Only explicit, code-owned mappings are accepted.
    pub fn bind_result(
        &mut self,
        journal: &Journal,
        reference: &ContentRef,
        mappings: &[(&str, &str)],
    ) -> Result<(), Error> {
        if journal.instance_id() != self.journal_instance
            || reference.kind() != ContentKind::OperationResult
            || mappings.len() > 32
        {
            return Err(Error::InvalidEvidence);
        }
        let op = journal
            .operation(reference.id())
            .map_err(|_| Error::Storage)?
            .ok_or(Error::InvalidEvidence)?;
        if op.state != OperationState::Completed || !op.tool.starts_with("tutor/") {
            return Err(Error::InvalidEvidence);
        }
        let bytes = journal
            .resolve_content(reference)
            .map_err(|_| Error::Storage)?;
        let envelope: Value = serde_json::from_slice(&bytes).map_err(|_| Error::InvalidEvidence)?;
        let payload = result_payload(&envelope)?;
        let mut added = Vec::new();
        for (key, pointer) in mappings {
            if key.is_empty()
                || key.len() > 128
                || key.contains('/')
                || !key.bytes().all(|b| b.is_ascii_alphanumeric() || b == b'_')
                || pointer.len() > 1024
                || !pointer.starts_with('/')
            {
                return Err(Error::InvalidEvidence);
            }
            let value = payload
                .pointer(pointer)
                .filter(|v| !v.is_null())
                .ok_or(Error::InvalidEvidence)?;
            if serde_json::to_vec(value).map_or(true, |v| v.len() > 32 * 1024) {
                return Err(Error::InvalidEvidence);
            }
            added.push(((*key).to_owned(), value.clone()));
        }
        // A missing later field must not leave a partially refreshed binding.
        for (key, value) in added {
            self.fields.insert(key, value);
        }
        Ok(())
    }
}

/// A server error is never usable as an authority record. Structured content
/// takes precedence; a text-only fallback must contain one unambiguous JSON
/// object. No prose summarization occurs at this boundary.
pub fn result_payload(envelope: &Value) -> Result<Value, Error> {
    if !envelope.is_object()
        || envelope
            .get("isError")
            .is_some_and(|v| v.as_bool() != Some(false))
    {
        return Err(Error::InvalidEvidence);
    }
    if let Some(value) = envelope.get("structuredContent") {
        return value
            .is_object()
            .then(|| value.clone())
            .ok_or(Error::InvalidEvidence);
    }
    let content = envelope
        .get("content")
        .and_then(Value::as_array)
        .ok_or(Error::InvalidEvidence)?;
    if content.len() != 1 || content[0].get("type").and_then(Value::as_str) != Some("text") {
        return Err(Error::InvalidEvidence);
    }
    let text = content[0]
        .get("text")
        .and_then(Value::as_str)
        .ok_or(Error::InvalidEvidence)?;
    let payload: Value = serde_json::from_str(text).map_err(|_| Error::InvalidEvidence)?;
    payload
        .is_object()
        .then_some(payload)
        .ok_or(Error::InvalidEvidence)
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Effect {
    ReadOnly,
    /// tools/idempotency.go explicitly includes this business operation.
    MiddlewareKey,
    /// record_learning_event uses its business event_key, not middleware.
    LearningEventKey,
    /// A read-oriented operation which may write; never automatically retry.
    MutationWithoutReplayContract,
}
impl Effect {
    pub fn mutating(self) -> bool {
        self != Self::ReadOnly
    }
}

/// Explicit reviewed contracts, never inferred from name prefixes or MCP
/// readOnlyHint. Expansion requires reviewing the real server handler.
fn effect(tool: &str) -> Result<Effect, Error> {
    match tool {
        "get_learner_context"
        | "get_pending_alerts"
        | "get_dashboard_state"
        | "get_olm_snapshot"
        | "get_misconceptions"
        | "get_pedagogical_snapshots"
        | "get_autonomy_metrics"
        | "get_availability_model"
        | "get_goal_relevance"
        | "list_implementation_intentions"
        | "read_raw_session"
        | "check_mastery"
        | "get_decision_replay_summary"
        | "validate_domain_graph" => Ok(Effect::ReadOnly),
        "get_conversation_context"
        | "get_curriculum_snapshot"
        | "get_memory_state"
        | "get_metacognitive_mirror" => Ok(Effect::MutationWithoutReplayContract),
        "start_learning_session"
        | "get_next_activity"
        | "prepare_assessment_attempt"
        | "submit_assessment_attempt"
        | "cancel_assessment_attempt"
        | "record_interaction"
        | "record_session_close"
        | "record_affect" => Ok(Effect::MiddlewareKey),
        "record_learning_event" => Ok(Effect::LearningEventKey),
        _ => Err(Error::UnsupportedContract),
    }
}

pub(crate) fn protected(key: &str) -> bool {
    key == "id"
        || key.ends_with("_id")
        || key.ends_with("_ids")
        || key.ends_with("_version")
        || matches!(
            key,
            "concept"
                | "activity_type"
                | "revision"
                | "version"
                | "expected_selection"
                | "expected_revision"
                | "learner_response"
                | "rubric_json"
                | "evaluator_id"
                | "response_time_seconds"
                | "hints_used"
                | "hints_requested"
                | "response_content_hash"
                | "task_content_hash"
                // read_raw_session addresses a retained session by timestamp.
                | "timestamp"
                | "evaluation_method"
                | "evaluation_provenance_json"
                | "interpretation_brief"
                | "self_initiated"
                | "event_key"
                | "key"
                | "idempotency_key"
        )
}

/// Private fields and no Deserialize/Clone. A validated proposal still needs
/// workflow preconditions, a live ACP approval and a durable dispatch permit.
pub struct ValidatedCall {
    journal_instance: uuid::Uuid,
    operation_id: String,
    schema: Value,
    binding: String,
    tool_id: String,
    name: String,
    args: Value,
    key: Option<String>,
    effect: Effect,
    fingerprint: String,
}
impl ValidatedCall {
    pub fn check_journal(&self, journal: &Journal) -> Result<(), Error> {
        (self.journal_instance == journal.instance_id())
            .then_some(())
            .ok_or(Error::InvalidEvidence)
    }
    pub fn operation_id(&self) -> &str {
        &self.operation_id
    }
    pub fn tool_id(&self) -> &str {
        &self.tool_id
    }
    pub fn name(&self) -> &str {
        &self.name
    }
    pub fn arguments(&self) -> &Value {
        &self.args
    }
    pub fn idempotency_key(&self) -> Option<&str> {
        self.key.as_deref()
    }
    pub fn effect(&self) -> Effect {
        self.effect
    }
    pub fn fingerprint(&self) -> &str {
        &self.fingerprint
    }
    pub fn binding(&self) -> &str {
        &self.binding
    }

    pub fn check_current(&self, catalog: &ToolCatalog, binding: &str) -> Result<(), Error> {
        if self.binding != binding {
            return Err(Error::StaleBinding);
        }
        let entry = catalog
            .resolve(binding, &self.tool_id)
            .map_err(|_| Error::StaleBinding)?;
        if entry.input_schema != self.schema {
            return Err(Error::StaleBinding);
        }
        Ok(())
    }

    pub fn permission_params(&self, session: &str, call_id: &str) -> Value {
        json!({"sessionId":session,"_meta":{"is_mcp_tool_approval":true},
            "toolCall":{"toolCallId":call_id,"title":self.name,"kind":"other","status":"pending",
                "rawInput":{"server":"tutor","tool":self.name,"arguments":self.args}},
            "options":[{"kind":"allow_once","optionId":"allow","name":"Autoriser"},
                {"kind":"reject_once","optionId":"reject","name":"Refuser"}]})
    }
}

pub fn validate_call(
    catalog: &ToolCatalog,
    binding: &str,
    tool_id: &str,
    proposal: &Value,
    fields: &VerifiedFields,
    operation: uuid::Uuid,
) -> Result<ValidatedCall, Error> {
    if fields.binding != binding {
        return Err(Error::StaleBinding);
    }
    let entry = catalog
        .resolve(binding, tool_id)
        .map_err(|_| Error::UnknownTool)?;
    if entry.server_id != "tutor" {
        return Err(Error::UnsupportedContract);
    }
    let effect = effect(&entry.name)?;
    let mut args: Map<String, Value> = proposal
        .as_object()
        .cloned()
        .ok_or(Error::InvalidArguments)?;
    if args.len() > 128
        || serde_json::to_vec(proposal).map_or(true, |v| v.len() > MAX_ARGUMENT_BYTES)
    {
        return Err(Error::InvalidArguments);
    }
    // The operation identity is issued by the harness and persisted before
    // any dispatch. A model cannot choose a key for a different operation.
    if args.contains_key("idempotency_key") || args.contains_key("event_key") {
        return Err(Error::UnverifiedField);
    }
    let properties = entry
        .input_schema
        .get("properties")
        .and_then(Value::as_object);
    for (key, value) in &fields.fields {
        if properties.is_some_and(|p| p.contains_key(key)) {
            if args.get(key).is_some_and(|proposed| proposed != value) {
                return Err(Error::ConflictingField);
            }
            args.insert(key.clone(), value.clone());
        }
    }
    for (key, value) in &args {
        if protected(key) {
            match fields.fields.get(key) {
                None => return Err(Error::UnverifiedField),
                Some(expected) if expected != value => return Err(Error::ConflictingField),
                _ => (),
            }
        }
    }
    let operation = operation.to_string();
    let key = match effect {
        Effect::MiddlewareKey => Some(operation.clone()),
        Effect::LearningEventKey => {
            args.insert("event_key".into(), json!(operation.clone()));
            None
        }
        _ => None,
    };
    let args = Value::Object(args);
    if serde_json::to_vec(&args).map_or(true, |v| v.len() > MAX_ARGUMENT_BYTES) {
        return Err(Error::InvalidArguments);
    }
    // Cargo disables both network and filesystem schema resolution. Built-in
    // metaschemas and local $defs are available; unknown external refs fail.
    let validator =
        jsonschema::validator_for(&entry.input_schema).map_err(|_| Error::InvalidSchema)?;
    if !validator.is_valid(&args) {
        return Err(Error::InvalidArguments);
    }
    let fingerprint = format!(
        "{:x}",
        Sha256::digest(
            serde_json::to_vec(&json!([binding, tool_id, entry.input_schema, args, key]))
                .map_err(|_| Error::InvalidArguments)?
        )
    );
    Ok(ValidatedCall {
        journal_instance: fields.journal_instance,
        operation_id: operation,
        schema: entry.input_schema.clone(),
        binding: binding.into(),
        tool_id: tool_id.into(),
        name: entry.name.clone(),
        args,
        key,
        effect,
        fingerprint,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::catalog::ToolEntry;
    use crate::journal::{Binding, OperationRequest};

    fn catalog(name: &str, schema: Value) -> ToolCatalog {
        let mut catalog = ToolCatalog::new("epoch1".into()).unwrap();
        catalog
            .replace_server_tools(
                "epoch1",
                "tutor",
                vec![ToolEntry {
                    server_id: "tutor".into(),
                    name: name.into(),
                    description: "Fixture tool".into(),
                    input_schema: schema,
                    annotations: json!({"readOnlyHint":true}),
                }],
            )
            .unwrap();
        catalog
    }
    fn journal(root: &std::path::Path) -> Journal {
        Journal::open(
            &root.join("fixture.sqlite"),
            &Binding {
                profile_id: "p",
                account_id: "a",
                tenant_id: "t",
                server_id: "s",
            },
        )
        .unwrap()
    }
    fn result(journal: &mut Journal, id: &str, value: Value) -> ContentRef {
        journal
            .prepare_operation(OperationRequest {
                id,
                turn_id: None,
                tool: "tutor/get_conversation_context",
                arguments: &json!({}),
                idempotency_key: None,
                mutating: true,
            })
            .unwrap();
        let _permit = journal.mark_dispatched(id).unwrap();
        journal
            .complete_operation(id, &serde_json::to_vec(&value).unwrap())
            .unwrap();
        journal
            .content_ref(ContentKind::OperationResult, id)
            .unwrap()
    }
    fn schema() -> Value {
        json!({"type":"object","additionalProperties":false,
        "properties":{"domain_id":{"type":"string"},"intent":{"enum":["auto","review"]}},
        "required":["domain_id","intent"]})
    }

    #[test]
    fn identity_requires_completed_server_evidence_and_conflicts_are_rejected() {
        let root = tempfile::tempdir().unwrap();
        let mut journal = journal(root.path());
        let reference = result(
            &mut journal,
            "context",
            json!({"structuredContent":{"domain_id":"verified-domain"}}),
        );
        let mut fields = VerifiedFields::new("epoch1".into(), &journal).unwrap();
        let catalog = catalog("get_next_activity", schema());
        let id = uuid::Uuid::new_v4();
        assert!(matches!(
            validate_call(
                &catalog,
                "epoch1",
                "tutor/get_next_activity",
                &json!({"domain_id":"invented","intent":"auto"}),
                &fields,
                id
            ),
            Err(Error::UnverifiedField)
        ));
        fields
            .bind_result(&journal, &reference, &[("domain_id", "/domain_id")])
            .unwrap();
        assert!(matches!(
            validate_call(
                &catalog,
                "epoch1",
                "tutor/get_next_activity",
                &json!({"domain_id":"invented","intent":"auto"}),
                &fields,
                id
            ),
            Err(Error::ConflictingField)
        ));
        let call = validate_call(
            &catalog,
            "epoch1",
            "tutor/get_next_activity",
            &json!({"intent":"auto"}),
            &fields,
            id,
        )
        .unwrap();
        assert_eq!(call.arguments()["domain_id"], "verified-domain");
        assert_eq!(call.idempotency_key(), Some(id.to_string().as_str()));
        assert_eq!(
            call.effect(),
            Effect::MiddlewareKey,
            "get_next_activity mutates even if the annotation says otherwise"
        );
        assert_eq!(
            call.permission_params("session", "call")["toolCall"]["rawInput"]["tool"],
            "get_next_activity"
        );
        assert!(matches!(
            validate_call(
                &catalog,
                "epoch2",
                "tutor/get_next_activity",
                &json!({"intent":"auto"}),
                &fields,
                id
            ),
            Err(Error::StaleBinding)
        ));
    }

    #[test]
    fn raw_session_timestamp_requires_verified_evidence_and_matching_contract() {
        let root = tempfile::tempdir().unwrap();
        let mut journal = journal(root.path());
        let mut fields = VerifiedFields::new("epoch1".into(), &journal).unwrap();
        let timestamp = "2026-10-30T09:15:00Z";
        let raw_session = catalog(
            "read_raw_session",
            json!({"type":"object","properties":{"timestamp":{"type":"string"}},
                "required":["timestamp"],"additionalProperties":false}),
        );
        let id = uuid::Uuid::new_v4();

        // Even a syntactically valid timestamp cannot address a retained session
        // merely because the model supplied it.
        assert!(matches!(
            validate_call(
                &raw_session,
                "epoch1",
                "tutor/read_raw_session",
                &json!({"timestamp":timestamp}),
                &fields,
                id
            ),
            Err(Error::UnverifiedField)
        ));

        // Tutor's get_memory_state exposes newest_session in its JSON payload.
        // Exercise the retained MCP receipt and text-JSON decoding, not direct
        // insertion into VerifiedFields or a check of protected() itself.
        let envelope = json!({"isError":false,"content":[{"type":"text","text":
            json!({"ok":true,"session_count":1,"oldest_session":timestamp,
                "newest_session":timestamp}).to_string()}]});
        journal
            .prepare_operation(OperationRequest {
                id: "memory-state",
                turn_id: None,
                tool: "tutor/get_memory_state",
                arguments: &json!({}),
                idempotency_key: None,
                mutating: true,
            })
            .unwrap();
        let _permit = journal.mark_dispatched("memory-state").unwrap();
        journal
            .complete_operation("memory-state", &serde_json::to_vec(&envelope).unwrap())
            .unwrap();
        let reference = journal
            .content_ref(ContentKind::OperationResult, "memory-state")
            .unwrap();
        fields
            .bind_result(&journal, &reference, &[("timestamp", "/newest_session")])
            .unwrap();

        let call = validate_call(
            &raw_session,
            "epoch1",
            "tutor/read_raw_session",
            &json!({}),
            &fields,
            id,
        )
        .unwrap();
        assert_eq!(call.arguments(), &json!({"timestamp":timestamp}));
        assert!(matches!(
            validate_call(
                &raw_session,
                "epoch1",
                "tutor/read_raw_session",
                &json!({"timestamp":"2026-10-29T09:15:00Z"}),
                &fields,
                id
            ),
            Err(Error::ConflictingField)
        ));

        // Retaining a session address must not inject it into unrelated tools
        // whose live contract does not accept that argument.
        let alerts = catalog(
            "get_pending_alerts",
            json!({"type":"object","properties":{},"additionalProperties":false}),
        );
        let call = validate_call(
            &alerts,
            "epoch1",
            "tutor/get_pending_alerts",
            &json!({}),
            &fields,
            uuid::Uuid::new_v4(),
        )
        .unwrap();
        assert_eq!(call.arguments(), &json!({}));
    }

    #[test]
    fn invalid_schema_arguments_and_external_references_fail_closed() {
        let root = tempfile::tempdir().unwrap();
        let journal = journal(root.path());
        let fields = VerifiedFields::new("epoch1".into(), &journal).unwrap();
        let id = uuid::Uuid::new_v4();
        let typed = catalog(
            "get_pending_alerts",
            json!({"type":"object","properties":{"limit":{"type":"integer","minimum":1,"maximum":10}},"additionalProperties":false}),
        );
        for args in [
            json!({"limit":"5"}),
            json!({"limit":11}),
            json!({"limit":5,"extra":1}),
            json!({"idempotency_key":"model-key"}),
        ] {
            assert!(validate_call(
                &typed,
                "epoch1",
                "tutor/get_pending_alerts",
                &args,
                &fields,
                id
            )
            .is_err());
        }
        assert!(validate_call(
            &typed,
            "epoch1",
            "tutor/get_pending_alerts",
            &json!({"limit":5}),
            &fields,
            id
        )
        .is_ok());
        for reference in [
            "file:///tmp/private-schema.json",
            "https://example.invalid/schema",
        ] {
            let external = catalog(
                "get_pending_alerts",
                json!({"type":"object","$ref":reference}),
            );
            assert!(matches!(
                validate_call(
                    &external,
                    "epoch1",
                    "tutor/get_pending_alerts",
                    &json!({}),
                    &fields,
                    id
                ),
                Err(Error::InvalidSchema)
            ));
        }
        let unknown = catalog("future_mutation", json!({"type":"object"}));
        assert!(matches!(
            validate_call(
                &unknown,
                "epoch1",
                "tutor/future_mutation",
                &json!({}),
                &fields,
                id
            ),
            Err(Error::UnsupportedContract)
        ));
    }

    #[test]
    fn failed_results_and_partial_mappings_never_become_authority() {
        let root = tempfile::tempdir().unwrap();
        let mut journal = journal(root.path());
        let good = result(
            &mut journal,
            "good",
            json!({"structuredContent":{"domain_id":"verified"}}),
        );
        let bad = result(
            &mut journal,
            "bad",
            json!({"isError":true,"structuredContent":{"domain_id":"error"}}),
        );
        let mut fields = VerifiedFields::new("epoch1".into(), &journal).unwrap();
        assert!(fields
            .bind_result(&journal, &bad, &[("domain_id", "/domain_id")])
            .is_err());
        assert!(fields
            .bind_result(
                &journal,
                &good,
                &[("domain_id", "/domain_id"), ("session_id", "/missing")]
            )
            .is_err());
        assert!(fields.fields.is_empty());
        let payload = result_payload(
            &json!({"structuredContent":{"x":1},"content":[{"type":"text","text":"{\"x\":1}"}]}),
        )
        .unwrap();
        assert_eq!(payload, json!({"x":1}));
        assert_eq!(
            result_payload(&json!({"content":[{"type":"text","text":"{\"x\":1}"}]})).unwrap(),
            json!({"x":1})
        );
        assert!(result_payload(&json!({"content":[{"type":"text","text":"just prose"}]})).is_err());
    }

    #[test]
    fn response_hashes_hints_and_cross_journal_proofs_cannot_be_fabricated() {
        let root = tempfile::tempdir().unwrap();
        let mut journal = journal(root.path());
        let other_root = tempfile::tempdir().unwrap();
        let mut other = super::tests::journal(other_root.path());
        let mut fields = VerifiedFields::new("epoch1".into(), &journal).unwrap();
        let evidence = result(
            &mut journal,
            "attempt",
            json!({"structuredContent":{"attempt_id":"verified-attempt","domain_id":"verified-domain"}}),
        );
        fields
            .bind_result(
                &journal,
                &evidence,
                &[("attempt_id", "/attempt_id"), ("domain_id", "/domain_id")],
            )
            .unwrap();
        let wrong = result(
            &mut other,
            "attempt",
            json!({"structuredContent":{"attempt_id":"different-attempt"}}),
        );
        assert_eq!(
            fields.bind_result(&other, &wrong, &[("attempt_id", "/attempt_id")]),
            Err(Error::InvalidEvidence)
        );
        for (tool, field, value) in [
            (
                "submit_assessment_attempt",
                "response_content_hash",
                json!("a".repeat(64)),
            ),
            (
                "prepare_assessment_attempt",
                "task_content_hash",
                json!("b".repeat(64)),
            ),
            ("record_interaction", "hints_requested", json!(42)),
            (
                "record_interaction",
                "evaluation_method",
                json!("trusted_human"),
            ),
        ] {
            let schema = json!({"type":"object","properties":{"attempt_id":{"type":"string"},field:{}},"additionalProperties":false});
            let catalog = catalog(tool, schema);
            assert!(matches!(
                validate_call(
                    &catalog,
                    "epoch1",
                    &format!("tutor/{tool}"),
                    &json!({field:value}),
                    &fields,
                    uuid::Uuid::new_v4()
                ),
                Err(Error::UnverifiedField)
            ));
        }
        // Even a permissive schema cannot bypass equality with a frozen field
        // by omitting that field from its properties annotation.
        let catalog = catalog("get_learner_context", json!({"type":"object"}));
        assert!(matches!(
            validate_call(
                &catalog,
                "epoch1",
                "tutor/get_learner_context",
                &json!({"domain_id":"invented"}),
                &fields,
                uuid::Uuid::new_v4()
            ),
            Err(Error::ConflictingField)
        ));
    }
}
