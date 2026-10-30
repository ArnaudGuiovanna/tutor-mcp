// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! Typed desktop context. The host constructs this after its authenticated
//! get_conversation_context request; learner text cannot populate this channel.

use serde::{Deserialize, Serialize};
use serde_json::Value;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct InvalidContext;

#[derive(Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct HostContext {
    pub conversation_id: String,
    pub language: String,
    pub training_context: Value,
}
impl HostContext {
    pub fn from_prompt(params: &Value, servers: &[Value]) -> Result<Option<Self>, InvalidContext> {
        let Some(raw) = params.pointer("/_meta/tutorLocal/task") else {
            return Ok(None);
        };
        if serde_json::to_vec(raw).map_or(true, |v| v.len() > 64 * 1024) {
            return Err(InvalidContext);
        }
        let value: Self = serde_json::from_value(raw.clone()).map_err(|_| InvalidContext)?;
        value.validate()?;
        // Bind the server's conversation scope, independent of model prose.
        let mut scoped = Vec::new();
        for server in servers
            .iter()
            .filter(|s| s.get("name").and_then(Value::as_str) == Some("tutor"))
        {
            if server.get("type").and_then(Value::as_str) == Some("http") {
                for header in server
                    .get("headers")
                    .and_then(Value::as_array)
                    .ok_or(InvalidContext)?
                {
                    if header
                        .get("name")
                        .and_then(Value::as_str)
                        .is_some_and(|s| s.eq_ignore_ascii_case("Tutor-Conversation-ID"))
                    {
                        scoped.push(
                            header
                                .get("value")
                                .and_then(Value::as_str)
                                .ok_or(InvalidContext)?,
                        );
                    }
                }
            } else {
                for env in server
                    .get("env")
                    .and_then(Value::as_array)
                    .ok_or(InvalidContext)?
                {
                    if env.get("name").and_then(Value::as_str) == Some("TUTOR_CONVERSATION_ID") {
                        scoped.push(
                            env.get("value")
                                .and_then(Value::as_str)
                                .ok_or(InvalidContext)?,
                        );
                    }
                }
            }
        }
        if scoped.as_slice() != [value.conversation_id.as_str()] {
            return Err(InvalidContext);
        }
        Ok(Some(value))
    }

    pub fn validate(&self) -> Result<(), InvalidContext> {
        let id = uuid::Uuid::parse_str(&self.conversation_id).map_err(|_| InvalidContext)?;
        if id.to_string() != self.conversation_id
            || !matches!(self.language.as_str(), "fr" | "en")
            || !self.training_context.is_object()
            || self
                .training_context
                .get("conversation_id")
                .and_then(Value::as_str)
                != Some(self.conversation_id.as_str())
        {
            return Err(InvalidContext);
        }
        for field in ["training_id", "domain_id", "revision", "name"] {
            let text = self
                .training_context
                .get(field)
                .and_then(Value::as_str)
                .ok_or(InvalidContext)?;
            if text.len() > 4096 || text.contains('\0') {
                return Err(InvalidContext);
            }
        }
        self.training_context
            .get("selection_version")
            .and_then(Value::as_u64)
            .ok_or(InvalidContext)?;
        Ok(())
    }
    pub fn conversation_uuid(&self) -> uuid::Uuid {
        uuid::Uuid::parse_str(&self.conversation_id).expect("validated host context")
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;
    #[test]
    fn metadata_must_match_the_scoped_mcp_connection_and_server_context() {
        let id = uuid::Uuid::new_v4().to_string();
        let task = json!({"conversationId":id,"language":"fr","trainingContext":{
            "conversation_id":id,"domain_id":"domain","training_id":"training","revision":"7","selection_version":2,"name":"Géométrie"}});
        let mut params = json!({"_meta":{"tutorLocal":{"task":task}}});
        let http = json!({"type":"http","name":"tutor","headers":[{"name":"Tutor-Conversation-ID","value":id},{"name":"Authorization","value":"Bearer private"}]});
        let stdio = json!({"name":"tutor","env":[{"name":"TUTOR_CONVERSATION_ID","value":id}]});
        for config in [http, stdio] {
            assert!(HostContext::from_prompt(&params, &[config])
                .unwrap()
                .is_some());
        }
        assert!(HostContext::from_prompt(&params, &[]).is_err());
        params["_meta"]["tutorLocal"]["task"]["trainingContext"]["conversation_id"] =
            json!(uuid::Uuid::new_v4().to_string());
        assert!(HostContext::from_prompt(&params, &[]).is_err());
        assert!(HostContext::from_prompt(&json!({}), &[]).unwrap().is_none());
    }
}
