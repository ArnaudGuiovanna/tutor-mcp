// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! Opt-in actual-model acceptance. Uses a fresh Tutor profile and the selected
//! user's existing provider login. Never reads or prints provider credentials.

use serde_json::{json, Value};
use std::path::PathBuf;
use std::sync::Arc;
use tutor_client::{
    backend::Backend,
    discovery::AgentTarget,
    permissions::{canonical_tool, ToolCallRegistry},
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
    let options: Vec<String> = args.collect();
    let live = options.iter().any(|option| option == "--live-model");
    let language = options
        .iter()
        .find_map(|option| option.strip_prefix("--language="))
        .unwrap_or("fr");
    let speed = options
        .iter()
        .find_map(|option| option.strip_prefix("--speed="))
        .unwrap_or("balanced");
    if !["fr", "en"].contains(&language) || !["balanced", "fast", "deep"].contains(&speed) {
        return Err("Invalid qualification language or speed.".into());
    }
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
        label: "Qualification agent réelle".into(),
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
        language: language.into(),
        agent: tutor_client::workspace::AgentSettings {
            speed: speed.into(),
            ..Default::default()
        },
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
        ..Default::default()
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
    let cold_started = std::time::Instant::now();
    let initialized = backend
        .connect_agent(target.clone(), settings.cwd.clone())
        .await?;
    let cold_start_ms = cold_started.elapsed().as_millis();
    let context_started = std::time::Instant::now();
    let overview = backend.tutor_overview().await?;
    let context_ms = context_started.elapsed().as_millis();
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
        let mut registry = ToolCallRegistry::default();
        let started = std::time::Instant::now();
        while let Some(event) = events.recv().await {
            registry.observe(&event);
            if event["method"] == "session/request_permission" {
                // Only isolated Tutor MCP operations are authorized. Built-in
                // terminal/filesystem tools and publication remain denied.
                let tool = registry.request_tool(&event).unwrap_or("");
                if !tool.is_empty() {
                    tools.push(tool.to_owned());
                    eprintln!(
                        "{}",
                        json!({"qualification":"permission","elapsedMs":started.elapsed().as_millis(),"name":tool})
                    );
                }
                let allowed = !tool.is_empty()
                    && !["publish", "delete", "share", "webhook"]
                        .iter()
                        .any(|name| tool.contains(name));
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
                // Claude's ordinary tool reports carry the SDK tool name,
                // while only approval requests carry mcpServer metadata.
                // This observational fallback never grants a permission.
                let update = &event["params"]["update"];
                let observed = canonical_tool(update).or_else(|| {
                    update
                        .pointer("/_meta/claudeCode/toolName")?
                        .as_str()?
                        .strip_prefix("mcp__tutor__")
                        .filter(|name| {
                            !name.is_empty()
                                && name
                                    .bytes()
                                    .all(|c| c.is_ascii_alphanumeric() || b"_.-".contains(&c))
                        })
                });
                if let Some(tool) = observed {
                    tools.push(tool.to_string());
                    eprintln!(
                        "{}",
                        json!({"qualification":"tool","elapsedMs":started.elapsed().as_millis(),"name":tool,"status":event["params"]["update"]["status"]})
                    );
                }
            }
            if event["_smokeDone"] == true {
                break;
            }
        }
        let _ = tools_tx.send(tools);
    });
    let mut original_go = String::new();
    let mut turn_timings_ms = Vec::new();
    for (index, message) in [
        "Je veux apprendre le Go.",
        "Je veux apprendre Python.",
        "Je veux apprendre l’astronomie.",
    ]
    .iter()
    .enumerate()
    {
        let turn_started = std::time::Instant::now();
        backend
            .send_prompt(conversation.id.clone(), (*message).into())
            .await?;
        turn_timings_ms.push(turn_started.elapsed().as_millis());
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
    let reopen_started = std::time::Instant::now();
    reopened.connect_agent(target, settings.cwd).await?;
    let reopen_ms = reopen_started.elapsed().as_millis();
    reopened
        .start_conversation(None, Some(conversation.id.clone()))
        .await?;
    let turn_started = std::time::Instant::now();
    reopened
        .send_prompt(conversation.id.clone(), "Reprenons ma formation Go.".into())
        .await?;
    turn_timings_ms.push(turn_started.elapsed().as_millis());
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
    let report: Value = json!({"adapter":initialized["agentInfo"],"language":language,"speed":speed,"timings":{"coldConnectMs":cold_start_ms,"tutorContextMs":context_ms,"reopenedConnectMs":reopen_ms,"promptRoundTripsMs":turn_timings_ms,"humanWaitIncluded":false},"realTutorContext":true,"liveModel":true,"personalFormationsCreated":3,"explicitContext":true,"goPythonAstronomyGo":true,"activityToolCalled":true,"activityPresented":true,"observationRecorded":false,"replyPersisted":true,"restartPassed":true,"duplicateMessages":false,"tools":tools});
    std::fs::write(
        directory.join("acceptance.json"),
        serde_json::to_vec_pretty(&report).unwrap(),
    )
    .map_err(|_| "Rapport non enregistré.")?;
    println!("{}", report);
    Ok(())
}
