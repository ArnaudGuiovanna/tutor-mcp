// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! One durable journal per ACP session. Stable verified owner identifiers are
//! distinct from access tokens and catalog permission epochs. This synchronous
//! API belongs on the blocking worker, never the ACP event loop.

use crate::{
    context::ProjectionAudit,
    journal::{Binding, Journal, JournalError},
};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::{
    collections::HashMap,
    fmt,
    path::{Path, PathBuf},
};

const MAX_USER_BYTES: usize = 64 * 1024;
const MAX_ASSISTANT_BYTES: usize = 64 * 1024;
const MAX_EVENTS_PER_TURN: usize = 4096;

#[derive(Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields, rename_all = "camelCase")]
pub struct Owner {
    pub profile_id: String,
    pub account_id: String,
    pub tenant_id: String,
    pub server_id: String,
}

impl Owner {
    fn binding(&self) -> Result<Binding<'_>, StoreError> {
        for value in [
            &self.profile_id,
            &self.account_id,
            &self.tenant_id,
            &self.server_id,
        ] {
            if value.is_empty() || value.len() > 2048 || value.chars().any(char::is_control) {
                return Err(StoreError::InvalidInput);
            }
        }
        Ok(Binding {
            profile_id: &self.profile_id,
            account_id: &self.account_id,
            tenant_id: &self.tenant_id,
            server_id: &self.server_id,
        })
    }

    fn directory(&self, root: &Path) -> Result<PathBuf, StoreError> {
        self.binding()?;
        if !root.is_absolute() {
            return Err(StoreError::InvalidInput);
        }
        let bytes = serde_json::to_vec(self).map_err(|_| StoreError::InvalidInput)?;
        let mut hash = Sha256::new();
        hash.update(b"tutor-session-owner-v1\0");
        hash.update(bytes);
        Ok(root.join(format!("{:x}", hash.finalize())))
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum StoreError {
    InvalidInput,
    Storage,
    MissingSession,
    ModelMismatch,
    CorruptSession,
    FinishedTurn,
    TurnInProgress,
    OutputLimit,
    Journal(JournalError),
}

impl fmt::Display for StoreError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            Self::InvalidInput => "session_store_invalid_input",
            Self::Storage => "session_store_failed",
            Self::MissingSession => "session_not_found_for_owner",
            Self::ModelMismatch => "session_model_changed",
            Self::CorruptSession => "session_journal_invalid",
            Self::FinishedTurn => "session_turn_already_finished",
            Self::TurnInProgress => "session_turn_in_progress",
            Self::OutputLimit => "session_output_limit",
            Self::Journal(_) => "session_journal_failed",
        })
    }
}
impl std::error::Error for StoreError {}
impl From<JournalError> for StoreError {
    fn from(error: JournalError) -> Self {
        Self::Journal(error)
    }
}

#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Header {
    version: u32,
    session_id: String,
    model_sha256: String,
}

#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct UserTurn {
    user: String,
}

#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Chunk {
    text: String,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Completion {
    Complete,
    Interrupted,
    Failed,
}

pub struct StoredTurn {
    pub id: String,
    pub user: String,
    pub assistant: String,
    pub status: Completion,
}

pub struct SessionStore {
    id: String,
    journal: Journal,
    // Bounded to unfinished turns; avoid rereading every previous token for
    // each new streamed chunk. Reopening reconstructs this only when needed.
    output_bytes: HashMap<String, usize>,
}

impl SessionStore {
    pub fn create(root: &Path, owner: &Owner, model_sha256: &str) -> Result<Self, StoreError> {
        valid_digest(model_sha256)?;
        let directory = owner.directory(root)?;
        private_directory(&directory)?;
        let id = uuid::Uuid::new_v4().to_string();
        let mut journal =
            Journal::open(&directory.join(format!("{id}.sqlite3")), &owner.binding()?)?;
        let header = Header {
            version: 1,
            session_id: id.clone(),
            model_sha256: model_sha256.into(),
        };
        journal.append_event("session-header", None, "session_header", &encode(&header)?)?;
        Ok(Self {
            id,
            journal,
            output_bytes: HashMap::new(),
        })
    }

    pub fn load(
        root: &Path,
        owner: &Owner,
        id: &str,
        model_sha256: &str,
    ) -> Result<Self, StoreError> {
        valid_digest(model_sha256)?;
        let parsed = uuid::Uuid::parse_str(id).map_err(|_| StoreError::InvalidInput)?;
        if parsed.to_string() != id {
            return Err(StoreError::InvalidInput);
        }
        let path = owner.directory(root)?.join(format!("{id}.sqlite3"));
        if !path.is_file() {
            return Err(StoreError::MissingSession);
        }
        let journal = Journal::open(&path, &owner.binding()?)?;
        let event = journal
            .event("session-header")?
            .ok_or(StoreError::CorruptSession)?;
        let header: Header = decode(&event.payload)?;
        if header.version != 1 || header.session_id != id {
            return Err(StoreError::CorruptSession);
        }
        if header.model_sha256 != model_sha256 {
            return Err(StoreError::ModelMismatch);
        }
        Ok(Self {
            id: id.into(),
            journal,
            output_bytes: HashMap::new(),
        })
    }

    pub fn id(&self) -> &str {
        &self.id
    }
    pub fn journal(&self) -> &Journal {
        &self.journal
    }
    pub fn journal_mut(&mut self) -> &mut Journal {
        &mut self.journal
    }

    /// Commit the exact input before inference or any operation is started.
    pub fn begin_turn(&mut self, user: &str) -> Result<String, StoreError> {
        let id = uuid::Uuid::new_v4().to_string();
        self.begin_turn_with_id(&id, user)?;
        Ok(id)
    }

    pub fn begin_turn_with_id(&mut self, id: &str, user: &str) -> Result<(), StoreError> {
        let parsed = uuid::Uuid::parse_str(id).map_err(|_| StoreError::InvalidInput)?;
        if parsed.to_string() != id {
            return Err(StoreError::InvalidInput);
        }
        if !self.output_bytes.is_empty() {
            return Err(StoreError::TurnInProgress);
        }
        if user.trim().is_empty() || user.len() > MAX_USER_BYTES {
            return Err(StoreError::InvalidInput);
        }
        self.journal
            .append_turn(id, &encode(&UserTurn { user: user.into() })?)?;
        self.output_bytes.insert(id.into(), 0);
        Ok(())
    }

    /// Commit text before emitting it to ACP. No append is allowed after finish.
    pub fn append_assistant(&mut self, turn_id: &str, text: &str) -> Result<(), StoreError> {
        if text.is_empty() || text.len() > MAX_ASSISTANT_BYTES {
            return Err(StoreError::InvalidInput);
        }
        if !self.output_bytes.contains_key(turn_id) {
            self.require_unfinished(turn_id)?;
        }
        let current = match self.output_bytes.get(turn_id) {
            Some(bytes) => *bytes,
            None => self.read_turn(turn_id)?.assistant.len(),
        };
        if current + text.len() > MAX_ASSISTANT_BYTES {
            return Err(StoreError::OutputLimit);
        }
        self.journal.append_event(
            &uuid::Uuid::new_v4().to_string(),
            Some(turn_id),
            "assistant_chunk",
            &encode(&Chunk { text: text.into() })?,
        )?;
        self.output_bytes
            .insert(turn_id.into(), current + text.len());
        Ok(())
    }

    pub fn record_projection(
        &mut self,
        turn_id: &str,
        audit: &ProjectionAudit,
    ) -> Result<(), StoreError> {
        self.require_unfinished(turn_id)?;
        self.journal.append_event(
            &uuid::Uuid::new_v4().to_string(),
            Some(turn_id),
            "context_projection",
            &encode(audit)?,
        )?;
        Ok(())
    }

    pub fn finish_turn(&mut self, turn_id: &str, status: Completion) -> Result<(), StoreError> {
        // Immutable event identity makes repeated identical finalization harmless
        // and rejects attempts to rewrite a failed/interrupted turn as completed.
        self.journal.append_event(
            &format!("{turn_id}:finish"),
            Some(turn_id),
            "turn_finished",
            &encode(&status)?,
        )?;
        self.output_bytes.remove(turn_id);
        Ok(())
    }

    fn require_unfinished(&self, turn_id: &str) -> Result<(), StoreError> {
        if self.journal.turn(turn_id)?.is_none() {
            return Err(StoreError::CorruptSession);
        }
        if self.journal.event(&format!("{turn_id}:finish"))?.is_some() {
            return Err(StoreError::FinishedTurn);
        }
        Ok(())
    }

    pub fn read_turn(&self, turn_id: &str) -> Result<StoredTurn, StoreError> {
        let turn = self
            .journal
            .turn(turn_id)?
            .ok_or(StoreError::CorruptSession)?;
        let user: UserTurn = decode(&turn.payload)?;
        if user.user.trim().is_empty() || user.user.len() > MAX_USER_BYTES {
            return Err(StoreError::CorruptSession);
        }
        let mut cursor = None;
        let mut chunks = Vec::new();
        let mut bytes = 0usize;
        let mut scanned = 0usize;
        loop {
            let page = self.journal.event_refs(Some(turn_id), cursor, 128)?;
            if page.is_empty() {
                break;
            }
            scanned += page.len();
            if scanned > MAX_EVENTS_PER_TURN {
                return Err(StoreError::CorruptSession);
            }
            cursor = page.last().map(|e| e.sequence);
            for event in page {
                if event.kind == "assistant_chunk" {
                    let chunk: Chunk = decode(&self.journal.resolve_content(&event.content)?)?;
                    bytes = bytes
                        .checked_add(chunk.text.len())
                        .ok_or(StoreError::CorruptSession)?;
                    if bytes > MAX_ASSISTANT_BYTES {
                        return Err(StoreError::CorruptSession);
                    }
                    chunks.push(chunk.text);
                }
            }
        }
        let status = match self.journal.event(&format!("{turn_id}:finish"))? {
            Some(event) => decode(&event.payload)?,
            // A crash between the committed user input and completion does not
            // authorize inference or mutation replay during session recovery.
            None => Completion::Interrupted,
        };
        Ok(StoredTurn {
            id: turn_id.into(),
            user: user.user,
            assistant: chunks.into_iter().rev().collect(),
            status,
        })
    }

    /// Only the recent window is loaded; old exact content stays on disk.
    pub fn recent_turns(&self, limit: usize) -> Result<Vec<StoredTurn>, StoreError> {
        let refs = self.journal.turn_refs(None, limit)?;
        refs.into_iter()
            .rev()
            .map(|reference| self.read_turn(reference.content.id()))
            .collect()
    }
}

fn encode(value: &impl Serialize) -> Result<Vec<u8>, StoreError> {
    serde_json::to_vec(value).map_err(|_| StoreError::InvalidInput)
}
fn decode<T: for<'a> Deserialize<'a>>(value: &[u8]) -> Result<T, StoreError> {
    serde_json::from_slice(value).map_err(|_| StoreError::CorruptSession)
}
fn valid_digest(value: &str) -> Result<(), StoreError> {
    if value.len() != 64
        || !value
            .bytes()
            .all(|b| b.is_ascii_hexdigit() && !b.is_ascii_uppercase())
    {
        return Err(StoreError::InvalidInput);
    }
    Ok(())
}
fn private_directory(path: &Path) -> Result<(), StoreError> {
    let mut builder = std::fs::DirBuilder::new();
    builder.recursive(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::DirBuilderExt;
        builder.mode(0o700);
    }
    builder.create(path).map_err(|_| StoreError::Storage)?;
    let metadata = path.symlink_metadata().map_err(|_| StoreError::Storage)?;
    if !metadata.is_dir() || metadata.file_type().is_symlink() {
        return Err(StoreError::Storage);
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        if metadata.permissions().mode() & 0o077 != 0 {
            return Err(StoreError::Storage);
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    const MODEL: &str = crate::config::MODEL_SHA256;
    fn owner() -> Owner {
        Owner {
            profile_id: "profile1".into(),
            account_id: "account1".into(),
            tenant_id: "tenant1".into(),
            server_id: "https://school.example".into(),
        }
    }

    #[test]
    fn closed_process_can_restore_exact_text_and_completion() {
        let root = tempfile::tempdir().unwrap();
        let mut store = SessionStore::create(root.path(), &owner(), MODEL).unwrap();
        let session = store.id().to_owned();
        let turn = store.begin_turn("Élève : x = 2\n日本語").unwrap();
        store.append_assistant(&turn, "Première ").unwrap();
        store.append_assistant(&turn, "étape 🦀").unwrap();
        store.finish_turn(&turn, Completion::Complete).unwrap();
        store.finish_turn(&turn, Completion::Complete).unwrap();
        drop(store);
        let restored = SessionStore::load(root.path(), &owner(), &session, MODEL).unwrap();
        let turn = restored.read_turn(&turn).unwrap();
        assert_eq!(turn.user, "Élève : x = 2\n日本語");
        assert_eq!(turn.assistant, "Première étape 🦀");
        assert_eq!(turn.status, Completion::Complete);
    }

    #[test]
    fn unfinished_input_and_partial_output_restore_as_interrupted() {
        let root = tempfile::tempdir().unwrap();
        let mut store = SessionStore::create(root.path(), &owner(), MODEL).unwrap();
        let session = store.id().to_owned();
        let turn = store.begin_turn("Mon calcul exact").unwrap();
        store
            .append_assistant(&turn, "Une réponse partielle")
            .unwrap();
        drop(store);
        let restored = SessionStore::load(root.path(), &owner(), &session, MODEL).unwrap();
        let recovered = restored.read_turn(&turn).unwrap();
        assert_eq!(recovered.user, "Mon calcul exact");
        assert_eq!(recovered.assistant, "Une réponse partielle");
        assert_eq!(recovered.status, Completion::Interrupted);
        assert!(restored
            .journal()
            .operation_ids(crate::journal::OperationState::Dispatched, "", 128)
            .unwrap()
            .is_empty());
    }

    #[test]
    fn different_owner_or_model_cannot_resume_the_session() {
        let root = tempfile::tempdir().unwrap();
        let store = SessionStore::create(root.path(), &owner(), MODEL).unwrap();
        let session = store.id().to_owned();
        drop(store);
        let mut changed = owner();
        changed.account_id = "account2".into();
        assert!(matches!(
            SessionStore::load(root.path(), &changed, &session, MODEL),
            Err(StoreError::MissingSession)
        ));
        assert!(matches!(
            SessionStore::load(root.path(), &owner(), &session, &"0".repeat(64)),
            Err(StoreError::ModelMismatch)
        ));
        assert!(matches!(
            SessionStore::load(root.path(), &owner(), "../other", MODEL),
            Err(StoreError::InvalidInput)
        ));
        assert_eq!(std::fs::read_dir(root.path()).unwrap().count(), 1);
    }

    #[test]
    fn completion_is_immutable_and_late_chunks_are_refused() {
        let root = tempfile::tempdir().unwrap();
        let mut store = SessionStore::create(root.path(), &owner(), MODEL).unwrap();
        let turn = store.begin_turn("question").unwrap();
        assert!(matches!(
            store.begin_turn("parallel"),
            Err(StoreError::TurnInProgress)
        ));
        store.finish_turn(&turn, Completion::Failed).unwrap();
        assert!(store.finish_turn(&turn, Completion::Complete).is_err());
        assert!(matches!(
            store.append_assistant(&turn, "late"),
            Err(StoreError::FinishedTurn)
        ));
        assert_eq!(store.read_turn(&turn).unwrap().status, Completion::Failed);
        assert!(store.begin_turn("nouveau message explicite").is_ok());
    }

    #[test]
    fn output_limit_prevents_committing_an_unemitted_extra_chunk() {
        let root = tempfile::tempdir().unwrap();
        let mut store = SessionStore::create(root.path(), &owner(), MODEL).unwrap();
        let turn = store.begin_turn("question").unwrap();
        store
            .append_assistant(&turn, &"x".repeat(MAX_ASSISTANT_BYTES))
            .unwrap();
        assert!(matches!(
            store.append_assistant(&turn, "y"),
            Err(StoreError::OutputLimit)
        ));
        assert_eq!(
            store.read_turn(&turn).unwrap().assistant.len(),
            MAX_ASSISTANT_BYTES
        );
    }

    #[test]
    fn user_input_without_any_event_is_discoverable_after_reopen() {
        let root = tempfile::tempdir().unwrap();
        let mut store = SessionStore::create(root.path(), &owner(), MODEL).unwrap();
        let session = store.id().to_owned();
        let turn = store.begin_turn("Entrée exacte avant panne").unwrap();
        drop(store);
        let store = SessionStore::load(root.path(), &owner(), &session, MODEL).unwrap();
        let recent = store.recent_turns(24).unwrap();
        assert_eq!(recent.len(), 1);
        assert_eq!(recent[0].id, turn);
        assert_eq!(recent[0].user, "Entrée exacte avant panne");
        assert!(recent[0].assistant.is_empty());
        assert_eq!(recent[0].status, Completion::Interrupted);
    }
}
