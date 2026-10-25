// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! Local accounts own separate vaults, histories and agent processes. A label
//! never grants an institutional role; roles are returned by the server.
use crate::backend::{Backend, EventSink};
use crate::workspace::private_write;
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use std::path::{Path, PathBuf};
use std::sync::{
    atomic::{AtomicU64, Ordering},
    Arc, RwLock as MetadataLock,
};
use tokio::sync::{Mutex, RwLock};

const LEGACY: &str = "principal";
const MAX_PROFILES: usize = 20;

#[derive(Clone, Serialize, Deserialize)]
pub struct Profile {
    pub id: String,
    pub label: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub npub: Option<String>,
    #[serde(default)]
    pub reconnect: bool,
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::workspace::LearningIntent;

    #[tokio::test]
    async fn accounts_preserve_legacy_keys_and_isolate_histories_and_events() {
        let directory = tempfile::tempdir().unwrap();
        let original = Backend::new(directory.path(), Arc::new(|_, _| {})).unwrap();
        let identity = original.create_identity(None).await.unwrap();
        original
            .save_intent(LearningIntent {
                title: "Formation du propriétaire".into(),
                ..Default::default()
            })
            .await
            .unwrap();
        let identity_bytes = std::fs::read(directory.path().join("native-identity.json")).unwrap();
        let (events, mut receiver) = tokio::sync::mpsc::unbounded_channel();
        let profiles = ClientProfiles::new(
            directory.path(),
            Arc::new(move |name, value| {
                let _ = events.send((name.to_string(), value));
            }),
        )
        .unwrap();
        let initial = profiles.snapshot().await;
        assert_eq!(initial["profile"]["id"], LEGACY);
        assert_eq!(initial["identity"]["npub"], identity.npub.clone().unwrap());
        let old = profiles.current().await;
        let created = profiles.create("Compte apprenant".into()).await.unwrap();
        assert_eq!(created["identity"]["exists"], false);
        assert_eq!(created["intents"].as_array().unwrap().len(), 0);
        assert_eq!(created["conversations"].as_array().unwrap().len(), 0);
        let new_id = created["profile"]["id"].as_str().unwrap().to_string();
        let second = profiles
            .current()
            .await
            .create_identity(None)
            .await
            .unwrap();
        assert_ne!(second.npub, identity.npub);
        while receiver.try_recv().is_ok() {}
        old.save_intent(LearningIntent {
            title: "Ancien compte seulement".into(),
            ..Default::default()
        })
        .await
        .unwrap();
        assert!(
            receiver.try_recv().is_err(),
            "an inactive account emitted into the new UI"
        );
        assert_eq!(
            profiles.snapshot().await["intents"]
                .as_array()
                .unwrap()
                .len(),
            0
        );
        let restored = profiles.select(LEGACY.into()).await.unwrap();
        assert_eq!(restored["identity"]["npub"], identity.npub.unwrap());
        assert_eq!(restored["intents"].as_array().unwrap().len(), 2);
        assert_eq!(
            std::fs::read(directory.path().join("native-identity.json")).unwrap(),
            identity_bytes
        );
        profiles.select(new_id.clone()).await.unwrap();
        let reopened = ClientProfiles::new(directory.path(), Arc::new(|_, _| {})).unwrap();
        assert_eq!(reopened.snapshot().await["profile"]["id"], new_id);
        assert_eq!(
            reopened.snapshot().await["identity"]["npub"],
            second.npub.unwrap()
        );
        assert!(
            !std::fs::read_to_string(directory.path().join("profiles.json"))
                .unwrap()
                .contains("secret")
        );
    }

    #[tokio::test]
    async fn profile_names_never_grant_roles_and_invalid_switches_preserve_the_active_account() {
        let directory = tempfile::tempdir().unwrap();
        let profiles = ClientProfiles::new(directory.path(), Arc::new(|_, _| {})).unwrap();
        assert!(profiles.select("../another-account".into()).await.is_err());
        assert!(profiles.create("\n".into()).await.is_err());
        assert_eq!(profiles.snapshot().await["profile"]["id"], LEGACY);
        let state = profiles.create("Propriétaire".into()).await.unwrap();
        assert_eq!(state["tutorStatus"]["connected"], false);
        assert!(state.get("roles").is_none());
        assert_eq!(state["identity"]["exists"], false);
    }

    #[test]
    fn a_corrupted_or_escaping_index_is_not_replaced() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("profiles.json");
        let bytes=br#"{"version":1,"selected":"../outside","profiles":[{"id":"../outside","label":"test"}]}"#;
        std::fs::write(&path, bytes).unwrap();
        assert!(ClientProfiles::new(directory.path(), Arc::new(|_, _| {})).is_err());
        assert_eq!(std::fs::read(path).unwrap(), bytes);
    }
}
#[derive(Clone, Serialize, Deserialize)]
struct Index {
    version: u32,
    selected: String,
    profiles: Vec<Profile>,
}

#[derive(Clone)]
pub struct ProfileContext {
    id: String,
    epoch: u64,
    index: Arc<MetadataLock<Index>>,
}
impl ProfileContext {
    pub fn decorate(&self, value: &mut Value) {
        let index = self.index.read().unwrap_or_else(|error| error.into_inner());
        value["profile"] = index
            .profiles
            .iter()
            .find(|p| p.id == self.id)
            .map(|p| serde_json::to_value(p).expect("public profile serialization"))
            .unwrap_or(json!({"id":self.id,"label":"Compte"}));
        value["profiles"] =
            serde_json::to_value(&index.profiles).expect("public profiles serialization");
        value["profileEpoch"] = json!(self.epoch);
    }
}

pub struct ClientProfiles {
    root: PathBuf,
    index: Arc<MetadataLock<Index>>,
    active: RwLock<Arc<Backend>>,
    generation: Arc<AtomicU64>,
    operation: Mutex<()>,
    emit: EventSink,
}

impl ClientProfiles {
    pub fn new(root: &Path, emit: EventSink) -> Result<Arc<Self>, String> {
        std::fs::create_dir_all(root)
            .map_err(|_| "Impossible de créer le stockage des comptes.")?;
        let path = root.join("profiles.json");
        let index: Index = match std::fs::read(&path) {
            Ok(bytes) => {
                if bytes.len() > 65536 {
                    return Err("La liste des comptes est trop volumineuse.".into());
                }
                serde_json::from_slice(&bytes).map_err(|_|"La liste des comptes est illisible. Conservez profiles.json avant récupération.")?
            }
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => Index {
                version: 1,
                selected: LEGACY.into(),
                profiles: vec![Profile {
                    id: LEGACY.into(),
                    label: "Mon compte".into(),
                    npub: None,
                    reconnect: false,
                }],
            },
            Err(_) => return Err("Impossible de lire la liste des comptes.".into()),
        };
        Self::validate(&index)?;
        let selected = index.selected.clone();
        let index = Arc::new(MetadataLock::new(index));
        let generation = Arc::new(AtomicU64::new(1));
        let backend = Self::backend(
            root,
            &selected,
            index.clone(),
            generation.clone(),
            1,
            emit.clone(),
        )?;
        Ok(Arc::new(Self {
            root: root.into(),
            index,
            active: RwLock::new(backend),
            generation,
            operation: Mutex::new(()),
            emit,
        }))
    }

    fn validate(index: &Index) -> Result<(), String> {
        if index.version != 1 || index.profiles.is_empty() || index.profiles.len() > MAX_PROFILES {
            return Err("Format de comptes locaux invalide.".into());
        }
        let mut ids = std::collections::HashSet::new();
        for profile in &index.profiles {
            if (profile.id != LEGACY
                && !uuid::Uuid::parse_str(&profile.id).is_ok_and(|id| id.to_string() == profile.id))
                || !ids.insert(&profile.id)
                || profile.label.trim().is_empty()
                || profile.label.len() > 160
                || profile.label.chars().any(char::is_control)
            {
                return Err("Compte local invalide.".into());
            }
        }
        if !ids.contains(&index.selected) {
            return Err("Le compte actif est introuvable.".into());
        }
        Ok(())
    }

    fn directory(root: &Path, id: &str) -> PathBuf {
        // The original root stays in place: upgrading never moves/replaces a vault.
        if id == LEGACY {
            root.into()
        } else {
            root.join("profiles").join(id)
        }
    }

    fn backend(
        root: &Path,
        id: &str,
        index: Arc<MetadataLock<Index>>,
        generation: Arc<AtomicU64>,
        epoch: u64,
        emit: EventSink,
    ) -> Result<Arc<Backend>, String> {
        Backend::new_with_profile(
            &Self::directory(root, id),
            Arc::new(move |event, mut value| {
                if generation.load(Ordering::Acquire) == epoch {
                    if value.is_object() {
                        value["profileEpoch"] = json!(epoch)
                    }
                    (emit)(event, value)
                }
            }),
            Some(ProfileContext {
                id: id.into(),
                epoch,
                index,
            }),
        )
    }

    pub async fn current(&self) -> Arc<Backend> {
        self.active.read().await.clone()
    }
    pub async fn snapshot(&self) -> Value {
        self.current().await.snapshot().await
    }

    pub async fn create(&self, label: String) -> Result<Value, String> {
        let _operation = self.operation.lock().await;
        let current = self.current().await;
        let previous = current.snapshot().await;
        let mut index = self.index.read().unwrap_or_else(|e| e.into_inner()).clone();
        let id = uuid::Uuid::new_v4().to_string();
        index.profiles.push(Profile {
            id: id.clone(),
            label: label.trim().into(),
            npub: None,
            reconnect: false,
        });
        Self::validate(&index)?;
        // Only the non-secret institution address is shared. Histories, local
        // MCP arguments, credentials and working files are never inherited.
        let directory = Self::directory(&self.root, &id);
        std::fs::create_dir_all(directory.join("work"))
            .map_err(|_| "Impossible de créer le nouveau compte.")?;
        let candidate = Backend::new(&directory, Arc::new(|_, _| {}))?;
        let mut settings = candidate.settings().await;
        settings.cwd = directory.join("work").to_string_lossy().into_owned();
        if previous
            .pointer("/settings/tutor/mode")
            .and_then(Value::as_str)
            == Some("remote")
        {
            settings.tutor.url = previous
                .pointer("/settings/tutor/url")
                .and_then(Value::as_str)
                .unwrap_or("")
                .into();
        }
        candidate.save_settings(settings).await?;
        self.activate(index, id, current).await
    }

    pub async fn select(&self, id: String) -> Result<Value, String> {
        let _operation = self.operation.lock().await;
        let index = self.index.read().unwrap_or_else(|e| e.into_inner()).clone();
        if !index.profiles.iter().any(|p| p.id == id) {
            return Err("Compte local introuvable.".into());
        }
        if index.selected == id {
            return Ok(self.snapshot().await);
        }
        let current = self.current().await;
        self.activate(index, id, current).await
    }

    async fn activate(
        &self,
        mut next: Index,
        id: String,
        current: Arc<Backend>,
    ) -> Result<Value, String> {
        let epoch = self.generation.load(Ordering::Acquire) + 1;
        // Validate the destination vault before interrupting the old account.
        let backend = Self::backend(
            &self.root,
            &id,
            self.index.clone(),
            self.generation.clone(),
            epoch,
            self.emit.clone(),
        )?;
        current.shutdown().await;
        let old = current.snapshot().await;
        let disconnect = current.disconnect_tutor().await;
        if let Some(profile) = next.profiles.iter_mut().find(|p| p.id == next.selected) {
            profile.reconnect = old
                .pointer("/tutorStatus/connected")
                .and_then(Value::as_bool)
                == Some(true);
            profile.npub = old
                .pointer("/identity/npub")
                .and_then(Value::as_str)
                .map(str::to_string);
        }
        next.selected = id.clone();
        let reconnect = next
            .profiles
            .iter()
            .any(|profile| profile.id == id && profile.reconnect);
        let bytes = serde_json::to_vec_pretty(&next)
            .map_err(|_| "Impossible de préparer le compte actif.")?;
        private_write(&self.root.join("profiles.json"), &bytes)?;
        *self.index.write().unwrap_or_else(|e| e.into_inner()) = next;
        self.generation.store(epoch, Ordering::Release);
        *self.active.write().await = backend.clone();
        if reconnect && backend.settings().await.tutor.mode == "remote" {
            let settings = backend.settings().await;
            if let Err(error) = backend.connect_tutor(settings.tutor.url, false).await {
                backend.report_connection_error(error).await;
            }
        } else {
            backend.restore_tutor().await;
        }
        let mut snapshot = backend.snapshot().await;
        match disconnect {
            Err(error) => snapshot["profileSwitchWarning"] = Value::String(error),
            Ok(old) => {
                if let Some(message) = old.pointer("/tutorStatus/message").and_then(Value::as_str) {
                    snapshot["profileSwitchWarning"] = Value::String(message.into())
                }
            }
        }
        (self.emit)("client:state", snapshot.clone());
        Ok(snapshot)
    }

    pub async fn rename(&self, label: String) -> Result<Value, String> {
        let _operation = self.operation.lock().await;
        let mut next = self.index.read().unwrap_or_else(|e| e.into_inner()).clone();
        let selected = next.selected.clone();
        if let Some(profile) = next.profiles.iter_mut().find(|p| p.id == selected) {
            profile.label = label.trim().into()
        }
        Self::validate(&next)?;
        private_write(
            &self.root.join("profiles.json"),
            &serde_json::to_vec_pretty(&next)
                .map_err(|_| "Impossible de préparer le nom du compte.")?,
        )?;
        *self.index.write().unwrap_or_else(|e| e.into_inner()) = next;
        let snapshot = self.snapshot().await;
        (self.emit)("client:state", snapshot.clone());
        Ok(snapshot)
    }
}
