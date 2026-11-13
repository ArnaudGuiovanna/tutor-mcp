use super::*;
use crate::workspace::{Message, PendingMessage, WorkspaceStore};
use serde_json::json;

fn turn_packet(mode: &str) -> Value {
    json!({"schema_version":"pedagogical-turn/v1","mode":mode,"conversation":context(),"permissions":{"read":true,"prepare_activity":true},
        "decision":{"activity":{"type":"RECALL_EXERCISE","concept":"Variables"},"session_id":"session-go","pedagogical_contract":{"decision_id":"decision-1","competency":{"outcomes":[]}}},
        "attempt":{"id":"attempt-1","domain_id":"go","concept":"Variables","status":"prepared","rubric":serde_json::from_str::<Value>(card()["rubricJson"].as_str().unwrap()).unwrap()}})
}

fn resume_packet(activity_type: &str, presentation: &str) -> Value {
    let mut packet = turn_packet("activity");
    packet["decision"]["activity"]["type"] = json!(activity_type);
    packet["needs_selection"] = json!(false);
    packet["presentation"] = json!(presentation);
    packet["training"] = json!({"id":"personal:go","domain_id":"go","name":"Go"});
    packet
}

#[test]
fn resume_requires_server_selection_capability_and_canonical_presentation() {
    let conversation = conversation(card());
    for message in ["Reprendre la formation Go.", "```tutor-resume-request\n{\"version\":1,\"trainingId\":\"personal:go\",\"domainId\":\"go\"}\n```"] {
        let (args, tools) = PreparedTurn::request(&conversation, message).unwrap();
        assert_eq!(args["mode"], "activity");
        assert_eq!(args["intent"], "auto");
        assert_eq!(args["expected_selection"], 1);
        assert!(tools.contains(&"select_conversation_training"));
        assert!(tools.contains(&"prepare_pedagogical_turn"));
        assert!(!tools.contains(&"get_next_activity"));
        let packet = resume_packet("NEW_CONCEPT", "instruction");
        assert!(PreparedTurn::new(&conversation, message, packet.clone()).unwrap().private_output());
        let mut wrong = packet.clone();
        wrong["presentation"] = json!("assessment");
        assert!(PreparedTurn::new(&conversation, message, wrong).is_err());
        let mut wrong = packet;
        wrong["training"]["domain_id"] = json!("another");
        assert!(PreparedTurn::new(&conversation, message, wrong).is_err());
        assert!(PreparedTurn::new(&conversation, message, turn_packet("activity")).is_err());
    }
    let tool = json!({"name":"prepare_pedagogical_turn","inputSchema":{"properties":{"training_id":{},"training_name":{},"expected_selection":{},"expected_revision":{}}}});
    assert!(PreparedTurn::supports_resume(&tool));
    let mut old = tool;
    old["inputSchema"]["properties"]
        .as_object_mut()
        .unwrap()
        .remove("training_name");
    assert!(!PreparedTurn::supports_resume(&old));
}

#[test]
fn introduction_is_only_instruction_and_guidance_never_awards_progress() {
    let conversation = conversation(card());
    let turn = PreparedTurn::new(
        &conversation,
        "Reprendre",
        resume_packet("NEW_CONCEPT", "instruction"),
    )
    .unwrap();
    let mut intro = json!({"version":1,"trainingId":"personal:go","domainId":"go","concept":"Variables","sessionId":"session-go","decisionId":"decision-1","body":"Une variable conserve une valeur. Par exemple, x vaut 5."});
    let output = turn.output(&fence("tutor-introduction", &intro)).unwrap();
    assert_eq!(output.text, intro["body"]);
    assert!(output.mutation.is_none());
    let delivery = output.delivery.unwrap();
    assert_eq!(delivery.kind, "instruction");
    assert!(delivery.success.is_none() && !delivery.recorded && delivery.attempt_id.is_none());
    assert!(turn.output("Une variable conserve une valeur.").is_err());
    assert!(turn.output(&fence("tutor-activity", &card())).is_err());
    intro["decisionId"] = json!("foreign");
    assert!(turn.output(&fence("tutor-introduction", &intro)).is_err());
    intro["decisionId"] = json!("decision-1");
    let guidance = PreparedTurn::new(
        &conversation,
        "Reprendre",
        resume_packet("REST", "guidance"),
    )
    .unwrap();
    let result = guidance.output(&fence("tutor-guidance", &intro)).unwrap();
    assert!(result.mutation.is_none() && result.delivery.is_none());
}

#[test]
fn resume_choices_are_bounded_and_require_no_generation_or_learning_mutation() {
    let conversation = conversation(card());
    let mut packet = turn_packet("activity");
    packet.as_object_mut().unwrap().remove("decision");
    packet["needs_selection"] = json!(true);
    packet["selection_reason"] = json!("ambiguous");
    packet["training_choices_has_more"] = json!(false);
    packet["training_choices"] = json!([
        {"id":"a","domain_id":"a","name":"Python"},
        {"id":"b","domain_id":"b","name":"Python"}
    ]);
    let turn = PreparedTurn::new(
        &conversation,
        "Reprendre la formation Python",
        packet.clone(),
    )
    .unwrap();
    let choice: Value = payload(
        &turn
            .immediate_text("fr", "Reprendre la formation Python")
            .unwrap()
            .unwrap(),
        "tutor-training-choice",
    )
    .unwrap()
    .unwrap();
    assert_eq!(choice["choices"].as_array().unwrap().len(), 2);
    assert_eq!(choice["choices"][1]["trainingId"], "b");
    assert!(turn.submission().is_none());
    assert!(turn.output("J'ai sélectionné Python.").is_err());
    packet["training_choices"][1]["id"] = json!("a");
    assert!(
        PreparedTurn::new(&conversation, "Reprendre", packet.clone())
            .unwrap()
            .immediate_text("fr", "Reprendre")
            .is_err()
    );
    packet.as_object_mut().unwrap().remove("training_choices");
    let empty = PreparedTurn::new(&conversation, "Reprendre", packet)
        .unwrap()
        .immediate_text("fr", "Reprendre")
        .unwrap()
        .unwrap();
    assert!(!empty.contains("tutor-training-choice"));
}

#[test]
fn open_exercise_is_frozen_before_display_and_only_explicit_answer_is_submitted() {
    let mut exercise = card();
    exercise["kind"] = json!("exercise");
    for key in ["options", "correctOptionId", "explanation", "attemptId"] {
        exercise.as_object_mut().unwrap().remove(key);
    }
    let previous = conversation(card());
    let turn = PreparedTurn::new(
        &previous,
        "Reprendre",
        resume_packet("RECALL_EXERCISE", "assessment"),
    )
    .unwrap();
    let mut output = turn.output(&fence("tutor-activity", &exercise)).unwrap();
    assert_eq!(
        output.mutation.as_ref().unwrap().0,
        "prepare_assessment_attempt"
    );
    assert!(output.delivery.is_none());
    output.commit(&json!({"attempt_id":"attempt-1"})).unwrap();
    exercise = payload::<Value>(&output.text, "tutor-activity")
        .unwrap()
        .unwrap();
    let conversation = conversation(exercise.clone());
    let answer = json!({"version":1,"activityId":exercise["id"],"kind":"exercise","trainingId":"personal:go","domainId":"go","concept":"Variables","attemptId":"attempt-1","answer":"x = 5, car on ajoute 2 à 3.","responseTimeSeconds":15});
    let response_text = fence("tutor-response", &answer);
    let turn = PreparedTurn::new(&conversation, &response_text, turn_packet("context")).unwrap();
    assert_eq!(
        turn.submission().unwrap()["learner_response"],
        answer["answer"]
    );
    for text in ["Un indice, s'il vous plaît.", "x = 5"] {
        let help = PreparedTurn::new(&conversation, text, turn_packet("context")).unwrap();
        assert!(help.submission().is_none());
        assert!(!help.private_output());
    }
    let mut wrong = answer;
    wrong["attemptId"] = json!("foreign");
    assert!(PreparedTurn::request(&conversation, &fence("tutor-response", &wrong)).is_err());
    wrong["attemptId"] = json!("attempt-1");
    wrong["selectedOptionId"] = json!("a");
    assert!(PreparedTurn::request(&conversation, &fence("tutor-response", &wrong)).is_err());
}

#[test]
fn prepared_turn_freezes_before_display_and_records_before_feedback() {
    let conversation = conversation(card());
    let (hint, permissions) =
        PreparedTurn::request(&conversation, "Un indice, s’il vous plaît.").unwrap();
    assert_eq!(hint["mode"], "context");
    assert_eq!(hint["attempt_id"], "attempt-1");
    assert_eq!(permissions, vec!["prepare_pedagogical_turn"]);
    let study = fence(
        "tutor-study-request",
        &json!({"version":1,"format":"quiz","intent":"review","trainingId":"personal:go","domainId":"go"}),
    );
    let (request, permissions) = PreparedTurn::request(&conversation, &study).unwrap();
    assert_eq!(request["mode"], "activity");
    assert!(permissions.contains(&"prepare_assessment_attempt"));
    let turn = PreparedTurn::new(&conversation, &study, turn_packet("activity")).unwrap();
    let mut generated = card();
    generated.as_object_mut().unwrap().remove("attemptId");
    let mut output = turn.output(&fence("tutor-activity", &generated)).unwrap();
    assert!(turn.private_output());
    assert_eq!(
        output.mutation.as_ref().unwrap().0,
        "prepare_assessment_attempt"
    );
    assert_eq!(
        output.mutation.as_ref().unwrap().1["decision_id"],
        "decision-1"
    );
    // The MCP contract takes the frozen rubric as an object, never JSON text.
    assert_eq!(
        output.mutation.as_ref().unwrap().1["rubric"],
        serde_json::from_str::<Value>(card()["rubricJson"].as_str().unwrap()).unwrap()
    );
    assert!(output
        .mutation
        .as_ref()
        .unwrap()
        .1
        .get("rubric_json")
        .is_none());
    assert!(output.commit(&json!({})).is_err());
    output.commit(&json!({"attempt_id":"confirmed"})).unwrap();
    assert_eq!(
        payload::<Activity>(&output.text, "tutor-activity")
            .unwrap()
            .unwrap()
            .attempt_id
            .as_deref(),
        Some("confirmed")
    );
    generated["concept"] = json!("foreign");
    assert!(turn.output(&fence("tutor-activity", &generated)).is_err());

    let response = fence("tutor-response", &response());
    let turn = PreparedTurn::new(&conversation, &response, turn_packet("context")).unwrap();
    assert_eq!(turn.submission().unwrap()["learner_response"], "b: 5");
    let score = json!({"criteria_scores":[{"id":"correct","score":1,"max_score":1,"evidence":"The selected value is 5."}],"total":1,"max_total":1,"summary":"Correct","confidence":0.9});
    let encoded = json!({"version":1,"attempt_id":"attempt-1","rubric_score_json":score.to_string(),"feedback":"Vous avez retrouvé la valeur."});
    assert!(turn.output(&fence("tutor-evaluation", &encoded)).is_err());
    let evaluation = json!({"version":1,"attempt_id":"attempt-1","rubric_score":score,"feedback":"Vous avez retrouvé la valeur."});
    let mut output = turn
        .output(&fence("tutor-evaluation", &evaluation))
        .unwrap();
    assert_eq!(output.mutation.as_ref().unwrap().0, "record_interaction");
    assert_eq!(output.mutation.as_ref().unwrap().1["rubric_score"], score);
    assert!(output.commit(&json!({"updated":true,"assessment_attempt_id":"foreign","assessment_status":"evaluated","derived_success":true})).is_err());
    output.commit(&json!({"updated":true,"assessment_attempt_id":"attempt-1","assessment_status":"evaluated","derived_success":true})).unwrap();
    assert_eq!(output.delivery.as_ref().unwrap().kind, "feedback");
    assert!(!output.delivery.unwrap().recorded);
}

#[test]
fn interrupted_generation_reuses_decision_and_submission_without_accepting_a_changed_answer() {
    let mut conversation = conversation(card());
    let study = fence(
        "tutor-study-request",
        &json!({"version":1,"format":"quiz","intent":"review","trainingId":"personal:go","domainId":"go"}),
    );
    let original = PreparedTurn::request(&conversation, &study).unwrap().0;
    let append_failure = |conversation: &mut Conversation, text: &str| {
        let template = conversation.messages[0].clone();
        let mut user = template.clone();
        user.role = "user".into();
        user.text = text.into();
        user.id = format!("user-{}", conversation.messages.len());
        let mut reply = template;
        reply.private_output = true;
        reply.status = "interrupted".into();
        reply.text = "partial".into();
        reply.id = format!("reply-{}", conversation.messages.len());
        conversation.messages.extend([user, reply]);
    };
    append_failure(&mut conversation, &study);
    append_failure(&mut conversation, &study);
    assert_eq!(
        PreparedTurn::request(&conversation, &study).unwrap().0,
        original
    );
    let mut completed = conversation.clone();
    completed.messages.last_mut().unwrap().private_output = false;
    completed.messages.last_mut().unwrap().status = "complete".into();
    assert_ne!(
        PreparedTurn::request(&completed, &study).unwrap().0["idempotency_key"],
        original["idempotency_key"]
    );
    let answer = fence("tutor-response", &response());
    let before = PreparedTurn::new(&conversation, &answer, turn_packet("context"))
        .unwrap()
        .submission();
    append_failure(&mut conversation, &answer);
    let restored: Conversation =
        serde_json::from_str(&serde_json::to_string(&conversation).unwrap()).unwrap();
    assert_eq!(
        PreparedTurn::new(&restored, &answer, turn_packet("context"))
            .unwrap()
            .submission(),
        before
    );
    let mut changed = response();
    changed["selectedOptionId"] = json!("a");
    assert!(PreparedTurn::request(&restored, &fence("tutor-response", &changed)).is_err());
}

#[test]
fn prepared_turn_rejects_changed_rubric_and_false_score_or_prose_receipts() {
    let conversation = conversation(card());
    let response = fence("tutor-response", &response());
    let mut packet = turn_packet("context");
    packet["attempt"]["rubric"]["passing_score"] = json!(2);
    assert!(PreparedTurn::new(&conversation, &response, packet).is_err());
    let turn = PreparedTurn::new(&conversation, &response, turn_packet("context")).unwrap();
    assert!(turn.output("Le résultat est enregistré.").is_err());
    for score in [
        json!({"criteria_scores":[],"total":1,"max_total":1,"confidence":0.9}),
        json!({"criteria_scores":[{"id":"correct","score":2,"max_score":1,"evidence":"invented"}],"total":2,"max_total":1,"confidence":0.9}),
        json!({"criteria_scores":[{"id":"correct","score":1,"max_score":1,"evidence":"Observed value"}],"total":1,"max_total":1,"confidence":"medium"}),
    ] {
        assert!(turn.output(&fence("tutor-evaluation",&json!({"version":1,"attempt_id":"attempt-1","rubric_score":score,"feedback":"Good"}))).is_err());
    }
}

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
            pending_learning_commit: None,
            learning_delivery: None,
            private_output: false,
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
        paused: false,
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
        assert!(instructions.contains("n’appelle PAS prepare_pedagogical_turn"));
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
                paused: false,
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
                pending_learning_commit: None,
                learning_delivery: None,
                private_output: false,
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
        paused: false,
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
