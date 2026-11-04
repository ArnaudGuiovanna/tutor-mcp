// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT
use super::*;
use crate::learning_ui::{PreparedOutput, PreparedTurn};
use crate::permissions::{PermissionDecision, PermissionPolicy};

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
        if self.settings().await.target_id == crate::local_agent::TARGET_ID {
            return Ok(None);
        }
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
        if !self
            .tutor
            .lock()
            .await
            .tool_catalog
            .contains("prepare_pedagogical_turn")
        {
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
        self.consent_native(&tools, binding, operation).await?;
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
        for tool in tools {
            if !self.tutor.lock().await.tool_catalog.contains(*tool) {
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
                .map(|p| p.tools.get(*tool).copied().unwrap_or(p.default_decision))
                .unwrap_or(PermissionDecision::Ask);
            match decision {
                PermissionDecision::Deny => {
                    return Err(format!(
                        "L’opération {tool} est refusée par vos autorisations MCP."
                    ))
                }
                PermissionDecision::Ask => ask.push(*tool),
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
