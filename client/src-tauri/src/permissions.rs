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
        crate::native_auth::validate_origin(&self.origin)?;
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
            && crate::native_auth::validate_origin(&self.origin).ok()
                == crate::native_auth::validate_origin(origin).ok()
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

    fn decision(&self, context: &PermissionContext<'_>, tool: &str) -> PermissionDecision {
        if !self.matches(context.origin, context.institution_id, context.agent_id)
            || !context.known_tools.contains(tool)
        {
            return PermissionDecision::Ask;
        }
        self.tools
            .get(tool)
            .copied()
            .unwrap_or(self.default_decision)
    }
}

/// Keep only identity facts from trusted adapter events, never arguments or
/// responses. Keys include the ACP session so an old call cannot authorize a
/// new conversation which happens to reuse its tool-call identifier.
#[derive(Default)]
pub struct ToolCallRegistry {
    calls: HashMap<(String, String), String>,
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
        } else if let Some(tool) = canonical_tool(update) {
            if self.calls.len() >= 4096 {
                self.calls.clear();
            }
            self.calls.insert(key, tool.to_owned());
        }
    }

    pub fn clear_session(&mut self, session: &str) {
        self.calls.retain(|(owner, _), _| owner != session);
    }

    pub fn request_tool<'a>(&'a self, event: &'a Value) -> Option<&'a str> {
        if event.get("method").and_then(Value::as_str) != Some("session/request_permission") {
            return None;
        }
        let session = event.pointer("/params/sessionId")?.as_str()?;
        let call = event.pointer("/params/toolCall")?;
        if let Some(tool) = canonical_tool(call) {
            return Some(tool);
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
                .map(String::as_str);
        }
        None
    }
}

pub fn canonical_tool(call: &Value) -> Option<&str> {
    // Codex's MCP reporter supplies these fields from the actual MCP item.
    if call.pointer("/rawInput/server").and_then(Value::as_str) == Some("tutor") {
        let tool = call.pointer("/rawInput/tool")?.as_str()?;
        return valid_name(tool).then_some(tool);
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
    valid_name(tool).then_some(tool)
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
    let tool = registry.request_tool(event)?;
    let decision = policy?.decision(context, tool);
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
        registry.observe(&json!({"method":"session/update","params":{"sessionId":"session1","update":{"sessionUpdate":"tool_call","toolCallId":"call1","title":"anything","rawInput":{"server":"tutor","tool":"get_next_activity","arguments":{"private":"not retained"}},"status":"pending"}}}));
        registry
    }

    #[test]
    fn persisted_consent_is_bounded_to_origin_institution_agent_and_real_tool() {
        let names = BTreeSet::from(["get_next_activity".into()]);
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
        let names = BTreeSet::from(["get_next_activity".into()]);
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
            .insert("get_next_activity".into(), PermissionDecision::Deny);
        assert_eq!(
            automatic_outcome(Some(&policy), &context, &registry, &request("session1")),
            Some(json!({"outcome":"selected","optionId":"reject"}))
        );
        policy
            .tools
            .insert("get_next_activity".into(), PermissionDecision::Ask);
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
        let names = BTreeSet::from(["get_next_activity".into()]);
        let context = PermissionContext {
            origin: "https://school.example",
            institution_id: "institution1",
            agent_id: "bundled:codex",
            known_tools: &names,
        };
        let mut request = request("session1");
        request["params"]["toolCall"]["title"] = json!("mcp.tutor.get_next_activity");
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
        let names = BTreeSet::from(["get_next_activity".into()]);
        let context = PermissionContext {
            origin: "https://school.example",
            institution_id: "institution1",
            agent_id: "bundled:codex",
            known_tools: &names,
        };
        let mut request = request("session1");
        request["params"]["toolCall"]["_meta"] = json!({"claudeCode":{"toolName":"mcp__tutor__get_next_activity","mcpServer":{"name":"tutor","source":"sdk"}}});
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
