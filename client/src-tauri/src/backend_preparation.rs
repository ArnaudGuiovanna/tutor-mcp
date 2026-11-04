// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! Own a learner turn before waiting for locks or remote session setup.
use super::*;
use crate::timing::{OperationTiming, TimingOutcome, TurnPhase};
use tokio::sync::Notify;

pub(super) struct Preparation {
    pub id: String,
    pub conversation_id: Option<String>,
    pub cancelled: AtomicBool,
    pub loading: AtomicBool,
    pub sent: AtomicBool,
    pub wake: Notify,
    pub timing: OperationTiming,
}

pub(super) struct PreparationGuard<'a> {
    pub operation: Arc<Preparation>,
    backend: &'a Backend,
}

impl Drop for PreparationGuard<'_> {
    fn drop(&mut self) {
        let mut active = self.backend.preparing.lock().unwrap();
        if active
            .as_ref()
            .is_some_and(|item| item.id == self.operation.id)
        {
            active.take();
        }
    }
}

impl Backend {
    pub(super) fn begin_preparation(
        &self,
        conversation_id: Option<String>,
    ) -> Result<PreparationGuard<'_>, String> {
        let mut active = self.preparing.lock().unwrap();
        if active.is_some() {
            return Err("Une demande est déjà en cours.".into());
        }
        let id = uuid::Uuid::new_v4().to_string();
        let operation = Arc::new(Preparation {
            timing: OperationTiming::new(self.emit.clone(), id.clone(), conversation_id.clone()),
            id,
            conversation_id,
            cancelled: AtomicBool::new(false),
            loading: AtomicBool::new(false),
            sent: AtomicBool::new(false),
            wake: Notify::new(),
        });
        *active = Some(operation.clone());
        self.cancel_memory_maintenance();
        Ok(PreparationGuard {
            operation,
            backend: self,
        })
    }

    pub(super) async fn pause_outbox(&self, conversation_id: Option<&str>) -> Result<(), String> {
        let mut store = self.store.lock().await;
        if !store.data.pending_message.as_ref().is_some_and(|message| {
            !message.paused
                && (conversation_id.is_none()
                    || message.conversation_id.is_none()
                    || message.conversation_id.as_deref() == conversation_id)
        }) {
            return Ok(());
        }
        store.transaction(|data| {
            if let Some(message) = data.pending_message.as_mut().filter(|message| {
                conversation_id.is_none()
                    || message.conversation_id.is_none()
                    || message.conversation_id.as_deref() == conversation_id
            }) {
                message.paused = true;
            }
            Ok(())
        })
    }

    pub(super) async fn cancelled_preparation(
        &self,
        operation: &Preparation,
    ) -> Result<(), String> {
        self.close_native_permissions(&operation.id).await;
        // A cancelled session/new/load may still finish in the adapter. Retire
        // that process so no late replay/configuration can become reusable state.
        let stopped = if operation.loading.load(Ordering::Acquire) {
            self.stop_agent().await
        } else {
            Ok(())
        };
        // Cancellation can arrive after local message staging but before the
        // transport starts. Restore that exact unsent turn to the paused outbox.
        if let Some(prompt) = self.prompt.lock().await.take() {
            self.store.lock().await.transaction(|data| {
                if let Some(conversation) = data
                    .conversations
                    .iter_mut()
                    .find(|item| item.id == prompt.conversation_id)
                {
                    if let Some(index) = conversation
                        .messages
                        .iter()
                        .position(|message| message.id == prompt.assistant_message_id)
                    {
                        conversation.messages.remove(index);
                        if index > 0 && conversation.messages[index - 1].role == "user" {
                            let user = conversation.messages.remove(index - 1);
                            data.pending_message = Some(crate::workspace::PendingMessage {
                                text: user.text,
                                conversation_id: Some(conversation.id.clone()),
                                paused: true,
                            });
                        }
                    }
                }
                Ok(())
            })?;
        }
        self.pause_outbox(operation.conversation_id.as_deref())
            .await?;
        operation.timing.finish(TimingOutcome::Cancelled);
        self.state_changed().await;
        stopped
    }
}

impl Preparation {
    pub fn check(&self) -> Result<(), String> {
        if self.cancelled.load(Ordering::Acquire) {
            Err("Préparation annulée.".into())
        } else {
            Ok(())
        }
    }
    pub async fn cancelled(&self) {
        let notified = self.wake.notified();
        tokio::pin!(notified);
        notified.as_mut().enable();
        if !self.cancelled.load(Ordering::Acquire) {
            notified.await;
        }
    }
    pub fn phase(&self, phase: TurnPhase) {
        self.timing.phase(phase);
    }
}
