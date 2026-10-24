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
    workspace::{LearningIntent, Settings, TutorSettings},
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
    let state = backend
        .save_intent(LearningIntent {
            title: "Comprendre les saisons".into(),
            goals: "Expliquer simplement l’inclinaison de la Terre".into(),
            constraints: "Une activité courte ; ne rien publier".into(),
            validated: true,
            ..Default::default()
        })
        .await?;
    if !live {
        backend.shutdown().await;
        println!(
            "{}",
            json!({"adapter":initialized["agentInfo"],"realTutorContext":true,"intentPersisted":true,"liveModel":false})
        );
        return Ok(());
    }
    let intent_id = state["intents"][0]["id"]
        .as_str()
        .ok_or("Intention non enregistrée.")?
        .to_string();
    let conversation = backend.start_conversation(Some(intent_id), None).await?;
    let (tools_tx, tools_rx) = tokio::sync::oneshot::channel();
    let permission_backend = backend.clone();
    let event_task = tokio::spawn(async move {
        let mut tools = Vec::<String>::new();
        while let Some(event) = events.recv().await {
            if event["method"] == "session/request_permission" {
                // Denial is the acceptance default. The smoke must not grant
                // general shell/filesystem rights to make a test appear green.
                let _ = permission_backend
                    .answer_permission(event["id"].clone(), None)
                    .await;
            }
            if event["method"] == "session/update"
                && [Some("tool_call"), Some("tool_call_update")]
                    .contains(&event["params"]["update"]["sessionUpdate"].as_str())
            {
                if let Some(title) = event["params"]["update"]["title"].as_str() {
                    tools.push(title.to_string());
                }
            }
            if event["_smokeDone"] == true {
                break;
            }
        }
        let _ = tools_tx.send(tools);
    });
    let first=backend.send_prompt(conversation.id.clone(),"Ceci est un essai technique dans un profil neuf. Consulte réellement get_learner_context sur le serveur MCP tutor. Je valide explicitement la création d’un domaine personnel nommé « Saisons — essai client », avec un seul concept « Inclinaison terrestre », aucune publication, aucune réponse évaluée. Enregistre ce domaine avec init_domain, puis ouvre une séance pédagogique avec start_learning_session. Utilise des clés d’idempotence pour cet essai. Réponds en une phrase avec les identifiants retournés, sans inventer de résultat et sans utiliser le shell.".into()).await;
    first?;
    let after = backend.tutor_overview().await?;
    if after["context"]["domains"]
        .as_array()
        .is_none_or(|domains| domains.is_empty())
    {
        return Err("L’agent n’a enregistré aucune formation réelle dans Tutor.".into());
    }
    let domains = after["context"]["domains"].as_array().unwrap();
    if domains.len() != 1
        || domains[0]["name"] != "Saisons — essai client"
        || domains[0]["concept_count"] != 1
    {
        return Err("La formation enregistrée ne correspond pas à l’intention validée.".into());
    }
    let learning_session = after["context"]["session_id"]
        .as_str()
        .filter(|id| !id.is_empty())
        .ok_or("L’agent n’a ouvert aucune séance pédagogique réelle.")?
        .to_string();
    backend.send_prompt(conversation.id.clone(),"Dans la séance pédagogique que tu viens d’ouvrir, appelle réellement get_next_activity pour le domaine créé. Utilise cette recommandation pour me proposer une première activité courte sur l’inclinaison terrestre. Pose une seule question et attends ma réponse. Ne soumets aucune réponse en mon nom, ne modifie pas mes objectifs, n’utilise pas le shell et ne prétends pas avoir observé un apprentissage.".into()).await?;
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
    let messages = backend.snapshot().await["conversations"][0]["messages"]
        .as_array()
        .cloned()
        .ok_or("Conversation non persistée.")?;
    if messages.len() != 4 || messages[3]["text"].as_str().is_none_or(str::is_empty) {
        return Err("Réponse réelle non conservée.".into());
    }
    backend.shutdown().await;
    let reopened = Backend::new(&directory.join("client"), Arc::new(|_, _| {}))?;
    reopened.connect_agent(target, settings.cwd).await?;
    reopened
        .start_conversation(None, Some(conversation.id.clone()))
        .await?;
    let resumed = reopened.tutor_overview().await?;
    if resumed["context"]["domains"] != after["context"]["domains"] {
        return Err("Le contexte pédagogique n’a pas été retrouvé.".into());
    }
    if resumed["context"]["session_id"] != learning_session {
        return Err("La séance pédagogique n’a pas été retrouvée après redémarrage.".into());
    }
    let persisted = reopened.snapshot().await;
    if persisted["conversations"][0]["messages"]
        .as_array()
        .map(Vec::len)
        != Some(4)
    {
        return Err("La reprise a dupliqué les échanges.".into());
    }
    reopened.shutdown().await;
    let report: Value = json!({"adapter":initialized["agentInfo"],"realTutorContext":true,"liveModel":true,"formationCreated":true,"learningSessionOpened":true,"activityToolCalled":true,"activityPresented":true,"observationRecorded":false,"replyPersisted":true,"restartPassed":true,"duplicateMessages":false,"tools":tools});
    std::fs::write(
        directory.join("acceptance.json"),
        serde_json::to_vec_pretty(&report).unwrap(),
    )
    .map_err(|_| "Rapport non enregistré.")?;
    println!("{}", report);
    Ok(())
}
