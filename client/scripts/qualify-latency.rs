// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! Explicit live-model qualification. Owns a fresh Tutor database and client
//! profile; provider login remains with the selected harness. Reports timings
//! and tool names, never model text, credentials or raw provider diagnostics.

use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use std::{
    collections::{BTreeMap, HashMap},
    path::PathBuf,
    sync::Arc,
    time::{Duration, Instant},
};
use tutor_client::{
    backend::Backend,
    discovery::AgentTarget,
    permissions::ToolCallRegistry,
    workspace::{Settings, TutorSettings},
};

#[derive(Default)]
struct Options {
    adapter: String,
    tutor: String,
    directory: PathBuf,
    environment: String,
    distribution: Option<String>,
    wsl_user: Option<String>,
    agent_directory: Option<String>,
    live: bool,
    turns: usize,
}
impl Options {
    fn parse(args: impl Iterator<Item = String>) -> Result<Self, String> {
        let mut args = args;
        let mut out = Self {
            environment: "native".into(),
            turns: 3,
            ..Default::default()
        };
        while let Some(key) = args.next() {
            if key == "--live-model" {
                out.live = true;
                continue;
            }
            let value = args.next().ok_or("Missing qualification argument")?;
            match key.as_str() {
                "--adapter" => out.adapter = value,
                "--tutor" => out.tutor = value,
                "--directory" => out.directory = value.into(),
                "--environment" => out.environment = value,
                "--distribution" => out.distribution = Some(value),
                "--wsl-user" => out.wsl_user = Some(value),
                "--agent-directory" => out.agent_directory = Some(value),
                "--turns" => out.turns = value.parse().map_err(|_| "Invalid turn count")?,
                _ => return Err("Unknown qualification argument".into()),
            }
        }
        if !out.live
            || out.adapter.is_empty()
            || out.tutor.is_empty()
            || out.directory.as_os_str().is_empty()
            || !(2..=30).contains(&out.turns)
        {
            return Err("Usage: --live-model --adapter PATH --tutor PATH --directory NEW_DIRECTORY [--turns 3] [--environment wsl --distribution NAME --agent-directory NEW_LINUX_DIRECTORY]".into());
        }
        if !matches!(out.environment.as_str(), "native" | "wsl")
            || (out.environment == "wsl"
                && (out.distribution.is_none() || out.agent_directory.is_none()))
            || (out.environment == "native"
                && (out.distribution.is_some()
                    || out.wsl_user.is_some()
                    || out.agent_directory.is_some()))
        {
            return Err("Select native, or wsl with explicit distribution and fresh absolute Linux directory".into());
        }
        Ok(out)
    }
}

#[tokio::main]
async fn main() {
    let result = match Options::parse(std::env::args().skip(1)) {
        Ok(options) => qualify(options).await,
        Err(error) => Err(error),
    };
    if let Err(error) = result {
        eprintln!("{error}");
        std::process::exit(1);
    }
}

const MAX_EVENTS: usize = 10000;
const EXPECTED_TOOL: &str = "get_learner_context";

// These bytes are embedded by the compiler, so later edits to the checkout do
// not change the provenance of an already-built native or Windows executable.
const BUILD_SOURCES: &[(&str, &[u8])] = &[
    ("backend.rs", include_bytes!("../src-tauri/src/backend.rs")),
    (
        "backend_preparation.rs",
        include_bytes!("../src-tauri/src/backend_preparation.rs"),
    ),
    ("acp.rs", include_bytes!("../src-tauri/src/acp.rs")),
    ("timing.rs", include_bytes!("../src-tauri/src/timing.rs")),
    (
        "workspace.rs",
        include_bytes!("../src-tauri/src/workspace.rs"),
    ),
    ("Cargo.lock", include_bytes!("../src-tauri/Cargo.lock")),
    ("qualify-latency.rs", include_bytes!("qualify-latency.rs")),
];

fn source_provenance(sources: &[(&str, &[u8])]) -> Value {
    let mut aggregate = Sha256::new();
    aggregate.update(b"Tutor-MCP latency sources v1\0");
    let mut files = Vec::with_capacity(sources.len());
    for (name, bytes) in sources {
        let digest = Sha256::digest(bytes);
        aggregate.update((name.len() as u64).to_be_bytes());
        aggregate.update(name.as_bytes());
        aggregate.update(digest);
        files.push(json!({"name":name,"sha256":format!("{digest:x}")}));
    }
    json!({"sha256":format!("{:x}",aggregate.finalize()),"files":files})
}

fn build_provenance() -> Value {
    json!({
        "clientVersion":env!("CARGO_PKG_VERSION"),
        "os":std::env::consts::OS,"architecture":std::env::consts::ARCH,
        "sources":source_provenance(BUILD_SOURCES),
    })
}

enum Observation {
    Event(Value),
    Flush(tokio::sync::oneshot::Sender<()>),
}

struct ObservedTool {
    name: &'static str,
    started: Instant,
}

struct Capture {
    started: Instant,
    events: Vec<Value>,
    truncated: bool,
    unexpected_tool: bool,
    active: HashMap<(String, String), ObservedTool>,
}
impl Default for Capture {
    fn default() -> Self {
        Self {
            started: Instant::now(),
            events: Vec::new(),
            truncated: false,
            unexpected_tool: false,
            active: HashMap::new(),
        }
    }
}
impl Capture {
    fn push(&mut self, event: Value) {
        if self.events.len() < MAX_EVENTS {
            self.events.push(event);
        } else {
            self.truncated = true;
        }
    }
    fn clear(&mut self) {
        self.started = Instant::now();
        self.events.clear();
        self.truncated = false;
        self.unexpected_tool = false;
        self.active.clear();
    }
    fn observe(&mut self, event: &Value) {
        self.observe_at(event, Instant::now());
    }
    fn observe_at(&mut self, event: &Value, now: Instant) {
        if let Some(record) = metric_record(event) {
            self.push(record);
            return;
        }
        if event["method"] != "session/update" {
            return;
        }
        let update = &event["params"]["update"];
        if !matches!(
            update["sessionUpdate"].as_str(),
            Some("tool_call" | "tool_call_update")
        ) {
            return;
        }
        let (Some(session), Some(id)) = (
            bounded_text(&event["params"]["sessionId"], 256),
            bounded_text(&update["toolCallId"], 256),
        ) else {
            self.truncated = true;
            return;
        };
        // Codex exposes startup as a synthetic tool row, not an MCP invocation.
        if id.starts_with("mcp_startup.") {
            return;
        }
        let key = (session.to_string(), id.to_string());
        let canonical = tutor_client::permissions::canonical_tool(update).map(|tool| {
            if tool == EXPECTED_TOOL {
                EXPECTED_TOOL
            } else {
                "unexpected_tool"
            }
        });
        let terminal = matches!(
            update["status"].as_str(),
            Some("completed" | "failed" | "cancelled")
        );
        let (name, duration) = if terminal {
            let observed = self.active.remove(&key);
            (
                canonical
                    .or_else(|| observed.as_ref().map(|tool| tool.name))
                    .unwrap_or("unexpected_tool"),
                observed.map(|tool| observed_ms(tool.started, now)),
            )
        } else {
            if !self.active.contains_key(&key) && self.active.len() >= 4096 {
                self.truncated = true;
                return;
            }
            let observed = self.active.entry(key).or_insert(ObservedTool {
                name: canonical.unwrap_or("unexpected_tool"),
                started: now,
            });
            if let Some(name) = canonical {
                observed.name = name;
            }
            (observed.name, None)
        };
        if update["status"] == "completed" && name != EXPECTED_TOOL {
            self.unexpected_tool = true;
        }
        // This spans first and terminal client observations, including transport
        // and event delivery. It must not be interpreted as pure server time.
        let mut record = json!({
            "type":"observed_tool","name":name,"completed":update["status"]=="completed",
            "elapsedMs":observed_ms(self.started,now),
            "timingScope":"client_observed_including_transport",
        });
        if let Some(duration) = duration {
            record["observedDurationMs"] = json!(duration);
        }
        self.push(record);
    }
}

fn observed_ms(started: Instant, now: Instant) -> u64 {
    now.saturating_duration_since(started)
        .as_millis()
        .min(u64::MAX as u128) as u64
}

fn bounded_text(value: &Value, max: usize) -> Option<&str> {
    value
        .as_str()
        .filter(|text| !text.is_empty() && text.len() <= max && !text.chars().any(char::is_control))
}
fn metric_record(event: &Value) -> Option<Value> {
    let kind = event["type"].as_str()?;
    let mut out = json!({"type":kind});
    let keys: &[&str] = match kind {
        "rpc_timing" => {
            let method = event["method"].as_str()?;
            if !matches!(
                method,
                "initialize"
                    | "authenticate"
                    | "auth/login"
                    | "session/new"
                    | "session/load"
                    | "session/resume"
                    | "session/prompt"
                    | "session/set_config_option"
                    | "session/set_mode"
                    | "session/set_model"
                    | "tools/list"
                    | "tools/call"
                    | "_tutor/rebind"
                    | "other"
            ) {
                return None;
            }
            out["method"] = json!(method);
            out["requestId"] = json!(event["requestId"].as_u64()?);
            out["connectionId"] = json!(bounded_text(&event["connectionId"], 128)?);
            out["offsetMs"] = json!(event["offsetMs"].as_u64()?);
            &["operationId"]
        }
        "turn_phase" => {
            let phase = event["phase"].as_str()?;
            if !matches!(
                phase,
                "preparing"
                    | "waiting_lock"
                    | "context"
                    | "loading_session"
                    | "waiting_model"
                    | "finalizing"
            ) {
                return None;
            }
            out["phase"] = json!(phase);
            out["phaseElapsedMs"] = json!(event["phaseElapsedMs"].as_u64()?);
            &["operationId", "conversationId"]
        }
        "turn_timing" => {
            let marker = event["marker"].as_str()?;
            if !matches!(marker, "first_event" | "first_token") {
                return None;
            }
            out["marker"] = json!(marker);
            &["operationId", "conversationId"]
        }
        _ => return None,
    };
    out["elapsedMs"] = json!(event["elapsedMs"].as_u64()?);
    for key in keys {
        if let Some(value) = bounded_text(&event[*key], 128) {
            out[*key] = json!(value);
        }
    }
    if kind != "turn_timing" {
        let state = event["state"].as_str()?;
        if !matches!(state, "started" | "completed") {
            return None;
        }
        out["state"] = json!(state);
    }
    if let Some(outcome) = event["outcome"].as_str() {
        if !matches!(outcome, "succeeded" | "failed" | "cancelled" | "timed_out") {
            return None;
        }
        out["outcome"] = json!(outcome);
    }
    Some(out)
}

fn journal_delta(before: &[Value], after: &[Value]) -> (Vec<Value>, bool) {
    let Some(connection) = before.last().and_then(|r| r["connectionId"].as_str()) else {
        return (vec![], false);
    };
    let baseline = before
        .iter()
        .filter_map(|r| r["requestId"].as_u64())
        .max()
        .unwrap_or(0);
    let mut complete = true;
    let mut pairs = BTreeMap::<u64, Vec<&Value>>::new();
    let mut records = Vec::new();
    for raw in after {
        if raw["connectionId"] != connection {
            complete = false;
        }
        let Some(id) = raw["requestId"].as_u64() else {
            complete = false;
            continue;
        };
        if id <= baseline {
            continue;
        }
        pairs.entry(id).or_default().push(raw);
        if let Some(record) = metric_record(raw) {
            records.push(record);
        } else {
            complete = false;
        }
    }
    if pairs.is_empty() {
        return (records, false);
    }
    for (offset, (id, pair)) in pairs.iter().enumerate() {
        if *id != baseline + offset as u64 + 1
            || pair.len() != 2
            || pair[0]["state"] != "started"
            || pair[1]["state"] != "completed"
            || pair[0]["method"] != pair[1]["method"]
            || pair[1]["outcome"].as_str().is_none()
        {
            complete = false;
        }
    }
    (records, complete)
}

async fn flush_observations(
    tx: &tokio::sync::mpsc::UnboundedSender<Observation>,
) -> Result<(), String> {
    let (done, waiter) = tokio::sync::oneshot::channel();
    tx.send(Observation::Flush(done))
        .map_err(|_| "Diagnostic collector stopped")?;
    tokio::time::timeout(std::time::Duration::from_secs(5), waiter)
        .await
        .map_err(|_| "Diagnostic collector timeout")?
        .map_err(|_| "Diagnostic collector stopped".into())
}

fn turn_report(
    index: usize,
    elapsed_ms: u128,
    response: &Value,
    capture: &Capture,
    before: &[Value],
    after: &[Value],
) -> Value {
    let (rpc, journal_complete) = journal_delta(before, after);
    let count = |method: &str| {
        rpc.iter()
            .filter(|r| r["state"] == "started" && r["method"] == method)
            .count()
    };
    let loads = count("session/load");
    let creations = count("session/new");
    let prompts = count("session/prompt");
    let completed = capture.events.iter().any(|r| {
        r["type"] == "observed_tool" && r["name"] == EXPECTED_TOOL && r["completed"] == true
    });
    json!({
        "index":index,"cold":index==0,"elapsedMs":elapsed_ms,"stopReason":stop_reason(response),
        "contextToolCompleted":completed,"sessionLoads":loads,"sessionCreations":creations,
        "sessionReused":prompts==1 && loads==0 && creations==usize::from(index==0),
        "diagnosticsComplete":journal_complete && !capture.truncated,
        "unexpectedToolCompleted":capture.unexpected_tool,"rpc":rpc,"events":capture.events,
    })
}

fn stop_reason(response: &Value) -> &'static str {
    match response["stopReason"].as_str() {
        Some("end_turn") => "end_turn",
        Some("cancelled") => "cancelled",
        Some("refusal") => "refusal",
        Some("max_tokens") => "max_tokens",
        Some("max_turn_requests") => "max_turn_requests",
        _ => "other",
    }
}

fn passed_turn(turn: &Value) -> bool {
    turn["stopReason"] == "end_turn"
        && turn["contextToolCompleted"] == true
        && turn["sessionReused"] == true
        && turn["diagnosticsComplete"] == true
        && turn["unexpectedToolCompleted"] == false
}

async fn qualify(options: Options) -> Result<(), String> {
    // create_dir is exclusive: never overwrite a real profile or earlier run.
    std::fs::create_dir(&options.directory)
        .map_err(|_| "Qualification directory must be new, with an existing parent")?;
    let directory = options
        .directory
        .canonicalize()
        .map_err(|_| "Invalid directory")?;
    let build = build_provenance();
    // Persist before any child process or harness call; setup failures retain a receipt.
    tutor_client::workspace::private_write(
        &directory.join("latency.json"),
        &serde_json::to_vec_pretty(&json!({
            "contractVersion":1,"host":std::env::consts::OS,"build":build,
            "environment":options.environment,"liveModel":true,"status":"incomplete","stage":"setup"
        }))
        .unwrap(),
    )?;
    let cwd = if options.environment == "wsl" {
        if !cfg!(windows) {
            return Err("Run WSL qualification from Windows".into());
        }
        let cwd = options.agent_directory.as_deref().unwrap();
        if !cwd.starts_with('/') || cwd.contains('\0') {
            return Err("Absolute Linux directory required".into());
        }
        let mut command = tokio::process::Command::new("wsl.exe");
        command.args(["--distribution", options.distribution.as_deref().unwrap()]);
        if let Some(user) = &options.wsl_user {
            command.args(["--user", user]);
        }
        command.kill_on_drop(true);
        let status = tokio::time::timeout(
            Duration::from_secs(30),
            command
                .args(["--exec", "mkdir", "-m", "700", "--", cwd])
                .status(),
        )
        .await
        .map_err(|_| "Fresh Linux directory creation timed out")?
        .map_err(|_| "Cannot create fresh Linux directory")?;
        if !status.success() {
            return Err("Linux directory must be new with an existing parent".into());
        }
        cwd.to_string()
    } else {
        directory.to_string_lossy().into_owned()
    };
    let target = AgentTarget {
        id: format!("latency-{}", options.environment),
        label: "Latency qualification".into(),
        environment: options.environment.clone(),
        distribution: options.distribution.clone(),
        wsl_user: options.wsl_user.clone(),
        adapter_path: Some(options.adapter),
        codex_path: None,
        adapter_version: None,
        codex_version: None,
        available: true,
        detail: String::new(),
    };
    let settings = Settings {
        target_id: target.id.clone(),
        manual_target: Some(target.clone()),
        cwd: cwd.clone(),
        tutor: TutorSettings {
            mode: "local".into(),
            activity: "auto".into(),
            url: String::new(),
            command: options.tutor,
            args: vec![
                "--local".into(),
                "--data-dir".into(),
                format!("{cwd}/tutor"),
            ],
        },
        ..Default::default()
    };
    let (tx, mut rx) = tokio::sync::mpsc::unbounded_channel();
    let sink = tx.clone();
    let backend = Backend::new(
        &directory.join("client"),
        Arc::new(move |name, event| {
            if name == "acp:event" {
                let _ = sink.send(Observation::Event(event));
            }
        }),
    )?;
    let records = Arc::new(std::sync::Mutex::new(Capture::default()));
    let observed = records.clone();
    let owner = backend.clone();
    let event_task = tokio::spawn(async move {
        let mut registry = ToolCallRegistry::default();
        while let Some(observation) = rx.recv().await {
            let event = match observation {
                Observation::Event(event) => event,
                Observation::Flush(done) => {
                    let _ = done.send(());
                    continue;
                }
            };
            registry.observe(&event);
            if event["method"] == "session/request_permission" {
                // This benchmark authorizes one read-only tool on its disposable
                // server. Every unrecognized operation is explicitly denied.
                let allowed = registry.request_tool(&event) == Some("get_learner_context");
                let choice = allowed
                    .then(|| {
                        event["params"]["options"]
                            .as_array()?
                            .iter()
                            .find(|o| o["kind"] == "allow_once")?["optionId"]
                            .as_str()
                            .map(str::to_string)
                    })
                    .flatten();
                let _ = owner.answer_permission(event["id"].clone(), choice).await;
            }
            observed.lock().unwrap().observe(&event);
        }
    });
    let started = Instant::now();
    let mut turns = Vec::new();
    let result = async {
        let agent = tokio::time::timeout(Duration::from_secs(180), async {
            backend.save_settings(settings).await?;
            let agent = backend.connect_agent(target, cwd).await?;
            backend.tutor_overview().await?;
            Ok::<_, String>(agent)
        }).await.map_err(|_| "Harness setup exceeded the qualification budget")??;
        let connect_ms = started.elapsed().as_millis();
        let mut conversation = None;
        for index in 0..options.turns {
            flush_observations(&tx).await?;
            let start = {
                let mut capture = records.lock().unwrap();
                capture.clear();
                capture.started
            };
            let previous = backend.agent_timings().await;
            let response = tokio::time::timeout(Duration::from_secs(180), async {
                let text = "Consulte uniquement mon profil avec get_learner_context, puis confirme en une phrase que tu l’as consulté. Ne crée aucune formation et ne modifie rien.".to_string();
                backend.queue_prompt(Some(text.clone()), conversation.clone()).await?;
                if conversation.is_none() {
                    conversation = Some(backend.start_conversation(None, None).await?.id);
                }
                backend.send_prompt(conversation.clone().unwrap(), text).await
            }).await.map_err(|_| "Model turn exceeded the qualification budget")??;
            // The backend drains its own event consumer; this second barrier also
            // drains our diagnostic collector before a snapshot or the next turn.
            flush_observations(&tx).await?;
            let after = backend.agent_timings().await;
            turns.push(turn_report(index, start.elapsed().as_millis(), &response, &records.lock().unwrap(), &previous, &after));
        }
        Ok::<Value, String>(json!({
            "contractVersion":1,"host":std::env::consts::OS,"environment":options.environment,
            "distribution":options.distribution,"liveModel":true,
            "agent":{"name":bounded_text(&agent["agentInfo"]["name"],128),"version":bounded_text(&agent["agentInfo"]["version"],64)},
            "connectMs":connect_ms,"turns":turns,
            "scope":"Isolated real MCP read and actual harness prompts; provider time included; credentials unchanged"
        }))
    }.await;
    // A broken harness must still leave a report when process cleanup stalls.
    let shutdown = tokio::time::timeout(Duration::from_secs(15), backend.shutdown())
        .await
        .is_ok();
    let collected = flush_observations(&tx).await.is_ok();
    event_task.abort();
    let mut report = match result {
        Ok(report) => report,
        Err(_) => {
            json!({"contractVersion":1,"host":std::env::consts::OS,"environment":options.environment,"liveModel":true,"status":"incomplete","turns":turns,"events":records.lock().unwrap().events.clone()})
        }
    };
    let passed = shutdown
        && collected
        && report["status"] != "incomplete"
        && report["turns"]
            .as_array()
            .is_some_and(|turns| turns.len() == options.turns && turns.iter().all(passed_turn));
    report["build"] = build;
    report["status"] = json!(if passed { "passed" } else { "incomplete" });
    tutor_client::workspace::private_write(
        &directory.join("latency.json"),
        &serde_json::to_vec_pretty(&report).unwrap(),
    )?;
    if !passed {
        return Err("Qualification incomplete; see sanitized latency.json".into());
    }
    println!(
        "Qualification complete: {}",
        directory.join("latency.json").display()
    );
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn rpc(id: u64, method: &str, state: &str) -> Value {
        let mut event = json!({"type":"rpc_timing","connectionId":"local-connection","requestId":id,
            "method":method,"state":state,"elapsedMs":2,"offsetMs":10,"operationId":"local-operation"});
        if state == "completed" {
            event["outcome"] = json!("succeeded");
        }
        event
    }

    fn pair(id: u64, method: &str) -> Vec<Value> {
        vec![rpc(id, method, "started"), rpc(id, method, "completed")]
    }

    fn tool(id: &str, status: &str, name: Option<&str>) -> Value {
        let mut event = json!({"method":"session/update","params":{"sessionId":"remote-session-secret",
            "update":{"sessionUpdate":"tool_call_update","toolCallId":id,"status":status}}});
        if let Some(name) = name {
            event["params"]["update"]["rawInput"] =
                json!({"server":"tutor","tool":name,"arguments":{"token":"secret"}});
        }
        event
    }

    #[test]
    fn terminal_tool_updates_keep_identity_without_persisting_remote_values() {
        let mut capture = Capture::default();
        capture.observe(&tool("call-secret", "in_progress", Some(EXPECTED_TOOL)));
        capture.observe(&tool("call-secret", "completed", None));
        assert_eq!(capture.events.len(), 2);
        assert_eq!(capture.events[1]["name"], EXPECTED_TOOL);
        assert_eq!(capture.events[1]["completed"], true);
        assert!(capture.active.is_empty());
        assert!(!capture.unexpected_tool);
        let saved = serde_json::to_string(&capture.events).unwrap();
        assert!(!saved.contains("secret"));
        capture.observe(&tool("other", "completed", Some("private_tool_name")));
        assert!(capture.unexpected_tool);
        assert!(!serde_json::to_string(&capture.events)
            .unwrap()
            .contains("private_tool_name"));
        let count = capture.events.len();
        capture.observe(&tool("mcp_startup.tutor", "completed", None));
        assert_eq!(capture.events.len(), count);
    }

    #[test]
    fn observed_tool_duration_keeps_first_update_and_resets_between_turns() {
        let mut capture = Capture::default();
        let start = capture.started;
        capture.observe_at(
            &tool("call", "pending", Some(EXPECTED_TOOL)),
            start + Duration::from_millis(100),
        );
        capture.observe_at(
            &tool("call", "in_progress", Some(EXPECTED_TOOL)),
            start + Duration::from_millis(200),
        );
        capture.observe_at(
            &tool("call", "completed", None),
            start + Duration::from_millis(400),
        );
        assert_eq!(capture.events[0]["elapsedMs"], 100);
        assert_eq!(capture.events[1]["elapsedMs"], 200);
        assert!(capture.events[1].get("observedDurationMs").is_none());
        assert_eq!(capture.events[2]["elapsedMs"], 400);
        assert_eq!(capture.events[2]["observedDurationMs"], 300);
        assert_eq!(
            capture.events[2]["timingScope"],
            "client_observed_including_transport"
        );

        // A pending call from an earlier tour cannot supply a duration to the
        // next one, even if the harness reuses its call identifier.
        capture.observe_at(
            &tool("same-id", "in_progress", Some(EXPECTED_TOOL)),
            start + Duration::from_millis(500),
        );
        capture.clear();
        assert!(capture.active.is_empty());
        let next = capture.started;
        capture.observe_at(
            &tool("same-id", "completed", Some(EXPECTED_TOOL)),
            next + Duration::from_millis(5),
        );
        assert_eq!(capture.events[0]["elapsedMs"], 5);
        assert!(capture.events[0].get("observedDurationMs").is_none());
        capture.observe_at(
            &tool("same-id", "in_progress", Some(EXPECTED_TOOL)),
            next + Duration::from_millis(10),
        );
        capture.observe_at(
            &tool("same-id", "completed", None),
            next + Duration::from_millis(60),
        );
        assert_eq!(capture.events[2]["elapsedMs"], 60);
        assert_eq!(capture.events[2]["observedDurationMs"], 50);
    }

    #[test]
    fn observed_tool_clocks_are_isolated_by_session_and_call_id() {
        let mut capture = Capture::default();
        let start = capture.started;
        capture.observe_at(
            &tool("same-id", "in_progress", Some(EXPECTED_TOOL)),
            start + Duration::from_millis(10),
        );
        let mut second = tool("same-id", "in_progress", Some(EXPECTED_TOOL));
        second["params"]["sessionId"] = json!("another-remote-secret");
        capture.observe_at(&second, start + Duration::from_millis(50));
        second["params"]["update"]["status"] = json!("completed");
        capture.observe_at(&second, start + Duration::from_millis(80));
        capture.observe_at(
            &tool("same-id", "completed", None),
            start + Duration::from_millis(100),
        );
        assert_eq!(capture.events[2]["observedDurationMs"], 30);
        assert_eq!(capture.events[3]["observedDurationMs"], 90);
        assert!(!serde_json::to_string(&capture.events)
            .unwrap()
            .contains("secret"));
    }

    #[test]
    fn journal_requires_contiguous_pairs_on_the_same_connection() {
        let before = pair(1, "initialize");
        let after = [
            before.clone(),
            pair(2, "session/new"),
            pair(3, "session/prompt"),
        ]
        .concat();
        let (records, complete) = journal_delta(&before, &after);
        assert!(complete);
        assert_eq!(records.len(), 4);
        assert!(!journal_delta(&before, &pair(3, "session/prompt")).1);
        assert!(!journal_delta(&before, &pair(2, "session/prompt")[1..]).1);
        assert!(!journal_delta(&[], &after).1);
        let mut replaced = pair(2, "session/prompt");
        replaced
            .iter_mut()
            .for_each(|r| r["connectionId"] = json!("new-connection"));
        assert!(!journal_delta(&before, &replaced).1);
        let mut malformed = pair(2, "session/prompt");
        malformed[1]["method"] = json!("session/load");
        assert!(!journal_delta(&before, &malformed).1);
    }

    #[test]
    fn success_requires_exactly_one_cold_creation_and_exhaustive_warm_reuse() {
        let before = pair(1, "initialize");
        let cold = [pair(2, "session/new"), pair(3, "session/prompt")].concat();
        let mut capture = Capture::default();
        capture.observe(&tool("call", "completed", Some(EXPECTED_TOOL)));
        assert!(passed_turn(&turn_report(
            0,
            100,
            &json!({"stopReason":"end_turn"}),
            &capture,
            &before,
            &cold
        )));
        assert!(!passed_turn(&turn_report(
            1,
            100,
            &json!({"stopReason":"end_turn"}),
            &capture,
            &before,
            &cold
        )));
        let second_creation = [cold.clone(), pair(4, "session/new")].concat();
        assert!(!passed_turn(&turn_report(
            0,
            100,
            &json!({"stopReason":"end_turn"}),
            &capture,
            &before,
            &second_creation
        )));
        let warm = pair(4, "session/prompt");
        assert!(passed_turn(&turn_report(
            1,
            100,
            &json!({"stopReason":"end_turn"}),
            &capture,
            &cold,
            &warm
        )));
        let loaded = [pair(4, "session/load"), pair(5, "session/prompt")].concat();
        assert!(!passed_turn(&turn_report(
            1,
            100,
            &json!({"stopReason":"end_turn"}),
            &capture,
            &cold,
            &loaded
        )));
        capture.truncated = true;
        assert!(!passed_turn(&turn_report(
            1,
            100,
            &json!({"stopReason":"end_turn"}),
            &capture,
            &cold,
            &warm
        )));
    }

    #[test]
    fn completed_tool_does_not_qualify_an_interrupted_response() {
        let before = pair(1, "initialize");
        let after = [pair(2, "session/new"), pair(3, "session/prompt")].concat();
        let mut capture = Capture::default();
        capture.observe(&tool("call", "completed", Some(EXPECTED_TOOL)));
        for reason in [
            "cancelled",
            "refusal",
            "max_tokens",
            "max_turn_requests",
            "provider-secret",
        ] {
            let response = json!({"stopReason":reason});
            let report = turn_report(0, 100, &response, &capture, &before, &after);
            assert_eq!(report["contextToolCompleted"], true);
            assert_eq!(report["diagnosticsComplete"], true);
            assert!(!passed_turn(&report));
            assert_eq!(
                report["stopReason"],
                if reason == "provider-secret" {
                    "other"
                } else {
                    reason
                }
            );
            assert!(!serde_json::to_string(&report)
                .unwrap()
                .contains("provider-secret"));
        }
        assert_eq!(stop_reason(&json!({})), "other");
    }

    #[test]
    fn build_provenance_hashes_embedded_sources_with_fixed_public_names() {
        let provenance = build_provenance();
        assert_eq!(provenance["clientVersion"], env!("CARGO_PKG_VERSION"));
        assert_eq!(provenance["os"], std::env::consts::OS);
        assert_eq!(provenance["architecture"], std::env::consts::ARCH);
        let sources = &provenance["sources"];
        let files = sources["files"].as_array().unwrap();
        let names: Vec<_> = files
            .iter()
            .map(|file| file["name"].as_str().unwrap())
            .collect();
        assert_eq!(
            names,
            [
                "backend.rs",
                "backend_preparation.rs",
                "acp.rs",
                "timing.rs",
                "workspace.rs",
                "Cargo.lock",
                "qualify-latency.rs"
            ]
        );
        for hash in files
            .iter()
            .map(|file| &file["sha256"])
            .chain(std::iter::once(&sources["sha256"]))
        {
            let value = hash.as_str().unwrap();
            assert_eq!(value.len(), 64);
            assert!(value.bytes().all(|byte| byte.is_ascii_hexdigit()));
        }
        let first = source_provenance(&[("one.rs", b"secret-one")]);
        assert_eq!(first, source_provenance(&[("one.rs", b"secret-one")]));
        assert_ne!(
            first["sha256"],
            source_provenance(&[("one.rs", b"secret-two")])["sha256"]
        );
        assert_ne!(
            first["sha256"],
            source_provenance(&[("two.rs", b"secret-one")])["sha256"]
        );
        assert!(!serde_json::to_string(&first).unwrap().contains("secret"));
    }

    #[test]
    fn diagnostics_are_projected_and_tool_capture_is_bounded() {
        let mut source = rpc(1, "session/prompt", "completed");
        source["headers"] = json!({"authorization":"secret"});
        source["response"] = json!({"text":"secret"});
        let record = metric_record(&source).unwrap();
        assert!(!serde_json::to_string(&record).unwrap().contains("secret"));
        source["method"] = json!("extension/secret");
        assert!(metric_record(&source).is_none());
        let mut capture = Capture::default();
        for _ in 0..=MAX_EVENTS {
            capture.observe(&tool("call", "completed", Some(EXPECTED_TOOL)));
        }
        assert_eq!(capture.events.len(), MAX_EVENTS);
        assert!(capture.truncated);
        capture.clear();
        assert!(capture.events.is_empty());
        assert!(!capture.truncated);
    }

    #[tokio::test]
    async fn collector_barrier_prevents_cross_turn_event_spill() {
        let (tx, mut rx) = tokio::sync::mpsc::unbounded_channel();
        let records = Arc::new(std::sync::Mutex::new(Capture::default()));
        let observed = records.clone();
        let collector = tokio::spawn(async move {
            while let Some(item) = rx.recv().await {
                match item {
                    Observation::Event(event) => {
                        tokio::task::yield_now().await;
                        observed.lock().unwrap().observe(&event);
                    }
                    Observation::Flush(done) => {
                        let _ = done.send(());
                    }
                }
            }
        });
        tx.send(Observation::Event(tool(
            "first",
            "in_progress",
            Some(EXPECTED_TOOL),
        )))
        .unwrap();
        tx.send(Observation::Event(tool("first", "completed", None)))
            .unwrap();
        flush_observations(&tx).await.unwrap();
        assert_eq!(records.lock().unwrap().events.len(), 2);
        records.lock().unwrap().clear();
        tx.send(Observation::Event(tool(
            "second",
            "completed",
            Some(EXPECTED_TOOL),
        )))
        .unwrap();
        flush_observations(&tx).await.unwrap();
        assert_eq!(records.lock().unwrap().events.len(), 1);
        drop(tx);
        collector.await.unwrap();
    }

    #[test]
    fn live_calls_and_environment_are_explicit() {
        let args = |extra: &[&str]| {
            [
                "--adapter",
                "adapter",
                "--tutor",
                "tutor",
                "--directory",
                "new",
            ]
            .into_iter()
            .chain(extra.iter().copied())
            .map(str::to_string)
            .collect::<Vec<_>>()
            .into_iter()
        };
        assert!(Options::parse(args(&[])).is_err());
        assert!(Options::parse(args(&["--live-model"])).is_ok());
        assert!(Options::parse(args(&["--live-model", "--environment", "wsl"])).is_err());
        assert!(Options::parse(args(&[
            "--live-model",
            "--environment",
            "wsl",
            "--distribution",
            "Ubuntu",
            "--agent-directory",
            "/tmp/new"
        ]))
        .is_ok());
        assert!(Options::parse(args(&["--live-model", "--turns", "1000"])).is_err());
    }
}
