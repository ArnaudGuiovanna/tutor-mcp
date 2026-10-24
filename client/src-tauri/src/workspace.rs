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
    pub url: String,
    pub command: String,
    pub args: Vec<String>,
}

impl Default for TutorSettings {
    fn default() -> Self {
        Self {
            mode: "remote".into(),
            url: String::new(),
            command: String::new(),
            args: vec!["--local".into()],
        }
    }
}

#[derive(Clone, Debug, Default, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct Settings {
    pub target_id: String,
    pub manual_target: Option<AgentTarget>,
    pub cwd: String,
    pub tutor: TutorSettings,
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
    pub id: String,
    pub role: String,
    pub text: String,
    pub created_at: String,
    pub status: String,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Conversation {
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
pub struct Workspace {
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
            settings: Settings::default(),
            intents: vec![],
            conversations: vec![],
            active_conversation_id: None,
        }
    }
}

pub struct WorkspaceStore {
    path: PathBuf,
    pub data: Workspace,
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
        // A crashed agent cannot keep a message in a fictitious streaming state.
        for conversation in &mut data.conversations {
            for message in &mut conversation.messages {
                if message.status == "streaming" {
                    message.status = "interrupted".into();
                }
            }
        }
        Ok(Self { path, data })
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
        private_write(&self.path, &bytes)
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
