// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

use super::*;

fn rubric() -> Value {
    json!({"criteria":[{"id":"equivalence","description":"Preserve equality","max_score":1}],"passing_score":1,"answer_key":"PRIVATE_EQUATION_SOLUTION"})
}

fn packet(presentation: &str, activity_type: &str) -> Value {
    json!({
        "schema_version":"pedagogical-turn/v1","mode":"activity","needs_selection":false,
        "presentation":presentation,"permissions":{"read":true,"prepare_activity":true},
        "conversation":{"conversation_id":"unused","training_id":"personal:algebra","domain_id":"algebra","revision":"2","selection_version":4,"name":"Algèbre"},
        "training":{"id":"personal:algebra","domain_id":"algebra","name":"Algèbre"},
        "decision":{"activity":{"type":activity_type,"concept":"equations"},"session_id":"session-algebra","pedagogical_contract":{"decision_id":"decision-algebra","competency":{"outcomes":[]}}},
        "attempt":{"id":"attempt-1","domain_id":"algebra","concept":"equations","status":"prepared","rubric":rubric()}
    })
}

fn write_fixture(directory: &Path, packet: &Value, output: &str) {
    std::fs::write(
        directory.join("fixture-pedagogy.json"),
        json!({"resumeSchema":true,"packet":packet,"output":output}).to_string(),
    )
    .unwrap();
}

fn envelope(tag: &str, value: &Value) -> String {
    format!("```{tag}\n{value}\n```")
}

fn resume_button() -> String {
    envelope(
        "tutor-resume-request",
        &json!({"version":1,"trainingId":"personal:algebra","domainId":"algebra","trainingName":"Algèbre"}),
    )
}

#[tokio::test]
async fn queued_native_resume_opens_one_isolated_session_without_immediate_reload() {
    for by_button in [true, false] {
        let directory = tempfile::tempdir().unwrap();
        let introduction = json!({"version":1,"trainingId":"personal:algebra","domainId":"algebra","concept":"equations","sessionId":"session-algebra","decisionId":"decision-algebra","body":"Une même transformation conserve les deux membres de l’égalité."});
        write_fixture(
            directory.path(),
            &packet("instruction", "NEW_CONCEPT"),
            &envelope("tutor-introduction", &introduction),
        );
        let (backend, mut events) = connected(directory.path()).await;
        let text = if by_button {
            resume_button()
        } else {
            "Reprendre la formation Algèbre.".into()
        };
        backend
            .queue_prompt(Some(text.clone()), None)
            .await
            .unwrap();
        let conversation = backend.start_conversation(None, None).await.unwrap();
        // The upcoming native formulation runs in the private session: no MCP
        // session is opened for it, and creation has not selected, prepared
        // or graded anything server-side.
        assert!(conversation.agent_session_id.is_none());
        assert!(!latency_requests(directory.path())
            .iter()
            .any(|row| row["method"] == "session/new"));
        assert!(calls(directory.path()).is_empty());
        send_with_consent(&backend, &mut events, &conversation.id, text)
            .await
            .unwrap();
        let records = latency_requests(directory.path());
        for (method, expected) in [
            ("session/new", 1),
            ("session/load", 0),
            ("session/prompt", 1),
        ] {
            assert_eq!(
                records.iter().filter(|row| row["method"] == method).count(),
                expected,
                "{method}, button={by_button}"
            );
        }
        let sessions: Value = serde_json::from_slice(
            &std::fs::read(directory.path().join("fixture-agent-sessions.json")).unwrap(),
        )
        .unwrap();
        assert_eq!(
            sessions[private_session_of(&backend, &conversation.id).await]["mcpServers"],
            json!([])
        );
        assert_eq!(
            calls(directory.path())[0]["name"],
            "prepare_pedagogical_turn"
        );
        let state = backend.snapshot().await;
        let current = &state["conversations"][0];
        assert_eq!(current["messages"][1]["text"], introduction["body"]);
        let title = current["title"].as_str().unwrap();
        assert!(
            title.contains("Algèbre") && !title.contains("tutor-resume-request"),
            "{title}"
        );
        backend.shutdown().await;
    }
}

#[tokio::test]
async fn queued_free_message_keeps_mcp_available_without_reloading_its_new_session() {
    let directory = tempfile::tempdir().unwrap();
    write_fixture(
        directory.path(),
        &packet("instruction", "NEW_CONCEPT"),
        "Une explication contextuelle.",
    );
    let (backend, mut events) = connected(directory.path()).await;
    let text = "Explique-moi le rôle du serveur MCP.".to_owned();
    backend
        .queue_prompt(Some(text.clone()), None)
        .await
        .unwrap();
    let conversation = backend.start_conversation(None, None).await.unwrap();
    let sessions: Value = serde_json::from_slice(
        &std::fs::read(directory.path().join("fixture-agent-sessions.json")).unwrap(),
    )
    .unwrap();
    assert!(
        !sessions[conversation.agent_session_id.as_deref().unwrap()]["mcpServers"]
            .as_array()
            .unwrap()
            .is_empty()
    );
    send_with_consent(&backend, &mut events, &conversation.id, text)
        .await
        .unwrap();
    let records = latency_requests(directory.path());
    for (method, expected) in [
        ("session/new", 1),
        ("session/load", 0),
        ("session/prompt", 1),
    ] {
        assert_eq!(
            records.iter().filter(|row| row["method"] == method).count(),
            expected,
            "{method}"
        );
    }
    assert_eq!(calls(directory.path())[0]["arguments"]["mode"], "context");
    backend.shutdown().await;
}

fn calls(directory: &Path) -> Vec<Value> {
    std::fs::read_to_string(directory.join("fixture-pedagogy-log.jsonl"))
        .unwrap_or_default()
        .lines()
        .map(|line| serde_json::from_str(line).unwrap())
        .collect()
}

async fn permit_events(backend: &Backend, event: Value) {
    if event["value"]["type"] == "native_permission" {
        backend
            .answer_permission(event["value"]["id"].clone(), Some("allow_once".into()))
            .await
            .unwrap();
    }
}

async fn send_with_consent(
    backend: &Arc<Backend>,
    events: &mut tokio::sync::mpsc::UnboundedReceiver<Value>,
    conversation_id: &str,
    text: String,
) -> Result<Value, String> {
    let sender = backend.clone();
    let id = conversation_id.to_owned();
    let mut task = tokio::spawn(async move { sender.send_prompt(id, text).await });
    tokio::time::timeout(std::time::Duration::from_secs(10), async {
        loop {
            tokio::select! {
                result = &mut task => break result.unwrap(),
                event = events.recv() => permit_events(backend, event.unwrap()).await,
            }
        }
    })
    .await
    .expect("native fixture must complete without an unanswered consent")
}

async fn select_previous_training(backend: &Backend, conversation_id: &str) -> Value {
    let context = json!({"conversation_id":conversation_id,"training_id":"personal:python","domain_id":"python","revision":"old-revision","selection_version":3,"name":"Python"});
    backend
        .store
        .lock()
        .await
        .transaction(|data| {
            data.conversations[0].active_training = Some(context.clone());
            Ok(())
        })
        .unwrap();
    context
}

#[tokio::test]
async fn button_and_text_resume_select_natively_before_one_isolated_formulation() {
    for by_button in [true, false] {
        let directory = tempfile::tempdir().unwrap();
        let packet = packet("instruction", "NEW_CONCEPT");
        let introduction = json!({"version":1,"trainingId":"personal:algebra","domainId":"algebra","concept":"equations","sessionId":"session-algebra","decisionId":"decision-algebra","body":"Une égalité reste vraie si les deux membres subissent la même opération."});
        write_fixture(
            directory.path(),
            &packet,
            &envelope("tutor-introduction", &introduction),
        );
        let (backend, mut events) = connected(directory.path()).await;
        let conversation = backend.start_conversation(None, None).await.unwrap();
        select_previous_training(&backend, &conversation.id).await;
        let text = if by_button {
            resume_button()
        } else {
            "Reprendre la formation Algèbre.".into()
        };
        backend
            .queue_prompt(Some(text.clone()), Some(conversation.id.clone()))
            .await
            .unwrap();
        let resumed = backend
            .start_conversation(None, Some(conversation.id.clone()))
            .await
            .unwrap();
        assert_eq!(resumed.agent_session_id, conversation.agent_session_id);
        assert_eq!(
            send_with_consent(&backend, &mut events, &conversation.id, text)
                .await
                .unwrap()["stopReason"],
            "end_turn"
        );

        let calls = calls(directory.path());
        assert_eq!(
            calls.len(),
            1,
            "introduction must not prepare or grade an assessment"
        );
        let request = &calls[0];
        assert_eq!(request["name"], "prepare_pedagogical_turn");
        assert_eq!(request["arguments"]["mode"], "activity");
        assert_eq!(request["arguments"]["intent"], "auto");
        assert_eq!(request["arguments"]["expected_selection"], 3);
        assert_eq!(request["arguments"]["expected_revision"], "old-revision");
        assert!(request["arguments"].get("format").is_none());
        let target = if by_button {
            "training_id"
        } else {
            "training_name"
        };
        assert_eq!(
            request["arguments"][target],
            if by_button {
                "personal:algebra"
            } else {
                "Algèbre"
            }
        );
        assert!(request["arguments"]["idempotency_key"]
            .as_str()
            .is_some_and(|key| !key.is_empty()));
        let records = latency_requests(directory.path());
        // The native formulation never reaches the MCP grants: it runs in the
        // conversation's private session (one more session/new, no reload).
        assert_eq!(
            records
                .iter()
                .filter(|row| row["method"] == "session/new")
                .count(),
            2
        );
        assert_eq!(
            records
                .iter()
                .filter(|row| row["method"] == "session/load")
                .count(),
            0,
            "alternating sessions are never reloaded"
        );
        let prepare = records
            .iter()
            .position(|row| row["tool"] == "prepare_pedagogical_turn")
            .unwrap();
        let prompts: Vec<_> = records
            .iter()
            .enumerate()
            .filter(|(_, row)| row["method"] == "session/prompt")
            .collect();
        assert_eq!(prompts.len(), 1);
        assert!(prepare < prompts[0].0);
        assert!(!records
            .iter()
            .any(|row| row["tool"] == "select_conversation_training"));
        // One canonical activity call; no separate get_next_activity anymore.
        assert_eq!(
            records
                .iter()
                .filter(|row| row["tool"] == "prepare_pedagogical_turn")
                .count(),
            1
        );

        let state = backend.snapshot().await;
        let current = &state["conversations"][0];
        assert_eq!(current["activeTraining"]["training_id"], "personal:algebra");
        let published = &current["messages"][1];
        assert_eq!(published["status"], "complete");
        assert_eq!(published["text"], introduction["body"]);
        assert_eq!(published["learningDelivery"]["kind"], "instruction");
        assert_eq!(published["learningDelivery"]["recorded"], false);
        assert!(!published.to_string().contains("tutor-introduction"));
        let sessions: Value = serde_json::from_slice(
            &std::fs::read(directory.path().join("fixture-agent-sessions.json")).unwrap(),
        )
        .unwrap();
        assert_eq!(
            sessions[current["agentSessionId"].as_str().unwrap()]["mcpServers"][0]["name"],
            "tutor"
        );
        assert_eq!(
            sessions[private_session_of(&backend, &conversation.id).await]["mcpServers"],
            json!([])
        );

        for _ in 0..2 {
            backend
                .acknowledge_learning_delivery(
                    conversation.id.clone(),
                    published["id"].as_str().unwrap().into(),
                )
                .await
                .unwrap();
        }
        let deliveries = self::calls(directory.path());
        assert_eq!(
            deliveries
                .iter()
                .filter(|row| row["name"] == "record_learning_event")
                .count(),
            1
        );
        assert!(!deliveries
            .iter()
            .any(|row| row["name"] == "record_interaction"));
        backend.shutdown().await;
    }
}

#[tokio::test]
async fn ambiguous_or_missing_training_returns_server_choices_without_a_harness_prompt() {
    for with_choices in [true, false] {
        let directory = tempfile::tempdir().unwrap();
        let mut packet = packet("assessment", "PRACTICE");
        packet["needs_selection"] = json!(true);
        packet["selection_reason"] = json!(if with_choices {
            "ambiguous"
        } else {
            "not_found"
        });
        packet["decision"] = Value::Null;
        packet["training_choices"] = if with_choices {
            json!([
                {"id":"personal:algebra","name":"Algèbre","domain_id":"algebra"},
                {"id":"personal:algebra-2","name":"Algèbre avancée","domain_id":"algebra-2"}
            ])
        } else {
            json!([])
        };
        packet["training_choices_has_more"] = json!(false);
        write_fixture(directory.path(), &packet, "MUST_NOT_GENERATE");
        let (backend, mut events) = connected(directory.path()).await;
        let conversation = backend.start_conversation(None, None).await.unwrap();
        let previous = select_previous_training(&backend, &conversation.id).await;
        packet["conversation"] = previous.clone();
        write_fixture(directory.path(), &packet, "MUST_NOT_GENERATE");
        send_with_consent(
            &backend,
            &mut events,
            &conversation.id,
            "Reprendre la formation Algèbre.".into(),
        )
        .await
        .unwrap();
        assert!(!latency_requests(directory.path())
            .iter()
            .any(|row| row["method"] == "session/prompt"));
        assert_eq!(calls(directory.path()).len(), 1);
        let state = backend.snapshot().await;
        assert_eq!(state["conversations"][0]["activeTraining"], previous);
        let message = &state["conversations"][0]["messages"][1];
        assert_eq!(message["status"], "complete");
        assert!(message["learningDelivery"].is_null());
        assert!(!message.to_string().contains("MUST_NOT_GENERATE"));
        let choice: Option<Value> =
            crate::learning_ui::payload(message["text"].as_str().unwrap(), "tutor-training-choice")
                .unwrap();
        if with_choices {
            let choice = choice.unwrap();
            assert_eq!(choice["choices"].as_array().unwrap().len(), 2);
            assert_eq!(choice["choices"][0]["trainingId"], "personal:algebra");
            assert_eq!(choice["requestText"], "Reprendre la formation Algèbre.");
        } else {
            assert!(choice.is_none());
            assert!(message["text"]
                .as_str()
                .unwrap()
                .contains("Ouvrez Formations"));
        }
        backend.shutdown().await;
    }
}

#[tokio::test]
async fn old_prepare_schema_blocks_resume_before_any_selection_or_mutation_and_keeps_outbox() {
    for missing in ["training_id", "training_name"] {
        let directory = tempfile::tempdir().unwrap();
        std::fs::write(directory.path().join("fixture-pedagogy.json"), json!({"resumeSchema":true,"omittedResumeFields":[missing],"packet":packet("instruction","NEW_CONCEPT"),"output":"MUST_NOT_GENERATE"}).to_string()).unwrap();
        let (backend, mut events) = connected(directory.path()).await;
        let conversation = backend.start_conversation(None, None).await.unwrap();
        let previous = select_previous_training(&backend, &conversation.id).await;
        let text = resume_button();
        backend
            .queue_prompt(Some(text.clone()), Some(conversation.id.clone()))
            .await
            .unwrap();
        let error = send_with_consent(&backend, &mut events, &conversation.id, text.clone())
            .await
            .unwrap_err();
        assert!(error.contains("mis à jour"), "{error}");
        assert!(calls(directory.path()).is_empty());
        assert!(!latency_requests(directory.path())
            .iter()
            .any(|row| row["method"] == "session/prompt"));
        let state = backend.snapshot().await;
        assert_eq!(state["conversations"][0]["activeTraining"], previous);
        assert!(state["conversations"][0]["messages"]
            .as_array()
            .unwrap()
            .is_empty());
        assert_eq!(state["pendingMessage"]["text"], text);
        backend.shutdown().await;
    }
}

#[tokio::test]
async fn changed_selection_during_generation_prevents_instruction_exposure_and_delivery() {
    let directory = tempfile::tempdir().unwrap();
    let mut packet = packet("instruction", "NEW_CONCEPT");
    let introduction = envelope(
        "tutor-introduction",
        &json!({"version":1,"trainingId":"personal:algebra","domainId":"algebra","concept":"equations","sessionId":"session-algebra","decisionId":"decision-algebra","body":"PRIVATE_STALE_INTRODUCTION"}),
    );
    write_fixture(directory.path(), &packet, &introduction);
    let (backend, mut events) = connected(directory.path()).await;
    let conversation = backend.start_conversation(None, None).await.unwrap();
    std::fs::write(directory.path().join("fixture-hold-generation"), "hold").unwrap();
    let sender = backend.clone();
    let id = conversation.id.clone();
    let mut task = tokio::spawn(async move { sender.send_prompt(id, resume_button()).await });
    tokio::time::timeout(std::time::Duration::from_secs(10), async {
        loop {
            tokio::select! {
                result = &mut task => panic!("generation completed before fixture release: {result:?}"),
                event = events.recv() => {
                    let event=event.unwrap();
                    assert!(!event.to_string().contains("PRIVATE_STALE_INTRODUCTION"));
                    permit_events(&backend,event).await;
                },
                _ = tokio::time::sleep(std::time::Duration::from_millis(5)) => {
                    if backend.store.lock().await.data.conversations[0].messages.last().is_some_and(|message| message.text.contains("PRIVATE_STALE_INTRODUCTION")) {break;}
                }
            }
        }
    }).await.expect("fixture must stream a private introduction");
    packet["conversation"]["revision"] = json!("revised-elsewhere");
    packet["conversation"]["selection_version"] = json!(5);
    write_fixture(directory.path(), &packet, &introduction);
    std::fs::remove_file(directory.path().join("fixture-hold-generation")).unwrap();
    assert!(task.await.unwrap().is_err());
    let state = backend.snapshot().await;
    assert!(!state.to_string().contains("PRIVATE_STALE_INTRODUCTION"));
    let messages = state["conversations"][0]["messages"].as_array().unwrap();
    assert_ne!(messages.last().unwrap()["status"], "complete");
    assert!(messages.last().unwrap()["learningDelivery"].is_null());
    assert_eq!(
        calls(directory.path())
            .iter()
            .map(|row| row["name"].as_str().unwrap())
            .collect::<Vec<_>>(),
        vec!["prepare_pedagogical_turn"]
    );
    assert_eq!(
        latency_requests(directory.path())
            .iter()
            .filter(|row| row["method"] == "session/prompt")
            .count(),
        1
    );
    backend.shutdown().await;
}

#[tokio::test]
async fn exercise_is_frozen_before_display_help_is_not_submitted_and_answer_precedes_grading() {
    let directory = tempfile::tempdir().unwrap();
    let packet = packet("assessment", "PRACTICE");
    let exercise = json!({"version":1,"id":"model-exercise","kind":"exercise","trainingId":"personal:algebra","domainId":"algebra","concept":"equations","sessionId":"session-algebra","decisionId":"decision-algebra","question":"Résolvez x + 3 = 7 en écrivant une transformation.","activityType":"PRACTICE","rubricJson":rubric().to_string()});
    write_fixture(
        directory.path(),
        &packet,
        &envelope("tutor-activity", &exercise),
    );
    let (backend, mut events) = connected(directory.path()).await;
    let conversation = backend.start_conversation(None, None).await.unwrap();
    std::fs::write(directory.path().join("fixture-hold-generation"), "hold").unwrap();
    let sender = backend.clone();
    let id = conversation.id.clone();
    let task = tokio::spawn(async move { sender.send_prompt(id, resume_button()).await });
    tokio::time::timeout(std::time::Duration::from_secs(10), async {
        loop {
            tokio::select! {
                event = events.recv() => {
                    let event = event.unwrap();
                    assert!(!event.to_string().contains("PRIVATE_EQUATION_SOLUTION"));
                    permit_events(&backend, event).await;
                },
                _ = tokio::time::sleep(std::time::Duration::from_millis(5)) => {
                    if backend.store.lock().await.data.conversations[0].messages.last().is_some_and(|message| message.text.contains("PRIVATE_EQUATION_SOLUTION")) { break; }
                }
            }
        }
    }).await.expect("fixture must stream a private draft");
    assert!(!backend
        .snapshot()
        .await
        .to_string()
        .contains("PRIVATE_EQUATION_SOLUTION"));
    assert!(!directory
        .path()
        .join("fixture-prepared-attempt.json")
        .exists());
    std::fs::remove_file(directory.path().join("fixture-hold-generation")).unwrap();
    assert_eq!(task.await.unwrap().unwrap()["stopReason"], "end_turn");
    assert!(directory
        .path()
        .join("fixture-prepared-attempt.json")
        .exists());
    let state = backend.snapshot().await;
    let published = &state["conversations"][0]["messages"][1];
    let activity: Value =
        crate::learning_ui::payload(published["text"].as_str().unwrap(), "tutor-activity")
            .unwrap()
            .unwrap();
    assert_eq!(published["status"], "complete");
    assert_eq!(activity["kind"], "exercise");
    assert_eq!(activity["attemptId"], "attempt-1");
    assert!(activity.get("correctOptionId").is_none());

    write_fixture(
        directory.path(),
        &packet,
        "Un indice : appliquez la même opération aux deux membres.",
    );
    send_with_consent(
        &backend,
        &mut events,
        &conversation.id,
        "Donne-moi un indice sans donner la solution.".into(),
    )
    .await
    .unwrap();
    assert!(!directory
        .path()
        .join("fixture-submitted-attempt.json")
        .exists());
    let before_answer = calls(directory.path());
    assert_eq!(
        before_answer
            .iter()
            .filter(|row| row["name"] == "prepare_assessment_attempt")
            .count(),
        1
    );
    assert!(!before_answer
        .iter()
        .any(|row| row["name"] == "record_interaction" || row["name"] == "record_learning_event"));
    let hint_context = before_answer.last().unwrap();
    assert_eq!(hint_context["name"], "prepare_pedagogical_turn");
    assert_eq!(hint_context["arguments"]["mode"], "context");
    assert_eq!(hint_context["arguments"]["attempt_id"], "attempt-1");

    let evaluation = json!({"version":1,"attempt_id":"attempt-1","rubric_score":{"criteria_scores":[{"id":"equivalence","score":1,"max_score":1,"evidence":"Subtracted three from both sides"}],"total":1,"max_total":1,"confidence":0.8},"feedback":"Retour enregistré sur la transformation observée."});
    write_fixture(
        directory.path(),
        &packet,
        &envelope("tutor-evaluation", &evaluation),
    );
    let answer = json!({"version":1,"activityId":activity["id"],"kind":"exercise","trainingId":"personal:algebra","domainId":"algebra","concept":"equations","attemptId":"attempt-1","answer":"x + 3 - 3 = 7 - 3\nx = 4","responseTimeSeconds":12});
    send_with_consent(
        &backend,
        &mut events,
        &conversation.id,
        envelope("tutor-response", &answer),
    )
    .await
    .unwrap();
    let submitted: Value = serde_json::from_slice(
        &std::fs::read(directory.path().join("fixture-submitted-attempt.json")).unwrap(),
    )
    .unwrap();
    assert_eq!(submitted["learner_response"], answer["answer"]);
    let records = latency_requests(directory.path());
    let prompts: Vec<_> = records
        .iter()
        .enumerate()
        .filter(|(_, row)| row["method"] == "session/prompt")
        .collect();
    assert_eq!(
        prompts.len(),
        3,
        "resume, help, explicit answer each have one formulation"
    );
    let submit = records
        .iter()
        .position(|row| row["tool"] == "submit_assessment_attempt")
        .unwrap();
    let grade = records
        .iter()
        .position(|row| row["tool"] == "record_interaction")
        .unwrap();
    assert!(prompts[1].0 < submit && submit < prompts[2].0 && prompts[2].0 < grade);
    let state = backend.snapshot().await;
    let feedback = state["conversations"][0]["messages"]
        .as_array()
        .unwrap()
        .last()
        .unwrap();
    assert_eq!(feedback["text"], evaluation["feedback"]);
    assert_eq!(feedback["status"], "complete");
    assert_eq!(feedback["learningDelivery"]["kind"], "feedback");
    assert_eq!(
        calls(directory.path())
            .iter()
            .filter(|row| row["name"] == "record_interaction")
            .count(),
        1
    );
    backend.shutdown().await;
}

#[tokio::test]
async fn alternating_private_and_mcp_turns_keep_two_stable_sessions_without_reload() {
    let directory = tempfile::tempdir().unwrap();
    let introduction = json!({"version":1,"trainingId":"personal:algebra","domainId":"algebra","concept":"equations","sessionId":"session-algebra","decisionId":"decision-algebra","body":"UNIQUE_PRIVATE_INTRODUCTION"});
    let private_output = envelope("tutor-introduction", &introduction);
    let packet = packet("instruction", "NEW_CONCEPT");
    let (backend, mut events) = connected(directory.path()).await;
    // Closing replaced sessions requires the advertised capability.
    backend.agent.lock().await.as_mut().unwrap().capabilities["sessionCapabilities"] =
        json!({"close":{}});
    let conversation = backend.start_conversation(None, None).await.unwrap();
    let mcp_session = conversation.agent_session_id.clone().unwrap();
    std::fs::write(directory.path().join("fixture-rpc.jsonl"), "").unwrap();
    for (turn, private) in [true, false, true, false].into_iter().enumerate() {
        let text = if private {
            write_fixture(directory.path(), &packet, &private_output);
            resume_button()
        } else {
            write_fixture(directory.path(), &packet, "Une explication contextuelle.");
            format!("Explique encore, tour {turn}.")
        };
        assert_eq!(
            send_with_consent(&backend, &mut events, &conversation.id, text)
                .await
                .unwrap()["stopReason"],
            "end_turn"
        );
    }
    let records = latency_requests(directory.path());
    let count = |method: &str| records.iter().filter(|row| row["method"] == method).count();
    assert_eq!(
        count("session/new"),
        1,
        "only the private session is created"
    );
    assert_eq!(count("session/load"), 0);
    assert_eq!(count("session/prompt"), 4);
    let private_session = private_session_of(&backend, &conversation.id).await;
    let sessions: Value = serde_json::from_slice(
        &std::fs::read(directory.path().join("fixture-agent-sessions.json")).unwrap(),
    )
    .unwrap();
    assert_eq!(sessions[&mcp_session]["promptCount"], 2);
    assert_eq!(sessions[&private_session]["promptCount"], 2);
    assert_eq!(sessions[&mcp_session]["mcpServers"][0]["name"], "tutor");
    assert_eq!(sessions[&private_session]["mcpServers"], json!([]));
    // The MCP session missed the private turn: its next prompt carries it.
    assert!(sessions[&mcp_session]["lastPrompt"]
        .as_str()
        .unwrap()
        .contains("UNIQUE_PRIVATE_INTRODUCTION"));
    assert!(backend
        .store
        .lock()
        .await
        .data
        .stale_agent_sessions
        .is_empty());
    // A private turn in another conversation retires this one's private
    // session (one per process) with session/close.
    let other = backend.start_conversation(None, None).await.unwrap();
    write_fixture(directory.path(), &packet, &private_output);
    send_with_consent(&backend, &mut events, &other.id, resume_button())
        .await
        .unwrap();
    tokio::time::timeout(std::time::Duration::from_secs(5), async {
        while !latency_requests(directory.path()).iter().any(|row| {
            row["method"] == "session/close" && row["sessionId"] == private_session.as_str()
        }) {
            tokio::time::sleep(std::time::Duration::from_millis(5)).await;
        }
    })
    .await
    .unwrap();
    let agent = backend.agent.lock().await;
    let registry = &agent.as_ref().unwrap().private_sessions;
    assert_eq!(registry.len(), 1);
    assert!(registry.contains_key(&other.id));
    drop(agent);
    // The first conversation's MCP session is marked as having missed nothing
    // (its last turn was MCP); the other conversation's MCP session missed the
    // private turn, durably.
    let stale = backend.store.lock().await.data.stale_agent_sessions.clone();
    assert!(stale.contains(other.agent_session_id.as_ref().unwrap()));
    assert!(!stale.contains(&mcp_session));
    backend.shutdown().await;
}
