// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! Bounded, bidirectional JSON-RPC over a supervised agent's standard streams.
//!
//! Version/capability negotiation belongs to the caller. This transport also
//! works for a local MCP server; it never assumes an ACP-shaped `initialize`.

use std::collections::{HashMap, HashSet, VecDeque};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex as StdMutex, Weak};
use std::time::{Duration, Instant};

use serde_json::{json, Value};
use tokio::io::{AsyncBufRead, AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader};
use tokio::process::{ChildStdin, Command};
use tokio::sync::{mpsc, oneshot, watch, Mutex};
use tokio::time::timeout;

use crate::launch::LaunchSpec;
use crate::timing::{local_id, millis, rpc_method, TimingJournal, TimingOutcome};

const MAX_FRAME_BYTES: usize = 8 * 1024 * 1024;
const MAX_PENDING: usize = 64;
const WRITE_TIMEOUT: Duration = Duration::from_secs(15);
const STOP_TIMEOUT: Duration = Duration::from_secs(10);
/// Consecutive malformed frames tolerated before the transport is closed.
const MAX_CONSECUTIVE_INVALID_FRAMES: u32 = 16;
/// Private stderr retention; only a redacted tail ever leaves this module.
const STDERR_TAIL_BYTES: usize = 64 * 1024;
/// Redacted stderr excerpt attached to an unexpected-exit diagnostic.
const STDERR_DIAGNOSTIC_BYTES: usize = 8 * 1024;
/// session/prompt fails only after this long without any session/update for
/// its session (and no pending permission), re-armed by every update.
const PROMPT_IDLE_TIMEOUT: Duration = Duration::from_secs(120);
/// Absolute ceiling for one session/prompt, whatever its activity.
const PROMPT_MAX_DURATION: Duration = Duration::from_secs(30 * 60);
pub const REQUEST_CANCELLED_BEFORE_WRITE: &str = "Préparation annulée.";

/// Stable classification of an ACP/JSON-RPC failure. Provider text never
/// decides the kind except for the documented auth wording on -32000.
#[derive(Clone, Copy, Debug, PartialEq, Eq, serde::Serialize)]
#[serde(rename_all = "snake_case")]
pub enum ErrorKind {
    RateLimit,
    Overloaded,
    Auth,
    UsageLimit,
    Context,
    Transport,
    Protocol,
    Cancelled,
    Other,
}

impl ErrorKind {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::RateLimit => "rate_limit",
            Self::Overloaded => "overloaded",
            Self::Auth => "auth",
            Self::UsageLimit => "usage_limit",
            Self::Context => "context",
            Self::Transport => "transport",
            Self::Protocol => "protocol",
            Self::Cancelled => "cancelled",
            Self::Other => "other",
        }
    }

    pub fn parse(kind: &str) -> Option<Self> {
        [
            Self::RateLimit,
            Self::Overloaded,
            Self::Auth,
            Self::UsageLimit,
            Self::Context,
            Self::Transport,
            Self::Protocol,
            Self::Cancelled,
            Self::Other,
        ]
        .into_iter()
        .find(|candidate| candidate.as_str() == kind)
    }

    /// Kinds during which new provider requests are pointless for a while.
    pub fn is_limit(self) -> bool {
        matches!(self, Self::RateLimit | Self::Overloaded | Self::UsageLimit)
    }
}

#[derive(Clone, Debug)]
pub struct AcpError {
    pub kind: ErrorKind,
    pub retry_after: Option<Duration>,
    pub message: String,
}

impl AcpError {
    fn new(kind: ErrorKind, message: impl Into<String>) -> Self {
        Self {
            kind,
            retry_after: None,
            message: message.into(),
        }
    }
}

impl std::fmt::Display for AcpError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.message)
    }
}

impl From<AcpError> for String {
    fn from(error: AcpError) -> Self {
        error.message
    }
}

impl From<&str> for AcpError {
    fn from(message: &str) -> Self {
        Self::new(ErrorKind::Transport, message)
    }
}

impl From<String> for AcpError {
    fn from(message: String) -> Self {
        Self::new(ErrorKind::Transport, message)
    }
}

type RpcResponse = Result<Value, String>;
type TypedResponse = Result<Value, AcpError>;
type PendingRequests = HashMap<u64, oneshot::Sender<TypedResponse>>;

/// Why one inbound frame was not processed.
enum FrameError {
    /// Malformed or unexpected frame: skipped and counted.
    Invalid,
    /// The transport itself is unusable (already failed, or lock poisoned).
    Fatal(String),
}

impl From<&str> for FrameError {
    fn from(_: &str) -> Self {
        Self::Invalid
    }
}

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
    connection_id: String,
    started: Instant,
    timings: TimingJournal,
    stderr_tail: StdMutex<VecDeque<u8>>,
    invalid_frames: AtomicU64,
    /// Last session/update per session with an outstanding session/prompt.
    prompt_activity: StdMutex<HashMap<String, Instant>>,
    /// Inactivity limit of session/prompt in milliseconds; 0 disables it.
    prompt_idle_ms: AtomicU64,
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
    operation_id: Option<String>,
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
            // The first user thread must not run or spawn descendants before
            // its job is attached. Piped stdio does not require a console.
            const CREATE_NO_WINDOW: u32 = 0x0800_0000;
            command.creation_flags(
                CREATE_NO_WINDOW | windows_sys::Win32::System::Threading::CREATE_SUSPENDED,
            );
        }
        #[cfg(windows)]
        let windows_job = windows_job::WindowsJob::new()?;

        let mut child = command
            .spawn()
            .map_err(|error| format!("Impossible de lancer l’agent : {error}"))?;
        let pid = child
            .id()
            .ok_or_else(|| "Le processus de l’agent n’a pas d’identifiant.".to_string())?;
        #[cfg(windows)]
        if let Err(message) = windows_job.assign_and_resume(&child, pid) {
            // No ACP request is sent and no agent user code is allowed to run
            // if its job/thread setup fails. Dropping the job kills any process
            // already assigned to it; kill_on_drop is an additional fallback.
            drop(windows_job);
            let _ = child.start_kill();
            let _ = timeout(Duration::from_secs(3), child.wait()).await;
            return Err(message);
        }
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
            connection_id: uuid::Uuid::new_v4().to_string(),
            started: Instant::now(),
            timings: TimingJournal::default(),
            stderr_tail: StdMutex::new(VecDeque::new()),
            invalid_frames: AtomicU64::new(0),
            prompt_activity: StdMutex::new(HashMap::new()),
            prompt_idle_ms: AtomicU64::new(PROMPT_IDLE_TIMEOUT.as_millis() as u64),
        });

        let reader_inner = Arc::downgrade(&inner);
        tokio::spawn(async move {
            let mut reader = BufReader::new(stdout);
            let mut consecutive_invalid = 0_u32;
            loop {
                let frame = read_frame(&mut reader).await;
                let Some(inner) = reader_inner.upgrade() else {
                    break;
                };
                if inner.closed.load(Ordering::Acquire) {
                    break;
                }
                match frame {
                    Ok(Some(frame)) => match dispatch(&inner, &frame).await {
                        Ok(()) => consecutive_invalid = 0,
                        Err(FrameError::Invalid) => {
                            // A stray log line or an atypical envelope must not
                            // kill a healthy harness. Its content is never kept.
                            inner.invalid_frames.fetch_add(1, Ordering::Relaxed);
                            consecutive_invalid += 1;
                            if consecutive_invalid >= MAX_CONSECUTIVE_INVALID_FRAMES {
                                fail_with_diagnostic(
                                    &inner,
                                    "error",
                                    "L’agent envoie des réponses RPC invalides de façon répétée.",
                                );
                                break;
                            }
                        }
                        Err(FrameError::Fatal(message)) => {
                            fail_with_diagnostic(&inner, "error", &message);
                            break;
                        }
                    },
                    Ok(None) => break,
                    Err(message) => {
                        fail_with_diagnostic(&inner, "error", &message);
                        break;
                    }
                }
            }
            let _ = reader_done_tx.send(());
            // Let the supervisor publish the exit code first. Raw stderr stays
            // private; EOF must not hide a useful process exit diagnosis.
            if let Some(inner) = reader_inner.upgrade() {
                tokio::time::sleep(Duration::from_millis(250)).await;
                if !inner.closed.load(Ordering::Acquire) {
                    fail_with_diagnostic(
                        &inner,
                        "closed",
                        "La sortie de l’agent a été fermée avant sa réponse. Reconnectez l’agent.",
                    );
                }
            }
        });

        // Drain in fixed chunks: stderr may contain a very long line, provider
        // credentials, prompts, or access tokens. Only the last 64 KiB stay in
        // memory; a short, redacted tail is attached to an unexpected-exit
        // diagnostic and is never persisted.
        let stderr_inner = Arc::downgrade(&inner);
        tokio::spawn(async move {
            let mut stderr = stderr;
            let mut buffer = [0_u8; 4096];
            loop {
                match stderr.read(&mut buffer).await {
                    Ok(0) | Err(_) => break,
                    Ok(read) => {
                        let Some(inner) = stderr_inner.upgrade() else {
                            continue;
                        };
                        if let Ok(mut tail) = inner.stderr_tail.lock() {
                            push_tail(&mut tail, &buffer[..read]);
                        };
                    }
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
                            Ok(status) if status.success() => "Le processus de l’agent s’est arrêté.".to_string(),
                            Ok(status) => format!("L’agent s’est arrêté avec une erreur (code {}). Vérifiez sa connexion et son installation.", status.code().map(|code|code.to_string()).unwrap_or_else(||"signal".into())),
                            Err(_) => "Le statut du processus de l’agent est indisponible.".to_string(),
                        };
                        fail_with_diagnostic(&inner, "closed", &message);
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
                        #[cfg(unix)]
                        terminate_tree(pid).await;
                        #[cfg(windows)]
                        let _ = windows_job.terminate();
                        let _ = child.start_kill();
                        let _ = timeout(Duration::from_secs(3), child.wait()).await;
                    } else {
                        #[cfg(unix)]
                        terminate_tree(pid).await;
                    }
                }
            }
            #[cfg(windows)]
            if let Err(message) = windows_job.finish().await {
                if let Some(inner) = supervisor_inner.upgrade() {
                    let _ = inner
                        .events
                        .send(json!({"type":"connection","status":"error","message":message}));
                }
                // Keep exited=false. stop() must report the failed cleanup,
                // rather than treating a disconnected watch as confirmation.
                return;
            }
            let _ = exit_tx.send(true);
        });

        Ok(Self {
            inner,
            operation_id: None,
        })
    }

    pub async fn request(&self, method: &str, params: Value) -> RpcResponse {
        self.request_typed(method, params)
            .await
            .map_err(String::from)
    }

    /// Same as `request`, keeping the classified failure.
    pub async fn request_typed(&self, method: &str, params: Value) -> TypedResponse {
        self.request_with_gate(method, params, request_deadline(method), None)
            .await
    }

    /// Override the session/prompt inactivity limit (None: only the absolute
    /// ceiling applies, e.g. for a local model with a long silent prefill).
    pub fn set_prompt_idle_timeout(&self, idle: Option<Duration>) {
        self.inner.prompt_idle_ms.store(
            idle.map_or(0, |idle| idle.as_millis().max(1) as u64),
            Ordering::Relaxed,
        );
    }

    /// Malformed inbound frames skipped since the process started.
    pub fn invalid_frames(&self) -> u64 {
        self.inner.invalid_frames.load(Ordering::Relaxed)
    }

    /// Attribute diagnostics locally without changing any JSON-RPC parameters.
    pub fn for_operation(&self, operation_id: &str) -> Self {
        Self {
            inner: self.inner.clone(),
            operation_id: Some(local_id(operation_id)),
        }
    }

    pub async fn request_in_operation(
        &self,
        method: &str,
        params: Value,
        operation_id: &str,
    ) -> RpcResponse {
        self.for_operation(operation_id)
            .request(method, params)
            .await
    }

    /// Cancellation is checked again while holding the writer lock. If this
    /// returns REQUEST_CANCELLED_BEFORE_WRITE no request bytes were written.
    /// Cancellation after that boundary uses normal ACP session cancellation.
    pub async fn request_in_operation_cancellable(
        &self,
        method: &str,
        params: Value,
        operation_id: &str,
        cancelled: &AtomicBool,
    ) -> RpcResponse {
        self.for_operation(operation_id)
            .request_with_gate(method, params, request_deadline(method), Some(cancelled))
            .await
            .map_err(String::from)
    }

    /// Bounded, content-free diagnostic records, including unattributed calls.
    pub fn timings(&self) -> Vec<Value> {
        self.inner.timings.snapshot()
    }

    #[cfg(test)]
    async fn request_with_deadline(
        &self,
        method: &str,
        params: Value,
        deadline: Duration,
    ) -> RpcResponse {
        self.request_with_gate(method, params, deadline, None)
            .await
            .map_err(String::from)
    }

    async fn request_with_gate(
        &self,
        method: &str,
        params: Value,
        deadline: Duration,
        cancelled: Option<&AtomicBool>,
    ) -> TypedResponse {
        validate_method(method).map_err(|m| AcpError::new(ErrorKind::Protocol, m))?;
        let id = self.inner.next_id.fetch_add(1, Ordering::Relaxed);
        let mut timing = RpcTiming::new(self.inner.clone(), id, self.operation_id.clone(), method);
        let mut forced_outcome = None;
        let prompt_session = (method == "session/prompt")
            .then(|| {
                params
                    .get("sessionId")
                    .and_then(Value::as_str)
                    .map(str::to_owned)
            })
            .flatten();
        let _activity = prompt_session.as_ref().map(|session| {
            if let Ok(mut activity) = self.inner.prompt_activity.lock() {
                activity.insert(session.clone(), Instant::now());
            }
            ActivityGuard {
                inner: Arc::downgrade(&self.inner),
                session: session.clone(),
            }
        });
        let result: TypedResponse = async {
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
            write_message_checked(
                &self.inner,
                &json!({
                    "jsonrpc": "2.0", "id": id, "method": method, "params": params
                }),
                cancelled,
            )
            .await
            .map_err(|message| {
                if message == REQUEST_CANCELLED_BEFORE_WRITE {
                    AcpError::new(ErrorKind::Cancelled, message)
                } else {
                    AcpError::new(ErrorKind::Transport, message)
                }
            })?;
            let waited = match &prompt_session {
                Some(session) => {
                    wait_for_prompt(
                        &self.inner,
                        session,
                        receiver,
                        deadline.min(PROMPT_MAX_DURATION),
                    )
                    .await
                }
                None => timeout(deadline, receiver).await.map_err(|_| ()),
            };
            match waited {
                Ok(Ok(response)) => response,
                Ok(Err(_)) => Err("La connexion à l’agent a été interrompue.".into()),
                Err(_) => {
                    forced_outcome = Some(TimingOutcome::TimedOut);
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
                    Err(AcpError::new(
                        ErrorKind::Transport,
                        format!("Le délai de réponse de l’agent est dépassé pour {method}."),
                    ))
                }
            }
        }
        .await;
        let outcome = forced_outcome.unwrap_or_else(|| match &result {
            Ok(response)
                if response.get("stopReason").and_then(Value::as_str) == Some("cancelled") =>
            {
                TimingOutcome::Cancelled
            }
            Ok(response) if response.get("isError").and_then(Value::as_bool) != Some(true) => {
                TimingOutcome::Succeeded
            }
            Err(error) if error.message == REQUEST_CANCELLED_BEFORE_WRITE => {
                TimingOutcome::Cancelled
            }
            _ => TimingOutcome::Failed,
        });
        timing.finish(outcome);
        result
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
            let session = permission.session_id.clone();
            pending.remove(&key);
            // The human decision restarts the inactivity window: the time
            // spent waiting for the learner is not silence of the harness.
            touch_prompt_activity(&self.inner, &session);
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
                exited
                    .changed()
                    .await
                    .map_err(|_| "L’arrêt complet de l’agent n’a pas été confirmé.".to_string())?;
            }
            Ok::<(), String>(())
        })
        .await
        .map_err(|_| "L’arrêt de l’agent n’a pas été confirmé.".to_string())??;
        Ok(())
    }
}

/// Exactly one terminal diagnostic is recorded even when an awaiting task is
/// aborted. No parameter, response body, error message or remote ID is copied.
struct RpcTiming {
    inner: Arc<Inner>,
    request_id: u64,
    operation_id: Option<String>,
    method: &'static str,
    started: Instant,
    finished: bool,
}

impl RpcTiming {
    fn new(inner: Arc<Inner>, request_id: u64, operation_id: Option<String>, method: &str) -> Self {
        let timing = Self {
            inner,
            request_id,
            operation_id,
            method: rpc_method(method),
            started: Instant::now(),
            finished: false,
        };
        timing.record("started", None);
        timing
    }

    fn record(&self, state: &str, outcome: Option<TimingOutcome>) {
        let now = Instant::now();
        let mut event = json!({"type":"rpc_timing","connectionId":self.inner.connection_id,
            "requestId":self.request_id,"method":self.method,"state":state,
            "elapsedMs":millis(self.started,now),"offsetMs":millis(self.inner.started,now)});
        if let Some(operation_id) = &self.operation_id {
            event["operationId"] = json!(operation_id);
        }
        if let Some(outcome) = outcome {
            event["outcome"] = json!(outcome.as_str());
        }
        self.inner.timings.push(event.clone());
        if self.operation_id.is_some() {
            let _ = self.inner.events.send(event);
        }
    }

    fn finish(&mut self, outcome: TimingOutcome) {
        if !self.finished {
            self.finished = true;
            self.record("completed", Some(outcome));
        }
    }
}

impl Drop for RpcTiming {
    fn drop(&mut self) {
        self.finish(TimingOutcome::Cancelled);
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

fn request_deadline(method: &str) -> Duration {
    match method {
        "session/prompt" => Duration::from_secs(30 * 60),
        "authenticate" | "auth/login" => Duration::from_secs(5 * 60),
        "session/new" | "session/load" | "session/resume" => Duration::from_secs(90),
        _ => Duration::from_secs(30),
    }
}

async fn write_message(inner: &Inner, message: &Value) -> Result<(), String> {
    write_message_checked(inner, message, None).await
}

async fn write_message_checked(
    inner: &Inner,
    message: &Value,
    cancelled: Option<&AtomicBool>,
) -> Result<(), String> {
    if cancelled.is_some_and(|flag| flag.load(Ordering::Acquire)) {
        return Err(REQUEST_CANCELLED_BEFORE_WRITE.into());
    }
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
        if cancelled.is_some_and(|flag| flag.load(Ordering::Acquire)) {
            return Err(REQUEST_CANCELLED_BEFORE_WRITE);
        }
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
        Ok(Err(REQUEST_CANCELLED_BEFORE_WRITE)) => Err(REQUEST_CANCELLED_BEFORE_WRITE.into()),
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

async fn dispatch(inner: &Inner, frame: &[u8]) -> Result<(), FrameError> {
    if frame.iter().all(u8::is_ascii_whitespace) {
        return Ok(()); // Blank keep-alive lines are harmless.
    }
    let packet: Value = serde_json::from_slice(frame).map_err(|_| FrameError::Invalid)?;
    let object = packet.as_object().ok_or(FrameError::Invalid)?;
    if object.get("jsonrpc").and_then(Value::as_str) != Some("2.0") {
        return Err(FrameError::Invalid);
    }
    let fatal = |message: String| FrameError::Fatal(message);
    if let Some(method) = object.get("method") {
        let method = method.as_str().ok_or(FrameError::Invalid)?;
        validate_method(method).map_err(|_| FrameError::Invalid)?;
        if object.contains_key("result") || object.contains_key("error") {
            return Err(FrameError::Invalid);
        }
        if let Some(id) = object.get("id") {
            let key = rpc_id_key(id).ok_or(FrameError::Invalid)?;
            if method == "session/request_permission" {
                let permission = parse_permission(&packet);
                match permission {
                    Some(permission) => {
                        let accepted = {
                            let mut pending = inner
                                .permissions
                                .lock()
                                .map_err(|_| fatal("Verrou ACP indisponible.".into()))?;
                            if inner.closed.load(Ordering::Acquire) {
                                return Ok(());
                            }
                            if pending.contains_key(&key) {
                                // A reused pending ID is ignored; the original
                                // request keeps waiting for its human decision.
                                return Err(FrameError::Invalid);
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
                                cancel_permissions(inner, None).await.map_err(fatal)?;
                            }
                        } else {
                            write_message(
                                inner,
                                &json!({"jsonrpc":"2.0", "id":id,
                                "error":{"code":-32000,"message":"Client busy"}}),
                            )
                            .await
                            .map_err(fatal)?;
                        }
                    }
                    None => write_message(
                        inner,
                        &json!({"jsonrpc":"2.0", "id":id,
                        "error":{"code":-32602,"message":"Invalid permission request"}}),
                    )
                    .await
                    .map_err(fatal)?,
                }
            } else {
                write_message(
                    inner,
                    &json!({"jsonrpc":"2.0", "id":id,
                    "error":{"code":-32601,"message":"Method not supported by this client"}}),
                )
                .await
                .map_err(fatal)?;
            }
        } else {
            if method == "$/cancel_request" {
                if let Some(id) = packet.pointer("/params/requestId") {
                    if let Some(key) = rpc_id_key(id) {
                        let permission = inner
                            .permissions
                            .lock()
                            .map_err(|_| fatal("Verrou ACP indisponible.".into()))?
                            .remove(&key);
                        if let Some(permission) = permission {
                            touch_prompt_activity(inner, &permission.session_id);
                            write_message(
                                inner,
                                &json!({"jsonrpc":"2.0", "id":permission.id,
                                "result":{"outcome":{"outcome":"cancelled"}}}),
                            )
                            .await
                            .map_err(fatal)?;
                        }
                    }
                }
            }
            if method == "session/update" {
                if let Some(session) = packet.pointer("/params/sessionId").and_then(Value::as_str) {
                    touch_prompt_activity(inner, session);
                }
                if let Some(limit) = packet
                    .pointer("/params/update/_meta/_claude~1rateLimit")
                    .and_then(claude_rate_limit)
                {
                    let _ = inner.events.send(limit_event(&limit));
                }
            }
            // Notifications cannot execute client actions. Forward them for
            // capability-aware presentation, including future ACP updates.
            let _ = inner.events.send(packet);
        }
        return Ok(());
    }
    let Some(id) = response_id(object.get("id")) else {
        return Err(FrameError::Invalid);
    };
    let result = if object.contains_key("result") == object.contains_key("error") {
        Err(AcpError::new(
            ErrorKind::Protocol,
            "La réponse RPC de l’agent ne contient ni un résultat ni une erreur valides.",
        ))
    } else if let Some(error) = object.get("error") {
        let error = classify_rpc_error(error);
        if error.kind == ErrorKind::Auth {
            let _ = inner.events.send(json!({"type":"authentication_required"}));
        }
        if error.kind.is_limit() {
            let _ = inner.events.send(limit_event(&error));
        }
        Err(error)
    } else {
        Ok(object.get("result").cloned().unwrap_or(Value::Null))
    };
    let malformed = matches!(&result, Err(error) if error.kind == ErrorKind::Protocol && !object.contains_key("error"));
    if let Some(sender) = inner
        .pending
        .lock()
        .map_err(|_| fatal("Verrou ACP indisponible.".into()))?
        .remove(&id)
    {
        let _ = sender.send(result);
    }
    // A late response after a timeout is harmless and must not become an event
    // or be mistaken for the result of a later mutation.
    if malformed {
        Err(FrameError::Invalid)
    } else {
        Ok(())
    }
}

/// Our request IDs are integers; some agents echo them back as strings.
fn response_id(id: Option<&Value>) -> Option<u64> {
    match id? {
        Value::Number(number) => number.as_u64(),
        Value::String(text) if text.len() <= 20 => text.parse().ok(),
        _ => None,
    }
}

/// Classify an ACP error object. Only codes, documented enums and numeric
/// statuses decide; provider text never reaches the message (it can contain a
/// credential or a copied request).
pub fn classify_rpc_error(error: &Value) -> AcpError {
    let code = error.get("code").and_then(Value::as_i64).unwrap_or(-32603);
    let lower = error
        .get("message")
        .and_then(Value::as_str)
        .unwrap_or_default()
        .to_lowercase();
    let data = error.get("data").unwrap_or(&Value::Null);
    let info = data.get("codexErrorInfo").unwrap_or(&Value::Null);
    // Codex: a string enum, or {"variant": {"httpStatusCode": n, ...}}.
    let info_name = info
        .as_str()
        .or_else(|| {
            info.as_object()
                .and_then(|o| o.keys().next().map(String::as_str))
        })
        .unwrap_or_default();
    let info_details = info
        .as_object()
        .and_then(|o| o.values().next())
        .unwrap_or(&Value::Null);
    // Claude: data.errorKind from SDKAssistantMessageError.
    let kind = data
        .get("errorKind")
        .and_then(Value::as_str)
        .unwrap_or_default();
    let http_status = data
        .get("httpStatusCode")
        .and_then(Value::as_u64)
        .or_else(|| info_details.get("httpStatusCode").and_then(Value::as_u64));
    let retry_after = [data, info_details].into_iter().find_map(|source| {
        source
            .get("retryAfterMs")
            .and_then(Value::as_u64)
            .map(Duration::from_millis)
            .or_else(|| {
                ["retryAfter", "retryAfterSeconds", "retry_after"]
                    .iter()
                    .find_map(|key| source.get(*key).and_then(Value::as_u64))
                    .map(Duration::from_secs)
            })
    });
    let auth_wording = [
        "authentication",
        "unauthorized",
        "api key",
        "credential",
        "not logged in",
    ]
    .iter()
    .any(|word| lower.contains(word));
    let (kind, category) = if http_status == Some(429)
        || info_name == "rateLimitExceeded"
        || kind == "rate_limit"
    {
        (ErrorKind::RateLimit, "L’adaptateur ACP signale une limitation temporaire des requêtes, pas un état vérifié de votre forfait")
    } else if matches!(kind, "overloaded" | "server_error")
        || info_name == "serverOverloaded"
        || matches!(http_status, Some(503 | 529))
    {
        (
            ErrorKind::Overloaded,
            "Le harness signale une indisponibilité temporaire du fournisseur",
        )
    } else if info_name == "usageLimitExceeded"
        || matches!(kind, "billing_error" | "account_on_hold")
    {
        (ErrorKind::UsageLimit, "Le harness signale une limite d’utilisation ; consultez son état de connexion et d’accès")
    } else if http_status == Some(401)
        || info_name == "unauthorized"
        || matches!(kind, "authentication_failed" | "oauth_org_not_allowed")
        // ACP reserves -32000 for auth_required; other agents reuse it, so
        // the documented wording is also required.
        || (code == -32000 && auth_wording)
    {
        (
            ErrorKind::Auth,
            "Authentification de l’agent nécessaire ou refusée",
        )
    } else if matches!(info_name, "contextWindowExceeded" | "sessionBudgetExceeded")
        || kind == "max_output_tokens"
    {
        (
            ErrorKind::Context,
            "La limite de cette session est atteinte ; ouvrez une nouvelle conversation",
        )
    } else if matches!(kind, "verification_required" | "cloud_credential_error") {
        (
            ErrorKind::Other,
            "Le harness demande une vérification des accès dans son environnement fournisseur",
        )
    } else if matches!(kind, "transport_lost" | "worker_shutdown")
        || matches!(
            info_name,
            "httpConnectionFailed"
                | "responseStreamConnectionFailed"
                | "responseStreamDisconnected"
                | "responseTooManyFailedAttempts"
        )
    {
        (
            ErrorKind::Transport,
            "Le harness a perdu sa connexion au fournisseur",
        )
    } else if lower.contains("cancel") {
        (ErrorKind::Cancelled, "Requête annulée")
    } else if lower.contains("not found") {
        (ErrorKind::Other, "Ressource introuvable")
    } else if lower.contains("not support") || code == -32601 {
        (ErrorKind::Protocol, "Opération non prise en charge")
    } else if matches!(code, -32700 | -32600 | -32602) {
        (
            ErrorKind::Protocol,
            "L’adaptateur ACP a refusé une requête invalide",
        )
    } else {
        (
            ErrorKind::Other,
            "L’adaptateur ACP a refusé la requête ; la cause fournisseur n’est pas précisée",
        )
    };
    AcpError {
        kind,
        retry_after,
        message: format!("{category} (JSON-RPC {code})."),
    }
}

/// `usage_update._meta["_claude/rateLimit"]` (SDKRateLimitInfo). Only a
/// rejected status not covered by an allowed overage blocks new requests.
fn claude_rate_limit(info: &Value) -> Option<AcpError> {
    if info.get("status").and_then(Value::as_str) != Some("rejected")
        || matches!(
            info.get("overageStatus").and_then(Value::as_str),
            Some("allowed" | "allowed_warning")
        )
    {
        return None;
    }
    let retry_after = info
        .get("resetsAt")
        .and_then(Value::as_f64)
        .map(|resets| resets - chrono::Utc::now().timestamp() as f64)
        .filter(|seconds| *seconds > 0.0)
        .map(|seconds| Duration::from_secs(seconds.ceil() as u64));
    let usage = info
        .get("rateLimitType")
        .and_then(Value::as_str)
        .is_some_and(|kind| kind != "overage" && kind.starts_with("seven_day"));
    Some(AcpError {
        kind: if usage {
            ErrorKind::UsageLimit
        } else {
            ErrorKind::RateLimit
        },
        retry_after,
        message: "Le harness signale que la limite de requêtes est atteinte.".into(),
    })
}

fn limit_event(error: &AcpError) -> Value {
    json!({"type":"rate_limited","kind":error.kind.as_str(),
        "retryAfterMs":error.retry_after.map(|d| d.as_millis() as u64)})
}

/// Wait for a session/prompt answer while its session shows activity.
async fn wait_for_prompt(
    inner: &Arc<Inner>,
    session: &str,
    receiver: oneshot::Receiver<TypedResponse>,
    ceiling: Duration,
) -> Result<Result<TypedResponse, oneshot::error::RecvError>, ()> {
    let started = Instant::now();
    let idle = match inner.prompt_idle_ms.load(Ordering::Relaxed) {
        0 => ceiling,
        millis => Duration::from_millis(millis),
    };
    tokio::pin!(receiver);
    loop {
        let last = inner
            .prompt_activity
            .lock()
            .ok()
            .and_then(|activity| activity.get(session).copied())
            .unwrap_or(started);
        let awaiting_human = inner
            .permissions
            .lock()
            .is_ok_and(|pending| pending.values().any(|p| p.session_id == session));
        let now = Instant::now();
        if now.duration_since(started) >= ceiling
            || (!awaiting_human && now.duration_since(last) >= idle)
        {
            return Err(());
        }
        let idle_deadline = if awaiting_human {
            // Re-check soon enough that a decision is followed by the
            // inactivity rule again, never later than one idle window.
            now + idle.min(Duration::from_secs(5))
        } else {
            last + idle
        };
        let wake = idle_deadline.min(started + ceiling);
        tokio::select! {
            response = &mut receiver => return Ok(response),
            _ = tokio::time::sleep_until(tokio::time::Instant::from_std(wake)) => {}
        }
    }
}

/// Record activity for a session whose session/prompt is outstanding.
/// Sessions without an outstanding prompt are left untracked.
fn touch_prompt_activity(inner: &Inner, session: &str) {
    if let Ok(mut activity) = inner.prompt_activity.lock() {
        if let Some(last) = activity.get_mut(session) {
            *last = Instant::now();
        }
    }
}

struct ActivityGuard {
    inner: Weak<Inner>,
    session: String,
}

impl Drop for ActivityGuard {
    fn drop(&mut self) {
        if let Some(inner) = self.inner.upgrade() {
            if let Ok(mut activity) = inner.prompt_activity.lock() {
                activity.remove(&self.session);
            }
        }
    }
}

fn push_tail(tail: &mut VecDeque<u8>, bytes: &[u8]) {
    let bytes = &bytes[bytes.len().saturating_sub(STDERR_TAIL_BYTES)..];
    let overflow = (tail.len() + bytes.len()).saturating_sub(STDERR_TAIL_BYTES);
    tail.drain(..overflow);
    tail.extend(bytes);
}

/// Remove anything resembling a credential from diagnostic text.
pub fn redact_secrets(text: &str) -> String {
    const SECRET_KEYS: [&str; 8] = [
        "token",
        "secret",
        "password",
        "passwd",
        "api_key",
        "apikey",
        "api-key",
        "authorization",
    ];
    let mut lines = Vec::new();
    for line in text.lines() {
        let lower = line.to_ascii_lowercase();
        if SECRET_KEYS.iter().any(|key| lower.contains(key)) || lower.contains("bearer") {
            // The whole line is suspect: keep that something was there.
            lines.push("[ligne expurgée]".to_string());
            continue;
        }
        let words: Vec<String> = line
            .split(' ')
            .map(|word| {
                let core = word.trim_matches(|c: char| "\"'`,;()[]{}<>".contains(c));
                let credential_like = core.starts_with("sk-")
                    || core.starts_with("ghp_")
                    || core.starts_with("xox")
                    || core.starts_with("eyJ")
                    || core.matches('.').count() == 2 && core.len() > 40
                    || (core.len() >= 32
                        && core
                            .chars()
                            .all(|c| c.is_ascii_alphanumeric() || "-_=+/".contains(c))
                        && core.chars().any(|c| c.is_ascii_digit()));
                if credential_like {
                    "[expurgé]".to_string()
                } else {
                    word.to_string()
                }
            })
            .collect();
        lines.push(words.join(" "));
    }
    lines.join("\n")
}

fn stderr_diagnostic(inner: &Inner) -> Option<String> {
    let tail = inner.stderr_tail.lock().ok()?;
    if tail.is_empty() {
        return None;
    }
    let bytes: Vec<u8> = tail.iter().copied().collect();
    let text = String::from_utf8_lossy(&bytes);
    let redacted = redact_secrets(&text);
    // Keep whole lines from the end within the excerpt budget.
    let mut excerpt = String::new();
    for line in redacted.lines().rev() {
        if excerpt.len() + line.len() + 1 > STDERR_DIAGNOSTIC_BYTES {
            break;
        }
        excerpt = if excerpt.is_empty() {
            line.to_string()
        } else {
            format!("{line}\n{excerpt}")
        };
    }
    (!excerpt.trim().is_empty()).then_some(excerpt)
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
    for permission in &permissions {
        touch_prompt_activity(inner, &permission.session_id);
    }
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
    fail_inner(inner, status, message, false);
}

/// Unexpected end of the transport: also attach a redacted stderr excerpt.
fn fail_with_diagnostic(inner: &Inner, status: &str, message: &str) {
    fail_inner(inner, status, message, true);
}

fn fail_inner(inner: &Inner, status: &str, message: &str, diagnostic: bool) {
    if inner.closed.swap(true, Ordering::AcqRel) {
        return;
    }
    if let Ok(mut pending) = inner.pending.lock() {
        for (_, sender) in pending.drain() {
            let _ = sender.send(Err(AcpError::new(ErrorKind::Transport, message)));
        }
    }
    if let Ok(mut pending) = inner.permissions.lock() {
        pending.clear();
    }
    let mut event = json!({"type":"connection", "status":status, "message":message});
    if diagnostic {
        if let Some(tail) = stderr_diagnostic(inner) {
            event["stderrTail"] = json!(tail);
        }
    }
    let _ = inner.events.send(event);
    if let Ok(mut shutdown) = inner.shutdown.lock() {
        if let Some(shutdown) = shutdown.take() {
            let _ = shutdown.send(());
        }
    }
}

#[cfg(unix)]
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
}

#[cfg(windows)]
mod windows_job {
    use std::os::windows::io::{AsRawHandle, FromRawHandle, OwnedHandle};
    use std::time::Duration;

    use tokio::process::Child;
    use windows_sys::Win32::Foundation::{ERROR_NO_MORE_FILES, INVALID_HANDLE_VALUE};
    use windows_sys::Win32::System::Diagnostics::ToolHelp::{
        CreateToolhelp32Snapshot, Thread32First, Thread32Next, TH32CS_SNAPTHREAD, THREADENTRY32,
    };
    use windows_sys::Win32::System::JobObjects::{
        AssignProcessToJobObject, CreateJobObjectW, JobObjectBasicAccountingInformation,
        JobObjectExtendedLimitInformation, QueryInformationJobObject, SetInformationJobObject,
        TerminateJobObject, JOBOBJECT_BASIC_ACCOUNTING_INFORMATION,
        JOBOBJECT_EXTENDED_LIMIT_INFORMATION, JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
    };
    use windows_sys::Win32::System::Threading::{OpenThread, ResumeThread, THREAD_SUSPEND_RESUME};

    pub(super) struct WindowsJob(OwnedHandle);

    fn error(context: &str) -> String {
        format!("{context} : {}", std::io::Error::last_os_error())
    }

    impl WindowsJob {
        pub(super) fn new() -> Result<Self, String> {
            // Null security attributes make this an unnamed, non-inheritable
            // handle. An agent must never inherit a handle that keeps its own
            // KILL_ON_JOB_CLOSE job alive after the client exits.
            let raw = unsafe { CreateJobObjectW(std::ptr::null(), std::ptr::null()) };
            if raw.is_null() {
                return Err(error("Impossible de créer le groupe Windows de l’agent"));
            }
            // SAFETY: CreateJobObjectW returned a fresh valid owned handle.
            let job = Self(unsafe { OwnedHandle::from_raw_handle(raw) });
            let mut limits = JOBOBJECT_EXTENDED_LIMIT_INFORMATION::default();
            limits.BasicLimitInformation.LimitFlags = JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE;
            // SAFETY: the handle and initialized structure remain live for the
            // call; the supplied byte size matches the information class.
            let configured = unsafe {
                SetInformationJobObject(
                    job.0.as_raw_handle(),
                    JobObjectExtendedLimitInformation,
                    std::ptr::from_ref(&limits).cast(),
                    std::mem::size_of_val(&limits) as u32,
                )
            };
            if configured == 0 {
                return Err(error("Impossible de protéger le groupe Windows de l’agent"));
            }
            Ok(job)
        }

        pub(super) fn assign_and_resume(&self, child: &Child, pid: u32) -> Result<(), String> {
            let process = child
                .raw_handle()
                .ok_or("Le processus Windows suspendu est indisponible.")?;
            // SAFETY: Tokio owns this live process handle. The primary thread
            // was created suspended, so no agent code can race the assignment.
            if unsafe { AssignProcessToJobObject(self.0.as_raw_handle(), process) } == 0 {
                return Err(error("Impossible d’associer l’agent à son groupe Windows"));
            }
            // Rust 1.93 does not expose the initial thread handle on stable.
            // Enumerating threads uses documented Win32 APIs and avoids
            // nightly attributes or undocumented NtResumeProcess calls.
            let snapshot = unsafe { CreateToolhelp32Snapshot(TH32CS_SNAPTHREAD, 0) };
            if snapshot == INVALID_HANDLE_VALUE || snapshot.is_null() {
                return Err(error("Impossible de trouver le thread initial de l’agent"));
            }
            // SAFETY: the snapshot is a fresh valid owned handle.
            let snapshot = unsafe { OwnedHandle::from_raw_handle(snapshot) };
            let mut entry = THREADENTRY32 {
                dwSize: std::mem::size_of::<THREADENTRY32>() as u32,
                ..Default::default()
            };
            let mut threads = Vec::new();
            // SAFETY: entry is initialized and has the required dwSize.
            if unsafe { Thread32First(snapshot.as_raw_handle(), &mut entry) } == 0 {
                return Err(error("Impossible de lire le thread initial de l’agent"));
            }
            loop {
                if entry.th32OwnerProcessID == pid {
                    threads.push(entry.th32ThreadID);
                }
                entry.dwSize = std::mem::size_of::<THREADENTRY32>() as u32;
                // SAFETY: snapshot and entry remain live during enumeration.
                if unsafe { Thread32Next(snapshot.as_raw_handle(), &mut entry) } == 0 {
                    if std::io::Error::last_os_error().raw_os_error()
                        != Some(ERROR_NO_MORE_FILES as i32)
                    {
                        return Err(error("L’énumération des threads de l’agent a échoué"));
                    }
                    break;
                }
            }
            if threads.len() != 1 {
                return Err(
                    "Le thread initial suspendu de l’agent est introuvable ou ambigu.".into(),
                );
            }
            // SAFETY: only the unique thread belonging to our suspended child
            // is opened, with the minimum right needed to resume it.
            let thread = unsafe { OpenThread(THREAD_SUSPEND_RESUME, 0, threads[0]) };
            if thread.is_null() {
                return Err(error("Impossible d’ouvrir le thread initial de l’agent"));
            }
            // SAFETY: OpenThread returned a fresh valid owned handle.
            let thread = unsafe { OwnedHandle::from_raw_handle(thread) };
            // SAFETY: the handle has THREAD_SUSPEND_RESUME access. Exactly one
            // suspension is expected from CREATE_SUSPENDED; any other result
            // fails closed and the caller terminates the newly assigned job.
            if unsafe { ResumeThread(thread.as_raw_handle()) } != 1 {
                return Err(
                    "Impossible de reprendre de façon sûre le thread initial de l’agent.".into(),
                );
            }
            Ok(())
        }

        pub(super) fn terminate(&self) -> Result<(), String> {
            // SAFETY: this handle belongs only to the private job created for
            // this agent; no whole-machine or WSL-distribution action occurs.
            if unsafe { TerminateJobObject(self.0.as_raw_handle(), 1) } == 0 {
                return Err(error("Impossible d’arrêter le groupe Windows de l’agent"));
            }
            Ok(())
        }

        pub(super) async fn finish(self) -> Result<(), String> {
            self.terminate()?;
            let deadline = tokio::time::Instant::now() + Duration::from_secs(3);
            loop {
                let mut accounting = JOBOBJECT_BASIC_ACCOUNTING_INFORMATION::default();
                // SAFETY: the output structure and byte size match this class.
                if unsafe {
                    QueryInformationJobObject(
                        self.0.as_raw_handle(),
                        JobObjectBasicAccountingInformation,
                        std::ptr::from_mut(&mut accounting).cast(),
                        std::mem::size_of_val(&accounting) as u32,
                        std::ptr::null_mut(),
                    )
                } == 0
                {
                    return Err(error("Impossible de vérifier l’arrêt du groupe Windows"));
                }
                if accounting.ActiveProcesses == 0 {
                    return Ok(());
                }
                if tokio::time::Instant::now() >= deadline {
                    return Err(
                        "Des processus de l’agent Windows sont encore actifs après l’arrêt.".into(),
                    );
                }
                tokio::time::sleep(Duration::from_millis(20)).await;
            }
            // The owned job handle closes on every return and on cancellation,
            // enforcing KILL_ON_JOB_CLOSE even if the supervisor is dropped.
        }
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
    async fn timing_records_correlate_concurrent_operations_without_request_content() {
        let (client, mut events) = client().await;
        let first = client.request_in_operation(
            "test/first",
            json!({"text":"private-prompt","Authorization":"Bearer private-token","sessionId":"private-session"}),
            "local-operation-one",
        );
        let second = async {
            tokio::time::sleep(Duration::from_millis(20)).await;
            client
                .request_in_operation("test/second", json!({}), "local-operation-two")
                .await
        };
        let (first, second) = tokio::join!(first, second);
        assert_eq!(first.unwrap()["text"], "private-prompt");
        second.unwrap();
        let records = client.timings();
        assert_eq!(records.len(), 4);
        assert!(!serde_json::to_string(&records)
            .unwrap()
            .contains("private-"));
        for operation in ["local-operation-one", "local-operation-two"] {
            let pair: Vec<_> = records
                .iter()
                .filter(|r| r["operationId"] == operation)
                .collect();
            assert_eq!(pair.len(), 2);
            assert_eq!(pair[0]["state"], "started");
            assert_eq!(pair[1]["state"], "completed");
            assert_eq!(pair[0]["requestId"], pair[1]["requestId"]);
            assert_eq!(pair[0]["connectionId"], pair[1]["connectionId"]);
            assert_eq!(pair[1]["outcome"], "succeeded");
            assert!(pair[1]["offsetMs"].as_u64().unwrap() >= pair[0]["offsetMs"].as_u64().unwrap());
        }
        for _ in 0..4 {
            assert_eq!(events.recv().await.unwrap()["type"], "rpc_timing");
        }
        client.stop().await.unwrap();
    }

    #[tokio::test]
    async fn timing_records_distinguish_timeout_failure_and_cancelled_future() {
        let (client, mut events) = client().await;
        let result = client
            .request_with_deadline(
                "test/ignore",
                json!({"secret":"private"}),
                Duration::from_millis(30),
            )
            .await;
        assert!(result.is_err());
        assert_eq!(client.timings().last().unwrap()["outcome"], "timed_out");
        // Unattributed calls are journaled without changing protocol events.
        assert!(events.try_recv().is_err());
        let worker = client.for_operation("abort-operation");
        let request = tokio::spawn(async move { worker.request("test/ignore", json!({})).await });
        assert_eq!(events.recv().await.unwrap()["state"], "started");
        request.abort();
        assert!(request.await.unwrap_err().is_cancelled());
        assert_eq!(events.recv().await.unwrap()["outcome"], "cancelled");
        assert!(client.inner.pending.lock().unwrap().is_empty());
        client.stop().await.unwrap();
        assert!(client
            .request("session/load", json!({"sessionId":"private-session"}))
            .await
            .is_err());
        assert_eq!(client.timings().last().unwrap()["outcome"], "failed");
        assert!(!serde_json::to_string(&client.timings())
            .unwrap()
            .contains("private"));
    }

    #[tokio::test]
    async fn cancellation_while_waiting_for_writer_prevents_request_and_preserves_connection() {
        let (client, mut events) = client().await;
        let writer = client.inner.stdin.lock().await;
        let cancelled = Arc::new(AtomicBool::new(false));
        let flag = cancelled.clone();
        let worker = client.clone();
        let pending = tokio::spawn(async move {
            worker
                .request_in_operation_cancellable(
                    "test/ignore",
                    json!({}),
                    "cancel-before-write",
                    &flag,
                )
                .await
        });
        assert_eq!(events.recv().await.unwrap()["state"], "started");
        cancelled.store(true, Ordering::Release);
        drop(writer);
        assert_eq!(
            pending.await.unwrap().unwrap_err(),
            REQUEST_CANCELLED_BEFORE_WRITE
        );
        assert_eq!(events.recv().await.unwrap()["outcome"], "cancelled");
        assert!(client.inner.pending.lock().unwrap().is_empty());
        assert!(!client.inner.closed.load(Ordering::Acquire));
        assert_eq!(
            client.request("test/count", json!({})).await.unwrap()["ignored"],
            0
        );
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
    async fn an_invalid_line_is_skipped_and_counted_without_closing_a_healthy_agent() {
        let (client, mut events) = client().await;
        // The invalid line is never a reply: the request can only end at its
        // deadline, whatever the machine load.
        let result = client
            .request_with_deadline("test/invalid", json!({}), Duration::from_millis(300))
            .await;
        assert!(result.unwrap_err().contains("délai"));
        // The agent writes the next reply after the invalid line and the
        // reader handles lines in order: once this reply arrives, the invalid
        // line has been processed. No timing assumption is needed.
        let count = timeout(
            Duration::from_secs(30),
            client.request("test/count", json!({})),
        )
        .await
        .expect("the healthy agent answers after an invalid line")
        .unwrap();
        assert_eq!(count["ignored"], 0);
        assert_eq!(client.invalid_frames(), 1);
        assert!(!client.inner.closed.load(Ordering::Acquire));
        while let Ok(event) = events.try_recv() {
            assert_ne!(event["type"], "connection");
            assert!(!event.to_string().contains("private-payload"));
        }
        client.stop().await.unwrap();
    }

    #[tokio::test]
    async fn oversized_or_truncated_frames_are_rejected_without_logging_the_payload() {
        for method in ["test/oversized", "test/partial"] {
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
        assert_eq!(
            events.try_recv().unwrap(),
            json!({"type":"authentication_required"})
        );
        assert!(events.try_recv().is_err(), "stderr must not be forwarded");
        client.stop().await.unwrap();
    }

    #[test]
    fn errors_are_classified_from_codes_and_enums_not_provider_text() {
        let kind = |error: Value| classify_rpc_error(&error).kind;
        assert_eq!(
            kind(json!({"code":-32603,"message":"x","data":{"errorKind":"rate_limit"}})),
            ErrorKind::RateLimit
        );
        assert_eq!(
            kind(
                json!({"code":-32603,"message":"x","data":{"codexErrorInfo":{"httpConnectionFailed":{"httpStatusCode":429}}}})
            ),
            ErrorKind::RateLimit
        );
        assert_eq!(
            kind(
                json!({"code":-32603,"message":"x","data":{"codexErrorInfo":"rateLimitExceeded"}})
            ),
            ErrorKind::RateLimit
        );
        assert_eq!(
            kind(json!({"code":-32603,"message":"x","data":{"errorKind":"overloaded"}})),
            ErrorKind::Overloaded
        );
        assert_eq!(
            kind(
                json!({"code":-32603,"message":"x","data":{"codexErrorInfo":"usageLimitExceeded"}})
            ),
            ErrorKind::UsageLimit
        );
        assert_eq!(
            kind(
                json!({"code":-32603,"message":"x","data":{"codexErrorInfo":"contextWindowExceeded"}})
            ),
            ErrorKind::Context
        );
        assert_eq!(
            kind(json!({"code":-32000,"message":"Authentication required"})),
            ErrorKind::Auth
        );
        assert_eq!(
            kind(json!({"code":-32603,"message":"x","data":{"httpStatusCode":401}})),
            ErrorKind::Auth
        );
        // -32000 alone, or provider prose mentioning "author", is not auth.
        assert_eq!(
            kind(json!({"code":-32000,"message":"permission failed"})),
            ErrorKind::Other
        );
        assert_eq!(
            kind(json!({"code":-32603,"message":"Unknown author of this OAuth API key"})),
            ErrorKind::Other
        );
        assert_eq!(
            kind(json!({"code":-32601,"message":"nope"})),
            ErrorKind::Protocol
        );
        assert_eq!(
            kind(json!({"message":"request cancelled"})),
            ErrorKind::Cancelled
        );
        assert_eq!(kind(json!({})), ErrorKind::Other);
        let error = classify_rpc_error(&json!({"code":-32603,"message":"sk-secret",
            "data":{"errorKind":"rate_limit","retryAfterMs":1500}}));
        assert_eq!(error.retry_after, Some(Duration::from_millis(1500)));
        assert!(!error.message.contains("sk-secret"));
        let error = classify_rpc_error(&json!({"code":-32603,"message":"x",
            "data":{"codexErrorInfo":{"responseTooManyFailedAttempts":{"retryAfter":30}}}}));
        assert_eq!(error.kind, ErrorKind::Transport);
        assert_eq!(error.retry_after, Some(Duration::from_secs(30)));
    }

    #[test]
    fn claude_rate_limit_metadata_blocks_only_rejected_requests() {
        let resets = chrono::Utc::now().timestamp() + 600;
        let limit = claude_rate_limit(
            &json!({"status":"rejected","resetsAt":resets,"rateLimitType":"five_hour"}),
        )
        .unwrap();
        assert_eq!(limit.kind, ErrorKind::RateLimit);
        let seconds = limit.retry_after.unwrap().as_secs();
        assert!((590..=601).contains(&seconds), "{seconds}");
        assert_eq!(
            claude_rate_limit(&json!({"status":"rejected","rateLimitType":"seven_day"}))
                .unwrap()
                .kind,
            ErrorKind::UsageLimit
        );
        assert!(claude_rate_limit(&json!({"status":"allowed_warning"})).is_none());
        assert!(
            claude_rate_limit(&json!({"status":"rejected","overageStatus":"allowed"})).is_none()
        );
    }

    #[test]
    fn response_ids_accept_integer_strings_only() {
        assert_eq!(response_id(Some(&json!(7))), Some(7));
        assert_eq!(response_id(Some(&json!("7"))), Some(7));
        assert_eq!(response_id(Some(&json!("abc"))), None);
        assert_eq!(response_id(Some(&json!(-1))), None);
        assert_eq!(response_id(None), None);
    }

    #[test]
    fn stderr_tail_is_bounded_and_redacted() {
        let mut tail = VecDeque::new();
        push_tail(&mut tail, &vec![b'a'; STDERR_TAIL_BYTES + 10]);
        push_tail(&mut tail, b"end");
        assert_eq!(tail.len(), STDERR_TAIL_BYTES);
        assert_eq!(
            tail.iter().rev().take(3).copied().collect::<Vec<_>>(),
            b"dne"
        );
        let redacted = redact_secrets(
            "starting adapter\nAuthorization: Bearer abc\nkey sk-ant-private value\ntoken=xyz\njwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJl ok\nid 0123456789abcdef0123456789abcdef0123",
        );
        assert!(redacted.contains("starting adapter"));
        for secret in ["Bearer", "sk-ant", "xyz", "eyJhbGci", "0123456789abcdef"] {
            assert!(!redacted.contains(secret), "{secret}: {redacted}");
        }
        assert!(redacted.contains("ok"));
    }

    #[tokio::test]
    async fn an_unexpected_exit_reports_only_a_redacted_stderr_tail() {
        let (client, mut events) = client().await;
        let _ = client.request("test/secret_error", json!({})).await;
        tokio::time::sleep(Duration::from_millis(100)).await;
        assert!(client.request("test/crash", json!({})).await.is_err());
        let event = timeout(Duration::from_secs(3), async {
            loop {
                let event = events.recv().await.unwrap();
                if event["type"] == "connection" {
                    return event;
                }
            }
        })
        .await
        .unwrap();
        assert_eq!(event["stderrTail"], "[ligne expurgée]");
        assert!(!event.to_string().contains("sk-private"));
        client.stop().await.unwrap();
    }

    #[tokio::test]
    async fn a_silent_prompt_times_out_but_a_pending_human_permission_does_not() {
        let (client, mut events) = client().await;
        client.set_prompt_idle_timeout(Some(Duration::from_millis(300)));
        let worker = client.clone();
        let prompt = tokio::spawn(async move {
            worker
                .request(
                    "session/prompt",
                    json!({"sessionId":"lesson-1","permission":true}),
                )
                .await
        });
        let permission = timeout(Duration::from_secs(3), async {
            loop {
                let event = events.recv().await.unwrap();
                if event["method"] == "session/request_permission" {
                    return event;
                }
            }
        })
        .await
        .unwrap();
        // Waiting for the learner is not inactivity of the harness.
        tokio::time::sleep(Duration::from_millis(900)).await;
        assert!(!prompt.is_finished());
        client
            .reply(
                permission["id"].clone(),
                json!({"outcome":{"outcome":"selected","optionId":"allow-once"}}),
            )
            .await
            .unwrap();
        assert_eq!(prompt.await.unwrap().unwrap()["decision"], "allow-once");
        // The fixture never answers this one and sends no update.
        let silent = client
            .request(
                "session/prompt",
                json!({"sessionId":"silent","permission":true}),
            )
            .await;
        // The fixture's update and permission name another session: this
        // prompt sees no activity of its own and ends on inactivity.
        assert!(silent.unwrap_err().contains("délai"));
        client.stop().await.unwrap();
    }

    /// Agent whose tool stays silent for a while after the permission answer.
    fn silent_tool_spec(silence_ms: u64) -> LaunchSpec {
        let script = format!(
            r#"import json, sys, time
prompt = None
for line in sys.stdin:
    request = json.loads(line)
    method = request.get("method")
    if method == "session/prompt":
        prompt = request
        print(json.dumps({{"jsonrpc": "2.0", "id": "perm", "method": "session/request_permission",
            "params": {{"sessionId": "lesson-1", "toolCall": {{"toolCallId": "tool-1"}},
            "options": [{{"optionId": "allow-once", "name": "Autoriser", "kind": "allow_once"}}]}}}}), flush=True)
    elif method is None and request.get("id") == "perm":
        time.sleep({silence})
        print(json.dumps({{"jsonrpc": "2.0", "id": prompt["id"],
            "result": {{"stopReason": "end_turn"}}}}), flush=True)
"#,
            silence = silence_ms as f64 / 1000.0
        );
        LaunchSpec {
            program: if cfg!(windows) { "python" } else { "python3" }.into(),
            args: vec!["-u".into(), "-c".into(), script],
            env: Vec::new(),
            cwd: None,
        }
    }

    #[tokio::test]
    async fn a_late_permission_decision_restarts_the_inactivity_window() {
        let (events, mut receiver) = mpsc::unbounded_channel();
        // The silent tool (250 ms) stays far below the inactivity limit, so a
        // loaded machine cannot turn the success path into a timeout.
        let client = AcpClient::spawn(silent_tool_spec(250), events)
            .await
            .unwrap();
        client.set_prompt_idle_timeout(Some(Duration::from_millis(1500)));
        let worker = client.clone();
        let prompt = tokio::spawn(async move {
            worker
                .request("session/prompt", json!({"sessionId":"lesson-1"}))
                .await
        });
        let permission = timeout(Duration::from_secs(3), async {
            loop {
                let event = receiver.recv().await.unwrap();
                if event["method"] == "session/request_permission" {
                    return event;
                }
            }
        })
        .await
        .unwrap();
        // The learner decides well after the inactivity limit.
        tokio::time::sleep(Duration::from_millis(2500)).await;
        assert!(!prompt.is_finished());
        client
            .reply(
                permission["id"].clone(),
                json!({"outcome":{"outcome":"selected","optionId":"allow-once"}}),
            )
            .await
            .unwrap();
        // The tool then works silently for less than the inactivity limit.
        assert_eq!(prompt.await.unwrap().unwrap()["stopReason"], "end_turn");
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

    #[cfg(windows)]
    struct WindowsDescendant(std::os::windows::io::OwnedHandle);

    #[cfg(windows)]
    impl WindowsDescendant {
        fn open(pid: u32) -> Self {
            use std::os::windows::io::FromRawHandle;
            use windows_sys::Win32::System::Threading::{
                OpenProcess, PROCESS_SYNCHRONIZE, PROCESS_TERMINATE,
            };
            // SAFETY: the PID comes from the private fake agent's own child.
            let raw = unsafe { OpenProcess(PROCESS_SYNCHRONIZE | PROCESS_TERMINATE, 0, pid) };
            assert!(
                !raw.is_null(),
                "must observe the actual live descendant: {}",
                std::io::Error::last_os_error()
            );
            // SAFETY: OpenProcess returned a fresh owned handle. Holding the
            // handle also prevents PID reuse from making this test pass.
            let child = Self(unsafe { std::os::windows::io::OwnedHandle::from_raw_handle(raw) });
            use std::os::windows::io::AsRawHandle;
            assert_eq!(
                unsafe {
                    windows_sys::Win32::System::Threading::WaitForSingleObject(
                        child.0.as_raw_handle(),
                        0,
                    )
                },
                windows_sys::Win32::Foundation::WAIT_TIMEOUT
            );
            child
        }

        async fn assert_stopped(&self) {
            use std::os::windows::io::AsRawHandle;
            use windows_sys::Win32::Foundation::{WAIT_OBJECT_0, WAIT_TIMEOUT};
            use windows_sys::Win32::System::Threading::WaitForSingleObject;
            timeout(Duration::from_secs(5), async {
                loop {
                    // SAFETY: the handle is owned by this observer; zero is a
                    // nonblocking poll, keeping the Tokio supervisor runnable.
                    match unsafe { WaitForSingleObject(self.0.as_raw_handle(), 0) } {
                        WAIT_OBJECT_0 => return,
                        WAIT_TIMEOUT => tokio::time::sleep(Duration::from_millis(20)).await,
                        other => panic!("cannot inspect descendant process: {other}"),
                    }
                }
            })
            .await
            .expect("a real Windows descendant survived its agent");
        }
    }

    #[cfg(windows)]
    impl Drop for WindowsDescendant {
        fn drop(&mut self) {
            use std::os::windows::io::AsRawHandle;
            use windows_sys::Win32::System::Threading::{TerminateProcess, WaitForSingleObject};
            // Clean up this test-created process even if an assertion fails.
            // This guard never targets a PID supplied by a real agent/user.
            if unsafe { WaitForSingleObject(self.0.as_raw_handle(), 0) }
                == windows_sys::Win32::Foundation::WAIT_TIMEOUT
            {
                unsafe {
                    TerminateProcess(self.0.as_raw_handle(), 1);
                }
            }
        }
    }

    #[cfg(windows)]
    async fn windows_child(client: &AcpClient) -> WindowsDescendant {
        let response = client.request("test/child", json!({})).await.unwrap();
        WindowsDescendant::open(response["pid"].as_u64().unwrap() as u32)
    }

    #[cfg(windows)]
    async fn connection_closed(events: &mut mpsc::UnboundedReceiver<Value>) {
        timeout(Duration::from_secs(5), async {
            while let Some(event) = events.recv().await {
                if event["type"] == "connection" && event["status"] == "closed" {
                    return;
                }
            }
            panic!("missing connection-closed event");
        })
        .await
        .unwrap();
    }

    #[cfg(windows)]
    #[tokio::test]
    async fn windows_stop_terminates_descendants_after_graceful_stdin_eof() {
        let (client, _events) = client().await;
        let descendant = windows_child(&client).await;
        client.stop().await.unwrap();
        descendant.assert_stopped().await;
    }

    #[cfg(windows)]
    #[tokio::test]
    async fn windows_graceful_agent_exit_terminates_descendants_without_manual_stop() {
        let (client, mut events) = client().await;
        let descendant = windows_child(&client).await;
        assert_eq!(
            client
                .request("test/final_response", json!({}))
                .await
                .unwrap()["saved"],
            true
        );
        connection_closed(&mut events).await;
        descendant.assert_stopped().await;
        client.stop().await.unwrap();
    }

    #[cfg(windows)]
    #[tokio::test]
    async fn windows_agent_crash_terminates_descendants_without_manual_stop() {
        let (client, mut events) = client().await;
        let descendant = windows_child(&client).await;
        assert!(client.request("test/crash", json!({})).await.is_err());
        connection_closed(&mut events).await;
        descendant.assert_stopped().await;
        client.stop().await.unwrap();
    }

    #[cfg(windows)]
    #[tokio::test]
    async fn windows_last_handle_drop_terminates_agent_and_descendants() {
        let (client, _events) = client().await;
        let descendant = windows_child(&client).await;
        drop(client);
        descendant.assert_stopped().await;
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
                env: vec![
                    ("CODEX_HOME".into(), home.to_string_lossy().into_owned()),
                    (
                        "CLAUDE_CONFIG_DIR".into(),
                        home.to_string_lossy().into_owned(),
                    ),
                ],
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
