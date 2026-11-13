// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! Account-local Tutor tool consent. These preferences answer only a verified
//! MCP permission request; the institution continues to enforce every right.
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use std::collections::{BTreeMap, BTreeSet, HashMap};

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum PermissionDecision {
    #[default]
    Ask,
    Allow,
    Deny,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct PermissionPolicy {
    pub origin: String,
    pub institution_id: String,
    pub agent_id: String,
    pub default_decision: PermissionDecision,
    #[serde(default)]
    pub tools: BTreeMap<String, PermissionDecision>,
}

pub struct PermissionContext<'a> {
    pub origin: &'a str,
    pub institution_id: &'a str,
    pub agent_id: &'a str,
    pub known_tools: &'a BTreeSet<String>,
}

fn valid_name(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 128
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || b"_.-".contains(&byte))
}

impl PermissionPolicy {
    pub fn validate(&self) -> Result<(), String> {
        if self.origin != "local://tutor" {
            crate::native_auth::validate_origin(&self.origin)?;
        }
        if self.institution_id.is_empty()
            || self.institution_id.len() > 256
            || self.agent_id.is_empty()
            || self.agent_id.len() > 512
            || self.institution_id.chars().any(char::is_control)
            || self.agent_id.chars().any(char::is_control)
            || self.tools.len() > 4096
            || self.tools.keys().any(|name| !valid_name(name))
            || serde_json::to_vec(&self.tools).map_or(true, |bytes| bytes.len() > 60 * 1024)
        {
            return Err("Politique d’autorisation MCP invalide.".into());
        }
        Ok(())
    }

    pub fn matches(&self, origin: &str, institution: &str, agent: &str) -> bool {
        self.validate().is_ok()
            && if self.origin == "local://tutor" || origin == "local://tutor" {
                self.origin == origin
            } else {
                crate::native_auth::validate_origin(&self.origin).ok()
                    == crate::native_auth::validate_origin(origin).ok()
            }
            && self.institution_id == institution
            && self.agent_id == agent
    }

    pub fn transport_header(&self, origin: &str, institution: &str, agent: &str) -> Option<String> {
        if !self.matches(origin, institution, agent) {
            return None;
        }
        let header = serde_json::to_string(&json!({
            "default":self.default_decision,
            "tools":self.tools,
        }))
        .ok()?;
        (header.len() <= 64 * 1024).then_some(header)
    }

    fn decision(
        &self,
        context: &PermissionContext<'_>,
        tool: &str,
        operation: &ToolOperation,
    ) -> PermissionDecision {
        if !self.matches(context.origin, context.institution_id, context.agent_id)
            || !context.known_tools.contains(tool)
        {
            return PermissionDecision::Ask;
        }
        self.decide(tool, operation)
    }

    /// Resolve a rule as the server does: an explicit bare-name deny blocks
    /// every operation, then `<tool>.<operation>`, the bare name and the
    /// default. A merged tool whose operation cannot be read never reaches an
    /// allow through its bare name while operation rules exist: it asks.
    pub fn decide(&self, tool: &str, operation: &ToolOperation) -> PermissionDecision {
        let bare = self.tools.get(tool).copied();
        if bare == Some(PermissionDecision::Deny) {
            return PermissionDecision::Deny;
        }
        let fallback = bare.unwrap_or(self.default_decision);
        match operation {
            ToolOperation::Selected(operation) => self
                .tools
                .get(&format!("{tool}.{operation}"))
                .copied()
                .unwrap_or(fallback),
            ToolOperation::Undetermined
                if fallback != PermissionDecision::Deny && self.has_operation_rules(tool) =>
            {
                PermissionDecision::Ask
            }
            ToolOperation::Undetermined | ToolOperation::NotApplicable => fallback,
        }
    }

    fn has_operation_rules(&self, tool: &str) -> bool {
        self.tools.keys().any(|key| {
            key.strip_prefix(tool)
                .is_some_and(|rest| rest.starts_with('.'))
        })
    }
}

/// Tools merging several operations, with the argument selecting one, as in
/// the server's `toolPolicySelectors`.
const OPERATION_SELECTORS: &[(&str, &str)] = &[
    ("manage_domain", "action"),
    ("edit_training", "action"),
    ("manage_training_lifecycle", "action"),
    ("prepare_pedagogical_turn", "mode"),
];

fn operation_selector(tool: &str) -> Option<&'static str> {
    OPERATION_SELECTORS
        .iter()
        .find(|(name, _)| *name == tool)
        .map(|(_, field)| *field)
}

/// The operation a call addresses: the only argument-derived fact retained.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum ToolOperation {
    /// The tool has a single operation; only its bare name applies.
    NotApplicable,
    /// A validated selector value (`action` or `mode`).
    Selected(String),
    /// A merged tool whose selector is absent, garbled or invisible.
    Undetermined,
}

impl ToolOperation {
    /// Read only the selecting argument of a merged tool. `arguments` is the
    /// call input when the adapter exposes it; `None` means it is not visible.
    pub fn of(tool: &str, arguments: Option<&Value>) -> Self {
        let Some(field) = operation_selector(tool) else {
            return Self::NotApplicable;
        };
        let value = match arguments {
            Some(Value::Object(fields)) => fields.get(field),
            Some(Value::Null) => None,
            _ => return Self::Undetermined,
        };
        let mode = field == "mode";
        match value {
            // An omitted mode is "context" on the server; an omitted action
            // selects no operation and is rejected by its handler.
            None | Some(Value::Null) if mode => Self::Selected("context".into()),
            Some(Value::String(value)) if mode && value.is_empty() => {
                Self::Selected("context".into())
            }
            Some(Value::String(value)) if valid_operation(value) => Self::Selected(value.clone()),
            _ => Self::Undetermined,
        }
    }

    /// Interpret a policy-style key (`tool` or `tool.operation`) naming one
    /// operation that Tutor itself is about to call.
    pub fn from_key(key: &str) -> (&str, Self) {
        match key.split_once('.') {
            Some((tool, operation))
                if operation_selector(tool).is_some() && valid_operation(operation) =>
            {
                (tool, Self::Selected(operation.into()))
            }
            Some(_) => (key, Self::Undetermined),
            None if operation_selector(key).is_some() => (key, Self::Undetermined),
            None => (key, Self::NotApplicable),
        }
    }
}

fn valid_operation(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 64
        && value
            .bytes()
            .all(|byte| byte.is_ascii_lowercase() || byte.is_ascii_digit() || b"_-".contains(&byte))
}

/// Keep only identity facts from trusted adapter events, never arguments or
/// responses: the tool name and, for a merged tool, its validated operation
/// selector. Keys include the ACP session so an old call cannot authorize a
/// new conversation which happens to reuse its tool-call identifier.
#[derive(Default)]
pub struct ToolCallRegistry {
    calls: HashMap<(String, String), (String, ToolOperation)>,
}

impl ToolCallRegistry {
    pub fn observe(&mut self, event: &Value) {
        if event.get("method").and_then(Value::as_str) != Some("session/update") {
            return;
        }
        let Some(session) = event.pointer("/params/sessionId").and_then(Value::as_str) else {
            return;
        };
        let Some(update) = event.pointer("/params/update") else {
            return;
        };
        if !matches!(
            update.get("sessionUpdate").and_then(Value::as_str),
            Some("tool_call" | "tool_call_update")
        ) {
            return;
        }
        let Some(id) = update.get("toolCallId").and_then(Value::as_str) else {
            return;
        };
        let key = (session.to_owned(), id.to_owned());
        if matches!(
            update.get("status").and_then(Value::as_str),
            Some("completed" | "failed" | "cancelled")
        ) {
            self.calls.remove(&key);
        } else if let Some((tool, operation)) = canonical_call(update) {
            if self.calls.len() >= 4096 {
                self.calls.clear();
            }
            self.calls.insert(key, (tool.to_owned(), operation));
        }
    }

    pub fn clear_session(&mut self, session: &str) {
        self.calls.retain(|(owner, _), _| owner != session);
    }

    pub fn request_tool<'a>(&'a self, event: &'a Value) -> Option<(&'a str, ToolOperation)> {
        if event.get("method").and_then(Value::as_str) != Some("session/request_permission") {
            return None;
        }
        let session = event.pointer("/params/sessionId")?.as_str()?;
        let call = event.pointer("/params/toolCall")?;
        if let Some(call) = canonical_call(call) {
            return Some(call);
        }
        // Codex's approval repeats the correlated call ID, not the tool input.
        // A generic MCP elicitation/question must still reach the person.
        if event
            .pointer("/params/_meta/is_mcp_tool_approval")?
            .as_bool()?
        {
            let id = call.get("toolCallId")?.as_str()?;
            return self
                .calls
                .get(&(session.to_owned(), id.to_owned()))
                .map(|(tool, operation)| (tool.as_str(), operation.clone()));
        }
        None
    }
}

pub fn canonical_tool(call: &Value) -> Option<&str> {
    canonical_call(call).map(|(tool, _)| tool)
}

/// The Tutor tool a trusted adapter item names, with the operation selected
/// by its input. Nothing else from the input is read.
pub fn canonical_call(call: &Value) -> Option<(&str, ToolOperation)> {
    // Codex's MCP reporter supplies these fields from the actual MCP item.
    if call.pointer("/rawInput/server").and_then(Value::as_str) == Some("tutor") {
        let tool = call.pointer("/rawInput/tool")?.as_str()?;
        let operation = ToolOperation::of(tool, call.pointer("/rawInput/arguments"));
        return valid_name(tool).then_some((tool, operation));
    }
    // Claude explicitly identifies both its MCP server and the full tool name.
    let meta = call.pointer("/_meta/claudeCode")?;
    if meta.pointer("/mcpServer/name")?.as_str()? != "tutor" {
        return None;
    }
    let tool = meta
        .get("toolName")?
        .as_str()?
        .strip_prefix("mcp__tutor__")?;
    // Claude's ACP adapter reports the tool input itself as `rawInput`.
    let operation = ToolOperation::of(tool, call.get("rawInput"));
    valid_name(tool).then_some((tool, operation))
}

/// Return an offered one-call outcome. Persistent consent belongs to Tutor's
/// settings, so changing it immediately affects future requests even if an
/// adapter's own `allow_always` would otherwise outlive that change.
pub fn automatic_outcome(
    policy: Option<&PermissionPolicy>,
    context: &PermissionContext<'_>,
    registry: &ToolCallRegistry,
    event: &Value,
) -> Option<Value> {
    let (tool, operation) = registry.request_tool(event)?;
    let decision = policy?.decision(context, tool, &operation);
    let kind = match decision {
        PermissionDecision::Ask => return None,
        PermissionDecision::Allow => "allow_once",
        PermissionDecision::Deny => "reject_once",
    };
    let options = event.pointer("/params/options")?.as_array()?;
    let offered = options.iter().find(|option| {
        option.get("kind").and_then(Value::as_str) == Some(kind)
            && option
                .get("optionId")
                .and_then(Value::as_str)
                .is_some_and(|id| !id.is_empty())
    });
    match offered {
        Some(option) => Some(json!({"outcome":"selected","optionId":option["optionId"]})),
        None if decision == PermissionDecision::Deny => Some(json!({"outcome":"cancelled"})),
        None => None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn policy() -> PermissionPolicy {
        PermissionPolicy {
            origin: "https://school.example".into(),
            institution_id: "institution1".into(),
            agent_id: "bundled:codex".into(),
            default_decision: PermissionDecision::Allow,
            tools: BTreeMap::new(),
        }
    }

    fn request(session: &str) -> Value {
        json!({"method":"session/request_permission","id":7,"params":{"sessionId":session,"_meta":{"is_mcp_tool_approval":true},"toolCall":{"toolCallId":"call1"},"options":[{"kind":"allow_once","optionId":"allow"},{"kind":"allow_always","optionId":"persistent"},{"kind":"reject_once","optionId":"reject"}]}})
    }

    fn registered() -> ToolCallRegistry {
        let mut registry = ToolCallRegistry::default();
        registry.observe(&json!({"method":"session/update","params":{"sessionId":"session1","update":{"sessionUpdate":"tool_call","toolCallId":"call1","title":"anything","rawInput":{"server":"tutor","tool":"get_learner_state","arguments":{"private":"not retained"}},"status":"pending"}}}));
        registry
    }

    #[test]
    fn local_consent_cannot_authorize_another_installation_or_remote_server() {
        let mut local = policy();
        local.origin = "local://tutor".into();
        local.institution_id = "installation-one".into();
        assert!(local.validate().is_ok());
        assert!(local.matches("local://tutor", "installation-one", &local.agent_id));
        assert!(!local.matches("local://tutor", "installation-two", &local.agent_id));
        assert!(!local.matches(
            "https://school.example",
            "installation-one",
            &local.agent_id
        ));
        assert!(!local.matches("invalid", "installation-one", &local.agent_id));
        assert!(!local.matches("local://tutor", "installation-one", "another-harness"));
        assert!(!policy().matches("local://tutor", "institution1", "bundled:codex"));
    }

    #[test]
    fn persisted_consent_is_bounded_to_origin_institution_agent_and_real_tool() {
        let names = BTreeSet::from(["get_learner_state".into()]);
        let context = PermissionContext {
            origin: "https://school.example",
            institution_id: "institution1",
            agent_id: "bundled:codex",
            known_tools: &names,
        };
        let persisted = serde_json::to_string(&policy()).unwrap();
        let mut restored: PermissionPolicy = serde_json::from_str(&persisted).unwrap();
        let registry = registered();
        assert_eq!(
            automatic_outcome(Some(&restored), &context, &registry, &request("session1")),
            Some(json!({"outcome":"selected","optionId":"allow"}))
        );
        for different in ["https://other.example", "http://localhost:9000"] {
            restored.origin = different.into();
            assert!(
                automatic_outcome(Some(&restored), &context, &registry, &request("session1"))
                    .is_none()
            );
        }
        restored = policy();
        restored.institution_id = "another".into();
        assert!(
            automatic_outcome(Some(&restored), &context, &registry, &request("session1")).is_none()
        );
        restored = policy();
        restored.agent_id = "bundled:claude".into();
        assert!(
            automatic_outcome(Some(&restored), &context, &registry, &request("session1")).is_none()
        );
        restored = policy();
        let empty = BTreeSet::new();
        let unknown = PermissionContext {
            known_tools: &empty,
            ..context
        };
        assert!(
            automatic_outcome(Some(&restored), &unknown, &registry, &request("session1")).is_none()
        );
    }

    #[test]
    fn revocation_refusal_and_session_reuse_do_not_inherit_an_allow() {
        let names = BTreeSet::from(["get_learner_state".into()]);
        let context = PermissionContext {
            origin: "https://school.example",
            institution_id: "institution1",
            agent_id: "bundled:codex",
            known_tools: &names,
        };
        let mut policy = policy();
        let mut registry = registered();
        assert!(
            automatic_outcome(Some(&policy), &context, &registry, &request("different")).is_none()
        );
        policy
            .tools
            .insert("get_learner_state".into(), PermissionDecision::Deny);
        assert_eq!(
            automatic_outcome(Some(&policy), &context, &registry, &request("session1")),
            Some(json!({"outcome":"selected","optionId":"reject"}))
        );
        policy
            .tools
            .insert("get_learner_state".into(), PermissionDecision::Ask);
        assert!(
            automatic_outcome(Some(&policy), &context, &registry, &request("session1")).is_none()
        );
        policy.tools.clear();
        registry.clear_session("session1");
        assert!(
            automatic_outcome(Some(&policy), &context, &registry, &request("session1")).is_none()
        );
    }

    #[test]
    fn titles_and_mcp_questions_never_authorize_tools() {
        let names = BTreeSet::from(["get_learner_state".into()]);
        let context = PermissionContext {
            origin: "https://school.example",
            institution_id: "institution1",
            agent_id: "bundled:codex",
            known_tools: &names,
        };
        let mut request = request("session1");
        request["params"]["toolCall"]["title"] = json!("mcp.tutor.get_learner_state");
        assert!(automatic_outcome(
            Some(&policy()),
            &context,
            &ToolCallRegistry::default(),
            &request
        )
        .is_none());
        request["params"]["_meta"] = json!({});
        assert!(automatic_outcome(Some(&policy()), &context, &registered(), &request).is_none());
    }

    #[test]
    fn claude_requires_canonical_server_metadata_and_an_offered_one_call_option() {
        let names = BTreeSet::from(["get_learner_state".into()]);
        let context = PermissionContext {
            origin: "https://school.example",
            institution_id: "institution1",
            agent_id: "bundled:codex",
            known_tools: &names,
        };
        let mut request = request("session1");
        request["params"]["toolCall"]["_meta"] = json!({"claudeCode":{"toolName":"mcp__tutor__get_learner_state","mcpServer":{"name":"tutor","source":"sdk"}}});
        assert!(automatic_outcome(
            Some(&policy()),
            &context,
            &ToolCallRegistry::default(),
            &request
        )
        .is_some());
        request["params"]["options"] = json!([{"kind":"allow_always","optionId":"persistent"}]);
        assert!(automatic_outcome(
            Some(&policy()),
            &context,
            &ToolCallRegistry::default(),
            &request
        )
        .is_none());
        request["params"]["toolCall"]["_meta"]["claudeCode"]["mcpServer"]["name"] =
            json!("another-server");
        assert!(automatic_outcome(Some(&policy()), &context, &registered(), &request).is_none());
    }

    fn operation_policy(
        default: PermissionDecision,
        rules: &[(&str, PermissionDecision)],
    ) -> PermissionPolicy {
        let mut policy = policy();
        policy.default_decision = default;
        policy.tools = rules
            .iter()
            .map(|(name, decision)| ((*name).to_owned(), *decision))
            .collect();
        policy
    }

    fn codex_decision(
        policy: &PermissionPolicy,
        tool: &str,
        arguments: Option<Value>,
    ) -> PermissionDecision {
        let names = BTreeSet::from([tool.to_owned(), "get_learner_state".to_owned()]);
        let context = PermissionContext {
            origin: "https://school.example",
            institution_id: "institution1",
            agent_id: "bundled:codex",
            known_tools: &names,
        };
        let mut raw = json!({"server":"tutor","tool":tool});
        if let Some(arguments) = arguments {
            raw["arguments"] = arguments;
        }
        // Codex's approval only repeats the call ID: the operation must come
        // from the observed tool call.
        let mut registry = ToolCallRegistry::default();
        registry.observe(&json!({"method":"session/update","params":{"sessionId":"session1","update":{"sessionUpdate":"tool_call","toolCallId":"call1","rawInput":raw,"status":"pending"}}}));
        match automatic_outcome(Some(policy), &context, &registry, &request("session1")) {
            None => PermissionDecision::Ask,
            Some(outcome) if outcome["optionId"] == "allow" => PermissionDecision::Allow,
            Some(_) => PermissionDecision::Deny,
        }
    }

    #[test]
    fn an_operation_rule_overrides_the_bare_tool_for_that_operation_only() {
        use PermissionDecision::*;
        let policy = operation_policy(
            Ask,
            &[("manage_domain", Allow), ("manage_domain.delete", Ask)],
        );
        let call = |action: &str| {
            codex_decision(
                &policy,
                "manage_domain",
                Some(json!({"action":action,"domain_id":"d1"})),
            )
        };
        assert_eq!(call("delete"), Ask);
        assert_eq!(call("archive"), Allow);
        assert_eq!(call("list"), Allow);
        // Claude reports the input itself as rawInput on its permission request.
        let names = BTreeSet::from(["manage_domain".to_owned()]);
        let context = PermissionContext {
            origin: "https://school.example",
            institution_id: "institution1",
            agent_id: "bundled:codex",
            known_tools: &names,
        };
        let mut request = request("session1");
        request["params"]["toolCall"]["_meta"] = json!({"claudeCode":{"toolName":"mcp__tutor__manage_domain","mcpServer":{"name":"tutor"}}});
        request["params"]["toolCall"]["rawInput"] = json!({"action":"delete"});
        let empty = ToolCallRegistry::default();
        assert!(automatic_outcome(Some(&policy), &context, &empty, &request).is_none());
        request["params"]["toolCall"]["rawInput"] = json!({"action":"archive"});
        assert_eq!(
            automatic_outcome(Some(&policy), &context, &empty, &request),
            Some(json!({"outcome":"selected","optionId":"allow"}))
        );
    }

    #[test]
    fn a_bare_deny_blocks_every_operation_even_with_an_operation_allow() {
        use PermissionDecision::*;
        let policy = operation_policy(
            Allow,
            &[
                ("manage_training_lifecycle", Deny),
                ("manage_training_lifecycle.archive", Allow),
            ],
        );
        for arguments in [
            Some(json!({"action":"archive"})),
            Some(json!({"action":"withdraw"})),
            Some(json!({"action":7})),
            Some(json!({})),
            None,
        ] {
            assert_eq!(
                codex_decision(&policy, "manage_training_lifecycle", arguments),
                Deny
            );
        }
    }

    #[test]
    fn an_operation_allow_applies_under_a_default_ask() {
        use PermissionDecision::*;
        let policy = operation_policy(Ask, &[("edit_training.preview", Allow)]);
        let call =
            |action: &str| codex_decision(&policy, "edit_training", Some(json!({"action":action})));
        assert_eq!(call("preview"), Allow);
        assert_eq!(call("save"), Ask);
        let denied = operation_policy(Allow, &[("edit_training.save", Deny)]);
        assert_eq!(
            codex_decision(&denied, "edit_training", Some(json!({"action":"save"}))),
            Deny
        );
        assert_eq!(
            codex_decision(&denied, "edit_training", Some(json!({"action":"propose"}))),
            Allow
        );
    }

    #[test]
    fn an_omitted_pedagogical_mode_is_the_context_operation() {
        use PermissionDecision::*;
        let policy = operation_policy(
            Ask,
            &[
                ("prepare_pedagogical_turn.context", Allow),
                ("prepare_pedagogical_turn.activity", Deny),
            ],
        );
        let call = |arguments| codex_decision(&policy, "prepare_pedagogical_turn", arguments);
        assert_eq!(call(Some(json!({"conversation_id":"c1"}))), Allow);
        assert_eq!(call(Some(json!({"mode":""}))), Allow);
        assert_eq!(call(Some(json!({"mode":null}))), Allow);
        assert_eq!(call(Some(Value::Null)), Allow);
        assert_eq!(call(Some(json!({"mode":"context"}))), Allow);
        assert_eq!(call(Some(json!({"mode":"activity"}))), Deny);
        // Invisible arguments are not an omitted mode.
        assert_eq!(call(None), Ask);
        assert_eq!(
            ToolOperation::from_key("prepare_pedagogical_turn.activity"),
            (
                "prepare_pedagogical_turn",
                ToolOperation::Selected("activity".into())
            )
        );
        assert_eq!(
            policy.decide(
                "prepare_pedagogical_turn",
                &ToolOperation::from_key("prepare_pedagogical_turn.activity").1
            ),
            Deny
        );
    }

    #[test]
    fn a_missing_or_garbled_operation_asks_when_operation_rules_exist() {
        use PermissionDecision::*;
        let policy = operation_policy(
            Allow,
            &[("manage_domain", Allow), ("manage_domain.delete", Ask)],
        );
        for arguments in [
            Some(json!({})),
            Some(json!({"action":""})),
            Some(json!({"action":"DELETE"})),
            Some(json!({"action":"delete everything"})),
            Some(json!({"action":"delete.now"})),
            Some(json!({"action":["delete"]})),
            Some(json!({"action":"x".repeat(65)})),
            Some(json!("{\"action\":\"delete\"}")),
            None,
        ] {
            assert_eq!(codex_decision(&policy, "manage_domain", arguments), Ask);
        }
        // Without operation rules the bare name decides, as before.
        let bare = operation_policy(Ask, &[("manage_domain", Allow)]);
        assert_eq!(codex_decision(&bare, "manage_domain", None), Allow);
        // An undetermined operation never escapes a deny it would fall back to.
        let closed = operation_policy(Deny, &[("manage_domain.list", Allow)]);
        assert_eq!(codex_decision(&closed, "manage_domain", None), Deny);
        assert_eq!(
            codex_decision(&closed, "manage_domain", Some(json!({"action":"list"}))),
            Allow
        );
    }

    #[test]
    fn tools_without_operations_ignore_dotted_rules_and_arguments() {
        use PermissionDecision::*;
        let policy = operation_policy(
            Ask,
            &[
                ("get_learner_state", Allow),
                ("get_learner_state.action", Deny),
                ("manage_domainx.delete", Deny),
            ],
        );
        assert!(policy.validate().is_ok());
        for arguments in [Some(json!({"action":"action"})), Some(json!(7)), None] {
            assert_eq!(
                codex_decision(&policy, "get_learner_state", arguments),
                Allow
            );
        }
        // A prefix of another tool's name is not an operation rule.
        let prefixed = operation_policy(Allow, &[("manage_domainx.delete", Ask)]);
        assert_eq!(codex_decision(&prefixed, "manage_domain", None), Allow);
        // Dotted keys survive persistence and the transport header.
        let restored: PermissionPolicy =
            serde_json::from_str(&serde_json::to_string(&policy).unwrap()).unwrap();
        assert_eq!(restored.tools, policy.tools);
        let header = policy
            .transport_header("https://school.example", "institution1", "bundled:codex")
            .unwrap();
        assert!(header.contains("\"get_learner_state.action\":\"deny\""));
    }

    #[test]
    fn the_registry_retains_only_the_tool_and_its_operation() {
        let mut registry = ToolCallRegistry::default();
        registry.observe(&json!({"method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"tool_call","toolCallId":"c","rawInput":{"server":"tutor","tool":"manage_domain","arguments":{"action":"delete","domain_id":"PRIVATE"}},"status":"pending"}}}));
        let stored = registry.calls.values().cloned().collect::<Vec<_>>();
        assert_eq!(
            stored,
            vec![(
                "manage_domain".to_owned(),
                ToolOperation::Selected("delete".into())
            )]
        );
        assert!(!format!("{stored:?}").contains("PRIVATE"));
    }

    #[test]
    fn malformed_policies_fail_before_they_can_be_saved() {
        let mut policy = policy();
        policy.origin = "https://school.example/other".into();
        assert!(policy.validate().is_err());
        policy = super::tests::policy();
        policy
            .tools
            .insert("read all files".into(), PermissionDecision::Allow);
        assert!(policy.validate().is_err());
    }
}
