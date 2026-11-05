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
    acp::AcpClient,
    backend::Backend,
    discovery::AgentTarget,
    permissions::ToolCallRegistry,
    workspace::{AgentSettings, Settings, TutorSettings},
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
    model: String,
    effort: String,
    speed: String,
    harness: String,
    scenario: String,
}
impl Options {
    fn parse(args: impl Iterator<Item = String>) -> Result<Self, String> {
        let mut args = args;
        let mut out = Self {
            environment: "native".into(),
            turns: 3,
            speed: "balanced".into(),
            harness: "codex".into(),
            scenario: "context-read".into(),
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
                "--model" => out.model = value,
                "--effort" => out.effort = value,
                "--speed" => out.speed = value,
                "--harness" => out.harness = value,
                "--scenario" => out.scenario = value,
                _ => return Err("Unknown qualification argument".into()),
            }
        }
        if !out.live
            || out.adapter.is_empty()
            || out.tutor.is_empty()
            || out.directory.as_os_str().is_empty()
            || !(2..=30).contains(&out.turns)
            || out.model.len() > 256
            || out.effort.len() > 64
            || !["balanced", "fast", "deep"].contains(&out.speed.as_str())
            || !["codex", "claude"].contains(&out.harness.as_str())
            || !["context-read", "explanation", "resume", "correction"]
                .contains(&out.scenario.as_str())
        {
            return Err("Usage: --live-model --adapter PATH --tutor PATH --directory NEW_DIRECTORY [--harness codex|claude] [--scenario context-read|explanation|resume|correction] [--model ID --effort LEVEL --speed balanced|fast|deep] [--turns 3] [--environment wsl --distribution NAME --agent-directory NEW_LINUX_DIRECTORY]".into());
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
        "backend_pedagogy.rs",
        include_bytes!("../src-tauri/src/backend_pedagogy.rs"),
    ),
    (
        "backend_maintenance.rs",
        include_bytes!("../src-tauri/src/backend_maintenance.rs"),
    ),
    (
        "learning_ui_prepared.rs",
        include_bytes!("../src-tauri/src/learning_ui_prepared.rs"),
    ),
    (
        "learning_ui.rs",
        include_bytes!("../src-tauri/src/learning_ui.rs"),
    ),
    (
        "learning_resume.rs",
        include_bytes!("../src-tauri/src/learning_resume.rs"),
    ),
    (
        "learning_ui_resume_prepared.rs",
        include_bytes!("../src-tauri/src/learning_ui_resume_prepared.rs"),
    ),
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
                    | "waiting_permission"
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

fn passed_response(turn: &Value) -> bool {
    turn["stopReason"] == "end_turn"
        && turn["elapsedMs"].as_u64().is_some_and(|ms| ms <= 5000)
        && if turn["scenario"].as_str().unwrap_or("context-read") == "context-read" {
            turn["contextToolCompleted"] == true
        } else {
            turn["events"]
                .as_array()
                .is_some_and(|events| !events.iter().any(|event| event["type"] == "observed_tool"))
                && turn["persistedResponse"] == true
        }
        && turn["sessionReused"] == true
        && turn["diagnosticsComplete"] == true
        && turn["unexpectedToolCompleted"] == false
}

fn passed_turn(turn: &Value) -> bool {
    passed_response(turn) && turn["configurationVerified"] == true
}

fn option<'a>(configuration: &'a Value, category: &str) -> Option<&'a Value> {
    let mut matches = configuration["configOptions"]
        .as_array()?
        .iter()
        .filter(|option| option["category"] == category);
    let found = matches.next()?;
    matches.next().is_none().then_some(found)
}

fn configuration_evidence(configuration: &Value, requested: &AgentSettings) -> Value {
    // Never use configuration.current: that is the saved intention, not proof
    // of what the ACP session accepted. Inspect raw currentValue instead.
    let model = option(configuration, "model");
    let effort = option(configuration, "thought_level");
    let fast = configuration["configOptions"]
        .as_array()
        .and_then(|options| {
            let mut matches = options.iter().filter(|option| option["id"] == "fast-mode");
            let found = matches.next()?;
            matches.next().is_none().then_some(found)
        });
    let mut reasons = Vec::new();
    if configuration["connected"] != true {
        reasons.push("disconnected");
    }
    if model
        .and_then(|option| bounded_text(&option["currentValue"], 256))
        .is_none()
    {
        reasons.push("model_unverifiable");
    } else if !requested.model.is_empty()
        && model.map(|option| &option["currentValue"]) != Some(&json!(requested.model))
    {
        reasons.push("model_mismatch");
    }
    let mut expected_effort = requested.reasoning_effort.clone();
    if expected_effort.is_empty() && requested.speed != "balanced" {
        fn offered(option: &Value, value: &str) -> bool {
            option["value"] == value
                || option["options"]
                    .as_array()
                    .is_some_and(|options| options.iter().any(|option| offered(option, value)))
        }
        let preferences = if requested.speed == "deep" {
            ["high", "xhigh", "max", "ultra", "medium"]
        } else {
            ["low", "minimal", "none", "medium", "high"]
        };
        expected_effort = preferences
            .into_iter()
            .find(|value| effort.is_some_and(|option| offered(option, value)))
            .unwrap_or_default()
            .to_string();
    }
    if !expected_effort.is_empty()
        && effort.map(|option| &option["currentValue"]) != Some(&json!(expected_effort))
    {
        reasons.push("effort_mismatch_or_unverifiable");
    }
    if let Some(fast) = fast {
        let expected = if fast["type"] == "boolean" {
            json!(requested.speed == "fast")
        } else {
            json!(if requested.speed == "fast" {
                "on"
            } else {
                "off"
            })
        };
        if fast["currentValue"] != expected {
            reasons.push("speed_mismatch");
        }
    } else if (requested.speed == "fast" || requested.speed == "deep") && expected_effort.is_empty()
    {
        reasons.push("speed_unverifiable");
    }
    let scalar = |value: &Value| {
        if value.is_boolean() {
            value.clone()
        } else {
            json!(bounded_text(value, 256))
        }
    };
    json!({"verified":reasons.is_empty(),"reasons":reasons,
        "model":model.map(|option|scalar(&option["currentValue"])),
        "effort":effort.map(|option|scalar(&option["currentValue"])),
        "fastMode":fast.map(|option|scalar(&option["currentValue"])),
        "speedMechanism":if fast.is_some() {"fast_mode"} else if requested.speed != "balanced" {"effort"} else {"default"}})
}

fn harness_matches(agent: &Value, expected: &str) -> bool {
    let name = agent["agentInfo"]["name"]
        .as_str()
        .unwrap_or_default()
        .to_lowercase();
    let versioned = agent["agentInfo"]["version"]
        .as_str()
        .is_some_and(|s| !s.is_empty());
    versioned
        && match expected {
            "codex" => name.contains("codex") && !name.contains("claude"),
            "claude" => name.contains("claude") && !name.contains("codex"),
            _ => false,
        }
}

const FIXTURE_NAME: &str = "Qualification latence — équations";

fn native_operations_allowed(operations: &Value) -> bool {
    operations.as_array().is_some_and(|operations| {
        !operations.is_empty()
            && operations.iter().all(|operation| {
                matches!(
                    operation.as_str(),
                    Some(
                        "prepare_pedagogical_turn"
                            | "select_conversation_training"
                            | "get_next_activity"
                            | "get_conversation_context"
                            | "prepare_assessment_attempt"
                            | "submit_assessment_attempt"
                            | "record_interaction"
                            | "record_learning_event"
                    )
                )
            })
    })
}

async fn fixture_tool(rpc: &AcpClient, name: &str, arguments: Value) -> Result<Value, String> {
    let result = rpc
        .request("tools/call", json!({"name":name,"arguments":arguments}))
        .await?;
    if result["isError"] == true {
        return Err("Fixture operation rejected by Tutor".into());
    }
    if let Some(value) = result.get("structuredContent") {
        return Ok(value.clone());
    }
    result["content"]
        .as_array()
        .and_then(|blocks| blocks.iter().find_map(|block| block["text"].as_str()))
        .and_then(|text| serde_json::from_str(text).ok())
        .ok_or_else(|| "Fixture operation returned no structured content".into())
}

async fn seed_fixture(
    target: &AgentTarget,
    settings: &Settings,
    cwd: &str,
    count: usize,
) -> Result<Value, String> {
    let spec = tutor_client::discovery::command_spec(
        target,
        &settings.tutor.command,
        &settings.tutor.args,
        cwd,
    )
    .await?;
    let (tx, mut rx) = tokio::sync::mpsc::unbounded_channel();
    let drain = tokio::spawn(async move { while rx.recv().await.is_some() {} });
    let rpc = AcpClient::spawn(spec, tx).await?;
    let result = tokio::time::timeout(Duration::from_secs(30), async {
        let initialized = rpc.request("initialize", json!({"protocolVersion":"2025-06-18","capabilities":{},
            "clientInfo":{"name":"tutor-latency-fixture","version":"1"}})).await?;
        rpc.notify("notifications/initialized", json!({})).await?;
        let mut variants = Vec::new();
        for index in 0..count {
        let name = if count == 1 { FIXTURE_NAME.to_string() } else { format!("{FIXTURE_NAME} {}", index + 1) };
        let input = json!({"name":name,"concepts":["Équivalence"],"prerequisites":{},
            "personal_goal":"Résoudre une équation du premier degré en conservant l’égalité.",
            "details":{"Équivalence":{"label":"Conserver l’égalité","description":"Appliquer la même opération aux deux membres.",
                "criteria":[{"description":"Appliquer la même opération aux deux membres et vérifier le résultat."}]}},
            "idempotency_key":format!("qualification-fixture-v1-{index}")});
        let mut created = fixture_tool(&rpc, "init_domain", input.clone()).await?;
        let replay = fixture_tool(&rpc, "init_domain", input).await?;
        if created["domain_id"].as_str().is_none_or(str::is_empty)
            || created["training_id"].as_str().is_none_or(str::is_empty)
            || replay["domain_id"] != created["domain_id"] {
            return Err("Fixture training creation was not confirmed idempotently".into());
        }
        created["name"] = json!(name);
        variants.push(created);
        }
        let mut fixture = variants[0].clone();
        fixture["serverVersion"] = json!(bounded_text(&initialized["serverInfo"]["version"], 128));
        fixture["variants"] = json!(variants);
        Ok(fixture)
    }).await.map_err(|_| "Fixture setup exceeded its budget".to_string()).and_then(|value|value);
    let stopped = tokio::time::timeout(Duration::from_secs(10), rpc.stop()).await;
    drain.abort();
    if !matches!(stopped, Ok(Ok(()))) {
        return Err("Fixture server did not stop cleanly".into());
    }
    result
}

fn resume_text(fixture: &Value, typed: bool) -> String {
    let name = fixture["name"].as_str().unwrap_or(FIXTURE_NAME);
    if typed {
        return format!("Reprendre la formation {name}");
    }
    format!(
        "```tutor-resume-request\n{}\n```",
        json!({"version":1,
        "trainingId":fixture["training_id"],"domainId":fixture["domain_id"],"trainingName":name})
    )
}

fn persisted_response<'a>(snapshot: &'a Value, id: &str) -> Option<&'a Value> {
    let conversation = snapshot["conversations"]
        .as_array()?
        .iter()
        .find(|c| c["id"] == id)?;
    let message = conversation["messages"].as_array()?.last()?;
    (message["role"] == "assistant"
        && message["status"] == "complete"
        && message["privateOutput"] != true
        && message["canRetryCommit"] != true
        && message["text"].as_str().is_some_and(|s| !s.is_empty()))
    .then_some(message)
}

fn exercise_response(message: &Value) -> Result<String, String> {
    // Native finalization emits a validated canonical fence. Do not turn an
    // introduction, clarification, refusal or uncommitted card into a success.
    let text = message["text"].as_str().ok_or("No persisted activity")?;
    let prefix = "```tutor-activity\n";
    if text.matches(prefix).count() != 1 {
        return Err("Expected one native exercise".into());
    }
    let body = text
        .split_once(prefix)
        .and_then(|(_, rest)| rest.split_once("\n```"))
        .map(|(body, _)| body)
        .ok_or("No complete native exercise")?;
    let card: Value = serde_json::from_str(body).map_err(|_| "Invalid native exercise")?;
    if card["kind"] != "exercise" || card["attemptId"].as_str().is_none_or(str::is_empty) {
        return Err(
            "The server did not choose a frozen exercise; correction sample unavailable".into(),
        );
    }
    Ok(format!(
        "```tutor-response\n{}\n```",
        json!({"version":1,"activityId":card["id"],
        "kind":"exercise","trainingId":card["trainingId"],"domainId":card["domainId"],
        "concept":card["concept"],"attemptId":card["attemptId"],
        "answer":"Je ne sais pas résoudre cette équation.","responseTimeSeconds":1.0})
    ))
}

async fn send(
    backend: &Backend,
    conversation: &mut Option<String>,
    text: String,
) -> Result<Value, String> {
    backend
        .queue_prompt(Some(text.clone()), conversation.clone())
        .await?;
    if conversation.is_none() {
        *conversation = Some(backend.start_conversation(None, None).await?.id);
    }
    backend
        .send_prompt(conversation.clone().unwrap(), text)
        .await
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
            "contractVersion":2,"host":std::env::consts::OS,"build":build,
            "environment":options.environment,"harness":options.harness,"scenario":options.scenario,
            "requestedConfiguration":{"model":options.model,"effort":options.effort,"speed":options.speed},
            "liveModel":true,"status":"incomplete","stage":"setup","uiRenderMeasured":false
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
        id: format!("latency-{}-{}", options.harness, options.environment),
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
    let requested = AgentSettings {
        model: options.model.clone(),
        reasoning_effort: options.effort.clone(),
        speed: options.speed.clone(),
    };
    let settings = Settings {
        target_id: target.id.clone(),
        manual_target: Some(target.clone()),
        cwd: cwd.clone(),
        agent: requested.clone(),
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
    let fixture = if matches!(options.scenario.as_str(), "resume" | "correction") {
        Some(
            seed_fixture(
                &target,
                &settings,
                &cwd,
                if options.scenario == "correction" {
                    options.turns
                } else {
                    1
                },
            )
            .await?,
        )
    } else {
        None
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
    let context_read = options.scenario == "context-read";
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
            if event["type"] == "native_permission" {
                // Mutations are native, limited to this newly created profile.
                // The model never receives permission to perform them itself.
                let allowed = native_operations_allowed(&event["params"]["operations"]);
                let _ = owner
                    .answer_permission(event["id"].clone(), allowed.then(|| "allow_once".into()))
                    .await;
            }
            if event["method"] == "session/request_permission" {
                // This benchmark authorizes one read-only tool on its disposable
                // server. Every unrecognized operation is explicitly denied.
                let allowed =
                    context_read && registry.request_tool(&event) == Some("get_learner_context");
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
    let mut stage = "setup";
    let mut last_configuration = Value::Null;
    let mut prerequisites = Vec::new();
    let result = async {
        let agent = tokio::time::timeout(Duration::from_secs(180), async {
            backend.save_settings(settings).await?;
            let agent = backend.connect_agent(target, cwd).await?;
            if !harness_matches(&agent, &options.harness) {
                return Err("The versioned harness does not match the requested harness".into());
            }
            // Selecting a new target resets saved model preferences. Apply the
            // explicit request only after the target is connected, then verify
            // raw ACP configuration before any model invocation.
            stage = "apply_configuration";
            let applied = backend.set_agent_configuration(requested.clone()).await?;
            last_configuration = configuration_evidence(&applied, &requested);
            if last_configuration["verified"] != true {
                return Err("Requested harness configuration could not be verified".into());
            }
            backend.tutor_overview().await?;
            Ok::<_, String>(agent)
        }).await.map_err(|_| "Harness setup exceeded the qualification budget")??;
        let connect_ms = started.elapsed().as_millis();
        let baseline = configuration_evidence(&backend.agent_configuration().await?, &requested);
        let mut conversation = None;
        for index in 0..options.turns {
            stage = "verify_configuration_before_prompt";
            let before_configuration = configuration_evidence(&backend.agent_configuration().await?, &requested);
            last_configuration = before_configuration.clone();
            if before_configuration["verified"] != true || before_configuration != baseline {
                return Err("Harness configuration changed before prompt".into());
            }
            let current_fixture = fixture.as_ref().map(|fixture| {
                if options.scenario == "correction" { &fixture["variants"][index] } else { fixture }
            });
            let text = match options.scenario.as_str() {
                "context-read" => "Consulte uniquement mon profil avec get_learner_context, puis confirme en une phrase que tu l’as consulté. Ne crée aucune formation et ne modifie rien.".to_string(),
                "explanation" => "Explique en deux phrases pourquoi soustraire le même nombre aux deux membres d’une équation préserve l’égalité. Utilise le contexte préparé, sans appel d’outil.".to_string(),
                "resume" => resume_text(current_fixture.unwrap(), index % 2 == 1),
                "correction" => {
                    stage = "prepare_real_exercise";
                    let preparation = Instant::now();
                    let response = tokio::time::timeout(Duration::from_secs(180), send(&backend,
                        &mut conversation, resume_text(current_fixture.unwrap(), false))).await
                        .map_err(|_| "Exercise preparation exceeded its budget")??;
                    let snapshot = backend.snapshot().await;
                    let answer = persisted_response(&snapshot, conversation.as_deref().unwrap_or_default())
                        .ok_or_else(|| "The exercise was not committed".to_string()).and_then(exercise_response);
                    prerequisites.push(json!({"index":index,"kind":"real_native_exercise",
                        "elapsedMs":preparation.elapsed().as_millis(),"stopReason":stop_reason(&response),"exerciseCommitted":answer.is_ok(),
                        "scope":"Question preparation before the learner submits an answer; excluded from correction latency"}));
                    if stop_reason(&response) != "end_turn" { return Err("Exercise generation was interrupted".into()); }
                    let effective = configuration_evidence(&backend.agent_configuration().await?, &requested);
                    if effective != baseline { return Err("Harness configuration changed while preparing exercise".into()); }
                    answer?
                }
                _ => return Err("Unknown qualification scenario".into()),
            };
            let previous_message = persisted_response(&backend.snapshot().await, conversation.as_deref().unwrap_or_default())
                .map(|message|message["id"].clone());
            flush_observations(&tx).await?;
            let start = {
                let mut capture = records.lock().unwrap();
                capture.clear();
                capture.started
            };
            let previous = backend.agent_timings().await;
            stage = "model_turn";
            let response = tokio::time::timeout(Duration::from_secs(180), send(&backend, &mut conversation, text))
                .await.map_err(|_| "Model turn exceeded the qualification budget")??;
            // The backend drains its own event consumer; this second barrier also
            // drains our diagnostic collector before a snapshot or the next turn.
            flush_observations(&tx).await?;
            let after = backend.agent_timings().await;
            let elapsed_ms=start.elapsed().as_millis();
            // A correction has already generated its question in this session;
            // it is genuinely warm, including the first measured correction.
            let mut turn=turn_report(if options.scenario == "correction" { index + 1 } else { index }, elapsed_ms, &response, &records.lock().unwrap(), &previous, &after);
            turn["index"]=json!(index);
            turn["scenario"]=json!(options.scenario);
            turn["trigger"]=json!(if options.scenario == "resume" && index % 2 == 1 { "typed_resume" } else if options.scenario == "resume" { "structured_resume" } else { "message" });
            let snapshot = backend.snapshot().await;
            turn["persistedResponse"]=json!(persisted_response(&snapshot, conversation.as_deref().unwrap_or_default())
                .is_some_and(|message|Some(&message["id"]) != previous_message.as_ref()
                    && current_fixture.is_none_or(|fixture| {
                        message["trainingContext"]["training_id"] == fixture["training_id"]
                            && message["trainingContext"]["domain_id"] == fixture["domain_id"]
                    })));
            stage = "verify_configuration_after_prompt";
            let configuration=backend.agent_configuration().await?;
            let effective = configuration_evidence(&configuration, &requested);
            last_configuration = effective.clone();
            turn["configurationVerified"]=json!(effective["verified"] == true && effective == baseline);
            turn["effectiveConfiguration"]=effective;
            turns.push(turn);
        }
        Ok::<Value, String>(json!({
            "contractVersion":2,"host":std::env::consts::OS,"environment":options.environment,
            "distribution":options.distribution,"liveModel":true,
            "harness":options.harness,"scenario":options.scenario,
            "agent":{"name":bounded_text(&agent["agentInfo"]["name"],128),"version":bounded_text(&agent["agentInfo"]["version"],64)},
            "connectMs":connect_ms,"turns":turns,
            "prerequisites":prerequisites,"coldMeasured":options.scenario != "correction",
            "fixture":fixture.as_ref().map(|fixture| json!({"version":1,"createdIdempotently":true,"serverVersion":fixture["serverVersion"],"variants":fixture["variants"].as_array().map(Vec::len)})),
            "latencyTargetMs":5000,"uiRenderMeasured":false,"pedagogyQualified":false,
            "requestedConfiguration":{"model":options.model,"effort":options.effort,"speed":options.speed},
            "scope":"Isolated actual harness prompts through native backend; provider generation and native persistence included; desktop render not measured"
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
            json!({"contractVersion":2,"host":std::env::consts::OS,"environment":options.environment,"harness":options.harness,"scenario":options.scenario,"requestedConfiguration":{"model":options.model,"effort":options.effort,"speed":options.speed},"effectiveConfiguration":last_configuration,"liveModel":true,"status":"incomplete","stage":stage,"uiRenderMeasured":false,"pedagogyQualified":false,"turns":turns,"prerequisites":prerequisites,"events":records.lock().unwrap().events.clone()})
        }
    };
    let passed = shutdown
        && collected
        && report["status"] != "incomplete"
        && report["turns"]
            .as_array()
            .is_some_and(|turns| turns.len() == options.turns && turns.iter().all(passed_turn));
    report["build"] = build;
    report["totalElapsedMs"] = json!(started.elapsed().as_millis());
    report["configurationSource"] = json!("ACP advertised currentValue, not saved intentions");
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

    #[test]
    fn disposable_fixture_authorizes_native_resume_but_not_unknown_operations() {
        assert!(native_operations_allowed(&json!([
            "prepare_pedagogical_turn",
            "select_conversation_training",
            "get_next_activity",
            "get_conversation_context",
            "prepare_assessment_attempt",
            "record_learning_event"
        ])));
        assert!(!native_operations_allowed(&json!([
            "prepare_pedagogical_turn",
            "delete_training"
        ])));
        assert!(!native_operations_allowed(&json!([])));
        assert!(!native_operations_allowed(&json!(null)));
    }

    fn advertised_configuration() -> Value {
        json!({"connected":true,"current":{"model":"wanted","reasoningEffort":"high","speed":"balanced"},
            "configOptions":[
                {"id":"model","category":"model","currentValue":"wanted"},
                {"id":"effort","category":"thought_level","currentValue":"high",
                    "options":[{"value":"low"},{"value":"high"}]},
                {"id":"fast-mode","category":"model_config","type":"boolean","currentValue":false}]})
    }

    #[test]
    fn saved_intentions_do_not_prove_the_effective_model_or_effort() {
        let requested = AgentSettings {
            model: "wanted".into(),
            reasoning_effort: "high".into(),
            speed: "balanced".into(),
        };
        let configuration = advertised_configuration();
        assert_eq!(
            configuration_evidence(&configuration, &requested)["verified"],
            true
        );
        for (index, value) in [(0, json!("default")), (1, json!("low")), (2, json!(true))] {
            let mut changed = configuration.clone();
            changed["configOptions"][index]["currentValue"] = value;
            assert_eq!(
                configuration_evidence(&changed, &requested)["verified"],
                false
            );
        }
        let missing = json!({"current":configuration["current"],"configOptions":[]});
        assert_eq!(
            configuration_evidence(&missing, &requested)["verified"],
            false
        );
        let mut duplicate = configuration;
        duplicate["configOptions"]
            .as_array_mut()
            .unwrap()
            .push(json!({"category":"model","currentValue":"wanted"}));
        assert_eq!(
            configuration_evidence(&duplicate, &requested)["verified"],
            false
        );
    }

    #[test]
    fn speed_evidence_distinguishes_fast_mode_and_effort_fallback() {
        let mut configuration = advertised_configuration();
        configuration["configOptions"].as_array_mut().unwrap().pop();
        let requested = AgentSettings {
            model: "wanted".into(),
            reasoning_effort: String::new(),
            speed: "fast".into(),
        };
        assert_eq!(
            configuration_evidence(&configuration, &requested)["verified"],
            false
        );
        configuration["configOptions"][1]["currentValue"] = json!("low");
        let proof = configuration_evidence(&configuration, &requested);
        assert_eq!(proof["verified"], true);
        assert_eq!(proof["speedMechanism"], "effort");
        configuration["configOptions"].as_array_mut().unwrap().pop();
        assert_eq!(
            configuration_evidence(&configuration, &requested)["verified"],
            false
        );
    }

    #[test]
    fn qualification_requires_configuration_proof_in_addition_to_timing() {
        let mut turn = json!({"stopReason":"end_turn","elapsedMs":100,"contextToolCompleted":true,
            "sessionReused":true,"diagnosticsComplete":true,"unexpectedToolCompleted":false});
        assert!(passed_response(&turn));
        assert!(!passed_turn(&turn));
        turn["configurationVerified"] = json!(true);
        assert!(passed_turn(&turn));
        turn["scenario"] = json!("resume");
        turn["events"] = json!([]);
        turn["persistedResponse"] = json!(false);
        assert!(!passed_turn(&turn));
        turn["persistedResponse"] = json!(true);
        assert!(passed_turn(&turn));
        turn["events"] = json!([{"type":"observed_tool","name":EXPECTED_TOOL,"completed":true}]);
        assert!(!passed_turn(&turn));
    }

    #[test]
    fn partial_private_and_uncommitted_responses_are_not_final() {
        let complete = json!({"role":"assistant","status":"complete","text":"Visible answer"});
        for change in [
            json!({"status":"streaming"}),
            json!({"privateOutput":true}),
            json!({"canRetryCommit":true}),
            json!({"role":"user"}),
            json!({"text":""}),
        ] {
            let mut message = complete.clone();
            message
                .as_object_mut()
                .unwrap()
                .extend(change.as_object().unwrap().clone());
            assert!(persisted_response(
                &json!({"conversations":[{"id":"c","messages":[message]}]}),
                "c"
            )
            .is_none());
        }
        assert!(persisted_response(
            &json!({"conversations":[{"id":"c","messages":[complete]}]}),
            "c"
        )
        .is_some());
    }

    #[test]
    fn corrections_require_a_server_frozen_exercise() {
        assert!(exercise_response(&json!({"text":"An explanation"})).is_err());
        let mut card = json!({"version":1,"id":"activity","kind":"exercise","trainingId":"training",
            "domainId":"domain","concept":"concept"});
        let message = |card: &Value| json!({"text":format!("```tutor-activity\n{card}\n```")});
        assert!(exercise_response(&message(&card)).is_err());
        card["attemptId"] = json!("server-attempt");
        let answer = exercise_response(&message(&card)).unwrap();
        assert!(answer.contains("server-attempt"));
        assert!(answer.contains("tutor-response"));
        card["kind"] = json!("quiz");
        assert!(exercise_response(&message(&card)).is_err());
    }

    #[test]
    fn versioned_harness_identity_must_match_the_requested_provider() {
        assert!(harness_matches(
            &json!({"agentInfo":{"name":"@agentclientprotocol/codex-acp","version":"1"}}),
            "codex"
        ));
        assert!(harness_matches(
            &json!({"agentInfo":{"name":"@agentclientprotocol/claude-agent-acp","version":"1"}}),
            "claude"
        ));
        assert!(!harness_matches(
            &json!({"agentInfo":{"name":"claude-agent-acp","version":"1"}}),
            "codex"
        ));
        assert!(!harness_matches(
            &json!({"agentInfo":{"name":"codex-acp"}}),
            "codex"
        ));
    }

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
        assert!(passed_response(&turn_report(
            0,
            100,
            &json!({"stopReason":"end_turn"}),
            &capture,
            &before,
            &cold
        )));
        assert!(!passed_response(&turn_report(
            1,
            100,
            &json!({"stopReason":"end_turn"}),
            &capture,
            &before,
            &cold
        )));
        let second_creation = [cold.clone(), pair(4, "session/new")].concat();
        assert!(!passed_response(&turn_report(
            0,
            100,
            &json!({"stopReason":"end_turn"}),
            &capture,
            &before,
            &second_creation
        )));
        let warm = pair(4, "session/prompt");
        assert!(passed_response(&turn_report(
            1,
            100,
            &json!({"stopReason":"end_turn"}),
            &capture,
            &cold,
            &warm
        )));
        let loaded = [pair(4, "session/load"), pair(5, "session/prompt")].concat();
        assert!(!passed_response(&turn_report(
            1,
            100,
            &json!({"stopReason":"end_turn"}),
            &capture,
            &cold,
            &loaded
        )));
        capture.truncated = true;
        assert!(!passed_response(&turn_report(
            1,
            100,
            &json!({"stopReason":"end_turn"}),
            &capture,
            &cold,
            &warm
        )));
    }

    #[test]
    fn complete_response_over_five_seconds_fails_the_latency_target() {
        let before = pair(1, "initialize");
        let after = [pair(2, "session/new"), pair(3, "session/prompt")].concat();
        let mut capture = Capture::default();
        capture.observe(&tool("call", "completed", Some(EXPECTED_TOOL)));
        for (elapsed, accepted) in [(5000, true), (5001, false), (60000, false)] {
            let report = turn_report(
                0,
                elapsed,
                &json!({"stopReason":"end_turn"}),
                &capture,
                &before,
                &after,
            );
            assert_eq!(passed_response(&report), accepted);
        }
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
            assert!(!passed_response(&report));
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
                "backend_pedagogy.rs",
                "backend_maintenance.rs",
                "learning_ui_prepared.rs",
                "learning_ui.rs",
                "learning_resume.rs",
                "learning_ui_resume_prepared.rs",
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
