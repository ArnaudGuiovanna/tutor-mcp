// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! Scope Codex's tool surface to Tutor without changing the user's provider
//! login or writing their global configuration. The pinned ACP adapter consumes
//! CODEX_CONFIG as per-session app-server overrides; an unknown ACP metadata
//! field alone does not disable Codex's shell tools.
use crate::{discovery::AgentTarget, launch::LaunchSpec};
use serde_json::{json, Value};
use std::path::{Path, PathBuf};

/// Whether the user already chose a Claude model or provider (Bedrock,
/// Vertex, gateway...) that an injected default would override or break.
/// Reads the process environment, the launch environment and, best effort,
/// the settings files the Claude adapter resolves. Unknown means "chosen":
/// an unreadable file or an environment we cannot inspect (WSL) disables
/// every automatic model default.
pub fn claude_user_model(target: &AgentTarget, spec: &LaunchSpec) -> bool {
    target.environment != "native"
        || claude_user_choice(
            &launch_vars(spec),
            spec.cwd.as_deref(),
            &claude_managed_settings(),
        )
}

/// Whether the user already chose a Codex model, provider or profile in the
/// environment, CODEX_CONFIG or a config.toml the Codex app-server reads.
pub fn codex_user_model(target: &AgentTarget, spec: &LaunchSpec) -> bool {
    target.environment != "native" || codex_user_choice(&launch_vars(spec), spec.cwd.as_deref())
}

/// Process environment overlaid with the launch environment (last wins).
fn launch_vars(spec: &LaunchSpec) -> Vec<(String, String)> {
    let mut vars: Vec<(String, String)> = std::env::vars().collect();
    vars.extend(spec.env.iter().cloned());
    vars
}

fn lookup<'a>(vars: &'a [(String, String)], name: &str) -> Option<&'a str> {
    vars.iter()
        .rev()
        .find(|(key, _)| key == name)
        .map(|(_, value)| value.as_str())
        .filter(|value| !value.trim().is_empty())
}

fn truthy(value: &str) -> bool {
    let value = value.trim().to_ascii_lowercase();
    !value.is_empty() && value != "0" && value != "false" && value != "no" && value != "off"
}

fn home(vars: &[(String, String)]) -> Option<PathBuf> {
    lookup(vars, "HOME")
        .or_else(|| lookup(vars, "USERPROFILE"))
        .map(PathBuf::from)
}

/// Environment keys that select a Claude model or a non-default provider.
fn claude_env_selects<'a>(mut vars: impl Iterator<Item = (&'a str, &'a str)>) -> bool {
    vars.any(|(key, value)| {
        let value = value.trim();
        match key {
            "ANTHROPIC_MODEL" | "ANTHROPIC_BASE_URL" => !value.is_empty(),
            _ => key.starts_with("CLAUDE_CODE_USE_") && truthy(value),
        }
    })
}

/// Result of a best-effort settings read.
enum Settings<T> {
    Missing,
    Unreadable,
    Read(T),
}

fn read_settings(path: &Path) -> Settings<String> {
    const LIMIT: u64 = 1024 * 1024;
    match std::fs::metadata(path) {
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Settings::Missing,
        Err(_) => return Settings::Unreadable,
        Ok(metadata) if !metadata.is_file() || metadata.len() > LIMIT => {
            return Settings::Unreadable
        }
        Ok(_) => {}
    }
    match std::fs::read_to_string(path) {
        Ok(text) => Settings::Read(text),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Settings::Missing,
        Err(_) => Settings::Unreadable,
    }
}

fn claude_managed_settings() -> PathBuf {
    // Same per-platform location as the Claude adapter (settings.js).
    PathBuf::from(if cfg!(target_os = "macos") {
        "/Library/Application Support/ClaudeCode/managed-settings.json"
    } else if cfg!(windows) {
        "C:\\Program Files\\ClaudeCode\\managed-settings.json"
    } else {
        "/etc/claude-code/managed-settings.json"
    })
}

fn claude_user_choice(vars: &[(String, String)], cwd: Option<&Path>, managed: &Path) -> bool {
    // Last value per key decides, as in the spawned process environment.
    let mut keys: Vec<&str> = vars.iter().map(|(key, _)| key.as_str()).collect();
    keys.sort_unstable();
    keys.dedup();
    if claude_env_selects(
        keys.into_iter()
            .filter_map(|key| lookup(vars, key).map(|value| (key, value))),
    ) {
        return true;
    }
    let Some(config) = lookup(vars, "CLAUDE_CONFIG_DIR")
        .map(PathBuf::from)
        .or_else(|| home(vars).map(|home| home.join(".claude")))
    else {
        return true;
    };
    let mut files = vec![config.join("settings.json"), managed.to_path_buf()];
    if let Some(cwd) = cwd {
        // The adapter resolves the initial model with project settings too.
        files.push(cwd.join(".claude").join("settings.json"));
        files.push(cwd.join(".claude").join("settings.local.json"));
    }
    files.iter().any(|file| match read_settings(file) {
        Settings::Missing => false,
        Settings::Unreadable => true,
        Settings::Read(text) => match serde_json::from_str::<Value>(&text) {
            Ok(settings) => claude_settings_select(&settings),
            Err(_) => true,
        },
    })
}

fn claude_settings_select(settings: &Value) -> bool {
    let Some(settings) = settings.as_object() else {
        return true;
    };
    if settings
        .get("model")
        .and_then(Value::as_str)
        .is_some_and(|model| !model.trim().is_empty())
    {
        return true;
    }
    settings
        .get("env")
        .and_then(Value::as_object)
        .is_some_and(|env| {
            claude_env_selects(
                env.iter()
                    .filter_map(|(key, value)| Some((key.as_str(), value.as_str()?))),
            )
        })
}

/// Keys of the user's Claude settings that a Tutor session keeps although the
/// user settings file is not loaded as a source: credential helpers, login
/// pins, and the model/provider choice the adapter resolves from the same file
/// (session-model.js assumes the CLI sees the same `model`).
const CLAUDE_INHERITED_KEYS: [&str; 9] = [
    "apiKeyHelper",
    "awsAuthRefresh",
    "awsCredentialExport",
    "gcpAuthRefresh",
    "proxyAuthHelper",
    "forceLoginMethod",
    "forceLoginOrgUUID",
    "model",
    "modelOverrides",
];

/// Variables of the settings `env` block that select or authenticate the
/// provider, or reach it through the network (proxy, private CA).
fn claude_inherited_env(key: &str) -> bool {
    const EXACT: [&str; 11] = [
        "HTTP_PROXY",
        "HTTPS_PROXY",
        "NO_PROXY",
        "http_proxy",
        "https_proxy",
        "no_proxy",
        "NODE_EXTRA_CA_CERTS",
        "API_TIMEOUT_MS",
        "CLOUD_ML_REGION",
        "CLAUDE_CODE_API_KEY_HELPER_TTL_MS",
        "CLAUDE_CODE_OAUTH_TOKEN",
    ];
    const PREFIXES: [&str; 7] = [
        "ANTHROPIC_",
        "AWS_",
        "CLAUDE_CODE_USE_",
        "CLAUDE_CODE_SKIP_",
        "CLAUDE_CODE_CLIENT_",
        "VERTEX_REGION_",
        "GOOGLE_",
    ];
    EXACT.contains(&key) || PREFIXES.iter().any(|prefix| key.starts_with(prefix))
}

/// Loopback names the MCP relay can be reached by: a proxy must never
/// receive its requests nor its local bearer.
pub const LOOPBACK_NO_PROXY: [&str; 3] = ["127.0.0.1", "localhost", "::1"];

/// Union of comma-separated NO_PROXY lists, in order and without duplicates,
/// always followed by the loopback names missing from it.
pub fn no_proxy_with_loopback<'a>(lists: impl IntoIterator<Item = &'a str>) -> String {
    let mut entries: Vec<&str> = Vec::new();
    for entry in lists
        .into_iter()
        .flat_map(|list| list.split(','))
        .map(str::trim)
        .chain(LOOPBACK_NO_PROXY)
    {
        if !entry.is_empty() && !entries.contains(&entry) {
            entries.push(entry);
        }
    }
    entries.join(",")
}

/// The Claude CLI applies its settings `env` (here the flag tier,
/// `options.settings.env`, which also outranks a user settings file) over its
/// process environment. Both NO_PROXY spellings of that tier are set to the
/// process value (`process`, loopback included) merged with whatever the
/// inherited settings name, so a user proxy configuration can never route the
/// loopback relay through a proxy.
pub fn protect_loopback_no_proxy(options: &mut Value, process: &str) {
    if !options["settings"].is_object() {
        options["settings"] = json!({});
    }
    if !options["settings"]["env"].is_object() {
        options["settings"]["env"] = json!({});
    }
    let env = &mut options["settings"]["env"];
    let merged = no_proxy_with_loopback(
        [
            Some(process),
            env["NO_PROXY"].as_str(),
            env["no_proxy"].as_str(),
        ]
        .into_iter()
        .flatten(),
    );
    env["NO_PROXY"] = json!(merged);
    env["no_proxy"] = json!(merged);
}

/// Authentication and provider settings a native Claude session receives in
/// its flag tier (`options.settings`) instead of loading the user's settings
/// file, so that the user's plugins (`enabledPlugins`), permission rules
/// (`permissions.allow`…) and hooks never apply to Tutor.
///
/// None keeps the previous behaviour (`settingSources: ["user"]`): a WSL
/// harness reads a settings file this process cannot read reliably, and an
/// unreadable or invalid file must not silently drop the user's login.
pub fn claude_inherited_settings(target: &AgentTarget, spec: &LaunchSpec) -> Option<Value> {
    if target.environment != "native" {
        return None;
    }
    let vars = launch_vars(spec);
    let config = lookup(&vars, "CLAUDE_CONFIG_DIR")
        .map(PathBuf::from)
        .or_else(|| home(&vars).map(|home| home.join(".claude")))?;
    claude_inheritance(&config.join("settings.json"))
}

fn claude_inheritance(path: &Path) -> Option<Value> {
    let text = match read_settings(path) {
        Settings::Missing => return Some(json!({})),
        Settings::Unreadable => return None,
        Settings::Read(text) => text,
    };
    let Value::Object(settings) = serde_json::from_str::<Value>(&text).ok()? else {
        return None;
    };
    let mut inherited = serde_json::Map::new();
    for key in CLAUDE_INHERITED_KEYS {
        if let Some(value) = settings.get(key).filter(|value| !value.is_null()) {
            inherited.insert(key.into(), value.clone());
        }
    }
    if let Some(Value::Object(env)) = settings.get("env") {
        let env: serde_json::Map<String, Value> = env
            .iter()
            .filter(|(key, value)| claude_inherited_env(key) && value.is_string())
            .map(|(key, value)| (key.clone(), value.clone()))
            .collect();
        if !env.is_empty() {
            inherited.insert("env".into(), Value::Object(env));
        }
    }
    Some(Value::Object(inherited))
}

/// Top-level keys of a Codex config that choose the model it runs.
const CODEX_MODEL_KEYS: [&str; 3] = ["model", "model_provider", "profile"];

fn codex_user_choice(vars: &[(String, String)], cwd: Option<&Path>) -> bool {
    if lookup(vars, "MODEL_PROVIDER").is_some() {
        return true;
    }
    if let Some(raw) = lookup(vars, "CODEX_CONFIG") {
        match serde_json::from_str::<Value>(raw) {
            Ok(Value::Object(config)) => {
                if CODEX_MODEL_KEYS.iter().any(|key| config.contains_key(*key)) {
                    return true;
                }
            }
            _ => return true,
        }
    }
    let Some(codex_home) = lookup(vars, "CODEX_HOME")
        .map(PathBuf::from)
        .or_else(|| home(vars).map(|home| home.join(".codex")))
    else {
        return true;
    };
    let mut files = vec![codex_home.join("config.toml")];
    if let Some(cwd) = cwd {
        files.push(cwd.join(".codex").join("config.toml"));
    }
    files.iter().any(|file| match read_settings(file) {
        Settings::Missing => false,
        Settings::Unreadable => true,
        Settings::Read(text) => codex_toml_selects(&text),
    })
}

/// Tolerant scan of the root table of a config.toml: a top-level `model`,
/// `model_provider` or `profile` key. Tables (`[profiles.x]`...) are only
/// active when selected by such a key.
fn codex_toml_selects(text: &str) -> bool {
    for line in text.lines() {
        let line = line.trim();
        if line.starts_with('[') {
            let header = line.split('#').next().unwrap_or_default().trim_end();
            if header.ends_with(']') {
                return false;
            }
            continue;
        }
        let Some((key, _)) = line.split_once('=') else {
            continue;
        };
        let key = key.trim().trim_matches(|c| c == '"' || c == '\'');
        if CODEX_MODEL_KEYS.contains(&key) {
            return true;
        }
    }
    false
}

/// `default_model` is added to CODEX_CONFIG only when it sets no model; the
/// learner's in-app choice is applied per session and still takes priority.
pub fn restrict_tutor_agent(
    target: &AgentTarget,
    spec: &mut LaunchSpec,
    default_model: Option<&str>,
) -> Result<(), String> {
    let is_codex = target.id == "bundled:codex"
        || target
            .adapter_path
            .as_deref()
            .is_some_and(|path| path.to_ascii_lowercase().contains("codex-acp"));
    if !is_codex {
        return Ok(());
    }
    let previous = std::env::var("CODEX_CONFIG").ok();
    let configuration = tutor_codex_config(previous.as_deref(), default_model)?;
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

fn tutor_codex_config(
    previous: Option<&str>,
    default_model: Option<&str>,
) -> Result<String, String> {
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
    if let Some(model) = default_model {
        object.entry("model").or_insert_with(|| json!(model));
    }
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
            serde_json::from_str(&tutor_codex_config(Some(&encoded), Some("small")).unwrap())
                .unwrap();
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
    fn an_economy_model_is_added_only_without_an_inherited_one() {
        let applied: Value =
            serde_json::from_str(&tutor_codex_config(None, Some("small")).unwrap()).unwrap();
        assert_eq!(applied["model"], "small");
        let applied: Value =
            serde_json::from_str(&tutor_codex_config(None, None).unwrap()).unwrap();
        assert!(applied.get("model").is_none());
    }

    fn vars(pairs: &[(&str, &str)]) -> Vec<(String, String)> {
        pairs
            .iter()
            .map(|(key, value)| (key.to_string(), value.to_string()))
            .collect()
    }

    #[test]
    fn claude_user_model_or_provider_is_detected_in_environment_and_settings() {
        let home = tempfile::tempdir().unwrap();
        let project = tempfile::tempdir().unwrap();
        let base = vars(&[("HOME", home.path().to_str().unwrap())]);
        let managed = home.path().join("managed-settings.json");
        let claude_user_choice =
            |vars: &[(String, String)], cwd: Option<&Path>| claude_user_choice(vars, cwd, &managed);
        let claude = home.path().join(".claude");
        std::fs::create_dir_all(&claude).unwrap();
        // Nothing chosen: the default may be injected.
        assert!(!claude_user_choice(&base, Some(project.path())));
        std::fs::write(claude.join("settings.json"), r#"{"apiKeyHelper":"x"}"#).unwrap();
        assert!(!claude_user_choice(&base, Some(project.path())));
        for (key, value) in [
            ("ANTHROPIC_MODEL", "opus"),
            ("CLAUDE_CODE_USE_BEDROCK", "1"),
            ("CLAUDE_CODE_USE_VERTEX", "true"),
            ("ANTHROPIC_BASE_URL", "https://gateway.example"),
        ] {
            let mut env = base.clone();
            env.push((key.into(), value.into()));
            assert!(claude_user_choice(&env, None), "{key}");
        }
        let mut disabled = base.clone();
        disabled.push(("CLAUDE_CODE_USE_BEDROCK".into(), "0".into()));
        assert!(!claude_user_choice(&disabled, None));
        // A later empty launch value does not hide nor create a choice.
        let mut cleared = base.clone();
        cleared.push(("ANTHROPIC_MODEL".into(), "opus".into()));
        cleared.push(("ANTHROPIC_MODEL".into(), String::new()));
        assert!(!claude_user_choice(&cleared, None));
        for settings in [
            r#"{"model":"opus"}"#,
            r#"{"env":{"CLAUDE_CODE_USE_BEDROCK":"1"}}"#,
            r#"{"env":{"ANTHROPIC_BASE_URL":"https://gateway.example"}}"#,
            "not json",
        ] {
            std::fs::write(claude.join("settings.json"), settings).unwrap();
            assert!(claude_user_choice(&base, None), "{settings}");
        }
        std::fs::remove_file(claude.join("settings.json")).unwrap();
        // CLAUDE_CONFIG_DIR replaces ~/.claude, as in the adapter.
        let other = tempfile::tempdir().unwrap();
        std::fs::write(other.path().join("settings.json"), r#"{"model":"opus"}"#).unwrap();
        let mut configured = base.clone();
        configured.push((
            "CLAUDE_CONFIG_DIR".into(),
            other.path().to_str().unwrap().into(),
        ));
        assert!(claude_user_choice(&configured, None));
        // Project settings also select the adapter's initial model.
        let local = project.path().join(".claude");
        std::fs::create_dir_all(&local).unwrap();
        std::fs::write(local.join("settings.local.json"), r#"{"model":"sonnet"}"#).unwrap();
        assert!(claude_user_choice(&base, Some(project.path())));
        assert!(!claude_user_choice(&base, None));
        // Managed settings choose a model for every user.
        std::fs::write(&managed, r#"{"model":"haiku"}"#).unwrap();
        assert!(claude_user_choice(&base, None));
        // Without a home directory nothing can be verified.
        assert!(claude_user_choice(&[], None));
    }

    #[test]
    fn no_proxy_lists_always_end_with_every_loopback_name() {
        assert_eq!(no_proxy_with_loopback([""]), "127.0.0.1,localhost,::1");
        assert_eq!(
            no_proxy_with_loopback([" corp.example, localhost ,", "corp.example,.intra"]),
            "corp.example,localhost,.intra,127.0.0.1,::1"
        );
    }

    #[test]
    fn claude_sessions_inherit_only_authentication_and_provider_settings() {
        let home = tempfile::tempdir().unwrap();
        let file = home.path().join("settings.json");
        assert_eq!(claude_inheritance(&file), Some(json!({})));
        std::fs::write(
            &file,
            json!({
                "apiKeyHelper":"/usr/local/bin/key-helper",
                "awsAuthRefresh":"aws sso login",
                "model":"opus",
                "forceLoginMethod":"console",
                "enabledPlugins":{"evil@market":true},
                "permissions":{"allow":["mcp__tutor__*"],"defaultMode":"acceptEdits"},
                "hooks":{"PreToolUse":[]},
                "statusLine":{"type":"command","command":"x"},
                "env":{"ANTHROPIC_BASE_URL":"https://gateway.example","CLAUDE_CODE_USE_BEDROCK":"1",
                    "AWS_PROFILE":"work","HTTPS_PROXY":"http://proxy:3128","NODE_OPTIONS":"--require=/tmp/x.js",
                    "ENABLE_TOOL_SEARCH":"1","ANTHROPIC_NUMBER":1}
            })
            .to_string(),
        )
        .unwrap();
        assert_eq!(
            claude_inheritance(&file),
            Some(json!({
                "apiKeyHelper":"/usr/local/bin/key-helper",
                "awsAuthRefresh":"aws sso login",
                "model":"opus",
                "forceLoginMethod":"console",
                "env":{"ANTHROPIC_BASE_URL":"https://gateway.example","CLAUDE_CODE_USE_BEDROCK":"1",
                    "AWS_PROFILE":"work","HTTPS_PROXY":"http://proxy:3128"}
            }))
        );
        // Unknown content keeps the legacy user source rather than dropping a login.
        std::fs::write(&file, "not json").unwrap();
        assert_eq!(claude_inheritance(&file), None);
        std::fs::write(&file, "[]").unwrap();
        assert_eq!(claude_inheritance(&file), None);
        std::fs::remove_file(&file).unwrap();
        std::fs::create_dir(&file).unwrap();
        assert_eq!(claude_inheritance(&file), None);
    }

    #[test]
    fn codex_user_model_provider_or_profile_is_detected() {
        let home = tempfile::tempdir().unwrap();
        let base = vars(&[("HOME", home.path().to_str().unwrap())]);
        let codex = home.path().join(".codex");
        std::fs::create_dir_all(&codex).unwrap();
        assert!(!codex_user_choice(&base, None));
        std::fs::write(
            codex.join("config.toml"),
            "approval_policy = \"never\"\n[profiles.local]\nmodel = \"llama\"\n",
        )
        .unwrap();
        assert!(!codex_user_choice(&base, None));
        for config in [
            "model = \"gpt-5\"\n",
            "# comment\nmodel_provider = \"ollama\"\n[model_providers.ollama]\n",
            "profile = \"local\"\n[profiles.local]\nmodel = \"llama\"\n",
            "\"model\" = \"azure\"\n",
        ] {
            std::fs::write(codex.join("config.toml"), config).unwrap();
            assert!(codex_user_choice(&base, None), "{config}");
        }
        std::fs::remove_file(codex.join("config.toml")).unwrap();
        // CODEX_HOME replaces ~/.codex.
        let other = tempfile::tempdir().unwrap();
        std::fs::write(other.path().join("config.toml"), "model = \"o3\"\n").unwrap();
        let mut configured = base.clone();
        configured.push(("CODEX_HOME".into(), other.path().to_str().unwrap().into()));
        assert!(codex_user_choice(&configured, None));
        let mut provider = base.clone();
        provider.push(("MODEL_PROVIDER".into(), "azure".into()));
        assert!(codex_user_choice(&provider, None));
        let mut inherited = base.clone();
        inherited.push(("CODEX_CONFIG".into(), r#"{"model_provider":"oss"}"#.into()));
        assert!(codex_user_choice(&inherited, None));
        inherited.push(("CODEX_CONFIG".into(), r#"{"features":{}}"#.into()));
        assert!(!codex_user_choice(&inherited, None));
        // An unreadable configuration means no default is injected.
        std::fs::create_dir_all(codex.join("config.toml")).unwrap();
        assert!(codex_user_choice(&base, None));
        assert!(codex_user_choice(&[], None));
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
            assert!(tutor_codex_config(Some(raw), None).is_err());
        }
    }
}
