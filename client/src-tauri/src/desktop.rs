// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

use crate::backend::Backend;
use crate::discovery::AgentTarget;
use crate::workspace::{LearningIntent, Settings};
use serde_json::{json, Value};
use std::sync::Arc;
use tauri::{Emitter, Manager, State};
use tauri_plugin_opener::OpenerExt;

type AppBackend<'a> = State<'a, Arc<Backend>>;

#[tauri::command]
async fn load_state(backend: AppBackend<'_>) -> Result<Value, String> {
    Ok(backend.snapshot().await)
}

#[tauri::command]
async fn discover_agents() -> Vec<AgentTarget> {
    crate::discovery::discover_agents().await
}

#[tauri::command]
async fn save_settings(backend: AppBackend<'_>, settings: Settings) -> Result<Value, String> {
    backend.save_settings(settings).await
}

#[tauri::command]
async fn save_intent(backend: AppBackend<'_>, intent: LearningIntent) -> Result<Value, String> {
    backend.save_intent(intent).await
}

#[tauri::command]
async fn connect_agent(
    backend: AppBackend<'_>,
    target: AgentTarget,
    cwd: String,
) -> Result<Value, String> {
    backend.inner().connect_agent(target, cwd).await
}

#[tauri::command]
async fn authenticate_agent(backend: AppBackend<'_>, method_id: String) -> Result<Value, String> {
    backend.authenticate_agent(method_id).await
}

#[tauri::command]
async fn start_conversation(
    backend: AppBackend<'_>,
    intent_id: Option<String>,
    resume_id: Option<String>,
) -> Result<Value, String> {
    serde_json::to_value(backend.start_conversation(intent_id, resume_id).await?)
        .map_err(|_| "Impossible de présenter la séance.".into())
}

#[tauri::command]
async fn send_prompt(
    backend: AppBackend<'_>,
    conversation_id: String,
    text: String,
) -> Result<Value, String> {
    backend.send_prompt(conversation_id, text).await
}

#[tauri::command]
async fn cancel_prompt(backend: AppBackend<'_>, conversation_id: String) -> Result<(), String> {
    backend.inner().cancel_prompt(conversation_id).await
}

#[tauri::command]
async fn answer_permission(
    backend: AppBackend<'_>,
    request_id: Value,
    option_id: Option<String>,
) -> Result<(), String> {
    backend.answer_permission(request_id, option_id).await
}

#[tauri::command]
async fn disconnect_agent(backend: AppBackend<'_>) -> Result<(), String> {
    backend.disconnect_agent().await
}

#[tauri::command]
async fn begin_tutor_login<R: tauri::Runtime>(
    app: tauri::AppHandle<R>,
    backend: AppBackend<'_>,
    url: String,
    client_id: Option<String>,
    initial_access_token: Option<String>,
) -> Result<Value, String> {
    let authorization_url = backend
        .inner()
        .begin_tutor_login(url, client_id, initial_access_token)
        .await?;
    // The URL comes only from the validated OAuth flow, never from model output.
    // Returning it also allows an explicit copy/open fallback on this device.
    let _ = app.opener().open_url(&authorization_url, None::<&str>);
    Ok(json!({"authorizationUrl": authorization_url}))
}

#[tauri::command]
async fn disconnect_tutor(backend: AppBackend<'_>) -> Result<Value, String> {
    backend.disconnect_tutor().await
}

#[tauri::command]
async fn tutor_overview(backend: AppBackend<'_>) -> Result<Value, String> {
    backend.tutor_overview().await
}

fn command_handler<R: tauri::Runtime>(
) -> impl Fn(tauri::ipc::Invoke<R>) -> bool + Send + Sync + 'static {
    tauri::generate_handler![
        load_state,
        discover_agents,
        save_settings,
        save_intent,
        connect_agent,
        authenticate_agent,
        start_conversation,
        send_prompt,
        cancel_prompt,
        answer_permission,
        disconnect_agent,
        begin_tutor_login,
        disconnect_tutor,
        tutor_overview
    ]
}

pub fn run() {
    let app = tauri::Builder::default()
        .plugin(tauri_plugin_opener::init())
        .setup(|app| {
            let directory = app.path().app_data_dir()?;
            let handle = app.handle().clone();
            let backend = Backend::new(
                &directory,
                Arc::new(move |event, value| {
                    let _ = handle.emit(event, value);
                }),
            )
            .map_err(std::io::Error::other)?;
            app.manage(backend.clone());
            tauri::async_runtime::spawn(async move {
                backend.restore_tutor().await;
            });
            Ok(())
        })
        .invoke_handler(command_handler())
        .build(tauri::generate_context!())
        .expect("Impossible de démarrer Tutor");
    app.run(|app, event| {
        if matches!(event, tauri::RunEvent::ExitRequested { .. }) {
            let backend = app.state::<Arc<Backend>>().inner().clone();
            // The OS process is exiting: complete child cleanup before return.
            tauri::async_runtime::block_on(backend.shutdown());
        }
    });
}

#[cfg(all(test, windows))]
mod tests {
    use super::*;
    use crate::workspace::TutorSettings;

    #[test]
    fn desktop_ipc_uses_the_real_command_registry_and_camel_case_arguments() {
        let directory = tempfile::tempdir().unwrap();
        let backend = Backend::new(directory.path(), Arc::new(|_, _| {})).unwrap();
        let app = tauri::test::mock_builder()
            .manage(backend.clone())
            .invoke_handler(command_handler())
            .build(tauri::test::mock_context(tauri::test::noop_assets()))
            .unwrap();
        let window = tauri::WebviewWindowBuilder::new(&app, "main", Default::default())
            .build()
            .unwrap();
        let call = |command: &str, body: Value| -> Value {
            tauri::test::get_ipc_response(
                &window,
                tauri::webview::InvokeRequest {
                    cmd: command.into(),
                    callback: tauri::ipc::CallbackFn(0),
                    error: tauri::ipc::CallbackFn(1),
                    url: "http://tauri.localhost".parse().unwrap(),
                    body: tauri::ipc::InvokeBody::Json(body),
                    headers: Default::default(),
                    invoke_key: tauri::test::INVOKE_KEY.into(),
                },
            )
            .unwrap_or_else(|error| panic!("IPC {command} failed: {error}"))
            .deserialize()
            .unwrap()
        };
        assert_eq!(call("load_state", json!({}))["agentConnected"], false);
        let adapter = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../tests/backend/fake-runtime.cjs")
            .canonicalize()
            .unwrap();
        let target = AgentTarget {
            id: "ipc-test".into(),
            label: "IPC test".into(),
            environment: "native".into(),
            distribution: None,
            wsl_user: None,
            adapter_path: Some(adapter.to_string_lossy().into_owned()),
            codex_path: None,
            adapter_version: None,
            codex_version: None,
            available: true,
            detail: String::new(),
        };
        let cwd = directory.path().to_string_lossy().into_owned();
        let settings = Settings {
            target_id: target.id.clone(),
            manual_target: Some(target.clone()),
            cwd: cwd.clone(),
            tutor: TutorSettings {
                mode: "local".into(),
                url: String::new(),
                command: adapter.to_string_lossy().into_owned(),
                args: vec!["--local".into()],
            },
        };
        call("save_settings", json!({"settings":settings}));
        assert_eq!(
            call("connect_agent", json!({"target":target,"cwd":cwd}))["protocolVersion"],
            1
        );
        assert!(call("tutor_overview", json!({}))["context"].is_object());
        let state = call(
            "save_intent",
            json!({"intent":LearningIntent {title:"Contrat IPC réel".into(),validated:true,..Default::default()}}),
        );
        let conversation = call(
            "start_conversation",
            json!({"intentId":state["intents"][0]["id"]}),
        );
        let id = conversation["id"].clone();
        call("send_prompt", json!({"conversationId":id,"text":"Bonjour"}));
        let state = call("load_state", json!({}));
        assert_eq!(
            state["conversations"][0]["messages"][1]["text"],
            "Bonjour, élève."
        );
        assert_eq!(
            state["conversations"][0]["messages"][1]["status"],
            "complete"
        );
        call("disconnect_agent", json!({}));
        call("disconnect_tutor", json!({}));
        assert_eq!(call("load_state", json!({}))["agentConnected"], false);
        let reopened = Backend::new(directory.path(), Arc::new(|_, _| {})).unwrap();
        assert_eq!(
            tauri::async_runtime::block_on(reopened.snapshot())["conversations"][0]["messages"]
                .as_array()
                .unwrap()
                .len(),
            2
        );
    }
}
