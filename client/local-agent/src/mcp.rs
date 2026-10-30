// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! Bounded HTTP transport for the unchanged Tutor MCP endpoint. Stdio is
//! explicitly unsupported here. No tools are authorized or executed on behalf
//! of a model: the future H05 executor must approve each call before using this
//! transport. No automatic retries, redirects, background streams or replay.
//!
//! Requires reqwest >=0.12.24 with json,rustls-tls and default-features=false.

use std::collections::BTreeSet;
use std::fmt;
use std::io::{self, Write};
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;

use reqwest::header::{HeaderMap, HeaderName, HeaderValue};
use serde_json::{json, Value};

use crate::catalog::{ToolCatalog, ToolEntry, MAX_TOOLS};

const PROTOCOL: &str = "2025-06-18";
const MAX_REQUEST_BYTES: usize = 1024 * 1024;
const MAX_RESPONSE_BYTES: usize = 8 * 1024 * 1024;
const MAX_PAGES: usize = 64;
const MAX_EVENTS: usize = 64;
const REQUEST_TIMEOUT: Duration = Duration::from_secs(30);

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum McpError {
    InvalidConfig,
    UnsupportedTransport,
    Closed,
    Cancelled,
    Timeout,
    Network,
    HttpStatus,
    Protocol,
    RemoteError,
    SizeLimit,
    InvalidCatalog,
    CatalogChanged,
    UnsupportedTools,
}

impl fmt::Display for McpError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            Self::InvalidConfig => "Invalid trusted MCP configuration.",
            Self::UnsupportedTransport => "Only HTTP MCP is supported by this transport.",
            Self::Closed => "MCP connection is closed; reconnect explicitly.",
            Self::Cancelled => "MCP request cancelled; its remote outcome may be unknown.",
            Self::Timeout => "MCP request timed out; its remote outcome may be unknown.",
            Self::Network => "MCP network request failed; its remote outcome may be unknown.",
            Self::HttpStatus => "MCP endpoint rejected the HTTP request.",
            Self::Protocol => "Invalid MCP protocol response.",
            Self::RemoteError => "MCP endpoint returned an RPC error.",
            Self::SizeLimit => "MCP request or response exceeds the transport limit.",
            Self::InvalidCatalog => "MCP tool catalog is invalid or exceeds its limits.",
            Self::CatalogChanged => "MCP tools changed while listing; discard this snapshot.",
            Self::UnsupportedTools => "MCP server does not advertise tools.",
        })
    }
}
impl std::error::Error for McpError {}

/// Trusted ACP configuration, never Debug/Serialize and never sent to a model.
pub struct ServerConfig {
    name: String,
    url: reqwest::Url,
    headers: HeaderMap,
}

impl ServerConfig {
    pub fn from_acp(value: &Value) -> Result<Self, McpError> {
        if value.get("type").and_then(Value::as_str) != Some("http") {
            return Err(McpError::UnsupportedTransport);
        }
        let name = value
            .get("name")
            .and_then(Value::as_str)
            .filter(|s| valid_name(s))
            .ok_or(McpError::InvalidConfig)?;
        let url = value
            .get("url")
            .and_then(Value::as_str)
            .filter(|s| s.len() <= 4096)
            .ok_or(McpError::InvalidConfig)?;
        let url = reqwest::Url::parse(url).map_err(|_| McpError::InvalidConfig)?;
        if !matches!(url.scheme(), "http" | "https")
            || url.host_str().is_none()
            || !url.username().is_empty()
            || url.password().is_some()
            || url.fragment().is_some()
        {
            return Err(McpError::InvalidConfig);
        }
        let mut headers = HeaderMap::new();
        let items = value
            .get("headers")
            .and_then(Value::as_array)
            .ok_or(McpError::InvalidConfig)?;
        if items.len() > 32 {
            return Err(McpError::InvalidConfig);
        }
        let mut total = 0;
        for item in items {
            let key = item
                .get("name")
                .and_then(Value::as_str)
                .filter(|s| !s.is_empty() && s.len() <= 128)
                .ok_or(McpError::InvalidConfig)?;
            let value = item
                .get("value")
                .and_then(Value::as_str)
                .filter(|s| s.len() <= 64 * 1024)
                .ok_or(McpError::InvalidConfig)?;
            total += key.len() + value.len();
            if total > 96 * 1024 {
                return Err(McpError::InvalidConfig);
            }
            let key =
                HeaderName::from_bytes(key.as_bytes()).map_err(|_| McpError::InvalidConfig)?;
            if matches!(
                key.as_str(),
                "host"
                    | "connection"
                    | "content-length"
                    | "transfer-encoding"
                    | "accept"
                    | "content-type"
                    | "mcp-protocol-version"
                    | "mcp-session-id"
                    | "last-event-id"
            ) || headers.contains_key(&key)
            {
                return Err(McpError::InvalidConfig);
            }
            let mut value = HeaderValue::from_str(value).map_err(|_| McpError::InvalidConfig)?;
            value.set_sensitive(true);
            headers.insert(key, value);
        }
        Ok(Self {
            name: name.into(),
            url,
            headers,
        })
    }
}

/// &mut self serializes every request. Before sending, ready becomes false and
/// is restored only on a valid response. Dropping a request future therefore
/// prevents accidental reuse of a connection with an unknown mutation outcome.
pub struct McpClient {
    config: ServerConfig,
    http: reqwest::Client,
    session: Option<HeaderValue>,
    next_id: u64,
    ready: bool,
    supports_tools: bool,
    tools_changed: bool,
    catalog_revision: u64,
}

impl McpClient {
    pub async fn connect(config: ServerConfig, cancel: &AtomicBool) -> Result<Self, McpError> {
        let http = reqwest::Client::builder()
            .redirect(reqwest::redirect::Policy::none())
            .retry(reqwest::retry::never())
            .no_proxy()
            .connect_timeout(Duration::from_secs(10))
            .timeout(REQUEST_TIMEOUT)
            .build()
            .map_err(|_| McpError::Network)?;
        let mut client = Self {
            config,
            http,
            session: None,
            next_id: 1,
            ready: true,
            supports_tools: false,
            tools_changed: true,
            catalog_revision: 0,
        };
        let initialized = client.request("initialize", json!({"protocolVersion":PROTOCOL,
            "capabilities":{},"clientInfo":{"name":"tutor-local-agent","version":env!("CARGO_PKG_VERSION")}}), cancel).await?;
        if initialized.get("protocolVersion").and_then(Value::as_str) != Some(PROTOCOL)
            || !initialized
                .get("capabilities")
                .is_some_and(Value::is_object)
        {
            return Err(McpError::Protocol);
        }
        client.supports_tools = initialized
            .pointer("/capabilities/tools")
            .is_some_and(Value::is_object);
        client.ready = false;
        let body =
            encode(&json!({"jsonrpc":"2.0","method":"notifications/initialized","params":{}}))?;
        bounded(client.post(body, None, false), cancel).await?;
        client.ready = true;
        Ok(client)
    }

    pub fn server_id(&self) -> &str {
        &self.config.name
    }

    /// A hint only, not a fresh catalog. HTTP notifications are consumed from
    /// request SSE responses; no idle GET stream is opened. Tool results also
    /// conservatively invalidate the catalog because visibility can change.
    pub fn take_tools_changed(&mut self) -> bool {
        std::mem::take(&mut self.tools_changed)
    }

    /// Close locally without replay, HTTP DELETE, or any remote mutation.
    pub fn close(&mut self) {
        self.ready = false;
        self.session = None;
        self.config.headers.clear();
    }

    pub async fn list_tools(&mut self, cancel: &AtomicBool) -> Result<Vec<ToolEntry>, McpError> {
        if !self.supports_tools {
            return Err(McpError::UnsupportedTools);
        }
        let revision = self.catalog_revision;
        let mut cursor = None;
        let mut cursors = BTreeSet::new();
        let mut names = BTreeSet::new();
        let mut entries = Vec::new();
        let mut check = ToolCatalog::new("transport-validation".into())
            .map_err(|_| McpError::InvalidCatalog)?;
        for _ in 0..MAX_PAGES {
            let params = cursor
                .as_ref()
                .map(|c| json!({"cursor":c}))
                .unwrap_or_else(|| json!({}));
            let result = self.request("tools/list", params, cancel).await?;
            let tools = result
                .get("tools")
                .and_then(Value::as_array)
                .ok_or(McpError::InvalidCatalog)?;
            if entries.len() + tools.len() > MAX_TOOLS {
                return Err(McpError::InvalidCatalog);
            }
            for tool in tools {
                let name = tool
                    .get("name")
                    .and_then(Value::as_str)
                    .filter(|s| valid_name(s))
                    .ok_or(McpError::InvalidCatalog)?;
                if !names.insert(name.to_owned()) {
                    return Err(McpError::InvalidCatalog);
                }
                let description = match tool.get("description") {
                    None => String::new(),
                    Some(Value::String(s)) => s.clone(),
                    _ => return Err(McpError::InvalidCatalog),
                };
                let entry = ToolEntry {
                    server_id: self.config.name.clone(),
                    name: name.into(),
                    description,
                    input_schema: tool
                        .get("inputSchema")
                        .cloned()
                        .ok_or(McpError::InvalidCatalog)?,
                    annotations: tool.get("annotations").cloned().unwrap_or(Value::Null),
                };
                // Validate each entry before retaining another page, so a bad
                // server cannot accumulate pages of oversized schemas/descriptions.
                check
                    .replace_server_tools(
                        "transport-validation",
                        &self.config.name,
                        vec![entry.clone()],
                    )
                    .map_err(|_| McpError::InvalidCatalog)?;
                entries.push(entry);
            }
            cursor = match result.get("nextCursor") {
                None | Some(Value::Null) => None,
                Some(Value::String(s)) if s.is_empty() => None,
                Some(Value::String(s)) if s.len() <= 4096 => Some(s.clone()),
                _ => return Err(McpError::InvalidCatalog),
            };
            if let Some(cursor) = &cursor {
                if !cursors.insert(cursor.clone()) {
                    return Err(McpError::InvalidCatalog);
                }
            } else {
                if revision != self.catalog_revision {
                    return Err(McpError::CatalogChanged);
                }
                self.tools_changed = false;
                return Ok(entries);
            }
        }
        Err(McpError::InvalidCatalog)
    }

    /// Transport only: caller must enforce identity, live schema, permission,
    /// pedagogical state and idempotency before this single attempt. isError
    /// and structuredContent are returned untouched, never rewritten to success.
    pub async fn call_tool(
        &mut self,
        name: &str,
        arguments: Value,
        idempotency_key: Option<&str>,
        cancel: &AtomicBool,
    ) -> Result<Value, McpError> {
        if !self.supports_tools {
            return Err(McpError::UnsupportedTools);
        }
        if !valid_name(name) || !arguments.is_object() {
            return Err(McpError::Protocol);
        }
        let mut params = json!({"name":name,"arguments":arguments});
        if let Some(key) = idempotency_key {
            if key.is_empty() || key.len() > 256 {
                return Err(McpError::Protocol);
            }
            params["_meta"] = json!({"idempotency_key":key});
        }
        self.tools_changed = true;
        let result = self.request("tools/call", params, cancel).await?;
        if !result.get("content").is_some_and(Value::is_array)
            || result.get("isError").is_some_and(|v| !v.is_boolean())
            || result
                .get("structuredContent")
                .is_some_and(|v| !v.is_object())
        {
            self.ready = false;
            return Err(McpError::Protocol);
        }
        Ok(result)
    }

    async fn request(
        &mut self,
        method: &str,
        params: Value,
        cancel: &AtomicBool,
    ) -> Result<Value, McpError> {
        if !self.ready {
            return Err(McpError::Closed);
        }
        let id = self.next_id;
        self.next_id = self.next_id.checked_add(1).ok_or(McpError::Closed)?;
        let body = encode(&json!({"jsonrpc":"2.0","id":id,"method":method,"params":params}))?;
        self.ready = false;
        let result = bounded(self.post(body, Some(id), method == "initialize"), cancel).await?;
        self.ready = true;
        result.ok_or(McpError::Protocol)
    }

    async fn post(
        &mut self,
        body: Vec<u8>,
        id: Option<u64>,
        initializing: bool,
    ) -> Result<Option<Value>, McpError> {
        let mut request = self
            .http
            .post(self.config.url.clone())
            .headers(self.config.headers.clone())
            .header("Accept", "application/json, text/event-stream")
            .header("Content-Type", "application/json")
            .header("Mcp-Protocol-Version", PROTOCOL)
            .body(body);
        if let Some(session) = &self.session {
            request = request.header("Mcp-Session-Id", session.clone());
        }
        let mut response = request.send().await.map_err(network_error)?;
        if !response.status().is_success() {
            return Err(McpError::HttpStatus);
        }
        if let Some(session) = response.headers().get("mcp-session-id") {
            if session.as_bytes().is_empty()
                || session.as_bytes().len() > 1024
                || !session.as_bytes().iter().all(|b| (0x21..=0x7e).contains(b))
            {
                return Err(McpError::Protocol);
            }
            if initializing {
                let mut session = session.clone();
                session.set_sensitive(true);
                self.session = Some(session);
            } else if self.session.as_ref() != Some(session) {
                return Err(McpError::Protocol);
            }
        }
        let Some(id) = id else {
            return if matches!(response.status().as_u16(), 202 | 204) {
                Ok(None)
            } else {
                Err(McpError::Protocol)
            };
        };
        if response
            .content_length()
            .is_some_and(|n| n > MAX_RESPONSE_BYTES as u64)
        {
            return Err(McpError::SizeLimit);
        }
        let content_type = response
            .headers()
            .get("content-type")
            .and_then(|h| h.to_str().ok())
            .and_then(|s| s.split(';').next())
            .unwrap_or("")
            .trim();
        let sse = content_type.eq_ignore_ascii_case("text/event-stream");
        if !sse && !content_type.eq_ignore_ascii_case("application/json") {
            return Err(McpError::Protocol);
        }
        let mut data = Vec::new();
        let mut decoder = SseDecoder::default();
        let mut total = 0;
        while let Some(chunk) = response.chunk().await.map_err(network_error)? {
            total += chunk.len();
            if total > MAX_RESPONSE_BYTES {
                return Err(McpError::SizeLimit);
            }
            if sse {
                let found = self.response_batch(decoder.push(&chunk)?, id)?;
                // A POST SSE connection need not close before delivering the
                // result. Do not wait for EOF or request a Last-Event-ID replay.
                if found.is_some() {
                    return Ok(found);
                }
            } else {
                data.extend_from_slice(&chunk);
            }
        }
        if sse {
            return Err(McpError::Protocol);
        }
        let message = serde_json::from_slice(&data).map_err(|_| McpError::Protocol)?;
        self.response_message(message, id)?
            .map(Some)
            .ok_or(McpError::Protocol)
    }

    fn response_batch(&mut self, messages: Vec<Value>, id: u64) -> Result<Option<Value>, McpError> {
        let mut found = None;
        for message in messages {
            if let Some(result) = self.response_message(message, id)? {
                if found.replace(result).is_some() {
                    return Err(McpError::Protocol);
                }
            }
        }
        Ok(found)
    }

    fn response_message(&mut self, message: Value, id: u64) -> Result<Option<Value>, McpError> {
        if message.get("jsonrpc").and_then(Value::as_str) != Some("2.0") {
            return Err(McpError::Protocol);
        }
        if let Some(method) = message.get("method") {
            if message.get("id").is_some()
                || !method.is_string()
                || message.get("result").is_some()
                || message.get("error").is_some()
            {
                return Err(McpError::Protocol);
            }
            if method.as_str() == Some("notifications/tools/list_changed") {
                self.tools_changed = true;
                self.catalog_revision = self.catalog_revision.wrapping_add(1);
            }
            return Ok(None);
        }
        if message.get("id").and_then(Value::as_u64) != Some(id)
            || message.get("result").is_some() == message.get("error").is_some()
        {
            return Err(McpError::Protocol);
        }
        if message.get("error").is_some() {
            return Err(McpError::RemoteError);
        }
        Ok(message.get("result").cloned())
    }
}

fn valid_name(name: &str) -> bool {
    !name.is_empty()
        && name.len() <= 128
        && name
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"_.-".contains(&b))
}

fn network_error(error: reqwest::Error) -> McpError {
    if error.is_timeout() {
        McpError::Timeout
    } else {
        McpError::Network
    }
}

async fn bounded<T>(
    future: impl std::future::Future<Output = Result<T, McpError>>,
    cancel: &AtomicBool,
) -> Result<T, McpError> {
    tokio::select! {
        biased;
        _ = async { while !cancel.load(Ordering::Acquire) { tokio::time::sleep(Duration::from_millis(10)).await; } } => Err(McpError::Cancelled),
        result = tokio::time::timeout(REQUEST_TIMEOUT, future) => result.map_err(|_| McpError::Timeout)?,
    }
}

fn encode(value: &Value) -> Result<Vec<u8>, McpError> {
    struct BoundedWriter(Vec<u8>);
    impl Write for BoundedWriter {
        fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
            if self.0.len().saturating_add(bytes.len()) > MAX_REQUEST_BYTES {
                return Err(io::Error::other("MCP request size limit"));
            }
            self.0.extend_from_slice(bytes);
            Ok(bytes.len())
        }
        fn flush(&mut self) -> io::Result<()> {
            Ok(())
        }
    }
    let mut writer = BoundedWriter(Vec::new());
    serde_json::to_writer(&mut writer, value).map_err(|_| McpError::SizeLimit)?;
    Ok(writer.0)
}

#[derive(Default)]
struct SseDecoder {
    line: Vec<u8>,
    data: Vec<u8>,
    events: usize,
}

impl SseDecoder {
    fn push(&mut self, bytes: &[u8]) -> Result<Vec<Value>, McpError> {
        let mut messages = Vec::new();
        for byte in bytes {
            if *byte != b'\n' {
                self.line.push(*byte);
                if self.line.len() + self.data.len() > MAX_RESPONSE_BYTES {
                    return Err(McpError::SizeLimit);
                }
                continue;
            }
            if self.line.last() == Some(&b'\r') {
                self.line.pop();
            }
            if self.line.is_empty() {
                if !self.data.is_empty() {
                    self.events += 1;
                    if self.events > MAX_EVENTS {
                        return Err(McpError::SizeLimit);
                    }
                    messages
                        .push(serde_json::from_slice(&self.data).map_err(|_| McpError::Protocol)?);
                    self.data.clear();
                }
            } else if let Some(data) = self.line.strip_prefix(b"data:") {
                let data = data.strip_prefix(b" ").unwrap_or(data);
                if !self.data.is_empty() {
                    self.data.push(b'\n');
                }
                self.data.extend_from_slice(data);
                if self.data.len() > MAX_RESPONSE_BYTES {
                    return Err(McpError::SizeLimit);
                }
            }
            self.line.clear();
        }
        Ok(messages)
    }
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use std::sync::{Arc, Mutex};
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    use tokio::net::TcpListener;
    use tokio::task::JoinHandle;

    pub(crate) struct Reply {
        status: u16,
        kind: &'static str,
        body: String,
        headers: String,
        hold_open: bool,
    }
    impl Reply {
        pub(crate) fn json(value: Value) -> Self {
            Self {
                status: 200,
                kind: "application/json",
                body: value.to_string(),
                headers: String::new(),
                hold_open: false,
            }
        }
        fn sse(body: String) -> Self {
            Self {
                status: 200,
                kind: "text/event-stream",
                body,
                headers: String::new(),
                hold_open: false,
            }
        }
    }

    type Observed = Arc<Mutex<Vec<(Value, String)>>>;
    pub(crate) struct Mock {
        pub(crate) url: String,
        pub(crate) observed: Observed,
        task: JoinHandle<()>,
    }
    impl Drop for Mock {
        fn drop(&mut self) {
            self.task.abort();
        }
    }

    pub(crate) async fn mock(handler: impl Fn(&Value) -> Reply + Send + 'static) -> Mock {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let url = format!("http://{}/mcp", listener.local_addr().unwrap());
        let observed: Observed = Arc::new(Mutex::new(Vec::new()));
        let captured = observed.clone();
        let task = tokio::spawn(async move {
            loop {
                let (mut stream, _) = listener.accept().await.unwrap();
                let mut input = Vec::new();
                let split = loop {
                    let mut chunk = [0; 4096];
                    let count = stream.read(&mut chunk).await.unwrap();
                    assert!(count > 0);
                    input.extend_from_slice(&chunk[..count]);
                    assert!(input.len() < 128 * 1024);
                    if let Some(index) = input.windows(4).position(|s| s == b"\r\n\r\n") {
                        break index + 4;
                    }
                };
                let headers = String::from_utf8(input[..split].to_vec()).unwrap();
                let length: usize = headers
                    .lines()
                    .find_map(|line| {
                        let (key, value) = line.split_once(':')?;
                        key.eq_ignore_ascii_case("content-length")
                            .then(|| value.trim().parse().unwrap())
                    })
                    .unwrap();
                while input.len() < split + length {
                    let mut chunk = [0; 4096];
                    let count = stream.read(&mut chunk).await.unwrap();
                    assert!(count > 0);
                    input.extend_from_slice(&chunk[..count]);
                }
                let request: Value = serde_json::from_slice(&input[split..split + length]).unwrap();
                captured.lock().unwrap().push((request.clone(), headers));
                let reply = handler(&request);
                let body = reply.body.as_bytes();
                let framing = if reply.hold_open {
                    "Transfer-Encoding: chunked\r\n".to_owned()
                } else {
                    format!("Content-Length: {}\r\n", body.len())
                };
                let head = format!(
                    "HTTP/1.1 {} Test\r\nContent-Type: {}\r\n{}{}Connection: close\r\n\r\n",
                    reply.status, reply.kind, reply.headers, framing
                );
                stream.write_all(head.as_bytes()).await.unwrap();
                if reply.hold_open {
                    stream
                        .write_all(format!("{:x}\r\n", body.len()).as_bytes())
                        .await
                        .unwrap();
                    stream.write_all(body).await.unwrap();
                    stream.write_all(b"\r\n").await.unwrap();
                    // Deliberately no final chunk: POST result must complete
                    // before SSE EOF. Mock Drop aborts this private task.
                    tokio::time::sleep(Duration::from_secs(60)).await;
                } else {
                    stream.write_all(body).await.unwrap();
                    stream.shutdown().await.unwrap();
                }
            }
        });
        Mock {
            url,
            observed,
            task,
        }
    }

    pub(crate) fn initialization(request: &Value) -> Option<Reply> {
        match request["method"].as_str().unwrap() {
            "initialize" => {
                assert_eq!(request["params"]["protocolVersion"], PROTOCOL);
                let mut reply = Reply::json(json!({"jsonrpc":"2.0","id":request["id"],"result":{
                    "protocolVersion":PROTOCOL,"capabilities":{"tools":{"listChanged":true}},"serverInfo":{"name":"tutor","version":"test"}
                }}));
                reply.headers = "Mcp-Session-Id: private-session-test\r\n".into();
                Some(reply)
            }
            "notifications/initialized" => Some(Reply {
                status: 202,
                kind: "application/json",
                body: String::new(),
                headers: String::new(),
                hold_open: false,
            }),
            _ => None,
        }
    }

    fn config(url: &str) -> ServerConfig {
        ServerConfig::from_acp(&json!({"type":"http","name":"tutor","url":url,"headers":[
            {"name":"Authorization","value":"Bearer fake-test-secret"},
            {"name":"Tutor-Conversation-ID","value":"conversation-test"}
        ]}))
        .unwrap()
    }

    fn listed(name: &str) -> Value {
        json!({"name":name,"description":"Live tool","inputSchema":{"type":"object","properties":{"kind":{"type":"string","enum":["one","two"]}}},"annotations":{"readOnlyHint":true}})
    }

    #[test]
    fn configuration_accepts_large_tutor_policy_and_rejects_credential_leak_vectors() {
        let policy = "p".repeat(64 * 1024);
        let valid = json!({"type":"http","name":"tutor","url":"https://tutor.example/mcp","headers":[
            {"name":"Tutor-Tool-Policy","value":policy},{"name":"Authorization","value":"Bearer fake-secret"}
        ]});
        assert!(ServerConfig::from_acp(&valid).is_ok());
        for headers in [
            json!([{"name":"Tutor-Tool-Policy","value":"p".repeat(64*1024+1)}]),
            json!([{"name":"One","value":"p".repeat(64*1024)},{"name":"Two","value":"p".repeat(40*1024)}]),
            json!([{"name":"Authorization","value":"secret\r\nHost: malicious.example"}]),
            json!([{"name":"Host","value":"malicious.example"}]),
            json!([{"name":"mcp-session-id","value":"guessed"}]),
            json!([{"name":"Authorization","value":"one"},{"name":"authorization","value":"two"}]),
        ] {
            let mut invalid = valid.clone();
            invalid["headers"] = headers;
            assert_eq!(
                ServerConfig::from_acp(&invalid).err(),
                Some(McpError::InvalidConfig)
            );
        }
        for url in [
            "file:///private",
            "https://secret:password@tutor.example/mcp",
            "https://tutor.example/mcp#secret",
        ] {
            let mut invalid = valid.clone();
            invalid["url"] = json!(url);
            assert_eq!(
                ServerConfig::from_acp(&invalid).err(),
                Some(McpError::InvalidConfig)
            );
        }
        assert_eq!(
            ServerConfig::from_acp(&json!({"name":"tutor","command":"tutor","args":[],"env":[]}))
                .err(),
            Some(McpError::UnsupportedTransport)
        );
    }

    #[tokio::test]
    async fn real_http_handshake_pagination_and_call_preserve_protocol_and_tool_errors() {
        let server = mock(|req| {
            if let Some(reply) = initialization(req) { return reply; }
            let result = match req["method"].as_str().unwrap() {
                "tools/list" if req["params"].get("cursor").is_none() => json!({"tools":[listed("first")],"nextCursor":"page-2"}),
                "tools/list" => { assert_eq!(req["params"]["cursor"], "page-2"); json!({"tools":[listed("second")]}) },
                "tools/call" => {
                    assert_eq!(req["params"]["name"], "first");
                    assert_eq!(req["params"]["arguments"], json!({"kind":"one"}));
                    assert_eq!(req["params"]["_meta"]["idempotency_key"], "exact-private-key-v1");
                    json!({"isError":true,"content":[{"type":"text","text":"Tool rejected this operation"}],"structuredContent":{"accepted":false}})
                }
                _ => panic!("unexpected method"),
            };
            Reply::json(json!({"jsonrpc":"2.0","id":req["id"],"result":result}))
        }).await;
        let cancel = AtomicBool::new(false);
        let mut client = McpClient::connect(config(&server.url), &cancel)
            .await
            .unwrap();
        assert_eq!(client.server_id(), "tutor");
        let entries = client.list_tools(&cancel).await.unwrap();
        assert_eq!(entries.len(), 2);
        assert_eq!(
            entries[0].input_schema["properties"]["kind"]["enum"],
            json!(["one", "two"])
        );
        assert!(!client.take_tools_changed());
        let result = client
            .call_tool(
                "first",
                json!({"kind":"one"}),
                Some("exact-private-key-v1"),
                &cancel,
            )
            .await
            .unwrap();
        assert_eq!(result["isError"], true);
        assert_eq!(result["structuredContent"]["accepted"], false);
        assert!(client.take_tools_changed());
        let observed = server.observed.lock().unwrap();
        assert_eq!(observed.len(), 5);
        for (_, headers) in observed.iter().skip(1) {
            let headers = headers.to_ascii_lowercase();
            assert!(headers.contains("mcp-session-id: private-session-test"));
            assert!(headers.contains("mcp-protocol-version: 2025-06-18"));
            assert!(headers.contains("authorization: bearer fake-test-secret"));
            assert!(headers.contains("tutor-conversation-id: conversation-test"));
        }
    }

    #[tokio::test]
    async fn redirects_are_not_followed_and_credentials_never_reach_the_target() {
        let target = mock(|_| panic!("redirect received credentials")).await;
        let location = target.url.clone();
        let server = mock(move |_| Reply {
            status: 307,
            kind: "application/json",
            body: String::new(),
            headers: format!("Location: {location}\r\n"),
            hold_open: false,
        })
        .await;
        let error = McpClient::connect(config(&server.url), &AtomicBool::new(false))
            .await
            .err()
            .unwrap();
        assert_eq!(error, McpError::HttpStatus);
        assert!(target.observed.lock().unwrap().is_empty());
        assert_eq!(server.observed.lock().unwrap().len(), 1);
        assert!(!error.to_string().contains("fake-test-secret"));
    }

    #[tokio::test]
    async fn wrong_ids_and_rpc_errors_are_sanitized_and_never_retried() {
        for mismatch in [true, false] {
            let server = mock(move |req| {
                if let Some(reply) = initialization(req) { return reply; }
                Reply::json(if mismatch { json!({"jsonrpc":"2.0","id":999,"result":{}}) }
                    else { json!({"jsonrpc":"2.0","id":req["id"],"error":{"code":-32000,
                        "message":format!("Bearer fake-secret /private/path {}", req["params"]["_meta"]["idempotency_key"])}}) })
            }).await;
            let cancel = AtomicBool::new(false);
            let mut client = McpClient::connect(config(&server.url), &cancel)
                .await
                .unwrap();
            let error = client
                .call_tool(
                    "some_mutation",
                    json!({}),
                    Some("private-key-no-echo"),
                    &cancel,
                )
                .await
                .unwrap_err();
            assert_eq!(
                error,
                if mismatch {
                    McpError::Protocol
                } else {
                    McpError::RemoteError
                }
            );
            assert!(!error.to_string().contains("fake-secret"));
            assert!(!error.to_string().contains("private-key-no-echo"));
            assert_eq!(
                client
                    .call_tool("some_mutation", json!({}), None, &cancel)
                    .await,
                Err(McpError::Closed)
            );
            assert_eq!(server.observed.lock().unwrap().len(), 3);
        }
    }

    #[tokio::test]
    async fn sse_result_finishes_before_eof_and_preserves_invalidation_notifications() {
        let server = mock(|req| {
            if let Some(reply) = initialization(req) {
                return reply;
            }
            let mut reply = Reply::sse(format!(
                "data: {}\r\n\r\ndata: {}\r\n\r\n",
                json!({"jsonrpc":"2.0","method":"notifications/tools/list_changed"}),
                json!({"jsonrpc":"2.0","id":req["id"],"result":{"content":[],"isError":false}})
            ));
            reply.hold_open = true;
            reply
        })
        .await;
        let cancel = AtomicBool::new(false);
        let mut client = McpClient::connect(config(&server.url), &cancel)
            .await
            .unwrap();
        let value = tokio::time::timeout(
            Duration::from_secs(2),
            client.call_tool("read", json!({}), None, &cancel),
        )
        .await
        .unwrap()
        .unwrap();
        assert_eq!(value["isError"], false);
        assert!(client.take_tools_changed());
        assert_eq!(client.catalog_revision, 1);
    }

    #[tokio::test]
    async fn list_changed_during_snapshot_rejects_the_partial_catalog() {
        let server = mock(|req| {
            if let Some(reply) = initialization(req) {
                return reply;
            }
            Reply::sse(format!(
                "data: {}\n\ndata: {}\n\n",
                json!({"jsonrpc":"2.0","method":"notifications/tools/list_changed"}),
                json!({"jsonrpc":"2.0","id":req["id"],"result":{"tools":[listed("stale")]}})
            ))
        })
        .await;
        let cancel = AtomicBool::new(false);
        let mut client = McpClient::connect(config(&server.url), &cancel)
            .await
            .unwrap();
        assert_eq!(
            client.list_tools(&cancel).await.err(),
            Some(McpError::CatalogChanged)
        );
        assert!(client.take_tools_changed());
    }

    #[tokio::test]
    async fn pagination_loops_duplicates_and_oversized_entries_fail_atomically() {
        for variant in 0..4 {
            let server = mock(move |req| {
                if let Some(reply) = initialization(req) { return reply; }
                let result = match variant {
                    0 => json!({"tools":[],"nextCursor":"same-cursor"}),
                    1 => json!({"tools":[listed("duplicate"),listed("duplicate")]}),
                    2 => json!({"tools":(0..513).map(|i| listed(&format!("tool_{i}"))).collect::<Vec<_>>()}),
                    _ => { let mut item = listed("oversized"); item["inputSchema"]["description"] = json!("x".repeat(65536)); json!({"tools":[item]}) }
                };
                Reply::json(json!({"jsonrpc":"2.0","id":req["id"],"result":result}))
            }).await;
            let cancel = AtomicBool::new(false);
            let mut client = McpClient::connect(config(&server.url), &cancel)
                .await
                .unwrap();
            assert_eq!(
                client.list_tools(&cancel).await.err(),
                Some(McpError::InvalidCatalog)
            );
            assert!(server.observed.lock().unwrap().len() <= 4);
        }
    }

    #[tokio::test]
    async fn cancellation_and_abandoned_future_poison_connection_without_replay() {
        for abandon in [false, true] {
            let server = mock(|req| {
                if let Some(reply) = initialization(req) {
                    return reply;
                }
                let mut reply = Reply::sse(": waiting\n\n".into());
                reply.hold_open = true;
                reply
            })
            .await;
            let cancel = AtomicBool::new(false);
            let mut client = McpClient::connect(config(&server.url), &cancel)
                .await
                .unwrap();
            if abandon {
                assert!(tokio::time::timeout(
                    Duration::from_millis(200),
                    client.call_tool("mutation", json!({}), None, &cancel)
                )
                .await
                .is_err());
            } else {
                let observed = server.observed.clone();
                let trigger = async {
                    while observed.lock().unwrap().len() < 3 {
                        tokio::time::sleep(Duration::from_millis(1)).await;
                    }
                    cancel.store(true, Ordering::Release);
                };
                let (result, ()) = tokio::join!(
                    client.call_tool("mutation", json!({}), None, &cancel),
                    trigger
                );
                assert_eq!(result, Err(McpError::Cancelled));
            }
            cancel.store(false, Ordering::Release);
            assert_eq!(
                client.call_tool("mutation", json!({}), None, &cancel).await,
                Err(McpError::Closed)
            );
            assert_eq!(server.observed.lock().unwrap().len(), 3);
        }
    }

    #[test]
    fn sse_decoder_handles_fragmented_utf8_crlf_multiline_and_bounds() {
        let source = "data: {\"jsonrpc\":\"2.0\",\r\ndata: \"id\":2,\"result\":{\"text\":\"élève 🦀\"}}\r\n\r\n";
        let mut decoder = SseDecoder::default();
        let mut messages = Vec::new();
        for byte in source.bytes() {
            messages.extend(decoder.push(&[byte]).unwrap());
        }
        assert_eq!(messages.len(), 1);
        assert_eq!(messages[0]["result"]["text"], "élève 🦀");
        assert_eq!(
            SseDecoder::default().push(b"data: invalid-json\n\n"),
            Err(McpError::Protocol)
        );
        let mut decoder = SseDecoder::default();
        assert_eq!(
            decoder.push(&vec![b'x'; MAX_RESPONSE_BYTES + 1]),
            Err(McpError::SizeLimit)
        );
        let mut decoder = SseDecoder::default();
        assert_eq!(
            decoder.push("data: {}\n\n".repeat(MAX_EVENTS + 1).as_bytes()),
            Err(McpError::SizeLimit)
        );
        assert_eq!(
            encode(&json!({"oversized":"x".repeat(MAX_REQUEST_BYTES)})),
            Err(McpError::SizeLimit)
        );
    }

    #[tokio::test]
    async fn duplicate_results_in_one_batch_are_rejected_instead_of_resolving_twice() {
        let server = mock(|req| initialization(req).expect("only initialization is sent")).await;
        let cancel = AtomicBool::new(false);
        let mut client = McpClient::connect(config(&server.url), &cancel)
            .await
            .unwrap();
        let result = json!({"jsonrpc":"2.0","id":2,"result":{"tools":[]}});
        assert_eq!(
            client.response_batch(vec![result.clone(), result], 2),
            Err(McpError::Protocol)
        );
    }
}
