use super::*;
use crate::workspace::{Message, PendingMessage, WorkspaceStore};
use serde_json::json;

fn context() -> Value {
    json!({"conversation_id":"conversation-go","training_id":"personal:go","domain_id":"go","revision":"2","selection_version":1})
}

fn card() -> Value {
    json!({"version":1,"id":"variables-1","kind":"quiz","trainingId":"personal:go","domainId":"go","concept":"Variables","question":"Quelle est la valeur de x ?","sessionId":"session-go","decisionId":"decision-1","attemptId":"attempt-1","activityType":"RECALL_EXERCISE","rubricJson":"{\"criteria\":[{\"id\":\"correct\",\"description\":\"Valeur correcte\",\"max_score\":1}],\"passing_score\":1,\"answer_key\":\"5\"}","options":[{"id":"a","text":"3"},{"id":"b","text":"5"}],"correctOptionId":"b","explanation":"x valait 3 et on ajoute 2."})
}

fn response() -> Value {
    json!({"version":1,"activityId":"variables-1","kind":"quiz","trainingId":"personal:go","domainId":"go","concept":"Variables","attemptId":"attempt-1","selectedOptionId":"b","responseTimeSeconds":7.5})
}

fn fence(tag: &str, value: &Value) -> String {
    format!("```{tag}\n{value}\n```")
}

fn conversation(card: Value) -> Conversation {
    Conversation {
        id: "conversation-go".into(),
        title: "Go".into(),
        intent_id: None,
        agent_session_id: None,
        target_id: "target".into(),
        tutor_binding: "binding".into(),
        runtime_binding: "runtime".into(),
        created_at: String::new(),
        updated_at: String::new(),
        active_training: Some(context()),
        messages: vec![Message {
            id: "assistant-1".into(),
            role: "assistant".into(),
            text: fence("tutor-activity", &card),
            created_at: String::new(),
            status: "complete".into(),
            training_context: Some(context()),
        }],
    }
}

#[test]
fn accepts_only_a_choice_from_the_complete_original_card_in_its_own_context() {
    let conversation = conversation(card());
    let response = fence("tutor-response", &response());
    assert!(validate_message(Some(&conversation), &response, Some(&context())).is_ok());
    assert!(validate_message(None, &response, None).is_err());
    let wrong_context =
        json!({"training_id":"personal:python","domain_id":"python","revision":"2"});
    assert!(validate_message(Some(&conversation), &response, Some(&wrong_context)).is_err());
    let mut revised = context();
    revised["revision"] = json!("3");
    assert!(validate_message(Some(&conversation), &response, Some(&revised)).is_err());
    let mut interrupted = conversation.clone();
    interrupted.messages[0].status = "interrupted".into();
    assert!(validate_message(Some(&interrupted), &response, Some(&context())).is_err());
    let mut duplicate_card = conversation.clone();
    duplicate_card
        .messages
        .push(conversation.messages[0].clone());
    assert!(validate_message(Some(&duplicate_card), &response, Some(&context())).is_err());
}

#[test]
fn prepared_quizzes_keep_the_exact_grading_contract_across_session_restarts() {
    let message = fence("tutor-response", &response());
    for field in ["activityType", "rubricJson"] {
        let mut invalid = card();
        invalid.as_object_mut().unwrap().remove(field);
        assert!(
            validate_message(Some(&conversation(invalid)), &message, Some(&context())).is_err()
        );
    }
    for (field, value) in [
        ("activityType", json!("INVENTED_TYPE")),
        ("rubricJson", json!("[]")),
        ("rubricJson", json!("not JSON")),
    ] {
        let mut invalid = card();
        invalid[field] = value;
        assert!(
            validate_message(Some(&conversation(invalid)), &message, Some(&context())).is_err()
        );
    }
    let original = card();
    let instructions = prompt(&conversation(original.clone()), &message);
    assert!(instructions.contains("activity_type EXACTEMENT égal à activityType"));
    assert!(instructions.contains("success et confidence restent requis"));
    assert!(instructions.contains(&serde_json::to_string(&original["rubricJson"]).unwrap()));
    assert!(instructions.contains("ne prépare aucune nouvelle tentative"));
}

#[test]
fn transfer_probe_requires_a_preserved_dimension_and_other_quizzes_reject_it() {
    let message = fence("tutor-response", &response());
    let mut transfer = card();
    transfer["activityType"] = json!("TRANSFER_PROBE");
    assert!(validate_message(
        Some(&conversation(transfer.clone())),
        &message,
        Some(&context())
    )
    .is_err());
    for dimension in ["near", "far", "debugging", "teaching", "creative"] {
        transfer["transferDimension"] = json!(dimension);
        assert!(validate_message(
            Some(&conversation(transfer.clone())),
            &message,
            Some(&context())
        )
        .is_ok());
    }
    transfer["transferDimension"] = json!("unknown");
    assert!(validate_message(
        Some(&conversation(transfer.clone())),
        &message,
        Some(&context())
    )
    .is_err());
    transfer["transferDimension"] = json!("near");
    transfer["activityType"] = json!("RECALL_EXERCISE");
    assert!(validate_message(Some(&conversation(transfer)), &message, Some(&context())).is_err());
}

#[test]
fn rejects_mismatched_ids_forged_options_and_mixed_self_reports() {
    let conversation = conversation(card());
    for (key, value) in [
        ("activityId", json!("unknown")),
        ("domainId", json!("python")),
        ("trainingId", json!("personal:python")),
        ("concept", json!("Loops")),
        ("attemptId", json!("other-attempt")),
        ("selectedOptionId", json!("not-an-option")),
        ("rating", json!("easy")),
        ("responseTimeSeconds", json!(-1)),
        ("responseTimeSeconds", json!(86401)),
        ("success", json!(true)),
        ("attemptId", Value::Null),
    ] {
        let mut invalid = response();
        invalid[key] = value;
        assert!(
            validate_message(
                Some(&conversation),
                &fence("tutor-response", &invalid),
                Some(&context())
            )
            .is_err(),
            "accepted {invalid}"
        );
    }
    let mut invalid = response();
    invalid.as_object_mut().unwrap().remove("attemptId");
    assert!(validate_message(
        Some(&conversation),
        &fence("tutor-response", &invalid),
        Some(&context())
    )
    .is_err());
}

#[test]
fn rejects_invalid_or_ambiguous_protocol_without_affecting_normal_messages() {
    let chat = conversation(card());
    let valid = fence("tutor-response", &response());
    for invalid in [
        format!("{valid}\n{valid}"),
        "```tutor-response\n{\"version\":1}".into(),
        "```tutor-response\n[]\n```".into(),
        format!(
            "{valid}\n{}",
            fence(
                "tutor-study-request",
                &json!({"version":1,"format":"quiz","intent":"practice"})
            )
        ),
    ] {
        assert!(validate_message(Some(&chat), &invalid, Some(&context())).is_err());
    }
    assert!(validate_message(
        None,
        "Explique-moi les variables.\n```go\nx := 1\n```",
        None
    )
    .is_ok());
    let mut oversized = card();
    oversized["question"] = json!("é".repeat(6001));
    assert!(validate_message(Some(&conversation(oversized)), &valid, Some(&context())).is_err());
}

#[test]
fn matches_renderer_fences_and_ignores_quoted_examples() {
    let chat = conversation(card());
    let response = response();
    for encoded in [
        format!("~~~TUTOR-RESPONSE\n{response}\n~~~"),
        format!("   ```` tutor-response\r\n{response}\r\n   `````"),
        format!(
            "```text\n```tutor-response\nnot json\n```\n{}",
            fence("tutor-response", &response)
        ),
    ] {
        assert!(
            validate_message(Some(&chat), &encoded, Some(&context())).is_ok(),
            "{encoded}"
        );
    }
    let only_example = "````markdown\n```tutor-response\nnot json\n```\n````";
    assert!(validate_message(None, only_example, None).is_ok());
    assert!(payload::<Response>(only_example, "tutor-response")
        .unwrap()
        .is_none());
}

#[test]
fn a_committed_outbox_response_cannot_be_replaced_by_another_click() {
    let message = fence("tutor-response", &response());
    let pending = PendingMessage {
        text: message.clone(),
        conversation_id: Some("go".into()),
    };
    assert!(validate_outbox(Some(&pending), &message, Some("go")).is_ok());
    let mut changed = response();
    changed["selectedOptionId"] = json!("a");
    assert!(validate_outbox(
        Some(&pending),
        &fence("tutor-response", &changed),
        Some("go")
    )
    .is_err());
    assert!(validate_outbox(Some(&pending), &message, Some("python")).is_err());
    assert!(validate_outbox(Some(&pending), "Une autre question", Some("go")).is_err());
}

#[test]
fn flashcards_are_self_reports_without_assessment_attempts() {
    let mut flashcard = card();
    for key in [
        "attemptId",
        "activityType",
        "rubricJson",
        "options",
        "correctOptionId",
        "explanation",
    ] {
        flashcard.as_object_mut().unwrap().remove(key);
    }
    flashcard["kind"] = json!("flashcard");
    flashcard["answer"] = json!("Yesterday");
    let conversation = conversation(flashcard);
    let mut rating = response();
    for key in ["attemptId", "selectedOptionId"] {
        rating.as_object_mut().unwrap().remove(key);
    }
    rating["kind"] = json!("flashcard");
    for choice in ["again", "hard", "easy"] {
        rating["rating"] = json!(choice);
        let message = fence("tutor-response", &rating);
        assert!(validate_message(Some(&conversation), &message, Some(&context())).is_ok());
        let instructions = prompt(&conversation, &message);
        assert!(instructions.contains("AUTOÉVALUATION"));
        assert!(instructions.contains("sans attempt_id"));
        assert!(instructions.contains("confidence=0.4"));
        assert!(instructions.contains("n’appelle PAS get_next_activity"));
        assert!(instructions.contains("acp-"));
    }
    rating["attemptId"] = json!("invented-proof");
    assert!(validate_message(
        Some(&conversation),
        &fence("tutor-response", &rating),
        Some(&context())
    )
    .is_err());
}

#[test]
fn queued_and_answered_cards_survive_reload_without_duplicate_acceptance() {
    let dir = tempfile::tempdir().unwrap();
    let mut store = WorkspaceStore::load(dir.path()).unwrap();
    let message = fence("tutor-response", &response());
    store
        .transaction(|data| {
            data.conversations.push(conversation(card()));
            data.pending_message = Some(PendingMessage {
                text: message.clone(),
                conversation_id: Some("conversation-go".into()),
            });
            data.settings.review_plans.insert(
                "profile|institution|go".into(),
                ReviewPlan {
                    enabled: true,
                    format: LearningFormat::Quiz,
                },
            );
            Ok(())
        })
        .unwrap();
    drop(store);
    let mut recovered = WorkspaceStore::load(dir.path()).unwrap();
    assert!(recovered.data.settings.review_plans["profile|institution|go"].enabled);
    let pending = recovered.data.pending_message.as_ref().unwrap();
    assert_eq!(pending.text, message);
    assert!(validate_message(
        Some(&recovered.data.conversations[0]),
        &pending.text,
        Some(&context())
    )
    .is_ok());
    recovered
        .transaction(|data| {
            data.pending_message = None;
            data.conversations[0].messages.push(Message {
                id: "response-1".into(),
                role: "user".into(),
                text: message.clone(),
                created_at: String::new(),
                status: "complete".into(),
                training_context: Some(context()),
            });
            Ok(())
        })
        .unwrap();
    drop(recovered);
    let answered = WorkspaceStore::load(dir.path()).unwrap();
    assert!(validate_message(
        Some(&answered.data.conversations[0]),
        &message,
        Some(&context())
    )
    .unwrap_err()
    .contains("déjà"));
}

#[test]
fn review_request_is_explicitly_scoped_and_prompt_freezes_quiz_before_answer() {
    let conversation = conversation(card());
    let request = fence(
        "tutor-study-request",
        &json!({"version":1,"format":"quiz","intent":"review","trainingId":"personal:go","domainId":"go"}),
    );
    assert!(validate_message(Some(&conversation), &request, Some(&context())).is_ok());
    assert!(validate_message(
        Some(&conversation),
        &request,
        Some(&json!({"training_id":"personal:python","domain_id":"python"}))
    )
    .is_err());
    let instructions = prompt(&conversation, &request);
    assert!(instructions.contains("intent=review"));
    assert!(instructions.contains("AVANT affichage"));
    assert!(instructions.contains("prepare_assessment_attempt"));
    assert!(instructions.contains("ne fabrique"));
    assert!(instructions.contains("exactement UNE activité"));
    let without_training = fence(
        "tutor-study-request",
        &json!({"version":1,"format":"flashcard","intent":"practice"}),
    );
    assert!(validate_message(None, &without_training, None).is_ok());
    assert!(prompt(&conversation, &without_training)
        .contains("ne choisis jamais le dernier domaine global"));
}

#[test]
fn review_requests_need_a_training_but_allow_long_human_readable_names() {
    let mut request = json!({"version":1,"format":"quiz","intent":"review","trainingId":"personal:go","trainingName":"é".repeat(500)});
    assert!(validate_message(None, &fence("tutor-study-request", &request), None).is_ok());
    request["trainingName"] = json!("é".repeat(501));
    assert!(validate_message(None, &fence("tutor-study-request", &request), None).is_err());
    request.as_object_mut().unwrap().remove("trainingName");
    request.as_object_mut().unwrap().remove("trainingId");
    assert!(validate_message(None, &fence("tutor-study-request", &request), None).is_err());
}

#[test]
fn withdrawn_receipt_uses_account_language_and_is_not_duplicated() {
    let mut data = Workspace::default();
    data.settings.language = "en".into();
    data.conversations.push(conversation(card()));
    data.pending_message = Some(PendingMessage {
        text: fence("tutor-response", &response()),
        conversation_id: Some("conversation-go".into()),
    });
    archive_withdrawn_response(&mut data).unwrap();
    archive_withdrawn_response(&mut data).unwrap();
    assert_eq!(data.conversations[0].messages.len(), 2);
    assert!(data.conversations[0].messages[1]
        .text
        .starts_with("Answer saved on this device without sending it to the tutor."));
}
