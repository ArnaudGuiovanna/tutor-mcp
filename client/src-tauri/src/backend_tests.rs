// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

use super::*;

#[tokio::test]
async fn switching_from_wsl_home_to_packaged_agent_uses_the_accounts_private_directory() {
    let directory = tempfile::tempdir().unwrap();
    let backend = Backend::new(directory.path(), Arc::new(|_, _| {})).unwrap();
    let mut target = fixture_target();
    target.id = "bundled:codex".into();
    let mut settings = backend.settings().await;
    settings.target_id = target.id.clone();
    settings.cwd = "~".into();
    backend.save_settings(settings).await.unwrap();
    let cwd = backend.resolve_agent_directory(&target, "~").await.unwrap();
    assert!(Path::new(&cwd).is_absolute() && Path::new(&cwd).is_dir());
    assert_eq!(Path::new(&cwd), directory.path().join("work"));
    assert_eq!(backend.settings().await.cwd, cwd);
    assert!(discovery::agent_cwd(&target, &cwd).await.is_ok());
    target.id = "wsl:Ubuntu".into();
    target.environment = "wsl".into();
    assert_eq!(
        backend.resolve_agent_directory(&target, "~").await.unwrap(),
        "~"
    );
}

fn fixture_target() -> AgentTarget {
    AgentTarget {
        id: "manual".into(),
        label: "Test subprocess".into(),
        environment: "native".into(),
        distribution: None,
        wsl_user: None,
        adapter_path: Some(
            Path::new(env!("CARGO_MANIFEST_DIR"))
                .join("../tests/backend/fake-runtime.cjs")
                .canonicalize()
                .unwrap()
                .to_string_lossy()
                .into_owned(),
        ),
        codex_path: None,
        adapter_version: None,
        codex_version: None,
        available: true,
        detail: String::new(),
    }
}

// Capture diagnostics only from this deterministic, repository-owned fixture.
// Production ACP stderr can contain credentials and remains private. Launch the
// same canonical script through discovery, without depending on the RPC reader
// that otherwise turns a Node loader failure into a generic connection EOF.
async fn fixture_startup_diagnostic(directory: &Path) -> Result<(), String> {
    use std::time::Duration;
    use tokio::io::AsyncReadExt;

    const LIMIT: u64 = 4096;
    let target = fixture_target();
    let mut spec = discovery::launch_spec(&target, directory.to_str().unwrap()).await?;
    spec.args.push("--self-test".into());
    let invocation = format!(
        "program={:?}, args={:?}, cwd={:?}",
        spec.program, spec.args, spec.cwd
    );
    let mut command = tokio::process::Command::new(&spec.program);
    command
        .args(&spec.args)
        .env_remove("CODEX_PATH")
        .envs(spec.env)
        .current_dir(directory)
        .stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::piped())
        .kill_on_drop(true);
    #[cfg(windows)]
    command.creation_flags(0x0800_0000); // CREATE_NO_WINDOW, as in production.
    let mut child = command
        .spawn()
        .map_err(|error| format!("{invocation}: {error}"))?;
    let mut stdout = child.stdout.take().unwrap().take(LIMIT + 1);
    let mut stderr = child.stderr.take().unwrap().take(LIMIT + 1);
    let capture = tokio::time::timeout(Duration::from_secs(5), async {
        let mut output = Vec::new();
        let mut diagnostic = Vec::new();
        let (status, out, err) = tokio::join!(
            child.wait(),
            stdout.read_to_end(&mut output),
            stderr.read_to_end(&mut diagnostic)
        );
        (status, out, err, output, diagnostic)
    })
    .await;
    let (status, out, err, output, diagnostic) = match capture {
        Ok(capture) => capture,
        Err(_) => {
            let _ = child.start_kill();
            let _ = tokio::time::timeout(Duration::from_secs(1), child.wait()).await;
            return Err(format!("Fixture Node timed out: {invocation}"));
        }
    };
    let details = format!(
        "{invocation}; status={status:?}; stdout={:?}; fixture stderr={:?}",
        String::from_utf8_lossy(&output[..output.len().min(LIMIT as usize)]),
        String::from_utf8_lossy(&diagnostic[..diagnostic.len().min(LIMIT as usize)])
    );
    if output.len() > LIMIT as usize
        || diagnostic.len() > LIMIT as usize
        || out.is_err()
        || err.is_err()
        || !status.is_ok_and(|status| status.success())
    {
        return Err(details);
    }
    let value: Value = serde_json::from_slice(&output).map_err(|_| details.clone())?;
    if value["fixture"] != "tutor-client-backend"
        || value["cwd"]
            .as_str()
            .and_then(|cwd| Path::new(cwd).canonicalize().ok())
            != directory.canonicalize().ok()
    {
        return Err(details);
    }
    Ok(())
}

#[tokio::test]
async fn canonical_node_fixture_starts_through_the_production_launcher() {
    let directory = tempfile::tempdir().unwrap();
    fixture_startup_diagnostic(directory.path()).await.unwrap();
}

async fn connected(
    directory: &Path,
) -> (Arc<Backend>, tokio::sync::mpsc::UnboundedReceiver<Value>) {
    let (events, receiver) = tokio::sync::mpsc::unbounded_channel();
    let backend = Backend::new(
        directory,
        Arc::new(move |name, value| {
            let _ = events.send(json!({"name":name,"value":value}));
        }),
    )
    .unwrap();
    let target = fixture_target();
    let settings = Settings {
        target_id: target.id.clone(),
        manual_target: Some(target.clone()),
        cwd: directory.to_string_lossy().into_owned(),
        tutor: crate::workspace::TutorSettings {
            mode: "local".into(),
            activity: "auto".into(),
            url: String::new(),
            command: target.adapter_path.clone().unwrap(),
            args: vec!["--local".into()],
        },
        ..Default::default()
    };
    backend.save_settings(settings).await.unwrap();
    if let Err(error) = backend
        .connect_agent(target, directory.to_string_lossy().into_owned())
        .await
    {
        panic!(
            "ACP fixture connection failed: {error}; fixture-only startup diagnostic: {:?}",
            fixture_startup_diagnostic(directory).await
        );
    }
    backend.tutor_overview().await.unwrap();
    (backend, receiver)
}

async fn intent(backend: &Backend, validated: bool) -> String {
    backend
        .save_intent(LearningIntent {
            title: "Comprendre les saisons".into(),
            constraints: "15 minutes, sources accessibles".into(),
            validated,
            ..Default::default()
        })
        .await
        .unwrap();
    backend.store.lock().await.data.intents[0].id.clone()
}

fn latency_requests(directory: &Path) -> Vec<Value> {
    std::fs::read_to_string(directory.join("fixture-rpc.jsonl"))
        .unwrap_or_default()
        .lines()
        .map(|line| serde_json::from_str(line).unwrap())
        .collect()
}

async fn wait_for_latency_request(directory: &Path, method: &str) {
    tokio::time::timeout(std::time::Duration::from_secs(5), async {
        while !latency_requests(directory)
            .iter()
            .any(|row| row["method"] == method)
        {
            tokio::time::sleep(std::time::Duration::from_millis(5)).await;
        }
    })
    .await
    .unwrap();
}

#[tokio::test]
#[ignore = "Controlled 50-turn latency budget; run explicitly with --ignored --nocapture"]
async fn warm_turn_preparation_stays_under_500ms_p95_with_100ms_tools() {
    use std::time::{Duration, Instant};

    let directory = tempfile::tempdir().unwrap();
    let (backend, mut events) = connected(directory.path()).await;
    let conversation = backend.start_conversation(None, None).await.unwrap();
    backend
        .send_prompt(conversation.id.clone(), "Warm the existing session".into())
        .await
        .unwrap();
    std::fs::write(directory.path().join("fixture-rpc.jsonl"), "").unwrap();
    std::fs::write(
        directory.path().join("fixture-delays.json"),
        json!({"toolMs":100}).to_string(),
    )
    .unwrap();
    let mut preparation_times = Vec::new();
    for turn in 0..50 {
        while events.try_recv().is_ok() {}
        let started = Instant::now();
        let pending = backend.send_prompt(conversation.id.clone(), format!("Warm turn {turn}"));
        tokio::pin!(pending);
        let mut prompt_started = false;
        let mut operation_id = None;
        tokio::time::timeout(Duration::from_secs(5), async {
            loop {
                tokio::select! {
                    event = events.recv() => {
                        let event = event.expect("backend events remain open");
                        let value = &event["value"];
                        if value["type"] == "turn_phase" && value["phase"] == "preparing"
                            && value["state"] == "started"
                        {
                            operation_id = value["operationId"].as_str().map(str::to_owned);
                        }
                        if value["type"] == "rpc_timing" && value["method"] == "session/prompt"
                            && value["state"] == "started"
                        {
                            assert!(!prompt_started, "only one prompt dispatch per turn");
                            assert_eq!(value["operationId"].as_str(), operation_id.as_deref());
                            preparation_times.push(started.elapsed().as_secs_f64() * 1000.0);
                            prompt_started = true;
                        }
                    }
                    result = &mut pending => {
                        result.unwrap();
                        break;
                    }
                }
            }
        })
        .await
        .unwrap();
        assert!(prompt_started, "missing prompt timing for turn {turn}");
    }
    let records = latency_requests(directory.path());
    assert_eq!(
        records
            .iter()
            .filter(|r| r["method"] == "session/prompt")
            .count(),
        50
    );
    assert!(!records
        .iter()
        .any(|r| r["method"] == "session/load" || r["method"] == "session/new"));
    assert!(!records.iter().any(|r| [
        "get_learner_context",
        "get_my_formations",
        "list_available_formations"
    ]
    .iter()
    .any(|tool| r["tool"] == *tool)));
    preparation_times.sort_by(f64::total_cmp);
    let p95 = preparation_times[47];
    println!(
        "50 warm turns, tool delay 100ms: preparation P50={:.1}ms, P95={p95:.1}ms, max={:.1}ms; session/new=0, session/load=0",
        preparation_times[24], preparation_times[49]
    );
    assert!(
        p95 <= 500.0,
        "warm preparation P95 {p95:.1}ms exceeds 500ms"
    );
    backend.shutdown().await;
}

#[tokio::test]
async fn warm_turns_reuse_the_session_without_catalog_or_history_replay() {
    for load_supported in [true, false] {
        let directory = tempfile::tempdir().unwrap();
        let (backend, _) = connected(directory.path()).await;
        backend.agent.lock().await.as_mut().unwrap().capabilities["loadSession"] =
            json!(load_supported);
        let conversation = backend.start_conversation(None, None).await.unwrap();
        backend
            .send_prompt(conversation.id.clone(), "FIRST_UNIQUE_HISTORY".into())
            .await
            .unwrap();
        std::fs::write(directory.path().join("fixture-rpc.jsonl"), "").unwrap();
        // Explicit selection/resume is also idempotent for an already live session.
        backend
            .start_conversation(None, Some(conversation.id.clone()))
            .await
            .unwrap();
        backend
            .send_prompt(conversation.id.clone(), "SECOND_UNIQUE_CURRENT".into())
            .await
            .unwrap();
        let records = latency_requests(directory.path());
        assert_eq!(
            records
                .iter()
                .filter(|r| r["method"] == "session/prompt")
                .count(),
            1
        );
        assert!(!records
            .iter()
            .any(|r| r["method"] == "session/load" || r["method"] == "session/new"));
        assert!(!records.iter().any(|r| [
            "get_learner_context",
            "get_my_formations",
            "list_available_formations"
        ]
        .iter()
        .any(|tool| r["tool"] == *tool)));
        let prompt_index = records
            .iter()
            .position(|r| r["method"] == "session/prompt")
            .unwrap();
        assert_eq!(
            records[..prompt_index]
                .iter()
                .filter(|r| r["tool"] == "get_conversation_context")
                .count(),
            1
        );
        let sessions: Value = serde_json::from_slice(
            &std::fs::read(directory.path().join("fixture-agent-sessions.json")).unwrap(),
        )
        .unwrap();
        let prompt = sessions[conversation.agent_session_id.as_ref().unwrap()]["lastPrompt"]
            .as_str()
            .unwrap();
        assert!(prompt.contains("SECOND_UNIQUE_CURRENT"));
        assert!(!prompt.contains("FIRST_UNIQUE_HISTORY"));
        backend.shutdown().await;
    }
}

#[tokio::test]
async fn changed_transport_refreshes_once_and_keeps_claude_session_metadata_stable() {
    let directory = tempfile::tempdir().unwrap();
    let (backend, _) = connected(directory.path()).await;
    let conversation = backend.start_conversation(None, None).await.unwrap();
    let id = conversation.agent_session_id.as_ref().unwrap();
    let before: Value = serde_json::from_slice(
        &std::fs::read(directory.path().join("fixture-agent-sessions.json")).unwrap(),
    )
    .unwrap();
    backend
        .agent
        .lock()
        .await
        .as_mut()
        .unwrap()
        .mcp_configurations
        .insert(id.clone(), "expired-transport-fingerprint".into());
    std::fs::write(directory.path().join("fixture-rpc.jsonl"), "").unwrap();
    backend
        .send_prompt(conversation.id.clone(), "Premier".into())
        .await
        .unwrap();
    backend
        .send_prompt(conversation.id.clone(), "Deuxième".into())
        .await
        .unwrap();
    assert_eq!(
        latency_requests(directory.path())
            .iter()
            .filter(|r| r["method"] == "session/load")
            .count(),
        1
    );
    let after: Value = serde_json::from_slice(
        &std::fs::read(directory.path().join("fixture-agent-sessions.json")).unwrap(),
    )
    .unwrap();
    assert_eq!(before[id]["_meta"], after[id]["_meta"]);
    assert_eq!(after[id]["_meta"]["disableBuiltInTools"], true);
    backend.shutdown().await;
}

#[tokio::test]
async fn restart_restores_once_without_reinjecting_the_restored_history() {
    let directory = tempfile::tempdir().unwrap();
    let (backend, _) = connected(directory.path()).await;
    let conversation = backend.start_conversation(None, None).await.unwrap();
    backend
        .send_prompt(conversation.id.clone(), "UNIQUE_OLD_TURN".into())
        .await
        .unwrap();
    backend.stop_agent().await.unwrap();
    backend
        .connect_agent(
            fixture_target(),
            directory.path().to_string_lossy().into_owned(),
        )
        .await
        .unwrap();
    std::fs::write(directory.path().join("fixture-rpc.jsonl"), "").unwrap();
    backend
        .send_prompt(conversation.id.clone(), "UNIQUE_NEW_TURN".into())
        .await
        .unwrap();
    assert_eq!(
        latency_requests(directory.path())
            .iter()
            .filter(|r| r["method"] == "session/load")
            .count(),
        1
    );
    let sessions: Value = serde_json::from_slice(
        &std::fs::read(directory.path().join("fixture-agent-sessions.json")).unwrap(),
    )
    .unwrap();
    assert!(
        !sessions[conversation.agent_session_id.unwrap()]["lastPrompt"]
            .as_str()
            .unwrap()
            .contains("UNIQUE_OLD_TURN")
    );
    backend.shutdown().await;
}

#[tokio::test]
async fn stop_during_context_or_load_keeps_a_paused_outbox_and_never_sends_prompt() {
    for during_load in [false, true] {
        let directory = tempfile::tempdir().unwrap();
        let (backend, _) = connected(directory.path()).await;
        let conversation = backend.start_conversation(None, None).await.unwrap();
        if during_load {
            backend
                .agent
                .lock()
                .await
                .as_mut()
                .unwrap()
                .mcp_configurations
                .clear();
        }
        backend
            .queue_prompt(
                Some("Message conservé".into()),
                Some(conversation.id.clone()),
            )
            .await
            .unwrap();
        std::fs::write(directory.path().join("fixture-rpc.jsonl"), "").unwrap();
        std::fs::write(
            directory.path().join("fixture-delays.json"),
            json!({"toolMs":if during_load {0}else{1500},"loadMs":1500}).to_string(),
        )
        .unwrap();
        let worker = backend.clone();
        let id = conversation.id.clone();
        let running =
            tokio::spawn(async move { worker.send_prompt(id, "Message conservé".into()).await });
        wait_for_latency_request(
            directory.path(),
            if during_load {
                "session/load"
            } else {
                "tools/call"
            },
        )
        .await;
        backend.cancel_prompt(conversation.id).await.unwrap();
        let result = tokio::time::timeout(std::time::Duration::from_secs(3), running)
            .await
            .unwrap()
            .unwrap()
            .unwrap();
        assert_eq!(result["stopReason"], "cancelled");
        assert!(!latency_requests(directory.path())
            .iter()
            .any(|r| r["method"] == "session/prompt"));
        let stored = WorkspaceStore::load(directory.path()).unwrap();
        let pending = stored.data.pending_message.unwrap();
        assert!(pending.paused);
        assert_eq!(pending.text, "Message conservé");
        assert!(stored.data.conversations[0].messages.is_empty());
        backend.shutdown().await;
    }
}

#[tokio::test]
async fn stop_new_session_and_waiting_lock_are_cancellable_before_conversation_exists() {
    for blocked_lock in [true, false] {
        let directory = tempfile::tempdir().unwrap();
        let (backend, _) = connected(directory.path()).await;
        backend
            .queue_prompt(Some("Nouvelle demande".into()), None)
            .await
            .unwrap();
        std::fs::write(directory.path().join("fixture-rpc.jsonl"), "").unwrap();
        std::fs::write(
            directory.path().join("fixture-delays.json"),
            r#"{"newMs":1500}"#,
        )
        .unwrap();
        let held = if blocked_lock {
            Some(backend.operation.lock().await)
        } else {
            None
        };
        let worker = backend.clone();
        let running = tokio::spawn(async move { worker.start_conversation(None, None).await });
        if blocked_lock {
            tokio::time::timeout(std::time::Duration::from_secs(2), async {
                while backend.preparing.lock().unwrap().is_none() {
                    tokio::task::yield_now().await;
                }
            })
            .await
            .unwrap();
        } else {
            wait_for_latency_request(directory.path(), "session/new").await;
        }
        backend.cancel_prompt(String::new()).await.unwrap();
        let result = tokio::time::timeout(std::time::Duration::from_secs(3), running)
            .await
            .unwrap()
            .unwrap();
        assert!(result.is_err());
        assert!(
            backend
                .store
                .lock()
                .await
                .data
                .pending_message
                .as_ref()
                .unwrap()
                .paused
        );
        assert!(!latency_requests(directory.path())
            .iter()
            .any(|r| r["method"] == "session/prompt"));
        drop(held);
        backend.shutdown().await;
    }
}

#[tokio::test]
async fn stop_before_queued_outbox_gets_the_lock_stays_paused_after_reopening() {
    for existing_conversation in [false, true] {
        let directory = tempfile::tempdir().unwrap();
        let (backend, _) = connected(directory.path()).await;
        let conversation_id = if existing_conversation {
            Some(backend.start_conversation(None, None).await.unwrap().id)
        } else {
            None
        };
        std::fs::write(directory.path().join("fixture-rpc.jsonl"), "").unwrap();
        let held = backend.operation.lock().await;
        let worker = backend.clone();
        let id = conversation_id.clone();
        let queued = tokio::spawn(async move {
            worker
                .queue_prompt(Some("Message encore en attente du verrou".into()), id)
                .await
        });
        tokio::time::timeout(std::time::Duration::from_secs(2), async {
            while backend.preparing.lock().unwrap().is_none() {
                tokio::task::yield_now().await;
            }
        })
        .await
        .unwrap();
        backend
            .cancel_prompt(conversation_id.clone().unwrap_or_default())
            .await
            .unwrap();
        assert!(backend.store.lock().await.data.pending_message.is_none());
        drop(held);
        let queued_state = queued.await.unwrap().unwrap();
        assert_eq!(queued_state["pendingMessage"]["paused"], true);
        assert!(backend.preparing.lock().unwrap().is_none());
        backend.shutdown().await;
        drop(backend);

        // No second cancellation from the UI: disk state alone must prevent
        // reconnect auto-resume after the window was closed.
        let reopened = Backend::new(directory.path(), Arc::new(|_, _| {})).unwrap();
        reopened
            .connect_agent(
                fixture_target(),
                directory.path().to_string_lossy().into_owned(),
            )
            .await
            .unwrap();
        let state = reopened.snapshot().await;
        assert_eq!(state["pendingMessage"]["paused"], true);
        assert_eq!(
            state["pendingMessage"]["text"],
            "Message encore en attente du verrou"
        );
        assert!(!latency_requests(directory.path())
            .iter()
            .any(|row| row["method"] == "session/prompt"));
        let explicit = reopened
            .queue_prompt(Some("Nouvelle décision explicite".into()), conversation_id)
            .await
            .unwrap();
        assert_eq!(explicit["pendingMessage"]["paused"], false);
        reopened.shutdown().await;
    }
}

#[tokio::test]
async fn durable_completion_is_published_before_optional_context_refresh_finishes() {
    let directory = tempfile::tempdir().unwrap();
    let (backend, mut events) = connected(directory.path()).await;
    let conversation = backend.start_conversation(None, None).await.unwrap();
    while events.try_recv().is_ok() {}
    let result = tokio::time::timeout(
        std::time::Duration::from_secs(3),
        backend.send_prompt(conversation.id.clone(), "CONTEXT_REFRESH".into()),
    )
    .await;
    let held = directory.path().join("fixture-hold-context-refresh");
    assert!(held.is_file());
    std::fs::remove_file(held).unwrap();
    assert_eq!(
        result
            .expect("completion must not await the context refresh")
            .unwrap()["stopReason"],
        "end_turn"
    );
    let mut completed = false;
    let mut invalidated = false;
    while let Ok(event) = events.try_recv() {
        completed |= event["name"] == "client:state"
            && event["value"]["conversations"][0]["messages"][1]["status"] == "complete";
        invalidated |= event["value"]["type"] == "workspace_invalidated";
    }
    assert!(completed && invalidated);
    backend.shutdown().await;
}

#[tokio::test]
async fn changed_agent_preferences_apply_to_every_reused_session_without_reload() {
    let directory = tempfile::tempdir().unwrap();
    let (backend, _) = connected(directory.path()).await;
    let first = backend.start_conversation(None, None).await.unwrap();
    let second = backend.start_conversation(None, None).await.unwrap();
    let wanted = crate::workspace::AgentSettings {
        model: "fixture-fast".into(),
        reasoning_effort: "high".into(),
        speed: "balanced".into(),
    };
    backend
        .set_agent_configuration(wanted.clone())
        .await
        .unwrap();
    std::fs::write(directory.path().join("fixture-rpc.jsonl"), "").unwrap();
    backend
        .select_conversation(Some(first.id.clone()))
        .await
        .unwrap();
    backend
        .send_prompt(first.id.clone(), "Continue".into())
        .await
        .unwrap();
    let records = latency_requests(directory.path());
    assert!(!records.iter().any(|record| matches!(
        record["method"].as_str(),
        Some("session/new" | "session/load")
    )));
    let sessions: Value = serde_json::from_slice(
        &std::fs::read(directory.path().join("fixture-agent-sessions.json")).unwrap(),
    )
    .unwrap();
    for conversation in [first, second] {
        let configuration =
            &sessions[conversation.agent_session_id.as_deref().unwrap()]["fixtureConfig"];
        assert_eq!(configuration[0]["currentValue"], "fixture-fast");
        assert_eq!(configuration[1]["currentValue"], "high");
        assert_eq!(configuration[2]["currentValue"], "off");
    }
    assert_eq!(backend.settings().await.agent.reasoning_effort, "high");
    backend.shutdown().await;
}

#[tokio::test]
async fn agent_options_and_language_survive_sessions_and_reject_unknown_choices() {
    let directory = tempfile::tempdir().unwrap();
    let (backend, _) = connected(directory.path()).await;
    let available = backend.agent_configuration().await.unwrap();
    assert_eq!(available["supported"]["model"], true);
    assert_eq!(available["supported"]["reasoningEffort"], true);
    let wanted = crate::workspace::AgentSettings {
        model: "fixture-fast".into(),
        reasoning_effort: "high".into(),
        speed: "deep".into(),
    };
    backend
        .set_agent_configuration(wanted.clone())
        .await
        .unwrap();
    {
        let mut store = backend.store.lock().await;
        store
            .transaction(|data| {
                data.settings.language = "en".into();
                Ok(())
            })
            .unwrap();
    }
    let conversation = backend.start_conversation(None, None).await.unwrap();
    backend
        .send_prompt(conversation.id.clone(), "Hello".into())
        .await
        .unwrap();
    let sessions: Value = serde_json::from_slice(
        &std::fs::read(directory.path().join("fixture-agent-sessions.json")).unwrap(),
    )
    .unwrap();
    let active = &sessions[conversation.agent_session_id.as_deref().unwrap()];
    assert!(active["lastPrompt"]
        .as_str()
        .unwrap()
        .starts_with("LANGUE / LANGUAGE: English."));
    assert_eq!(active["fixtureConfig"][0]["currentValue"], "fixture-fast");
    assert_eq!(active["fixtureConfig"][1]["currentValue"], "high");
    let restored = WorkspaceStore::load(directory.path()).unwrap();
    assert_eq!(restored.data.settings.language, "en");
    assert_eq!(restored.data.settings.agent.model, wanted.model);
    assert!(backend
        .set_agent_configuration(crate::workspace::AgentSettings {
            model: "invented".into(),
            ..Default::default()
        })
        .await
        .is_err());
    assert_eq!(backend.settings().await.agent.model, "fixture-fast");
    backend.shutdown().await;
}

#[tokio::test]
async fn agent_preferences_reset_on_target_change_but_survive_other_settings_changes() {
    for replace_manual_program in [false, true] {
        let directory = tempfile::tempdir().unwrap();
        let (backend, _) = connected(directory.path()).await;
        let wanted = crate::workspace::AgentSettings {
            model: "fixture-fast".into(),
            reasoning_effort: "high".into(),
            speed: "deep".into(),
        };
        backend
            .set_agent_configuration(wanted.clone())
            .await
            .unwrap();
        let mut settings = backend.settings().await;
        settings.language = "en".into();
        backend.save_settings(settings).await.unwrap();
        assert_eq!(backend.settings().await.agent.model, wanted.model);
        assert_eq!(backend.settings().await.agent.reasoning_effort, "high");
        assert_eq!(backend.settings().await.agent.speed, "deep");

        let mut changed = backend.settings().await;
        if replace_manual_program {
            changed.manual_target.as_mut().unwrap().adapter_path = Some("replacement.cjs".into());
        } else {
            changed.target_id = crate::local_agent::TARGET_ID.into();
            changed.manual_target = None;
        }
        let snapshot = backend.save_settings(changed).await.unwrap();
        assert_eq!(
            snapshot["settings"]["agent"],
            json!({"model":"","reasoningEffort":"","speed":"balanced"})
        );
        let saved = WorkspaceStore::load(directory.path()).unwrap();
        assert!(saved.data.settings.agent.model.is_empty());
        assert!(saved.data.settings.agent.reasoning_effort.is_empty());
        assert_eq!(saved.data.settings.agent.speed, "balanced");
        assert_eq!(saved.data.settings.language, "en");
        backend.shutdown().await;
    }
}

#[tokio::test]
async fn legacy_local_preferences_recover_before_configuration_or_conversation_and_persist() {
    let profile: Value =
        serde_json::from_str(include_str!("../../local-agent/model-profile.json")).unwrap();
    let local_model = profile["id"].as_str().unwrap();
    for configuration_first in [true, false] {
        for (model, effort, speed) in [
            ("previous-provider-model", "high", "deep"),
            (local_model, "", "fast"),
            (local_model, "high", "balanced"),
        ] {
            let directory = tempfile::tempdir().unwrap();
            std::fs::write(directory.path().join("fixture-local-model"), b"fixture").unwrap();
            let (old, _) = connected(directory.path()).await;
            old.store
                .lock()
                .await
                .transaction(|data| {
                    // Reproduce the persisted alpha.1 state, before any new save_settings call.
                    data.settings.target_id = crate::local_agent::TARGET_ID.into();
                    data.settings.manual_target = None;
                    data.settings.agent = crate::workspace::AgentSettings {
                        model: model.into(),
                        reasoning_effort: effort.into(),
                        speed: speed.into(),
                    };
                    Ok(())
                })
                .unwrap();
            old.shutdown().await;
            drop(old);

            let backend = Backend::new(directory.path(), Arc::new(|_, _| {})).unwrap();
            backend
                .connect_agent(
                    fixture_target(),
                    directory.path().to_string_lossy().into_owned(),
                )
                .await
                .unwrap();
            // Exercise production session/configuration routing with a protocol-only
            // subprocess. This does not claim GGUF or native inference qualification.
            backend.agent.lock().await.as_mut().unwrap().target.id =
                crate::local_agent::TARGET_ID.into();
            backend.tutor_overview().await.unwrap();
            if configuration_first {
                let options = backend.agent_configuration().await.unwrap();
                assert_eq!(options["configOptions"][0]["currentValue"], local_model);
                assert_eq!(options["supported"]["model"], true);
                assert_eq!(options["supported"]["reasoningEffort"], false);
            }
            let conversation = backend.start_conversation(None, None).await.unwrap();
            backend
                .send_prompt(conversation.id, "Bonjour".into())
                .await
                .unwrap();
            assert_eq!(
                backend.snapshot().await["conversations"][0]["messages"][1]["text"],
                "Bonjour, élève."
            );
            let options = backend.agent_configuration().await.unwrap();
            assert_eq!(
                options["current"],
                json!({"model":"","reasoningEffort":"","speed":"balanced"})
            );
            let saved = WorkspaceStore::load(directory.path()).unwrap();
            assert!(saved.data.settings.agent.model.is_empty());
            assert!(saved.data.settings.agent.reasoning_effort.is_empty());
            assert_eq!(saved.data.settings.agent.speed, "balanced");
            assert_eq!(saved.data.conversations.len(), 1);

            // A new explicit invalid selection still fails; recovery is not an
            // exemption from validating the user's requested configuration.
            assert!(backend
                .set_agent_configuration(crate::workspace::AgentSettings {
                    model: "invented".into(),
                    ..Default::default()
                })
                .await
                .is_err());
            assert!(backend.settings().await.agent.model.is_empty());
            backend.shutdown().await;
        }
    }
}

#[tokio::test]
async fn local_preference_recovery_preserves_valid_choices_and_does_not_change_external_agents() {
    let directory = tempfile::tempdir().unwrap();
    std::fs::write(directory.path().join("fixture-local-model"), b"fixture").unwrap();
    let (backend, _) = connected(directory.path()).await;
    backend
        .store
        .lock()
        .await
        .transaction(|data| {
            data.settings.target_id = crate::local_agent::TARGET_ID.into();
            data.settings.agent.model = "previous-provider-model".into();
            Ok(())
        })
        .unwrap();
    // A sole advertised model is not enough to authorize migration: the
    // connected target must actually be the bundled local agent.
    assert!(backend.agent_configuration().await.is_err());
    assert_eq!(
        backend.settings().await.agent.model,
        "previous-provider-model"
    );
    backend.agent.lock().await.as_mut().unwrap().target.id = crate::local_agent::TARGET_ID.into();
    let profile: Value =
        serde_json::from_str(include_str!("../../local-agent/model-profile.json")).unwrap();
    let model = profile["id"].as_str().unwrap();
    backend
        .store
        .lock()
        .await
        .transaction(|data| {
            data.settings.agent.model = model.into();
            Ok(())
        })
        .unwrap();
    let view = backend.agent_configuration().await.unwrap();
    assert_eq!(view["current"]["model"], model);
    assert_eq!(
        WorkspaceStore::load(directory.path())
            .unwrap()
            .data
            .settings
            .agent
            .model,
        model
    );
    backend.shutdown().await;
}

#[tokio::test]
async fn full_client_flow_persists_last_chunk_and_resumes_without_replay() {
    let directory = tempfile::tempdir().unwrap();
    let (backend, _events) = connected(directory.path()).await;
    let id = intent(&backend, true).await;
    let conversation = backend.start_conversation(Some(id), None).await.unwrap();
    backend
        .send_prompt(conversation.id.clone(), "Bonjour".into())
        .await
        .unwrap();
    let snapshot = backend.snapshot().await;
    assert_eq!(
        snapshot["conversations"][0]["messages"][1]["text"],
        "Bonjour, élève."
    );
    assert_eq!(
        snapshot["conversations"][0]["messages"][1]["status"],
        "complete"
    );
    backend.shutdown().await;
    drop(backend);
    let (resumed, mut events) = connected(directory.path()).await;
    resumed
        .start_conversation(None, Some(conversation.id.clone()))
        .await
        .unwrap();
    resumed
        .send_prompt(conversation.id, "Encore".into())
        .await
        .unwrap();
    let snapshot = resumed.snapshot().await;
    assert_eq!(
        snapshot["conversations"][0]["messages"]
            .as_array()
            .unwrap()
            .len(),
        4
    );
    assert_eq!(
        snapshot["conversations"][0]["messages"][3]["text"],
        "Bonjour, élève."
    );
    while let Ok(event) = events.try_recv() {
        assert!(!event.to_string().contains("ANCIEN TEXTE"));
    }
    resumed.shutdown().await;
}

#[tokio::test]
async fn historical_unvalidated_brief_is_preserved_without_blocking_conversation() {
    let directory = tempfile::tempdir().unwrap();
    let (backend, _) = connected(directory.path()).await;
    let id = intent(&backend, false).await;
    let conversation = backend
        .start_conversation(Some(id.clone()), None)
        .await
        .unwrap();
    assert_eq!(conversation.intent_id.as_deref(), Some(id.as_str()));
    backend
        .send_prompt(conversation.id, "Commençons".into())
        .await
        .unwrap();
    assert_eq!(backend.snapshot().await["intents"][0]["validated"], false);
    backend.shutdown().await;
}

#[tokio::test]
async fn permission_requires_a_valid_human_choice_before_turn_completes() {
    let directory = tempfile::tempdir().unwrap();
    let (backend, mut events) = connected(directory.path()).await;
    let conversation = backend.start_conversation(None, None).await.unwrap();
    let pending = tokio::spawn({
        let backend = backend.clone();
        async move {
            backend
                .send_prompt(conversation.id, "PERMISSION".into())
                .await
        }
    });
    let request = tokio::time::timeout(std::time::Duration::from_secs(5), async {
        loop {
            let event = events.recv().await.unwrap();
            if event["value"]["method"] == "session/request_permission" {
                break event["value"]["id"].clone();
            }
        }
    })
    .await
    .unwrap();
    assert!(!pending.is_finished());
    assert!(backend
        .answer_permission(request.clone(), Some("invented".into()))
        .await
        .is_err());
    backend
        .answer_permission(request, Some("reject".into()))
        .await
        .unwrap();
    pending.await.unwrap().unwrap();
    assert_eq!(
        backend.snapshot().await["conversations"][0]["messages"][1]["text"],
        "Refusé."
    );
    backend.shutdown().await;
}

#[tokio::test]
async fn cancellation_is_interrupted_and_never_counted_as_a_completed_response() {
    let directory = tempfile::tempdir().unwrap();
    let (backend, mut events) = connected(directory.path()).await;
    let conversation = backend.start_conversation(None, None).await.unwrap();
    let id = conversation.id.clone();
    let pending = tokio::spawn({
        let backend = backend.clone();
        async move { backend.send_prompt(conversation.id, "WAIT".into()).await }
    });
    tokio::time::timeout(std::time::Duration::from_secs(5), async {
        loop {
            if events.recv().await.unwrap()["value"]["method"] == "session/update" {
                break;
            }
        }
    })
    .await
    .unwrap();
    backend.cancel_prompt(id).await.unwrap();
    pending.await.unwrap().unwrap();
    assert_eq!(
        backend.snapshot().await["conversations"][0]["messages"][1]["status"],
        "interrupted"
    );
    backend.shutdown().await;
}

#[tokio::test]
async fn changed_tutor_binding_cannot_replay_a_conversation_into_another_profile() {
    let directory = tempfile::tempdir().unwrap();
    let (backend, _) = connected(directory.path()).await;
    let conversation = backend.start_conversation(None, None).await.unwrap();
    {
        let mut store = backend.store.lock().await;
        store.data.conversations[0].tutor_binding = "other-account".into();
        store.save().unwrap();
    }
    assert!(backend
        .start_conversation(None, Some(conversation.id.clone()))
        .await
        .is_err());
    assert!(backend
        .send_prompt(conversation.id, "Bonjour".into())
        .await
        .is_err());
    assert!(backend.snapshot().await["conversations"][0]["messages"]
        .as_array()
        .unwrap()
        .is_empty());
    backend.shutdown().await;
}

#[tokio::test]
async fn replacing_a_manual_agent_drops_its_previous_tool_consent() {
    let directory = tempfile::tempdir().unwrap();
    let (backend, _) = connected(directory.path()).await;
    let mut settings = backend.settings().await;
    settings.permission_policy = Some(crate::permissions::PermissionPolicy {
        origin: "https://institution.example".into(),
        institution_id: "tenant-test".into(),
        agent_id: "manual".into(),
        default_decision: crate::permissions::PermissionDecision::Allow,
        tools: Default::default(),
    });
    backend.save_settings(settings.clone()).await.unwrap();
    assert!(backend.settings().await.permission_policy.is_some());
    settings.manual_target.as_mut().unwrap().adapter_path = Some("/different/program.js".into());
    backend.save_settings(settings).await.unwrap();
    assert!(backend.settings().await.permission_policy.is_none());
    backend.shutdown().await;
}

#[tokio::test]
async fn reading_and_language_changes_preserve_the_live_connections() {
    let directory = tempfile::tempdir().unwrap();
    let (backend, _) = connected(directory.path()).await;
    let mut settings = backend.settings().await;
    settings.language = "en".into();
    settings.appearance.font_size = 24;
    settings.appearance.theme = "light".into();
    backend.save_settings(settings).await.unwrap();
    assert_eq!(backend.snapshot().await["agentConnected"], true);
    assert_eq!(backend.snapshot().await["tutorStatus"]["connected"], true);
    backend.shutdown().await;
}

#[tokio::test]
async fn simultaneous_connects_reuse_the_initialized_process_without_authentication() {
    let directory = tempfile::tempdir().unwrap();
    let (backend, _) = connected(directory.path()).await;
    let generation = backend
        .agent
        .lock()
        .await
        .as_ref()
        .unwrap()
        .generation
        .clone();
    let cwd = directory.path().to_string_lossy().into_owned();
    let (a, b) = tokio::join!(
        backend.connect_agent(fixture_target(), cwd.clone()),
        backend.connect_agent(fixture_target(), cwd)
    );
    assert_eq!(a.unwrap(), b.unwrap());
    assert_eq!(
        backend.agent.lock().await.as_ref().unwrap().generation,
        generation
    );
    assert_eq!(
        backend.snapshot().await["agentStatus"]["authentication"],
        "unknown"
    );
    backend.shutdown().await;
}

#[tokio::test]
async fn explicit_disconnect_cancels_a_queued_automatic_launch() {
    let directory = tempfile::tempdir().unwrap();
    let backend = Backend::new(directory.path(), Arc::new(|_, _| {})).unwrap();
    let operation = backend.operation.lock().await;
    let worker = backend.clone();
    let cwd = directory.path().to_string_lossy().into_owned();
    let launch = tokio::spawn(async move { worker.connect_agent(fixture_target(), cwd).await });
    tokio::time::timeout(std::time::Duration::from_secs(2), async {
        while !backend.agent_requested.load(Ordering::Acquire) {
            tokio::task::yield_now().await;
        }
    })
    .await
    .unwrap();
    backend.disconnect_agent().await.unwrap();
    drop(operation);
    assert!(launch.await.unwrap().unwrap_err().contains("annulée"));
    assert_eq!(backend.snapshot().await["agentConnected"], false);
}

#[tokio::test]
async fn changing_harness_preserves_the_remote_tutor_identity() {
    let directory = tempfile::tempdir().unwrap();
    let (backend, _) = connected(directory.path()).await;
    let mut settings = backend.settings().await;
    settings.tutor.mode = "remote".into();
    settings.tutor.url = "https://institution.example".into();
    backend.save_settings(settings.clone()).await.unwrap();
    backend.tutor.lock().await.status = TutorStatus {
        connected: true,
        label: "Institution".into(),
        message: None,
    };
    let credentials = api_credentials("https://institution.example");
    backend.tutor.lock().await.credentials = Some(credentials.clone());
    settings.manual_target.as_mut().unwrap().label = "Another harness".into();
    backend.save_settings(settings.clone()).await.unwrap();
    backend
        .connect_agent(settings.manual_target.unwrap(), settings.cwd)
        .await
        .unwrap();
    assert_eq!(backend.snapshot().await["tutorStatus"]["connected"], true);
    assert_eq!(
        backend
            .tutor
            .lock()
            .await
            .credentials
            .as_ref()
            .unwrap()
            .session_id,
        credentials.session_id
    );
    backend.shutdown().await;
}

#[tokio::test]
async fn harness_authentication_is_reused_and_account_details_stay_private() {
    for (kind, expected) in [
        ("account", "authenticated"),
        ("api_key", "authenticated"),
        ("none", "none"),
    ] {
        let directory = tempfile::tempdir().unwrap();
        std::fs::write(directory.path().join("fixture-auth-status.json"), json!({"kind":kind,"label":"private-provider-token","account":{"email":"private-provider-token"}}).to_string()).unwrap();
        let (backend, mut events) = connected(directory.path()).await;
        backend.drain_agent_events(false).await.unwrap();
        let snapshot = backend.snapshot().await;
        assert_eq!(snapshot["agentStatus"]["authentication"], expected);
        assert!(!snapshot.to_string().contains("private-provider-token"));
        if kind != "none" {
            backend.authenticate_agent(String::new()).await.unwrap();
        }
        while let Ok(event) = events.try_recv() {
            assert!(!event.to_string().contains("private-provider-token"));
        }
        backend.shutdown().await;
    }
}

#[tokio::test]
async fn rpc_refusals_keep_the_harness_alive_and_do_not_invent_a_subscription_balance() {
    let directory = tempfile::tempdir().unwrap();
    let (backend, mut events) = connected(directory.path()).await;
    let generation = backend
        .agent
        .lock()
        .await
        .as_ref()
        .unwrap()
        .generation
        .clone();
    let conversation = backend.start_conversation(None, None).await.unwrap();
    for (marker, expected) in [
        ("RATE", "limitation temporaire"),
        ("CLAUDE_RATE", "limitation temporaire"),
        ("CLAUDE_BILLING", "limite d’utilisation"),
        ("QUOTA", "limite d’utilisation"),
        ("AUTH", "Authentification"),
        ("UNKNOWN", "cause fournisseur n’est pas précisée"),
    ] {
        let error = backend
            .send_prompt(conversation.id.clone(), format!("EXPERIENCE_RPC:{marker}"))
            .await
            .unwrap_err();
        assert!(error.contains(expected), "{error}");
        assert!(!error.contains("quota du fournisseur est atteint"));
        assert_eq!(
            backend.agent.lock().await.as_ref().unwrap().generation,
            generation
        );
        assert_eq!(
            backend.snapshot().await["agentStatus"]["lastFailure"]["source"],
            "acp"
        );
    }
    assert_eq!(
        backend.snapshot().await["agentStatus"]["authentication"],
        "none"
    );
    while let Ok(event) = events.try_recv() {
        assert!(!event.to_string().contains("private-provider-token"));
    }
    let captured: Value = serde_json::from_slice(
        &std::fs::read(directory.path().join("fixture-agent-sessions.json")).unwrap(),
    )
    .unwrap();
    assert_eq!(
        captured[conversation.agent_session_id.unwrap()]["promptCount"],
        6
    );
    backend.shutdown().await;
}

#[tokio::test]
async fn changing_mcp_settings_replaces_old_agent_and_restores_process_before_new_requests() {
    let directory = tempfile::tempdir().unwrap();
    let (backend, _) = connected(directory.path()).await;
    let generation = backend
        .agent
        .lock()
        .await
        .as_ref()
        .unwrap()
        .generation
        .clone();
    let mut settings = backend.store.lock().await.data.settings.clone();
    settings.tutor.url = "https://other.example".into();
    backend.save_settings(settings).await.unwrap();
    assert_eq!(backend.snapshot().await["agentConnected"], true);
    assert_ne!(
        backend.agent.lock().await.as_ref().unwrap().generation,
        generation
    );
    assert_eq!(backend.snapshot().await["tutorStatus"]["connected"], false);
    backend.shutdown().await;
}

fn block_workspace(directory: &Path) -> PathBuf {
    let path = directory.join("workspace.json");
    let backup = directory.join("workspace-before-failure.json");
    std::fs::rename(&path, &backup).unwrap();
    std::fs::create_dir(&path).unwrap();
    backup
}

fn restore_workspace(directory: &Path, backup: &Path) {
    std::fs::remove_dir(directory.join("workspace.json")).unwrap();
    std::fs::rename(backup, directory.join("workspace.json")).unwrap();
}

async fn first_chunk(events: &mut tokio::sync::mpsc::UnboundedReceiver<Value>) {
    tokio::time::timeout(std::time::Duration::from_secs(5), async {
        loop {
            if events.recv().await.unwrap()["value"]["method"] == "session/update" {
                break;
            }
        }
    })
    .await
    .unwrap();
}

#[tokio::test]
async fn disk_failure_rolls_back_new_conversations_and_unsent_messages() {
    let directory = tempfile::tempdir().unwrap();
    let (backend, _) = connected(directory.path()).await;
    let initial = serde_json::to_value(&backend.store.lock().await.data).unwrap();
    let backup = block_workspace(directory.path());
    assert!(backend.start_conversation(None, None).await.is_err());
    assert_eq!(
        serde_json::to_value(&backend.store.lock().await.data).unwrap(),
        initial
    );
    assert_eq!(
        serde_json::from_slice::<Value>(&std::fs::read(&backup).unwrap()).unwrap(),
        initial
    );
    restore_workspace(directory.path(), &backup);
    let conversation = backend.start_conversation(None, None).await.unwrap();
    let before = serde_json::to_value(&backend.store.lock().await.data).unwrap();
    let backup = block_workspace(directory.path());
    assert!(backend
        .send_prompt(conversation.id, "Cette demande ne doit pas partir".into())
        .await
        .is_err());
    assert_eq!(
        serde_json::to_value(&backend.store.lock().await.data).unwrap(),
        before
    );
    assert!(backend.prompt.lock().await.is_none());
    restore_workspace(directory.path(), &backup);
    backend.shutdown().await;
}

#[tokio::test]
async fn a_chunk_that_cannot_be_persisted_stops_the_agent_and_never_reports_success() {
    let directory = tempfile::tempdir().unwrap();
    let (backend, mut events) = connected(directory.path()).await;
    let conversation = backend.start_conversation(None, None).await.unwrap();
    let pending = tokio::spawn({
        let backend = backend.clone();
        async move { backend.send_prompt(conversation.id, "WAIT".into()).await }
    });
    first_chunk(&mut events).await;
    let generation = backend
        .agent
        .lock()
        .await
        .as_ref()
        .unwrap()
        .generation
        .clone();
    let session = backend
        .prompt
        .lock()
        .await
        .as_ref()
        .unwrap()
        .session_id
        .clone();
    let backup = block_workspace(directory.path());
    backend.on_agent_event(&generation, json!({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":session,"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Texte impossible à enregistrer"}}}})).await;
    let result = tokio::time::timeout(std::time::Duration::from_secs(10), pending)
        .await
        .unwrap()
        .unwrap();
    assert!(result.is_err());
    assert!(backend.prompt.lock().await.is_none());
    let snapshot = backend.snapshot().await;
    assert_eq!(snapshot["agentConnected"], false);
    assert_eq!(
        snapshot["conversations"][0]["messages"][1]["text"],
        "Réponse commencée."
    );
    assert_eq!(
        snapshot["conversations"][0]["messages"][1]["status"],
        "error"
    );
    restore_workspace(directory.path(), &backup);
    let restored = WorkspaceStore::load(directory.path()).unwrap();
    assert_eq!(
        restored.data.conversations[0].messages[1].text,
        "Réponse commencée."
    );
    assert_eq!(
        restored.data.conversations[0].messages[1].status,
        "interrupted"
    );
    backend.shutdown().await;
}

#[tokio::test]
async fn a_failed_final_write_is_returned_and_cleans_the_pending_turn() {
    let directory = tempfile::tempdir().unwrap();
    let (backend, mut events) = connected(directory.path()).await;
    let conversation = backend.start_conversation(None, None).await.unwrap();
    let id = conversation.id.clone();
    let pending = tokio::spawn({
        let backend = backend.clone();
        async move { backend.send_prompt(conversation.id, "WAIT".into()).await }
    });
    first_chunk(&mut events).await;
    let backup = block_workspace(directory.path());
    backend.cancel_prompt(id).await.unwrap();
    assert!(
        tokio::time::timeout(std::time::Duration::from_secs(10), pending)
            .await
            .unwrap()
            .unwrap()
            .is_err()
    );
    assert!(backend.prompt.lock().await.is_none());
    assert_eq!(
        backend.snapshot().await["conversations"][0]["messages"][1]["status"],
        "error"
    );
    restore_workspace(directory.path(), &backup);
    backend.shutdown().await;
}

#[tokio::test]
async fn a_failed_event_barrier_cannot_leave_the_turn_permanently_busy() {
    let directory = tempfile::tempdir().unwrap();
    let (backend, mut events) = connected(directory.path()).await;
    let conversation = backend.start_conversation(None, None).await.unwrap();
    let id = conversation.id.clone();
    let pending = tokio::spawn({
        let backend = backend.clone();
        async move { backend.send_prompt(conversation.id, "WAIT".into()).await }
    });
    first_chunk(&mut events).await;
    let (broken, receiver) = tokio::sync::mpsc::unbounded_channel();
    drop(receiver);
    backend.agent.lock().await.as_mut().unwrap().events = broken;
    backend.cancel_prompt(id).await.unwrap();
    assert!(
        tokio::time::timeout(std::time::Duration::from_secs(10), pending)
            .await
            .unwrap()
            .unwrap()
            .is_err()
    );
    assert!(backend.prompt.lock().await.is_none());
    assert_eq!(
        backend.snapshot().await["conversations"][0]["messages"][1]["status"],
        "interrupted"
    );
    backend
        .save_intent(LearningIntent {
            title: "Une nouvelle intention".into(),
            validated: true,
            ..Default::default()
        })
        .await
        .unwrap();
    backend.shutdown().await;
}

#[tokio::test]
async fn oversized_assistant_output_is_interrupted_at_a_utf8_boundary_and_never_completed() {
    let directory = tempfile::tempdir().unwrap();
    let (backend, mut events) = connected(directory.path()).await;
    let conversation = backend.start_conversation(None, None).await.unwrap();
    let pending = tokio::spawn({
        let backend = backend.clone();
        async move { backend.send_prompt(conversation.id, "WAIT".into()).await }
    });
    first_chunk(&mut events).await;
    let generation = backend
        .agent
        .lock()
        .await
        .as_ref()
        .unwrap()
        .generation
        .clone();
    let session = backend
        .prompt
        .lock()
        .await
        .as_ref()
        .unwrap()
        .session_id
        .clone();
    backend.on_agent_event(&generation, json!({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":session,"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"é".repeat(1024*1024+1)}}}})).await;
    assert!(
        tokio::time::timeout(std::time::Duration::from_secs(10), pending)
            .await
            .unwrap()
            .unwrap()
            .is_err()
    );
    let snapshot = backend.snapshot().await;
    let assistant = &snapshot["conversations"][0]["messages"][1];
    assert!(assistant["text"].as_str().unwrap().len() <= 2 * 1024 * 1024);
    assert_eq!(assistant["status"], "interrupted");
    assert!(backend.prompt.lock().await.is_none());
    assert_eq!(
        WorkspaceStore::load(directory.path())
            .unwrap()
            .data
            .conversations[0]
            .messages[1]
            .status,
        "interrupted"
    );
    backend.shutdown().await;
}

#[tokio::test]
async fn a_permission_for_another_session_is_not_shown_or_authorized() {
    let directory = tempfile::tempdir().unwrap();
    let (backend, mut events) = connected(directory.path()).await;
    let conversation = backend.start_conversation(None, None).await.unwrap();
    let id = conversation.id.clone();
    let pending = tokio::spawn({
        let backend = backend.clone();
        async move { backend.send_prompt(conversation.id, "WAIT".into()).await }
    });
    first_chunk(&mut events).await;
    let generation = backend
        .agent
        .lock()
        .await
        .as_ref()
        .unwrap()
        .generation
        .clone();
    backend.on_agent_event(&generation, json!({"jsonrpc":"2.0","id":"other-permission","method":"session/request_permission","params":{"sessionId":"another-session","toolCall":{"title":"Action sur une ancienne séance"},"options":[{"optionId":"allow","name":"Autoriser","kind":"allow_once"}]}})).await;
    assert!(backend
        .answer_permission(json!("other-permission"), Some("allow".into()))
        .await
        .is_err());
    while let Ok(event) = events.try_recv() {
        assert_ne!(event["value"]["id"], "other-permission");
    }
    backend.cancel_prompt(id).await.unwrap();
    pending.await.unwrap().unwrap();
    backend.shutdown().await;
}

struct ApiServer {
    origin: String,
    requests: Arc<std::sync::Mutex<Vec<String>>>,
    task: tokio::task::JoinHandle<()>,
}

impl Drop for ApiServer {
    fn drop(&mut self) {
        self.task.abort();
    }
}

async fn api_server(handler: impl Fn(&str) -> Value + Send + Sync + 'static) -> ApiServer {
    api_server_with_grant_gate(handler, None).await
}

async fn api_server_with_grant_gate(
    handler: impl Fn(&str) -> Value + Send + Sync + 'static,
    grant_gate: Option<Arc<tokio::sync::Notify>>,
) -> ApiServer {
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let origin = format!("http://{}", listener.local_addr().unwrap());
    let requests = Arc::new(std::sync::Mutex::new(Vec::new()));
    let captured = requests.clone();
    let handler = Arc::new(handler);
    let task = tokio::spawn(async move {
        loop {
            let (mut stream, _) = listener.accept().await.unwrap();
            let captured = captured.clone();
            let handler = handler.clone();
            let grant_gate = grant_gate.clone();
            tokio::spawn(async move {
                let mut bytes = Vec::new();
                let mut buffer = [0_u8; 1024];
                loop {
                    let read = stream.read(&mut buffer).await.unwrap();
                    if read == 0 {
                        return;
                    }
                    bytes.extend_from_slice(&buffer[..read]);
                    if bytes.windows(4).any(|bytes| bytes == b"\r\n\r\n") {
                        break;
                    }
                    assert!(bytes.len() < 16 * 1024);
                }
                let headers = String::from_utf8(bytes).unwrap();
                let path = headers
                    .lines()
                    .next()
                    .unwrap()
                    .split_whitespace()
                    .nth(1)
                    .unwrap();
                captured.lock().unwrap().push(path.into());
                let body = handler(path).to_string();
                if path == "/api/client/v1/identity/session" {
                    if let Some(gate) = grant_gate {
                        gate.notified().await;
                    }
                }
                let response = format!("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}", body.len());
                let _ = stream.write_all(response.as_bytes()).await;
            });
        }
    });
    ApiServer {
        origin,
        requests,
        task,
    }
}

fn api_credentials(origin: &str) -> Session {
    let keys = nostr::Keys::generate();
    Session {
        origin: origin.into(),
        access_token: "test-token".into(),
        expires_at: chrono::Utc::now().timestamp() + 3600,
        institution: native_auth::Institution {
            version: 1,
            institution_id: "tenant1".into(),
            name: "Test".into(),
            issuer: origin.into(),
            mcp_url: format!("{origin}/mcp"),
            identity: native_auth::IdentityEndpoints {
                scheme: "nostr-nip98".into(),
                challenge_url: format!("{origin}/api/client/v1/identity/challenge"),
                session_url: format!("{origin}/api/client/v1/identity/session"),
                rotate_url: format!("{origin}/api/client/v1/identity/rotate"),
                recover_url: format!("{origin}/api/client/v1/identity/recover"),
                logout_url: format!("{origin}/api/client/v1/identity/logout"),
                event_kind: 27235,
                challenge_ttl_seconds: 120,
            },
        },
        session_id: "session-test".into(),
        identity: native_auth::RemoteIdentity {
            public_key: keys.public_key().to_hex(),
            recovery_public_key: nostr::Keys::generate().public_key().to_hex(),
            key_version: 1,
        },
        principal: native_auth::Principal {
            user_id: "user1".into(),
            tenant_id: "tenant1".into(),
            membership_id: "member1".into(),
            learner_id: "learner1".into(),
            roles: vec!["learner".into()],
        },
        allowed_scopes: vec![],
        scope: "learner:read learner:write".into(),
    }
}

fn overview_page() -> Value {
    json!({"identity":{"tenantId":"tenant1","userId":"user1","membershipId":"member1","learnerId":"learner1"},"context":{"domains":[]},"formations":{"items":[],"next_after":""},"availableFormations":{"items":[],"next_after":""}})
}

fn page_parameters(path: &str) -> HashMap<String, String> {
    reqwest::Url::parse(&format!("http://localhost{path}"))
        .unwrap()
        .query_pairs()
        .map(|(name, value)| (name.into_owned(), value.into_owned()))
        .collect()
}

#[tokio::test]
async fn native_overview_collects_independent_enrollment_and_available_cursors() {
    let server = api_server(|path| {
        let params = page_parameters(path);
        assert_eq!(params.get("limit").map(String::as_str), Some("100"));
        let mut page = overview_page();
        let (mine, mine_next) = match params.get("after").map(String::as_str) {
            Some("mine-1") => ("mine1", "mine-2"),
            Some("mine-2") => ("mine2", ""),
            _ => ("mine0", "mine-1"),
        };
        let (available, available_next) =
            if params.get("available_after").map(String::as_str) == Some("available-1") {
                ("available1", "")
            } else {
                ("available0", "available-1")
            };
        page["formations"] = json!({"items":[{"cohort_id":mine}],"next_after":mine_next});
        page["availableFormations"] =
            json!({"items":[{"cohort_id":available}],"next_after":available_next});
        page
    })
    .await;
    let result = remote_overview(&api_credentials(&server.origin))
        .await
        .unwrap();
    let mine: Vec<_> = result["formations"]["items"]
        .as_array()
        .unwrap()
        .iter()
        .map(|item| item["cohort_id"].as_str().unwrap())
        .collect();
    let available: Vec<_> = result["availableFormations"]["items"]
        .as_array()
        .unwrap()
        .iter()
        .map(|item| item["cohort_id"].as_str().unwrap())
        .collect();
    assert_eq!(mine, ["mine0", "mine1", "mine2"]);
    assert_eq!(available, ["available0", "available1"]);
    assert_eq!(result["formations"]["next_after"], "");
    assert_eq!(result["availableFormations"]["next_after"], "");
    let requests = server.requests.lock().unwrap();
    assert_eq!(requests.len(), 3);
    assert!(!requests[2].contains("available_after"));
}

#[tokio::test]
async fn pagination_rejects_repeated_cursors_account_changes_and_excessive_pages() {
    for scenario in ["repeat", "identity", "limit"] {
        let server = api_server(move |path| {
            let params = page_parameters(path);
            let mut page = overview_page();
            let after = params.get("after");
            if scenario == "identity" && after.is_some() {
                page["identity"]["learnerId"] = json!("another-learner");
            }
            let next = if scenario == "repeat" {
                "repeat".to_string()
            } else {
                after
                    .and_then(|value| value.parse::<u32>().ok())
                    .unwrap_or(0)
                    .saturating_add(1)
                    .to_string()
            };
            page["formations"] = json!({"items":[{"cohort_id":next}],"next_after":next});
            page
        })
        .await;
        assert!(remote_overview(&api_credentials(&server.origin))
            .await
            .is_err());
        let count = server.requests.lock().unwrap().len();
        assert_eq!(count, if scenario == "limit" { 100 } else { 2 });
    }
}

#[tokio::test]
async fn native_login_requires_backup_and_preserves_legacy_credentials() {
    let directory = tempfile::tempdir().unwrap();
    crate::workspace::private_write(
        &directory.path().join("oauth-credentials.json"),
        b"legacy preserved",
    )
    .unwrap();
    let backend = Backend::new(directory.path(), Arc::new(|_, _| {})).unwrap();
    backend.create_identity(None).await.unwrap();
    let mut settings = backend.store.lock().await.data.settings.clone();
    settings.tutor.mode = "remote".into();
    settings.tutor.url = "https://institution.example".into();
    backend.save_settings(settings).await.unwrap();
    let error = backend
        .connect_tutor("https://institution.example".into(), false)
        .await
        .unwrap_err();
    assert!(error.contains("sauvegarde"));
    assert!(backend.tutor.lock().await.credentials.is_none());
    assert_eq!(
        std::fs::read(directory.path().join("oauth-credentials.json")).unwrap(),
        b"legacy preserved"
    );
    assert!(backend.snapshot().await["identity"].get("secret").is_none());
}

#[tokio::test]
async fn native_login_negotiates_contribution_owner_trainer_and_learner_grants() {
    use base64::{engine::general_purpose::STANDARD, Engine};
    use nostr::{Event, JsonUtil};
    use tokio::{
        io::{AsyncReadExt, AsyncWriteExt},
        net::TcpListener,
    };

    for (role, allowed, expected, learner) in [
        (
            "owner",
            vec![
                "formation:contribute",
                "formation:read",
                "formation:write",
                "progress:read",
            ],
            "formation:contribute formation:read progress:read",
            "",
        ),
        (
            "trainer",
            vec!["formation:contribute", "progress:read"],
            "formation:contribute progress:read",
            "",
        ),
        (
            "learner",
            vec!["learner:read", "learner:write", "formation:contribute"],
            "learner:read learner:write formation:contribute",
            "learner1",
        ),
    ] {
        let directory = tempfile::tempdir().unwrap();
        let backend = Backend::new(directory.path(), Arc::new(|_, _| {})).unwrap();
        let pair = KeyPair::generate();
        backend.identity.lock().await.install(&pair).unwrap();
        let public = pair.primary.public_key().to_hex();
        let recovery = pair.recovery.public_key().to_hex();
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let origin = format!("http://{}", listener.local_addr().unwrap());
        let mut credentials = api_credentials(&origin);
        credentials.identity.public_key = public.clone();
        credentials.identity.recovery_public_key = recovery.clone();
        credentials.principal.roles = vec![role.into()];
        credentials.principal.learner_id = learner.into();
        let issued = Arc::new(std::sync::Mutex::new(Vec::<String>::new()));
        let observed = issued.clone();
        let server = tokio::spawn(async move {
            loop {
                let (mut stream, _) = listener.accept().await.unwrap();
                let mut bytes = Vec::new();
                let mut buffer = [0u8; 2048];
                let (headers, body) = loop {
                    let count = stream.read(&mut buffer).await.unwrap();
                    assert!(count > 0);
                    bytes.extend_from_slice(&buffer[..count]);
                    if let Some(index) = bytes.windows(4).position(|part| part == b"\r\n\r\n") {
                        let headers = String::from_utf8(bytes[..index].to_vec()).unwrap();
                        let length = headers
                            .lines()
                            .find_map(|line| {
                                let (name, value) = line.split_once(':')?;
                                name.eq_ignore_ascii_case("content-length")
                                    .then(|| value.trim().parse::<usize>().unwrap())
                            })
                            .unwrap_or(0);
                        if bytes.len() >= index + 4 + length {
                            break (headers, bytes[index + 4..index + 4 + length].to_vec());
                        }
                    }
                };
                let path = headers.split_whitespace().nth(1).unwrap();
                let result = if path == "/.well-known/tutor-institution" {
                    serde_json::to_value(&credentials.institution).unwrap()
                } else if path == "/api/client/v1/identity/challenge" {
                    let mut request: Value = serde_json::from_slice(&body).unwrap();
                    assert_eq!(request["public_key"], public);
                    assert!(request.get("private_key").is_none());
                    request["challenge"] = json!("N".repeat(43));
                    request["key_version"] = json!(1);
                    let url = if request["action"] == "logout" {
                        &credentials.institution.identity.logout_url
                    } else {
                        &credentials.institution.identity.session_url
                    };
                    let body = request.to_string();
                    json!({"challenge":request["challenge"],"expires_at":chrono::Utc::now().timestamp()+120,"key_version":1,"url":url,"method":"POST","request_body":body,"payload_hash":format!("{:x}",Sha256::digest(body.as_bytes())),"tenant_id":"tenant1","public_key":public,"action":request["action"],"scope":request["scope"]})
                } else if path == "/api/client/v1/identity/session"
                    || path == "/api/client/v1/identity/logout"
                {
                    let authorization = headers
                        .lines()
                        .find_map(|line| {
                            let (name, value) = line.split_once(':')?;
                            name.eq_ignore_ascii_case("authorization")
                                .then(|| value.trim())
                        })
                        .unwrap()
                        .strip_prefix("Nostr ")
                        .unwrap();
                    let event = Event::from_json(STANDARD.decode(authorization).unwrap()).unwrap();
                    event.verify().unwrap();
                    assert_eq!(event.pubkey.to_hex(), public);
                    let hash = format!("{:x}", Sha256::digest(&body));
                    assert!(event
                        .tags
                        .iter()
                        .any(|tag| tag.as_slice() == ["payload", hash.as_str()]));
                    let request: Value = serde_json::from_slice(&body).unwrap();
                    if path.ends_with("logout") {
                        assert_eq!(request["scope"], "formation:contribute");
                        json!({"revoked":true,"session_id":request["session_id"]})
                    } else {
                        let scope = request["scope"].as_str().unwrap();
                        let mut scopes = observed.lock().unwrap();
                        scopes.push(scope.into());
                        json!({"access_token":format!("test-native-token-{}",scopes.len()),"token_type":"Bearer","expires_in":1800,"scope":scope,"session_id":format!("test-native-session-{}",scopes.len()),"identity":credentials.identity,"principal":credentials.principal,"allowed_scopes":allowed})
                    }
                } else if path.starts_with("/api/client/v1/overview") {
                    let mut page = overview_page();
                    page["identity"]["learnerId"] = json!(learner);
                    page["identity"]["roles"] = json!([role]);
                    page
                } else {
                    panic!("Unexpected request: {path}")
                };
                let body = result.to_string();
                stream.write_all(format!("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",body.len()).as_bytes()).await.unwrap();
            }
        });
        let mut settings = backend.settings().await;
        settings.tutor.mode = "remote".into();
        settings.tutor.activity = "auto".into();
        settings.tutor.url = origin.clone();
        settings.target_id = "manual".into();
        settings.manual_target = Some(fixture_target());
        backend.save_settings(settings).await.unwrap();
        let cwd = directory.path().join("work").to_string_lossy().into_owned();
        let agent_first = role != "trainer";
        let previous_generation = if agent_first {
            backend
                .connect_agent(fixture_target(), cwd.clone())
                .await
                .unwrap();
            let mut guard = backend.agent.lock().await;
            let agent = guard.as_mut().unwrap();
            agent
                .mcp_configurations
                .insert("old-session".into(), "old-credential".into());
            Some(agent.generation.clone())
        } else {
            None
        };
        backend.connect_tutor(origin, false).await.unwrap();
        if !agent_first {
            backend.connect_agent(fixture_target(), cwd).await.unwrap();
        }
        assert_eq!(
            backend.snapshot().await["agentConnected"],
            true,
            "connection order for {role}"
        );
        {
            let guard = backend.agent.lock().await;
            let agent = guard.as_ref().unwrap();
            assert!(agent.mcp_configurations.is_empty());
            if let Some(previous) = previous_generation {
                assert_ne!(agent.generation, previous);
            }
        }
        assert_eq!(
            issued.lock().unwrap().as_slice(),
            &["formation:contribute", expected]
        );
        let session = backend.tutor.lock().await.credentials.clone().unwrap();
        assert_eq!(session.scope, expected);
        assert_eq!(session.principal.roles, vec![role]);
        assert_eq!(session.principal.learner_id, learner);
        assert!(!session
            .scope
            .split_whitespace()
            .any(|scope| scope == "formation:write"));
        backend.shutdown().await;
        server.abort();
    }
}
#[tokio::test]
async fn backup_export_and_confirmation_broadcast_the_public_identity_state() {
    let directory = tempfile::tempdir().unwrap();
    let (events, mut receiver) = tokio::sync::mpsc::unbounded_channel();
    let backend = Backend::new(
        directory.path(),
        Arc::new(move |name, value| {
            let _ = events.send((name.to_string(), value));
        }),
    )
    .unwrap();
    let original = backend.create_identity(None).await.unwrap();
    while receiver.try_recv().is_ok() {}
    backend
        .backup_identity(
            directory.path().join("offline.json"),
            "une phrase vraiment privée".into(),
        )
        .await
        .unwrap();
    let (name, state) = receiver.try_recv().unwrap();
    assert_eq!(name, "client:state");
    assert_eq!(state["identity"]["backupExported"], true);
    assert_eq!(state["identity"]["backupConfirmed"], false);
    assert_eq!(state["identity"]["npub"], original.npub.clone().unwrap());
    backend.confirm_identity_backup().await.unwrap();
    let (name, state) = receiver.try_recv().unwrap();
    assert_eq!(name, "client:state");
    assert_eq!(state["identity"]["backupExported"], false);
    assert_eq!(state["identity"]["backupConfirmed"], true);
    assert_eq!(state["identity"]["npub"], original.npub.unwrap());
    assert_eq!(
        state["identity"]["recoveryNpub"],
        original.recovery_npub.unwrap()
    );
}

#[test]
fn staff_authoring_prompt_never_invents_a_learner() {
    let conversation = Conversation {
        active_training: None,
        id: "staff-session".into(),
        title: "Formation institution".into(),
        intent_id: None,
        agent_session_id: None,
        target_id: "target".into(),
        tutor_binding: "binding".into(),
        runtime_binding: String::new(),
        created_at: String::new(),
        updated_at: String::new(),
        messages: vec![],
    };
    let overview = json!({"identity":{"tenantId":"tenant1","userId":"owner","membershipId":"owner-member","learnerId":"","roles":["owner"]},"capabilities":{"formationAuthoring":true}});
    let prompt = pedagogical_prompt(
        None,
        &conversation,
        "Concevoir une formation",
        Some(&overview),
    );
    assert!(prompt.contains("PERSONNELLE PRIVÉE"));
    assert!(prompt.contains("Aucun formulaire d’intention"));
    assert!(prompt.contains("select_conversation_training"));
    assert!(prompt.contains("propose_training_edit"));
    assert!(prompt.contains("création institutionnelle doit être demandée explicitement"));
    assert!(prompt.contains("staff-session"));
}

#[tokio::test]
async fn authored_catalog_has_an_independent_cursor_and_no_fake_learner() {
    let server=api_server(|path| {
        let params=page_parameters(path);let mut page=overview_page();page["identity"]["learnerId"]=json!("");
        let (id,next)=match params.get("authored_after").map(String::as_str){Some("authored-1")=>("draft-1","authored-2"),Some("authored-2")=>("published-2",""),_=>("draft-0","authored-1")};
        page["authoredFormations"]=json!({"items":[{"id":id,"status":if next.is_empty(){"published"}else{"draft"}}],"next_after":next});page
    }).await;
    let result = remote_overview(&api_credentials(&server.origin))
        .await
        .unwrap();
    assert_eq!(
        result["authoredFormations"]["items"]
            .as_array()
            .unwrap()
            .len(),
        3
    );
    assert_eq!(result["identity"]["learnerId"], "");
    let requests = server.requests.lock().unwrap();
    assert_eq!(requests.len(), 3);
    assert!(requests[1].contains("authored_after=authored-1"));
    assert!(!requests[1].contains("available_after"));
}

#[tokio::test]
async fn mcp_permission_changes_reload_headers_and_never_replay_user_turns() {
    for supports_load in [true, false] {
        let directory = tempfile::tempdir().unwrap();
        let (backend, _) = connected(directory.path()).await;
        let conversation = backend.start_conversation(None, None).await.unwrap();
        if !supports_load {
            backend.agent.lock().await.as_mut().unwrap().capabilities["loadSession"] = json!(false);
        }
        // Change the configuration fingerprint as a credential reauthentication
        // does, while preserving the same pedagogical identity and transcript.
        backend
            .agent
            .lock()
            .await
            .as_mut()
            .unwrap()
            .mcp_configurations
            .insert(
                conversation.agent_session_id.clone().unwrap(),
                "prior-short-lived-token".into(),
            );
        backend
            .send_prompt(conversation.id.clone(), "Une seule requête humaine".into())
            .await
            .unwrap();
        let state = backend.snapshot().await;
        let messages = state["conversations"][0]["messages"].as_array().unwrap();
        assert_eq!(messages.len(), 2);
        assert_eq!(messages[0]["text"], "Une seule requête humaine");
        assert_eq!(messages[1]["text"], "Bonjour, élève.");
        let id = state["conversations"][0]["agentSessionId"]
            .as_str()
            .unwrap();
        assert_eq!(
            id == conversation.agent_session_id.as_deref().unwrap(),
            supports_load
        );
        let captured: Value = serde_json::from_slice(
            &std::fs::read(directory.path().join("fixture-agent-sessions.json")).unwrap(),
        )
        .unwrap();
        assert_eq!(captured[id]["mcpServers"][0]["name"], "tutor");
        assert!(!messages.iter().any(|message| message["text"]
            .as_str()
            .is_some_and(|text| text.contains("ANCIEN TEXTE"))));
        backend.shutdown().await;
    }
}

#[tokio::test]
async fn expiring_native_session_signs_again_and_installs_new_mcp_headers_before_prompt() {
    for cancel_refresh in [false, true] {
        let directory = tempfile::tempdir().unwrap();
        let (backend, _) = connected(directory.path()).await;
        let pair = KeyPair::generate();
        backend.identity.lock().await.install(&pair).unwrap();
        let server_origin = Arc::new(std::sync::Mutex::new(String::new()));
        let origin_copy = server_origin.clone();
        let public = pair.primary.public_key().to_hex();
        let recovery = pair.recovery.public_key().to_hex();
        let grant_gate = Arc::new(tokio::sync::Notify::new());
        let server=api_server_with_grant_gate(move |path| {
            let origin=origin_copy.lock().unwrap().clone();let mut session=api_credentials(&origin);session.identity.public_key=public.clone();session.identity.recovery_public_key=recovery.clone();
            match path {
                "/.well-known/tutor-institution"=>serde_json::to_value(session.institution).unwrap(),
                "/api/client/v1/identity/challenge"=>{
                    let body=json!({"challenge":"B".repeat(43),"tenant_id":"tenant1","public_key":public,"action":"session","scope":"learner:read learner:write","key_version":1}).to_string();
                    json!({"challenge":"B".repeat(43),"expires_at":chrono::Utc::now().timestamp()+120,"key_version":1,"url":format!("{origin}/api/client/v1/identity/session"),"method":"POST","payload_hash":format!("{:x}",Sha256::digest(body.as_bytes())),"request_body":body,"tenant_id":"tenant1","public_key":public,"action":"session","scope":"learner:read learner:write"})
                },
                "/api/client/v1/identity/session"=>json!({"access_token":"new-native-session-token","token_type":"Bearer","expires_in":1800,"scope":session.scope,"session_id":session.session_id,"identity":session.identity,"principal":session.principal,"allowed_scopes":[]}),
                "/mcp"=>json!({"jsonrpc":"2.0","id":"tutor-tool-catalog-0","result":{"tools":[]}}),
                _=>overview_page(),
            }
        }, cancel_refresh.then(|| grant_gate.clone())).await;
        *server_origin.lock().unwrap() = server.origin.clone();
        let mut settings = backend.store.lock().await.data.settings.clone();
        settings.tutor.mode = "remote".into();
        settings.tutor.url = server.origin.clone();
        backend.save_settings(settings).await.unwrap();
        backend
            .connect_agent(
                fixture_target(),
                directory.path().to_string_lossy().into_owned(),
            )
            .await
            .unwrap();
        let mut session = api_credentials(&server.origin);
        session.access_token = "initial-native-session-token".into();
        session.identity.public_key = pair.primary.public_key().to_hex();
        session.identity.recovery_public_key = pair.recovery.public_key().to_hex();
        backend.tutor.lock().await.credentials = Some(session);
        let conversation = backend.start_conversation(None, None).await.unwrap();
        backend
            .tutor
            .lock()
            .await
            .credentials
            .as_mut()
            .unwrap()
            .expires_at = 0;
        if cancel_refresh {
            backend
                .queue_prompt(
                    Some("Continuer une seule fois".into()),
                    Some(conversation.id.clone()),
                )
                .await
                .unwrap();
            std::fs::write(directory.path().join("fixture-rpc.jsonl"), "").unwrap();
            let worker = backend.clone();
            let id = conversation.id.clone();
            let turn = tokio::spawn(async move {
                worker
                    .send_prompt(id, "Continuer une seule fois".into())
                    .await
            });
            tokio::time::timeout(std::time::Duration::from_secs(5), async {
                while !server
                    .requests
                    .lock()
                    .unwrap()
                    .iter()
                    .any(|path| path == "/api/client/v1/identity/session")
                {
                    tokio::time::sleep(std::time::Duration::from_millis(5)).await;
                }
            })
            .await
            .unwrap();
            tokio::time::timeout(
                std::time::Duration::from_millis(500),
                backend.cancel_prompt(conversation.id.clone()),
            )
            .await
            .unwrap()
            .unwrap();
            assert_eq!(
                tokio::time::timeout(std::time::Duration::from_millis(500), turn)
                    .await
                    .unwrap()
                    .unwrap()
                    .unwrap()["stopReason"],
                "cancelled"
            );
            let snapshot =
                tokio::time::timeout(std::time::Duration::from_millis(500), backend.snapshot())
                    .await
                    .unwrap();
            assert_eq!(snapshot["pendingMessage"]["paused"], true);
            assert!(!latency_requests(directory.path())
                .iter()
                .any(|row| row["method"] == "session/prompt"));
            grant_gate.notify_one();
            tokio::time::timeout(std::time::Duration::from_secs(5), async {
                while backend
                    .tutor
                    .lock()
                    .await
                    .credentials
                    .as_ref()
                    .unwrap()
                    .access_token
                    != "new-native-session-token"
                {
                    tokio::time::sleep(std::time::Duration::from_millis(5)).await;
                }
            })
            .await
            .unwrap();
            let NativeCredential::NativeIdentity(saved) =
                crate::vault::load(directory.path(), "native-session.json")
                    .unwrap()
                    .unwrap();
            assert_eq!(saved.access_token, "new-native-session-token");
            // Only a later explicit send can use the renewed grant.
        }
        backend
            .send_prompt(conversation.id.clone(), "Continuer une seule fois".into())
            .await
            .unwrap();
        let captured: Value = serde_json::from_slice(
            &std::fs::read(directory.path().join("fixture-agent-sessions.json")).unwrap(),
        )
        .unwrap();
        assert_eq!(
            captured[conversation.agent_session_id.unwrap()]["mcpServers"][0]["headers"]
                .as_array()
                .unwrap()
                .iter()
                .find(|header| header["name"] == "Authorization")
                .unwrap()["value"],
            "Bearer new-native-session-token"
        );
        let state = backend.snapshot().await;
        assert_eq!(
            state["conversations"][0]["messages"]
                .as_array()
                .unwrap()
                .len(),
            2
        );
        assert!(!state.to_string().contains("native-session-token"));
        assert_eq!(
            server
                .requests
                .lock()
                .unwrap()
                .iter()
                .filter(|path| *path == "/api/client/v1/identity/session")
                .count(),
            1
        );
        let credential: NativeCredential =
            crate::vault::load(directory.path(), "native-session.json")
                .unwrap()
                .unwrap();
        let NativeCredential::NativeIdentity(saved) = credential;
        assert_eq!(saved.access_token, "new-native-session-token");
        backend.shutdown().await;
    }
}

#[tokio::test]
async fn native_pedagogical_turn_keeps_generation_private_until_server_commit() {
    let directory = tempfile::tempdir().unwrap();
    let rubric = json!({"criteria":[{"id":"correct","description":"The value is correct","max_score":1}],"passing_score":1,"answer_key":"5"});
    let card = json!({"version":1,"id":"generated","kind":"quiz","trainingId":"personal:go","domainId":"go","concept":"variables","question":"What is 3 + 2?","sessionId":"session-go","decisionId":"decision-1","activityType":"RECALL_EXERCISE","rubricJson":rubric.to_string(),"options":[{"id":"a","text":"3"},{"id":"b","text":"5"}],"correctOptionId":"b","explanation":"PRIVATE_ANSWER"});
    let packet = json!({"schema_version":"pedagogical-turn/v1","mode":"activity","permissions":{"read":true,"prepare_activity":true},"conversation":{"conversation_id":"unused","training_id":"personal:go","domain_id":"go","revision":"1","selection_version":1},"activity":{"activity":{"type":"RECALL_EXERCISE","concept":"variables"},"session_id":"session-go","decision_id":"decision-1","pedagogical_contract":{"competency":{"outcomes":[]}}},"attempt":{"id":"attempt-1","domain_id":"go","concept":"variables","status":"prepared","rubric":rubric}});
    let fixture = directory.path().join("fixture-pedagogy.json");
    std::fs::write(
        &fixture,
        json!({"packet":packet,"output":format!("```tutor-activity\n{card}\n```")}).to_string(),
    )
    .unwrap();
    let (backend, mut events) = connected(directory.path()).await;
    let conversation = backend.start_conversation(None, None).await.unwrap();
    std::fs::write(directory.path().join("fixture-hold-generation"), b"hold").unwrap();
    let sender = backend.clone();
    let id = conversation.id.clone();
    let send = tokio::spawn(async move {
        sender.send_prompt(id,"```tutor-study-request\n{\"version\":1,\"format\":\"quiz\",\"intent\":\"review\",\"trainingId\":\"personal:go\",\"domainId\":\"go\"}\n```".into()).await
    });
    tokio::time::timeout(std::time::Duration::from_secs(5),async {
        loop {
            tokio::select! {
                event=events.recv()=>{let event=event.unwrap();assert!(!event.to_string().contains("PRIVATE_ANSWER"));if event["value"]["type"]=="native_permission" {backend.answer_permission(event["value"]["id"].clone(),Some("allow_once".into())).await.unwrap();}}
                _=tokio::time::sleep(std::time::Duration::from_millis(5))=>{
                    let generated=backend.store.lock().await.data.conversations[0].messages.last().is_some_and(|m|m.text.contains("PRIVATE_ANSWER"));
                    if generated {break;}
                }
            }
        }
    }).await.unwrap();
    assert!(!backend
        .snapshot()
        .await
        .to_string()
        .contains("PRIVATE_ANSWER"));
    assert!(!directory
        .path()
        .join("fixture-prepared-attempt.json")
        .exists());
    std::fs::remove_file(directory.path().join("fixture-hold-generation")).unwrap();
    assert_eq!(send.await.unwrap().unwrap()["stopReason"], "end_turn");
    let state = backend.snapshot().await;
    let published = &state["conversations"][0]["messages"][1];
    let activity: Value =
        crate::learning_ui::payload(published["text"].as_str().unwrap(), "tutor-activity")
            .unwrap()
            .unwrap();
    assert_eq!(activity["attemptId"], "attempt-1");
    assert_eq!(published["status"], "complete");
    assert!(directory
        .path()
        .join("fixture-prepared-attempt.json")
        .exists());
    let sessions: Value = serde_json::from_slice(
        &std::fs::read(directory.path().join("fixture-agent-sessions.json")).unwrap(),
    )
    .unwrap();
    let session = state["conversations"][0]["agentSessionId"]
        .as_str()
        .unwrap();
    assert_eq!(sessions[session]["mcpServers"], json!([]));
    let response = json!({"version":1,"activityId":activity["id"],"kind":"quiz","trainingId":"personal:go","domainId":"go","concept":"variables","attemptId":"attempt-1","selectedOptionId":"b","responseTimeSeconds":2});
    let evaluation = json!({"version":1,"attempt_id":"attempt-1","rubric_score_json":json!({"criteria_scores":[{"id":"correct","score":1,"max_score":1,"evidence":"Selected 5"}],"total":1,"max_total":1,"confidence":0.9}).to_string(),"feedback":"SERVER_CONFIRMED_FEEDBACK"});
    std::fs::write(directory.path().join("fixture-fail-commit"), b"fail").unwrap();
    std::fs::write(
        &fixture,
        json!({"packet":packet,"output":format!("```tutor-evaluation\n{evaluation}\n```")})
            .to_string(),
    )
    .unwrap();
    let worker = backend.clone();
    let id = conversation.id.clone();
    let response_text = format!("```tutor-response\n{response}\n```");
    let retry_text = response_text.clone();
    let mut send = tokio::spawn(async move { worker.send_prompt(id, response_text).await });
    tokio::time::timeout(std::time::Duration::from_secs(5),async{
        loop {tokio::select!{
            result=&mut send=>{assert!(result.unwrap().is_err());break;}
            event=events.recv()=>{let event=event.unwrap();if event["value"]["type"]=="native_permission"{backend.answer_permission(event["value"]["id"].clone(),Some("allow_once".into())).await.unwrap();}}
        }}
    }).await.unwrap();
    let interrupted = backend.snapshot().await;
    assert_eq!(
        interrupted["conversations"][0]["messages"][3]["canRetryCommit"],
        true
    );
    assert!(!interrupted
        .to_string()
        .contains("SERVER_CONFIRMED_FEEDBACK"));
    let before = latency_requests(directory.path())
        .iter()
        .filter(|r| r["method"] == "session/prompt")
        .count();
    std::fs::remove_file(directory.path().join("fixture-fail-commit")).unwrap();
    let worker = backend.clone();
    let id = conversation.id.clone();
    let mut retry = tokio::spawn(async move { worker.send_prompt(id, retry_text).await });
    tokio::time::timeout(std::time::Duration::from_secs(5),async{loop{tokio::select!{
        result=&mut retry=>{assert_eq!(result.unwrap().unwrap()["recoveredCommit"],true);break;}
        event=events.recv()=>{let event=event.unwrap();if event["value"]["type"]=="native_permission"{backend.answer_permission(event["value"]["id"].clone(),Some("allow_once".into())).await.unwrap();}}
    }}}).await.unwrap();
    assert_eq!(
        latency_requests(directory.path())
            .iter()
            .filter(|r| r["method"] == "session/prompt")
            .count(),
        before
    );
    let state = backend.snapshot().await;
    let feedback = &state["conversations"][0]["messages"][3];
    assert_eq!(feedback["text"], "SERVER_CONFIRMED_FEEDBACK");
    assert_eq!(feedback["learningDelivery"]["recorded"], false);
    let calls: Vec<Value> =
        std::fs::read_to_string(directory.path().join("fixture-pedagogy-log.jsonl"))
            .unwrap()
            .lines()
            .map(|line| serde_json::from_str(line).unwrap())
            .collect();
    assert_eq!(
        calls
            .iter()
            .map(|v| v["name"].as_str().unwrap())
            .collect::<Vec<_>>(),
        vec![
            "prepare_pedagogical_turn",
            "prepare_assessment_attempt",
            "prepare_pedagogical_turn",
            "submit_assessment_attempt",
            "record_interaction",
            "record_interaction"
        ]
    );
    assert_eq!(calls[4]["arguments"], calls[5]["arguments"]);
    backend
        .acknowledge_learning_delivery(
            conversation.id.clone(),
            feedback["id"].as_str().unwrap().into(),
        )
        .await
        .unwrap();
    backend
        .acknowledge_learning_delivery(conversation.id, feedback["id"].as_str().unwrap().into())
        .await
        .unwrap();
    let calls =
        std::fs::read_to_string(directory.path().join("fixture-pedagogy-log.jsonl")).unwrap();
    assert_eq!(calls.matches("\"record_learning_event\"").count(), 1);
    backend.shutdown().await;
}

#[tokio::test]
async fn background_memory_requires_consent_and_yields_to_a_foreground_turn() {
    let directory = tempfile::tempdir().unwrap();
    std::fs::write(directory.path().join("fixture-pedagogy.json"),json!({"packet":{"conversation":{}},"batch":{"status":"claimed","claim_token":"synthetic-maintenance-token","snapshots":[],"sessions":[]},"output":"PRIVATE_MAINTENANCE_SECRET"}).to_string()).unwrap();
    let (backend, mut events) = connected(directory.path()).await;
    backend.start_conversation(None, None).await.unwrap();
    backend.list_mcp_tools().await.unwrap();
    assert!(
        !backend.memory_maintenance_batch().await.unwrap(),
        "ask must not start provider work"
    );
    let (origin, institution_id, agent_id) = backend.native_policy_binding().await.unwrap();
    backend
        .store
        .lock()
        .await
        .transaction(|data| {
            data.settings.permission_policy = Some(crate::permissions::PermissionPolicy {
                origin,
                institution_id,
                agent_id,
                default_decision: crate::permissions::PermissionDecision::Ask,
                tools: [
                    (
                        "claim_memory_maintenance".into(),
                        crate::permissions::PermissionDecision::Allow,
                    ),
                    (
                        "commit_memory_maintenance".into(),
                        crate::permissions::PermissionDecision::Allow,
                    ),
                ]
                .into_iter()
                .collect(),
            });
            Ok(())
        })
        .unwrap();
    std::fs::write(directory.path().join("fixture-hold-generation"), b"hold").unwrap();
    let worker = backend.clone();
    let work = tokio::spawn(async move { worker.memory_maintenance_batch().await });
    tokio::time::timeout(std::time::Duration::from_secs(5), async {
        loop {
            if latency_requests(directory.path())
                .iter()
                .any(|r| r["method"] == "session/prompt")
            {
                break;
            }
            tokio::time::sleep(std::time::Duration::from_millis(5)).await;
        }
    })
    .await
    .unwrap();
    let foreground = backend.begin_preparation(None).unwrap();
    assert!(
        !tokio::time::timeout(std::time::Duration::from_secs(1), work)
            .await
            .unwrap()
            .unwrap()
            .unwrap()
    );
    assert!(!directory
        .path()
        .join("fixture-maintenance-committed")
        .exists());
    std::fs::remove_file(directory.path().join("fixture-hold-generation")).unwrap();
    backend.drain_agent_events(false).await.unwrap();
    while let Ok(event) = events.try_recv() {
        assert!(!event.to_string().contains("PRIVATE_MAINTENANCE_SECRET"));
    }
    drop(foreground);
    backend.shutdown().await;
}

#[tokio::test]
async fn learning_prompt_uses_the_fresh_server_context_instead_of_the_cached_selection() {
    let directory = tempfile::tempdir().unwrap();
    let (backend, _) = connected(directory.path()).await;
    let conversation = backend.start_conversation(None, None).await.unwrap();
    backend.store.lock().await.transaction(|data| {
        data.conversations[0].active_training = Some(json!({"training_id":"personal:stale-python","domain_id":"stale-python","revision":"1"}));
        Ok(())
    }).unwrap();
    backend
        .send_prompt(conversation.id.clone(), "Continuons".into())
        .await
        .unwrap();
    let sessions: Value = serde_json::from_slice(
        &std::fs::read(directory.path().join("fixture-agent-sessions.json")).unwrap(),
    )
    .unwrap();
    let prompt = sessions[conversation.agent_session_id.as_deref().unwrap()]["lastPrompt"]
        .as_str()
        .unwrap();
    // The fixture's fresh context is {items:[]}; stale persisted selection must
    // never be promoted into the agent's confirmed formation instructions.
    assert!(prompt.contains("FORMATION CONFIRMÉE : {\"items\":[]}"));
    assert!(!prompt.contains("stale-python"));
    backend.shutdown().await;
}

#[tokio::test]
async fn learning_planning_preferences_preserve_live_agent_and_tutor_connections() {
    let directory = tempfile::tempdir().unwrap();
    let (backend, _) = connected(directory.path()).await;
    let mut settings = backend.settings().await;
    settings.review_plans.insert(
        "profile|tutor|training".into(),
        crate::learning_ui::ReviewPlan {
            enabled: true,
            format: crate::learning_ui::LearningFormat::Quiz,
        },
    );
    let snapshot = backend.save_settings(settings).await.unwrap();
    assert_eq!(snapshot["agentConnected"], true);
    assert_eq!(snapshot["tutorStatus"]["connected"], true);
    backend.shutdown().await;
}

#[tokio::test]
async fn learning_requests_with_wrong_confirmed_context_never_reach_the_agent_or_consume_outbox() {
    let directory = tempfile::tempdir().unwrap();
    let (backend, _) = connected(directory.path()).await;
    let conversation = backend.start_conversation(None, None).await.unwrap();
    // This fixture's tutor returns no selected training. A client must not
    // route a structured request to a provider before selection is confirmed.
    let message = "```tutor-study-request\n{\"version\":1,\"format\":\"quiz\",\"intent\":\"review\",\"trainingId\":\"personal:go\",\"domainId\":\"go\"}\n```".to_string();
    backend
        .queue_prompt(Some(message.clone()), Some(conversation.id.clone()))
        .await
        .unwrap();
    let error = backend
        .send_prompt(conversation.id.clone(), message.clone())
        .await
        .unwrap_err();
    assert!(error.contains("formation demandée"));
    let snapshot = backend.snapshot().await;
    assert_eq!(snapshot["pendingMessage"]["text"], message);
    assert!(snapshot["conversations"][0]["messages"]
        .as_array()
        .unwrap()
        .is_empty());
    let sessions: Value = serde_json::from_slice(
        &std::fs::read(directory.path().join("fixture-agent-sessions.json")).unwrap(),
    )
    .unwrap();
    assert!(sessions
        .as_object()
        .unwrap()
        .values()
        .all(|session| session.get("lastPrompt").is_none()));
    backend.shutdown().await;
}

#[tokio::test]
async fn learning_queue_rejects_forged_response_without_overwriting_an_existing_message() {
    let directory = tempfile::tempdir().unwrap();
    let backend = Backend::new(directory.path(), Arc::new(|_, _| {})).unwrap();
    backend
        .queue_prompt(Some("Question conservée".into()), None)
        .await
        .unwrap();
    let forged = "```tutor-response\n{\"version\":1,\"kind\":\"flashcard\",\"activityId\":\"made-up\",\"trainingId\":\"personal:go\",\"domainId\":\"go\",\"concept\":\"Variables\",\"rating\":\"easy\",\"responseTimeSeconds\":1}\n```";
    assert!(backend
        .queue_prompt(Some(forged.into()), Some("unknown".into()))
        .await
        .is_err());
    assert_eq!(
        backend.snapshot().await["pendingMessage"]["text"],
        "Question conservée"
    );
    let mut invalid = backend.settings().await;
    invalid.review_plans.insert(
        String::new(),
        crate::learning_ui::ReviewPlan {
            enabled: true,
            format: crate::learning_ui::LearningFormat::Quiz,
        },
    );
    assert!(backend.save_settings(invalid).await.is_err());
    assert_eq!(
        backend.snapshot().await["settings"]["reviewPlans"],
        json!({})
    );
}

#[tokio::test]
async fn learning_withdrawn_response_is_kept_locally_and_cannot_be_answered_again() {
    let directory = tempfile::tempdir().unwrap();
    let backend = Backend::new(directory.path(), Arc::new(|_, _| {})).unwrap();
    let context = json!({"training_id":"personal:go","domain_id":"go","revision":"2"});
    let card = json!({"version":1,"id":"function-1","kind":"flashcard","trainingId":"personal:go","domainId":"go","concept":"Functions","question":"Quel mot-clé déclare une fonction ?","answer":"func"});
    let response = json!({"version":1,"activityId":"function-1","kind":"flashcard","trainingId":"personal:go","domainId":"go","concept":"Functions","rating":"hard","responseTimeSeconds":4});
    let encoded = format!("```tutor-response\n{response}\n```");
    let conversation:Conversation = serde_json::from_value(json!({"id":"go-chat","title":"Go","intentId":null,"agentSessionId":null,"targetId":"manual","tutorBinding":"binding","createdAt":"now","updatedAt":"now","activeTraining":context,"messages":[{"id":"card-1","role":"assistant","text":format!("```tutor-activity\n{card}\n```"),"createdAt":"now","status":"complete","trainingContext":context}]})).unwrap();
    backend
        .store
        .lock()
        .await
        .transaction(|data| {
            data.conversations.push(conversation);
            Ok(())
        })
        .unwrap();
    backend
        .queue_prompt(Some(encoded.clone()), Some("go-chat".into()))
        .await
        .unwrap();
    assert!(backend
        .queue_prompt(Some("Another question".into()), Some("go-chat".into()))
        .await
        .is_err());
    let after = backend.queue_prompt(None, None).await.unwrap();
    assert!(after["pendingMessage"].is_null());
    let messages = after["conversations"][0]["messages"].as_array().unwrap();
    assert_eq!(messages.len(), 2);
    assert!(messages[1]["text"]
        .as_str()
        .unwrap()
        .starts_with("Réponse conservée sur cet appareil, sans envoi au tuteur."));
    assert_eq!(messages[1]["trainingContext"], context);
    assert_eq!(messages[1]["status"], "complete");
    backend.queue_prompt(None, None).await.unwrap();
    drop(backend);
    let reopened = Backend::new(directory.path(), Arc::new(|_, _| {})).unwrap();
    assert!(reopened
        .queue_prompt(Some(encoded), Some("go-chat".into()))
        .await
        .unwrap_err()
        .contains("déjà"));
    assert_eq!(
        reopened.snapshot().await["conversations"][0]["messages"]
            .as_array()
            .unwrap()
            .len(),
        2
    );
}

#[tokio::test]
async fn queued_message_survives_restart_and_is_consumed_with_one_user_turn() {
    let directory = tempfile::tempdir().unwrap();
    let (backend, _) = connected(directory.path()).await;
    backend
        .queue_prompt(Some("Je veux apprendre le Go".into()), None)
        .await
        .unwrap();
    backend.shutdown().await;
    drop(backend);
    let (resumed, _) = connected(directory.path()).await;
    assert_eq!(
        resumed.snapshot().await["pendingMessage"]["text"],
        "Je veux apprendre le Go"
    );
    let conversation = resumed.start_conversation(None, None).await.unwrap();
    resumed
        .send_prompt(conversation.id.clone(), "Je veux apprendre le Go".into())
        .await
        .unwrap();
    let snapshot = resumed.snapshot().await;
    assert!(snapshot["pendingMessage"].is_null());
    assert_eq!(
        snapshot["conversations"][0]["messages"]
            .as_array()
            .unwrap()
            .len(),
        2
    );
    assert_eq!(
        snapshot["conversations"][0]["title"],
        "Je veux apprendre le Go"
    );
    assert!(snapshot["conversations"][0]["messages"][0]["trainingContext"].is_object());
    resumed.select_conversation(None).await.unwrap();
    assert!(resumed.snapshot().await["activeConversationId"].is_null());
    resumed
        .select_conversation(Some(conversation.id.clone()))
        .await
        .unwrap();
    assert_eq!(
        resumed.snapshot().await["activeConversationId"],
        conversation.id
    );
    resumed.shutdown().await;
}

#[path = "experience_acceptance_tests.rs"]
mod experience_acceptance;

#[tokio::test]
async fn workspace_title_metadata_is_persisted_without_an_extra_prompt() {
    for (marker, expected) in [
        ("WORKSPACE_TITLE_BLOCK", "Mon profil d’apprenant"),
        ("WORKSPACE_TITLE_EVENT", "Comprendre les boucles"),
    ] {
        let directory = tempfile::tempdir().unwrap();
        let (backend, _) = connected(directory.path()).await;
        let conversation = backend.start_conversation(None, None).await.unwrap();
        backend
            .send_prompt(conversation.id.clone(), marker.into())
            .await
            .unwrap();
        let snapshot = backend.snapshot().await;
        assert_eq!(snapshot["conversations"][0]["title"], expected);
        assert_eq!(
            snapshot["conversations"][0]["messages"]
                .as_array()
                .unwrap()
                .len(),
            2
        );
        assert_eq!(
            WorkspaceStore::load(directory.path())
                .unwrap()
                .data
                .conversations[0]
                .title,
            expected
        );
        backend.shutdown().await;
    }
}
