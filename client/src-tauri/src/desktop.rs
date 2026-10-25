// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

#[cfg(test)]
use crate::backend::Backend;
use crate::discovery::AgentTarget;
use crate::profiles::ClientProfiles;
use crate::workspace::{LearningIntent, Settings};
#[cfg(all(test, windows))]
use serde_json::json;
use serde_json::Value;
use std::sync::Arc;
use tauri::{Emitter, Manager, State};
use tauri_plugin_dialog::DialogExt;
use tauri_plugin_opener::OpenerExt;

type AppBackend<'a> = State<'a, Arc<ClientProfiles>>;

#[tauri::command]
fn bundled_agents() -> Vec<crate::discovery::AgentTarget> {
    crate::bundled::targets()
}

#[tauri::command]
async fn create_profile(backend: AppBackend<'_>, label: String) -> Result<Value, String> {
    backend.create(label).await
}
#[tauri::command]
async fn select_profile(backend: AppBackend<'_>, id: String) -> Result<Value, String> {
    backend.select(id).await
}
#[tauri::command]
async fn rename_profile(backend: AppBackend<'_>, label: String) -> Result<Value, String> {
    backend.rename(label).await
}

#[tauri::command]
async fn formation_versions(
    backend: AppBackend<'_>,
    formation_id: String,
) -> Result<Value, String> {
    backend
        .current()
        .await
        .formation_versions(formation_id)
        .await
}
#[tauri::command]
async fn formation_review(backend: AppBackend<'_>, version_id: String) -> Result<Value, String> {
    backend.current().await.formation_review(version_id).await
}
#[tauri::command]
async fn create_formation(
    backend: AppBackend<'_>,
    name: String,
    description: String,
    key: String,
) -> Result<Value, String> {
    backend
        .current()
        .await
        .create_formation(name, description, key)
        .await
}
#[tauri::command]
async fn publish_formation_to_catalog(
    backend: AppBackend<'_>,
    version_id: String,
    key: String,
) -> Result<Value, String> {
    backend
        .current()
        .await
        .publish_formation_to_catalog(version_id, key)
        .await
}
#[tauri::command]
async fn clone_formation_version(
    backend: AppBackend<'_>,
    version_id: String,
    key: String,
) -> Result<Value, String> {
    backend
        .current()
        .await
        .clone_formation_version(version_id, key)
        .await
}
#[tauri::command]
async fn enable_learning(backend: AppBackend<'_>) -> Result<Value, String> {
    backend.current().await.enable_learning().await
}

#[tauri::command]
async fn open_external_link<R: tauri::Runtime>(
    app: tauri::AppHandle<R>,
    url: String,
) -> Result<(), String> {
    let parsed = reqwest::Url::parse(&url).map_err(|_| "Lien invalide.")?;
    if !["https", "http"].contains(&parsed.scheme())
        || parsed.host_str().is_none()
        || !parsed.username().is_empty()
        || parsed.password().is_some()
    {
        return Err("Ce type de lien ne peut pas être ouvert depuis une réponse.".into());
    }
    app.opener()
        .open_url(url, None::<&str>)
        .map_err(|_| "Impossible d’ouvrir le lien dans votre navigateur.".into())
}

#[tauri::command]
async fn load_state(backend: AppBackend<'_>) -> Result<Value, String> {
    let backend = backend.current().await;
    Ok(backend.snapshot().await)
}

#[tauri::command]
async fn discover_agents() -> Vec<AgentTarget> {
    crate::discovery::discover_agents().await
}

#[tauri::command]
async fn save_settings(backend: AppBackend<'_>, settings: Settings) -> Result<Value, String> {
    let backend = backend.current().await;
    backend.save_settings(settings).await
}

#[tauri::command]
async fn save_intent(backend: AppBackend<'_>, intent: LearningIntent) -> Result<Value, String> {
    let backend = backend.current().await;
    backend.save_intent(intent).await
}

#[tauri::command]
async fn connect_agent(
    backend: AppBackend<'_>,
    target: AgentTarget,
    cwd: String,
) -> Result<Value, String> {
    let backend = backend.current().await;
    backend.connect_agent(target, cwd).await
}

#[tauri::command]
async fn authenticate_agent(backend: AppBackend<'_>, method_id: String) -> Result<Value, String> {
    let backend = backend.current().await;
    backend.authenticate_agent(method_id).await
}

#[tauri::command]
async fn start_conversation(
    backend: AppBackend<'_>,
    intent_id: Option<String>,
    resume_id: Option<String>,
) -> Result<Value, String> {
    let backend = backend.current().await;
    serde_json::to_value(backend.start_conversation(intent_id, resume_id).await?)
        .map_err(|_| "Impossible de présenter la séance.".into())
}

#[tauri::command]
async fn send_prompt(
    backend: AppBackend<'_>,
    conversation_id: String,
    text: String,
) -> Result<Value, String> {
    let backend = backend.current().await;
    backend.send_prompt(conversation_id, text).await
}

#[tauri::command]
async fn cancel_prompt(backend: AppBackend<'_>, conversation_id: String) -> Result<(), String> {
    let backend = backend.current().await;
    backend.cancel_prompt(conversation_id).await
}

#[tauri::command]
async fn answer_permission(
    backend: AppBackend<'_>,
    request_id: Value,
    option_id: Option<String>,
) -> Result<(), String> {
    let backend = backend.current().await;
    backend.answer_permission(request_id, option_id).await
}

#[tauri::command]
async fn disconnect_agent(backend: AppBackend<'_>) -> Result<(), String> {
    let backend = backend.current().await;
    backend.disconnect_agent().await
}

#[tauri::command]
async fn create_identity(
    backend: AppBackend<'_>,
    nsec: Option<String>,
) -> Result<crate::identity::PublicIdentity, String> {
    let backend = backend.current().await;
    backend.create_identity(nsec).await
}
async fn select_backup<R: tauri::Runtime>(
    app: &tauri::AppHandle<R>,
    save: bool,
) -> Result<std::path::PathBuf, String> {
    let (sender, receiver) = tokio::sync::oneshot::channel();
    let dialog = app
        .dialog()
        .file()
        .add_filter("Sauvegarde d’identité Tutor", &["json"]);
    if save {
        dialog
            .set_file_name("tutor-identite-sauvegarde.json")
            .save_file(move |file| {
                let _ = sender.send(file);
            });
    } else {
        dialog.pick_file(move |file| {
            let _ = sender.send(file);
        });
    }
    receiver
        .await
        .map_err(|_| "La sélection de fichier a été interrompue.")?
        .ok_or("Aucun fichier sélectionné.")?
        .into_path()
        .map_err(|_| "Choisissez un fichier local.".into())
}
#[tauri::command]
async fn backup_identity<R: tauri::Runtime>(
    app: tauri::AppHandle<R>,
    backend: AppBackend<'_>,
    passphrase: String,
) -> Result<(), String> {
    let backend = backend.current().await;
    let passphrase = zeroize::Zeroizing::new(passphrase);
    let path = select_backup(&app, true).await?;
    backend.backup_identity(path, passphrase.to_string()).await
}
#[tauri::command]
async fn confirm_identity_backup(
    backend: AppBackend<'_>,
) -> Result<crate::identity::PublicIdentity, String> {
    let backend = backend.current().await;
    backend.confirm_identity_backup().await
}
#[tauri::command]
async fn restore_identity<R: tauri::Runtime>(
    app: tauri::AppHandle<R>,
    backend: AppBackend<'_>,
    passphrase: String,
) -> Result<crate::identity::PublicIdentity, String> {
    let backend = backend.current().await;
    let passphrase = zeroize::Zeroizing::new(passphrase);
    let path = select_backup(&app, false).await?;
    backend.restore_identity(path, passphrase.to_string()).await
}
#[tauri::command]
async fn discover_institution(
    backend: AppBackend<'_>,
    url: String,
) -> Result<crate::native_auth::Institution, String> {
    let backend = backend.current().await;
    backend.discover_institution(url).await
}
#[tauri::command]
async fn connect_tutor(
    backend: AppBackend<'_>,
    url: String,
    authoring: bool,
) -> Result<Value, String> {
    let backend = backend.current().await;
    backend.connect_tutor(url, authoring).await
}
#[tauri::command]
async fn rotate_identity<R: tauri::Runtime>(
    app: tauri::AppHandle<R>,
    backend: AppBackend<'_>,
    old_passphrase: String,
    new_passphrase: String,
) -> Result<crate::identity::PublicIdentity, String> {
    let backend = backend.current().await;
    let old_passphrase = zeroize::Zeroizing::new(old_passphrase);
    let new_passphrase = zeroize::Zeroizing::new(new_passphrase);
    let old = select_backup(&app, false).await?;
    let new = select_backup(&app, true).await?;
    backend
        .rotate_identity(
            old,
            old_passphrase.to_string(),
            new,
            new_passphrase.to_string(),
        )
        .await
}
#[tauri::command]
async fn recover_identity<R: tauri::Runtime>(
    app: tauri::AppHandle<R>,
    backend: AppBackend<'_>,
    url: String,
    old_passphrase: String,
    new_passphrase: String,
) -> Result<crate::identity::PublicIdentity, String> {
    let backend = backend.current().await;
    let old_passphrase = zeroize::Zeroizing::new(old_passphrase);
    let new_passphrase = zeroize::Zeroizing::new(new_passphrase);
    let old = select_backup(&app, false).await?;
    let new = select_backup(&app, true).await?;
    backend
        .recover_identity(
            url,
            old,
            old_passphrase.to_string(),
            new,
            new_passphrase.to_string(),
        )
        .await
}

#[tauri::command]
async fn disconnect_tutor(backend: AppBackend<'_>) -> Result<Value, String> {
    let backend = backend.current().await;
    backend.disconnect_tutor().await
}

#[tauri::command]
async fn tutor_overview(backend: AppBackend<'_>) -> Result<Value, String> {
    let backend = backend.current().await;
    backend.tutor_overview().await
}

fn command_handler<R: tauri::Runtime>(
) -> impl Fn(tauri::ipc::Invoke<R>) -> bool + Send + Sync + 'static {
    tauri::generate_handler![
        create_profile,
        select_profile,
        rename_profile,
        open_external_link,
        formation_versions,
        bundled_agents,
        formation_review,
        create_formation,
        publish_formation_to_catalog,
        clone_formation_version,
        enable_learning,
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
        create_identity,
        backup_identity,
        confirm_identity_backup,
        restore_identity,
        discover_institution,
        connect_tutor,
        rotate_identity,
        recover_identity,
        disconnect_tutor,
        tutor_overview
    ]
}

pub fn run() {
    let app = tauri::Builder::default()
        .plugin(tauri_plugin_opener::init())
        .plugin(tauri_plugin_dialog::init())
        .setup(|app| {
            crate::bundled::configure(app.path().resource_dir()?.join("agents"));
            let directory = app.path().app_data_dir()?;
            let handle = app.handle().clone();
            let backend = ClientProfiles::new(
                &directory,
                Arc::new(move |event, value| {
                    let _ = handle.emit(event, value);
                }),
            )
            .map_err(std::io::Error::other)?;
            app.manage(backend.clone());
            tauri::async_runtime::spawn(async move {
                backend.current().await.restore_tutor().await;
            });
            Ok(())
        })
        .invoke_handler(command_handler())
        .build(tauri::generate_context!())
        .expect("Impossible de démarrer Tutor");
    app.run(|app, event| {
        if matches!(event, tauri::RunEvent::ExitRequested { .. }) {
            let backend = app.state::<Arc<ClientProfiles>>().inner().clone();
            // The OS process is exiting: complete child cleanup before return.
            tauri::async_runtime::block_on(async { backend.current().await.shutdown().await });
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
        let profiles = ClientProfiles::new(directory.path(), Arc::new(|_, _| {})).unwrap();
        let backend = tauri::async_runtime::block_on(profiles.current());
        let app = tauri::test::mock_builder()
            .manage(profiles.clone())
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
        let initial = call("load_state", json!({}));
        assert_eq!(initial["agentConnected"], false);
        assert_eq!(initial["identity"]["exists"], false);
        let public = call("create_identity", json!({"nsec":null}));
        assert!(public["npub"].as_str().unwrap().starts_with("npub1"));
        assert_ne!(public["npub"], public["recoveryNpub"]);
        assert_eq!(public["backupConfirmed"], false);
        assert!(public.get("secret").is_none());
        assert_eq!(call("load_state", json!({}))["identity"]["exists"], true);
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
                activity: "auto".into(),
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
        let disconnected = call("load_state", json!({}));
        assert_eq!(disconnected["agentConnected"], false);
        assert_eq!(disconnected["identity"]["exists"], true);
        assert_eq!(disconnected["identity"]["npub"], public["npub"]);
        let reopened = Backend::new(directory.path(), Arc::new(|_, _| {})).unwrap();
        let reopened_state = tauri::async_runtime::block_on(reopened.snapshot());
        assert_eq!(reopened_state["identity"]["npub"], public["npub"]);
        assert_eq!(
            reopened_state["conversations"][0]["messages"]
                .as_array()
                .unwrap()
                .len(),
            2
        );
        assert!(call("bundled_agents", json!({})).is_array());
        let second = call("create_profile", json!({"label":"Compte apprenant"}));
        assert_eq!(second["identity"]["exists"], false);
        assert_eq!(second["conversations"].as_array().unwrap().len(), 0);
        call("rename_profile", json!({"label":"Compte de test"}));
        assert_eq!(
            call("load_state", json!({}))["profile"]["label"],
            "Compte de test"
        );
        let restored = call("select_profile", json!({"id":"principal"}));
        assert_eq!(restored["identity"]["npub"], public["npub"]);
        assert_eq!(restored["conversations"].as_array().unwrap().len(), 1);
    }
}
