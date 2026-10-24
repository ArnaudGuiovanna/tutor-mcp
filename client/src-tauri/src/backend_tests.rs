// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

use super::*;

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
            url: String::new(),
            command: target.adapter_path.clone().unwrap(),
            args: vec!["--local".into()],
        },
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
async fn unvalidated_brief_and_stale_revision_are_rejected() {
    let directory = tempfile::tempdir().unwrap();
    let (backend, _) = connected(directory.path()).await;
    let id = intent(&backend, false).await;
    assert!(backend.start_conversation(Some(id), None).await.is_err());
    assert!(backend.snapshot().await["conversations"]
        .as_array()
        .unwrap()
        .is_empty());
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

fn api_credentials(origin: &str) -> OAuthCredentials {
    OAuthCredentials {
        origin: origin.into(),
        client_id: "test-client".into(),
        access_token: "test-token".into(),
        refresh_token: "test-refresh".into(),
        expires_at: chrono::Utc::now().timestamp() + 3600,
        token_endpoint: format!("{origin}/token"),
        resource: format!("{origin}/mcp"),
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
async fn logout_invalidates_a_callback_before_it_can_replace_the_vault() {
    let directory = tempfile::tempdir().unwrap();
    let server = api_server(|_| overview_page()).await;
    let (backend, _) = connected(directory.path()).await;
    let mut settings = backend.store.lock().await.data.settings.clone();
    settings.tutor.mode = "remote".into();
    settings.tutor.url = server.origin.clone();
    backend.save_settings(settings).await.unwrap();
    backend.tutor.lock().await.login_generation = "old-browser-flow".into();
    let operation = backend.operation.lock().await;
    let callback = tokio::spawn({
        let backend = backend.clone();
        let credentials = api_credentials(&server.origin);
        async move {
            backend
                .complete_tutor_login("old-browser-flow", Ok(credentials))
                .await
        }
    });
    tokio::task::yield_now().await;
    let logout = tokio::spawn({
        let backend = backend.clone();
        async move { backend.disconnect_tutor().await }
    });
    tokio::time::timeout(std::time::Duration::from_secs(2), async {
        loop {
            if backend.tutor.lock().await.login_generation != "old-browser-flow" {
                break;
            }
            tokio::time::sleep(std::time::Duration::from_millis(5)).await;
        }
    })
    .await
    .unwrap();
    drop(operation);
    callback.await.unwrap();
    logout.await.unwrap().unwrap();
    assert!(backend.tutor.lock().await.credentials.is_none());
    assert!(crate::vault::load_credentials(directory.path())
        .unwrap()
        .is_none());
    assert!(server.requests.lock().unwrap().is_empty());
    backend.shutdown().await;
}

#[tokio::test]
async fn changing_institution_invalidates_success_and_error_from_an_old_login() {
    let directory = tempfile::tempdir().unwrap();
    let (backend, _) = connected(directory.path()).await;
    let mut settings = backend.store.lock().await.data.settings.clone();
    settings.tutor.mode = "remote".into();
    settings.tutor.url = "https://first.example".into();
    backend.save_settings(settings.clone()).await.unwrap();
    backend.tutor.lock().await.login_generation = "first-login".into();
    settings.tutor.url = "https://second.example".into();
    backend.save_settings(settings).await.unwrap();
    backend
        .complete_tutor_login("first-login", Ok(api_credentials("https://first.example")))
        .await;
    backend
        .complete_tutor_login(
            "first-login",
            Err("Ancienne erreur qui ne doit pas remplacer le nouvel état".into()),
        )
        .await;
    assert!(backend.tutor.lock().await.credentials.is_none());
    assert!(backend.tutor.lock().await.status.message.is_none());
    assert!(crate::vault::load_credentials(directory.path())
        .unwrap()
        .is_none());
    backend.shutdown().await;
}
