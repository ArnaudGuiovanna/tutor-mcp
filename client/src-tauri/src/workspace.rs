// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

use crate::discovery::AgentTarget;
use serde::{Deserialize, Serialize};
use std::path::{Path, PathBuf};

pub fn now() -> String {
    chrono::Utc::now().to_rfc3339_opts(chrono::SecondsFormat::Millis, true)
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct TutorSettings {
    pub mode: String,
    #[serde(default = "default_activity")]
    pub activity: String,
    pub url: String,
    pub command: String,
    pub args: Vec<String>,
}

fn default_activity() -> String {
    "auto".into()
}

impl Default for TutorSettings {
    fn default() -> Self {
        Self {
            mode: "remote".into(),
            activity: default_activity(),
            url: option_env!("TUTOR_DEFAULT_INSTITUTION")
                .unwrap_or("")
                .into(),
            command: String::new(),
            args: vec!["--local".into()],
        }
    }
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct Appearance {
    pub theme: String,
    pub font_size: u8,
    pub line_height: f64,
    pub reduce_motion: bool,
}
impl Default for Appearance {
    fn default() -> Self {
        Self {
            theme: "dark".into(),
            font_size: 20,
            line_height: 1.65,
            reduce_motion: false,
        }
    }
}
#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct AgentSettings {
    pub model: String,
    pub reasoning_effort: String,
    pub speed: String,
}
impl Default for AgentSettings {
    fn default() -> Self {
        Self {
            model: String::new(),
            reasoning_effort: String::new(),
            speed: "balanced".into(),
        }
    }
}
#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct Settings {
    pub target_id: String,
    pub manual_target: Option<AgentTarget>,
    pub cwd: String,
    pub tutor: TutorSettings,
    pub language: String,
    pub appearance: Appearance,
    pub agent: AgentSettings,
    pub permission_policy: Option<crate::permissions::PermissionPolicy>,
}
impl Default for Settings {
    fn default() -> Self {
        Self {
            target_id: String::new(),
            manual_target: None,
            cwd: String::new(),
            tutor: TutorSettings::default(),
            language: "fr".into(),
            appearance: Appearance::default(),
            agent: AgentSettings::default(),
            permission_policy: None,
        }
    }
}

#[derive(Clone, Debug, Default, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct LearningIntent {
    pub id: String,
    pub title: String,
    pub motivation: String,
    pub starting_point: String,
    pub goals: String,
    pub approach: String,
    pub constraints: String,
    pub design_criteria: String,
    pub validated: bool,
    pub revision: u64,
    pub created_at: String,
    pub updated_at: String,
    pub domain_id: Option<String>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Message {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub training_context: Option<serde_json::Value>,
    pub id: String,
    pub role: String,
    pub text: String,
    pub created_at: String,
    pub status: String,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Conversation {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub active_training: Option<serde_json::Value>,
    pub id: String,
    pub title: String,
    pub intent_id: Option<String>,
    pub agent_session_id: Option<String>,
    pub target_id: String,
    pub tutor_binding: String,
    #[serde(default)]
    pub runtime_binding: String,
    pub created_at: String,
    pub updated_at: String,
    pub messages: Vec<Message>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct PendingMessage {
    pub text: String,
    pub conversation_id: Option<String>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Workspace {
    #[serde(default)]
    pub pending_message: Option<PendingMessage>,
    pub schema_version: u32,
    pub settings: Settings,
    pub intents: Vec<LearningIntent>,
    pub conversations: Vec<Conversation>,
    pub active_conversation_id: Option<String>,
}

impl Default for Workspace {
    fn default() -> Self {
        Self {
            schema_version: 1,
            pending_message: None,
            settings: Settings::default(),
            intents: vec![],
            conversations: vec![],
            active_conversation_id: None,
        }
    }
}

#[cfg(test)]
std::thread_local! {
    static FAIL_NEXT_JOURNAL_SYNC: std::cell::Cell<bool> = const { std::cell::Cell::new(false) };
    static FAIL_NEXT_JOURNAL_TRUNCATE: std::cell::Cell<bool> = const { std::cell::Cell::new(false) };
}

pub struct WorkspaceStore {
    path: PathBuf,
    pub data: Workspace,
    journal_synced: std::time::Instant,
    encoded_bytes: std::cell::Cell<usize>,
    journal_failed: std::cell::Cell<bool>,
}

impl WorkspaceStore {
    pub fn load(directory: &Path) -> Result<Self, String> {
        std::fs::create_dir_all(directory)
            .map_err(|_| "Impossible de créer le dossier de données Tutor.")?;
        let path = directory.join("workspace.json");
        let mut data: Workspace = match std::fs::read(&path) {
            Ok(bytes) => {
                if bytes.len() > 32 * 1024 * 1024 {
                    return Err("Les données locales dépassent la taille autorisée.".into());
                }
                serde_json::from_slice(&bytes).map_err(|_| "Les données locales sont illisibles. Conservez workspace.json avant toute récupération.")?
            }
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Workspace::default(),
            Err(_) => return Err("Impossible de lire les données locales.".into()),
        };
        if data.schema_version != 1 {
            return Err("Ce format de données nécessite une autre version de Tutor.".into());
        }
        let journal = path.with_extension("stream.jsonl");
        let replayed = replay_stream_journal(&journal, &mut data)?;
        // A crashed agent cannot keep a message in a fictitious streaming state.
        for conversation in &mut data.conversations {
            for message in &mut conversation.messages {
                if message.status == "streaming" {
                    message.status = "interrupted".into();
                }
            }
        }
        let encoded_bytes = serde_json::to_vec_pretty(&data)
            .map_err(|_| "Données de conversation invalides.")?
            .len();
        let store = Self {
            path,
            data,
            journal_synced: std::time::Instant::now(),
            encoded_bytes: std::cell::Cell::new(encoded_bytes),
            journal_failed: std::cell::Cell::new(false),
        };
        if replayed {
            store.save()?;
        }
        Ok(store)
    }

    pub fn save(&self) -> Result<(), String> {
        let bytes = serde_json::to_vec_pretty(&self.data)
            .map_err(|_| "Impossible de préparer les données locales.")?;
        if bytes.len() > 32 * 1024 * 1024 {
            return Err(
                "Le stockage local a atteint sa limite de 32 Mo. Les échanges déjà enregistrés sont conservés ; consultez le guide de sauvegarde avant de poursuivre."
                    .into(),
            );
        }
        private_write(&self.path, &bytes)?;
        self.encoded_bytes.set(bytes.len());
        // Offsets make replay idempotent if a crash happens between replacement
        // and unlink. If cleanup fails, preserve the checkpoint but refuse new
        // appends into a possibly partial old journal until cleanup succeeds.
        let cleaned = match std::fs::remove_file(self.path.with_extension("stream.jsonl")) {
            Ok(()) => true,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => true,
            Err(_) => false,
        };
        self.journal_failed.set(!cleaned);
        Ok(())
    }

    /// Append only the new response bytes. The full workspace is checkpointed
    /// at prompt completion/cancellation/shutdown, not for every token. A crash
    /// replays complete journal records against their exact byte offsets.
    pub fn append_stream(
        &mut self,
        conversation_id: &str,
        message_id: &str,
        text: &str,
    ) -> Result<bool, String> {
        use std::io::Write;
        if self.journal_failed.get() {
            return Err("Le journal de réponse doit être sauvegardé avant de poursuivre.".into());
        }
        if !self.path.is_file() {
            return Err("Le stockage de conversation est indisponible.".into());
        }
        let conversation = self
            .data
            .conversations
            .iter()
            .position(|c| c.id == conversation_id)
            .ok_or("Séance introuvable.")?;
        let message = self.data.conversations[conversation]
            .messages
            .iter()
            .position(|m| m.id == message_id)
            .ok_or("Réponse introuvable.")?;
        let current = &self.data.conversations[conversation].messages[message];
        const LIMIT: usize = 2 * 1024 * 1024;
        let mut length = text.len().min(LIMIT.saturating_sub(current.text.len()));
        while !text.is_char_boundary(length) {
            length -= 1;
        }
        let record = StreamDelta {
            conversation_id: conversation_id.into(),
            message_id: message_id.into(),
            offset: current.text.len(),
            text: text[..length].into(),
        };
        // Count only escaped new text, preserving the workspace bound without
        // reserializing its history for every response fragment.
        let added = serde_json::to_vec(&record.text)
            .map_err(|_| "Fragment de réponse invalide.")?
            .len()
            .saturating_sub(2);
        let next_size = self.encoded_bytes.get().saturating_add(added);
        if next_size > 32 * 1024 * 1024 - 1024 {
            return Err("Le stockage local approche sa limite de 32 Mo. La réponse a été interrompue pour préserver la conversation.".into());
        }
        let path = self.path.with_extension("stream.jsonl");
        if std::fs::symlink_metadata(&path)
            .is_ok_and(|m| !m.is_file() || m.len() > 16 * 1024 * 1024)
        {
            return Err("Le journal de réponse est invalide ou trop volumineux.".into());
        }
        let mut options = std::fs::OpenOptions::new();
        options.create(true).append(true);
        #[cfg(unix)]
        {
            use std::os::unix::fs::OpenOptionsExt;
            options.mode(0o600);
        }
        let mut file = options
            .open(&path)
            .map_err(|_| "Impossible d’enregistrer le fragment de réponse.")?;
        let bytes = serde_json::to_vec(&record).map_err(|_| "Fragment de réponse invalide.")?;
        let mut before = file
            .metadata()
            .map_err(|_| "Journal de réponse illisible.")?
            .len();
        if before.saturating_add(bytes.len() as u64).saturating_add(1) > 16 * 1024 * 1024 {
            // Tiny provider chunks can have substantial journal overhead.
            // Compact occasionally instead of shortening the allowed response.
            drop(file);
            self.save()?;
            if self.journal_failed.get() {
                return Err("Impossible de préparer le prochain fragment de réponse.".into());
            }
            file = options
                .open(&path)
                .map_err(|_| "Impossible d’enregistrer le fragment de réponse.")?;
            before = 0;
        }
        let sync = self.journal_synced.elapsed() >= std::time::Duration::from_millis(250);
        let result = (|| -> std::io::Result<()> {
            file.write_all(&bytes)?;
            if sync {
                #[cfg(test)]
                if FAIL_NEXT_JOURNAL_SYNC.with(|flag| flag.replace(false)) {
                    return Err(std::io::Error::other("injected journal sync failure"));
                }
                file.sync_data()?;
            }
            // A newline is the commit marker. A failed write/sync has no marker
            // and is ignored during recovery even if truncation also fails.
            file.write_all(b"\n")?;
            Ok(())
        })();
        if result.is_err() {
            #[cfg(test)]
            let rollback_allowed = !FAIL_NEXT_JOURNAL_TRUNCATE.with(|flag| flag.replace(false));
            #[cfg(not(test))]
            let rollback_allowed = true;
            if rollback_allowed {
                let _ = file.set_len(before);
            }
            self.journal_failed.set(true);
            return Err(
                "Impossible d’enregistrer le fragment de réponse. La réponse a été interrompue."
                    .into(),
            );
        }
        if sync {
            self.journal_synced = std::time::Instant::now();
        }
        let conversation = &mut self.data.conversations[conversation];
        let message = &mut conversation.messages[message];
        message.text.push_str(&record.text);
        let truncated = length < text.len();
        if truncated {
            message.status = "interrupted".into();
        }
        conversation.updated_at = now();
        self.encoded_bytes.set(next_size);
        Ok(truncated)
    }

    /// Commit an in-memory change only if its atomic file replacement succeeds.
    /// A failed disk write must not create a second, fictitious workspace visible
    /// to IPC callers or silently saved by an unrelated later operation.
    pub fn transaction<T>(
        &mut self,
        update: impl FnOnce(&mut Workspace) -> Result<T, String>,
    ) -> Result<T, String> {
        let previous = self.data.clone();
        let result = update(&mut self.data).and_then(|value| self.save().map(|_| value));
        if result.is_err() {
            self.data = previous;
        }
        result
    }

    pub fn save_intent(&mut self, mut intent: LearningIntent) -> Result<LearningIntent, String> {
        let fields = [
            &intent.title,
            &intent.motivation,
            &intent.starting_point,
            &intent.goals,
            &intent.approach,
            &intent.constraints,
            &intent.design_criteria,
        ];
        if intent.title.trim().is_empty() || fields.iter().any(|f| f.len() > 16_000) {
            return Err(
                "Donnez un titre à votre intention et limitez chaque champ à 16 000 caractères."
                    .into(),
            );
        }
        let index = self.data.intents.iter().position(|i| i.id == intent.id);
        match index {
            Some(index) => {
                let previous = &self.data.intents[index];
                if previous.revision != intent.revision {
                    return Err(
                        "Cette intention a changé. Rechargez-la avant de l’enregistrer.".into(),
                    );
                }
                // IDs, creation time and server bindings are never taken from generated text.
                intent.created_at.clone_from(&previous.created_at);
                intent.domain_id.clone_from(&previous.domain_id);
                intent.revision += 1;
            }
            None => {
                if !intent.id.is_empty() {
                    return Err("Cette intention n’existe plus.".into());
                }
                intent.id = uuid::Uuid::new_v4().to_string();
                intent.created_at = now();
                intent.domain_id = None;
                intent.revision = 1;
            }
        }
        intent.updated_at = now();
        let previous = self.data.clone();
        match index {
            Some(i) => self.data.intents[i] = intent.clone(),
            None => self.data.intents.push(intent.clone()),
        }
        if let Err(error) = self.save() {
            self.data = previous;
            return Err(error);
        }
        Ok(intent)
    }
}

#[derive(Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct StreamDelta {
    conversation_id: String,
    message_id: String,
    offset: usize,
    text: String,
}
fn replay_stream_journal(path: &Path, data: &mut Workspace) -> Result<bool, String> {
    let metadata = match std::fs::symlink_metadata(path) {
        Ok(m) => m,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(false),
        Err(_) => return Err("Impossible de lire le journal de réponse.".into()),
    };
    if !metadata.is_file() || metadata.len() > 16 * 1024 * 1024 {
        return Err(
            "Journal de réponse invalide ; conservez les données avant récupération.".into(),
        );
    }
    let bytes = std::fs::read(path).map_err(|_| "Impossible de lire le journal de réponse.")?;
    for line in bytes.split_inclusive(|b| *b == b'\n') {
        if !line.ends_with(b"\n") {
            break;
        } // interrupted final write was never emitted
        let r: StreamDelta = serde_json::from_slice(line)
            .map_err(|_| "Le journal de réponse est illisible. Conservez-le avant récupération.")?;
        let message = data
            .conversations
            .iter_mut()
            .find(|c| c.id == r.conversation_id)
            .and_then(|c| c.messages.iter_mut().find(|m| m.id == r.message_id))
            .ok_or("La réponse du journal est introuvable.")?;
        if r.offset
            .checked_add(r.text.len())
            .is_none_or(|end| end > 2 * 1024 * 1024)
        {
            return Err("Journal de réponse trop volumineux.".into());
        }
        if message.text.len() == r.offset {
            message.text.push_str(&r.text);
        } else if message.text.get(r.offset..r.offset + r.text.len()) != Some(&r.text) {
            return Err("Le journal et la conversation ne correspondent pas. Conservez les deux avant récupération.".into());
        }
    }
    Ok(true)
}

/// Write through a sibling temporary file. Never truncate an existing workspace
/// before the replacement is complete. The Windows replacement uses MoveFileEx.
pub fn private_write(path: &Path, bytes: &[u8]) -> Result<(), String> {
    use std::io::Write;
    let parent = path.parent().ok_or("Chemin de stockage invalide.")?;
    std::fs::create_dir_all(parent).map_err(|_| "Impossible de créer le stockage local.")?;
    let temporary = path.with_extension(format!("{}.tmp", uuid::Uuid::new_v4()));
    let mut options = std::fs::OpenOptions::new();
    options.write(true).create_new(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.mode(0o600);
    }
    let result = (|| {
        let mut file = options
            .open(&temporary)
            .map_err(|_| "Impossible de créer le fichier temporaire.")?;
        file.write_all(bytes)
            .and_then(|_| file.sync_all())
            .map_err(|_| "Impossible d’enregistrer les données.")?;
        drop(file);
        #[cfg(windows)]
        {
            use std::os::windows::ffi::OsStrExt;
            use windows_sys::Win32::Storage::FileSystem::{
                MoveFileExW, MOVEFILE_REPLACE_EXISTING, MOVEFILE_WRITE_THROUGH,
            };
            let from: Vec<u16> = temporary.as_os_str().encode_wide().chain(Some(0)).collect();
            let to: Vec<u16> = path.as_os_str().encode_wide().chain(Some(0)).collect();
            if unsafe {
                MoveFileExW(
                    from.as_ptr(),
                    to.as_ptr(),
                    MOVEFILE_REPLACE_EXISTING | MOVEFILE_WRITE_THROUGH,
                )
            } == 0
            {
                return Err("Impossible de remplacer les données locales.".into());
            }
        }
        #[cfg(not(windows))]
        std::fs::rename(&temporary, path)
            .map_err(|_| "Impossible de remplacer les données locales.")?;
        Ok(())
    })();
    if result.is_err() {
        let _ = std::fs::remove_file(&temporary);
    }
    result
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn old_settings_gain_language_and_reading_defaults_without_replacing_data() {
        let settings:Settings=serde_json::from_value(serde_json::json!({"targetId":"existing-agent","cwd":"existing-work","tutor":{"mode":"remote","url":"https://institution.example"}})).unwrap();
        assert_eq!(settings.language, "fr");
        assert_eq!(settings.appearance.font_size, 20);
        assert_eq!(settings.appearance.theme, "dark");
        assert_eq!(settings.target_id, "existing-agent");
    }
    #[test]
    fn response_journal_recovers_unicode_without_rewriting_or_duplicating_history() {
        let dir = tempfile::tempdir().unwrap();
        let mut store = WorkspaceStore::load(dir.path()).unwrap();
        store.data.conversations.push(Conversation {
            id: "conversation".into(),
            title: "Existing".into(),
            intent_id: None,
            agent_session_id: None,
            target_id: "test".into(),
            tutor_binding: String::new(),
            runtime_binding: String::new(),
            created_at: now(),
            updated_at: now(),
            active_training: None,
            messages: vec![Message {
                id: "assistant".into(),
                role: "assistant".into(),
                text: String::new(),
                status: "streaming".into(),
                created_at: now(),
                training_context: None,
            }],
        });
        store.save().unwrap();
        let checkpoint = std::fs::read(dir.path().join("workspace.json")).unwrap();
        for _ in 0..40 {
            store
                .append_stream("conversation", "assistant", "élève 🦀 ")
                .unwrap();
        }
        assert_eq!(
            std::fs::read(dir.path().join("workspace.json")).unwrap(),
            checkpoint
        );
        let journal = std::fs::read(dir.path().join("workspace.stream.jsonl")).unwrap();
        drop(store);
        let recovered = WorkspaceStore::load(dir.path()).unwrap();
        assert_eq!(
            recovered.data.conversations[0].messages[0].text,
            "élève 🦀 ".repeat(40)
        );
        assert_eq!(
            recovered.data.conversations[0].messages[0].status,
            "interrupted"
        );
        assert!(!dir.path().join("workspace.stream.jsonl").exists());
        // Simulate a crash between checkpoint replacement and journal unlink.
        std::fs::write(dir.path().join("workspace.stream.jsonl"), journal).unwrap();
        let twice = WorkspaceStore::load(dir.path()).unwrap();
        assert_eq!(
            twice.data.conversations[0].messages[0].text,
            recovered.data.conversations[0].messages[0].text
        );
    }

    #[test]
    fn failed_journal_sync_and_rollback_never_recover_an_uncommitted_fragment() {
        let dir = tempfile::tempdir().unwrap();
        let mut store = WorkspaceStore::load(dir.path()).unwrap();
        store.data.conversations.push(Conversation {
            id: "c".into(),
            title: "Existing".into(),
            intent_id: None,
            agent_session_id: None,
            target_id: "test".into(),
            tutor_binding: String::new(),
            runtime_binding: String::new(),
            created_at: now(),
            updated_at: now(),
            active_training: None,
            messages: vec![Message {
                id: "a".into(),
                role: "assistant".into(),
                text: String::new(),
                status: "streaming".into(),
                created_at: now(),
                training_context: None,
            }],
        });
        store.save().unwrap();
        store.append_stream("c", "a", "Committed. ").unwrap();
        let encoded = store.encoded_bytes.get();
        store.journal_synced = std::time::Instant::now() - std::time::Duration::from_secs(1);
        FAIL_NEXT_JOURNAL_SYNC.with(|flag| flag.set(true));
        FAIL_NEXT_JOURNAL_TRUNCATE.with(|flag| flag.set(true));
        assert!(store.append_stream("c", "a", "Failed fragment.").is_err());
        assert_eq!(store.data.conversations[0].messages[0].text, "Committed. ");
        assert_eq!(store.encoded_bytes.get(), encoded);
        assert!(store
            .append_stream("c", "a", "Cannot append after failure")
            .is_err());
        drop(store);
        let recovered = WorkspaceStore::load(dir.path()).unwrap();
        assert_eq!(
            recovered.data.conversations[0].messages[0].text,
            "Committed. "
        );
        assert_eq!(
            recovered.data.conversations[0].messages[0].status,
            "interrupted"
        );
    }

    #[test]
    fn many_small_fragments_compact_the_journal_without_shortening_the_response() {
        let dir = tempfile::tempdir().unwrap();
        let mut store = WorkspaceStore::load(dir.path()).unwrap();
        store.data.conversations.push(Conversation {
            id: "c".into(),
            title: "Existing".into(),
            intent_id: None,
            agent_session_id: None,
            target_id: "test".into(),
            tutor_binding: String::new(),
            runtime_binding: String::new(),
            created_at: now(),
            updated_at: now(),
            active_training: None,
            messages: vec![Message {
                id: "a".into(),
                role: "assistant".into(),
                text: String::new(),
                status: "streaming".into(),
                created_at: now(),
                training_context: None,
            }],
        });
        store.save().unwrap();
        let mut empty = serde_json::to_vec(&StreamDelta {
            conversation_id: "c".into(),
            message_id: "a".into(),
            offset: 0,
            text: String::new(),
        })
        .unwrap();
        empty.push(b'\n');
        let repeated = empty.repeat((16 * 1024 * 1024 - 8) / empty.len());
        let journal = dir.path().join("workspace.stream.jsonl");
        std::fs::write(&journal, repeated).unwrap();
        store
            .append_stream("c", "a", "Response continues after compaction.")
            .unwrap();
        assert!(std::fs::metadata(&journal).unwrap().len() < 1024);
        drop(store);
        let restored = WorkspaceStore::load(dir.path()).unwrap();
        assert_eq!(
            restored.data.conversations[0].messages[0].text,
            "Response continues after compaction."
        );
    }

    #[test]
    fn response_journal_preserves_a_readable_checkpoint_at_the_workspace_limit() {
        let dir = tempfile::tempdir().unwrap();
        let mut store = WorkspaceStore::load(dir.path()).unwrap();
        store.data.conversations.push(Conversation {
            id: "c".into(),
            title: "Existing".into(),
            intent_id: None,
            agent_session_id: None,
            target_id: "test".into(),
            tutor_binding: String::new(),
            runtime_binding: String::new(),
            created_at: now(),
            updated_at: now(),
            active_training: None,
            messages: vec![Message {
                id: "a".into(),
                role: "assistant".into(),
                text: String::new(),
                status: "streaming".into(),
                created_at: now(),
                training_context: None,
            }],
        });
        store.data.conversations[0].messages.insert(
            0,
            Message {
                id: "large-history".into(),
                role: "user".into(),
                text: "x".repeat(32 * 1024 * 1024 - 3000),
                status: "complete".into(),
                created_at: now(),
                training_context: None,
            },
        );
        store.save().unwrap();
        assert!(store.append_stream("c", "a", &"é".repeat(2000)).is_err());
        assert!(WorkspaceStore::load(dir.path()).is_ok());
    }

    #[test]
    fn validated_intent_revisions_survive_restart_and_conflicts_do_not_overwrite() {
        let dir = tempfile::tempdir().unwrap();
        let mut store = WorkspaceStore::load(dir.path()).unwrap();
        let one = store
            .save_intent(LearningIntent {
                title: "Histoire des fruits".into(),
                validated: true,
                ..Default::default()
            })
            .unwrap();
        let mut second = one.clone();
        second.constraints = "Séances de 15 minutes".into();
        store.save_intent(second).unwrap();
        assert!(store.save_intent(one).is_err());
        let loaded = WorkspaceStore::load(dir.path()).unwrap();
        assert_eq!(loaded.data.intents[0].constraints, "Séances de 15 minutes");
        assert_eq!(loaded.data.intents[0].revision, 2);
    }
    #[test]
    fn corrupted_file_is_preserved_and_never_replaced_by_empty_state() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("workspace.json"), b"broken").unwrap();
        assert!(WorkspaceStore::load(dir.path()).is_err());
        assert_eq!(
            std::fs::read(dir.path().join("workspace.json")).unwrap(),
            b"broken"
        );
    }
}
