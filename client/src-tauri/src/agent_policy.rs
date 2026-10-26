// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! Scope Codex's tool surface to Tutor without changing the user's provider
//! login or writing their global configuration. The pinned ACP adapter consumes
//! CODEX_CONFIG as per-session app-server overrides; an unknown ACP metadata
//! field alone does not disable Codex's shell tools.
use crate::{discovery::AgentTarget, launch::LaunchSpec};
use serde_json::{json, Value};

pub fn restrict_tutor_agent(target: &AgentTarget, spec: &mut LaunchSpec) -> Result<(), String> {
    let is_codex = target.id == "bundled:codex"
        || target
            .adapter_path
            .as_deref()
            .is_some_and(|path| path.to_ascii_lowercase().contains("codex-acp"));
    if !is_codex {
        return Ok(());
    }
    let previous = std::env::var("CODEX_CONFIG").ok();
    let configuration = tutor_codex_config(previous.as_deref())?;
    spec.env.retain(|(name, _)| name != "CODEX_CONFIG");
    spec.env.push(("CODEX_CONFIG".into(), configuration));
    if target.environment == "wsl" {
        let existing = std::env::var("WSLENV").unwrap_or_default();
        spec.env.retain(|(name, _)| name != "WSLENV");
        spec.env.push(("WSLENV".into(), codex_wslenv(&existing)));
    }
    Ok(())
}

fn codex_wslenv(existing: &str) -> String {
    // JSON is not a path. Replace directional/path flags on an inherited
    // CODEX_CONFIG entry so our process override reaches Windows -> WSL.
    let mut entries: Vec<&str> = existing
        .split(':')
        .filter(|entry| !entry.is_empty() && entry.split('/').next() != Some("CODEX_CONFIG"))
        .collect();
    entries.push("CODEX_CONFIG");
    entries.join(":")
}

fn tutor_codex_config(previous: Option<&str>) -> Result<String, String> {
    let invalid = || "La configuration de l’agent Codex est invalide.".to_string();
    let mut configuration = match previous {
        Some(raw) if raw.len() > 64 * 1024 => return Err(invalid()),
        Some(raw) => serde_json::from_str::<Value>(raw).map_err(|_| invalid())?,
        None => json!({}),
    };
    let object = configuration.as_object_mut().ok_or_else(invalid)?;
    object.remove("features.shell_tool");
    object.remove("tools.view_image");
    let features = object.entry("features").or_insert_with(|| json!({}));
    features
        .as_object_mut()
        .ok_or_else(invalid)?
        .insert("shell_tool".into(), json!(false));
    let tools = object.entry("tools").or_insert_with(|| json!({}));
    tools
        .as_object_mut()
        .ok_or_else(invalid)?
        .insert("view_image".into(), json!(false));
    object.insert("web_search".into(), json!("disabled"));
    serde_json::to_string(&configuration).map_err(|_| invalid())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn tutor_overrides_remove_general_tools_and_preserve_provider_configuration() {
        let original = json!({"model":"user-model","model_provider":"user-provider","features":{"other_setting":true,"shell_tool":true},"tools":{"view_image":true},"web_search":"live","features.shell_tool":true});
        let encoded = original.to_string();
        let applied: Value =
            serde_json::from_str(&tutor_codex_config(Some(&encoded)).unwrap()).unwrap();
        assert_eq!(applied["features"]["shell_tool"], false);
        assert_eq!(applied["tools"]["view_image"], false);
        assert_eq!(applied["web_search"], "disabled");
        assert_eq!(applied["features"]["other_setting"], true);
        assert_eq!(applied["model"], "user-model");
        assert_eq!(applied["model_provider"], "user-provider");
        assert!(applied.get("features.shell_tool").is_none());
        assert_eq!(original["features"]["shell_tool"], true);
    }

    #[test]
    fn wsl_override_replaces_incompatible_direction_and_path_flags() {
        assert_eq!(
            codex_wslenv("OTHER/p:CODEX_CONFIG/w:CODEX_CONFIG/p:LAST/u"),
            "OTHER/p:LAST/u:CODEX_CONFIG"
        );
        assert_eq!(codex_wslenv(""), "CODEX_CONFIG");
        assert_eq!(codex_wslenv("CODEX_CONFIG:OTHER"), "OTHER:CODEX_CONFIG");
    }

    #[test]
    fn malformed_overrides_are_rejected_before_starting_a_process() {
        for raw in [
            "not-json",
            "[]",
            "null",
            r#"{"features":true}"#,
            r#"{"tools":null}"#,
        ] {
            assert!(tutor_codex_config(Some(raw)).is_err());
        }
    }
}
