// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! Supervised, bounded MCP over a trusted executable's standard streams.
//! Launch configuration must come from the ACP host, never generated text.
//! This transport performs no authorization, argument-schema validation, retry,
//! shell interpretation, or model context construction.

use std::collections::BTreeSet;
use std::fmt;
use std::io::{self, Write};
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::Arc;
use std::time::Duration;

use serde_json::{json, Value};
use tokio::io::{AsyncBufRead, AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader};
use tokio::process::{ChildStdin, Command};
use tokio::sync::{mpsc, watch, Notify};
use tokio::task::JoinHandle;

use crate::catalog::{ToolCatalog, ToolEntry, MAX_TOOLS};

const PROTOCOL: &str = "2025-06-18";
const MAX_FRAME: usize = 8 * 1024 * 1024;
const MAX_PAGES: usize = 64;
const REQUEST_TIMEOUT: Duration = Duration::from_secs(30);
const CLOSE_TIMEOUT: Duration = Duration::from_secs(5);

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum StdioError {
    InvalidConfig,
    UnsupportedTransport,
    Spawn,
    Closed,
    Cancelled,
    Timeout,
    Io,
    Protocol,
    RemoteError,
    SizeLimit,
    InvalidCatalog,
    CatalogChanged,
    UnsupportedTools,
    Cleanup,
}

impl fmt::Display for StdioError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            Self::InvalidConfig => "Invalid trusted stdio configuration.",
            Self::UnsupportedTransport => "This transport supports only native stdio MCP.",
            Self::Spawn => "Cannot start the local MCP process.",
            Self::Closed => "MCP stdio connection is closed; reconnect explicitly.",
            Self::Cancelled => "MCP request cancelled; its mutation outcome may be unknown.",
            Self::Timeout => "MCP request timed out; its mutation outcome may be unknown.",
            Self::Io => "MCP stdio failed; its mutation outcome may be unknown.",
            Self::Protocol => "Invalid MCP protocol response.",
            Self::RemoteError => "MCP endpoint returned an RPC error.",
            Self::SizeLimit => "MCP frame exceeds the transport limit.",
            Self::InvalidCatalog => "Invalid or oversized MCP tool catalog.",
            Self::CatalogChanged => "MCP tools changed during listing; discard the snapshot.",
            Self::UnsupportedTools => "MCP server does not advertise tools.",
            Self::Cleanup => "Cannot confirm that the local MCP processes stopped.",
        })
    }
}
impl std::error::Error for StdioError {}

/// Confidential host configuration, deliberately not Debug or Serialize.
pub struct StdioConfig {
    name: String,
    command: String,
    args: Vec<String>,
    env: Vec<(String, String)>,
    cwd: PathBuf,
}

impl StdioConfig {
    pub fn from_acp(value: &Value, cwd: &Path) -> Result<Self, StdioError> {
        if value
            .get("type")
            .is_some_and(|v| v.as_str() != Some("stdio"))
        {
            return Err(StdioError::UnsupportedTransport);
        }
        let name = value
            .get("name")
            .and_then(Value::as_str)
            .filter(|s| valid_name(s))
            .ok_or(StdioError::InvalidConfig)?;
        let command = value
            .get("command")
            .and_then(Value::as_str)
            .filter(|s| !s.trim().is_empty() && s.len() <= 4096 && !s.contains('\0'))
            .ok_or(StdioError::InvalidConfig)?;
        let path = Path::new(command);
        if !path.is_absolute()
            && (path.components().count() != 1 || command.chars().any(char::is_whitespace))
        {
            return Err(StdioError::InvalidConfig);
        }
        let file = path
            .file_name()
            .and_then(|n| n.to_str())
            .unwrap_or("")
            .to_ascii_lowercase();
        let stem = file.strip_suffix(".exe").unwrap_or(&file);
        // In particular, Rust may implicitly use cmd.exe for .bat/.cmd files.
        // A configured native executable is required; no shell launch fallback.
        if file.ends_with(".bat")
            || file.ends_with(".cmd")
            || matches!(
                stem,
                "sh" | "bash" | "dash" | "zsh" | "fish" | "cmd" | "powershell" | "pwsh"
            )
            || !cwd.is_absolute()
            || !cwd.is_dir()
            || cwd.as_os_str().len() > 4096
        {
            return Err(StdioError::InvalidConfig);
        }
        let args = value
            .get("args")
            .and_then(Value::as_array)
            .ok_or(StdioError::InvalidConfig)?;
        let env = value
            .get("env")
            .and_then(Value::as_array)
            .ok_or(StdioError::InvalidConfig)?;
        if args.len() > 128 || env.len() > 64 {
            return Err(StdioError::InvalidConfig);
        }
        let mut total = 0;
        let mut validated_args = Vec::new();
        for arg in args {
            let arg = arg
                .as_str()
                .filter(|s| s.len() <= 32768 && !s.contains('\0'))
                .ok_or(StdioError::InvalidConfig)?;
            total += arg.len();
            if total > 96 * 1024 {
                return Err(StdioError::InvalidConfig);
            }
            validated_args.push(arg.to_owned());
        }
        let mut keys = BTreeSet::new();
        let mut validated_env = Vec::new();
        for item in env {
            let key = item
                .get("name")
                .and_then(Value::as_str)
                .filter(|s| {
                    !s.is_empty()
                        && s.len() <= 128
                        && s.bytes().all(|b| b.is_ascii_alphanumeric() || b == b'_')
                })
                .ok_or(StdioError::InvalidConfig)?;
            let value = item
                .get("value")
                .and_then(Value::as_str)
                .filter(|s| s.len() <= 64 * 1024 && !s.contains('\0'))
                .ok_or(StdioError::InvalidConfig)?;
            total += key.len() + value.len();
            if total > 96 * 1024 || !keys.insert(key.to_ascii_lowercase()) {
                return Err(StdioError::InvalidConfig);
            }
            validated_env.push((key.into(), value.into()));
        }
        Ok(Self {
            name: name.into(),
            command: command.into(),
            args: validated_args,
            env: validated_env,
            cwd: cwd.into(),
        })
    }
}

struct State {
    closing: AtomicBool,
    stop: Notify,
    pending: AtomicU64,
    tools_changed: AtomicBool,
    revision: AtomicU64,
}
impl State {
    fn shutdown(&self) {
        self.closing.store(true, Ordering::Release);
        self.stop.notify_one();
    }
}

struct RequestGuard {
    state: Arc<State>,
    armed: bool,
}
impl Drop for RequestGuard {
    fn drop(&mut self) {
        if self.armed {
            self.state.shutdown();
        }
    }
}
struct AbortTask<T>(JoinHandle<T>);
impl<T> Drop for AbortTask<T> {
    fn drop(&mut self) {
        self.0.abort();
    }
}

/// A single in-flight request. The supervisor outlives dropped request futures
/// and owns cleanup; neither an Engine nor a model controls this process.
pub struct StdioClient {
    #[cfg(test)]
    pid: u32,
    name: String,
    stdin: Option<ChildStdin>,
    responses: mpsc::Receiver<Result<Value, StdioError>>,
    state: Arc<State>,
    stopped: watch::Receiver<Option<bool>>,
    next_id: u64,
    ready: bool,
    supports_tools: bool,
}
impl Drop for StdioClient {
    fn drop(&mut self) {
        self.stdin.take();
        self.state.shutdown();
    }
}

impl StdioClient {
    pub async fn connect(config: StdioConfig, cancel: &AtomicBool) -> Result<Self, StdioError> {
        if cancel.load(Ordering::Acquire) {
            return Err(StdioError::Cancelled);
        }
        let mut command = Command::new(&config.command);
        command
            .args(&config.args)
            .envs(config.env)
            .current_dir(config.cwd)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .kill_on_drop(true);
        #[cfg(unix)]
        {
            use std::os::unix::process::CommandExt;
            command.as_std_mut().process_group(0);
        }
        #[cfg(windows)]
        command
            .creation_flags(0x0800_0000 | windows_sys::Win32::System::Threading::CREATE_SUSPENDED);
        #[cfg(windows)]
        let job = windows_job::WindowsJob::new()?;
        let mut child = command.spawn().map_err(|_| StdioError::Spawn)?;
        let pid = child.id().ok_or(StdioError::Spawn)?;
        #[cfg(unix)]
        let mut group = UnixGroup(Some(pid));
        #[cfg(windows)]
        if let Err(error) = job.assign_and_resume(&child, pid) {
            drop(job);
            let _ = child.start_kill();
            let _ = tokio::time::timeout(Duration::from_secs(3), child.wait()).await;
            return Err(error);
        }
        let stdin = child.stdin.take().ok_or(StdioError::Spawn)?;
        let stdout = child.stdout.take().ok_or(StdioError::Spawn)?;
        let stderr = child.stderr.take().ok_or(StdioError::Spawn)?;
        let state = Arc::new(State {
            closing: AtomicBool::new(false),
            stop: Notify::new(),
            pending: AtomicU64::new(0),
            tools_changed: AtomicBool::new(true),
            revision: AtomicU64::new(0),
        });
        let (tx, responses) = mpsc::channel(2);
        let reader_state = state.clone();
        let reader = AbortTask(tokio::spawn(async move {
            let mut stdout = BufReader::new(stdout);
            let mut notifications = 0;
            loop {
                let result = match read_frame(&mut stdout).await {
                    Ok(Some(frame)) => {
                        serde_json::from_slice::<Value>(&frame).map_err(|_| StdioError::Protocol)
                    }
                    Ok(None) => Err(StdioError::Closed),
                    Err(error) => Err(error),
                };
                let message = match result {
                    Ok(message) => message,
                    Err(error) => {
                        let _ = tx.send(Err(error)).await;
                        break;
                    }
                };
                if message.get("jsonrpc").and_then(Value::as_str) != Some("2.0") {
                    let _ = tx.send(Err(StdioError::Protocol)).await;
                    break;
                }
                if let Some(method) = message.get("method") {
                    notifications += 1;
                    if !method.is_string()
                        || message.get("id").is_some()
                        || message.get("result").is_some()
                        || message.get("error").is_some()
                        || notifications > 256
                    {
                        let _ = tx.send(Err(StdioError::Protocol)).await;
                        break;
                    }
                    if method.as_str() == Some("notifications/tools/list_changed") {
                        reader_state.revision.fetch_add(1, Ordering::AcqRel);
                        reader_state.tools_changed.store(true, Ordering::Release);
                    }
                    continue;
                }
                notifications = 0;
                let expected = reader_state.pending.load(Ordering::Acquire);
                if expected == 0
                    || message.get("id").and_then(Value::as_u64) != Some(expected)
                    || message.get("result").is_some() == message.get("error").is_some()
                {
                    let _ = tx.send(Err(StdioError::Protocol)).await;
                    break;
                }
                if tx.send(Ok(message)).await.is_err() {
                    break;
                }
            }
            reader_state.shutdown();
        }));
        let stderr = AbortTask(tokio::spawn(async move {
            // Never accumulate lines or forward diagnostics: native stderr can
            // include credentials and full user payloads. Constant memory drain.
            let mut stderr = stderr;
            let mut buffer = [0; 4096];
            loop {
                match stderr.read(&mut buffer).await {
                    Ok(0) | Err(_) => break,
                    Ok(_) => {}
                }
            }
        }));
        let (stopped_tx, stopped) = watch::channel(None);
        let supervisor = state.clone();
        tokio::spawn(async move {
            let mut reader = reader;
            let _stderr = stderr;
            tokio::select! {
                _ = child.wait() => {
                    // Preserve a last buffered valid reply when the child exits.
                    let _ = tokio::time::timeout(Duration::from_millis(500), &mut reader.0).await;
                }
                _ = supervisor.stop.notified() => {}
            }
            supervisor.closing.store(true, Ordering::Release);
            #[cfg(unix)]
            let tree_ok = group.terminate().is_ok();
            #[cfg(windows)]
            let tree_ok = job.terminate().is_ok();
            let _ = child.start_kill();
            let reaped = matches!(
                tokio::time::timeout(Duration::from_secs(3), child.wait()).await,
                Ok(Ok(_))
            );
            #[cfg(windows)]
            let tree_ok = tree_ok && job.finish().await.is_ok();
            reader.0.abort();
            supervisor.pending.store(0, Ordering::Release);
            let _ = stopped_tx.send(Some(reaped && tree_ok));
        });
        let mut client = Self {
            #[cfg(test)]
            pid,
            name: config.name,
            stdin: Some(stdin),
            responses,
            state,
            stopped,
            next_id: 1,
            ready: true,
            supports_tools: false,
        };
        let result = client.initialize(cancel).await;
        if let Err(error) = result {
            let _ = client.close().await;
            return Err(error);
        }
        Ok(client)
    }

    async fn initialize(&mut self, cancel: &AtomicBool) -> Result<(), StdioError> {
        let result = self
            .request(
                "initialize",
                json!({"protocolVersion":PROTOCOL,"capabilities":{},
            "clientInfo":{"name":"tutor-local-agent","version":env!("CARGO_PKG_VERSION")}}),
                cancel,
            )
            .await?;
        if result.get("protocolVersion").and_then(Value::as_str) != Some(PROTOCOL)
            || !result.get("capabilities").is_some_and(Value::is_object)
        {
            return Err(StdioError::Protocol);
        }
        self.supports_tools = result
            .pointer("/capabilities/tools")
            .is_some_and(Value::is_object);
        let body =
            encode(&json!({"jsonrpc":"2.0","method":"notifications/initialized","params":{}}))?;
        self.ready = false;
        let mut guard = RequestGuard {
            state: self.state.clone(),
            armed: true,
        };
        bounded(
            write_frame(self.stdin.as_mut().ok_or(StdioError::Closed)?, body),
            cancel,
        )
        .await?;
        guard.armed = false;
        self.ready = !self.state.closing.load(Ordering::Acquire);
        Ok(())
    }

    pub fn server_id(&self) -> &str {
        &self.name
    }
    pub fn take_tools_changed(&self) -> bool {
        self.state.tools_changed.swap(false, Ordering::AcqRel)
    }

    pub async fn close(&mut self) -> Result<(), StdioError> {
        self.ready = false;
        self.stdin.take();
        self.state.shutdown();
        tokio::time::timeout(CLOSE_TIMEOUT, async {
            loop {
                if let Some(ok) = *self.stopped.borrow() {
                    return if ok { Ok(()) } else { Err(StdioError::Cleanup) };
                }
                self.stopped
                    .changed()
                    .await
                    .map_err(|_| StdioError::Cleanup)?;
            }
        })
        .await
        .map_err(|_| StdioError::Cleanup)?
    }

    pub async fn list_tools(&mut self, cancel: &AtomicBool) -> Result<Vec<ToolEntry>, StdioError> {
        if !self.supports_tools {
            return Err(StdioError::UnsupportedTools);
        }
        let revision = self.state.revision.load(Ordering::Acquire);
        let mut check =
            ToolCatalog::new("stdio-validation".into()).map_err(|_| StdioError::InvalidCatalog)?;
        let mut entries = Vec::new();
        let mut names = BTreeSet::new();
        let mut cursor = None;
        let mut cursors = BTreeSet::new();
        for _ in 0..MAX_PAGES {
            let params = cursor
                .as_ref()
                .map(|c| json!({"cursor":c}))
                .unwrap_or_else(|| json!({}));
            let page = self.request("tools/list", params, cancel).await?;
            let tools = page
                .get("tools")
                .and_then(Value::as_array)
                .ok_or(StdioError::InvalidCatalog)?;
            if entries.len() + tools.len() > MAX_TOOLS {
                return Err(StdioError::InvalidCatalog);
            }
            for tool in tools {
                let name = tool
                    .get("name")
                    .and_then(Value::as_str)
                    .filter(|s| valid_name(s))
                    .ok_or(StdioError::InvalidCatalog)?;
                if !names.insert(name.to_owned()) {
                    return Err(StdioError::InvalidCatalog);
                }
                let description = match tool.get("description") {
                    None => String::new(),
                    Some(Value::String(s)) => s.clone(),
                    _ => return Err(StdioError::InvalidCatalog),
                };
                let entry = ToolEntry {
                    server_id: self.name.clone(),
                    name: name.into(),
                    description,
                    input_schema: tool
                        .get("inputSchema")
                        .cloned()
                        .ok_or(StdioError::InvalidCatalog)?,
                    annotations: tool.get("annotations").cloned().unwrap_or(Value::Null),
                };
                check
                    .replace_server_tools("stdio-validation", &self.name, vec![entry.clone()])
                    .map_err(|_| StdioError::InvalidCatalog)?;
                entries.push(entry);
            }
            cursor = match page.get("nextCursor") {
                None | Some(Value::Null) => None,
                Some(Value::String(s)) if s.is_empty() => None,
                Some(Value::String(s)) if s.len() <= 4096 => Some(s.clone()),
                _ => return Err(StdioError::InvalidCatalog),
            };
            if let Some(cursor) = &cursor {
                if !cursors.insert(cursor.clone()) {
                    return Err(StdioError::InvalidCatalog);
                }
            } else {
                // Clear before checking the revision so an idle notification
                // racing the final check cannot be lost behind a false flag.
                self.state.tools_changed.store(false, Ordering::Release);
                if revision != self.state.revision.load(Ordering::Acquire) {
                    self.state.tools_changed.store(true, Ordering::Release);
                    return Err(StdioError::CatalogChanged);
                }
                return Ok(entries);
            }
        }
        Err(StdioError::InvalidCatalog)
    }

    pub async fn call_tool(
        &mut self,
        name: &str,
        arguments: Value,
        idempotency_key: Option<&str>,
        cancel: &AtomicBool,
    ) -> Result<Value, StdioError> {
        if !self.supports_tools {
            return Err(StdioError::UnsupportedTools);
        }
        if !valid_name(name) || !arguments.is_object() {
            return Err(StdioError::Protocol);
        }
        let mut params = json!({"name":name,"arguments":arguments});
        if let Some(key) = idempotency_key {
            if key.is_empty() || key.len() > 256 {
                return Err(StdioError::Protocol);
            }
            params["_meta"] = json!({"idempotency_key":key});
        }
        self.state.tools_changed.store(true, Ordering::Release);
        let result = self.request("tools/call", params, cancel).await?;
        if !result.get("content").is_some_and(Value::is_array)
            || result.get("isError").is_some_and(|v| !v.is_boolean())
            || result
                .get("structuredContent")
                .is_some_and(|v| !v.is_object())
        {
            let _ = self.close().await;
            return Err(StdioError::Protocol);
        }
        Ok(result)
    }

    async fn request(
        &mut self,
        method: &str,
        params: Value,
        cancel: &AtomicBool,
    ) -> Result<Value, StdioError> {
        if !self.ready || self.state.closing.load(Ordering::Acquire) {
            return Err(StdioError::Closed);
        }
        let id = self.next_id;
        self.next_id = self.next_id.checked_add(1).ok_or(StdioError::Closed)?;
        let body = encode(&json!({"jsonrpc":"2.0","id":id,"method":method,"params":params}))?;
        self.ready = false;
        self.state.pending.store(id, Ordering::Release);
        let mut guard = RequestGuard {
            state: self.state.clone(),
            armed: true,
        };
        let result = bounded(
            async {
                write_frame(self.stdin.as_mut().ok_or(StdioError::Closed)?, body).await?;
                let message = self.responses.recv().await.ok_or(StdioError::Closed)??;
                if message.get("id").and_then(Value::as_u64) != Some(id) {
                    return Err(StdioError::Protocol);
                }
                if message.get("error").is_some() {
                    return Err(StdioError::RemoteError);
                }
                message.get("result").cloned().ok_or(StdioError::Protocol)
            },
            cancel,
        )
        .await;
        match result {
            Ok(value) => {
                self.state.pending.store(0, Ordering::Release);
                guard.armed = false;
                self.ready = !self.state.closing.load(Ordering::Acquire);
                Ok(value)
            }
            Err(error) => {
                let _ = self.close().await;
                Err(error)
            }
        }
    }
}

fn valid_name(s: &str) -> bool {
    !s.is_empty()
        && s.len() <= 128
        && s.bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"_.-".contains(&b))
}

async fn bounded<T>(
    future: impl std::future::Future<Output = Result<T, StdioError>>,
    cancel: &AtomicBool,
) -> Result<T, StdioError> {
    tokio::select! {
        biased;
        _ = async { while !cancel.load(Ordering::Acquire) { tokio::time::sleep(Duration::from_millis(10)).await; } } => Err(StdioError::Cancelled),
        result = tokio::time::timeout(REQUEST_TIMEOUT, future) => result.map_err(|_| StdioError::Timeout)?,
    }
}

fn encode(value: &Value) -> Result<Vec<u8>, StdioError> {
    struct Output(Vec<u8>);
    impl Write for Output {
        fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
            if self.0.len().saturating_add(bytes.len()) >= MAX_FRAME {
                return Err(io::Error::other("frame limit"));
            }
            self.0.extend_from_slice(bytes);
            Ok(bytes.len())
        }
        fn flush(&mut self) -> io::Result<()> {
            Ok(())
        }
    }
    let mut output = Output(Vec::new());
    serde_json::to_writer(&mut output, value).map_err(|_| StdioError::SizeLimit)?;
    output.0.push(b'\n');
    Ok(output.0)
}

async fn write_frame(stdin: &mut ChildStdin, body: Vec<u8>) -> Result<(), StdioError> {
    stdin.write_all(&body).await.map_err(|_| StdioError::Io)?;
    stdin.flush().await.map_err(|_| StdioError::Io)
}

async fn read_frame<R: AsyncBufRead + Unpin>(
    reader: &mut R,
) -> Result<Option<Vec<u8>>, StdioError> {
    let mut frame = Vec::new();
    loop {
        let bytes = reader.fill_buf().await.map_err(|_| StdioError::Io)?;
        if bytes.is_empty() {
            return if frame.is_empty() {
                Ok(None)
            } else {
                Err(StdioError::Protocol)
            };
        }
        let newline = bytes.iter().position(|b| *b == b'\n');
        let length = newline.map_or(bytes.len(), |i| i + 1);
        if frame.len() + length > MAX_FRAME {
            return Err(StdioError::SizeLimit);
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

#[cfg(unix)]
struct UnixGroup(Option<u32>);
#[cfg(unix)]
impl UnixGroup {
    fn terminate(&mut self) -> Result<(), StdioError> {
        if let Some(pid) = self.0 {
            // SAFETY: pid is our child's fresh process group (process_group(0)),
            // never caller input or the host's own process group.
            if unsafe { libc::killpg(pid as libc::pid_t, libc::SIGKILL) } != 0
                && io::Error::last_os_error().raw_os_error() != Some(libc::ESRCH)
            {
                return Err(StdioError::Cleanup);
            }
            self.0 = None;
        }
        Ok(())
    }
}
#[cfg(unix)]
impl Drop for UnixGroup {
    fn drop(&mut self) {
        let _ = self.terminate();
    }
}

// Same documented suspended-process/job pattern as the reviewed desktop ACP
// transport, with static error codes to keep all native diagnostics private.
#[cfg(windows)]
mod windows_job {
    use super::StdioError;
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
    impl WindowsJob {
        pub(super) fn new() -> Result<Self, StdioError> {
            // SAFETY: null attributes create a non-inheritable unnamed job.
            let raw = unsafe { CreateJobObjectW(std::ptr::null(), std::ptr::null()) };
            if raw.is_null() {
                return Err(StdioError::Spawn);
            }
            // SAFETY: fresh owned handle returned by CreateJobObjectW.
            let job = Self(unsafe { OwnedHandle::from_raw_handle(raw) });
            let mut limits = JOBOBJECT_EXTENDED_LIMIT_INFORMATION::default();
            limits.BasicLimitInformation.LimitFlags = JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE;
            // SAFETY: initialized structure and byte size match this info class.
            if unsafe {
                SetInformationJobObject(
                    job.0.as_raw_handle(),
                    JobObjectExtendedLimitInformation,
                    std::ptr::from_ref(&limits).cast(),
                    std::mem::size_of_val(&limits) as u32,
                )
            } == 0
            {
                return Err(StdioError::Spawn);
            }
            Ok(job)
        }

        pub(super) fn assign_and_resume(&self, child: &Child, pid: u32) -> Result<(), StdioError> {
            let process = child.raw_handle().ok_or(StdioError::Spawn)?;
            // SAFETY: child was created suspended, so no child code can race
            // assignment or create an uncontained descendant before this call.
            if unsafe { AssignProcessToJobObject(self.0.as_raw_handle(), process) } == 0 {
                return Err(StdioError::Spawn);
            }
            // SAFETY: documented thread snapshot API, scoped to this child's PID.
            let raw = unsafe { CreateToolhelp32Snapshot(TH32CS_SNAPTHREAD, 0) };
            if raw == INVALID_HANDLE_VALUE || raw.is_null() {
                return Err(StdioError::Spawn);
            }
            // SAFETY: fresh valid snapshot handle owned by this scope.
            let snapshot = unsafe { OwnedHandle::from_raw_handle(raw) };
            let mut entry = THREADENTRY32 {
                dwSize: std::mem::size_of::<THREADENTRY32>() as u32,
                ..Default::default()
            };
            let mut threads = Vec::new();
            // SAFETY: dwSize and initialized structure match the Win32 contract.
            if unsafe { Thread32First(snapshot.as_raw_handle(), &mut entry) } == 0 {
                return Err(StdioError::Spawn);
            }
            loop {
                if entry.th32OwnerProcessID == pid {
                    threads.push(entry.th32ThreadID);
                }
                entry.dwSize = std::mem::size_of::<THREADENTRY32>() as u32;
                // SAFETY: snapshot and output entry remain live throughout.
                if unsafe { Thread32Next(snapshot.as_raw_handle(), &mut entry) } == 0 {
                    if std::io::Error::last_os_error().raw_os_error()
                        != Some(ERROR_NO_MORE_FILES as i32)
                    {
                        return Err(StdioError::Spawn);
                    }
                    break;
                }
            }
            if threads.len() != 1 {
                return Err(StdioError::Spawn);
            }
            // SAFETY: open only our unique suspended child thread with minimum access.
            let raw = unsafe { OpenThread(THREAD_SUSPEND_RESUME, 0, threads[0]) };
            if raw.is_null() {
                return Err(StdioError::Spawn);
            }
            // SAFETY: OpenThread returned a fresh owned handle.
            let thread = unsafe { OwnedHandle::from_raw_handle(raw) };
            // SAFETY: exactly one suspension is expected from CREATE_SUSPENDED.
            if unsafe { ResumeThread(thread.as_raw_handle()) } != 1 {
                return Err(StdioError::Spawn);
            }
            Ok(())
        }

        pub(super) fn terminate(&self) -> Result<(), StdioError> {
            // SAFETY: this job contains only the child tree created above.
            if unsafe { TerminateJobObject(self.0.as_raw_handle(), 1) } == 0 {
                return Err(StdioError::Cleanup);
            }
            Ok(())
        }

        pub(super) async fn finish(self) -> Result<(), StdioError> {
            self.terminate()?;
            let deadline = tokio::time::Instant::now() + Duration::from_secs(3);
            loop {
                let mut accounting = JOBOBJECT_BASIC_ACCOUNTING_INFORMATION::default();
                // SAFETY: output pointer and byte size match the requested class.
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
                    return Err(StdioError::Cleanup);
                }
                if accounting.ActiveProcesses == 0 {
                    return Ok(());
                }
                if tokio::time::Instant::now() >= deadline {
                    return Err(StdioError::Cleanup);
                }
                tokio::time::sleep(Duration::from_millis(20)).await;
            }
            // OwnedHandle closes on all return/cancellation paths; KILL_ON_JOB_CLOSE
            // also protects shutdown if the Tokio supervisor is itself dropped.
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::OnceLock;

    // libtest itself prints a prelude on stdout, so the current test executable
    // cannot be an NDJSON server. Compile this tiny std-only Rust fixture once.
    // It works on Windows and Unix without Bash, Python, or another dependency.
    const FIXTURE: &str = r###"
use std::io::{self, BufRead, Write};
use std::time::Duration;
fn send(text: &str) { let mut out=io::stdout().lock(); writeln!(out,"{text}").unwrap(); out.flush().unwrap(); }
fn main() {
    if std::env::args().nth(1).as_deref()==Some("--linger") { loop { std::thread::sleep(Duration::from_secs(60)); } }
    let mode=std::env::var("TUTOR_TEST_MODE").unwrap();
    let mut initialized=false;
    let mut pages=0;
    for line in io::stdin().lock().lines() {
        let line=line.unwrap();
        if line.contains("\"method\":\"notifications/initialized\"") {
            initialized=true;
            if mode=="idle" { std::thread::sleep(Duration::from_millis(30)); send(r#"{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}"#); }
            continue;
        }
        let start=line.find("\"id\":").unwrap()+5;
        let id:u64=line[start..].trim_start().chars().take_while(|c|c.is_ascii_digit()).collect::<String>().parse().unwrap();
        if line.contains("\"method\":\"initialize\"") {
            assert!(line.contains("2025-06-18"));
            send(&format!(r#"{{"jsonrpc":"2.0","id":{id},"result":{{"protocolVersion":"2025-06-18","capabilities":{{"tools":{{"listChanged":true}}}}}}}}"#));
            continue;
        }
        assert!(initialized);
        if line.contains("\"method\":\"tools/list\"") {
            pages+=1;
            let schema=r#""inputSchema":{"type":"object","properties":{"kind":{"type":"string","enum":["one","two"]}}},"annotations":{"readOnlyHint":true}"#;
            if mode=="notify" { send(r#"{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}"#); }
            let page=if mode=="loop" { r#"{"tools":[],"nextCursor":"same"}"#.to_owned() }
                else if mode=="pages" { format!(r#"{{"tools":[],"nextCursor":"page-{pages}"}}"#) }
                else if mode=="too_many" { format!(r#"{{"tools":[{}]}}"#,(0..513).map(|i|format!(r#"{{"name":"tool_{i}",{schema}}}"#)).collect::<Vec<_>>().join(",")) }
                else if mode=="duplicate" { format!(r#"{{"tools":[{{"name":"echo",{schema}}},{{"name":"echo",{schema}}}]}}"#) }
                else if !line.contains("\"cursor\"") { format!(r#"{{"tools":[{{"name":"echo",{schema}}}],"nextCursor":"next"}}"#) }
                else { format!(r#"{{"tools":[{{"name":"second",{schema}}}]}}"#) };
            send(&format!(r#"{{"jsonrpc":"2.0","id":{id},"result":{page}}}"#));
            continue;
        }
        match mode.as_str() {
            "hang" => { loop { std::thread::sleep(Duration::from_secs(60)); } },
            "bad_id" => { send(&format!(r#"{{"jsonrpc":"2.0","id":{},"result":{{"content":[]}}}}"#, id+1)); continue; },
            "error" => { send(&format!(r#"{{"jsonrpc":"2.0","id":{id},"error":{{"code":-32000,"message":"private-native-secret private-key"}}}}"#)); continue; },
            "partial" => { print!("{{\"jsonrpc\":"); io::stdout().flush().unwrap(); return; },
            "utf8" => { io::stdout().write_all(&[0xff,b'\n']).unwrap(); io::stdout().flush().unwrap(); return; },
            "oversized" => { let _=io::stdout().write_all(&vec![b'x';8*1024*1024+1]); let _=io::stdout().flush(); std::thread::sleep(Duration::from_secs(60)); return; },
            "exit" => std::process::exit(7),
            "stderr" => { let mut err=io::stderr().lock(); for _ in 0..2048 { err.write_all(&[b's';4096]).unwrap(); } err.flush().unwrap(); },
            _=>{}
        }
        let mut descendant=0;
        if line.contains("\"name\":\"spawn_child\"") {
            let child=std::process::Command::new(std::env::current_exe().unwrap()).arg("--linger")
                .stdin(std::process::Stdio::null()).stdout(std::process::Stdio::null()).stderr(std::process::Stdio::null()).spawn().unwrap();
            descendant=child.id(); std::mem::forget(child);
        }
        let args=std::env::args().skip(1).map(|v|format!("{:?}",v)).collect::<Vec<_>>().join(",");
        let env=std::env::var("TUTOR_TEST_VALUE").unwrap();
        let error=line.contains("\"name\":\"tool_error\"");
        send(&format!(r#"{{"jsonrpc":"2.0","id":{id},"result":{{"isError":{error},"content":[],"structuredContent":{{"request":{line},"args":[{args}],"env":{env:?},"pid":{},"childPid":{descendant}}}}}}}"#,std::process::id()));
        if mode=="final" || line.contains("\"name\":\"exit_after_reply\"") { return; }
    }
}
"###;

    fn fixture() -> &'static Path {
        static DIRECTORY: OnceLock<tempfile::TempDir> = OnceLock::new();
        let directory = DIRECTORY.get_or_init(|| {
            let directory = tempfile::Builder::new()
                .prefix("tutor stdio élève ")
                .tempdir()
                .unwrap();
            let source = directory.path().join("fixture.rs");
            let binary = directory.path().join(if cfg!(windows) {
                "mcp-fixture.exe"
            } else {
                "mcp-fixture"
            });
            std::fs::write(&source, FIXTURE).unwrap();
            let rustc = std::env::var_os("RUSTC").unwrap_or_else(|| "rustc".into());
            let output = std::process::Command::new(rustc)
                .arg("--edition=2021")
                .arg("--crate-name=mcp_fixture")
                .arg(&source)
                .arg("-o")
                .arg(&binary)
                .output()
                .unwrap();
            assert!(
                output.status.success(),
                "fixture compile failed: {}",
                String::from_utf8_lossy(&output.stderr)
            );
            directory
        });
        directory.path()
    }

    fn config(mode: &str) -> StdioConfig {
        let binary = fixture().join(if cfg!(windows) {
            "mcp-fixture.exe"
        } else {
            "mcp-fixture"
        });
        StdioConfig::from_acp(&json!({"name":"tutor","command":binary,"args":["literal ; $(echo never)","élève \"quoted\""],"env":[
            {"name":"TUTOR_TEST_MODE","value":mode},{"name":"TUTOR_TEST_VALUE","value":"opaque value ; $(no-shell)"}
        ]}), fixture()).unwrap()
    }

    struct Observer {
        #[cfg(unix)]
        pid: u32,
        #[cfg(windows)]
        handle: std::os::windows::io::OwnedHandle,
    }
    impl Observer {
        fn new(pid: u32) -> Self {
            #[cfg(unix)]
            {
                Self { pid }
            }
            #[cfg(windows)]
            {
                use std::os::windows::io::FromRawHandle;
                use windows_sys::Win32::System::Threading::{OpenProcess, PROCESS_SYNCHRONIZE};
                // SAFETY: pid comes only from our private fixture process.
                let raw = unsafe { OpenProcess(PROCESS_SYNCHRONIZE, 0, pid) };
                assert!(!raw.is_null());
                // SAFETY: fresh owned process handle, also prevents PID reuse.
                Self {
                    handle: unsafe { std::os::windows::io::OwnedHandle::from_raw_handle(raw) },
                }
            }
        }
        fn stopped(&self) -> bool {
            #[cfg(unix)]
            {
                // SAFETY: signal 0 only observes the fixture's PID.
                if unsafe { libc::kill(self.pid as libc::pid_t, 0) } != 0 {
                    return io::Error::last_os_error().raw_os_error() == Some(libc::ESRCH);
                }
                #[cfg(target_os = "linux")]
                if let Ok(stat) = std::fs::read_to_string(format!("/proc/{}/stat", self.pid)) {
                    return stat.split(") ").nth(1).is_some_and(|s| s.starts_with('Z'));
                }
                false
            }
            #[cfg(windows)]
            {
                use std::os::windows::io::AsRawHandle;
                // SAFETY: observer owns this fixture process handle.
                unsafe {
                    windows_sys::Win32::System::Threading::WaitForSingleObject(
                        self.handle.as_raw_handle(),
                        0,
                    ) == windows_sys::Win32::Foundation::WAIT_OBJECT_0
                }
            }
        }
        async fn assert_stopped(&self) {
            tokio::time::timeout(Duration::from_secs(4), async {
                while !self.stopped() {
                    tokio::time::sleep(Duration::from_millis(10)).await;
                }
            })
            .await
            .expect("fixture process survived transport cleanup");
        }
    }

    #[test]
    fn launch_configuration_is_bounded_and_never_invokes_a_shell() {
        let good = config("normal");
        assert_eq!(good.args[0], "literal ; $(echo never)");
        let basic = json!({"name":"tutor","command":"tutor","args":[],"env":[]});
        assert!(StdioConfig::from_acp(&basic, fixture()).is_ok());
        for command in [
            "sh",
            "bash",
            "cmd.exe",
            "powershell.exe",
            "server.cmd",
            "server.bat",
            "relative/path",
            "tutor --flag",
            "bad\0name",
        ] {
            let mut invalid = basic.clone();
            invalid["command"] = json!(command);
            assert_eq!(
                StdioConfig::from_acp(&invalid, fixture()).err(),
                Some(StdioError::InvalidConfig)
            );
        }
        let mut invalid = basic.clone();
        invalid["args"] = json!(["x".repeat(32769)]);
        assert_eq!(
            StdioConfig::from_acp(&invalid, fixture()).err(),
            Some(StdioError::InvalidConfig)
        );
        invalid = basic.clone();
        invalid["env"] = json!([{"name":"TOKEN","value":"one"},{"name":"token","value":"two"}]);
        assert_eq!(
            StdioConfig::from_acp(&invalid, fixture()).err(),
            Some(StdioError::InvalidConfig)
        );
        assert_eq!(
            StdioConfig::from_acp(&basic, Path::new("relative")).err(),
            Some(StdioError::InvalidConfig)
        );
        invalid = basic;
        invalid["type"] = json!("http");
        assert_eq!(
            StdioConfig::from_acp(&invalid, fixture()).err(),
            Some(StdioError::UnsupportedTransport)
        );
    }

    #[tokio::test]
    async fn actual_process_preserves_launch_arguments_schemas_and_idempotency_metadata() {
        let cancel = AtomicBool::new(false);
        let mut client = StdioClient::connect(config("normal"), &cancel)
            .await
            .unwrap();
        let observer = Observer::new(client.pid);
        assert!(!observer.stopped());
        assert_eq!(client.server_id(), "tutor");
        let tools = client.list_tools(&cancel).await.unwrap();
        assert_eq!(tools.len(), 2);
        assert_eq!(
            tools[0].input_schema["properties"]["kind"]["enum"],
            json!(["one", "two"])
        );
        assert!(!client.take_tools_changed());
        let response = client
            .call_tool(
                "tool_error",
                json!({"kind":"one"}),
                Some("private-key"),
                &cancel,
            )
            .await
            .unwrap();
        assert_eq!(response["isError"], true);
        assert_eq!(
            response["structuredContent"]["args"],
            json!(["literal ; $(echo never)", "élève \"quoted\""])
        );
        assert_eq!(
            response["structuredContent"]["env"],
            "opaque value ; $(no-shell)"
        );
        assert_eq!(
            response["structuredContent"]["request"]["params"]["_meta"]["idempotency_key"],
            "private-key"
        );
        assert_eq!(
            response["structuredContent"]["request"]["params"]["arguments"],
            json!({"kind":"one"})
        );
        assert!(client.take_tools_changed());
        client.close().await.unwrap();
        observer.assert_stopped().await;
        client.close().await.unwrap();
    }

    #[tokio::test]
    async fn errors_invalid_frames_and_child_eof_are_private_and_poison_the_connection() {
        for (mode, expected) in [
            ("error", StdioError::RemoteError),
            ("bad_id", StdioError::Protocol),
            ("partial", StdioError::Protocol),
            ("utf8", StdioError::Protocol),
            ("oversized", StdioError::SizeLimit),
            ("exit", StdioError::Closed),
        ] {
            let cancel = AtomicBool::new(false);
            let mut client = StdioClient::connect(config(mode), &cancel).await.unwrap();
            let observer = Observer::new(client.pid);
            let error = client
                .call_tool("mutation", json!({}), Some("private-key"), &cancel)
                .await
                .unwrap_err();
            assert_eq!(error, expected, "{mode}");
            assert!(!error.to_string().contains("private-native-secret"));
            assert!(!error.to_string().contains("private-key"));
            assert_eq!(
                client.call_tool("mutation", json!({}), None, &cancel).await,
                Err(StdioError::Closed)
            );
            observer.assert_stopped().await;
        }
    }

    #[tokio::test]
    async fn cancel_or_drop_of_request_stops_and_reaps_child_without_a_retry() {
        for abandon in [false, true] {
            let cancel = AtomicBool::new(false);
            let mut client = StdioClient::connect(config("hang"), &cancel).await.unwrap();
            let observer = Observer::new(client.pid);
            if abandon {
                assert!(tokio::time::timeout(
                    Duration::from_millis(80),
                    client.call_tool("mutation", json!({}), Some("stable"), &cancel)
                )
                .await
                .is_err());
            } else {
                let trigger = async {
                    tokio::time::sleep(Duration::from_millis(30)).await;
                    cancel.store(true, Ordering::Release);
                };
                let (result, ()) = tokio::join!(
                    client.call_tool("mutation", json!({}), Some("stable"), &cancel),
                    trigger
                );
                assert_eq!(result, Err(StdioError::Cancelled));
            }
            observer.assert_stopped().await;
            cancel.store(false, Ordering::Release);
            assert_eq!(
                client.call_tool("mutation", json!({}), None, &cancel).await,
                Err(StdioError::Closed)
            );
            client.close().await.unwrap();
        }
    }

    #[tokio::test]
    async fn final_reply_is_not_lost_to_child_exit_and_drop_stops_idle_child() {
        let cancel = AtomicBool::new(false);
        let mut client = StdioClient::connect(config("final"), &cancel)
            .await
            .unwrap();
        let observer = Observer::new(client.pid);
        assert_eq!(
            client
                .call_tool("echo", json!({}), None, &cancel)
                .await
                .unwrap()["isError"],
            false
        );
        observer.assert_stopped().await;
        client.close().await.unwrap();
        let client = StdioClient::connect(config("normal"), &cancel)
            .await
            .unwrap();
        let observer = Observer::new(client.pid);
        drop(client);
        observer.assert_stopped().await;
    }

    #[tokio::test]
    async fn shutdown_and_graceful_leader_exit_stop_descendants_too() {
        let cancel = AtomicBool::new(false);
        for graceful in [false, true] {
            let mut client = StdioClient::connect(config("normal"), &cancel)
                .await
                .unwrap();
            let response = client
                .call_tool("spawn_child", json!({}), None, &cancel)
                .await
                .unwrap();
            let descendant =
                Observer::new(response["structuredContent"]["childPid"].as_u64().unwrap() as u32);
            assert!(!descendant.stopped());
            if graceful {
                client
                    .call_tool("exit_after_reply", json!({}), None, &cancel)
                    .await
                    .unwrap();
            }
            client.close().await.unwrap();
            descendant.assert_stopped().await;
        }
    }

    #[tokio::test]
    async fn stderr_flood_is_drained_without_corrupting_stdout() {
        let cancel = AtomicBool::new(false);
        let mut client = StdioClient::connect(config("stderr"), &cancel)
            .await
            .unwrap();
        let response = client
            .call_tool("echo", json!({"text":"élève 🦀"}), None, &cancel)
            .await
            .unwrap();
        assert_eq!(
            response["structuredContent"]["request"]["params"]["arguments"]["text"],
            "élève 🦀"
        );
        client.close().await.unwrap();
    }

    #[tokio::test]
    async fn idle_notifications_invalidate_catalog_and_mid_snapshot_changes_fail() {
        let cancel = AtomicBool::new(false);
        let mut client = StdioClient::connect(config("idle"), &cancel).await.unwrap();
        client.take_tools_changed();
        tokio::time::timeout(Duration::from_secs(2), async {
            while !client.take_tools_changed() {
                tokio::time::sleep(Duration::from_millis(5)).await;
            }
        })
        .await
        .unwrap();
        client.close().await.unwrap();
        let mut client = StdioClient::connect(config("notify"), &cancel)
            .await
            .unwrap();
        assert_eq!(
            client.list_tools(&cancel).await.err(),
            Some(StdioError::CatalogChanged)
        );
        assert!(client.take_tools_changed());
        client.close().await.unwrap();
    }

    #[tokio::test]
    async fn duplicate_tools_and_cursor_cycles_never_return_partial_catalogs() {
        for mode in ["duplicate", "loop", "pages", "too_many"] {
            let cancel = AtomicBool::new(false);
            let mut client = StdioClient::connect(config(mode), &cancel).await.unwrap();
            assert_eq!(
                client.list_tools(&cancel).await.err(),
                Some(StdioError::InvalidCatalog)
            );
            if mode == "pages" {
                assert_eq!(client.next_id, 66);
            }
            client.close().await.unwrap();
        }
    }

    #[tokio::test]
    async fn frame_limits_apply_before_unterminated_data_can_grow() {
        let data = vec![b'x'; MAX_FRAME + 1];
        assert_eq!(
            read_frame(&mut BufReader::new(data.as_slice())).await,
            Err(StdioError::SizeLimit)
        );
        assert_eq!(
            read_frame(&mut BufReader::new(b"partial".as_slice())).await,
            Err(StdioError::Protocol)
        );
        assert_eq!(
            read_frame(&mut BufReader::new(b"{}\r\n".as_slice()))
                .await
                .unwrap()
                .unwrap(),
            b"{}"
        );
        assert_eq!(
            encode(&json!({"large":"x".repeat(MAX_FRAME)})),
            Err(StdioError::SizeLimit)
        );
    }

    #[tokio::test]
    async fn actual_request_timeout_stops_child_and_leaves_no_retry_path() {
        let cancel = AtomicBool::new(false);
        let mut client = StdioClient::connect(config("hang"), &cancel).await.unwrap();
        let observer = Observer::new(client.pid);
        let result = tokio::time::timeout(
            REQUEST_TIMEOUT + CLOSE_TIMEOUT + Duration::from_secs(1),
            client.call_tool("mutation", json!({}), Some("stable-timeout-key"), &cancel),
        )
        .await
        .unwrap();
        assert_eq!(result, Err(StdioError::Timeout));
        observer.assert_stopped().await;
        assert_eq!(
            client.call_tool("mutation", json!({}), None, &cancel).await,
            Err(StdioError::Closed)
        );
    }
}
