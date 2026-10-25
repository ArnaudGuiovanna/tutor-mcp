// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

use crate::acp::AcpClient;
use crate::discovery::{self, AgentTarget};
use crate::identity::{IdentityStore, KeyPair, PublicIdentity};
use crate::native_auth::{self, NativeCredential, Session};
use crate::workspace::{now, Conversation, LearningIntent, Message, Settings, WorkspaceStore};
use serde::Serialize;
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use std::collections::{HashMap, HashSet};
use std::path::{Path, PathBuf};
use std::sync::Arc;
use tokio::sync::{mpsc, oneshot, Mutex};

pub type EventSink = Arc<dyn Fn(&str, Value) + Send + Sync>;

#[derive(Clone, Default, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct TutorStatus {
    pub connected: bool,
    pub label: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub message: Option<String>,
}

struct Agent {
    rpc: AcpClient,
    target: AgentTarget,
    cwd: String,
    generation: String,
    capabilities: Value,
    auth_methods: Vec<Value>,
    events: mpsc::UnboundedSender<Value>,
    barriers: HashMap<String, oneshot::Sender<()>>,
    permissions: HashMap<String, (Value, String)>,
    replaying: bool,
    mcp_configurations: HashMap<String, String>,
}

#[derive(Clone)]
struct Prompt {
    conversation_id: String,
    session_id: String,
    assistant_message_id: String,
    generation: String,
    cancelled: bool,
    failure: Option<String>,
    truncated: bool,
}

#[derive(Default)]
struct Tutor {
    credentials: Option<Session>,
    status: TutorStatus,
    binding: String,
    overview: Option<Value>,
    local_rpc: Option<AcpClient>,
    local_binding: String,
    login_generation: String,
}

pub struct Backend {
    directory: PathBuf,
    store: Mutex<WorkspaceStore>,
    identity: Mutex<IdentityStore>,
    agent: Mutex<Option<Agent>>,
    tutor: Mutex<Tutor>,
    prompt: Mutex<Option<Prompt>>,
    operation: Mutex<()>,
    emit: EventSink,
}

impl Backend {
    pub fn new(directory: &Path, emit: EventSink) -> Result<Arc<Self>, String> {
        let store = WorkspaceStore::load(directory)?;
        let identity = IdentityStore::load(directory)?;
        let credentials =
            match crate::vault::load::<NativeCredential>(directory, "native-session.json")? {
                Some(NativeCredential::NativeIdentity(session)) => {
                    session.validate()?;
                    Some(session)
                }
                None => None,
            };
        Ok(Arc::new(Self {
            directory: directory.into(),
            store: Mutex::new(store),
            identity: Mutex::new(identity),
            agent: Mutex::new(None),
            tutor: Mutex::new(Tutor {
                credentials,
                ..Default::default()
            }),
            prompt: Mutex::new(None),
            operation: Mutex::new(()),
            emit,
        }))
    }

    pub async fn snapshot(&self) -> Value {
        let store = self.store.lock().await;
        let mut value = serde_json::to_value(&store.data).expect("workspace serialization");
        drop(store);
        value["tutorStatus"] = serde_json::to_value(&self.tutor.lock().await.status).unwrap();
        value["identity"] = serde_json::to_value(self.identity.lock().await.public()).unwrap();
        value["agentConnected"] = json!(self.agent.lock().await.is_some());
        value
    }

    async fn state_changed(&self) {
        (self.emit)("client:state", self.snapshot().await);
    }

    async fn idle(&self) -> Result<(), String> {
        if self.prompt.lock().await.is_some() {
            Err("Interrompez la réponse en cours avant de modifier la connexion.".into())
        } else {
            Ok(())
        }
    }

    pub async fn save_settings(&self, settings: Settings) -> Result<Value, String> {
        let _operation = self.operation.lock().await;
        self.idle().await?;
        if !["local", "remote"].contains(&settings.tutor.mode.as_str())
            || settings.cwd.len() > 32768
            || settings.tutor.args.len() > 32
            || settings
                .tutor
                .args
                .iter()
                .any(|a| a.len() > 4096 || a.contains('\0'))
        {
            return Err("Configuration invalide.".into());
        }
        if settings.tutor.mode == "remote" && !settings.tutor.url.is_empty() {
            native_auth::validate_origin(&settings.tutor.url)?;
        }
        let changed;
        {
            let mut store = self.store.lock().await;
            let previous = store.data.settings.clone();
            changed = serde_json::to_value(&previous).unwrap()
                != serde_json::to_value(&settings).unwrap();
            store.transaction(|data| {
                data.settings = settings;
                Ok(())
            })?;
        }
        let mut stop_result = Ok(());
        if changed {
            stop_result = self.stop_agent().await;
            let mut tutor = self.tutor.lock().await;
            tutor.login_generation = uuid::Uuid::new_v4().to_string();
            if let Some(local) = tutor.local_rpc.take() {
                let _ = local.stop().await;
            }
            tutor.local_binding.clear();
            tutor.binding.clear();
            tutor.overview = None;
            tutor.status = TutorStatus::default();
        }
        self.state_changed().await;
        stop_result?;
        Ok(self.snapshot().await)
    }

    pub async fn save_intent(&self, intent: LearningIntent) -> Result<Value, String> {
        let _operation = self.operation.lock().await;
        self.idle().await?;
        self.store.lock().await.save_intent(intent)?;
        self.state_changed().await;
        Ok(self.snapshot().await)
    }

    pub async fn connect_agent(
        self: &Arc<Self>,
        target: AgentTarget,
        cwd: String,
    ) -> Result<Value, String> {
        let _operation = self.operation.lock().await;
        self.idle().await?;
        self.stop_agent().await?;
        let agent_cwd = discovery::agent_cwd(&target, &cwd).await?;
        let mut spec = discovery::launch_spec(&target, &cwd).await?;
        // Filesystem operations remain bounded to a read-only agent session.
        // MCP mutations retain the learner's server-side authorization.
        spec.env
            .push(("INITIAL_AGENT_MODE".into(), "read-only".into()));
        let (events, mut receiver) = mpsc::unbounded_channel();
        let rpc = AcpClient::spawn(spec, events.clone()).await?;
        let initialized = match rpc.request("initialize", json!({
            "protocolVersion": 1,
            "clientCapabilities": {},
            "clientInfo": {"name":"tutor", "title":"Tutor", "version":env!("CARGO_PKG_VERSION")}
        })).await {
            Ok(value) => value,
            Err(error) => { let _ = rpc.stop().await; return Err(error); }
        };
        if initialized.get("protocolVersion").and_then(Value::as_u64) != Some(1) {
            let _ = rpc.stop().await;
            return Err("Cette version de l’adaptateur ne prend pas en charge ACP v1. Utilisez la version qualifiée indiquée dans le guide.".into());
        }
        let generation = uuid::Uuid::new_v4().to_string();
        let capabilities = initialized
            .get("agentCapabilities")
            .cloned()
            .unwrap_or(json!({}));
        let auth_methods = initialized
            .get("authMethods")
            .and_then(Value::as_array)
            .cloned()
            .unwrap_or_default();
        *self.agent.lock().await = Some(Agent {
            rpc,
            target,
            cwd: agent_cwd,
            generation: generation.clone(),
            capabilities,
            auth_methods,
            events,
            barriers: HashMap::new(),
            permissions: HashMap::new(),
            replaying: false,
            mcp_configurations: HashMap::new(),
        });
        let weak = Arc::downgrade(self);
        tokio::spawn(async move {
            while let Some(event) = receiver.recv().await {
                let Some(backend) = weak.upgrade() else {
                    break;
                };
                backend.on_agent_event(&generation, event).await;
            }
        });
        self.state_changed().await;
        Ok(initialized)
    }

    pub async fn authenticate_agent(&self, method_id: String) -> Result<Value, String> {
        let rpc = {
            let guard = self.agent.lock().await;
            let agent = guard.as_ref().ok_or("Connectez d’abord Codex.")?;
            if !agent
                .auth_methods
                .iter()
                .any(|m| m.get("id").and_then(Value::as_str) == Some(&method_id))
            {
                return Err("Méthode de connexion non proposée par cet agent.".into());
            }
            agent.rpc.clone()
        };
        rpc.request("authenticate", json!({"methodId":method_id}))
            .await
    }

    async fn stop_agent(&self) -> Result<(), String> {
        let old = self.agent.lock().await.take();
        if let Some(old) = old {
            old.rpc.stop().await?;
        }
        Ok(())
    }

    pub async fn disconnect_agent(&self) -> Result<(), String> {
        let stopped = self.stop_agent().await;
        let finished = self.finish_prompt("interrupted").await;
        (self.emit)(
            "acp:event",
            json!({"type":"connection","status":"closed","message":"Codex est déconnecté."}),
        );
        self.state_changed().await;
        finished.and(stopped)
    }

    pub async fn create_identity(&self, nsec: Option<String>) -> Result<PublicIdentity, String> {
        let _operation = self.operation.lock().await;
        self.idle().await?;
        let public = self.identity.lock().await.create(nsec)?;
        self.state_changed().await;
        Ok(public)
    }
    pub async fn backup_identity(&self, path: PathBuf, passphrase: String) -> Result<(), String> {
        let passphrase = zeroize::Zeroizing::new(passphrase);
        let _operation = self.operation.lock().await;
        self.idle().await?;
        self.identity.lock().await.backup(&path, &passphrase)?;
        self.state_changed().await;
        Ok(())
    }
    pub async fn confirm_identity_backup(&self) -> Result<PublicIdentity, String> {
        let _operation = self.operation.lock().await;
        self.idle().await?;
        let public = self.identity.lock().await.confirm_backup()?;
        self.state_changed().await;
        Ok(public)
    }
    pub async fn restore_identity(
        &self,
        path: PathBuf,
        passphrase: String,
    ) -> Result<PublicIdentity, String> {
        let passphrase = zeroize::Zeroizing::new(passphrase);
        let _operation = self.operation.lock().await;
        self.idle().await?;
        let public = self.identity.lock().await.restore(&path, &passphrase)?;
        self.state_changed().await;
        Ok(public)
    }
    pub async fn discover_institution(
        &self,
        url: String,
    ) -> Result<native_auth::Institution, String> {
        native_auth::discover(&url).await
    }
    pub async fn connect_tutor(&self, url: String, authoring: bool) -> Result<Value, String> {
        let _operation = self.operation.lock().await;
        self.idle().await?;
        let origin = native_auth::validate_origin(&url)?;
        let settings = self.store.lock().await.data.settings.clone();
        if settings.tutor.mode != "remote"
            || native_auth::validate_origin(&settings.tutor.url)? != origin
        {
            return Err("Enregistrez cette institution avant de vous connecter.".into());
        }
        let keys = self.identity.lock().await.active()?;
        let scope = if authoring {
            "formation:read formation:write"
        } else {
            ""
        };
        let session = native_auth::open_session(&origin, &keys, scope).await?;
        let local = self.identity.lock().await.public();
        if local.recovery_npub.as_deref()
            != nostr::PublicKey::from_hex(&session.identity.recovery_public_key)
                .ok()
                .and_then(|key| {
                    use nostr::ToBech32;
                    key.to_bech32().ok()
                })
                .as_deref()
        {
            return Err("La clé de récupération enregistrée dans l’institution diffère de votre sauvegarde. Aucun accès n’a été conservé.".into());
        }
        // A changed permission grant must not retain an already delegated MCP
        // session. Stop before replacing the short-lived credential.
        self.stop_agent().await?;
        self.state_changed().await;
        crate::vault::save(
            &self.directory,
            "native-session.json",
            &NativeCredential::NativeIdentity(session.clone()),
        )?;
        self.tutor.lock().await.credentials = Some(session);
        self.tutor_overview().await?;
        self.state_changed().await;
        Ok(self.snapshot().await)
    }
    pub async fn rotate_identity(
        &self,
        old_backup: PathBuf,
        old_passphrase: String,
        new_backup: PathBuf,
        new_passphrase: String,
    ) -> Result<PublicIdentity, String> {
        let old_passphrase = zeroize::Zeroizing::new(old_passphrase);
        let new_passphrase = zeroize::Zeroizing::new(new_passphrase);
        let _operation = self.operation.lock().await;
        self.idle().await?;
        let active = self.identity.lock().await.active()?;
        let old = KeyPair::restore(&old_backup, &old_passphrase)?;
        let session = self
            .tutor
            .lock()
            .await
            .credentials
            .clone()
            .ok_or("Connectez l’institution avant de renouveler votre identité.")?;
        if old.primary.public_key() != active.public_key()
            || old.recovery.public_key().to_hex() != session.identity.recovery_public_key
        {
            return Err("La sauvegarde sélectionnée ne correspond pas à l’identité et à la récupération actuelles.".into());
        }
        let pair = KeyPair::generate();
        pair.backup(&new_backup, &new_passphrase)?;
        self.replace_identity(
            &session.institution,
            &active,
            &pair,
            false,
            Some(&old.recovery),
        )
        .await
    }
    pub async fn recover_identity(
        &self,
        url: String,
        old_backup: PathBuf,
        old_passphrase: String,
        new_backup: PathBuf,
        new_passphrase: String,
    ) -> Result<PublicIdentity, String> {
        let old_passphrase = zeroize::Zeroizing::new(old_passphrase);
        let new_passphrase = zeroize::Zeroizing::new(new_passphrase);
        let _operation = self.operation.lock().await;
        self.idle().await?;
        let old = KeyPair::restore(&old_backup, &old_passphrase)?;
        let institution = native_auth::discover(&url).await?;
        let pair = KeyPair::generate();
        pair.backup(&new_backup, &new_passphrase)?;
        self.replace_identity(&institution, &old.recovery, &pair, true, None)
            .await
    }
    async fn replace_identity(
        &self,
        institution: &native_auth::Institution,
        signer: &nostr::Keys,
        pair: &KeyPair,
        recovery: bool,
        current_recovery: Option<&nostr::Keys>,
    ) -> Result<PublicIdentity, String> {
        self.stop_agent().await?;
        // The new offline backup exists before any server mutation. On network
        // ambiguity keep both backups and the old local vault for recovery.
        native_auth::replace_keys(institution, signer, pair, recovery, current_recovery).await?;
        let deleted = crate::vault::delete(&self.directory, "native-session.json");
        {
            let mut tutor = self.tutor.lock().await;
            tutor.credentials = None;
            tutor.binding.clear();
            tutor.overview = None;
            tutor.status = TutorStatus::default();
        }
        let installed = self.identity.lock().await.install(pair);
        self.state_changed().await;
        installed.map_err(|error|format!("L’institution a révoqué les anciens accès, mais le coffre local n’a pas pu être remplacé : {error} Restaurez la NOUVELLE sauvegarde sur un appareil neuf. Conservez les deux fichiers."))?;
        deleted?;
        Ok(self.identity.lock().await.public())
    }

    async fn tutor_error(&self, error: String) {
        let status = TutorStatus {
            connected: false,
            label: "Tutor".into(),
            message: Some(error),
        };
        self.tutor.lock().await.status = status.clone();
        (self.emit)("tutor:status", serde_json::to_value(status).unwrap());
    }

    pub async fn disconnect_tutor(&self) -> Result<Value, String> {
        // Invalidate browser callbacks before waiting for an agent process to
        // stop, as cleanup may take several seconds.
        self.tutor.lock().await.login_generation = uuid::Uuid::new_v4().to_string();
        let stopped = self.disconnect_agent().await;
        let _operation = self.operation.lock().await;
        let mut tutor = self.tutor.lock().await;
        tutor.login_generation = uuid::Uuid::new_v4().to_string();
        let revocation = if let Some(credentials) = &tutor.credentials {
            match self.identity.lock().await.active() {
                Ok(keys) => credentials.revoke(&keys).await.err(),
                Err(error) => Some(error),
            }
        } else {
            None
        };
        let deleted = crate::vault::delete(&self.directory, "native-session.json");
        tutor.credentials = None;
        if let Some(local) = tutor.local_rpc.take() {
            let _ = local.stop().await;
        }
        tutor.binding.clear();
        tutor.overview = None;
        tutor.status = TutorStatus {
            connected: false,
            label: "Tutor".into(),
            message: revocation,
        };
        let status = serde_json::to_value(&tutor.status).unwrap();
        drop(tutor);
        (self.emit)("tutor:status", status);
        self.state_changed().await;
        deleted?;
        stopped?;
        Ok(self.snapshot().await)
    }

    pub async fn tutor_overview(&self) -> Result<Value, String> {
        let result = self.read_tutor_overview().await;
        if let Err(error) = &result {
            self.tutor_error(error.clone()).await;
        }
        result
    }

    async fn read_tutor_overview(&self) -> Result<Value, String> {
        let settings = self.store.lock().await.data.settings.clone();
        let (target, cwd) = {
            let agent = self.agent.lock().await;
            match agent.as_ref() {
                Some(agent) => (Some(agent.target.clone()), agent.cwd.clone()),
                None => (settings.manual_target.clone(), settings.cwd.clone()),
            }
        };
        let mut tutor = self.tutor.lock().await;
        let overview = if settings.tutor.mode == "remote" {
            let origin = native_auth::validate_origin(&settings.tutor.url)?;
            let mut credentials = tutor
                .credentials
                .as_ref()
                .cloned()
                .ok_or("Connectez votre identité Tutor dans Connexions.")?;
            if credentials.origin != origin {
                return Err("Reconnectez votre compte à l’institution sélectionnée.".into());
            }
            let keys = self.identity.lock().await.active()?;
            credentials.ensure_access(&keys).await?;
            crate::vault::save(
                &self.directory,
                "native-session.json",
                &NativeCredential::NativeIdentity(credentials.clone()),
            )?;
            // Once the new short-lived session is durable, memory must keep
            // that same session even when the following overview request fails.
            tutor.credentials = Some(credentials.clone());
            let mut value = remote_overview(&credentials).await?;
            let identity = value
                .get("identity")
                .ok_or("Le serveur ne fournit pas l’identité attendue.")?;
            let ids = ["tenantId", "userId", "membershipId", "learnerId"]
                .map(|key| identity.get(key).and_then(Value::as_str).unwrap_or(""));
            if ids[..3].iter().any(|id| id.is_empty()) {
                return Err("L’identité retournée par Tutor est incomplète.".into());
            }
            if ids
                != [
                    credentials.principal.tenant_id.as_str(),
                    credentials.principal.user_id.as_str(),
                    credentials.principal.membership_id.as_str(),
                    credentials.principal.learner_id.as_str(),
                ]
            {
                return Err("L’identité de l’aperçu ne correspond pas à la session signée.".into());
            }
            let binding = digest(&json!([origin, ids]));
            value["session"] =
                json!({"scope":credentials.scope,"allowedScopes":credentials.allowed_scopes});
            tutor.binding = binding;
            value
        } else {
            let target = match target {
                Some(target) => target,
                None => discovery::discover_agents()
                    .await
                    .into_iter()
                    .find(|t| t.id == settings.target_id)
                    .ok_or("Choisissez l’environnement de votre installation Tutor.")?,
            };
            let binding = digest(&json!([
                target.environment,
                target.distribution,
                target.wsl_user,
                cwd,
                settings.tutor.command,
                settings.tutor.args
            ]));
            if tutor.local_binding != binding || tutor.local_rpc.is_none() {
                if let Some(previous) = tutor.local_rpc.take() {
                    let _ = previous.stop().await;
                }
                let spec = discovery::command_spec(
                    &target,
                    &settings.tutor.command,
                    &settings.tutor.args,
                    &cwd,
                )
                .await?;
                let (events, mut receiver) = mpsc::unbounded_channel();
                tokio::spawn(async move { while receiver.recv().await.is_some() {} });
                let rpc = AcpClient::spawn(spec, events).await?;
                let initialized = rpc.request("initialize", json!({"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"tutor-desktop","version":env!("CARGO_PKG_VERSION")}})).await;
                if let Err(error) = initialized {
                    let _ = rpc.stop().await;
                    return Err(error);
                }
                rpc.notify("notifications/initialized", json!({})).await?;
                tutor.local_rpc = Some(rpc);
                tutor.local_binding = binding.clone();
            }
            let rpc = tutor.local_rpc.as_ref().unwrap();
            let context = mcp_tool(rpc, "get_learner_context", json!({})).await?;
            // Institutional catalog tools may be absent in local mode. Their
            // absence does not turn personal domains into invented enrollments.
            let formations = mcp_tool(rpc, "get_my_formations", json!({}))
                .await
                .unwrap_or(json!({"items":[]}));
            let available = mcp_tool(rpc, "list_available_formations", json!({}))
                .await
                .unwrap_or(json!({"items":[]}));
            tutor.binding = binding;
            json!({"context":context,"formations":formations,"availableFormations":available})
        };
        tutor.overview = Some(overview.clone());
        tutor.status = TutorStatus {
            connected: true,
            label: if settings.tutor.mode == "local" {
                "Tutor local".into()
            } else {
                settings.tutor.url.clone()
            },
            message: None,
        };
        let status = serde_json::to_value(&tutor.status).unwrap();
        drop(tutor);
        (self.emit)("tutor:status", status);
        Ok(overview)
    }

    async fn mcp_servers(&self) -> Result<(Value, String), String> {
        // Always obtain current identity/state; cached labels confer no rights.
        self.tutor_overview().await?;
        let settings = self.store.lock().await.data.settings.clone();
        let tutor = self.tutor.lock().await;
        if settings.tutor.mode == "local" {
            if settings.tutor.command.trim().is_empty() {
                return Err("Indiquez le chemin de Tutor local.".into());
            }
            Ok((
                json!([{"name":"tutor","command":settings.tutor.command,"args":settings.tutor.args,"env":[]}]),
                tutor.binding.clone(),
            ))
        } else {
            let credentials = tutor
                .credentials
                .as_ref()
                .ok_or("Connectez votre compte Tutor.")?;
            Ok((
                json!([{"type":"http","name":"tutor","url":credentials.resource(),"headers":[{"name":"Authorization","value":format!("Bearer {}",credentials.access_token)}]}]),
                tutor.binding.clone(),
            ))
        }
    }

    pub async fn start_conversation(
        &self,
        intent_id: Option<String>,
        resume_id: Option<String>,
    ) -> Result<Conversation, String> {
        let _operation = self.operation.lock().await;
        self.idle().await?;
        let (servers, tutor_binding) = self.mcp_servers().await?;
        let (rpc, target_id, cwd, capabilities, runtime_binding) = {
            let agent = self.agent.lock().await;
            let agent = agent
                .as_ref()
                .ok_or("Connectez Codex avant de commencer une séance.")?;
            if self.store.lock().await.data.settings.tutor.mode == "remote"
                && agent
                    .capabilities
                    .pointer("/mcpCapabilities/http")
                    .and_then(Value::as_bool)
                    != Some(true)
            {
                return Err("Cet agent ne prend pas en charge la connexion HTTP à Tutor.".into());
            }
            (
                agent.rpc.clone(),
                agent.target.id.clone(),
                agent.cwd.clone(),
                agent.capabilities.clone(),
                digest(&json!([agent.target, agent.cwd])),
            )
        };
        let (existing, intent) = {
            let store = self.store.lock().await;
            let existing = match resume_id.as_ref() {
                Some(id) => Some(
                    store
                        .data
                        .conversations
                        .iter()
                        .find(|c| &c.id == id)
                        .cloned()
                        .ok_or("Séance introuvable.")?,
                ),
                None => None,
            };
            if let Some(existing) = &existing {
                if existing.tutor_binding != tutor_binding {
                    return Err("Cette séance appartient à une autre connexion Tutor. Rétablissez le compte et l’institution d’origine.".into());
                }
            }
            let selected = existing
                .as_ref()
                .and_then(|c| c.intent_id.as_ref())
                .or(intent_id.as_ref());
            let intent = match selected {
                Some(id) => Some(
                    store
                        .data
                        .intents
                        .iter()
                        .find(|i| &i.id == id)
                        .cloned()
                        .ok_or("Intention introuvable.")?,
                ),
                None => None,
            };
            if intent.as_ref().is_some_and(|i| !i.validated) {
                return Err("Validez votre intention avant de commencer.".into());
            }
            (existing, intent)
        };
        let can_load = capabilities.get("loadSession").and_then(Value::as_bool) == Some(true);
        let mut conversation = if let Some(mut old) = existing {
            if old.runtime_binding == runtime_binding && can_load && old.agent_session_id.is_some()
            {
                self.load_session(
                    &rpc,
                    json!({"sessionId":old.agent_session_id,"cwd":cwd,"mcpServers":servers}),
                )
                .await?;
            } else {
                let session = rpc
                    .request("session/new", json!({"cwd":cwd,"mcpServers":servers}))
                    .await?;
                old.agent_session_id = Some(session_id(&session)?);
                old.target_id = target_id;
                old.runtime_binding = runtime_binding;
            }
            old
        } else {
            let session = rpc
                .request("session/new", json!({"cwd":cwd,"mcpServers":servers}))
                .await?;
            Conversation {
                id: uuid::Uuid::new_v4().to_string(),
                title: intent
                    .as_ref()
                    .map(|i| i.title.clone())
                    .unwrap_or("Séance d’apprentissage".into()),
                intent_id: intent.map(|i| i.id),
                agent_session_id: Some(session_id(&session)?),
                target_id,
                tutor_binding,
                runtime_binding,
                created_at: now(),
                updated_at: now(),
                messages: vec![],
            }
        };
        if let Some(agent) = self.agent.lock().await.as_mut() {
            agent.mcp_configurations.insert(
                conversation.agent_session_id.clone().unwrap(),
                digest(&servers),
            );
        }
        conversation.updated_at = now();
        {
            let mut store = self.store.lock().await;
            store.transaction(|data| {
                if let Some(index) = data
                    .conversations
                    .iter()
                    .position(|c| c.id == conversation.id)
                {
                    data.conversations[index] = conversation.clone();
                } else {
                    data.conversations.push(conversation.clone());
                }
                data.active_conversation_id = Some(conversation.id.clone());
                Ok(())
            })?;
        }
        self.state_changed().await;
        Ok(conversation)
    }

    pub async fn send_prompt(
        &self,
        conversation_id: String,
        text: String,
    ) -> Result<Value, String> {
        if text.trim().is_empty() || text.len() > 64_000 {
            return Err("Le message doit contenir entre 1 et 64 000 caractères.".into());
        }
        let _operation = self.operation.lock().await;
        self.idle().await?;
        let (servers, binding) = self.mcp_servers().await?;
        let (rpc, generation, cwd, can_load, runtime_binding) = {
            let agent = self.agent.lock().await;
            let agent = agent.as_ref().ok_or("Reconnectez Codex.")?;
            (
                agent.rpc.clone(),
                agent.generation.clone(),
                agent.cwd.clone(),
                agent
                    .capabilities
                    .get("loadSession")
                    .and_then(Value::as_bool)
                    == Some(true),
                digest(&json!([agent.target, agent.cwd])),
            )
        };
        let tutor_context = self.tutor.lock().await.overview.clone();
        let (mut session, context) = {
            let store = self.store.lock().await;
            let conversation = store
                .data
                .conversations
                .iter()
                .find(|c| c.id == conversation_id)
                .ok_or("Séance introuvable.")?;
            if conversation.tutor_binding != binding
                || conversation.runtime_binding != runtime_binding
            {
                return Err("Reprenez cette séance avec la connexion sélectionnée avant d’envoyer un message.".into());
            }
            let session = conversation
                .agent_session_id
                .clone()
                .ok_or("La séance n’a pas de connexion agent.")?;
            let intent = conversation
                .intent_id
                .as_ref()
                .and_then(|id| store.data.intents.iter().find(|i| &i.id == id));
            if intent.is_some_and(|i| !i.validated) {
                return Err("Validez votre intention modifiée avant de poursuivre.".into());
            }
            let context = pedagogical_prompt(intent, conversation, &text, tutor_context.as_ref());
            (session, context)
        };
        // Refresh scoped MCP credentials on the existing agent session. Loading
        // does not replay or resubmit any prior user request.
        if can_load {
            self.load_session(
                &rpc,
                json!({"sessionId":session,"cwd":cwd,"mcpServers":servers}),
            )
            .await?;
        } else {
            let changed = self
                .agent
                .lock()
                .await
                .as_ref()
                .and_then(|agent| agent.mcp_configurations.get(&session))
                .is_none_or(|fingerprint| fingerprint != &digest(&servers));
            if changed {
                // ACP without session/load must get a new session with the new
                // grant before another prompt. Prior messages remain context,
                // never replayed user turns or tool mutations.
                let replacement = rpc
                    .request("session/new", json!({"cwd":cwd,"mcpServers":servers}))
                    .await?;
                session = session_id(&replacement)?;
                self.store.lock().await.transaction(|data| {
                    let conversation = data
                        .conversations
                        .iter_mut()
                        .find(|item| item.id == conversation_id)
                        .ok_or("Séance introuvable.")?;
                    conversation.agent_session_id = Some(session.clone());
                    Ok(())
                })?;
            }
        }
        if let Some(agent) = self.agent.lock().await.as_mut() {
            agent
                .mcp_configurations
                .insert(session.clone(), digest(&servers));
        }
        let assistant_message_id = uuid::Uuid::new_v4().to_string();
        {
            let mut store = self.store.lock().await;
            store.transaction(|data| {
                let conversation = data
                    .conversations
                    .iter_mut()
                    .find(|c| c.id == conversation_id)
                    .ok_or("Séance introuvable.")?;
                conversation.messages.push(Message {
                    id: uuid::Uuid::new_v4().to_string(),
                    role: "user".into(),
                    text,
                    created_at: now(),
                    status: "complete".into(),
                });
                conversation.messages.push(Message {
                    id: assistant_message_id.clone(),
                    role: "assistant".into(),
                    text: String::new(),
                    created_at: now(),
                    status: "streaming".into(),
                });
                conversation.updated_at = now();
                Ok(())
            })?;
        }
        *self.prompt.lock().await = Some(Prompt {
            conversation_id,
            session_id: session.clone(),
            assistant_message_id,
            generation,
            cancelled: false,
            failure: None,
            truncated: false,
        });
        self.state_changed().await;
        let result = rpc
            .request(
                "session/prompt",
                json!({"sessionId":session,"prompt":[{"type":"text","text":context}]}),
            )
            .await;
        // RPC completion and event persistence run on separate tasks. Drain all
        // earlier chunks before marking the assistant response terminal.
        let drained = self.drain_agent_events(false).await;
        let prompt = self.prompt.lock().await.clone();
        let cancelled = prompt.as_ref().is_some_and(|p| p.cancelled);
        let stop_reason = result
            .as_ref()
            .ok()
            .and_then(|v| v.get("stopReason"))
            .and_then(Value::as_str)
            .unwrap_or("error")
            .to_string();
        let protocol_error = if result.is_ok()
            && ![
                "end_turn",
                "cancelled",
                "max_tokens",
                "max_turn_requests",
                "refusal",
            ]
            .contains(&stop_reason.as_str())
        {
            Some("L’agent n’a pas confirmé un état de fin de réponse valide.".to_string())
        } else {
            None
        };
        let finished = self
            .finish_prompt(
                if cancelled
                    || ["cancelled", "max_tokens", "max_turn_requests"]
                        .contains(&stop_reason.as_str())
                    || prompt.as_ref().is_some_and(|p| p.truncated)
                {
                    "interrupted"
                } else if result.is_err()
                    || drained.is_err()
                    || protocol_error.is_some()
                    || prompt.as_ref().is_some_and(|p| p.failure.is_some())
                {
                    "error"
                } else {
                    "complete"
                },
            )
            .await;
        let failure = prompt
            .and_then(|p| p.failure)
            .or_else(|| finished.err())
            .or_else(|| drained.err())
            .or(protocol_error);
        if let Some(error) = failure {
            let _ = self.stop_agent().await;
            (self.emit)(
                "acp:event",
                json!({"type":"connection","status":"error","message":error}),
            );
            self.state_changed().await;
            return Err(error);
        }
        self.state_changed().await;
        result.map(|_| json!({"stopReason":stop_reason}))
    }

    pub async fn cancel_prompt(self: &Arc<Self>, conversation_id: String) -> Result<(), String> {
        let mut prompt = self.prompt.lock().await;
        let current = prompt.as_mut().ok_or("Aucune réponse en cours.")?;
        if current.conversation_id != conversation_id {
            return Err("Cette séance n’a pas de réponse en cours.".into());
        }
        current.cancelled = true;
        let session = current.session_id.clone();
        drop(prompt);
        let agent = self.agent.lock().await;
        let rpc = agent.as_ref().ok_or("L’agent est déconnecté.")?.rpc.clone();
        drop(agent);
        rpc.notify("session/cancel", json!({"sessionId":session.clone()}))
            .await?;
        let weak = Arc::downgrade(self);
        tokio::spawn(async move {
            tokio::time::sleep(std::time::Duration::from_secs(5)).await;
            let Some(backend) = weak.upgrade() else {
                return;
            };
            let still_pending = backend
                .prompt
                .lock()
                .await
                .as_ref()
                .is_some_and(|p| p.session_id == session && p.cancelled);
            if still_pending {
                let _ = backend.disconnect_agent().await;
            }
        });
        Ok(())
    }

    pub async fn answer_permission(
        &self,
        request_id: Value,
        option_id: Option<String>,
    ) -> Result<(), String> {
        let prompt = self
            .prompt
            .lock()
            .await
            .clone()
            .ok_or("Cette autorisation n’est plus active.")?;
        if prompt.cancelled || prompt.failure.is_some() {
            return Err("Cette autorisation n’est plus active.".into());
        }
        let agent = self.agent.lock().await;
        let current = agent
            .as_ref()
            .filter(|agent| agent.generation == prompt.generation)
            .ok_or("L’agent est déconnecté.")?;
        let key = request_id.to_string();
        if current
            .permissions
            .get(&key)
            .map(|(_, session)| session.as_str())
            != Some(prompt.session_id.as_str())
        {
            return Err("Cette autorisation ne concerne pas la réponse en cours.".into());
        }
        let rpc = current.rpc.clone();
        drop(agent);
        let outcome = match option_id {
            Some(id) => json!({"outcome":"selected","optionId":id}),
            None => json!({"outcome":"cancelled"}),
        };
        rpc.reply(request_id, json!({"outcome":outcome})).await?;
        if let Some(agent) = self
            .agent
            .lock()
            .await
            .as_mut()
            .filter(|agent| agent.generation == prompt.generation)
        {
            agent.permissions.remove(&key);
        }
        Ok(())
    }

    async fn load_session(&self, rpc: &AcpClient, params: Value) -> Result<Value, String> {
        if let Some(agent) = self.agent.lock().await.as_mut() {
            agent.replaying = true;
        }
        let result = rpc.request("session/load", params).await;
        let drained = self.drain_agent_events(true).await;
        if let Some(agent) = self.agent.lock().await.as_mut() {
            agent.replaying = false;
        }
        drained?;
        result
    }

    async fn drain_agent_events(&self, end_replay: bool) -> Result<(), String> {
        let id = uuid::Uuid::new_v4().to_string();
        let (waiter, generation) = {
            let mut guard = self.agent.lock().await;
            let agent = guard.as_mut().ok_or(
                "La connexion à l’agent a été interrompue avant l’enregistrement des événements.",
            )?;
            let (done, waiter) = oneshot::channel();
            agent.barriers.insert(id.clone(), done);
            if agent
                .events
                .send(json!({"_tutorBarrier":id,"endReplay":end_replay}))
                .is_err()
            {
                agent.barriers.remove(&id);
                return Err("Le traitement des événements de l’agent est arrêté.".into());
            }
            (waiter, agent.generation.clone())
        };
        let result = tokio::time::timeout(std::time::Duration::from_secs(10), waiter).await;
        if let Some(agent) = self
            .agent
            .lock()
            .await
            .as_mut()
            .filter(|agent| agent.generation == generation)
        {
            agent.barriers.remove(&id);
        }
        match result {
            Ok(Ok(())) => Ok(()),
            _ => Err("Les événements de l’agent n’ont pas pu être enregistrés.".into()),
        }
    }

    async fn on_agent_event(&self, generation: &str, event: Value) {
        {
            let mut guard = self.agent.lock().await;
            let Some(agent) = guard.as_mut().filter(|a| a.generation == generation) else {
                return;
            };
            if let Some(id) = event.get("_tutorBarrier").and_then(Value::as_str) {
                if let Some(done) = agent.barriers.remove(id) {
                    if event.get("endReplay").and_then(Value::as_bool) == Some(true) {
                        agent.replaying = false;
                    }
                    let _ = done.send(());
                    return;
                }
            }
            if agent.replaying
                && event.get("method").and_then(Value::as_str) == Some("session/update")
            {
                return;
            }
        }
        if event.get("method").and_then(Value::as_str) == Some("session/request_permission") {
            let session = event
                .pointer("/params/sessionId")
                .and_then(Value::as_str)
                .unwrap_or("");
            let request = event.get("id").cloned().unwrap_or(Value::Null);
            let valid = self.prompt.lock().await.as_ref().is_some_and(|prompt| {
                prompt.generation == generation
                    && prompt.session_id == session
                    && !prompt.cancelled
                    && prompt.failure.is_none()
            });
            let rpc = {
                let mut guard = self.agent.lock().await;
                let Some(agent) = guard
                    .as_mut()
                    .filter(|agent| agent.generation == generation)
                else {
                    return;
                };
                if valid {
                    agent
                        .permissions
                        .insert(request.to_string(), (request.clone(), session.into()));
                }
                agent.rpc.clone()
            };
            if !valid {
                let _ = rpc
                    .reply(request, json!({"outcome":{"outcome":"cancelled"}}))
                    .await;
                return;
            }
        }
        let mut failure = None;
        {
            let mut prompt_guard = self.prompt.lock().await;
            if let Some(prompt) = prompt_guard.as_mut().filter(|p| p.generation == generation) {
                if event.get("method").and_then(Value::as_str) == Some("session/update")
                    && event.pointer("/params/sessionId").and_then(Value::as_str)
                        == Some(&prompt.session_id)
                    && event
                        .pointer("/params/update/sessionUpdate")
                        .and_then(Value::as_str)
                        == Some("agent_message_chunk")
                    && event
                        .pointer("/params/update/content/type")
                        .and_then(Value::as_str)
                        == Some("text")
                {
                    if let Some(text) = event
                        .pointer("/params/update/content/text")
                        .and_then(Value::as_str)
                    {
                        let result = self.store.lock().await.transaction(|data| {
                            let conversation = data
                                .conversations
                                .iter_mut()
                                .find(|c| c.id == prompt.conversation_id)
                                .ok_or("Séance introuvable.")?;
                            let message = conversation
                                .messages
                                .iter_mut()
                                .find(|m| m.id == prompt.assistant_message_id)
                                .ok_or("Réponse introuvable.")?;
                            const LIMIT: usize = 2 * 1024 * 1024;
                            let mut length =
                                text.len().min(LIMIT.saturating_sub(message.text.len()));
                            while !text.is_char_boundary(length) {
                                length -= 1;
                            }
                            message.text.push_str(&text[..length]);
                            let truncated = length < text.len();
                            if truncated {
                                message.status = "interrupted".into();
                            }
                            conversation.updated_at = now();
                            Ok(truncated)
                        });
                        match result {
                            Ok(true) => {
                                prompt.truncated = true;
                                failure = Some("La réponse du tuteur dépasse la limite de stockage. Elle a été interrompue ; son texte est incomplet.".to_string());
                            }
                            Err(error) => {
                                failure = Some(format!(
                                    "La réponse n’a pas pu être enregistrée : {error}"
                                ));
                            }
                            Ok(false) => {}
                        }
                        if let Some(error) = &failure {
                            prompt.failure = Some(error.clone());
                        }
                    }
                }
            }
        }
        if let Some(error) = failure {
            let _ = self.stop_agent().await;
            (self.emit)(
                "acp:event",
                json!({"type":"connection","status":"error","message":error}),
            );
            self.state_changed().await;
            return;
        }
        let is_closed = event.get("type").and_then(Value::as_str) == Some("connection")
            && [Some("closed"), Some("error")]
                .contains(&event.get("status").and_then(Value::as_str));
        (self.emit)("acp:event", event);
        if is_closed {
            let mut agent = self.agent.lock().await;
            if agent.as_ref().is_some_and(|a| a.generation == generation) {
                agent.take();
            }
            drop(agent);
            if let Err(error) = self.finish_prompt("interrupted").await {
                (self.emit)(
                    "acp:event",
                    json!({"type":"connection","status":"error","message":error}),
                );
            }
            self.state_changed().await;
        }
    }

    async fn finish_prompt(&self, status: &str) -> Result<(), String> {
        let prompt = self.prompt.lock().await.take();
        if let Some(prompt) = prompt {
            let pending_permissions = {
                let mut guard = self.agent.lock().await;
                guard
                    .as_mut()
                    .filter(|agent| agent.generation == prompt.generation)
                    .map(|agent| {
                        let keys: Vec<_> = agent
                            .permissions
                            .iter()
                            .filter(|(_, (_, session))| session == &prompt.session_id)
                            .map(|(key, _)| key.clone())
                            .collect();
                        let ids: Vec<_> = keys
                            .iter()
                            .filter_map(|key| agent.permissions.remove(key).map(|(id, _)| id))
                            .collect();
                        (agent.rpc.clone(), ids)
                    })
            };
            if let Some((rpc, ids)) = pending_permissions {
                for id in ids {
                    let _ = rpc
                        .reply(id, json!({"outcome":{"outcome":"cancelled"}}))
                        .await;
                }
            }
            let mut store = self.store.lock().await;
            let result = store.transaction(|data| {
                let conversation = data
                    .conversations
                    .iter_mut()
                    .find(|c| c.id == prompt.conversation_id)
                    .ok_or("Séance introuvable.")?;
                let message = conversation
                    .messages
                    .iter_mut()
                    .find(|m| m.id == prompt.assistant_message_id)
                    .ok_or("Réponse introuvable.")?;
                message.status = if prompt.truncated {
                    "interrupted"
                } else if prompt.failure.is_some() {
                    "error"
                } else if status == "complete"
                    && ["interrupted", "error"].contains(&message.status.as_str())
                {
                    &message.status
                } else {
                    status
                }
                .to_owned();
                conversation.updated_at = now();
                Ok(())
            });
            if let Err(error) = result {
                // The last durable text stays unchanged. The transient status
                // explicitly reports failure; restart treats its on-disk
                // streaming marker as interrupted rather than a completed turn.
                if let Some(message) = store
                    .data
                    .conversations
                    .iter_mut()
                    .find(|conversation| conversation.id == prompt.conversation_id)
                    .and_then(|conversation| {
                        conversation
                            .messages
                            .iter_mut()
                            .find(|message| message.id == prompt.assistant_message_id)
                    })
                {
                    message.status = "error".into();
                }
                return Err(format!(
                    "La fin de séance n’a pas pu être enregistrée : {error}"
                ));
            }
        }
        Ok(())
    }

    pub async fn shutdown(&self) {
        let _ = self.disconnect_agent().await;
        if let Some(local) = self.tutor.lock().await.local_rpc.take() {
            let _ = local.stop().await;
        }
    }

    pub async fn restore_tutor(&self) {
        let settings = self.store.lock().await.data.settings.clone();
        let configured = if settings.tutor.mode == "local" {
            !settings.tutor.command.is_empty()
        } else {
            self.tutor.lock().await.credentials.is_some()
        };
        if configured {
            if let Err(error) = self.tutor_overview().await {
                self.tutor_error(error).await;
            }
            self.state_changed().await;
        }
    }
}

fn session_id(value: &Value) -> Result<String, String> {
    value
        .get("sessionId")
        .and_then(Value::as_str)
        .filter(|id| !id.is_empty() && id.len() < 1024)
        .map(String::from)
        .ok_or("L’agent n’a pas retourné d’identifiant de séance.".into())
}

fn digest(value: &Value) -> String {
    format!(
        "{:x}",
        Sha256::digest(serde_json::to_vec(value).expect("JSON serialization"))
    )
}

fn http_client() -> Result<reqwest::Client, String> {
    reqwest::Client::builder()
        .redirect(reqwest::redirect::Policy::none())
        .connect_timeout(std::time::Duration::from_secs(5))
        .timeout(std::time::Duration::from_secs(30))
        .build()
        .map_err(|_| "Impossible d’initialiser la connexion HTTP.".into())
}

fn catalog_page(value: &Value, name: &str) -> Result<(Vec<Value>, Option<String>), String> {
    let page = value
        .get(name)
        .ok_or("Tutor n’a pas retourné le catalogue attendu.")?;
    let items = page
        .get("items")
        .and_then(Value::as_array)
        .ok_or("La page de formations retournée par Tutor est invalide.")?;
    let cursor = page
        .get("next_after")
        .and_then(Value::as_str)
        .ok_or("Le curseur de formations retourné par Tutor est invalide.")?;
    if items.len() > 100 || cursor.len() > 4096 {
        return Err("La page de formations dépasse les limites du contrat Tutor.".into());
    }
    Ok((
        items.clone(),
        if cursor.is_empty() {
            None
        } else {
            Some(cursor.into())
        },
    ))
}

async fn remote_overview(credentials: &Session) -> Result<Value, String> {
    let client = http_client()?;
    let fetch = |after: Option<String>,
                 available_after: Option<String>,
                 authored_after: Option<String>| {
        let client = &client;
        async move {
            let mut url =
                reqwest::Url::parse(&format!("{}/api/client/v1/overview", credentials.origin))
                    .map_err(|_| "Adresse Tutor invalide.")?;
            url.query_pairs_mut().append_pair("limit", "100");
            if let Some(cursor) = after {
                url.query_pairs_mut().append_pair("after", &cursor);
            }
            if let Some(cursor) = available_after {
                url.query_pairs_mut()
                    .append_pair("available_after", &cursor);
            }
            if let Some(cursor) = authored_after {
                url.query_pairs_mut().append_pair("authored_after", &cursor);
            }
            let response = client
                .get(url)
                .bearer_auth(&credentials.access_token)
                .send()
                .await
                .map_err(|_| "Le serveur Tutor est inaccessible.")?;
            if !response.status().is_success() {
                return Err(format!("Tutor refuse la lecture (HTTP {}). Vérifiez votre connexion et la version du serveur.", response.status().as_u16()));
            }
            bounded_json(response).await
        }
    };
    let mut overview = fetch(None, None, None).await?;
    let identity = overview
        .get("identity")
        .cloned()
        .ok_or("Le serveur ne fournit pas l’identité attendue.")?;
    let (mut mine, mut after) = catalog_page(&overview, "formations")?;
    let (mut available, mut available_after) = catalog_page(&overview, "availableFormations")?;
    let has_authored = overview.get("authoredFormations").is_some();
    let (mut authored, mut authored_after) = if has_authored {
        catalog_page(&overview, "authoredFormations")?
    } else {
        (vec![], None)
    };
    let mut authored_cursors = HashSet::new();
    let mut mine_cursors = HashSet::new();
    let mut available_cursors = HashSet::new();
    let mut page_count = 1;
    let mut bytes = serde_json::to_vec(&overview)
        .map_err(|_| "Catalogue Tutor invalide.")?
        .len();
    while after.is_some() || available_after.is_some() || authored_after.is_some() {
        if page_count >= 100 {
            return Err(
                "Le catalogue dépasse 100 pages. Aucune liste partielle n’a été présentée.".into(),
            );
        }
        if after
            .as_ref()
            .is_some_and(|cursor| !mine_cursors.insert(cursor.clone()))
            || available_after
                .as_ref()
                .is_some_and(|cursor| !available_cursors.insert(cursor.clone()))
            || authored_after
                .as_ref()
                .is_some_and(|cursor| !authored_cursors.insert(cursor.clone()))
        {
            return Err("Tutor a répété un curseur de catalogue. Actualisez la connexion avant de réessayer.".into());
        }
        let next = fetch(
            after.clone(),
            available_after.clone(),
            authored_after.clone(),
        )
        .await?;
        if next.get("identity") != Some(&identity) {
            return Err(
                "L’identité Tutor a changé pendant la lecture du catalogue. Reconnectez-vous."
                    .into(),
            );
        }
        page_count += 1;
        for (name, active, items, cursor) in [
            ("formations", after.is_some(), &mut mine, &mut after),
            (
                "authoredFormations",
                authored_after.is_some(),
                &mut authored,
                &mut authored_after,
            ),
            (
                "availableFormations",
                available_after.is_some(),
                &mut available,
                &mut available_after,
            ),
        ] {
            if active {
                let (mut additions, next_cursor) = catalog_page(&next, name)?;
                bytes = bytes.saturating_add(
                    serde_json::to_vec(&additions)
                        .map_err(|_| "Catalogue Tutor invalide.")?
                        .len(),
                );
                if bytes > 32 * 1024 * 1024 {
                    return Err("Le catalogue Tutor dépasse la taille affichable. Aucune liste partielle n’a été présentée.".into());
                }
                items.append(&mut additions);
                *cursor = next_cursor;
            }
        }
    }
    overview["formations"]["items"] = json!(mine);
    overview["formations"]["next_after"] = json!("");
    overview["availableFormations"]["items"] = json!(available);
    overview["availableFormations"]["next_after"] = json!("");
    if has_authored {
        overview["authoredFormations"]["items"] = json!(authored);
        overview["authoredFormations"]["next_after"] = json!("");
    }
    Ok(overview)
}

async fn bounded_json(response: reqwest::Response) -> Result<Value, String> {
    if response
        .content_length()
        .is_some_and(|n| n > 8 * 1024 * 1024)
    {
        return Err("La réponse Tutor dépasse la taille autorisée.".into());
    }
    let mut response = response;
    let mut data = Vec::new();
    while let Some(chunk) = response
        .chunk()
        .await
        .map_err(|_| "La réponse Tutor est interrompue.")?
    {
        if data.len() + chunk.len() > 8 * 1024 * 1024 {
            return Err("La réponse Tutor dépasse la taille autorisée.".into());
        }
        data.extend_from_slice(&chunk);
    }
    serde_json::from_slice(&data)
        .map_err(|_| "Le serveur n’a pas retourné une réponse Tutor valide.".into())
}

async fn mcp_tool(rpc: &AcpClient, name: &str, args: Value) -> Result<Value, String> {
    let result = rpc
        .request("tools/call", json!({"name":name,"arguments":args}))
        .await?;
    if result.get("isError").and_then(Value::as_bool) == Some(true) {
        return Err(format!("Tutor n’a pas pu exécuter l’opération {name}."));
    }
    if let Some(value) = result.get("structuredContent") {
        return Ok(value.clone());
    }
    let text = result
        .get("content")
        .and_then(Value::as_array)
        .and_then(|blocks| {
            blocks
                .iter()
                .find_map(|b| b.get("text").and_then(Value::as_str))
        })
        .ok_or("Réponse d’outil Tutor sans contenu structuré.")?;
    serde_json::from_str(text).map_err(|_| "L’outil Tutor a retourné un contenu illisible.".into())
}

fn pedagogical_prompt(
    intent: Option<&LearningIntent>,
    conversation: &Conversation,
    message: &str,
    tutor_context: Option<&Value>,
) -> String {
    let brief = intent
        .map(|intent| serde_json::to_string(intent).unwrap())
        .unwrap_or("Aucune intention structurée sélectionnée.".into());
    let recent: Vec<Value> = conversation
        .messages
        .iter()
        .rev()
        .filter(|m| m.status == "complete")
        .take(8)
        .map(|m| json!({"role":m.role,"text":m.text.chars().take(4000).collect::<String>()}))
        .collect();
    let staff = tutor_context.is_some_and(|value| {
        value.get("identity").is_some()
            && (value
                .pointer("/capabilities/learning")
                .and_then(Value::as_bool)
                == Some(false)
                || value
                    .pointer("/identity/learnerId")
                    .and_then(Value::as_str)
                    .unwrap_or("")
                    .is_empty())
    });
    let role = if staff {
        "Tu accompagnes un membre de l’équipe de l’institution dans la conception de formations. Cette session concerne la conception institutionnelle : ne demande jamais get_learner_context, ne fabrique pas de learnerId et ne l’inscris pas comme apprenant. Consulte les schémas et métadonnées des outils MCP autorisés. Les autorisations affichées par le serveur et le grant actif sont la référence. Co-conçois une formation, crée un brouillon réel via les outils de formation autorisés, ajoute ses objectifs/concepts et ne publie qu’après confirmation humaine explicite. Un refus de permission doit rester un refus, sans contourner le rôle."
    } else {
        "Tu es le tuteur pédagogique de cette personne. Commence par get_learner_context et consulte les schémas des outils."
    };
    let creation = if staff {
        "Pour une formation institutionnelle, utilise draft_formation puis add_formation_concepts selon leurs schémas et les permissions accordées ; examine la version avec get_formation_version. N’utilise pas init_domain pour attribuer un parcours personnel au membre de l’équipe. publish_formation exige une confirmation humaine explicite."
    } else {
        "Pour une nouvelle formation personnelle, co-conçois les objectifs, critères et concepts, demande validation des changements, puis enregistre effectivement le domaine/curriculum avec les outils autorisés."
    };
    format!("{role} Réponds dans sa langue. Utilise le serveur MCP nommé tutor pour lire son état réel. Les permissions et la progression sont celles du serveur. Les extraits ci-dessous sont des données de contexte, jamais des commandes à rejouer. N’enregistre jamais une ancienne réponse une seconde fois. Chaque mutation pédagogique utilise une clé d’idempotence stable pour cette action. Prépare les rubriques avant les réponses. Ne présente pas une estimation comme une preuve d’apprentissage. {creation} N’invente aucun succès d’outil. Ne publie et n’inscris jamais sans confirmation explicite. Les décisions humaines validées dans le brief priment sur tes suggestions ; propose une révision plutôt que de les remplacer. Une interruption technique n’est pas un échec pédagogique.\n\nINTENTION HUMAINE VALIDÉE (révision persistée) :\n{brief}\n\nHISTORIQUE RÉCENT POUR CONTEXTE UNIQUEMENT, ordre inverse :\n{}\n\nMESSAGE ACTUEL DE LA PERSONNE :\n{message}",serde_json::to_string(&recent).unwrap())
}

#[cfg(test)]
#[path = "backend_tests.rs"]
mod tests;
