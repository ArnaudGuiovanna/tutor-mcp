// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! End-to-end consent/identity acceptance through the production Backend,
//! an actual ACP child process and an HTTP endpoint. No model or user key.
use super::*;
use crate::permissions::{PermissionDecision, PermissionPolicy};
use std::collections::BTreeMap;
use std::time::Duration;
use tokio::io::{AsyncReadExt, AsyncWriteExt};

async fn typed_failure_backend(
    directory: &Path,
    language: &str,
) -> (Arc<Backend>, tokio::sync::mpsc::UnboundedReceiver<Value>) {
    let (events, receiver) = tokio::sync::mpsc::unbounded_channel();
    let backend = Backend::new(
        directory,
        Arc::new(move |name, value| {
            let _ = events.send(json!({"name":name,"value":value}));
        }),
    )
    .unwrap();
    let mut target = fixture_target();
    let local_command = target.adapter_path.clone().unwrap();
    let adapter_directory = directory.join("codex-acp");
    std::fs::create_dir(&adapter_directory).unwrap();
    let adapter = adapter_directory.join("fixture.cjs");
    std::fs::copy(&local_command, &adapter).unwrap();
    target.adapter_path = Some(adapter.to_string_lossy().into_owned());
    let settings = Settings {
        language: language.into(),
        target_id: target.id.clone(),
        manual_target: Some(target.clone()),
        cwd: directory.to_string_lossy().into_owned(),
        tutor: crate::workspace::TutorSettings {
            mode: "local".into(),
            activity: "auto".into(),
            url: String::new(),
            command: local_command,
            args: vec!["--local".into()],
        },
        ..Default::default()
    };
    backend.save_settings(settings).await.unwrap();
    backend
        .connect_agent(target, directory.to_string_lossy().into_owned())
        .await
        .unwrap();
    backend.tutor_overview().await.unwrap();
    let initialize: Value = serde_json::from_slice(
        &std::fs::read(directory.join("fixture-agent-initialize.json")).unwrap(),
    )
    .unwrap();
    assert_eq!(
        initialize["clientCapabilities"]["_meta"],
        json!({"jetbrains":{"air":{"version":1,"capabilities":["sessionFailure"]}}})
    );
    (backend, receiver)
}

#[tokio::test]
async fn typed_terminal_provider_failures_are_errors_in_both_languages_without_raw_details() {
    for (language, marker, expected) in [
        (
            "fr",
            "ERROR",
            "Le fournisseur limite temporairement les requêtes. Réessayez plus tard.",
        ),
        (
            "en",
            "RESULT_ERROR",
            "The provider is temporarily limiting requests. Try again later.",
        ),
        (
            "fr",
            "CLEARED_ERROR",
            "Le fournisseur limite temporairement les requêtes. Réessayez plus tard.",
        ),
        (
            "en",
            "MALFORMED",
            "The provider could not finish the response. Try again later.",
        ),
    ] {
        let directory = tempfile::tempdir().unwrap();
        let (backend, mut events) = typed_failure_backend(directory.path(), language).await;
        let conversation = backend.start_conversation(None, None).await.unwrap();
        let error = backend
            .send_prompt(conversation.id, format!("EXPERIENCE_TYPED:{marker}"))
            .await
            .unwrap_err();
        assert_eq!(error, expected, "typed case {marker}");
        let snapshot = backend.snapshot().await;
        let assistant = &snapshot["conversations"][0]["messages"][1];
        assert_eq!(
            assistant["status"], "error",
            "end_turn is not success after a provider failure"
        );
        assert_eq!(
            assistant["text"],
            if marker == "RESULT_ERROR" {
                "Réponse partielle."
            } else {
                ""
            }
        );
        assert_eq!(snapshot["agentConnected"], false);
        let mut statuses = 0;
        while let Ok(event) = events.try_recv() {
            assert!(
                !event.to_string().contains("private-provider-token"),
                "raw provider details reached UI"
            );
            if event["value"]["type"] == "provider_status" {
                statuses += 1;
                assert_eq!(event["value"]["severity"], "error");
                assert_eq!(event["value"]["message"], expected);
            }
        }
        assert_eq!(statuses, 1, "a terminal error is emitted once");
        let saved = WorkspaceStore::load(directory.path()).unwrap();
        assert_eq!(saved.data.conversations[0].messages[1].status, "error");
        assert!(!serde_json::to_string(&saved.data)
            .unwrap()
            .contains("private-provider-token"));
        backend.shutdown().await;
    }
}

#[tokio::test]
async fn typed_retry_warnings_recover_and_stale_session_failures_are_ignored() {
    for (language, expected) in [
        ("fr", "Le fournisseur réessaie la requête…"),
        ("en", "The provider is retrying the request…"),
    ] {
        let directory = tempfile::tempdir().unwrap();
        let (backend, mut events) = typed_failure_backend(directory.path(), language).await;
        for marker in ["WARNING", "STALE"] {
            let conversation = backend.start_conversation(None, None).await.unwrap();
            backend
                .send_prompt(
                    conversation.id.clone(),
                    format!("EXPERIENCE_TYPED:{marker}"),
                )
                .await
                .unwrap();
            let snapshot = backend.snapshot().await;
            let assistant = &snapshot["conversations"]
                .as_array()
                .unwrap()
                .iter()
                .find(|value| value["id"] == conversation.id)
                .unwrap()["messages"][1];
            assert_eq!(assistant["status"], "complete");
            assert_eq!(assistant["text"], "Réponse récupérée.");
            assert_eq!(snapshot["agentConnected"], true);
            let mut warnings = 0;
            while let Ok(event) = events.try_recv() {
                assert!(!event.to_string().contains("private-provider-token"));
                if event["value"]["type"] == "provider_status" {
                    warnings += 1;
                    assert_eq!(event["value"]["severity"], "warning");
                    assert_eq!(event["value"]["message"], expected);
                }
            }
            assert_eq!(warnings, usize::from(marker == "WARNING"));
        }
        backend.shutdown().await;
    }
}

#[tokio::test]
async fn other_adapters_do_not_receive_codex_rendering_capabilities() {
    let directory = tempfile::tempdir().unwrap();
    let (backend, _) = connected(directory.path()).await;
    let initialize: Value = serde_json::from_slice(
        &std::fs::read(directory.path().join("fixture-agent-initialize.json")).unwrap(),
    )
    .unwrap();
    assert!(initialize["clientCapabilities"].get("_meta").is_none());
    backend.shutdown().await;
}

#[derive(Clone)]
struct Request {
    path: String,
    headers: BTreeMap<String, String>,
    body: Vec<u8>,
}
impl Request {
    fn json(&self) -> Value {
        serde_json::from_slice(&self.body).unwrap_or(Value::Null)
    }
}
struct Server {
    origin: String,
    requests: Arc<std::sync::Mutex<Vec<Request>>>,
    task: tokio::task::JoinHandle<()>,
}
impl Drop for Server {
    fn drop(&mut self) {
        self.task.abort();
    }
}
async fn server(mcp: impl Fn(&Request) -> (String, String) + Send + Sync + 'static) -> Server {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let origin = format!("http://{}", listener.local_addr().unwrap());
    let requests = Arc::new(std::sync::Mutex::new(Vec::new()));
    let captured = requests.clone();
    let mcp = Arc::new(mcp);
    let task = tokio::spawn(async move {
        loop {
            let (mut stream, _) = listener.accept().await.unwrap();
            let captured = captured.clone();
            let mcp = mcp.clone();
            tokio::spawn(async move {
                let mut bytes = Vec::new();
                let mut buffer = [0u8; 2048];
                let request = loop {
                    let count = stream.read(&mut buffer).await.unwrap();
                    if count == 0 {
                        return;
                    }
                    bytes.extend_from_slice(&buffer[..count]);
                    assert!(bytes.len() < 1024 * 1024);
                    if let Some(index) = bytes.windows(4).position(|part| part == b"\r\n\r\n") {
                        let text = String::from_utf8(bytes[..index].to_vec()).unwrap();
                        let headers: BTreeMap<_, _> = text
                            .lines()
                            .skip(1)
                            .filter_map(|line| {
                                let (name, value) = line.split_once(':')?;
                                Some((name.to_ascii_lowercase(), value.trim().to_string()))
                            })
                            .collect();
                        let length = headers
                            .get("content-length")
                            .map(|v| v.parse::<usize>().unwrap())
                            .unwrap_or(0);
                        if bytes.len() >= index + 4 + length {
                            break Request {
                                path: text.split_whitespace().nth(1).unwrap().into(),
                                headers,
                                body: bytes[index + 4..index + 4 + length].to_vec(),
                            };
                        }
                    }
                };
                captured.lock().unwrap().push(request.clone());
                let (kind, body) = if request.path == "/mcp" {
                    mcp(&request)
                } else {
                    let value = if request.path.starts_with("/api/client/v1/overview") {
                        let mut page = overview_page();
                        if request.headers.get("authorization").map(String::as_str)
                            == Some("Bearer other-account")
                        {
                            page["identity"]["userId"] = json!("user2");
                            page["identity"]["membershipId"] = json!("member2");
                            page["identity"]["learnerId"] = json!("learner2");
                        }
                        page
                    } else if request.path == "/api/client/v1/training/get_conversation_context" {
                        json!({"conversation_id":request.json()["conversation_id"],"training_id":"","domain_id":"","selection_version":0})
                    } else if request.path == "/api/client/v1/institution/actions" {
                        json!({"received":true})
                    } else {
                        panic!("Unexpected acceptance endpoint: {}", request.path)
                    };
                    ("application/json".into(), value.to_string())
                };
                let response = format!("HTTP/1.1 200 OK\r\nContent-Type: {kind}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}", body.len());
                stream.write_all(response.as_bytes()).await.unwrap();
            });
        }
    });
    Server {
        origin,
        requests,
        task,
    }
}
fn rpc(request: &Request, result: Value) -> (String, String) {
    (
        "application/json".into(),
        json!({"jsonrpc":"2.0","id":request.json()["id"],"result":result}).to_string(),
    )
}
fn tools() -> Value {
    json!({"tools":[{"name":"get_next_activity","description":"Actual endpoint catalogue","annotations":{"readOnlyHint":true}}]})
}
fn policy(origin: &str, decision: PermissionDecision) -> PermissionPolicy {
    PermissionPolicy {
        origin: origin.into(),
        institution_id: "tenant1".into(),
        agent_id: "manual".into(),
        default_decision: decision,
        tools: Default::default(),
    }
}
async fn remote_backend(
    directory: &Path,
    server: &Server,
    preference: Option<PermissionPolicy>,
    other_account: bool,
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
    let mut settings = backend.settings().await;
    settings.target_id = target.id.clone();
    settings.manual_target = Some(target.clone());
    settings.cwd = directory.to_string_lossy().into_owned();
    settings.tutor.mode = "remote".into();
    settings.tutor.url = server.origin.clone();
    backend.save_settings(settings).await.unwrap();
    let mut settings = backend.settings().await;
    settings.permission_policy = preference;
    backend.save_settings(settings).await.unwrap();
    let pair = KeyPair::generate();
    backend.identity.lock().await.install(&pair).unwrap();
    let mut credentials = api_credentials(&server.origin);
    credentials.identity.public_key = pair.primary.public_key().to_hex();
    credentials.identity.recovery_public_key = pair.recovery.public_key().to_hex();
    if other_account {
        credentials.access_token = "other-account".into();
        credentials.principal.user_id = "user2".into();
        credentials.principal.membership_id = "member2".into();
        credentials.principal.learner_id = "learner2".into();
    }
    backend.tutor.lock().await.credentials = Some(credentials);
    backend
        .connect_agent(target, directory.to_string_lossy().into_owned())
        .await
        .unwrap();
    (backend, receiver)
}
async fn reply(
    backend: &Arc<Backend>,
    events: &mut tokio::sync::mpsc::UnboundedReceiver<Value>,
    marker: &str,
    human: bool,
) -> String {
    let conversation = backend.start_conversation(None, None).await.unwrap();
    let id = conversation.id.clone();
    let pending = tokio::spawn({
        let backend = backend.clone();
        let marker = marker.to_string();
        async move { backend.send_prompt(id, marker).await }
    });
    if human {
        let request = tokio::time::timeout(Duration::from_secs(5), async {
            loop {
                let event = events.recv().await.unwrap();
                if event["value"]["method"] == "session/request_permission" {
                    break event["value"]["id"].clone();
                }
            }
        })
        .await
        .expect("a consent prompt must reach the UI");
        assert!(!pending.is_finished());
        backend
            .answer_permission(request, Some("reject".into()))
            .await
            .unwrap();
    }
    tokio::time::timeout(Duration::from_secs(5), pending)
        .await
        .expect("configured consent must resolve the ACP request")
        .unwrap()
        .unwrap();
    while let Ok(event) = events.try_recv() {
        assert_ne!(
            event["value"]["method"], "session/request_permission",
            "automatic consent unexpectedly reached the UI"
        );
    }
    let state = backend.snapshot().await;
    state["conversations"]
        .as_array()
        .unwrap()
        .iter()
        .find(|c| c["id"] == conversation.id)
        .unwrap()["messages"]
        .as_array()
        .unwrap()
        .last()
        .unwrap()["text"]
        .as_str()
        .unwrap()
        .into()
}

#[tokio::test]
async fn tool_catalogue_uses_real_json_and_sse_pages_and_local_rpc() {
    let server=server(|request| {
        assert_eq!(request.headers.get("authorization").map(String::as_str),Some("Bearer test-token"));
        assert!(request.headers["accept"].contains("text/event-stream"));
        let body=request.json();assert_eq!(body["method"],"tools/list");
        if body["params"]["cursor"].is_null() {
            rpc(request,json!({"tools":[{"name":"get_next_activity","description":"Page one"}],"nextCursor":"page-two"}))
        } else {
            assert_eq!(body["params"]["cursor"],"page-two");
            ("text/event-stream".into(),format!("event: message\r\ndata: {}\r\n\r\nevent: message\r\ndata: {}\r\n\r\n",json!({"jsonrpc":"2.0","method":"notifications/tools/list_changed"}),json!({"jsonrpc":"2.0","id":body["id"],"result":{"tools":[{"name":"get_training","description":"Page two","annotations":{"readOnlyHint":true}}]}})))
        }
    }).await;
    let directory = tempfile::tempdir().unwrap();
    let (backend, _) = remote_backend(directory.path(), &server, None, false).await;
    let catalog = backend.list_mcp_tools().await.unwrap();
    assert_eq!(catalog["tools"].as_array().unwrap().len(), 2);
    assert_eq!(catalog["tools"][1]["annotations"]["readOnlyHint"], true);
    assert_eq!(
        server
            .requests
            .lock()
            .unwrap()
            .iter()
            .filter(|r| r.path == "/mcp")
            .count(),
        2
    );
    backend.shutdown().await;
    let local_dir = tempfile::tempdir().unwrap();
    let (local, _) = connected(local_dir.path()).await;
    assert_eq!(
        local.list_mcp_tools().await.unwrap()["tools"][0]["name"],
        "get_next_activity"
    );
    local.shutdown().await;
}

#[tokio::test]
async fn malformed_tool_catalogues_never_install_a_partial_permission_catalogue() {
    for scenario in [
        "wrong-id",
        "bad-sse",
        "duplicate-result",
        "duplicate-tool",
        "repeated-cursor",
        "invalid-name",
        "invalid-protocol",
    ] {
        let server=server(move |request| {
            let id=request.json()["id"].clone();
            match scenario {
                "wrong-id"=>("application/json".into(),json!({"jsonrpc":"2.0","id":"wrong","result":tools()}).to_string()),
                "bad-sse"=>("text/event-stream".into(),"event: message\ndata: {bad json\n\n".into()),
                "duplicate-result"=>{let frame=format!("data: {}\n\n",json!({"jsonrpc":"2.0","id":id,"result":tools()}));("text/event-stream".into(),frame.repeat(2))},
                "duplicate-tool"=>rpc(request,json!({"tools":[{"name":"same"},{"name":"same"}]})),
                "repeated-cursor"=>rpc(request,json!({"tools":[{"name":if request.json()["params"]["cursor"].is_null(){"first"}else{"second"}}],"nextCursor":"repeat"})),
                "invalid-name"=>rpc(request,json!({"tools":[{"name":"not a tool"}]})),
                "invalid-protocol"=>("application/json".into(),json!({"jsonrpc":"1.0","id":id,"result":tools()}).to_string()),
                _=>unreachable!(),
            }
        }).await;
        let dir = tempfile::tempdir().unwrap();
        let (backend, _) = remote_backend(dir.path(), &server, None, false).await;
        assert!(
            backend.list_mcp_tools().await.is_err(),
            "accepted malformed catalogue: {scenario}"
        );
        assert!(backend.tutor.lock().await.tool_catalog.is_empty());
        backend.shutdown().await;
    }
}

#[tokio::test]
async fn persistent_consent_survives_reopening_without_permission_ui() {
    let server = server(|request| rpc(request, tools())).await;
    let directory = tempfile::tempdir().unwrap();
    let mut preference = policy(&server.origin, PermissionDecision::Ask);
    preference
        .tools
        .insert("get_next_activity".into(), PermissionDecision::Allow);
    let (backend, mut events) =
        remote_backend(directory.path(), &server, Some(preference), false).await;
    assert_eq!(
        reply(
            &backend,
            &mut events,
            "EXPERIENCE_PERMISSION:CORRELATED",
            false
        )
        .await,
        "Allowed."
    );
    backend.shutdown().await;
    drop(backend);
    let (send, mut events) = tokio::sync::mpsc::unbounded_channel();
    let reopened = Backend::new(
        directory.path(),
        Arc::new(move |name, value| {
            let _ = send.send(json!({"name":name,"value":value}));
        }),
    )
    .unwrap();
    assert!(reopened.settings().await.permission_policy.is_some());
    reopened
        .connect_agent(
            fixture_target(),
            directory.path().to_string_lossy().into_owned(),
        )
        .await
        .unwrap();
    assert_eq!(
        reply(
            &reopened,
            &mut events,
            "EXPERIENCE_PERMISSION:CLAUDE",
            false
        )
        .await,
        "Allowed."
    );
    reopened.shutdown().await;
}

#[tokio::test]
async fn ask_denial_and_stale_calls_have_distinct_effective_outcomes() {
    let server = server(|request| rpc(request, tools())).await;
    for (decision, marker, human, expected) in [
        (
            PermissionDecision::Allow,
            "EXPERIENCE_PERMISSION:CORRELATED",
            false,
            "Allowed.",
        ),
        (
            PermissionDecision::Ask,
            "EXPERIENCE_PERMISSION:DIRECT",
            true,
            "Rejected.",
        ),
        (
            PermissionDecision::Deny,
            "EXPERIENCE_PERMISSION:DIRECT",
            false,
            "Rejected.",
        ),
        (
            PermissionDecision::Deny,
            "EXPERIENCE_PERMISSION:NO_REJECT",
            false,
            "Cancelled.",
        ),
        (
            PermissionDecision::Allow,
            "EXPERIENCE_PERMISSION:STALE",
            true,
            "Rejected.",
        ),
        (
            PermissionDecision::Allow,
            "EXPERIENCE_PERMISSION:COMPLETED",
            true,
            "Rejected.",
        ),
        (
            PermissionDecision::Allow,
            "EXPERIENCE_PERMISSION:QUESTION",
            true,
            "Rejected.",
        ),
        (
            PermissionDecision::Allow,
            "EXPERIENCE_PERMISSION:UNKNOWN",
            true,
            "Rejected.",
        ),
    ] {
        let directory = tempfile::tempdir().unwrap();
        let (backend, mut events) = remote_backend(
            directory.path(),
            &server,
            Some(policy(&server.origin, decision)),
            false,
        )
        .await;
        assert_eq!(
            reply(&backend, &mut events, marker, human).await,
            expected,
            "scenario {marker}"
        );
        backend.shutdown().await;
    }
}

#[tokio::test]
async fn account_change_during_catalogue_pagination_cannot_install_the_old_catalogue() {
    let target: Arc<std::sync::Mutex<Option<std::sync::Weak<Backend>>>> = Default::default();
    let during_request = target.clone();
    let server = server(move |request| {
        if request.json()["params"]["cursor"].is_null() {
            let backend = during_request
                .lock()
                .unwrap()
                .as_ref()
                .unwrap()
                .upgrade()
                .unwrap();
            let mut tutor = backend.tutor.try_lock().unwrap();
            tutor.credentials.as_mut().unwrap().principal.user_id = "changed-account".into();
            rpc(
                request,
                json!({"tools":[{"name":"first_tool"}],"nextCursor":"second-page"}),
            )
        } else {
            rpc(request, json!({"tools":[{"name":"second_tool"}]}))
        }
    })
    .await;
    let directory = tempfile::tempdir().unwrap();
    let (backend, _) = remote_backend(directory.path(), &server, None, false).await;
    *target.lock().unwrap() = Some(Arc::downgrade(&backend));
    let error = backend.list_mcp_tools().await.unwrap_err();
    assert!(error.contains("compte Tutor a changé"));
    assert!(backend.tutor.lock().await.tool_catalog.is_empty());
    assert_eq!(
        server
            .requests
            .lock()
            .unwrap()
            .iter()
            .filter(|r| r.path == "/mcp")
            .count(),
        2
    );
    backend.shutdown().await;
}

#[tokio::test]
async fn consent_never_crosses_account_institution_or_agent_boundaries() {
    let server = server(|request| rpc(request, tools())).await;
    for mismatch in ["origin", "institution", "agent", "account"] {
        let mut preference = policy(&server.origin, PermissionDecision::Allow);
        match mismatch {
            "origin" => preference.origin = "https://another.example".into(),
            "institution" => preference.institution_id = "other-institution".into(),
            "agent" => preference.agent_id = "other-agent".into(),
            _ => {}
        }
        let directory = tempfile::tempdir().unwrap();
        let (backend, mut events) = remote_backend(
            directory.path(),
            &server,
            if mismatch == "account" {
                None
            } else {
                Some(preference)
            },
            mismatch == "account",
        )
        .await;
        backend.list_mcp_tools().await.unwrap();
        assert_eq!(
            reply(&backend, &mut events, "EXPERIENCE_PERMISSION:DIRECT", true).await,
            "Rejected."
        );
        backend.shutdown().await;
    }
}

#[tokio::test]
async fn mcp_deny_policy_reaches_the_http_server_through_the_agent_configuration() {
    let server = server(|request| {
        if request.json()["method"] == "tools/list" {
            rpc(request, tools())
        } else {
            let consent: Value =
                serde_json::from_str(&request.headers["tutor-tool-policy"]).unwrap();
            assert_eq!(consent["default"], "deny");
            assert_eq!(request.headers["authorization"], "Bearer test-token");
            assert_eq!(request.json()["params"]["name"], "get_next_activity");
            rpc(
                request,
                json!({"isError":true,"structuredContent":{"error":"tool_permission_denied"}}),
            )
        }
    })
    .await;
    let dir = tempfile::tempdir().unwrap();
    let (backend, mut events) = remote_backend(
        dir.path(),
        &server,
        Some(policy(&server.origin, PermissionDecision::Deny)),
        false,
    )
    .await;
    assert_eq!(
        reply(&backend, &mut events, "EXPERIENCE_HTTP", false).await,
        "Server denied tool."
    );
    assert_eq!(
        server
            .requests
            .lock()
            .unwrap()
            .iter()
            .filter(|r| r.path == "/mcp" && r.json()["method"] == "tools/call")
            .count(),
        1
    );
    backend.shutdown().await;
}

#[tokio::test]
async fn institutional_actions_carry_verifiable_exact_body_proofs_and_fresh_nonces() {
    use base64::{engine::general_purpose::STANDARD, Engine};
    use nostr::{Event, JsonUtil, Kind};
    let server = server(|request| rpc(request, tools())).await;
    let dir = tempfile::tempdir().unwrap();
    let (backend, _) = remote_backend(dir.path(), &server, None, false).await;
    let expected = backend.identity.lock().await.active().unwrap().public_key();
    for action in ["overview", "list_members"] {
        assert_eq!(
            backend
                .institution_action(action.into(), json!({"query":"fixture"}))
                .await
                .unwrap()["received"],
            true
        );
    }
    let requests = server.requests.lock().unwrap().clone();
    let actions: Vec<_> = requests
        .iter()
        .filter(|r| r.path == "/api/client/v1/institution/actions")
        .collect();
    assert_eq!(actions.len(), 2);
    let mut nonces = HashSet::new();
    for action in actions {
        assert_eq!(action.headers["authorization"], "Bearer test-token");
        let event = Event::from_json(
            STANDARD
                .decode(&action.headers["x-tutor-action-proof"])
                .unwrap(),
        )
        .unwrap();
        event.verify().unwrap();
        assert_eq!(event.pubkey, expected);
        assert_eq!(event.kind, Kind::Custom(27235));
        assert!(event.content.is_empty());
        let tags: BTreeMap<String, String> = event
            .tags
            .iter()
            .map(|t| {
                let values = t.as_slice();
                assert_eq!(values.len(), 2);
                (values[0].clone(), values[1].clone())
            })
            .collect();
        assert_eq!(
            tags["u"],
            format!("{}/api/client/v1/institution/actions", server.origin)
        );
        assert_eq!(tags["method"], "POST");
        assert_eq!(tags.len(), 6);
        assert_eq!(tags["tenant"], "tenant1");
        assert_eq!(tags["session"], "session-test");
        assert_eq!(
            tags["payload"],
            format!("{:x}", Sha256::digest(&action.body))
        );
        assert!(uuid::Uuid::parse_str(&tags["nonce"]).is_ok());
        assert!(nonces.insert(tags["nonce"].clone()));
    }
    drop(requests);
    backend.shutdown().await;
}
