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
async fn changing_settings_disconnects_old_agent_before_new_requests() {
    let directory = tempfile::tempdir().unwrap();
    let (backend, _) = connected(directory.path()).await;
    let mut settings = backend.store.lock().await.data.settings.clone();
    settings.tutor.url = "https://other.example".into();
    backend.save_settings(settings).await.unwrap();
    assert_eq!(backend.snapshot().await["agentConnected"], false);
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
        backend.save_settings(settings).await.unwrap();
        backend.connect_tutor(origin, false).await.unwrap();
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
    let directory = tempfile::tempdir().unwrap();
    let (backend, _) = connected(directory.path()).await;
    let pair = KeyPair::generate();
    backend.identity.lock().await.install(&pair).unwrap();
    let server_origin = Arc::new(std::sync::Mutex::new(String::new()));
    let origin_copy = server_origin.clone();
    let public = pair.primary.public_key().to_hex();
    let recovery = pair.recovery.public_key().to_hex();
    let server=api_server(move |path| {
        let origin=origin_copy.lock().unwrap().clone();let mut session=api_credentials(&origin);session.identity.public_key=public.clone();session.identity.recovery_public_key=recovery.clone();
        match path {
            "/.well-known/tutor-institution"=>serde_json::to_value(session.institution).unwrap(),
            "/api/client/v1/identity/challenge"=>{
                let body=json!({"challenge":"B".repeat(43),"tenant_id":"tenant1","public_key":public,"action":"session","scope":"learner:read learner:write","key_version":1}).to_string();
                json!({"challenge":"B".repeat(43),"expires_at":chrono::Utc::now().timestamp()+120,"key_version":1,"url":format!("{origin}/api/client/v1/identity/session"),"method":"POST","payload_hash":format!("{:x}",Sha256::digest(body.as_bytes())),"request_body":body,"tenant_id":"tenant1","public_key":public,"action":"session","scope":"learner:read learner:write"})
            },
            "/api/client/v1/identity/session"=>json!({"access_token":"new-native-session-token","token_type":"Bearer","expires_in":1800,"scope":session.scope,"session_id":session.session_id,"identity":session.identity,"principal":session.principal,"allowed_scopes":[]}),
            _=>overview_page(),
        }
    }).await;
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
    let credential: NativeCredential = crate::vault::load(directory.path(), "native-session.json")
        .unwrap()
        .unwrap();
    let NativeCredential::NativeIdentity(saved) = credential;
    assert_eq!(saved.access_token, "new-native-session-token");
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
