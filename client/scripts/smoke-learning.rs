// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! Opt-in actual-model acceptance. Uses a fresh Tutor profile and the selected
//! user's existing Codex login. Never reads or prints provider credentials.

use serde_json::{json, Value};
use std::path::PathBuf;
use std::sync::Arc;
use tutor_client::{
    backend::Backend,
    discovery::AgentTarget,
    workspace::{Settings, TutorSettings},
};

#[tokio::main]
async fn main() {
    if let Err(error) = smoke().await {
        eprintln!("Parcours réel incomplet : {error}");
        std::process::exit(1);
    }
}

async fn smoke() -> Result<(), String> {
    let mut args = std::env::args().skip(1);
    let adapter = PathBuf::from(
        args.next()
            .ok_or("Usage: tutor-client-smoke ADAPTER TUTOR TEST_DIRECTORY [--live-model]")?,
    )
    .canonicalize()
    .map_err(|_| "Adaptateur introuvable.")?;
    let tutor = PathBuf::from(args.next().ok_or("Binaire Tutor requis.")?)
        .canonicalize()
        .map_err(|_| "Binaire Tutor introuvable.")?;
    let directory = PathBuf::from(args.next().ok_or("Dossier de test neuf requis.")?);
    let live = args.next().as_deref() == Some("--live-model");
    if directory.exists() {
        return Err(
            "Le dossier de test doit être neuf pour protéger les données existantes.".into(),
        );
    }
    std::fs::create_dir_all(&directory).map_err(|_| "Création du dossier de test impossible.")?;
    let directory = directory
        .canonicalize()
        .map_err(|_| "Dossier de test invalide.")?;
    let target = AgentTarget {
        id: "smoke-native".into(),
        label: "Qualification Codex réelle".into(),
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
    let settings = Settings {
        target_id: target.id.clone(),
        manual_target: Some(target.clone()),
        cwd: directory.to_string_lossy().into_owned(),
        tutor: TutorSettings {
            mode: "local".into(),
            activity: "auto".into(),
            url: String::new(),
            command: tutor.to_string_lossy().into_owned(),
            args: vec![
                "--local".into(),
                "--data-dir".into(),
                directory.join("tutor").to_string_lossy().into_owned(),
            ],
        },
    };
    let (sender, mut events) = tokio::sync::mpsc::unbounded_channel();
    let event_control = sender.clone();
    let backend = Backend::new(
        &directory.join("client"),
        Arc::new(move |name, value| {
            if name == "acp:event" {
                let _ = sender.send(value);
            }
        }),
    )?;
    backend.save_settings(settings.clone()).await?;
    let initialized = backend
        .connect_agent(target.clone(), settings.cwd.clone())
        .await?;
    let overview = backend.tutor_overview().await?;
    if !overview["context"].is_object() {
        return Err("Tutor ne retourne pas son contexte pédagogique.".into());
    }
    if !live {
        backend.shutdown().await;
        println!(
            "{}",
            json!({"adapter":initialized["agentInfo"],"realTutorContext":true,"liveModel":false})
        );
        return Ok(());
    }
    let conversation = backend.start_conversation(None, None).await?;
    let (tools_tx, tools_rx) = tokio::sync::oneshot::channel();
    let permission_target = Arc::new(tokio::sync::Mutex::new(backend.clone()));
    let permission_backend = permission_target.clone();
    let event_task = tokio::spawn(async move {
        let mut tools = Vec::<String>::new();
        let mut tool_names = std::collections::HashMap::<String, String>::new();
        while let Some(event) = events.recv().await {
            if event["method"] == "session/request_permission" {
                // Only isolated Tutor MCP operations are authorized. Built-in
                // terminal/filesystem tools and publication remain denied.
                let title = event["params"]["toolCall"]["title"]
                    .as_str()
                    .filter(|value| !value.is_empty())
                    .or_else(|| {
                        event["params"]["toolCall"]["toolCallId"]
                            .as_str()
                            .and_then(|id| tool_names.get(id).map(String::as_str))
                    })
                    .unwrap_or("");
                let allowed = title.starts_with("mcp.tutor.")
                    && !["publish", "delete", "share", "webhook"]
                        .iter()
                        .any(|name| title.contains(name));
                let choice = if allowed {
                    event["params"]["options"]
                        .as_array()
                        .and_then(|options| {
                            options.iter().find(|option| option["kind"] == "allow_once")
                        })
                        .and_then(|option| option["optionId"].as_str())
                        .map(str::to_string)
                } else {
                    None
                };
                let target = permission_backend.lock().await.clone();
                let _ = target.answer_permission(event["id"].clone(), choice).await;
            }
            if event["method"] == "session/update"
                && [Some("tool_call"), Some("tool_call_update")]
                    .contains(&event["params"]["update"]["sessionUpdate"].as_str())
            {
                if let Some(title) = event["params"]["update"]["title"].as_str() {
                    tools.push(title.to_string());
                    if let Some(id) = event["params"]["update"]["toolCallId"].as_str() {
                        tool_names.insert(id.to_string(), title.to_string());
                    }
                }
            }
            if event["_smokeDone"] == true {
                break;
            }
        }
        let _ = tools_tx.send(tools);
    });
    let mut original_go = String::new();
    for (index, message) in [
        "Je veux apprendre le Go.",
        "Je veux apprendre Python.",
        "Je veux apprendre l’astronomie.",
    ]
    .iter()
    .enumerate()
    {
        backend
            .send_prompt(conversation.id.clone(), (*message).into())
            .await?;
        let library = backend
            .training_action(
                "list_training_library".into(),
                json!({"filter":"personal","limit":100}),
            )
            .await?;
        let items = library["items"]
            .as_array()
            .ok_or("Bibliothèque réelle absente.")?;
        if items.len() != index + 1 {
            return Err(format!(
                "Création immédiate : {} formations enregistrées, {} attendues.",
                items.len(),
                index + 1
            ));
        }
        let context = backend
            .training_action(
                "get_conversation_context".into(),
                json!({"conversation_id":conversation.id}),
            )
            .await?;
        if context["domain_id"].as_str().is_none_or(str::is_empty) {
            return Err("La formation n’a pas été sélectionnée sur le serveur.".into());
        }
        if index == 0 {
            original_go = context["domain_id"].as_str().unwrap().to_string();
        }
    }
    let saved = backend.snapshot().await;
    if saved["conversations"][0]["messages"]
        .as_array()
        .map(Vec::len)
        != Some(6)
    {
        return Err("Trois échanges réels non conservés.".into());
    }
    backend.shutdown().await;
    let resumed_events = event_control.clone();
    let reopened = Backend::new(
        &directory.join("client"),
        Arc::new(move |name, value| {
            if name == "acp:event" {
                let _ = resumed_events.send(value);
            }
        }),
    )?;
    *permission_target.lock().await = reopened.clone();
    reopened.connect_agent(target, settings.cwd).await?;
    reopened
        .start_conversation(None, Some(conversation.id.clone()))
        .await?;
    reopened
        .send_prompt(conversation.id.clone(), "Reprenons ma formation Go.".into())
        .await?;
    let context = reopened
        .training_action(
            "get_conversation_context".into(),
            json!({"conversation_id":conversation.id}),
        )
        .await?;
    if context["domain_id"] != original_go {
        return Err("La reprise n’a pas retrouvé la formation Go d’origine.".into());
    }
    let library = reopened
        .training_action(
            "list_training_library".into(),
            json!({"filter":"personal","limit":100}),
        )
        .await?;
    if library["items"].as_array().map(Vec::len) != Some(3) {
        return Err("La reprise a créé un doublon.".into());
    }
    let persisted = reopened.snapshot().await;
    if persisted["conversations"][0]["messages"]
        .as_array()
        .map(Vec::len)
        != Some(8)
    {
        return Err("La reprise a rejoué un échange.".into());
    }
    reopened.shutdown().await;
    let _ = event_control.send(json!({"_smokeDone":true}));
    event_task
        .await
        .map_err(|_| "La collecte des appels de l’agent a été interrompue.")?;
    let tools = tools_rx
        .await
        .map_err(|_| "L’inventaire des appels réels est absent.")?;
    if !tools
        .iter()
        .any(|title| title.contains("get_next_activity"))
    {
        return Err(format!(
            "L’appel réel à get_next_activity n’est pas prouvé. Outils observés : {}",
            tools.join(", ")
        ));
    }
    let report: Value = json!({"adapter":initialized["agentInfo"],"realTutorContext":true,"liveModel":true,"personalFormationsCreated":3,"explicitContext":true,"goPythonAstronomyGo":true,"activityToolCalled":true,"activityPresented":true,"observationRecorded":false,"replyPersisted":true,"restartPassed":true,"duplicateMessages":false,"tools":tools});
    std::fs::write(
        directory.join("acceptance.json"),
        serde_json::to_vec_pretty(&report).unwrap(),
    )
    .map_err(|_| "Rapport non enregistré.")?;
    println!("{}", report);
    Ok(())
}
