// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT
use super::*;
use crate::learning_ui::{PreparedOutput, PreparedTurn};
use crate::permissions::{PermissionDecision, PermissionPolicy, ToolOperation};

pub(super) struct NativePermission {
    operation_id: String,
    sender: oneshot::Sender<Option<String>>,
}
impl Backend {
    pub(super) async fn prepared_turn(
        &self,
        conversation_id: &str,
        text: &str,
        binding: &str,
        operation: &Preparation,
    ) -> Result<Option<PreparedTurn>, String> {
        let resume = PreparedTurn::is_resume(text)?;
        let refresh = {
            let tutor = self.tutor.lock().await;
            let expected = tutor
                .credentials
                .as_ref()
                .map(tool_catalog_binding)
                .unwrap_or_else(|| tutor.local_binding.clone());
            tutor.tool_catalog_binding != expected
                || tutor
                    .tool_catalog_refreshed
                    .is_none_or(|at| at.elapsed() > std::time::Duration::from_secs(300))
        };
        if refresh {
            self.list_mcp_tools().await?;
        }
        let tutor = self.tutor.lock().await;
        if resume && !tutor.native_resume_supported {
            return Err("Le serveur doit être mis à jour pour préparer cette reprise avant le harness. Votre demande reste dans la file.".into());
        }
        let supported = tutor.tool_catalog.contains("prepare_pedagogical_turn");
        drop(tutor);
        if !supported {
            return Ok(None);
        }
        let conversation = self
            .store
            .lock()
            .await
            .data
            .conversations
            .iter()
            .find(|c| c.id == conversation_id)
            .cloned()
            .ok_or("Séance introuvable.")?;
        let (args, tools) = PreparedTurn::request(&conversation, text)?;
        // Consent addresses the prepared operation (`mode`), as the server's
        // policy does, so `prepare_pedagogical_turn.activity` rules apply.
        let keys = tools
            .iter()
            .map(|tool| match ToolOperation::of(tool, Some(&args)) {
                ToolOperation::Selected(mode) => format!("{tool}.{mode}"),
                _ => (*tool).to_owned(),
            })
            .collect::<Vec<_>>();
        let keys = keys.iter().map(String::as_str).collect::<Vec<_>>();
        self.consent_native(&keys, binding, operation).await?;
        operation.phase(TurnPhase::Context);
        let packet = self
            .pedagogical_tool("prepare_pedagogical_turn", args, conversation_id, binding)
            .await?;
        operation.check()?;
        let turn = PreparedTurn::new(&conversation, text, packet)?;
        if let Some(args) = turn.submission() {
            let result = self
                .pedagogical_tool(
                    "submit_assessment_attempt",
                    args.clone(),
                    conversation_id,
                    binding,
                )
                .await?;
            if result["attempt_id"] != args["attempt_id"]
                || !matches!(result["status"].as_str(), Some("submitted" | "evaluated"))
            {
                return Err("Le serveur n’a pas confirmé la soumission de cette réponse.".into());
            }
            operation.check()?;
        }
        Ok(Some(turn))
    }

    pub(super) async fn finish_native_clarification(
        &self,
        conversation_id: &str,
        text: &str,
        reply: &str,
        context: &Value,
        binding: &str,
        operation: &Preparation,
    ) -> Result<(), String> {
        operation.check()?;
        let tutor = self.tutor.lock().await;
        if tutor.binding != binding || self.retired.load(Ordering::Acquire) {
            return Err("La connexion pédagogique a changé.".into());
        }
        self.store.lock().await.transaction(|data| {
            operation.check()?;
            let conversation = data
                .conversations
                .iter_mut()
                .find(|c| c.id == conversation_id)
                .ok_or("Séance introuvable.")?;
            conversation.active_training = Some(context.clone());
            if conversation.messages.is_empty() {
                conversation.title = if data.settings.language == "en" {
                    "Resume a course".into()
                } else {
                    "Reprendre une formation".into()
                };
            }
            for (role, content) in [("user", text), ("assistant", reply)] {
                conversation.messages.push(Message {
                    pending_learning_commit: None,
                    learning_delivery: None,
                    private_output: false,
                    training_context: Some(context.clone()),
                    id: uuid::Uuid::new_v4().to_string(),
                    role: role.into(),
                    text: content.into(),
                    created_at: now(),
                    status: "complete".into(),
                });
            }
            conversation.updated_at = now();
            data.pending_message = None;
            Ok(())
        })?;
        drop(tutor);
        self.state_changed().await;
        Ok(())
    }
    pub(super) async fn native_policy_binding(&self) -> Option<(String, String, String)> {
        let target = self.store.lock().await.data.settings.target_id.clone();
        let tutor = self.tutor.lock().await;
        if let Some(c) = &tutor.credentials {
            Some((c.origin.clone(), c.principal.tenant_id.clone(), target))
        } else if tutor.local_rpc.is_some() && !tutor.local_binding.is_empty() {
            Some(("local://tutor".into(), tutor.local_binding.clone(), target))
        } else {
            None
        }
    }
    pub(super) async fn consent_native(
        &self,
        tools: &[&str],
        binding: &str,
        operation: &Preparation,
    ) -> Result<(), String> {
        operation.check()?;
        let scope = self.native_policy_binding().await;
        let policy = self
            .store
            .lock()
            .await
            .data
            .settings
            .permission_policy
            .clone();
        let mut ask = Vec::new();
        // Entries are policy keys: a bare tool, or `<tool>.<operation>` for a
        // merged tool. A remembered choice is stored under that same key.
        for key in tools {
            let (tool, selected) = ToolOperation::from_key(key);
            if !self.tutor.lock().await.tool_catalog.contains(tool) {
                return Err(format!(
                    "Le serveur ne propose pas l’opération pédagogique {tool}."
                ));
            }
            let decision = scope
                .as_ref()
                .and_then(|(origin, institution, agent)| {
                    policy
                        .as_ref()
                        .filter(|p| p.matches(origin, institution, agent))
                })
                .map(|p| p.decide(tool, &selected))
                .unwrap_or(PermissionDecision::Ask);
            match decision {
                PermissionDecision::Deny => {
                    return Err(format!(
                        "L’opération {key} est refusée par vos autorisations MCP."
                    ))
                }
                PermissionDecision::Ask => ask.push(*key),
                PermissionDecision::Allow => {}
            }
        }
        if ask.is_empty() {
            return Ok(());
        }
        operation.phase(TurnPhase::WaitingPermission);
        let id = format!("native:{}", uuid::Uuid::new_v4());
        let (sender, receiver) = oneshot::channel();
        self.native_permissions.lock().await.insert(
            id.clone(),
            NativePermission {
                operation_id: operation.id.clone(),
                sender,
            },
        );
        let mut options = vec![
            json!({"optionId":"allow_once","kind":"allow_once","name":"Autoriser une fois"}),
            json!({"optionId":"reject_once","kind":"reject_once","name":"Refuser"}),
        ];
        if scope.is_some() {
            options.push(json!({"optionId":"allow_always","kind":"allow_always","name":"Toujours autoriser"}));
            options.push(json!({"optionId":"reject_always","kind":"reject_always","name":"Toujours refuser"}));
        }
        (self.emit)(
            "acp:event",
            json!({"type":"native_permission","id":id,"params":{"operations":tools,"toolCall":{"title":"Préparer votre échange pédagogique","content":[{"type":"text","text":format!("Opérations MCP pour cette action : {}. Les mutations éventuelles seront limitées à cette conversation et à son activité.",tools.join(", "))}]},"options":options}}),
        );
        let choice = tokio::select! {biased;_ = operation.cancelled()=>None,result=receiver=>result.ok().flatten()};
        self.native_permissions.lock().await.remove(&id);
        (self.emit)(
            "acp:event",
            json!({"type":"native_permission_closed","id":id}),
        );
        operation.check()?;
        if self.tutor.lock().await.binding != binding || self.retired.load(Ordering::Acquire) {
            return Err("La connexion pédagogique a changé.".into());
        }
        if matches!(choice.as_deref(), Some("allow_always" | "reject_always")) {
            let (origin, institution_id, agent_id) =
                scope.ok_or("Cette autorisation ne peut pas être mémorisée.")?;
            let decision = if choice.as_deref() == Some("allow_always") {
                PermissionDecision::Allow
            } else {
                PermissionDecision::Deny
            };
            self.store.lock().await.transaction(|data| {
                let policy = &mut data.settings.permission_policy;
                if !policy
                    .as_ref()
                    .is_some_and(|p| p.matches(&origin, &institution_id, &agent_id))
                {
                    *policy = Some(PermissionPolicy {
                        origin,
                        institution_id,
                        agent_id,
                        default_decision: PermissionDecision::Ask,
                        tools: Default::default(),
                    });
                }
                for tool in &ask {
                    policy
                        .as_mut()
                        .unwrap()
                        .tools
                        .insert((*tool).into(), decision);
                }
                Ok(())
            })?;
        }
        if !matches!(choice.as_deref(), Some("allow_once" | "allow_always")) {
            return Err("Action pédagogique refusée. Votre message reste dans la file.".into());
        }
        Ok(())
    }
    pub(super) async fn answer_native_permission(
        &self,
        id: &str,
        choice: Option<String>,
    ) -> Result<(), String> {
        if !matches!(
            choice.as_deref(),
            None | Some("allow_once" | "allow_always" | "reject_once" | "reject_always")
        ) {
            return Err("Choix d’autorisation invalide.".into());
        }
        let mut pending = self.native_permissions.lock().await;
        let request = pending
            .get(id)
            .ok_or("Cette autorisation n’est plus active.")?;
        if !self
            .preparing
            .lock()
            .unwrap()
            .as_ref()
            .is_some_and(|o| o.id == request.operation_id && !o.cancelled.load(Ordering::Acquire))
        {
            return Err("Cette autorisation n’est plus active.".into());
        }
        pending
            .remove(id)
            .unwrap()
            .sender
            .send(choice)
            .map_err(|_| "Cette autorisation n’est plus active.".into())
    }
    pub(super) async fn close_native_permissions(&self, operation_id: &str) {
        let mut requests = self.native_permissions.lock().await;
        let ids = requests
            .iter()
            .filter(|(_, r)| r.operation_id == operation_id)
            .map(|(id, _)| id.clone())
            .collect::<Vec<_>>();
        for id in ids {
            requests.remove(&id);
            (self.emit)(
                "acp:event",
                json!({"type":"native_permission_closed","id":id}),
            );
        }
    }
    pub(super) async fn pedagogical_tool(
        &self,
        name: &str,
        args: Value,
        conversation_id: &str,
        binding: &str,
    ) -> Result<Value, String> {
        let settings = self.store.lock().await.data.settings.clone();
        if settings.tutor.mode == "remote" {
            self.ensure_transport_credentials(&settings).await?;
        }
        let (remote, local) = {
            let tutor = self.tutor.lock().await;
            if tutor.binding != binding || self.retired.load(Ordering::Acquire) {
                return Err("La connexion pédagogique a changé.".into());
            }
            (tutor.credentials.clone(), tutor.local_rpc.clone())
        };
        let result = if let Some(credentials) = remote {
            let id = format!("pedagogy-{}", uuid::Uuid::new_v4());
            let mut request = http_client()?
                .post(credentials.resource())
                .bearer_auth(&credentials.access_token)
                .header("Accept", "application/json, text/event-stream")
                .header("Mcp-Protocol-Version", "2025-06-18")
                .header("Tutor-Conversation-ID", conversation_id);
            if let Some(header) = settings.permission_policy.as_ref().and_then(|p| {
                p.transport_header(
                    &credentials.origin,
                    &credentials.principal.tenant_id,
                    &settings.target_id,
                )
            }) {
                request = request.header("Tutor-Tool-Policy", header);
            }
            let response=request.json(&json!({"jsonrpc":"2.0","id":id,"method":"tools/call","params":{"name":name,"arguments":args,"_meta":{"tutor/conversation_id":conversation_id}}})).send().await.map_err(|_|"L’opération pédagogique MCP a échoué.")?;
            if !response.status().is_success() {
                return Err("Le serveur a refusé l’opération pédagogique.".into());
            }
            bounded_mcp_json(response, &id).await?
        } else if let Some(rpc) = local {
            rpc.request("tools/call",json!({"name":name,"arguments":args,"_meta":{"tutor/conversation_id":conversation_id}})).await?
        } else {
            return Err("Reconnectez Tutor.".into());
        };
        if self.tutor.lock().await.binding != binding || self.retired.load(Ordering::Acquire) {
            return Err("La connexion pédagogique a changé.".into());
        }
        unwrap_mcp_tool(result, name)
    }
    pub(super) async fn finalize_prepared(
        &self,
        turn: &PreparedTurn,
        prompt: &Prompt,
        binding: &str,
        operation: &Preparation,
    ) -> Result<(), String> {
        operation.check()?;
        let raw = self
            .store
            .lock()
            .await
            .data
            .conversations
            .iter()
            .find(|c| c.id == prompt.conversation_id)
            .and_then(|c| {
                c.messages
                    .iter()
                    .find(|m| m.id == prompt.assistant_message_id)
            })
            .map(|m| m.text.clone())
            .ok_or("Réponse introuvable.")?;
        let mut output: PreparedOutput = turn.output(&raw)?;
        if turn.requires_selection_check() {
            let current = self
                .pedagogical_tool(
                    "get_conversation_context",
                    json!({"conversation_id":prompt.conversation_id}),
                    &prompt.conversation_id,
                    binding,
                )
                .await?;
            turn.validate_selection(&current)?;
            operation.check()?;
        }
        if let Some((name, args)) = output.mutation.clone() {
            self.store.lock().await.transaction(|data| {
                let message = data
                    .conversations
                    .iter_mut()
                    .find(|c| c.id == prompt.conversation_id)
                    .and_then(|c| {
                        c.messages
                            .iter_mut()
                            .find(|m| m.id == prompt.assistant_message_id)
                    })
                    .ok_or("Réponse introuvable.")?;
                message.pending_learning_commit = Some(crate::workspace::PendingLearningCommit {
                    tool: name.into(),
                    arguments: args.clone(),
                    text: output.text.clone(),
                    delivery: output.delivery.clone(),
                });
                Ok(())
            })?;
            let result = self
                .pedagogical_tool(name, args, &prompt.conversation_id, binding)
                .await?;
            output.commit(&result)?;
        }
        operation.check()?;
        self.store.lock().await.transaction(|data| {
            let message = data
                .conversations
                .iter_mut()
                .find(|c| c.id == prompt.conversation_id)
                .and_then(|c| {
                    c.messages
                        .iter_mut()
                        .find(|m| m.id == prompt.assistant_message_id)
                })
                .ok_or("Réponse introuvable.")?;
            message.text = output.text;
            message.learning_delivery = output.delivery;
            message.private_output = false;
            message.pending_learning_commit = None;
            // Publish the durable completion in the same local transaction as
            // the confirmed content. A late Stop cannot undo the server commit.
            message.status = "complete".into();
            Ok(())
        })?;
        if turn.private_output() {
            (self.emit)(
                "acp:event",
                json!({"type":"workspace_invalidated","scope":"training_catalog"}),
            );
        }
        Ok(())
    }
    pub(super) async fn retry_learning_commit(
        &self,
        conversation_id: &str,
        text: &str,
        binding: &str,
        operation: &Preparation,
    ) -> Result<bool, String> {
        let saved = {
            let store = self.store.lock().await;
            let Some(conversation) = store
                .data
                .conversations
                .iter()
                .find(|c| c.id == conversation_id && c.tutor_binding == binding)
            else {
                return Ok(false);
            };
            conversation
                .messages
                .windows(2)
                .rev()
                .find(|pair| {
                    pair[0].role == "user"
                        && pair[0].text == text
                        && pair[1].private_output
                        && pair[1].pending_learning_commit.is_some()
                })
                .map(|pair| {
                    (
                        pair[1].id.clone(),
                        pair[1].pending_learning_commit.clone().unwrap(),
                    )
                })
        };
        let Some((message_id, saved)) = saved else {
            return Ok(false);
        };
        self.list_mcp_tools().await?;
        let name = match saved.tool.as_str() {
            "prepare_assessment_attempt" => "prepare_assessment_attempt",
            "record_interaction" => "record_interaction",
            _ => return Err("Opération pédagogique conservée invalide.".into()),
        };
        let mut tools = vec![name];
        if saved.delivery.is_some() {
            tools.push("record_learning_event");
        }
        self.consent_native(&tools, binding, operation).await?;
        operation.phase(TurnPhase::Finalizing);
        let result = self
            .pedagogical_tool(name, saved.arguments.clone(), conversation_id, binding)
            .await?;
        let mut output = PreparedOutput {
            text: saved.text,
            mutation: Some((name, saved.arguments)),
            delivery: saved.delivery,
        };
        output.commit(&result)?;
        operation.check()?;
        self.store.lock().await.transaction(|data| {
            let message = data
                .conversations
                .iter_mut()
                .find(|c| c.id == conversation_id && c.tutor_binding == binding)
                .and_then(|c| c.messages.iter_mut().find(|m| m.id == message_id))
                .ok_or("Réponse introuvable.")?;
            message.text = output.text;
            message.learning_delivery = output.delivery;
            message.private_output = false;
            message.pending_learning_commit = None;
            message.status = "complete".into();
            if data.pending_message.as_ref().is_some_and(|p| {
                p.text == text && p.conversation_id.as_deref() == Some(conversation_id)
            }) {
                data.pending_message = None;
            }
            Ok(())
        })?;
        self.state_changed().await;
        Ok(true)
    }
    pub async fn acknowledge_learning_delivery(
        &self,
        conversation_id: String,
        message_id: String,
    ) -> Result<(), String> {
        // No lock around the foreground turn: delivery is idempotent and must
        // never prevent the learner from sending another message.
        let (delivery, binding) = {
            let store = self.store.lock().await;
            let conversation = store
                .data
                .conversations
                .iter()
                .find(|c| c.id == conversation_id)
                .ok_or("Séance introuvable.")?;
            let message = conversation
                .messages
                .iter()
                .find(|m| m.id == message_id && m.status == "complete" && !m.private_output)
                .ok_or("Retour pédagogique non publié.")?;
            (
                message
                    .learning_delivery
                    .clone()
                    .ok_or("Ce message n’a pas de retour à acquitter.")?,
                conversation.tutor_binding.clone(),
            )
        };
        if delivery.recorded {
            return Ok(());
        }
        // A later explicit deny revokes even a previously approved delivery.
        if let Some((origin, institution, agent)) = self.native_policy_binding().await {
            let policy = self
                .store
                .lock()
                .await
                .data
                .settings
                .permission_policy
                .clone();
            if policy
                .filter(|p| p.matches(&origin, &institution, &agent))
                .is_some_and(|p| {
                    p.tools
                        .get("record_learning_event")
                        .copied()
                        .unwrap_or(p.default_decision)
                        == PermissionDecision::Deny
                })
            {
                return Err(
                    "L’enregistrement de l’exposition est refusé par vos autorisations MCP.".into(),
                );
            }
        }
        let mut args = json!({"idempotency_key":delivery.event_key,"event_key":delivery.event_key,"domain_id":delivery.domain_id,"concept":delivery.concept,"kind":delivery.kind});
        if let Some(id) = &delivery.attempt_id {
            args["attempt_id"] = json!(id);
        }
        let receipt = self
            .pedagogical_tool("record_learning_event", args, &conversation_id, &binding)
            .await?;
        if receipt["event"]["event_key"] != delivery.event_key {
            return Err("Le serveur n’a pas confirmé la réception du retour pédagogique.".into());
        }
        self.store.lock().await.transaction(|data| {
            let receipt = data
                .conversations
                .iter_mut()
                .find(|c| c.id == conversation_id && c.tutor_binding == binding)
                .and_then(|c| c.messages.iter_mut().find(|m| m.id == message_id))
                .and_then(|m| m.learning_delivery.as_mut())
                .ok_or("Le retour pédagogique a changé.")?;
            if receipt.event_key != delivery.event_key {
                return Err("Le retour pédagogique a changé.".into());
            }
            receipt.recorded = true;
            Ok(())
        })?;
        self.state_changed().await;
        Ok(())
    }
}

/// Training tools the interface may call, named as on the MCP surface.
/// Edits and lifecycle changes are single tools selected by `action`
/// (`edit_training`: preview/propose/save; `manage_training_lifecycle`:
/// archive/restore/delete/withdraw with `preview=true` or `confirm=true`).
pub(super) const TRAINING_TOOLS: &[&str] = &[
    "list_training_library",
    "get_training_groups",
    "get_training",
    "get_conversation_context",
    "select_conversation_training",
    "edit_training",
    "share_training",
    "submit_training",
    "decide_training",
    "publish_training",
    "set_training_access",
    "get_training_policy",
    "set_training_policy",
    "find_training_members",
    "join_training",
    "manage_training_lifecycle",
];

const EDIT_ACTIONS: &[&str] = &["preview", "propose", "save"];
const LIFECYCLE_ACTIONS: &[&str] = &["archive", "restore", "delete", "withdraw"];

fn invalid_training_request() -> String {
    "Requête de formation invalide.".into()
}

/// Validates the `action` selector of a merged training tool.
fn training_selector<'a>(tool: &str, input: &'a Value) -> Result<Option<&'a str>, String> {
    let allowed = match tool {
        "edit_training" => EDIT_ACTIONS,
        "manage_training_lifecycle" => LIFECYCLE_ACTIONS,
        _ => return Ok(None),
    };
    input
        .get("action")
        .and_then(Value::as_str)
        .filter(|action| allowed.contains(action))
        .map(Some)
        .ok_or_else(invalid_training_request)
}

/// Local mode calls the MCP tool itself. The interface names the training
/// and the idempotency key `id` and `key`, as the native client API does;
/// the MCP surface expects `training_id` and `idempotency_key`.
pub(super) fn local_training_call(
    tool: &str,
    input: Value,
) -> Result<(&'static str, Value), String> {
    let tool = TRAINING_TOOLS
        .iter()
        .copied()
        .find(|name| *name == tool)
        .ok_or_else(invalid_training_request)?;
    training_selector(tool, &input)?;
    let Value::Object(fields) = input else {
        return Err(invalid_training_request());
    };
    let args = fields
        .into_iter()
        .map(|(name, value)| {
            let name = match name.as_str() {
                "id" => "training_id".to_owned(),
                "key" => "idempotency_key".to_owned(),
                _ => name,
            };
            (name, value)
        })
        .collect();
    Ok((tool, Value::Object(args)))
}

/// Remote mode goes through the native client API, which keeps the store
/// action vocabulary (`save_training_edit`, `archive_training`,
/// `preview_training_lifecycle` with `decision`, ...). Translate one
/// validated MCP tool call into that action and its request body.
pub(super) fn native_training_call(tool: &str, input: Value) -> Result<(String, Value), String> {
    if !TRAINING_TOOLS.contains(&tool) {
        return Err(invalid_training_request());
    }
    let selector = training_selector(tool, &input)?.map(str::to_owned);
    let Value::Object(mut fields) = input else {
        return Err(invalid_training_request());
    };
    fields.remove("action");
    let preview = fields.remove("preview");
    let action = match (tool, selector) {
        ("edit_training", Some(action)) => {
            if preview.is_some() {
                return Err(invalid_training_request());
            }
            format!("{action}_training_edit")
        }
        ("manage_training_lifecycle", Some(action)) => match preview {
            Some(Value::Bool(true)) => {
                if fields.contains_key("confirm") {
                    return Err(invalid_training_request());
                }
                fields.insert("decision".into(), json!(format!("{action}_training")));
                "preview_training_lifecycle".to_owned()
            }
            None | Some(Value::Bool(false)) => format!("{action}_training"),
            Some(_) => return Err(invalid_training_request()),
        },
        (_, None) if preview.is_none() => tool.to_owned(),
        _ => return Err(invalid_training_request()),
    };
    Ok((action, Value::Object(fields)))
}
