// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! Bounded, bidirectional JSON-RPC over a supervised agent's standard streams.
//!
//! Version/capability negotiation belongs to the caller. This transport also
//! works for a local MCP server; it never assumes an ACP-shaped `initialize`.

use std::collections::{HashMap, HashSet};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex as StdMutex, Weak};
use std::time::Duration;

use serde_json::{json, Value};
use tokio::io::{AsyncBufRead, AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader};
use tokio::process::{ChildStdin, Command};
use tokio::sync::{mpsc, oneshot, watch, Mutex};
use tokio::time::timeout;

use crate::launch::LaunchSpec;

const MAX_FRAME_BYTES: usize = 8 * 1024 * 1024;
const MAX_PENDING: usize = 64;
const WRITE_TIMEOUT: Duration = Duration::from_secs(15);
const STOP_TIMEOUT: Duration = Duration::from_secs(10);

type RpcResponse = Result<Value, String>;
type PendingRequests = HashMap<u64, oneshot::Sender<RpcResponse>>;

struct Permission {
    id: Value,
    session_id: String,
    options: HashSet<String>,
}

struct Inner {
    stdin: Mutex<Option<ChildStdin>>,
    pending: StdMutex<PendingRequests>,
    permissions: StdMutex<HashMap<String, Permission>>,
    shutdown: StdMutex<Option<oneshot::Sender<()>>>,
    exited: watch::Receiver<bool>,
    next_id: AtomicU64,
    closed: AtomicBool,
    events: mpsc::UnboundedSender<Value>,
}

impl Drop for Inner {
    fn drop(&mut self) {
        // The supervisor owns Child and performs asynchronous process cleanup.
        // Reader/supervisor tasks only keep Weak references, so dropping the
        // final public handle really reaches this destructor.
        if let Ok(shutdown) = self.shutdown.get_mut() {
            if let Some(shutdown) = shutdown.take() {
                let _ = shutdown.send(());
            }
        }
    }
}

/// Cloneable connection handle. Call `stop` when switching agent or closing the
/// application; dropping the final handle also asks the supervisor to stop.
#[derive(Clone)]
pub struct AcpClient {
    inner: Arc<Inner>,
}

impl AcpClient {
    pub async fn spawn(
        spec: LaunchSpec,
        events: mpsc::UnboundedSender<Value>,
    ) -> Result<Self, String> {
        if spec.program.trim().is_empty() {
            return Err("Le programme de l’agent est manquant.".into());
        }
        let mut command = Command::new(&spec.program);
        command
            .args(&spec.args)
            // Discovery chooses the adapter's bundled runtime by default. Only
            // a reviewed, explicit LaunchSpec override may select another CLI.
            .env_remove("CODEX_PATH")
            .envs(spec.env)
            .stdin(std::process::Stdio::piped())
            .stdout(std::process::Stdio::piped())
            .stderr(std::process::Stdio::piped())
            .kill_on_drop(true);
        if let Some(cwd) = spec.cwd {
            command.current_dir(cwd);
        }
        #[cfg(unix)]
        {
            use std::os::unix::process::CommandExt;
            command.as_std_mut().process_group(0);
        }
        #[cfg(windows)]
        {
            // No console window; the adapter still has piped stdin/stdout.
            const CREATE_NO_WINDOW: u32 = 0x0800_0000;
            command.creation_flags(CREATE_NO_WINDOW);
        }

        let mut child = command
            .spawn()
            .map_err(|error| format!("Impossible de lancer l’agent : {error}"))?;
        let pid = child
            .id()
            .ok_or_else(|| "Le processus de l’agent n’a pas d’identifiant.".to_string())?;
        let stdin = child
            .stdin
            .take()
            .ok_or("Entrée de l’agent indisponible.")?;
        let stdout = child
            .stdout
            .take()
            .ok_or("Sortie de l’agent indisponible.")?;
        let stderr = child
            .stderr
            .take()
            .ok_or("Diagnostics de l’agent indisponibles.")?;
        let (shutdown_tx, shutdown_rx) = oneshot::channel();
        let (exit_tx, exit_rx) = watch::channel(false);
        let (reader_done_tx, reader_done_rx) = oneshot::channel();
        let inner = Arc::new(Inner {
            stdin: Mutex::new(Some(stdin)),
            pending: StdMutex::new(HashMap::new()),
            permissions: StdMutex::new(HashMap::new()),
            shutdown: StdMutex::new(Some(shutdown_tx)),
            exited: exit_rx,
            next_id: AtomicU64::new(1),
            closed: AtomicBool::new(false),
            events,
        });

        let reader_inner = Arc::downgrade(&inner);
        tokio::spawn(async move {
            let mut reader = BufReader::new(stdout);
            loop {
                let frame = read_frame(&mut reader).await;
                let Some(inner) = reader_inner.upgrade() else {
                    break;
                };
                if inner.closed.load(Ordering::Acquire) {
                    break;
                }
                match frame {
                    Ok(Some(frame)) => {
                        if let Err(message) = dispatch(&inner, &frame).await {
                            fail(&inner, "error", &message);
                            break;
                        }
                    }
                    Ok(None) => {
                        fail(&inner, "closed", "La connexion à l’agent a été fermée.");
                        break;
                    }
                    Err(message) => {
                        fail(&inner, "error", &message);
                        break;
                    }
                }
            }
            let _ = reader_done_tx.send(());
        });

        // Drain in fixed chunks: stderr may contain a very long line, provider
        // credentials, prompts, or access tokens. Its content is never emitted
        // into UI events, persisted, or included in connection errors.
        tokio::spawn(async move {
            let mut stderr = stderr;
            let mut buffer = [0_u8; 4096];
            loop {
                match stderr.read(&mut buffer).await {
                    Ok(0) | Err(_) => break,
                    Ok(_) => {}
                }
            }
        });

        let supervisor_inner = Arc::downgrade(&inner);
        tokio::spawn(async move {
            tokio::select! {
                status = child.wait() => {
                    // Reap the process immediately, but let stdout deliver a
                    // final buffered response before rejecting its waiter.
                    let _ = timeout(Duration::from_millis(500), reader_done_rx).await;
                    if let Some(inner) = supervisor_inner.upgrade() {
                        let message = match status {
                            Ok(status) if status.success() => "Le processus de l’agent s’est arrêté.",
                            _ => "Le processus de l’agent s’est arrêté avec une erreur.",
                        };
                        fail(&inner, "closed", message);
                    }
                    // Unix process groups can still contain descendants after
                    // their leader exits (for example a local MCP server).
                    #[cfg(unix)]
                    terminate_tree(pid).await;
                }
                _ = shutdown_rx => {
                    // Closing stdin first lets WSL forward EOF to its Linux
                    // adapter, which can release Codex and local MCP children.
                    if let Some(inner) = supervisor_inner.upgrade() {
                        let _ = timeout(Duration::from_millis(100), async {
                            inner.stdin.lock().await.take();
                        }).await;
                    }
                    let graceful = timeout(Duration::from_millis(400), child.wait()).await;
                    if graceful.is_err() {
                        terminate_tree(pid).await;
                        let _ = child.start_kill();
                        let _ = timeout(Duration::from_secs(3), child.wait()).await;
                    } else {
                        #[cfg(unix)]
                        terminate_tree(pid).await;
                    }
                }
            }
            let _ = exit_tx.send(true);
        });

        Ok(Self { inner })
    }

    pub async fn request(&self, method: &str, params: Value) -> RpcResponse {
        let deadline = match method {
            "session/prompt" => Duration::from_secs(30 * 60),
            "authenticate" | "auth/login" => Duration::from_secs(5 * 60),
            "session/new" | "session/load" | "session/resume" => Duration::from_secs(90),
            _ => Duration::from_secs(30),
        };
        self.request_with_deadline(method, params, deadline).await
    }

    async fn request_with_deadline(
        &self,
        method: &str,
        params: Value,
        deadline: Duration,
    ) -> RpcResponse {
        validate_method(method)?;
        let id = self.inner.next_id.fetch_add(1, Ordering::Relaxed);
        let (sender, receiver) = oneshot::channel();
        {
            let mut pending = self
                .inner
                .pending
                .lock()
                .map_err(|_| "Verrou ACP indisponible.")?;
            if self.inner.closed.load(Ordering::Acquire) {
                return Err("La connexion à l’agent est fermée.".into());
            }
            if pending.len() >= MAX_PENDING {
                return Err("Trop de requêtes sont déjà en cours auprès de l’agent.".into());
            }
            pending.insert(id, sender);
        }
        let _guard = PendingGuard {
            inner: Arc::downgrade(&self.inner),
            id,
        };
        write_message(
            &self.inner,
            &json!({
                "jsonrpc": "2.0", "id": id, "method": method, "params": params
            }),
        )
        .await?;
        match timeout(deadline, receiver).await {
            Ok(Ok(response)) => response,
            Ok(Err(_)) => Err("La connexion à l’agent a été interrompue.".into()),
            Err(_) => {
                // No automatic replay: a timed-out mutation may already have
                // reached Tutor. The caller must reconcile persisted state.
                if method == "session/prompt" {
                    if let Some(session_id) = params.get("sessionId") {
                        let _ = self
                            .notify("session/cancel", json!({"sessionId": session_id}))
                            .await;
                    }
                } else {
                    let _ = self
                        .notify("$/cancel_request", json!({"requestId": id}))
                        .await;
                }
                Err(format!(
                    "Le délai de réponse de l’agent est dépassé pour {method}."
                ))
            }
        }
    }

    pub async fn notify(&self, method: &str, params: Value) -> Result<(), String> {
        validate_method(method)?;
        if method == "session/cancel" {
            let session_id = params
                .get("sessionId")
                .and_then(Value::as_str)
                .ok_or("La séance à annuler est manquante.")?;
            cancel_permissions(&self.inner, Some(session_id)).await?;
        }
        write_message(
            &self.inner,
            &json!({
                "jsonrpc": "2.0", "method": method, "params": params
            }),
        )
        .await
    }

    /// Answer an outstanding permission request. Unknown IDs and options fail
    /// closed; the frontend cannot authorize an option the agent never offered.
    pub async fn reply(&self, id: Value, result: Value) -> Result<(), String> {
        let key = rpc_id_key(&id).ok_or("Identifiant de permission invalide.")?;
        {
            let mut pending = self
                .inner
                .permissions
                .lock()
                .map_err(|_| "Verrou ACP indisponible.")?;
            let permission = pending
                .get(&key)
                .ok_or("Cette permission n’est plus en attente.")?;
            let outcome = result
                .get("outcome")
                .and_then(Value::as_object)
                .ok_or("La décision de permission est manquante.")?;
            match outcome.get("outcome").and_then(Value::as_str) {
                Some("cancelled") => {}
                Some("selected") => {
                    let option = outcome
                        .get("optionId")
                        .and_then(Value::as_str)
                        .ok_or("L’option de permission est manquante.")?;
                    if !permission.options.contains(option) {
                        return Err("Cette option de permission n’a pas été proposée.".into());
                    }
                }
                _ => return Err("La décision de permission est invalide.".into()),
            }
            pending.remove(&key);
        }
        write_message(
            &self.inner,
            &json!({"jsonrpc": "2.0", "id": id, "result": result}),
        )
        .await
    }

    pub async fn stop(&self) -> Result<(), String> {
        if !self.inner.closed.load(Ordering::Acquire) {
            let _ = timeout(
                Duration::from_millis(500),
                cancel_permissions(&self.inner, None),
            )
            .await;
            fail(
                &self.inner,
                "closed",
                "La connexion à l’agent a été arrêtée.",
            );
        }
        if let Some(shutdown) = self
            .inner
            .shutdown
            .lock()
            .map_err(|_| "Verrou ACP indisponible.")?
            .take()
        {
            let _ = shutdown.send(());
        }
        // A writer may be blocked because the agent stopped consuming stdin.
        // Do not let that mutex prevent the independent supervisor from killing
        // the process and unblocking the writer.
        let _ = timeout(Duration::from_millis(100), async {
            self.inner.stdin.lock().await.take();
        })
        .await;
        let mut exited = self.inner.exited.clone();
        timeout(STOP_TIMEOUT, async {
            while !*exited.borrow() {
                if exited.changed().await.is_err() {
                    break;
                }
            }
        })
        .await
        .map_err(|_| "L’arrêt de l’agent n’a pas été confirmé.".to_string())?;
        Ok(())
    }
}

struct PendingGuard {
    inner: Weak<Inner>,
    id: u64,
}

impl Drop for PendingGuard {
    fn drop(&mut self) {
        if let Some(inner) = self.inner.upgrade() {
            if let Ok(mut pending) = inner.pending.lock() {
                pending.remove(&self.id);
            }
        }
    }
}

fn validate_method(method: &str) -> Result<(), String> {
    if method.is_empty() || method.len() > 128 || method.chars().any(char::is_control) {
        return Err("La méthode RPC est invalide.".into());
    }
    Ok(())
}

fn rpc_id_key(id: &Value) -> Option<String> {
    match id {
        Value::String(value) if value.len() <= 1024 => Some(format!("s:{value}")),
        Value::Number(value) if value.is_i64() || value.is_u64() => Some(format!("n:{value}")),
        _ => None,
    }
}

async fn read_frame<R: AsyncBufRead + Unpin>(reader: &mut R) -> Result<Option<Vec<u8>>, String> {
    let mut frame = Vec::new();
    loop {
        let bytes = reader
            .fill_buf()
            .await
            .map_err(|_| "Impossible de lire la sortie de l’agent.")?;
        if bytes.is_empty() {
            return if frame.is_empty() {
                Ok(None)
            } else {
                Err("L’agent a fermé une réponse RPC incomplète.".into())
            };
        }
        let newline = bytes.iter().position(|byte| *byte == b'\n');
        let length = newline.map_or(bytes.len(), |index| index + 1);
        if frame.len().saturating_add(length) > MAX_FRAME_BYTES {
            return Err("Une réponse de l’agent dépasse la taille autorisée.".into());
        }
        frame.extend_from_slice(&bytes[..length]);
        reader.consume(length);
        if newline.is_some() {
            frame.pop();
            if frame.last() == Some(&b'\r') {
                frame.pop();
            }
            return Ok(Some(frame));
        }
    }
}

async fn write_message(inner: &Inner, message: &Value) -> Result<(), String> {
    if inner.closed.load(Ordering::Acquire) {
        return Err("La connexion à l’agent est fermée.".into());
    }
    let mut frame = serde_json::to_vec(message).map_err(|_| "La requête RPC est invalide.")?;
    if frame.len() >= MAX_FRAME_BYTES {
        return Err("La requête dépasse la taille autorisée.".into());
    }
    frame.push(b'\n');
    let operation = async {
        let mut stdin = inner.stdin.lock().await;
        let stdin = stdin.as_mut().ok_or("L’entrée de l’agent est fermée.")?;
        stdin
            .write_all(&frame)
            .await
            .map_err(|_| "Impossible d’écrire vers l’agent.")?;
        stdin
            .flush()
            .await
            .map_err(|_| "Impossible de transmettre la requête à l’agent.")?;
        Ok::<_, &str>(())
    };
    match timeout(WRITE_TIMEOUT, operation).await {
        Ok(Ok(())) => Ok(()),
        Ok(Err(message)) => {
            fail(inner, "error", message);
            Err(message.into())
        }
        Err(_) => {
            let message = "L’agent ne lit plus les requêtes.";
            fail(inner, "error", message);
            Err(message.into())
        }
    }
}

async fn dispatch(inner: &Inner, frame: &[u8]) -> Result<(), String> {
    let packet: Value =
        serde_json::from_slice(frame).map_err(|_| "L’agent a envoyé une réponse RPC invalide.")?;
    let object = packet
        .as_object()
        .ok_or("L’agent a envoyé une enveloppe RPC invalide.")?;
    if object.get("jsonrpc").and_then(Value::as_str) != Some("2.0") {
        return Err("La version JSON-RPC de l’agent est invalide.".into());
    }
    if let Some(method) = object.get("method") {
        let method = method
            .as_str()
            .ok_or("La méthode de l’agent est invalide.")?;
        validate_method(method)?;
        if object.contains_key("result") || object.contains_key("error") {
            return Err("L’agent a mélangé requête et réponse RPC.".into());
        }
        if let Some(id) = object.get("id") {
            let key = rpc_id_key(id).ok_or("L’identifiant de requête de l’agent est invalide.")?;
            if method == "session/request_permission" {
                let permission = parse_permission(&packet);
                match permission {
                    Some(permission) => {
                        let accepted = {
                            let mut pending = inner
                                .permissions
                                .lock()
                                .map_err(|_| "Verrou ACP indisponible.")?;
                            if inner.closed.load(Ordering::Acquire) {
                                return Ok(());
                            }
                            if pending.contains_key(&key) {
                                return Err(
                                    "L’agent a réutilisé une permission encore en attente.".into(),
                                );
                            }
                            if pending.len() >= MAX_PENDING {
                                false
                            } else {
                                pending.insert(key, permission);
                                true
                            }
                        };
                        if accepted {
                            if inner.events.send(packet).is_err() {
                                cancel_permissions(inner, None).await?;
                            }
                        } else {
                            write_message(
                                inner,
                                &json!({"jsonrpc":"2.0", "id":id,
                                "error":{"code":-32000,"message":"Client busy"}}),
                            )
                            .await?;
                        }
                    }
                    None => {
                        write_message(
                            inner,
                            &json!({"jsonrpc":"2.0", "id":id,
                        "error":{"code":-32602,"message":"Invalid permission request"}}),
                        )
                        .await?
                    }
                }
            } else {
                write_message(
                    inner,
                    &json!({"jsonrpc":"2.0", "id":id,
                    "error":{"code":-32601,"message":"Method not supported by this client"}}),
                )
                .await?;
            }
        } else {
            if method == "$/cancel_request" {
                if let Some(id) = packet.pointer("/params/requestId") {
                    if let Some(key) = rpc_id_key(id) {
                        let permission = inner
                            .permissions
                            .lock()
                            .map_err(|_| "Verrou ACP indisponible.")?
                            .remove(&key);
                        if let Some(permission) = permission {
                            write_message(
                                inner,
                                &json!({"jsonrpc":"2.0", "id":permission.id,
                                "result":{"outcome":{"outcome":"cancelled"}}}),
                            )
                            .await?;
                        }
                    }
                }
            }
            // Notifications cannot execute client actions. Forward them for
            // capability-aware presentation, including future ACP updates.
            let _ = inner.events.send(packet);
        }
        return Ok(());
    }
    let id = object
        .get("id")
        .and_then(Value::as_u64)
        .ok_or("L’identifiant de réponse de l’agent est invalide.")?;
    if object.contains_key("result") == object.contains_key("error") {
        return Err("La réponse RPC doit contenir un résultat ou une erreur.".into());
    }
    let result = if let Some(error) = object.get("error") {
        let code = error
            .get("code")
            .and_then(Value::as_i64)
            .ok_or("L’erreur RPC est invalide.")?;
        let message = error
            .get("message")
            .and_then(Value::as_str)
            .ok_or("L’erreur RPC est invalide.")?;
        // Error data and arbitrary provider text can contain a credential or a
        // copied request. Preserve the code and useful category, not its text.
        let lower = message.to_lowercase();
        let category = if lower.contains("auth") || lower.contains("api key") {
            "Authentification de l’agent nécessaire ou refusée"
        } else if lower.contains("cancel") {
            "Requête annulée"
        } else if lower.contains("not found") {
            "Ressource introuvable"
        } else if lower.contains("not support") || code == -32601 {
            "Opération non prise en charge"
        } else {
            "L’agent a refusé la requête"
        };
        Err(format!("{category} (JSON-RPC {code})."))
    } else {
        Ok(object.get("result").cloned().unwrap_or(Value::Null))
    };
    if let Some(sender) = inner
        .pending
        .lock()
        .map_err(|_| "Verrou ACP indisponible.")?
        .remove(&id)
    {
        let _ = sender.send(result);
    }
    // A late response after a timeout is harmless and must not become an event
    // or be mistaken for the result of a later mutation.
    Ok(())
}

fn parse_permission(packet: &Value) -> Option<Permission> {
    let params = packet.get("params")?;
    let session_id = params.get("sessionId")?.as_str()?;
    if session_id.is_empty() || session_id.len() > 1024 || !params.get("toolCall")?.is_object() {
        return None;
    }
    let options = params.get("options")?.as_array()?;
    if options.is_empty() || options.len() > 32 {
        return None;
    }
    let mut ids = HashSet::new();
    for option in options {
        let id = option.get("optionId")?.as_str()?;
        if id.is_empty() || id.len() > 1024 || !ids.insert(id.to_string()) {
            return None;
        }
        option.get("name")?.as_str()?;
        match option.get("kind")?.as_str()? {
            "allow_once" | "allow_always" | "reject_once" | "reject_always" => {}
            _ => return None,
        }
    }
    Some(Permission {
        id: packet.get("id")?.clone(),
        session_id: session_id.to_string(),
        options: ids,
    })
}

async fn cancel_permissions(inner: &Inner, session_id: Option<&str>) -> Result<(), String> {
    let permissions = {
        let mut pending = inner
            .permissions
            .lock()
            .map_err(|_| "Verrou ACP indisponible.")?;
        let keys: Vec<_> = pending
            .iter()
            .filter(|(_, value)| session_id.is_none_or(|session| value.session_id == session))
            .map(|(key, _)| key.clone())
            .collect();
        keys.into_iter()
            .filter_map(|key| pending.remove(&key))
            .collect::<Vec<_>>()
    };
    for permission in permissions {
        write_message(
            inner,
            &json!({"jsonrpc":"2.0", "id":permission.id,
            "result":{"outcome":{"outcome":"cancelled"}}}),
        )
        .await?;
    }
    Ok(())
}

fn fail(inner: &Inner, status: &str, message: &str) {
    if inner.closed.swap(true, Ordering::AcqRel) {
        return;
    }
    if let Ok(mut pending) = inner.pending.lock() {
        for (_, sender) in pending.drain() {
            let _ = sender.send(Err(message.to_string()));
        }
    }
    if let Ok(mut pending) = inner.permissions.lock() {
        pending.clear();
    }
    let _ = inner
        .events
        .send(json!({"type":"connection", "status":status, "message":message}));
    if let Ok(mut shutdown) = inner.shutdown.lock() {
        if let Some(shutdown) = shutdown.take() {
            let _ = shutdown.send(());
        }
    }
}

async fn terminate_tree(pid: u32) {
    #[cfg(unix)]
    {
        // A fresh process group is created in spawn, so descendants are covered
        // without ever signaling the application's own group.
        let group = format!("-{pid}");
        let _ = timeout(
            Duration::from_secs(2),
            Command::new("/bin/kill")
                .args(["-TERM", "--", &group])
                .stdout(std::process::Stdio::null())
                .stderr(std::process::Stdio::null())
                .status(),
        )
        .await;
        tokio::time::sleep(Duration::from_millis(100)).await;
        let _ = timeout(
            Duration::from_secs(2),
            Command::new("/bin/kill")
                .args(["-KILL", "--", &group])
                .stdout(std::process::Stdio::null())
                .stderr(std::process::Stdio::null())
                .status(),
        )
        .await;
    }
    #[cfg(windows)]
    {
        // /T includes adapter/Codex/MCP descendants on Windows. For WSL,
        // closing the wsl.exe pipe additionally closes the Linux adapter's
        // stdin; the adapter must release its own children on stdin EOF.
        let _ = timeout(
            Duration::from_secs(4),
            Command::new("taskkill.exe")
                .args(["/PID", &pid.to_string(), "/T", "/F"])
                .creation_flags(0x0800_0000)
                .stdout(std::process::Stdio::null())
                .stderr(std::process::Stdio::null())
                .status(),
        )
        .await;
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn fake_spec() -> LaunchSpec {
        let fixture =
            std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("../tests/acp/fake_agent.py");
        LaunchSpec {
            program: if cfg!(windows) { "python" } else { "python3" }.into(),
            args: vec!["-u".into(), fixture.to_string_lossy().into_owned()],
            env: Vec::new(),
            cwd: None,
        }
    }

    async fn client() -> (AcpClient, mpsc::UnboundedReceiver<Value>) {
        let (events, receiver) = mpsc::unbounded_channel();
        (
            AcpClient::spawn(fake_spec(), events).await.unwrap(),
            receiver,
        )
    }

    #[tokio::test]
    async fn concurrent_responses_are_correlated_and_utf8_is_preserved() {
        let (client, _events) = client().await;
        let first = client.request("test/first", json!({"text":"Été — 日本語"}));
        let second = async {
            tokio::time::sleep(Duration::from_millis(20)).await;
            client.request("test/second", json!({})).await
        };
        let (first, second) = tokio::join!(first, second);
        assert_eq!(first.unwrap()["text"], "Été — 日本語");
        assert_eq!(second.unwrap()["order"], "second");
        client.stop().await.unwrap();
    }

    #[tokio::test]
    async fn permissions_require_a_known_human_decision_and_stream_updates() {
        let (client, mut events) = client().await;
        let request_client = client.clone();
        let prompt = tokio::spawn(async move {
            request_client
                .request(
                    "session/prompt",
                    json!({"sessionId":"lesson-1","permission":true}),
                )
                .await
        });
        let update = timeout(Duration::from_secs(3), events.recv())
            .await
            .unwrap()
            .unwrap();
        assert_eq!(update["method"], "session/update");
        assert_eq!(
            update["params"]["update"]["content"]["text"],
            "Bonjour, élève."
        );
        let permission = timeout(Duration::from_secs(3), events.recv())
            .await
            .unwrap()
            .unwrap();
        assert_eq!(permission["method"], "session/request_permission");
        assert!(
            !prompt.is_finished(),
            "permission must never be automatically approved"
        );
        assert!(client
            .reply(
                permission["id"].clone(),
                json!({"outcome":{"outcome":"selected","optionId":"invented"}})
            )
            .await
            .is_err());
        client
            .reply(
                permission["id"].clone(),
                json!({"outcome":{"outcome":"selected","optionId":"reject-once"}}),
            )
            .await
            .unwrap();
        assert_eq!(prompt.await.unwrap().unwrap()["decision"], "reject-once");
        assert!(client
            .reply(
                permission["id"].clone(),
                json!({"outcome":{"outcome":"cancelled"}})
            )
            .await
            .is_err());
        client.stop().await.unwrap();
    }

    #[tokio::test]
    async fn cancellation_answers_pending_permissions_before_the_agent_is_notified() {
        let (client, mut events) = client().await;
        let prompt_client = client.clone();
        let prompt = tokio::spawn(async move {
            prompt_client
                .request(
                    "session/prompt",
                    json!({"sessionId":"lesson-1","permission":true}),
                )
                .await
        });
        while let Some(event) = timeout(Duration::from_secs(3), events.recv())
            .await
            .unwrap()
        {
            if event["method"] == "session/request_permission" {
                break;
            }
        }
        client
            .notify("session/cancel", json!({"sessionId":"lesson-1"}))
            .await
            .unwrap();
        let result = prompt.await.unwrap().unwrap();
        assert_eq!(result["stopReason"], "cancelled");
        assert_eq!(result["decision"], "cancelled");
        client.stop().await.unwrap();
    }

    #[tokio::test]
    async fn unsupported_agent_requests_fail_closed() {
        let (client, _events) = client().await;
        let result = client.request("test/unsupported", json!({})).await.unwrap();
        assert_eq!(result["clientError"], -32601);
        client.stop().await.unwrap();
    }

    #[tokio::test]
    async fn timeout_does_not_replay_a_request_and_removes_its_waiter() {
        let (client, _events) = client().await;
        let result = client
            .request_with_deadline("test/ignore", json!({}), Duration::from_millis(100))
            .await;
        assert!(result.unwrap_err().contains("délai"));
        assert!(client.inner.pending.lock().unwrap().is_empty());
        let result = client.request("test/count", json!({})).await.unwrap();
        assert_eq!(result["ignored"], 1);
        client.stop().await.unwrap();
    }

    #[tokio::test]
    async fn eof_releases_pending_requests_and_reports_disconnection() {
        let (client, mut events) = client().await;
        let error = timeout(
            Duration::from_secs(3),
            client.request("test/crash", json!({})),
        )
        .await
        .unwrap()
        .unwrap_err();
        assert!(error.contains("agent"));
        let event = timeout(Duration::from_secs(3), events.recv())
            .await
            .unwrap()
            .unwrap();
        assert_eq!(event["type"], "connection");
        assert!(!event.to_string().contains("secret"));
        client.stop().await.unwrap();
    }

    #[tokio::test]
    async fn a_final_buffered_result_is_not_lost_when_the_process_exits() {
        for _ in 0..8 {
            let (client, _events) = client().await;
            let result = client
                .request("test/final_response", json!({}))
                .await
                .unwrap();
            assert_eq!(result["saved"], true);
            client.stop().await.unwrap();
        }
    }

    #[tokio::test]
    async fn invalid_or_oversized_frames_are_rejected_without_logging_the_payload() {
        for method in ["test/invalid", "test/oversized", "test/partial"] {
            let (client, mut events) = client().await;
            let result = timeout(Duration::from_secs(5), client.request(method, json!({})))
                .await
                .unwrap();
            assert!(result.is_err(), "{method}");
            let event = timeout(Duration::from_secs(2), events.recv())
                .await
                .unwrap()
                .unwrap();
            assert_eq!(event["type"], "connection");
            assert!(!event.to_string().contains("private-payload"));
            client.stop().await.unwrap();
        }
    }

    #[tokio::test]
    async fn provider_error_and_stderr_cannot_leak_credentials() {
        let (client, mut events) = client().await;
        let error = client
            .request("test/secret_error", json!({}))
            .await
            .unwrap_err();
        assert!(error.contains("-32000"));
        assert!(!error.contains("sk-private"));
        let result = client.request("test/count", json!({})).await.unwrap();
        assert_eq!(result["ignored"], 0);
        assert!(events.try_recv().is_err(), "stderr must not be forwarded");
        client.stop().await.unwrap();
    }

    #[tokio::test]
    async fn dropping_a_request_future_removes_the_waiter() {
        let (client, _events) = client().await;
        let request_client = client.clone();
        let pending =
            tokio::spawn(async move { request_client.request("test/ignore", json!({})).await });
        for _ in 0..50 {
            if !client.inner.pending.lock().unwrap().is_empty() {
                break;
            }
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
        pending.abort();
        let _ = pending.await;
        assert!(client.inner.pending.lock().unwrap().is_empty());
        client.stop().await.unwrap();
    }

    #[tokio::test]
    async fn stop_releases_requests_and_prevents_new_effects() {
        let (client, _events) = client().await;
        let pending_client = client.clone();
        let pending =
            tokio::spawn(async move { pending_client.request("test/ignore", json!({})).await });
        tokio::time::sleep(Duration::from_millis(30)).await;
        client.stop().await.unwrap();
        assert!(pending.await.unwrap().is_err());
        assert!(client.request("test/count", json!({})).await.is_err());
        client.stop().await.unwrap();
    }

    #[tokio::test]
    async fn stop_is_bounded_even_when_the_agent_stops_reading_stdin() {
        let (client, _events) = client().await;
        client.request("test/block", json!({})).await.unwrap();
        let writer_client = client.clone();
        let writer = tokio::spawn(async move {
            writer_client
                .request("test/large", json!({"text":"x".repeat(1024 * 1024)}))
                .await
        });
        tokio::time::sleep(Duration::from_millis(50)).await;
        timeout(Duration::from_secs(3), client.stop())
            .await
            .unwrap()
            .unwrap();
        assert!(writer.await.unwrap().is_err());
    }

    #[tokio::test]
    async fn dropping_the_last_connection_handle_stops_the_supervised_process() {
        let (client, _events) = client().await;
        let mut exited = client.inner.exited.clone();
        assert!(!*exited.borrow());
        drop(client);
        timeout(Duration::from_secs(5), async {
            while !*exited.borrow() {
                exited.changed().await.unwrap();
            }
        })
        .await
        .unwrap();
    }

    #[cfg(target_os = "linux")]
    #[tokio::test]
    async fn stopping_also_terminates_the_agents_descendants() {
        let (client, _events) = client().await;
        let result = client.request("test/child", json!({})).await.unwrap();
        let pid = result["pid"].as_u64().unwrap();
        let path = format!("/proc/{pid}/stat");
        assert!(std::path::Path::new(&path).exists());
        client.stop().await.unwrap();
        let state = std::fs::read_to_string(path).unwrap_or_default();
        assert!(
            state.is_empty()
                || state
                    .split(") ")
                    .nth(1)
                    .is_some_and(|tail| tail.starts_with('Z')),
            "a descendant must not continue running after stop"
        );
    }

    /// Explicitly enabled real-adapter acceptance test; never invokes a model
    /// or reads credentials. A fresh CODEX_HOME isolates the user's sessions.
    #[tokio::test]
    #[ignore = "requires TUTOR_ACP_TEST_ADAPTER pointing to a pinned installed adapter"]
    async fn real_codex_adapter_negotiates_acp() {
        let adapter = std::env::var("TUTOR_ACP_TEST_ADAPTER").expect("TUTOR_ACP_TEST_ADAPTER");
        // Windows cannot execute npm's .cmd shim as a direct process. CI can
        // provide node.exe as the program and the pinned package entry here.
        let args = std::env::var("TUTOR_ACP_TEST_ADAPTER_ENTRY")
            .ok()
            .map(|entry| vec![entry])
            .unwrap_or_default();
        let home = std::env::temp_dir().join(format!("tutor-acp-handshake-{}", std::process::id()));
        std::fs::create_dir_all(&home).unwrap();
        let (events, _receiver) = mpsc::unbounded_channel();
        let client = AcpClient::spawn(
            LaunchSpec {
                program: adapter,
                args,
                env: vec![("CODEX_HOME".into(), home.to_string_lossy().into_owned())],
                cwd: Some(home.clone()),
            },
            events,
        )
        .await
        .unwrap();
        let response = client
            .request(
                "initialize",
                json!({
                    "protocolVersion":1, "clientCapabilities":{},
                    "clientInfo":{"name":"tutor-client-test","version":"0.1.0"}
                }),
            )
            .await;
        client.stop().await.unwrap();
        let response = response.unwrap();
        assert_eq!(response["protocolVersion"], 1);
        assert!(response["agentCapabilities"].is_object());
        assert!(response["agentInfo"]["name"].is_string());
        // Only this isolated test directory is removed; no Codex user home is
        // ever read, copied, or deleted by this test.
        std::fs::remove_dir_all(home).unwrap();
    }
}
