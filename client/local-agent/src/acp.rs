// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! ACP v1 server with a durable journal and a bounded model projection.
//! MCP connection values stay confidential; durable sessions use the bounded
//! read-only harness and the host's matched permission responses.
//! Session load replays retained history to the UI, never to the model.
//! The Engine must observe cancellation, including while processing a prompt.

use std::collections::{HashMap, HashSet};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use serde_json::{json, Value};
use tokio::io::{AsyncBufRead, AsyncBufReadExt, AsyncWrite, AsyncWriteExt};
use tokio::sync::mpsc;
use tokio::task::JoinHandle;
use tokio::time::{timeout, Instant};

use crate::config::MODEL_ID;
use crate::context::{self, HistoricalTurn, TaskContext};
use crate::runtime::{Engine, GenerationRequest, GenerationStats};
use crate::session_store::{Completion, Owner, SessionStore};

pub struct Options {
    pub data_dir: Option<std::path::PathBuf>,
    pub model_sha256: String,
}
impl Default for Options {
    fn default() -> Self {
        Self {
            data_dir: None,
            model_sha256: crate::config::MODEL_SHA256.into(),
        }
    }
}

const MAX_FRAME_BYTES: usize = 256 * 1024;
const MAX_PROMPT_BYTES: usize = 64 * 1024;
const MAX_OUTPUT_BYTES: usize = 64 * 1024;
const MAX_HISTORY_BYTES: usize = 512 * 1024;
const MAX_TURNS: usize = 64;
const MAX_SESSIONS: usize = 16;
const MAX_REQUEST_IDS: usize = MAX_SESSIONS * MAX_TURNS;
const CHUNK_BYTES: usize = 4096;
const WRITE_TIMEOUT: Duration = Duration::from_secs(2);
const STOP_TIMEOUT: Duration = Duration::from_secs(2);
const GENERATION_TIMEOUT: Duration = Duration::from_secs(120);
// The fixed workflow also includes MCP IO and the user's permission response.
// This is a timeout, not a latency target; there is at most one generation and
// cancellation remains responsive throughout.
const HARNESS_TIMEOUT: Duration = Duration::from_secs(15 * 60);
const SYSTEM_TEXT: &str =
    "You are Tutor's local assistant. Reply briefly in the user's language and follow the requested format. \
    No tools are available in this conversation. Never claim access to learner records, saved progress or a server assessment.";

// Intentionally not Debug or Serialize: these values may contain credentials.
struct ConnectionConfig {
    _cwd: String,
    _mcp_servers: Vec<Value>,
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum TurnStatus {
    Running,
    Complete,
    Interrupted,
    Failed,
}

struct Turn {
    id: String,
    user: String,
    assistant: String,
    status: TurnStatus,
    projection: Option<context::ProjectionAudit>,
}

struct Session {
    _connection: ConnectionConfig,
    turns: Vec<Turn>,
    history_bytes: usize,
    owner: Option<Owner>,
    durable: Option<Arc<Mutex<SessionStore>>>,
}

impl Session {
    fn prompt_context(&self) -> (String, Vec<HistoricalTurn>) {
        let current = self.turns.last().expect("accepted current turn");
        let history = self.turns[..self.turns.len() - 1]
            .iter()
            .map(|turn| HistoricalTurn {
                id: turn.id.clone(),
                user: turn.user.clone(),
                assistant: turn.assistant.clone(),
                status: match turn.status {
                    TurnStatus::Complete => context::TurnStatus::Complete,
                    TurnStatus::Interrupted => context::TurnStatus::Interrupted,
                    TurnStatus::Failed | TurnStatus::Running => context::TurnStatus::Failed,
                },
            })
            .collect();
        (current.user.clone(), history)
    }
}

struct Active {
    id: Value,
    session_id: String,
    chunks: mpsc::Receiver<StreamItem>,
    worker: JoinHandle<Result<WorkResult, String>>,
    output_overflow: Arc<AtomicBool>,
    deadline: Instant,
    projection: Arc<Mutex<Option<context::ProjectionAudit>>>,
    replay: bool,
    permission: Option<PendingPermission>,
}

struct PendingPermission {
    id: Value,
    response: std::sync::mpsc::SyncSender<Value>,
}

enum StreamItem {
    Text(String),
    ToolUpdate(Value),
    Permission {
        id: Value,
        params: Value,
        response: std::sync::mpsc::SyncSender<Value>,
    },
    Replay {
        kind: &'static str,
        text: String,
        status: &'static str,
    },
}
enum WorkResult {
    Generation(GenerationStats),
    Replay,
}

/// The blocking worker persists visible text before enqueueing it. Control
/// events never become transcript text and do not enter the model's history.
struct WorkerOutput {
    chunks: mpsc::Sender<StreamItem>,
    cancel: Arc<AtomicBool>,
    overflow: Arc<AtomicBool>,
    store: Option<Arc<Mutex<SessionStore>>>,
    turn: String,
    bytes: usize,
    controls: usize,
}
impl WorkerOutput {
    fn send(&self, mut item: StreamItem) -> Result<(), String> {
        loop {
            if self.cancel.load(Ordering::Acquire) {
                return Err("cancelled".into());
            }
            match self.chunks.try_send(item) {
                Ok(()) => return Ok(()),
                Err(mpsc::error::TrySendError::Full(value)) => {
                    item = value;
                    std::thread::sleep(Duration::from_millis(1));
                }
                Err(mpsc::error::TrySendError::Closed(_)) => return Err("connection_closed".into()),
            }
        }
    }
    fn text(&mut self, text: &str) -> Result<(), String> {
        if self.cancel.load(Ordering::Acquire) {
            return Err("cancelled".into());
        }
        if text.len() > MAX_OUTPUT_BYTES - self.bytes {
            self.overflow.store(true, Ordering::Release);
            self.cancel.store(true, Ordering::Release);
            return Err("output_limit".into());
        }
        if let Some(store) = &self.store {
            store
                .lock()
                .map_err(|_| "journal_unavailable")?
                .append_assistant(&self.turn, text)
                .map_err(|_| "journal_write_failed")?;
        }
        self.bytes += text.len();
        let mut remaining = text;
        while !remaining.is_empty() {
            let mut len = remaining.len().min(CHUNK_BYTES);
            while !remaining.is_char_boundary(len) {
                len -= 1;
            }
            self.send(StreamItem::Text(remaining[..len].into()))?;
            remaining = &remaining[len..];
        }
        Ok(())
    }
    fn control(&mut self, item: StreamItem) -> Result<(), String> {
        self.controls += 1;
        if self.controls > 64 {
            return Err("control_event_limit".into());
        }
        self.send(item)
    }
    fn tool_update(&mut self, value: Value) -> Result<(), String> {
        if !matches!(
            value.get("sessionUpdate").and_then(Value::as_str),
            Some("tool_call" | "tool_call_update")
        ) {
            return Err("invalid_tool_update".into());
        }
        self.control(StreamItem::ToolUpdate(value))
    }
    fn approve(&mut self, call_id: &str, params: Value) -> Result<Value, String> {
        if call_id.is_empty() || call_id.len() > 96 {
            return Err("invalid_permission_id".into());
        }
        let (response, receiver) = std::sync::mpsc::sync_channel(1);
        self.control(StreamItem::Permission {
            id: json!(format!("permission:{call_id}")),
            params,
            response,
        })?;
        loop {
            if self.cancel.load(Ordering::Acquire) {
                return Err("cancelled".into());
            }
            match receiver.recv_timeout(Duration::from_millis(10)) {
                Ok(value) => return Ok(value),
                Err(std::sync::mpsc::RecvTimeoutError::Timeout) => (),
                Err(std::sync::mpsc::RecvTimeoutError::Disconnected) => {
                    return Err("permission_channel_closed".into())
                }
            }
        }
    }
}

impl crate::harness::Ui for WorkerOutput {
    fn text(&mut self, text: &str) -> Result<(), String> {
        WorkerOutput::text(self, text)
    }
    fn tool_update(&mut self, update: Value) -> Result<(), String> {
        WorkerOutput::tool_update(self, update)
    }
    fn approve(&mut self, call_id: &str, params: Value) -> Result<Value, String> {
        WorkerOutput::approve(self, call_id, params)
    }
}

#[derive(Default)]
struct Server {
    options: Options,
    initialized: bool,
    sessions: HashMap<String, Session>,
    // Never evict and accidentally execute a repeated accepted prompt again.
    // The total is bounded; restart the connection when it is exhausted.
    prompt_ids: HashSet<String>,
    active: Option<Active>,
}

struct CancelOnDrop(Arc<AtomicBool>);
impl Drop for CancelOnDrop {
    fn drop(&mut self) {
        self.0.store(true, Ordering::Release);
    }
}

struct AbortOnDrop<T>(JoinHandle<T>);
impl<T> Drop for AbortOnDrop<T> {
    fn drop(&mut self) {
        self.0.abort();
    }
}

/// Serve newline-delimited JSON-RPC as consumed by the desktop ACP transport.
/// No native Engine mutex guard is ever held on a Tokio async worker thread.
pub async fn serve<R, W>(
    reader: R,
    writer: W,
    engine: Arc<Mutex<Box<dyn Engine>>>,
) -> anyhow::Result<()>
where
    R: AsyncBufRead + Unpin + Send + 'static,
    W: AsyncWrite + Unpin + Send + 'static,
{
    serve_with_options(reader, writer, engine, Options::default()).await
}

pub async fn serve_with_options<R, W>(
    reader: R,
    mut writer: W,
    engine: Arc<Mutex<Box<dyn Engine>>>,
    options: Options,
) -> anyhow::Result<()>
where
    R: AsyncBufRead + Unpin + Send + 'static,
    W: AsyncWrite + Unpin + Send + 'static,
{
    let cancel = Arc::new(AtomicBool::new(false));
    let _cancel_on_exit = CancelOnDrop(cancel.clone());
    let closed = Arc::new(AtomicBool::new(false));
    let (input_tx, mut input_rx) = mpsc::channel(8);
    let reader_closed = closed.clone();
    let reader_cancel = cancel.clone();
    let _reader = AbortOnDrop(tokio::spawn(async move {
        let mut reader = reader;
        loop {
            match read_frame(&mut reader).await {
                Ok(Some(frame)) => {
                    if input_tx.send(Ok(frame)).await.is_err() {
                        break;
                    }
                }
                result => {
                    // Signal directly, even if output or the input queue is
                    // backpressured. Empty EOF must cancel ongoing inference.
                    reader_closed.store(true, Ordering::Release);
                    reader_cancel.store(true, Ordering::Release);
                    if result.is_err() {
                        let _ = input_tx.send(Err(())).await;
                    }
                    break;
                }
            }
        }
    }));
    let mut server = Server {
        options,
        ..Server::default()
    };
    let outcome = async {
        loop {
            let deadline = server.active.as_ref().map(|active| active.deadline)
                .unwrap_or_else(|| Instant::now() + GENERATION_TIMEOUT);
            tokio::select! {
                frame = input_rx.recv() => {
                    let frame = match frame {
                        Some(Ok(frame)) => frame,
                        Some(Err(())) => anyhow::bail!("Invalid ACP input frame."),
                        None => break,
                    };
                    // Do not start work from queued frames after peer EOF.
                    if closed.load(Ordering::Acquire) {
                        break;
                    }
                    let response = match serde_json::from_slice::<Value>(&frame) {
                        Ok(message) => server.dispatch(message, &engine, &cancel, &closed).await,
                        Err(_) => Some(error(Value::Null, -32700, "Invalid JSON.")),
                    };
                    if let Some(response) = response {
                        write_frame(&mut writer, response).await?;
                    }
                }
                chunk = async { server.active.as_mut().unwrap().chunks.recv().await }, if server.active.is_some() => {
                    if let Some(item) = chunk {
                        if let Some(message)=server.stream_message(item,&cancel).map_err(anyhow::Error::msg)? {
                            write_frame(&mut writer,message).await?;
                        }
                    } else {
                        // Channel closure follows the last chunk. Awaiting the
                        // worker here cannot let its result overtake that chunk.
                        let active = server.active.take().unwrap();
                        let response = server.finish(active, &cancel).await;
                        write_frame(&mut writer, response).await?;
                    }
                }
                _ = tokio::time::sleep_until(deadline), if server.active.is_some() => {
                    // A stalled Engine must not allow a second model job. End
                    // this connection after cancellation, rather than retrying.
                    anyhow::bail!("Local generation timed out.");
                }
            }
        }
        Ok(())
    }.await;
    cancel.store(true, Ordering::Release);
    if let Some(active) = server.active.take() {
        if !active.replay {
            if let Some(turn) = server
                .sessions
                .get_mut(&active.session_id)
                .and_then(|s| s.turns.last_mut())
            {
                turn.status = TurnStatus::Interrupted;
            }
        }
        // Dropping the receiver releases a producer waiting on backpressure.
        drop(active.chunks);
        let _ = timeout(STOP_TIMEOUT, active.worker).await;
    }
    outcome
}

impl Server {
    fn stream_message(
        &mut self,
        item: StreamItem,
        cancel: &AtomicBool,
    ) -> Result<Option<Value>, String> {
        let active = self.active.as_mut().ok_or("missing_active_operation")?;
        let session_id = active.session_id.clone();
        let update = match item {
            StreamItem::Text(text) => {
                let session = self
                    .sessions
                    .get_mut(&session_id)
                    .ok_or("missing_active_session")?;
                let turn = session.turns.last_mut().ok_or("missing_active_turn")?;
                session.history_bytes += text.len();
                turn.assistant.push_str(&text);
                json!({"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":text}})
            }
            StreamItem::Replay { kind, text, status } => {
                json!({"sessionUpdate":kind,"content":{"type":"text","text":text},"_meta":{"tutor":{"replayedStatus":status}}})
            }
            StreamItem::ToolUpdate(value) => value,
            StreamItem::Permission {
                id,
                params,
                response,
            } => {
                if cancel.load(Ordering::Acquire) {
                    return Ok(None);
                }
                if active.permission.is_some()
                    || active.replay
                    || params.get("sessionId").and_then(Value::as_str) != Some(session_id.as_str())
                {
                    return Err("invalid_permission_request".into());
                }
                active.permission = Some(PendingPermission {
                    id: id.clone(),
                    response,
                });
                return Ok(Some(
                    json!({"jsonrpc":"2.0","id":id,"method":"session/request_permission","params":params}),
                ));
            }
        };
        Ok(Some(
            json!({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":session_id,"update":update}}),
        ))
    }

    async fn dispatch(
        &mut self,
        message: Value,
        engine: &Arc<Mutex<Box<dyn Engine>>>,
        cancel: &Arc<AtomicBool>,
        closed: &AtomicBool,
    ) -> Option<Value> {
        // Responses belong to the exact outstanding host approval. They are
        // never interpreted as learner requests, model output or new consent.
        if message.get("jsonrpc").and_then(Value::as_str) == Some("2.0")
            && message.get("method").is_none()
            && message.get("id").is_some()
            && (message.get("result").is_some() || message.get("error").is_some())
        {
            if let Some(active) = self.active.as_mut() {
                if active
                    .permission
                    .as_ref()
                    .is_some_and(|p| Some(&p.id) == message.get("id"))
                {
                    let pending = active.permission.take().unwrap();
                    let result = if message.get("error").is_none() {
                        message
                            .get("result")
                            .filter(|v| v.is_object())
                            .cloned()
                            .unwrap_or(Value::Null)
                    } else {
                        Value::Null
                    };
                    let _ = pending.response.try_send(result);
                }
            }
            return None;
        }
        let valid_id = |id: &Value| {
            id.as_str()
                .is_some_and(|id| !id.is_empty() && id.len() <= 128)
                || id.as_i64().is_some()
                || id.as_u64().is_some()
        };
        let id = message.get("id").cloned();
        if !message.is_object()
            || message.get("jsonrpc").and_then(Value::as_str) != Some("2.0")
            || message.get("method").and_then(Value::as_str).is_none()
            || id.as_ref().is_some_and(|id| !valid_id(id))
            || message.get("result").is_some()
            || message.get("error").is_some()
        {
            return Some(error(Value::Null, -32600, "Invalid request."));
        }
        let method = message["method"].as_str().unwrap();
        let params = message.get("params").unwrap_or(&Value::Null);
        // Even cancellation requests must not reuse the active prompt's id:
        // replying with that id would resolve the wrong desktop RPC waiter.
        if id
            .as_ref()
            .is_some_and(|id| self.active.as_ref().is_some_and(|a| a.id == *id))
        {
            return Some(error(Value::Null, -32600, "Request id is already active."));
        }
        if method == "session/cancel" {
            let result = self.cancel(params, cancel);
            return id.map(|id| match result {
                Ok(()) => success(id, json!({})),
                Err((code, text)) => error(id, code, text),
            });
        }
        // Other notifications have no side effects or JSON-RPC response.
        let id = id?;
        if !params.is_object() {
            return Some(error(id, -32602, "Expected object parameters."));
        }
        if method == "initialize" {
            if self.initialized {
                return Some(error(id, -32600, "Already initialized."));
            }
            if params.get("protocolVersion").and_then(Value::as_u64) != Some(1) {
                return Some(error(id, -32602, "ACP protocol version 1 is required."));
            }
            self.initialized = true;
            let mut metadata = agent_metadata(self.options.data_dir.is_some());
            metadata["tutor"]["persistentResume"] = json!(self.options.data_dir.is_some());
            metadata["tutor"]["rebind"] = json!(self.options.data_dir.is_some());
            return Some(success(
                id,
                json!({
                    "protocolVersion":1,
                    "agentCapabilities":{
                        "loadSession":self.options.data_dir.is_some(),
                        "promptCapabilities":{"image":false,"audio":false,"embeddedContext":false},
                        "mcpCapabilities":{"http":self.options.data_dir.is_some(),"sse":false}
                    },
                    "agentInfo":{"name":"tutor-local-agent","title":"Tutor local","version":env!("CARGO_PKG_VERSION")},
                    "authMethods":[],
                    "_meta":metadata
                }),
            ));
        }
        if !self.initialized {
            return Some(error(id, -32002, "Initialize the connection first."));
        }
        let result = match method {
            "session/new" => self.new_session(params).await,
            "_tutor/rebind" => self.rebind(params),
            "session/load" => {
                return match self.start_load(&id, params, cancel).await {
                    Ok(()) => None,
                    Err((code, text)) => Some(error(id, code, text)),
                };
            }
            "session/set_config_option" => self.set_config(params),
            "session/prompt" => {
                return match self.start_prompt(&id, params, engine, cancel, closed) {
                    Ok(()) => None,
                    Err((code, text)) => Some(error(id, code, text)),
                };
            }
            _ => Err((-32601, "Method not supported.")),
        };
        Some(match result {
            Ok(result) => success(id, result),
            Err((code, text)) => error(id, code, text),
        })
    }

    async fn new_session(&mut self, params: &Value) -> RpcResult<Value> {
        if self.sessions.len() >= MAX_SESSIONS {
            return Err((
                -32004,
                "Session limit reached; reconnect to start a new session.",
            ));
        }
        let cwd = params
            .get("cwd")
            .and_then(Value::as_str)
            .filter(|s| !s.is_empty() && s.len() <= 4096 && !s.contains('\0'))
            .ok_or((-32602, "Invalid working directory."))?;
        let servers = params
            .get("mcpServers")
            .and_then(Value::as_array)
            .filter(|s| s.len() <= 32 && s.iter().all(Value::is_object))
            .ok_or((-32602, "Invalid MCP configuration."))?;
        if serde_json::to_vec(servers).map_or(true, |s| s.len() > 128 * 1024) {
            return Err((-32602, "MCP configuration exceeds the session limit."));
        }
        let owner = params
            .pointer("/_meta/tutorLocal/owner")
            .map(|owner| serde_json::from_value::<Owner>(owner.clone()))
            .transpose()
            .map_err(|_| (-32602, "Invalid local session owner."))?;
        let durable = if let Some(owner) = &owner {
            let root = self
                .options
                .data_dir
                .clone()
                .ok_or((-32004, "Durable local storage is not configured."))?;
            let model = self.options.model_sha256.clone();
            let owner = owner.clone();
            let store = timeout(
                Duration::from_secs(2),
                tokio::task::spawn_blocking(move || SessionStore::create(&root, &owner, &model)),
            )
            .await
            .map_err(|_| (-32603, "Local session storage timed out."))?
            .map_err(|_| (-32603, "Local session storage failed."))?
            .map_err(|_| (-32603, "Local session storage failed."))?;
            Some(Arc::new(Mutex::new(store)))
        } else {
            None
        };
        let session_id = match &durable {
            Some(store) => store
                .lock()
                .map_err(|_| (-32603, "Local session storage failed."))?
                .id()
                .to_owned(),
            None => uuid::Uuid::new_v4().to_string(),
        };
        let persistent = durable.is_some();
        self.sessions.insert(
            session_id.clone(),
            Session {
                _connection: ConnectionConfig {
                    _cwd: cwd.into(),
                    _mcp_servers: servers.clone(),
                },
                turns: Vec::new(),
                history_bytes: 0,
                owner,
                durable,
            },
        );
        let mut metadata = agent_metadata(persistent && !servers.is_empty());
        metadata["tutor"]["durableJournal"] = json!(persistent);
        metadata["tutor"]["persistentResume"] = json!(persistent);
        Ok(json!({"sessionId":session_id,"configOptions":config_options(),"_meta":metadata}))
    }

    fn session_id<'a>(&self, params: &'a Value) -> RpcResult<&'a str> {
        let id = params
            .get("sessionId")
            .and_then(Value::as_str)
            .filter(|s| !s.is_empty() && s.len() <= 128)
            .ok_or((-32602, "Invalid session id."))?;
        if !self.sessions.contains_key(id) {
            return Err((-32602, "Unknown session."));
        }
        Ok(id)
    }

    /// Refresh confidential connection configuration without replaying a
    /// conversation. Only the already-bound owner can update an idle session.
    fn rebind(&mut self, params: &Value) -> RpcResult<Value> {
        if self.active.is_some() {
            return Err((-32003, "An operation is already active."));
        }
        let session_id = self.session_id(params)?.to_owned();
        let owner: Owner = serde_json::from_value(
            params
                .pointer("/_meta/tutorLocal/owner")
                .cloned()
                .ok_or((-32602, "A verified session owner is required."))?,
        )
        .map_err(|_| (-32602, "Invalid local session owner."))?;
        let cwd = params
            .get("cwd")
            .and_then(Value::as_str)
            .filter(|s| !s.is_empty() && s.len() <= 4096 && !s.contains('\0'))
            .ok_or((-32602, "Invalid working directory."))?;
        let servers = params
            .get("mcpServers")
            .and_then(Value::as_array)
            .filter(|s| s.len() <= 32 && s.iter().all(Value::is_object))
            .ok_or((-32602, "Invalid MCP configuration."))?;
        if serde_json::to_vec(servers).map_or(true, |s| s.len() > 128 * 1024) {
            return Err((-32602, "MCP configuration exceeds the session limit."));
        }
        let session = self.sessions.get_mut(&session_id).unwrap();
        if session.owner.as_ref() != Some(&owner) || session.durable.is_none() {
            return Err((-32602, "Session owner does not match."));
        }
        session._connection = ConnectionConfig {
            _cwd: cwd.into(),
            _mcp_servers: servers.clone(),
        };
        Ok(json!({"configOptions":config_options(),"_meta":{"tutor":{"persistentResume":true}}}))
    }

    async fn start_load(
        &mut self,
        id: &Value,
        params: &Value,
        cancel: &Arc<AtomicBool>,
    ) -> RpcResult<()> {
        if self.active.is_some() {
            return Err((-32003, "An operation is already active."));
        }
        let root = self
            .options
            .data_dir
            .clone()
            .ok_or((-32601, "Durable sessions are not configured."))?;
        let session_id = params
            .get("sessionId")
            .and_then(Value::as_str)
            .filter(|s| !s.is_empty() && s.len() <= 128)
            .ok_or((-32602, "Invalid session id."))?
            .to_owned();
        let owner: Owner = serde_json::from_value(
            params
                .pointer("/_meta/tutorLocal/owner")
                .cloned()
                .ok_or((-32602, "A verified session owner is required."))?,
        )
        .map_err(|_| (-32602, "Invalid local session owner."))?;
        let cwd = params
            .get("cwd")
            .and_then(Value::as_str)
            .filter(|s| !s.is_empty() && s.len() <= 4096 && !s.contains('\0'))
            .ok_or((-32602, "Invalid working directory."))?
            .to_owned();
        let servers = params
            .get("mcpServers")
            .and_then(Value::as_array)
            .filter(|s| s.len() <= 32 && s.iter().all(Value::is_object))
            .ok_or((-32602, "Invalid MCP configuration."))?
            .clone();
        if serde_json::to_vec(&servers).map_or(true, |s| s.len() > 128 * 1024) {
            return Err((-32602, "MCP configuration exceeds the session limit."));
        }
        let existing = match self.sessions.get(&session_id) {
            Some(session) if session.owner.as_ref() == Some(&owner) => session.durable.clone(),
            Some(_) => return Err((-32602, "Session owner does not match.")),
            None if self.sessions.len() >= MAX_SESSIONS => {
                return Err((-32004, "Session limit reached."))
            }
            None => None,
        };
        let model = self.options.model_sha256.clone();
        let load_id = session_id.clone();
        let load_owner = owner.clone();
        let (store, history) = timeout(
            Duration::from_secs(2),
            tokio::task::spawn_blocking(move || -> Result<_, String> {
                let store = match existing {
                    Some(store) => store,
                    None => Arc::new(Mutex::new(
                        SessionStore::load(&root, &load_owner, &load_id, &model)
                            .map_err(|_| "storage")?,
                    )),
                };
                let history = store
                    .lock()
                    .map_err(|_| "storage")?
                    .recent_turns(24)
                    .map_err(|_| "storage")?;
                Ok((store, history))
            }),
        )
        .await
        .map_err(|_| (-32603, "Local session loading timed out."))?
        .map_err(|_| (-32603, "Local session could not be loaded."))?
        .map_err(|_| {
            (
                -32603,
                "Local session could not be loaded for this owner and model.",
            )
        })?;
        let turns: Vec<_> = history
            .into_iter()
            .map(|turn| Turn {
                id: turn.id,
                user: turn.user,
                assistant: turn.assistant,
                projection: None,
                status: match turn.status {
                    Completion::Complete => TurnStatus::Complete,
                    Completion::Interrupted => TurnStatus::Interrupted,
                    Completion::Failed => TurnStatus::Failed,
                },
            })
            .collect();
        let history_bytes = turns.iter().map(|t| t.user.len() + t.assistant.len()).sum();
        self.sessions.insert(
            session_id.clone(),
            Session {
                _connection: ConnectionConfig {
                    _cwd: cwd,
                    _mcp_servers: servers,
                },
                turns,
                history_bytes,
                owner: Some(owner),
                durable: Some(store.clone()),
            },
        );
        cancel.store(false, Ordering::Release);
        let worker_cancel = cancel.clone();
        let (tx, chunks) = mpsc::channel(8);
        let worker = tokio::task::spawn_blocking(move || {
            let store = store.lock().map_err(|_| "journal_unavailable")?;
            let mut cursor = None;
            loop {
                if worker_cancel.load(Ordering::Acquire) {
                    return Err("cancelled".into());
                }
                let page = store
                    .journal()
                    .turn_refs_after(cursor, 128)
                    .map_err(|_| "journal_read_failed")?;
                if page.is_empty() {
                    break;
                }
                cursor = page.last().map(|t| t.sequence);
                for reference in page {
                    let turn = store
                        .read_turn(reference.content.id())
                        .map_err(|_| "journal_read_failed")?;
                    let status = match turn.status {
                        Completion::Complete => "complete",
                        Completion::Interrupted => "interrupted",
                        Completion::Failed => "failed",
                    };
                    for (kind, text) in [
                        ("user_message_chunk", turn.user),
                        ("agent_message_chunk", turn.assistant),
                    ] {
                        let mut remaining = text.as_str();
                        while !remaining.is_empty() {
                            let mut len = remaining.len().min(CHUNK_BYTES);
                            while !remaining.is_char_boundary(len) {
                                len -= 1;
                            }
                            let mut item = StreamItem::Replay {
                                kind,
                                text: remaining[..len].into(),
                                status,
                            };
                            loop {
                                if worker_cancel.load(Ordering::Acquire) {
                                    return Err("cancelled".into());
                                }
                                match tx.try_send(item) {
                                    Ok(()) => break,
                                    Err(mpsc::error::TrySendError::Full(value)) => {
                                        item = value;
                                        std::thread::sleep(Duration::from_millis(1));
                                    }
                                    Err(mpsc::error::TrySendError::Closed(_)) => {
                                        return Err("connection_closed".into())
                                    }
                                }
                            }
                            remaining = &remaining[len..];
                        }
                    }
                }
            }
            Ok(WorkResult::Replay)
        });
        self.active = Some(Active {
            id: id.clone(),
            session_id,
            chunks,
            worker,
            output_overflow: Arc::new(AtomicBool::new(false)),
            deadline: Instant::now() + GENERATION_TIMEOUT,
            projection: Arc::new(Mutex::new(None)),
            replay: true,
            permission: None,
        });
        Ok(())
    }

    fn set_config(&self, params: &Value) -> RpcResult<Value> {
        self.session_id(params)?;
        if params.get("configId").and_then(Value::as_str) != Some("model")
            || params.get("value").and_then(Value::as_str) != Some(MODEL_ID)
        {
            return Err((-32602, "Only the configured local model is available."));
        }
        Ok(json!({"configOptions":config_options()}))
    }

    fn cancel(&self, params: &Value, cancel: &AtomicBool) -> RpcResult<()> {
        if !self.initialized {
            return Err((-32002, "Initialize the connection first."));
        }
        let id = self.session_id(params)?;
        if self
            .active
            .as_ref()
            .is_some_and(|active| active.session_id == id)
        {
            cancel.store(true, Ordering::Release);
        }
        Ok(())
    }

    fn start_prompt(
        &mut self,
        id: &Value,
        params: &Value,
        engine: &Arc<Mutex<Box<dyn Engine>>>,
        cancel: &Arc<AtomicBool>,
        closed: &AtomicBool,
    ) -> RpcResult<()> {
        let session_id = self.session_id(params)?.to_owned();
        let text = prompt_text(params)?;
        if self.active.is_some() {
            return Err((-32003, "A generation is already active."));
        }
        let key = id.to_string();
        if self.prompt_ids.contains(&key) {
            return Err((-32600, "This prompt request id was already accepted."));
        }
        if self.prompt_ids.len() >= MAX_REQUEST_IDS {
            return Err((-32004, "Request limit reached; reconnect to continue."));
        }
        let session = self.sessions.get_mut(&session_id).unwrap();
        let host_context = crate::host_context::HostContext::from_prompt(
            params,
            &session._connection._mcp_servers,
        )
        .map_err(|_| (-32602, "Invalid verified Tutor context."))?;
        if host_context.is_some() && session.durable.is_none() {
            return Err((-32602, "Verified Tutor context requires a durable session."));
        }
        if session.durable.is_some() {
            while !session.turns.is_empty()
                && (session.turns.len() >= 24
                    || session.history_bytes + text.len() + MAX_OUTPUT_BYTES > MAX_HISTORY_BYTES)
            {
                let old = session.turns.remove(0);
                session.history_bytes -= old.user.len() + old.assistant.len();
            }
        }
        if session.turns.len() >= MAX_TURNS
            || session.history_bytes + text.len() + MAX_OUTPUT_BYTES > MAX_HISTORY_BYTES
        {
            return Err((-32004, "Session history is full; start a new session."));
        }
        // Ordering matters: EOF sets closed before cancel. Reset then check so
        // an EOF racing acceptance can never be overwritten by this reset.
        cancel.store(false, Ordering::Release);
        if closed.load(Ordering::Acquire) {
            cancel.store(true, Ordering::Release);
            return Err((-32000, "ACP connection is closed."));
        }
        session.history_bytes += text.len();
        session.turns.push(Turn {
            id: uuid::Uuid::new_v4().to_string(),
            user: text,
            assistant: String::new(),
            status: TurnStatus::Running,
            projection: None,
        });
        self.prompt_ids.insert(key);
        let (current_user, history) = session.prompt_context();
        let durable = session.durable.clone();
        let durable_turn_id = session.turns.last().unwrap().id.clone();
        let worker_session_id = session_id.clone();
        let servers = session._connection._mcp_servers.clone();
        let turn_timeout = if durable.is_some() && !servers.is_empty() {
            HARNESS_TIMEOUT
        } else {
            GENERATION_TIMEOUT
        };
        let cwd = std::path::PathBuf::from(&session._connection._cwd);
        let runtime = tokio::runtime::Handle::current();
        let (chunks_tx, chunks) = mpsc::channel(8);
        let overflow = Arc::new(AtomicBool::new(false));
        let output_overflow = overflow.clone();
        let worker_engine = engine.clone();
        let worker_cancel = cancel.clone();
        let projection = Arc::new(Mutex::new(None));
        let worker_projection = projection.clone();
        let worker = tokio::task::spawn_blocking(move || {
            let turn_started = std::time::Instant::now();
            if let Some(store) = &durable {
                store
                    .lock()
                    .map_err(|_| "journal_unavailable")?
                    .begin_turn_with_id(&durable_turn_id, &current_user)
                    .map_err(|_| "journal_write_failed")?;
            }
            let generated = (|| -> Result<GenerationStats, String> {
                let host_reference = match (&durable, &host_context) {
                    (Some(store), Some(host)) => {
                        let mut store = store.lock().map_err(|_| "journal_unavailable")?;
                        let event_id = uuid::Uuid::new_v4().to_string();
                        store
                            .journal_mut()
                            .append_event(
                                &event_id,
                                Some(&durable_turn_id),
                                "verified_host_context",
                                &serde_json::to_vec(host).map_err(|_| "invalid_host_context")?,
                            )
                            .map_err(|_| "journal_write_failed")?;
                        Some(
                            store
                                .journal()
                                .content_ref(crate::journal::ContentKind::Event, &event_id)
                                .map_err(|_| "journal_read_failed")?,
                        )
                    }
                    _ => None,
                };
                let mut output = WorkerOutput {
                    chunks: chunks_tx,
                    cancel: worker_cancel.clone(),
                    overflow,
                    store: durable.clone(),
                    turn: durable_turn_id.clone(),
                    bytes: 0,
                    controls: 0,
                };
                // Even lock acquisition stays on the blocking pool.
                let mut engine = worker_engine
                    .lock()
                    .map_err(|_| "engine_unavailable".to_owned())?;
                if worker_cancel.load(Ordering::Acquire) {
                    return Err("cancelled".into());
                }
                if let Some(store) = &durable {
                    if !servers.is_empty() {
                        let mut stats = crate::fast_harness::run(
                            engine.as_mut(),
                            store,
                            crate::harness::Input {
                                session_id: &worker_session_id,
                                turn_id: &durable_turn_id,
                                user: &current_user,
                                history: &history,
                                servers: &servers,
                                cwd: &cwd,
                                language: host_context.as_ref().map(|h| h.language.as_str()),
                                host_context: host_reference.as_ref(),
                                conversation_id: host_context
                                    .as_ref()
                                    .map(|h| h.conversation_uuid()),
                            },
                            &worker_cancel,
                            &runtime,
                            &mut output,
                        )?;
                        stats.elapsed_ms = turn_started.elapsed().as_millis() as u64;
                        return Ok(stats);
                    }
                }
                let budget = engine.context_budget();
                let projected = context::project(
                    &TaskContext {
                        instructions: SYSTEM_TEXT,
                        current_user: &current_user,
                        required: &[],
                        optional: &[],
                        history: &history,
                        active_tool: None,
                    },
                    budget,
                    |messages| engine.count_input(messages, &worker_cancel),
                )
                .map_err(|error| match error {
                    context::ContextError::RequiredEvidenceDoesNotFit => {
                        "context_budget_exceeded".to_owned()
                    }
                    _ => "context_projection_failed".to_owned(),
                })?;
                *worker_projection
                    .lock()
                    .map_err(|_| "context_audit_failed")? = Some(projected.audit());
                if let Some(store) = &durable {
                    store
                        .lock()
                        .map_err(|_| "journal_unavailable")?
                        .record_projection(&durable_turn_id, &projected.audit())
                        .map_err(|_| "journal_write_failed")?;
                }
                let mut stats = engine.generate(
                    GenerationRequest {
                        messages: projected.messages,
                        max_output_tokens: projected.output_reserved,
                        grammar: None,
                    },
                    &worker_cancel,
                    &mut |text| output.text(text),
                )?;
                stats.elapsed_ms = turn_started.elapsed().as_millis() as u64;
                Ok(stats)
            })();
            if let Some(store) = &durable {
                let status = match &generated {
                    Ok(stats)
                        if stats.stop_reason == "end_turn"
                            && !worker_cancel.load(Ordering::Acquire) =>
                    {
                        Completion::Complete
                    }
                    Ok(_) => Completion::Interrupted,
                    Err(_) if worker_cancel.load(Ordering::Acquire) => Completion::Interrupted,
                    Err(_) => Completion::Failed,
                };
                store
                    .lock()
                    .map_err(|_| "journal_unavailable")?
                    .finish_turn(&durable_turn_id, status)
                    .map_err(|_| "journal_write_failed")?;
            }
            generated.map(WorkResult::Generation)
        });
        self.active = Some(Active {
            id: id.clone(),
            session_id,
            chunks,
            worker,
            output_overflow,
            deadline: Instant::now() + turn_timeout,
            projection,
            replay: false,
            permission: None,
        });
        Ok(())
    }

    async fn finish(&mut self, active: Active, cancel: &AtomicBool) -> Value {
        let result = active.worker.await;
        if active.replay {
            return if matches!(result, Ok(Ok(WorkResult::Replay)))
                && !cancel.load(Ordering::Acquire)
            {
                success(
                    active.id,
                    json!({"configOptions":config_options(),"_meta":{"tutor":{"persistentResume":true}}}),
                )
            } else {
                error(
                    active.id,
                    -32603,
                    "Local session replay failed or was cancelled.",
                )
            };
        }
        let turn = self
            .sessions
            .get_mut(&active.session_id)
            .unwrap()
            .turns
            .last_mut()
            .unwrap();
        turn.projection = active
            .projection
            .lock()
            .ok()
            .and_then(|mut audit| audit.take());
        if active.output_overflow.load(Ordering::Acquire) {
            turn.status = TurnStatus::Interrupted;
            return error(active.id, -32005, "Generation output exceeded the limit.");
        }
        if cancel.load(Ordering::Acquire) {
            turn.status = TurnStatus::Interrupted;
            return success(active.id, json!({"stopReason":"cancelled"}));
        }
        match result {
            Ok(Ok(WorkResult::Generation(stats)))
                if matches!(
                    stats.stop_reason.as_str(),
                    "end_turn" | "max_tokens" | "cancelled"
                ) =>
            {
                turn.status = if stats.stop_reason == "end_turn" {
                    TurnStatus::Complete
                } else {
                    TurnStatus::Interrupted
                };
                success(
                    active.id,
                    json!({"stopReason":stats.stop_reason,"_meta":{"tutor":{"context":turn.projection.as_ref().map(|p| json!({"inputTokens":p.input_tokens,"includedRecords":p.kept_ids.len(),"omittedRecords":p.omitted.len()})),"generation":{
                        "inputTokens":stats.input_tokens,"outputTokens":stats.output_tokens,"elapsedMs":stats.elapsed_ms
                    }}}}),
                )
            }
            Ok(Err(code)) if code == "context_budget_exceeded" => {
                turn.status = TurnStatus::Failed;
                error(
                    active.id,
                    -32004,
                    "Required context exceeds the model limit; split the task.",
                )
            }
            Ok(Err(code)) if code == "harness_mcp_connection_failed" => {
                turn.status = TurnStatus::Failed;
                error(active.id, -32007, "Could not connect to Tutor MCP.")
            }
            Ok(Err(code)) if code == "harness_catalog_unavailable" => {
                turn.status = TurnStatus::Failed;
                error(
                    active.id,
                    -32007,
                    "The live Tutor tool catalogue is unavailable.",
                )
            }
            Ok(Err(code)) if code == "harness_input_invalid" => {
                turn.status = TurnStatus::Failed;
                error(
                    active.id,
                    -32602,
                    "The local harness requires one configured Tutor server.",
                )
            }
            _ => {
                turn.status = TurnStatus::Failed;
                // Never forward native/provider errors, panic payloads or paths.
                error(active.id, -32603, "Local generation failed.")
            }
        }
    }
}

type RpcResult<T> = Result<T, (i64, &'static str)>;

fn prompt_text(params: &Value) -> RpcResult<String> {
    let blocks = params
        .get("prompt")
        .and_then(Value::as_array)
        .filter(|blocks| !blocks.is_empty() && blocks.len() <= 16)
        .ok_or((-32602, "Expected a nonempty text prompt."))?;
    let mut text = String::new();
    for (index, block) in blocks.iter().enumerate() {
        if block.get("type").and_then(Value::as_str) != Some("text") {
            return Err((-32602, "Only text content is supported."));
        }
        let part = block
            .get("text")
            .and_then(Value::as_str)
            .ok_or((-32602, "Invalid text content."))?;
        let separator = if index == 0 { "" } else { "\n\n" };
        if text.len() + separator.len() + part.len() > MAX_PROMPT_BYTES {
            return Err((-32602, "Prompt exceeds the input limit."));
        }
        text.push_str(separator);
        text.push_str(part);
    }
    if text.trim().is_empty() {
        return Err((-32602, "Expected a nonempty text prompt."));
    }
    Ok(text)
}

fn agent_metadata(harness: bool) -> Value {
    json!({"tutor":{"mode":if harness {"read_only_harness"} else {"text_only"},
        "toolsAvailable":harness,"mutationsAvailable":false,
        "mcpStatus":"not_connected","persistentResume":false}})
}

fn config_options() -> Value {
    json!([{"id":"model","name":"Local model","category":"model","type":"select",
        "currentValue":MODEL_ID,"options":[{"value":MODEL_ID,"name":"Qwen3-1.7B Q4_K_M"}]}])
}

fn success(id: Value, result: Value) -> Value {
    json!({"jsonrpc":"2.0","id":id,"result":result})
}

fn error(id: Value, code: i64, message: &'static str) -> Value {
    json!({"jsonrpc":"2.0","id":id,"error":{"code":code,"message":message}})
}

async fn read_frame<R: AsyncBufRead + Unpin>(reader: &mut R) -> Result<Option<Vec<u8>>, ()> {
    let mut frame = Vec::new();
    loop {
        let bytes = reader.fill_buf().await.map_err(|_| ())?;
        if bytes.is_empty() {
            return if frame.is_empty() { Ok(None) } else { Err(()) };
        }
        let newline = bytes.iter().position(|b| *b == b'\n');
        let length = newline.map_or(bytes.len(), |index| index + 1);
        if frame.len() + length > MAX_FRAME_BYTES {
            return Err(());
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

async fn write_frame<W: AsyncWrite + Unpin>(writer: &mut W, message: Value) -> anyhow::Result<()> {
    let mut bytes =
        serde_json::to_vec(&message).map_err(|_| anyhow::anyhow!("Invalid ACP output."))?;
    anyhow::ensure!(
        bytes.len() < MAX_FRAME_BYTES,
        "ACP output exceeds the frame limit."
    );
    bytes.push(b'\n');
    timeout(WRITE_TIMEOUT, async {
        writer.write_all(&bytes).await?;
        writer.flush().await
    })
    .await
    .map_err(|_| anyhow::anyhow!("ACP output timed out."))?
    .map_err(|_| anyhow::anyhow!("ACP output closed."))?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use tokio::io::{BufReader, DuplexStream, ReadHalf, WriteHalf};

    type Handler = dyn FnMut(
            GenerationRequest,
            &AtomicBool,
            &mut dyn FnMut(&str) -> Result<(), String>,
        ) -> Result<GenerationStats, String>
        + Send;
    struct EngineDouble(Box<Handler>);
    impl Engine for EngineDouble {
        fn grammar_for(&self, _: &Value) -> Result<String, String> {
            // Scheduling/permission fixture; the GGUF integration test checks
            // actual constrained decoding with the native grammar sampler.
            Ok("fixture-grammar".into())
        }

        fn count_input(
            &mut self,
            messages: &[crate::runtime::ChatMessage],
            _: &AtomicBool,
        ) -> Result<usize, String> {
            // Protocol fixture only. Real tokenizer/template parity is checked
            // by the separate ignored test with the pinned native GGUF.
            Ok(16
                + messages
                    .iter()
                    .map(|m| m.content.len().div_ceil(4))
                    .sum::<usize>())
        }

        fn generate(
            &mut self,
            request: GenerationRequest,
            cancel: &AtomicBool,
            emit: &mut dyn FnMut(&str) -> Result<(), String>,
        ) -> Result<GenerationStats, String> {
            (self.0)(request, cancel, emit)
        }
    }

    fn engine(
        handler: impl FnMut(
                GenerationRequest,
                &AtomicBool,
                &mut dyn FnMut(&str) -> Result<(), String>,
            ) -> Result<GenerationStats, String>
            + Send
            + 'static,
    ) -> Arc<Mutex<Box<dyn Engine>>> {
        Arc::new(Mutex::new(Box::new(EngineDouble(Box::new(handler)))))
    }

    fn stats(reason: &str) -> GenerationStats {
        GenerationStats {
            input_tokens: 10,
            output_tokens: 3,
            elapsed_ms: 1,
            stop_reason: reason.into(),
        }
    }

    fn request(id: Value, method: &str, params: Value) -> Value {
        json!({"jsonrpc":"2.0","id":id,"method":method,"params":params})
    }

    fn prompt(id: Value, session: &str, text: &str) -> Value {
        request(
            id,
            "session/prompt",
            json!({"sessionId":session,"prompt":[{"type":"text","text":text}]}),
        )
    }

    struct Client {
        input: WriteHalf<DuplexStream>,
        output: BufReader<ReadHalf<DuplexStream>>,
        server: JoinHandle<anyhow::Result<()>>,
    }

    impl Client {
        fn connect(engine: Arc<Mutex<Box<dyn Engine>>>, capacity: usize) -> Self {
            Self::connect_options(engine, capacity, Options::default())
        }

        fn connect_options(
            engine: Arc<Mutex<Box<dyn Engine>>>,
            capacity: usize,
            options: Options,
        ) -> Self {
            let (client, server) = tokio::io::duplex(capacity);
            let (reader, writer) = tokio::io::split(server);
            let server = tokio::spawn(serve_with_options(
                BufReader::new(reader),
                writer,
                engine,
                options,
            ));
            let (output, input) = tokio::io::split(client);
            Self {
                input,
                output: BufReader::new(output),
                server,
            }
        }

        async fn send(&mut self, value: Value) {
            write_frame(&mut self.input, value).await.unwrap();
        }

        async fn read(&mut self) -> Value {
            let frame = timeout(Duration::from_secs(3), read_frame(&mut self.output))
                .await
                .expect("server response timed out")
                .unwrap()
                .expect("server closed output");
            serde_json::from_slice(&frame).unwrap()
        }

        async fn initialize(&mut self) -> Value {
            self.send(request(
                json!(1),
                "initialize",
                json!({"protocolVersion":1,"clientCapabilities":{}}),
            ))
            .await;
            self.read().await
        }

        async fn new_session(&mut self, id: u64, servers: Value) -> String {
            self.send(request(
                json!(id),
                "session/new",
                json!({"cwd":"C:\\Tutor local\\séances","mcpServers":servers}),
            ))
            .await;
            let response = self.read().await;
            assert_eq!(
                response["result"]["_meta"]["tutor"]["mcpStatus"],
                "not_connected"
            );
            response["result"]["sessionId"].as_str().unwrap().to_owned()
        }

        async fn completion(&mut self, id: Value) -> (String, Value) {
            let mut text = String::new();
            loop {
                let value = self.read().await;
                if value.get("id") == Some(&id) {
                    return (text, value);
                }
                assert_eq!(value["method"], "session/update");
                assert_eq!(
                    value["params"]["update"]["sessionUpdate"],
                    "agent_message_chunk"
                );
                text.push_str(
                    value["params"]["update"]["content"]["text"]
                        .as_str()
                        .unwrap(),
                );
            }
        }

        async fn close(mut self) {
            self.input.shutdown().await.unwrap();
            timeout(Duration::from_secs(3), self.server)
                .await
                .unwrap()
                .unwrap()
                .unwrap();
        }
    }

    async fn wait_flag(flag: &AtomicBool) {
        timeout(Duration::from_secs(3), async {
            while !flag.load(Ordering::Acquire) {
                tokio::time::sleep(Duration::from_millis(1)).await;
            }
        })
        .await
        .expect("worker did not observe cancellation");
    }

    #[tokio::test]
    async fn desktop_protocol_streams_once_and_keeps_credentials_out_of_context() {
        let seen = Arc::new(Mutex::new(Vec::new()));
        let captured = seen.clone();
        let mut client = Client::connect(
            engine(move |req, _, emit| {
                assert_eq!(req.max_output_tokens, 768);
                assert!(req.grammar.is_none());
                captured.lock().unwrap().push(
                    req.messages
                        .into_iter()
                        .map(|m| (m.role, m.content))
                        .collect::<Vec<_>>(),
                );
                emit("Bonjour ")?;
                emit("élève 🦀")?;
                Ok(stats("end_turn"))
            }),
            65536,
        );
        let init = client.initialize().await;
        assert_eq!(init["result"]["protocolVersion"], 1);
        assert_eq!(init["result"]["agentCapabilities"]["loadSession"], false);
        assert_eq!(
            init["result"]["agentCapabilities"]["mcpCapabilities"]["http"],
            false
        );
        assert_eq!(init["result"]["_meta"]["tutor"]["toolsAvailable"], false);
        assert_eq!(init["result"]["authMethods"], json!([]));
        let session = client
            .new_session(
                2,
                json!([{"name":"tutor","url":"https://private.invalid/?secret=opaque-secret",
            "headers":[{"name":"Authorization","value":"Bearer opaque-secret"}]}]),
            )
            .await;
        client
            .send(prompt(json!(3), &session, "Premier message"))
            .await;
        let (text, result) = client.completion(json!(3)).await;
        assert_eq!(text, "Bonjour élève 🦀");
        assert_eq!(result["result"]["stopReason"], "end_turn");
        client
            .send(prompt(json!(4), &session, "Deuxième message"))
            .await;
        assert_eq!(client.completion(json!(4)).await.0, "Bonjour élève 🦀");
        let requests = seen.lock().unwrap().clone();
        assert_eq!(requests.len(), 2);
        assert_eq!(
            requests[1]
                .iter()
                .filter(|(_, text)| text == "Premier message")
                .count(),
            1
        );
        assert_eq!(
            requests[1]
                .iter()
                .filter(|(_, text)| text == "Bonjour élève 🦀")
                .count(),
            1
        );
        assert_eq!(
            requests[1].last().unwrap(),
            &("user".into(), "Deuxième message".into())
        );
        assert!(!format!("{requests:?}").contains("opaque-secret"));
        assert!(requests[0][0].1.contains("No tools"));
        drop(requests);
        client.close().await;
    }

    #[tokio::test]
    async fn mcp_permission_controls_real_http_dispatch_and_exact_journal_receipts() {
        use crate::mcp::tests::{initialization, mock, Reply};

        for allowed in [true, false] {
            let server = mock(|request| {
                if let Some(reply) = initialization(request) {
                    return reply;
                }
                let result = match request["method"].as_str().unwrap() {
                    "tools/list" => json!({"tools":[{
                        "name":"get_pending_alerts", "description":"Consulter les alertes en attente",
                        "inputSchema":{"type":"object","properties":{"domain_id":{"type":"string"}},"additionalProperties":false}
                    }]}),
                    "tools/call" => {
                        assert_eq!(request["params"]["name"], "get_pending_alerts");
                        assert_eq!(request["params"]["arguments"], json!({"domain_id":"verified-domain"}));
                        json!({"content":[],"structuredContent":{"alerts":["Une révision est disponible."]}})
                    }
                    _ => panic!("unexpected MCP method"),
                };
                Reply::json(json!({"jsonrpc":"2.0","id":request["id"],"result":result}))
            }).await;
            let root = tempfile::tempdir().unwrap();
            let conversation = uuid::Uuid::new_v4().to_string();
            let owner = json!({"profileId":"profile1","accountId":"account1","tenantId":"tenant1","serverId":"school-test"});
            let seen = Arc::new(Mutex::new(Vec::new()));
            let captured = seen.clone();
            let mut client = Client::connect_options(
                engine(move |request, _, emit| {
                    let prompt = request
                        .messages
                        .iter()
                        .map(|m| m.content.as_str())
                        .collect::<Vec<_>>()
                        .join("\n");
                    assert!(!prompt.contains("connection-secret"));
                    assert!(!prompt.contains("private-session-test"));
                    captured.lock().unwrap().push(prompt.clone());
                    assert!(request.grammar.is_none());
                    if allowed {
                        assert!(prompt.contains("Une révision est disponible."));
                        emit("Une révision est disponible.")?;
                    } else {
                        assert!(prompt.contains("tool_permission_denied"));
                        emit("Vous avez refusé la lecture des alertes.")?;
                    }
                    Ok(stats("end_turn"))
                }),
                65536,
                Options {
                    data_dir: Some(root.path().into()),
                    ..Options::default()
                },
            );
            client.initialize().await;
            client.send(request(json!(2), "session/new", json!({
                "cwd":root.path(),"mcpServers":[{"type":"http","name":"tutor","url":server.url,
                    "headers":[{"name":"Authorization","value":"Bearer connection-secret"},
                        {"name":"Tutor-Conversation-ID","value":conversation}]}],
                "_meta":{"tutorLocal":{"owner":owner}}
            }))).await;
            let created = client.read().await;
            let session = created["result"]["sessionId"].as_str().unwrap().to_owned();
            let mut next = prompt(json!(3), &session, "/alertes");
            next["params"]["_meta"] = json!({"tutorLocal":{"task":{
                "conversationId":conversation,"language":"fr","trainingContext":{
                    "conversation_id":conversation,"training_id":"training","domain_id":"verified-domain",
                    "revision":"7","selection_version":2,"name":"Géométrie"
                }
            }}});
            let mut wrong_context = next.clone();
            wrong_context["id"] = json!(90);
            wrong_context["params"]["_meta"]["tutorLocal"]["task"]["conversationId"] =
                json!(uuid::Uuid::new_v4().to_string());
            client.send(wrong_context).await;
            assert_eq!(client.read().await["error"]["code"], -32602);
            assert!(seen.lock().unwrap().is_empty());
            client.send(next).await;
            let mut permission_count = 0;
            let mut text = String::new();
            let mut terminal = String::new();
            loop {
                let message = client.read().await;
                if message.get("id") == Some(&json!(3)) {
                    assert_eq!(message["result"]["stopReason"], "end_turn", "{message}");
                    break;
                }
                if message["method"] == "session/request_permission" {
                    permission_count += 1;
                    assert_eq!(
                        message["params"]["toolCall"]["rawInput"]["tool"],
                        "get_pending_alerts"
                    );
                    assert_eq!(
                        message["params"]["toolCall"]["rawInput"]["arguments"],
                        json!({"domain_id":"verified-domain"})
                    );
                    assert!(!server
                        .observed
                        .lock()
                        .unwrap()
                        .iter()
                        .any(|(r, _)| r["method"] == "tools/call"));
                    // A stale response cannot satisfy the pending approval.
                    client.send(json!({"jsonrpc":"2.0","id":"unrelated-old-permission","result":{"outcome":{"outcome":"selected","optionId":"allow"}}})).await;
                    client.send(json!({"jsonrpc":"2.0","id":message["id"],"result":{"outcome":{"outcome":"selected","optionId":if allowed {"allow"} else {"reject"}}}})).await;
                } else {
                    let update = &message["params"]["update"];
                    match update["sessionUpdate"].as_str().unwrap() {
                        "agent_message_chunk" => {
                            text.push_str(update["content"]["text"].as_str().unwrap())
                        }
                        "tool_call" | "tool_call_update" => {
                            assert_eq!(update["title"], "get_pending_alerts");
                            terminal = update["status"].as_str().unwrap().to_owned();
                        }
                        _ => panic!("unexpected update"),
                    }
                }
            }
            assert_eq!(permission_count, 1);
            assert_eq!(terminal, if allowed { "completed" } else { "failed" });
            assert_eq!(
                server
                    .observed
                    .lock()
                    .unwrap()
                    .iter()
                    .filter(|(r, _)| r["method"] == "tools/call")
                    .count(),
                usize::from(allowed)
            );
            client.close().await;
            let owner: Owner = serde_json::from_value(owner).unwrap();
            let store =
                SessionStore::load(root.path(), &owner, &session, crate::config::MODEL_SHA256)
                    .unwrap();
            let turn = store.recent_turns(1).unwrap().pop().unwrap();
            assert_eq!(turn.user, "/alertes");
            assert_eq!(turn.assistant, text);
            assert_eq!(turn.status, Completion::Complete);
            let events = store
                .journal()
                .event_refs(Some(&turn.id), None, 128)
                .unwrap();
            assert_eq!(
                events
                    .iter()
                    .filter(|e| e.kind == "context_projection")
                    .count(),
                seen.lock().unwrap().len()
            );
            assert!(!events.iter().any(|e| e.kind == "harness_model_action"));
            assert_eq!(seen.lock().unwrap().len(), 1);
            assert_eq!(
                events
                    .iter()
                    .filter(|e| e.kind == "verified_host_context")
                    .count(),
                1
            );
            for event in events {
                let bytes = store.journal().resolve_content(&event.content).unwrap();
                assert!(!String::from_utf8_lossy(&bytes).contains("connection-secret"));
            }
        }
    }

    #[tokio::test]
    async fn durable_load_replays_full_history_but_keeps_model_context_short() {
        let root = tempfile::tempdir().unwrap();
        let options = || Options {
            data_dir: Some(root.path().into()),
            ..Options::default()
        };
        let owner = json!({"profileId":"profile1","accountId":"account1","tenantId":"tenant1","serverId":"https://school.example"});
        let mut client = Client::connect_options(
            engine(|_, _, emit| {
                emit("Réponse exacte 🦀")?;
                Ok(stats("end_turn"))
            }),
            65536,
            options(),
        );
        assert_eq!(
            client.initialize().await["result"]["agentCapabilities"]["loadSession"],
            true
        );
        client
            .send(request(
                json!(2),
                "session/new",
                json!({"cwd":root.path(),"mcpServers":[],"_meta":{"tutorLocal":{"owner":owner}}}),
            ))
            .await;
        let created = client.read().await;
        let session = created["result"]["sessionId"].as_str().unwrap().to_owned();
        for i in 0..30 {
            let id = json!(i + 3);
            client
                .send(prompt(
                    id.clone(),
                    &session,
                    &format!("Question {i} de l’élève"),
                ))
                .await;
            assert_eq!(client.completion(id).await.0, "Réponse exacte 🦀");
        }
        client.close().await;
        let generated = Arc::new(AtomicBool::new(false));
        let observed = generated.clone();
        let mut client = Client::connect_options(
            engine(move |request, _, emit| {
                observed.store(true, Ordering::Release);
                assert!(request.messages.len() <= 50);
                assert_eq!(
                    request.messages.last().unwrap().content,
                    "Continue explicitement"
                );
                emit("Nouvelle réponse")?;
                Ok(stats("end_turn"))
            }),
            65536,
            options(),
        );
        client.initialize().await;
        let mut wrong = owner.clone();
        wrong["accountId"] = json!("another-account");
        client.send(request(json!(2),"session/load",json!({"sessionId":session,"cwd":root.path(),"mcpServers":[],"_meta":{"tutorLocal":{"owner":wrong}}}))).await;
        assert!(client.read().await.get("error").is_some());
        client.send(request(json!(3),"session/load",json!({"sessionId":session,"cwd":root.path(),"mcpServers":[],"_meta":{"tutorLocal":{"owner":owner}}}))).await;
        let mut replayed_users = Vec::new();
        let mut replayed_agents = Vec::new();
        loop {
            let event = client.read().await;
            if event.get("id") == Some(&json!(3)) {
                assert!(event.get("result").is_some(), "{event}");
                break;
            }
            let text = event
                .pointer("/params/update/content/text")
                .and_then(Value::as_str)
                .unwrap()
                .to_owned();
            match event
                .pointer("/params/update/sessionUpdate")
                .and_then(Value::as_str)
            {
                Some("user_message_chunk") => replayed_users.push(text),
                Some("agent_message_chunk") => replayed_agents.push(text),
                _ => panic!("unexpected replay event"),
            }
        }
        assert_eq!(replayed_users.len(), 30);
        assert_eq!(replayed_agents.len(), 30);
        assert_eq!(replayed_users[0], "Question 0 de l’élève");
        assert_eq!(replayed_users[29], "Question 29 de l’élève");
        assert!(replayed_agents
            .iter()
            .all(|text| text == "Réponse exacte 🦀"));
        assert!(!generated.load(Ordering::Acquire));
        let refresh = json!({"sessionId":session,"cwd":root.path(),
            "mcpServers":[{"type":"http","name":"tutor","url":"https://school.example/mcp", "headers":[{"name":"Authorization","value":"Bearer refreshed-secret"}]}],
            "_meta":{"tutorLocal":{"owner":owner}}});
        let mut wrong_refresh = refresh.clone();
        wrong_refresh["_meta"]["tutorLocal"]["owner"]["accountId"] = json!("another-account");
        client
            .send(request(json!(10), "_tutor/rebind", wrong_refresh))
            .await;
        let rejected = client.read().await;
        assert_eq!(rejected["id"], 10);
        assert!(rejected.get("error").is_some());
        client
            .send(request(json!(11), "_tutor/rebind", refresh))
            .await;
        let refreshed = client.read().await;
        assert_eq!(refreshed["id"], 11, "rebind must not replay any history");
        assert!(refreshed.get("result").is_some());
        assert!(!refreshed.to_string().contains("refreshed-secret"));
        assert!(!generated.load(Ordering::Acquire));
        // Rebind behavior is independent of tool execution. Return this
        // text-generation fixture to an empty MCP configuration before use.
        client
            .send(request(
                json!(12),
                "_tutor/rebind",
                json!({
                    "sessionId":session,"cwd":root.path(),"mcpServers":[],
                    "_meta":{"tutorLocal":{"owner":owner}}
                }),
            ))
            .await;
        assert!(client.read().await.get("result").is_some());
        client
            .send(prompt(json!(4), &session, "Continue explicitement"))
            .await;
        assert_eq!(client.completion(json!(4)).await.0, "Nouvelle réponse");
        assert!(generated.load(Ordering::Acquire));
        client.close().await;
    }

    #[tokio::test]
    async fn context_projection_bounds_prompt_and_reports_omitted_history() {
        let seen = Arc::new(Mutex::new(Vec::new()));
        let captured = seen.clone();
        let mut client = Client::connect(
            engine(move |request, _, emit| {
                let count = 16
                    + request
                        .messages
                        .iter()
                        .map(|m| m.content.len().div_ceil(4))
                        .sum::<usize>();
                assert!(count + request.max_output_tokens as usize + 256 <= 4096);
                *captured.lock().unwrap() = request.messages;
                emit(&"Une explication terminée. ".repeat(30))?;
                Ok(stats("end_turn"))
            }),
            65536,
        );
        client.initialize().await;
        let session = client.new_session(2, json!([])).await;
        let mut last = Value::Null;
        for i in 0..30 {
            let id = json!(i + 3);
            client
                .send(prompt(
                    id.clone(),
                    &session,
                    &format!("Tour {i}: {}", "Pourquoi ? ".repeat(30)),
                ))
                .await;
            last = client.completion(id).await.1;
        }
        assert!(
            last.pointer("/result/_meta/tutor/context/omittedRecords")
                .and_then(Value::as_u64)
                .unwrap()
                > 0
        );
        {
            let messages = seen.lock().unwrap();
            assert!(messages.last().unwrap().content.starts_with("Tour 29:"));
            assert!(!messages.iter().any(|m| m.content.starts_with("Tour 0:")));
        }
        client.close().await;
    }

    #[tokio::test]
    async fn only_one_generation_can_run_and_cancelled_history_is_not_replayed() {
        let seen = Arc::new(Mutex::new(Vec::new()));
        let captured = seen.clone();
        let observed_cancel = Arc::new(AtomicBool::new(false));
        let observed = observed_cancel.clone();
        let mut calls = 0;
        let mut client = Client::connect(
            engine(move |req, cancel, emit| {
                calls += 1;
                captured.lock().unwrap().push(
                    req.messages
                        .into_iter()
                        .map(|m| (m.role, m.content))
                        .collect::<Vec<_>>(),
                );
                if calls == 1 {
                    emit("partial")?;
                    while !cancel.load(Ordering::Acquire) {
                        std::thread::sleep(Duration::from_millis(1));
                    }
                    observed.store(true, Ordering::Release);
                    Err("native error with Bearer secret-value".into())
                } else {
                    emit("next")?;
                    Ok(stats("end_turn"))
                }
            }),
            65536,
        );
        client.initialize().await;
        let first = client.new_session(2, json!([])).await;
        let second = client.new_session(3, json!([])).await;
        client.send(prompt(json!(4), &first, "first")).await;
        assert_eq!(
            client.read().await["params"]["update"]["content"]["text"],
            "partial"
        );
        client
            .send(prompt(json!(5), &second, "must not be accepted"))
            .await;
        assert_eq!(client.read().await["error"]["code"], -32003);
        client
            .send(request(
                json!(4),
                "session/cancel",
                json!({"sessionId":first}),
            ))
            .await;
        let duplicate = client.read().await;
        assert_eq!(duplicate["id"], Value::Null);
        assert_eq!(duplicate["error"]["code"], -32600);
        assert!(!observed_cancel.load(Ordering::Acquire));
        client
            .send(request(
                json!(6),
                "session/cancel",
                json!({"sessionId":second}),
            ))
            .await;
        assert!(client.read().await.get("result").is_some());
        assert!(!observed_cancel.load(Ordering::Acquire));
        client
            .send(json!({"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":first}}))
            .await;
        let cancelled = client.read().await;
        assert_eq!(cancelled["id"], 4);
        assert_eq!(cancelled["result"]["stopReason"], "cancelled");
        assert!(!cancelled.to_string().contains("secret-value"));
        client.send(prompt(json!(4), &first, "first")).await;
        assert_eq!(client.read().await["error"]["code"], -32600);
        client.send(prompt(json!(7), &first, "continue")).await;
        assert_eq!(client.completion(json!(7)).await.0, "next");
        let requests = seen.lock().unwrap().clone();
        assert_eq!(requests.len(), 2);
        assert_eq!(
            requests[1]
                .iter()
                .filter(|(_, text)| text == "first")
                .count(),
            1
        );
        assert_eq!(
            requests[1]
                .iter()
                .filter(|(_, text)| text == "partial")
                .count(),
            1
        );
        assert!(requests[1]
            .iter()
            .any(|(role, text)| role == "system" && text.contains("interrupted")));
        assert!(!format!("{requests:?}").contains("must not be accepted"));
        drop(requests);
        client.close().await;
    }

    #[tokio::test]
    async fn invalid_requests_do_not_reach_the_engine_or_echo_their_payloads() {
        let mut client = Client::connect(
            engine(|_, _, _| panic!("invalid request reached Engine")),
            65536,
        );
        client
            .send(request(
                json!(0),
                "session/new",
                json!({"cwd":"/tmp","mcpServers":[]}),
            ))
            .await;
        assert_eq!(client.read().await["error"]["code"], -32002);
        client
            .send(request(
                json!(1),
                "initialize",
                json!({"protocolVersion":2}),
            ))
            .await;
        assert_eq!(client.read().await["error"]["code"], -32602);
        client.initialize().await;
        let session = client.new_session(2, json!([])).await;
        for params in [
            json!({"sessionId":"unknown-secret","prompt":[{"type":"text","text":"secret"}]}),
            json!({"sessionId":null,"prompt":[{"type":"text","text":"secret"}]}),
            json!({"sessionId":session,"prompt":[]}),
            json!({"sessionId":session,"prompt":[{"type":"image","data":"secret"}]}),
            json!({"sessionId":session,"prompt":[{"type":"text","text":42}]}),
            json!({"sessionId":session,"prompt":[{"type":"text","text":"  \n "}]}),
            json!({"sessionId":session,"prompt":[{"type":"text","text":"x".repeat(MAX_PROMPT_BYTES+1)}]}),
            json!({"sessionId":session,"prompt":[{"type":"resource_link","uri":"secret"}]}),
        ] {
            client
                .send(request(json!(3), "session/prompt", params))
                .await;
            let reply = client.read().await;
            assert_eq!(reply["error"]["code"], -32602);
            assert!(!reply.to_string().contains("secret"));
        }
        for method in [
            "session/load",
            "session/resume",
            "session/set_model",
            "terminal/create",
            "tools/call",
        ] {
            client
                .send(request(
                    json!(4),
                    method,
                    json!({"sessionId":session,"secret":"opaque"}),
                ))
                .await;
            assert_eq!(client.read().await["error"]["code"], -32601);
        }
        for invalid in [
            json!([]),
            json!({"jsonrpc":"2.0","id":null,"method":"session/new"}),
            json!({"jsonrpc":"2.0","id":1.2,"method":"session/new"}),
            json!({"jsonrpc":"2.0","id":{},"method":"session/new"}),
            json!({"jsonrpc":"1.0","id":"secret","method":"session/new"}),
        ] {
            client.send(invalid).await;
            let reply = client.read().await;
            assert_eq!(reply["id"], Value::Null);
            assert_eq!(reply["error"]["code"], -32600);
        }
        client
            .input
            .write_all(b"{secret-invalid-json}\n")
            .await
            .unwrap();
        let error = client.read().await;
        assert_eq!(error["error"]["code"], -32700);
        assert!(!error.to_string().contains("secret"));
        client.close().await;
    }

    #[tokio::test]
    async fn fixed_model_configuration_and_opaque_mcp_storage_are_truthful() {
        let engine = engine(|_, _, _| panic!("unexpected generation"));
        let cancel = Arc::new(AtomicBool::new(false));
        let closed = AtomicBool::new(false);
        let mut server = Server::default();
        server
            .dispatch(
                request(json!(1), "initialize", json!({"protocolVersion":1})),
                &engine,
                &cancel,
                &closed,
            )
            .await;
        let servers =
            json!([{"command":"secret-command","env":[{"name":"TOKEN","value":"secret-token"}]}]);
        let response = server
            .dispatch(
                request(
                    json!(2),
                    "session/new",
                    json!({"cwd":"/private","mcpServers":servers}),
                ),
                &engine,
                &cancel,
                &closed,
            )
            .await
            .unwrap();
        assert!(!response.to_string().contains("secret"));
        let id = response["result"]["sessionId"].as_str().unwrap();
        assert_eq!(
            server.sessions[id]._connection._mcp_servers,
            servers.as_array().unwrap().clone()
        );
        assert_eq!(
            response["result"]["configOptions"][0]["currentValue"],
            MODEL_ID
        );
        assert_eq!(
            response["result"]["configOptions"][0]["options"]
                .as_array()
                .unwrap()
                .len(),
            1
        );
        let option = server
            .set_config(&json!({"sessionId":id,"configId":"model","value":MODEL_ID}))
            .unwrap();
        assert_eq!(option["configOptions"], response["result"]["configOptions"]);
        assert!(server
            .set_config(&json!({"sessionId":id,"configId":"model","value":"some-secret-path"}))
            .is_err());
        assert!(server
            .set_config(&json!({"sessionId":id,"configId":"thought_level","value":"high"}))
            .is_err());
        for _ in 1..MAX_SESSIONS {
            server
                .new_session(&json!({"cwd":"/private","mcpServers":[]}))
                .await
                .unwrap();
        }
        assert_eq!(
            server
                .new_session(&json!({"cwd":"/private","mcpServers":[]}))
                .await
                .unwrap_err()
                .0,
            -32004
        );
    }

    #[tokio::test]
    async fn empty_eof_cancels_inflight_without_waiting_for_another_frame() {
        let cancelled = Arc::new(AtomicBool::new(false));
        let observed = cancelled.clone();
        let mut client = Client::connect(
            engine(move |_, cancel, emit| {
                emit("started")?;
                while !cancel.load(Ordering::Acquire) {
                    std::thread::sleep(Duration::from_millis(1));
                }
                observed.store(true, Ordering::Release);
                Err("cancelled".into())
            }),
            65536,
        );
        client.initialize().await;
        let session = client.new_session(2, json!([])).await;
        client.send(prompt(json!(3), &session, "wait")).await;
        assert_eq!(
            client.read().await["params"]["update"]["content"]["text"],
            "started"
        );
        client.input.shutdown().await.unwrap();
        wait_flag(&cancelled).await;
        timeout(Duration::from_secs(3), client.server)
            .await
            .unwrap()
            .unwrap()
            .unwrap();
    }

    #[tokio::test]
    async fn dropping_server_future_also_cancels_the_worker() {
        let cancelled = Arc::new(AtomicBool::new(false));
        let observed = cancelled.clone();
        let mut client = Client::connect(
            engine(move |_, cancel, emit| {
                emit("started")?;
                while !cancel.load(Ordering::Acquire) {
                    std::thread::sleep(Duration::from_millis(1));
                }
                observed.store(true, Ordering::Release);
                Err("cancelled".into())
            }),
            65536,
        );
        client.initialize().await;
        let session = client.new_session(2, json!([])).await;
        client.send(prompt(json!(3), &session, "wait")).await;
        client.read().await;
        client.server.abort();
        let _ = client.server.await;
        wait_flag(&cancelled).await;
    }

    #[tokio::test]
    async fn failed_turn_retains_exact_partial_history_without_leaking_native_errors() {
        let seen = Arc::new(Mutex::new(Vec::new()));
        let captured = seen.clone();
        let mut call = 0;
        let mut client = Client::connect(
            engine(move |req, _, emit| {
                call += 1;
                captured.lock().unwrap().push(
                    req.messages
                        .into_iter()
                        .map(|m| (m.role, m.content))
                        .collect::<Vec<_>>(),
                );
                if call == 1 {
                    emit("incomplete")?;
                    Err("C:\\private\\model.gguf Bearer secret-token".into())
                } else {
                    emit("new answer")?;
                    Ok(stats("max_tokens"))
                }
            }),
            65536,
        );
        client.initialize().await;
        let session = client.new_session(2, json!([])).await;
        client.send(prompt(json!(3), &session, "original")).await;
        let (text, error) = client.completion(json!(3)).await;
        assert_eq!(text, "incomplete");
        assert_eq!(error["error"]["code"], -32603);
        assert!(!error.to_string().contains("secret-token"));
        client
            .send(prompt(json!(4), &session, "retry differently"))
            .await;
        let (text, result) = client.completion(json!(4)).await;
        assert_eq!(text, "new answer");
        assert_eq!(result["result"]["stopReason"], "max_tokens");
        let requests = seen.lock().unwrap().clone();
        assert_eq!(requests[1][1], ("user".into(), "original".into()));
        assert_eq!(requests[1][2], ("assistant".into(), "incomplete".into()));
        assert!(requests[1][3].1.contains("failed"));
        drop(requests);
        client.close().await;
    }

    #[tokio::test]
    async fn output_is_utf8_chunked_bounded_and_unknown_stop_reasons_are_private() {
        let large = "é🦀".repeat(2000);
        let emitted = large.clone();
        let mut calls = 0;
        let mut client = Client::connect(
            engine(move |_, _, emit| {
                calls += 1;
                if calls == 1 {
                    emit(&emitted)?;
                    Ok(stats("end_turn"))
                } else if calls == 2 {
                    emit(&"x".repeat(MAX_OUTPUT_BYTES + 1))?;
                    Ok(stats("end_turn"))
                } else {
                    Ok(stats("Bearer secret-token"))
                }
            }),
            65536,
        );
        client.initialize().await;
        let session = client.new_session(2, json!([])).await;
        client.send(prompt(json!(3), &session, "unicode")).await;
        let mut collected = String::new();
        loop {
            let value = client.read().await;
            if value.get("id") == Some(&json!(3)) {
                break;
            }
            let chunk = value["params"]["update"]["content"]["text"]
                .as_str()
                .unwrap();
            assert!(chunk.len() <= CHUNK_BYTES);
            collected.push_str(chunk);
        }
        assert_eq!(collected, large);
        client.send(prompt(json!(4), &session, "too long")).await;
        assert_eq!(client.read().await["error"]["code"], -32005);
        client
            .send(prompt(json!(5), &session, "unknown reason"))
            .await;
        let error = client.read().await;
        assert_eq!(error["error"]["code"], -32603);
        assert!(!error.to_string().contains("secret-token"));
        client.close().await;
    }

    #[tokio::test]
    async fn frame_reader_bounds_even_unterminated_input_and_rejects_partial_eof() {
        let valid = b"{\"jsonrpc\":\"2.0\"}\r\n";
        let mut reader = BufReader::new(&valid[..]);
        assert_eq!(
            read_frame(&mut reader).await.unwrap().unwrap(),
            b"{\"jsonrpc\":\"2.0\"}"
        );
        assert!(read_frame(&mut reader).await.unwrap().is_none());
        for invalid in [
            b"{unfinished secret".to_vec(),
            vec![b'x'; MAX_FRAME_BYTES + 1],
        ] {
            let mut reader = BufReader::new(invalid.as_slice());
            assert!(read_frame(&mut reader).await.is_err());
        }
    }

    #[tokio::test]
    async fn exact_runtime_context_error_is_actionable_without_forwarding_arbitrary_errors() {
        let mut client = Client::connect(
            engine(|_, _, _| Err("context_budget_exceeded".into())),
            65536,
        );
        client.initialize().await;
        let session = client.new_session(2, json!([])).await;
        client
            .send(prompt(json!(3), &session, "full context"))
            .await;
        let result = client.read().await;
        assert_eq!(result["error"]["code"], -32004);
        assert!(result["error"]["message"]
            .as_str()
            .unwrap()
            .contains("split the task"));
        client.close().await;
    }

    #[tokio::test]
    async fn slow_output_has_a_deadline_and_cancels_backpressured_inference() {
        let cancelled = Arc::new(AtomicBool::new(false));
        let observed = cancelled.clone();
        let mut client = Client::connect(
            engine(move |_, cancel, emit| {
                let result = emit(&"x".repeat(MAX_OUTPUT_BYTES));
                if cancel.load(Ordering::Acquire) {
                    observed.store(true, Ordering::Release);
                }
                result?;
                Ok(stats("end_turn"))
            }),
            1024,
        );
        client.initialize().await;
        let session = client.new_session(2, json!([])).await;
        client.send(prompt(json!(3), &session, "answer")).await;
        // Intentionally never consume the model output from this small pipe.
        let result = timeout(Duration::from_secs(5), client.server)
            .await
            .unwrap()
            .unwrap();
        assert!(result.is_err());
        wait_flag(&cancelled).await;
    }

    #[tokio::test]
    async fn history_capacity_is_rejected_without_dropping_accepted_turns() {
        let engine = engine(|_, _, _| panic!("full history reached engine"));
        let cancel = Arc::new(AtomicBool::new(false));
        let closed = AtomicBool::new(false);
        let mut server = Server::default();
        let created = server
            .new_session(&json!({"cwd":"/tmp","mcpServers":[]}))
            .await
            .unwrap();
        let id = created["sessionId"].as_str().unwrap();
        let session = server.sessions.get_mut(id).unwrap();
        session.turns.push(Turn {
            id: uuid::Uuid::new_v4().to_string(),
            user: "preserved".into(),
            assistant: String::new(),
            status: TurnStatus::Interrupted,
            projection: None,
        });
        session.history_bytes = MAX_HISTORY_BYTES - MAX_OUTPUT_BYTES;
        let params = json!({"sessionId":id,"prompt":[{"type":"text","text":"next"}]});
        assert_eq!(
            server
                .start_prompt(&json!(1), &params, &engine, &cancel, &closed)
                .unwrap_err()
                .0,
            -32004
        );
        let session = &server.sessions[id];
        assert_eq!(session.turns.len(), 1);
        assert_eq!(session.turns[0].user, "preserved");
        assert!(session.turns[0].status == TurnStatus::Interrupted);
        assert!(server.prompt_ids.is_empty());
    }

    #[tokio::test]
    async fn host_approval_only_resolves_the_exact_pending_rpc_and_never_changes_history() {
        let mut server = Server {
            initialized: true,
            ..Server::default()
        };
        let created = server
            .new_session(&json!({"cwd":"/tmp","mcpServers":[]}))
            .await
            .unwrap();
        let session = created["sessionId"].as_str().unwrap().to_owned();
        let (_tx, chunks) = mpsc::channel(1);
        server.active = Some(Active {
            id: json!(3),
            session_id: session.clone(),
            chunks,
            worker: tokio::spawn(async { Ok(WorkResult::Replay) }),
            output_overflow: Arc::new(AtomicBool::new(false)),
            deadline: Instant::now() + GENERATION_TIMEOUT,
            projection: Arc::new(Mutex::new(None)),
            replay: false,
            permission: None,
        });
        let cancel = Arc::new(AtomicBool::new(false));
        let closed = AtomicBool::new(false);
        let engine = engine(|_, _, _| panic!("approval must not call the engine"));
        let (sender, receiver) = std::sync::mpsc::sync_channel(1);
        let id = json!("permission:public-fixture-operation");
        let frame = server
            .stream_message(
                StreamItem::Permission {
                    id: id.clone(),
                    params: json!({"sessionId":session}),
                    response: sender,
                },
                &cancel,
            )
            .unwrap()
            .unwrap();
        assert_eq!(frame["method"], "session/request_permission");
        assert!(server.dispatch(json!({"jsonrpc":"2.0","id":"permission:old","result":{"outcome":{"outcome":"selected","optionId":"allow"}}}),&engine,&cancel,&closed).await.is_none());
        assert!(matches!(
            receiver.try_recv(),
            Err(std::sync::mpsc::TryRecvError::Empty)
        ));
        let result = json!({"outcome":{"outcome":"selected","optionId":"allow"}});
        assert!(server
            .dispatch(
                json!({"jsonrpc":"2.0","id":id,"result":result}),
                &engine,
                &cancel,
                &closed
            )
            .await
            .is_none());
        assert_eq!(receiver.try_recv().unwrap(), result);
        assert!(server.active.as_ref().unwrap().permission.is_none());
        assert!(server.sessions[&session].turns.is_empty());
        assert_eq!(server.sessions[&session].history_bytes, 0);
    }

    #[tokio::test]
    async fn worker_approval_wait_observes_cancellation_without_a_host_reply() {
        let (tx, mut rx) = mpsc::channel(8);
        let cancel = Arc::new(AtomicBool::new(false));
        let flag = cancel.clone();
        let worker = tokio::task::spawn_blocking(move || {
            let mut output = WorkerOutput {
                chunks: tx,
                cancel: flag,
                overflow: Arc::new(AtomicBool::new(false)),
                store: None,
                turn: "fixture".into(),
                bytes: 0,
                controls: 0,
            };
            output.tool_update(json!({"sessionUpdate":"tool_call","toolCallId":"fixture","title":"get_pending_alerts","status":"pending"})).unwrap();
            output.approve("fixture", json!({"sessionId":"session"}))
        });
        assert!(matches!(rx.recv().await, Some(StreamItem::ToolUpdate(_))));
        let event = rx.recv().await.unwrap();
        assert!(matches!(&event, StreamItem::Permission { .. }));
        cancel.store(true, Ordering::Release);
        assert_eq!(
            timeout(Duration::from_secs(1), worker)
                .await
                .unwrap()
                .unwrap()
                .unwrap_err(),
            "cancelled"
        );
        drop(event);
    }
}
