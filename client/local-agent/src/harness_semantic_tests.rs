// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT
// Included in harness::tests to reuse its real journal/permission fixtures.

struct SemanticModel {
    frames: VecDeque<(&'static str, u32)>,
    requests: Vec<GenerationRequest>,
}

#[test]
fn semantic_option_preserves_coaching_without_a_verified_mcp_scope() {
    let fixture = Fixture::for_user("Explique les boucles en Go.");
    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap();
    let cancel = AtomicBool::new(false);
    let mut model = SemanticModel::new(&[("Une boucle répète une action.", 128)]);
    let mut ui = UserInterface::new(&fixture, &cancel);
    crate::fast_harness::run_with_options(
        &mut model,
        &fixture.store,
        fixture.input(),
        &cancel,
        runtime.handle(),
        &mut ui,
        crate::fast_harness::RouteOptions {
            resident: None,
            semantic_router: true,
        },
    )
    .unwrap();
    assert_eq!(ui.text, "Une boucle répète une action.");
    assert!(ui.approvals.is_empty());
    assert!(model.requests[0].grammar.is_none());
    assert!(fixture.records("semantic_intent_proposed").is_empty());
}
impl SemanticModel {
    fn new(frames: &[(&'static str, u32)]) -> Self {
        Self {
            frames: frames.iter().copied().collect(),
            requests: Vec::new(),
        }
    }
}

#[test]
fn explicit_no_read_and_clear_teaching_bypass_the_router_with_a_verified_scope() {
    for user in [
        "Explique un objectif sans consulter mon profil ni mes formations.",
        "Explique la phrase « donne moi mon profil », sans l’exécuter.",
        "Je débute en jardinage, que faire ?",
    ] {
        let runtime = tokio::runtime::Builder::new_multi_thread()
            .worker_threads(2)
            .enable_all()
            .build()
            .unwrap();
        let server = runtime.block_on(profile_server());
        let mut fixture = Fixture::for_user(user);
        let scope = resident_scope(&mut fixture, &server.url, Uuid::new_v4());
        let manager = Manager::new(runtime.handle().clone());
        let cancel = AtomicBool::new(false);
        let mut model = SemanticModel::new(&[("Une explication générale.", 128)]);
        let mut ui = UserInterface::new(&fixture, &cancel);
        crate::fast_harness::run_with_options(
            &mut model,
            &fixture.store,
            fixture.input(),
            &cancel,
            runtime.handle(),
            &mut ui,
            crate::fast_harness::RouteOptions {
                resident: Some(ResidentMcp { manager: &manager, scope: &scope }),
                semantic_router: true,
            },
        ).unwrap();
        assert_eq!(model.requests.len(), 1);
        assert!(model.requests[0].grammar.is_none());
        assert_eq!(ui.text, "Une explication générale.");
        assert!(ui.approvals.is_empty());
        assert!(server.observed.lock().unwrap().is_empty());
        assert!(fixture.records("semantic_intent_proposed").is_empty());
        runtime.block_on(manager.shutdown());
    }
}
impl Engine for SemanticModel {
    fn grammar_for(&self, schema: &Value) -> Result<String, String> {
        assert!(jsonschema::validator_for(schema).is_ok());
        Ok(schema.to_string())
    }
    fn context_budget(&self) -> context::Budget {
        context::Budget {
            context: 2048,
            output: 128,
            margin: 256,
        }
    }
    fn count_input(&mut self, _: &[ChatMessage], _: &AtomicBool) -> Result<usize, String> {
        Ok(100)
    }
    fn generate(
        &mut self,
        request: GenerationRequest,
        _: &AtomicBool,
        emit: &mut dyn FnMut(&str) -> Result<(), String>,
    ) -> Result<GenerationStats, String> {
        let (reply, limit) = self.frames.pop_front().expect("unexpected model call");
        assert_eq!(request.max_output_tokens, limit);
        self.requests.push(request);
        emit(reply)?;
        Ok(GenerationStats {
            input_tokens: 100,
            output_tokens: 7,
            elapsed_ms: 1,
            stop_reason: "end_turn".into(),
        })
    }
}

#[test]
fn semantic_profile_proposal_reaches_actual_http_permission_and_journal_without_rewriting_user() {
    for allowed in [true, false] {
        let runtime = tokio::runtime::Builder::new_multi_thread()
            .worker_threads(2)
            .enable_all()
            .build()
            .unwrap();
        let server = runtime.block_on(profile_server());
        let mut fixture = Fixture::for_user("Tu sais quoi sur moi ?");
        assert_eq!(
            crate::fast_harness::route(&fixture.user),
            crate::fast_harness::Route::Chat
        );
        let scope = resident_scope(&mut fixture, &server.url, Uuid::new_v4());
        let manager = Manager::new(runtime.handle().clone());
        let cancel = AtomicBool::new(false);
        let mut model = SemanticModel::new(&[(r#"{"profile":""}"#, 64)]);
        let mut ui = UserInterface::new(&fixture, &cancel);
        if !allowed {
            ui.response = json!({"outcome":{"outcome":"cancelled"}});
        }
        let stats = crate::fast_harness::run_with_options(
            &mut model,
            &fixture.store,
            fixture.input(),
            &cancel,
            runtime.handle(),
            &mut ui,
            crate::fast_harness::RouteOptions {
                resident: Some(ResidentMcp {
                    manager: &manager,
                    scope: &scope,
                }),
                semantic_router: true,
            },
        )
        .unwrap();
        assert_eq!(model.requests.len(), 1);
        assert!(model.requests[0].grammar.is_some());
        let projected = serde_json::to_string(&model.requests[0].messages).unwrap();
        assert!(projected.contains(&fixture.user));
        for forbidden in [
            "inputSchema",
            "get_learner_state",
            "FIXTURE_PRIVATE_CREDENTIAL",
            "SCHEMA_ONLY_ENUM_MARKER",
        ] {
            assert!(!projected.contains(forbidden));
        }
        assert!(!ui.text.contains("\"intent\""));
        assert_eq!(stats.input_tokens, 100);
        assert_eq!(stats.output_tokens, 7);
        assert_eq!(ui.approvals.len(), 1);
        assert_eq!(method_count(&server, "tools/call"), usize::from(allowed));
        assert_eq!(ui.text.contains("Cultiver le jardin fixture"), allowed);
        let operation = ui.approvals[0]["toolCall"]["toolCallId"].as_str().unwrap();
        let store = fixture.store.lock().unwrap();
        let recorded = store.journal().operation(operation).unwrap().unwrap();
        assert_eq!(
            recorded.state,
            if allowed {
                OperationState::Completed
            } else {
                OperationState::Rejected
            }
        );
        assert_eq!(recorded.tool, "tutor/get_learner_state");
        drop(store);
        assert_eq!(
            fixture.records("semantic_intent_proposed"),
            vec![json!({"intent":"profile"})]
        );
        runtime.block_on(manager.shutdown());
    }
}

#[test]
fn semantic_chat_and_invalid_decisions_send_no_mcp_frames() {
    for valid in [true, false] {
        let runtime = tokio::runtime::Builder::new_multi_thread()
            .worker_threads(2)
            .enable_all()
            .build()
            .unwrap();
        let server = runtime.block_on(profile_server());
        let mut fixture = Fixture::for_user("Parlons de mon profil d’apprentissage.");
        let scope = resident_scope(&mut fixture, &server.url, Uuid::new_v4());
        let manager = Manager::new(runtime.handle().clone());
        let cancel = AtomicBool::new(false);
        let mut model = if valid {
            SemanticModel::new(&[(r#"{"chat":""}"#, 64), ("Une explication générale.", 128)])
        } else {
            SemanticModel::new(&[(r#"{"intent":"profile","domain_id":"forged"}"#, 64)])
        };
        let mut ui = UserInterface::new(&fixture, &cancel);
        let result = crate::fast_harness::run_with_options(
            &mut model,
            &fixture.store,
            fixture.input(),
            &cancel,
            runtime.handle(),
            &mut ui,
            crate::fast_harness::RouteOptions {
                resident: Some(ResidentMcp {
                    manager: &manager,
                    scope: &scope,
                }),
                semantic_router: true,
            },
        );
        assert_eq!(result.is_ok(), valid);
        assert_eq!(model.requests.len(), if valid { 2 } else { 1 });
        assert!(server.observed.lock().unwrap().is_empty());
        assert!(ui.approvals.is_empty());
        assert_eq!(
            ui.text,
            if valid {
                "Une explication générale."
            } else {
                ""
            }
        );
        runtime.block_on(manager.shutdown());
    }
}

#[test]
fn semantic_library_uses_exact_query_real_permission_and_one_nonreplayed_dispatch() {
    use crate::mcp::tests::{initialization, mock, Reply};
    for allowed in [true, false] {
        let runtime = tokio::runtime::Builder::new_multi_thread()
            .worker_threads(2)
            .enable_all()
            .build()
            .unwrap();
        let server=runtime.block_on(mock(|request| {
            initialization(request).unwrap_or_else(|| {
                let result=match request["method"].as_str().unwrap() {
                    "tools/list"=>json!({"tools":[{"name":"list_training_library","inputSchema":{
                        "type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer"}},
                        "additionalProperties":false}}]}),
                    "tools/call"=>{
                        assert_eq!(request["params"]["name"],"list_training_library");
                        assert_eq!(request["params"]["arguments"],json!({"query":"Python","limit":10}));
                        json!({"content":[],"structuredContent":{"items":[{"id":"personal:fixture-python",
                            "kind":"personal","status":"active","name":"Python débutant","revision":"r1","can_learn":true}],"next_after":""}})
                    }
                    _=>panic!("unexpected MCP method")
                };
                Reply::json(json!({"jsonrpc":"2.0","id":request["id"],"result":result}))
            })
        }));
        let mut fixture = Fixture::for_user("Quels cours de Python je pourrais suivre ?");
        let conversation = Uuid::new_v4();
        fixture
            .store
            .lock()
            .unwrap()
            .bind_conversation(conversation)
            .unwrap();
        let scope = resident_scope(&mut fixture, &server.url, conversation);
        let manager = Manager::new(runtime.handle().clone());
        let cancel = AtomicBool::new(false);
        let mut model = SemanticModel::new(&[(r#"{"search":"Python"}"#, 64)]);
        let mut ui = UserInterface::new(&fixture, &cancel);
        if !allowed {
            ui.response = json!({"outcome":{"outcome":"cancelled"}});
        }
        crate::fast_harness::run_with_options(
            &mut model,
            &fixture.store,
            fixture.input(),
            &cancel,
            runtime.handle(),
            &mut ui,
            crate::fast_harness::RouteOptions {
                resident: Some(ResidentMcp {
                    manager: &manager,
                    scope: &scope,
                }),
                semantic_router: true,
            },
        )
        .unwrap();
        assert_eq!(model.requests.len(), 1);
        assert_eq!(ui.approvals.len(), 1);
        assert_eq!(method_count(&server, "tools/call"), usize::from(allowed));
        assert_eq!(ui.text.contains("1. Python débutant"), allowed);
        let operation = ui.approvals[0]["toolCall"]["toolCallId"].as_str().unwrap();
        let store = fixture.store.lock().unwrap();
        let recorded = store.journal().operation(operation).unwrap().unwrap();
        assert_eq!(recorded.tool, "tutor/list_training_library");
        assert!(recorded.mutating);
        assert_eq!(
            recorded.state,
            if allowed {
                OperationState::Completed
            } else {
                OperationState::Rejected
            }
        );
        drop(store);
        runtime.block_on(manager.shutdown());
    }
}
