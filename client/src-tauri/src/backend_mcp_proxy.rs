// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! Loopback relay between an ACP adapter and the Tutor MCP endpoint.
//!
//! The adapter is configured with a stable URL and a stable bearer that only
//! this process knows. Each request is relayed to the current Tutor resource
//! with the current short-lived institution bearer, so renewing that grant
//! changes neither the session's MCP configuration nor the harness process
//! (nor, for Claude, its prompt cache). Streamable HTTP is relayed as is:
//! method, MCP headers (`Mcp-Session-Id`, `Mcp-Protocol-Version`,
//! `Last-Event-ID`…), status codes, and response bodies chunk by chunk, so an
//! SSE stream reaches the adapter while the server writes it. An upstream 401
//! renews the institution bearer and replays the buffered request once.
//!
//! The listener accepts loopback peers only, requires the local bearer, a
//! loopback `Host` and no browser `Origin`, checked on the request head before
//! any body is read. Idle connections and slow request heads or bodies are
//! closed after a bounded time.
use std::future::Future;
use std::net::SocketAddr;
use std::pin::Pin;
use std::sync::Arc;
use std::time::Duration;
use tokio::io::{
    AsyncBufRead, AsyncBufReadExt, AsyncReadExt, AsyncWrite, AsyncWriteExt, BufReader,
};
use tokio::net::{TcpListener, TcpStream};

const MAX_HEAD: u64 = 64 * 1024;
const MAX_HEADERS: usize = 100;
const MAX_BODY: usize = 16 * 1024 * 1024;
/// Waiting for, then receiving, a request head (keep-alive idle included).
const HEAD_TIMEOUT: Duration = Duration::from_secs(60);
/// Receiving a request body once its head was accepted.
const BODY_TIMEOUT: Duration = Duration::from_secs(60);

/// Current destination of a route.
pub(super) struct Upstream {
    pub url: String,
    pub bearer: String,
}

pub(super) type UpstreamFuture = Pin<Box<dyn Future<Output = Result<Upstream, u16>> + Send>>;

/// Resolve the upstream of `route`. `rejected` is the bearer the upstream has
/// just refused with 401: renew it if it is still the current one. An error is
/// the HTTP status returned to the adapter (403: the route no longer matches
/// the connected account; 503: no usable Tutor grant).
pub(super) type Resolver = Arc<dyn Fn(String, Option<String>) -> UpstreamFuture + Send + Sync>;

pub(super) struct McpProxy {
    address: SocketAddr,
    token: String,
    task: tokio::task::JoinHandle<()>,
}

impl Drop for McpProxy {
    fn drop(&mut self) {
        self.task.abort();
    }
}

struct Shared {
    authorization: String,
    hosts: [String; 2],
    resolver: Resolver,
    client: reqwest::Client,
    head_timeout: Duration,
    body_timeout: Duration,
}

/// How the body of a request is delimited.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Framing {
    Empty,
    Length(usize),
    Chunked,
}

struct Head {
    method: String,
    target: String,
    headers: Vec<(String, Vec<u8>)>,
    keep_alive: bool,
    framing: Framing,
}

struct Request {
    method: String,
    target: String,
    headers: Vec<(String, Vec<u8>)>,
    body: Vec<u8>,
    keep_alive: bool,
}

impl McpProxy {
    pub async fn start(resolver: Resolver) -> std::io::Result<Self> {
        Self::start_with(resolver, HEAD_TIMEOUT, BODY_TIMEOUT).await
    }

    async fn start_with(
        resolver: Resolver,
        head_timeout: Duration,
        body_timeout: Duration,
    ) -> std::io::Result<Self> {
        let listener = TcpListener::bind(("127.0.0.1", 0)).await?;
        let address = listener.local_addr()?;
        // 256 bits from the thread CSPRNG; never persisted nor logged.
        let token = format!(
            "{:032x}{:032x}",
            rand::random::<u128>(),
            rand::random::<u128>()
        );
        let client = reqwest::Client::builder()
            .redirect(reqwest::redirect::Policy::none())
            .retry(reqwest::retry::never())
            .connect_timeout(std::time::Duration::from_secs(10))
            .build()
            .map_err(std::io::Error::other)?;
        let shared = Arc::new(Shared {
            authorization: format!("Bearer {token}"),
            hosts: [
                format!("127.0.0.1:{}", address.port()),
                format!("localhost:{}", address.port()),
            ],
            resolver,
            client,
            head_timeout,
            body_timeout,
        });
        let task = tokio::spawn(async move {
            loop {
                let (stream, peer) = match listener.accept().await {
                    Ok(accepted) => accepted,
                    Err(_) => {
                        // Descriptor exhaustion must not spin the runtime.
                        tokio::time::sleep(std::time::Duration::from_millis(50)).await;
                        continue;
                    }
                };
                if !peer.ip().is_loopback() {
                    continue;
                }
                let shared = shared.clone();
                tokio::spawn(async move {
                    let _ = serve(stream, &shared).await;
                });
            }
        });
        Ok(Self {
            address,
            token,
            task,
        })
    }

    /// Stable URL of `route` for this process.
    pub fn endpoint(&self, route: &str) -> String {
        format!("http://{}/mcp/{route}", self.address)
    }

    /// Value of the `Authorization` header the adapter must send.
    pub fn authorization(&self) -> String {
        format!("Bearer {}", self.token)
    }
}

async fn serve(stream: TcpStream, shared: &Shared) -> std::io::Result<()> {
    let _ = stream.set_nodelay(true);
    let (read, mut write) = stream.into_split();
    let mut reader = BufReader::new(read);
    loop {
        let head = match tokio::time::timeout(shared.head_timeout, read_head(&mut reader)).await {
            // Idle keep-alive connection or a head trickled too slowly.
            Err(_) | Ok(Ok(None)) => return Ok(()),
            Ok(Err(status)) => return write_status(&mut write, status, false).await,
            Ok(Ok(Some(head))) => head,
        };
        // Authenticate on the head: an unauthorized caller never makes the
        // relay buffer a body. Its unread body leaves the connection unusable.
        if !admitted(shared, &head.headers) {
            return write_status(&mut write, 403, false).await;
        }
        let body =
            match tokio::time::timeout(shared.body_timeout, read_body(&mut reader, head.framing))
                .await
            {
                Err(_) => return write_status(&mut write, 408, false).await,
                Ok(Err(status)) => return write_status(&mut write, status, false).await,
                Ok(Ok(body)) => body,
            };
        let request = Request {
            method: head.method,
            target: head.target,
            headers: head.headers,
            body,
            keep_alive: head.keep_alive,
        };
        let keep_alive = request.keep_alive;
        if !relay(shared, request, &mut write).await? || !keep_alive {
            return Ok(());
        }
    }
}

/// A complete request (head and body); the relay itself authenticates the
/// head first (`serve`).
#[cfg(test)]
async fn read_request<R: AsyncBufRead + Unpin>(reader: &mut R) -> Result<Option<Request>, u16> {
    let Some(head) = read_head(reader).await? else {
        return Ok(None);
    };
    let body = read_body(reader, head.framing).await?;
    Ok(Some(Request {
        method: head.method,
        target: head.target,
        headers: head.headers,
        body,
        keep_alive: head.keep_alive,
    }))
}

/// Whether the head carries the local bearer, a loopback `Host` and no
/// browser `Origin`. A browser page cannot use this relay: it would send an
/// Origin header, and a rebound DNS name would not be a loopback Host.
fn admitted(shared: &Shared, headers: &[(String, Vec<u8>)]) -> bool {
    let value = |name: &str| {
        headers
            .iter()
            .find(|(key, _)| key == name)
            .map(|(_, value)| value.as_slice())
    };
    let host = value("host").map(|host| String::from_utf8_lossy(host).to_ascii_lowercase());
    let authorized = value("authorization")
        .is_some_and(|value| constant_time_eq(value, shared.authorization.as_bytes()));
    value("origin").is_none() && host.is_some_and(|host| shared.hosts.contains(&host)) && authorized
}

/// `Ok(None)` on a clean end of connection; `Err(status)` on a malformed or
/// oversized request head.
async fn read_head<R: AsyncBufRead + Unpin>(reader: &mut R) -> Result<Option<Head>, u16> {
    let mut budget = MAX_HEAD;
    let Some(line) = read_line(reader, &mut budget).await? else {
        return Ok(None);
    };
    let line = String::from_utf8(line).map_err(|_| 400_u16)?;
    let mut parts = line.split(' ');
    let (Some(method), Some(target), Some(version), None) =
        (parts.next(), parts.next(), parts.next(), parts.next())
    else {
        return Err(400);
    };
    if !matches!(version, "HTTP/1.1" | "HTTP/1.0")
        || method.is_empty()
        || !method.bytes().all(|byte| byte.is_ascii_uppercase())
        || !target.starts_with('/')
    {
        return Err(400);
    }
    let mut headers = Vec::new();
    loop {
        let line = read_line(reader, &mut budget).await?.ok_or(400_u16)?;
        if line.is_empty() {
            break;
        }
        if headers.len() >= MAX_HEADERS || line[0] == b' ' || line[0] == b'\t' {
            return Err(400);
        }
        let colon = line.iter().position(|byte| *byte == b':').ok_or(400_u16)?;
        let name = std::str::from_utf8(&line[..colon])
            .ok()
            .filter(|name| !name.is_empty() && !name.contains([' ', '\t']))
            .ok_or(400_u16)?
            .to_ascii_lowercase();
        let value = line[colon + 1..].trim_ascii().to_vec();
        headers.push((name, value));
    }
    let header = |name: &str| {
        headers
            .iter()
            .filter(|(key, _)| key == name)
            .map(|(_, value)| String::from_utf8_lossy(value).to_ascii_lowercase())
            .collect::<Vec<_>>()
    };
    let connection = header("connection").join(",");
    let keep_alive = if version == "HTTP/1.1" {
        !connection.split(',').any(|item| item.trim() == "close")
    } else {
        connection
            .split(',')
            .any(|item| item.trim() == "keep-alive")
    };
    let encodings = header("transfer-encoding");
    let lengths = header("content-length");
    let framing = if !encodings.is_empty() {
        if !lengths.is_empty() || encodings.join(",").trim() != "chunked" {
            return Err(400);
        }
        Framing::Chunked
    } else if let Some(length) = lengths.first() {
        if lengths.iter().any(|other| other != length) {
            return Err(400);
        }
        let length: usize = length.trim().parse().map_err(|_| 400_u16)?;
        if length > MAX_BODY {
            return Err(413);
        }
        Framing::Length(length)
    } else {
        Framing::Empty
    };
    Ok(Some(Head {
        method: method.into(),
        target: target.into(),
        headers,
        keep_alive,
        framing,
    }))
}

async fn read_body<R: AsyncBufRead + Unpin>(
    reader: &mut R,
    framing: Framing,
) -> Result<Vec<u8>, u16> {
    match framing {
        Framing::Empty => Ok(Vec::new()),
        Framing::Chunked => read_chunked(reader).await,
        Framing::Length(length) => {
            let mut body = vec![0; length];
            reader.read_exact(&mut body).await.map_err(|_| 400_u16)?;
            Ok(body)
        }
    }
}

/// One CRLF-terminated line without its terminator, within `budget` bytes.
async fn read_line<R: AsyncBufRead + Unpin>(
    reader: &mut R,
    budget: &mut u64,
) -> Result<Option<Vec<u8>>, u16> {
    let mut line = Vec::new();
    let read = reader
        .take(*budget)
        .read_until(b'\n', &mut line)
        .await
        .map_err(|_| 400_u16)?;
    if read == 0 {
        return Ok(None);
    }
    *budget -= read as u64;
    if line.pop() != Some(b'\n') {
        return Err(if *budget == 0 { 431 } else { 400 });
    }
    if line.last() == Some(&b'\r') {
        line.pop();
    }
    Ok(Some(line))
}

async fn read_chunked<R: AsyncBufRead + Unpin>(reader: &mut R) -> Result<Vec<u8>, u16> {
    let mut body = Vec::new();
    let mut budget = MAX_HEAD;
    loop {
        let line = read_line(reader, &mut budget).await?.ok_or(400_u16)?;
        let digits = std::str::from_utf8(&line)
            .ok()
            .and_then(|line| line.split(';').next())
            .map(str::trim)
            .filter(|size| !size.is_empty() && size.bytes().all(|byte| byte.is_ascii_hexdigit()))
            .ok_or(400_u16)?;
        // Leading zeros are valid; any other size beyond the body limit is
        // refused before it is added to the buffered length.
        let size = match usize::from_str_radix(digits, 16) {
            Ok(size) => size,
            Err(_) => return Err(413),
        };
        if size == 0 {
            // Trailers are not relayed.
            while !read_line(reader, &mut budget)
                .await?
                .ok_or(400_u16)?
                .is_empty()
            {}
            return Ok(body);
        }
        if body
            .len()
            .checked_add(size)
            .is_none_or(|total| total > MAX_BODY)
        {
            return Err(413);
        }
        let start = body.len();
        body.resize(start + size, 0);
        reader
            .read_exact(&mut body[start..])
            .await
            .map_err(|_| 400_u16)?;
        if !read_line(reader, &mut budget)
            .await?
            .ok_or(400_u16)?
            .is_empty()
        {
            return Err(400);
        }
    }
}

/// Hop-by-hop or re-framed headers, never relayed in either direction.
fn hop_by_hop(name: &str, listed: &[String]) -> bool {
    matches!(
        name,
        "connection"
            | "keep-alive"
            | "proxy-authenticate"
            | "proxy-authorization"
            | "proxy-connection"
            | "te"
            | "trailer"
            | "transfer-encoding"
            | "upgrade"
            | "content-length"
            | "host"
            | "expect"
    ) || listed.iter().any(|item| item == name)
}

fn constant_time_eq(left: &[u8], right: &[u8]) -> bool {
    left.len() == right.len()
        && left
            .iter()
            .zip(right)
            .fold(0_u8, |acc, (a, b)| acc | (a ^ b))
            == 0
}

/// Relay one request. `Ok(false)` when the connection cannot be reused.
async fn relay<W: AsyncWrite + Unpin>(
    shared: &Shared,
    request: Request,
    write: &mut W,
) -> std::io::Result<bool> {
    if !admitted(shared, &request.headers) {
        return write_status(write, 403, false).await.map(|_| false);
    }
    let (path, query) = match request.target.split_once('?') {
        Some((path, query)) => (path, Some(query)),
        None => (request.target.as_str(), None),
    };
    let Some(route) = path
        .strip_prefix("/mcp/")
        .filter(|route| {
            !route.is_empty()
                && route.len() <= 64
                && route
                    .bytes()
                    .all(|byte| matches!(byte, b'0'..=b'9' | b'a'..=b'f'))
        })
        .map(str::to_owned)
    else {
        return write_status(write, 404, request.keep_alive)
            .await
            .map(|_| request.keep_alive);
    };
    let method = match request.method.as_str() {
        "GET" => reqwest::Method::GET,
        "POST" => reqwest::Method::POST,
        "DELETE" => reqwest::Method::DELETE,
        _ => {
            return write_status(write, 405, request.keep_alive)
                .await
                .map(|_| request.keep_alive)
        }
    };
    let listed: Vec<String> = request
        .headers
        .iter()
        .filter(|(name, _)| name == "connection")
        .flat_map(|(_, value)| {
            String::from_utf8_lossy(value)
                .split(',')
                .map(|item| item.trim().to_ascii_lowercase())
                .collect::<Vec<_>>()
        })
        .collect();
    let mut headers = reqwest::header::HeaderMap::new();
    for (name, value) in &request.headers {
        if hop_by_hop(name, &listed)
            || matches!(
                name.as_str(),
                "authorization" | "origin" | "accept-encoding"
            )
        {
            continue;
        }
        let (Ok(name), Ok(value)) = (
            reqwest::header::HeaderName::from_bytes(name.as_bytes()),
            reqwest::header::HeaderValue::from_bytes(value),
        ) else {
            return write_status(write, 400, false).await.map(|_| false);
        };
        headers.append(name, value);
    }
    let mut rejected: Option<(String, reqwest::Response)> = None;
    let response = loop {
        let upstream = match (shared.resolver)(
            route.clone(),
            rejected.as_ref().map(|(bearer, _)| bearer.clone()),
        )
        .await
        {
            Ok(upstream) => upstream,
            // The renewal failed: the adapter sees the upstream refusal.
            Err(_) if rejected.is_some() => break rejected.take().unwrap().1,
            Err(status) => {
                return write_status(write, status, request.keep_alive)
                    .await
                    .map(|_| request.keep_alive)
            }
        };
        if rejected
            .as_ref()
            .is_some_and(|(bearer, _)| *bearer == upstream.bearer)
        {
            // Nothing new to try: never replay with the refused bearer.
            break rejected.take().unwrap().1;
        }
        let url = match query {
            Some(query) if upstream.url.contains('?') => format!("{}&{query}", upstream.url),
            Some(query) => format!("{}?{query}", upstream.url),
            None => upstream.url.clone(),
        };
        let Ok(mut bearer) =
            reqwest::header::HeaderValue::from_str(&format!("Bearer {}", upstream.bearer))
        else {
            return write_status(write, 503, request.keep_alive)
                .await
                .map(|_| request.keep_alive);
        };
        bearer.set_sensitive(true);
        let mut outgoing = headers.clone();
        outgoing.insert(reqwest::header::AUTHORIZATION, bearer);
        let mut builder = shared.client.request(method.clone(), url).headers(outgoing);
        if method != reqwest::Method::GET || !request.body.is_empty() {
            builder = builder.body(request.body.clone());
        }
        let response = match builder.send().await {
            Ok(response) => response,
            Err(_) => {
                return write_status(write, 502, request.keep_alive)
                    .await
                    .map(|_| request.keep_alive)
            }
        };
        if response.status() == reqwest::StatusCode::UNAUTHORIZED && rejected.is_none() {
            rejected = Some((upstream.bearer, response));
            continue;
        }
        break response;
    };
    write_response(write, response, request.keep_alive).await
}

async fn write_response<W: AsyncWrite + Unpin>(
    write: &mut W,
    mut response: reqwest::Response,
    keep_alive: bool,
) -> std::io::Result<bool> {
    let status = response.status();
    let mut head = format!(
        "HTTP/1.1 {} {}\r\n",
        status.as_u16(),
        status.canonical_reason().unwrap_or("Status")
    )
    .into_bytes();
    let listed: Vec<String> = response
        .headers()
        .get_all(reqwest::header::CONNECTION)
        .iter()
        .flat_map(|value| {
            String::from_utf8_lossy(value.as_bytes())
                .split(',')
                .map(|item| item.trim().to_ascii_lowercase())
                .collect::<Vec<_>>()
        })
        .collect();
    for (name, value) in response.headers() {
        if hop_by_hop(name.as_str(), &listed) {
            continue;
        }
        head.extend_from_slice(name.as_str().as_bytes());
        head.extend_from_slice(b": ");
        head.extend_from_slice(value.as_bytes());
        head.extend_from_slice(b"\r\n");
    }
    let bodiless = status.is_informational()
        || status == reqwest::StatusCode::NO_CONTENT
        || status == reqwest::StatusCode::NOT_MODIFIED;
    if !bodiless {
        head.extend_from_slice(b"transfer-encoding: chunked\r\n");
    }
    if !keep_alive {
        head.extend_from_slice(b"connection: close\r\n");
    }
    head.extend_from_slice(b"\r\n");
    write.write_all(&head).await?;
    write.flush().await?;
    if bodiless {
        return Ok(keep_alive);
    }
    loop {
        match response.chunk().await {
            Ok(Some(chunk)) if chunk.is_empty() => {}
            Ok(Some(chunk)) => {
                write
                    .write_all(format!("{:x}\r\n", chunk.len()).as_bytes())
                    .await?;
                write.write_all(&chunk).await?;
                write.write_all(b"\r\n").await?;
                // Each SSE event reaches the adapter as soon as it arrives.
                write.flush().await?;
            }
            Ok(None) => break,
            // A truncated upstream body must not look complete: close
            // without the terminating chunk.
            Err(_) => return Ok(false),
        }
    }
    write.write_all(b"0\r\n\r\n").await?;
    write.flush().await?;
    Ok(keep_alive)
}

async fn write_status<W: AsyncWrite + Unpin>(
    write: &mut W,
    status: u16,
    keep_alive: bool,
) -> std::io::Result<()> {
    let reason = reqwest::StatusCode::from_u16(status)
        .ok()
        .and_then(|status| status.canonical_reason())
        .unwrap_or("Error");
    let connection = if keep_alive {
        ""
    } else {
        "connection: close\r\n"
    };
    write
        .write_all(
            format!(
                "HTTP/1.1 {status} {reason}\r\ncontent-type: text/plain; charset=utf-8\r\ncontent-length: {}\r\n{connection}\r\n{reason}",
                reason.len()
            )
            .as_bytes(),
        )
        .await?;
    write.flush().await
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::sync::Mutex;
    use tokio::sync::Notify;

    #[derive(Clone, Debug)]
    struct Seen {
        method: String,
        path: String,
        headers: Vec<(String, String)>,
        body: Vec<u8>,
    }

    impl Seen {
        fn header(&self, name: &str) -> Option<&str> {
            self.headers
                .iter()
                .find(|(key, _)| key == name)
                .map(|(_, value)| value.as_str())
        }
    }

    struct Reply {
        status: u16,
        headers: Vec<(&'static str, String)>,
        /// Each chunk is written after its gate, if any, is notified.
        chunks: Vec<(Option<Arc<Notify>>, String)>,
    }

    struct Upstream {
        url: String,
        seen: Arc<Mutex<Vec<Seen>>>,
        task: tokio::task::JoinHandle<()>,
    }

    impl Drop for Upstream {
        fn drop(&mut self) {
            self.task.abort();
        }
    }

    /// Simulated Tutor MCP endpoint: raw HTTP/1.1, chunked responses.
    async fn upstream(reply: impl Fn(&Seen) -> Reply + Send + Sync + 'static) -> Upstream {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let url = format!("http://{}/mcp", listener.local_addr().unwrap());
        let seen = Arc::new(Mutex::new(Vec::new()));
        let captured = seen.clone();
        let reply = Arc::new(reply);
        let task = tokio::spawn(async move {
            loop {
                let (stream, _) = listener.accept().await.unwrap();
                let captured = captured.clone();
                let reply = reply.clone();
                tokio::spawn(async move {
                    let (read, mut write) = stream.into_split();
                    let mut reader = BufReader::new(read);
                    while let Ok(Some(request)) = read_request(&mut reader).await {
                        let request = Seen {
                            method: request.method,
                            path: request.target,
                            headers: request
                                .headers
                                .into_iter()
                                .map(|(name, value)| (name, String::from_utf8(value).unwrap()))
                                .collect(),
                            body: request.body,
                        };
                        captured.lock().unwrap().push(request.clone());
                        let answer = reply(&request);
                        let mut head = format!("HTTP/1.1 {} X\r\n", answer.status);
                        for (name, value) in &answer.headers {
                            head.push_str(&format!("{name}: {value}\r\n"));
                        }
                        head.push_str("transfer-encoding: chunked\r\n\r\n");
                        if write.write_all(head.as_bytes()).await.is_err() {
                            return;
                        }
                        let _ = write.flush().await;
                        for (gate, chunk) in answer.chunks {
                            if let Some(gate) = gate {
                                gate.notified().await;
                            }
                            let frame = format!("{:x}\r\n{chunk}\r\n", chunk.len());
                            if write.write_all(frame.as_bytes()).await.is_err() {
                                return;
                            }
                            let _ = write.flush().await;
                        }
                        let _ = write.write_all(b"0\r\n\r\n").await;
                        let _ = write.flush().await;
                    }
                });
            }
        });
        Upstream { url, seen, task }
    }

    /// Resolver double: "stale" until the stale bearer is reported refused,
    /// then "fresh", or always `fixed` when set.
    fn resolver(url: String, fixed: Option<&'static str>, renewals: Arc<AtomicUsize>) -> Resolver {
        let current = Arc::new(Mutex::new("stale".to_string()));
        Arc::new(move |route, rejected| {
            let url = url.clone();
            let current = current.clone();
            let renewals = renewals.clone();
            Box::pin(async move {
                if route != "abc123" {
                    return Err(403);
                }
                if let Some(bearer) = fixed {
                    return Ok(super::Upstream {
                        url,
                        bearer: bearer.into(),
                    });
                }
                let mut current = current.lock().unwrap();
                if rejected.as_deref() == Some(current.as_str()) {
                    renewals.fetch_add(1, Ordering::SeqCst);
                    *current = "fresh".into();
                }
                Ok(super::Upstream {
                    url,
                    bearer: current.clone(),
                })
            })
        })
    }

    fn client() -> reqwest::Client {
        reqwest::Client::builder().build().unwrap()
    }

    #[tokio::test]
    async fn a_refused_bearer_is_renewed_once_and_the_sse_stream_is_relayed_live() {
        let second_event = Arc::new(Notify::new());
        let gate = second_event.clone();
        let upstream = upstream(move |request| {
            if request.header("authorization") != Some("Bearer fresh") {
                return Reply {
                    status: 401,
                    headers: vec![("www-authenticate", "Bearer".into())],
                    chunks: vec![(None, "expired".into())],
                };
            }
            Reply {
                status: 200,
                headers: vec![
                    ("content-type", "text/event-stream".into()),
                    ("mcp-session-id", "upstream-session".into()),
                ],
                chunks: vec![
                    (None, "event: message\ndata: {\"id\":1}\n\n".into()),
                    (
                        Some(gate.clone()),
                        "event: message\ndata: {\"id\":2}\n\n".into(),
                    ),
                ],
            }
        })
        .await;
        let renewals = Arc::new(AtomicUsize::new(0));
        let proxy = McpProxy::start(resolver(upstream.url.clone(), None, renewals.clone()))
            .await
            .unwrap();
        let body = r#"{"jsonrpc":"2.0","id":1,"method":"tools/list"}"#;
        let mut response = client()
            .post(proxy.endpoint("abc123"))
            .header("Authorization", proxy.authorization())
            .header("Accept", "application/json, text/event-stream")
            .header("Content-Type", "application/json")
            .header("Mcp-Protocol-Version", "2025-06-18")
            .header("Mcp-Session-Id", "upstream-session")
            .header("Tutor-Conversation-ID", "conversation-1")
            .body(body)
            .send()
            .await
            .unwrap();
        assert_eq!(response.status(), 200);
        assert_eq!(response.headers()["mcp-session-id"], "upstream-session");
        assert_eq!(response.headers()["content-type"], "text/event-stream");
        // The first event arrives while the upstream still holds the second.
        let first = tokio::time::timeout(std::time::Duration::from_secs(5), response.chunk())
            .await
            .unwrap()
            .unwrap()
            .unwrap();
        assert_eq!(&first[..], b"event: message\ndata: {\"id\":1}\n\n");
        second_event.notify_one();
        let mut rest = Vec::new();
        while let Some(chunk) = response.chunk().await.unwrap() {
            rest.extend_from_slice(&chunk);
        }
        assert_eq!(rest, b"event: message\ndata: {\"id\":2}\n\n");
        assert_eq!(renewals.load(Ordering::SeqCst), 1);
        let seen = upstream.seen.lock().unwrap().clone();
        assert_eq!(seen.len(), 2, "one refused request and one replay");
        assert_eq!(seen[0].header("authorization"), Some("Bearer stale"));
        assert_eq!(seen[1].header("authorization"), Some("Bearer fresh"));
        for request in &seen {
            assert_eq!(request.method, "POST");
            assert_eq!(request.path, "/mcp");
            assert_eq!(request.body, body.as_bytes());
            assert_eq!(request.header("mcp-session-id"), Some("upstream-session"));
            assert_eq!(request.header("mcp-protocol-version"), Some("2025-06-18"));
            assert_eq!(
                request.header("tutor-conversation-id"),
                Some("conversation-1")
            );
            assert_eq!(
                request.header("accept"),
                Some("application/json, text/event-stream")
            );
            assert!(!request
                .headers
                .iter()
                .any(|(_, value)| value.contains(&proxy.token)));
        }
    }

    #[tokio::test]
    async fn an_unrenewable_401_and_upstream_errors_reach_the_adapter_unchanged() {
        let upstream = upstream(|request| match request.method.as_str() {
            "DELETE" => Reply {
                status: 404,
                headers: vec![("content-type", "application/json".into())],
                chunks: vec![(None, "{\"error\":\"unknown session\"}".into())],
            },
            "GET" => Reply {
                status: 405,
                headers: vec![("allow", "POST, DELETE".into())],
                chunks: vec![],
            },
            _ => Reply {
                status: 401,
                headers: vec![("www-authenticate", "Bearer error=\"invalid_token\"".into())],
                chunks: vec![(None, "refused".into())],
            },
        })
        .await;
        let renewals = Arc::new(AtomicUsize::new(0));
        let proxy = McpProxy::start(resolver(upstream.url.clone(), Some("same"), renewals))
            .await
            .unwrap();
        let response = client()
            .post(proxy.endpoint("abc123"))
            .header("Authorization", proxy.authorization())
            .body("{}")
            .send()
            .await
            .unwrap();
        assert_eq!(response.status(), 401);
        assert_eq!(
            response.headers()["www-authenticate"],
            "Bearer error=\"invalid_token\""
        );
        assert_eq!(response.text().await.unwrap(), "refused");
        assert_eq!(upstream.seen.lock().unwrap().len(), 1, "never replayed");
        let response = client()
            .delete(format!("{}?trace=1", proxy.endpoint("abc123")))
            .header("Authorization", proxy.authorization())
            .header("Mcp-Session-Id", "gone")
            .send()
            .await
            .unwrap();
        assert_eq!(response.status(), 404);
        assert_eq!(
            response.text().await.unwrap(),
            "{\"error\":\"unknown session\"}"
        );
        let response = client()
            .get(proxy.endpoint("abc123"))
            .header("Authorization", proxy.authorization())
            .header("Accept", "text/event-stream")
            .header("Last-Event-ID", "41")
            .send()
            .await
            .unwrap();
        assert_eq!(response.status(), 405);
        assert_eq!(response.headers()["allow"], "POST, DELETE");
        let seen = upstream.seen.lock().unwrap().clone();
        assert_eq!(seen[1].path, "/mcp?trace=1");
        assert_eq!(seen[1].header("mcp-session-id"), Some("gone"));
        assert_eq!(seen[2].header("last-event-id"), Some("41"));
    }

    #[tokio::test]
    async fn the_relay_refuses_foreign_callers_and_unknown_routes_without_reaching_tutor() {
        let upstream = upstream(|_| Reply {
            status: 200,
            headers: vec![],
            chunks: vec![(None, "ok".into())],
        })
        .await;
        let proxy = McpProxy::start(resolver(
            upstream.url.clone(),
            Some("token"),
            Arc::new(AtomicUsize::new(0)),
        ))
        .await
        .unwrap();
        let local = proxy.authorization();
        let cases = [
            (proxy.endpoint("abc123"), None, None, 403),
            (proxy.endpoint("abc123"), Some("Bearer guess"), None, 403),
            (
                proxy.endpoint("abc123"),
                Some(local.as_str()),
                Some("https://evil.example"),
                403,
            ),
            (proxy.endpoint("ABC"), Some(local.as_str()), None, 404),
            // A valid route that no longer matches the connected account.
            (proxy.endpoint("def456"), Some(local.as_str()), None, 403),
        ];
        for (url, authorization, origin, status) in cases {
            let mut request = client().post(url).body("{}");
            if let Some(authorization) = authorization {
                request = request.header("Authorization", authorization);
            }
            if let Some(origin) = origin {
                request = request.header("Origin", origin);
            }
            assert_eq!(request.send().await.unwrap().status(), status);
        }
        assert!(upstream.seen.lock().unwrap().is_empty());
    }

    #[tokio::test]
    async fn chunked_requests_and_kept_alive_connections_are_relayed_in_order() {
        let upstream = upstream(|request| Reply {
            status: 200,
            headers: vec![("content-type", "application/json".into())],
            chunks: vec![(None, String::from_utf8(request.body.clone()).unwrap())],
        })
        .await;
        let proxy = McpProxy::start(resolver(
            upstream.url.clone(),
            Some("token"),
            Arc::new(AtomicUsize::new(0)),
        ))
        .await
        .unwrap();
        let mut stream = TcpStream::connect(proxy.address).await.unwrap();
        let host = format!("127.0.0.1:{}", proxy.address.port());
        let first = format!(
            "POST /mcp/abc123 HTTP/1.1\r\nHost: {host}\r\nAuthorization: {}\r\nTransfer-Encoding: chunked\r\n\r\n5\r\n{{\"a\":\r\n2\r\n1}}\r\n0\r\n\r\n",
            proxy.authorization()
        );
        let second = format!(
            "POST /mcp/abc123 HTTP/1.1\r\nHost: {host}\r\nAuthorization: {}\r\nContent-Length: 7\r\nConnection: close\r\n\r\n{{\"b\":2}}",
            proxy.authorization()
        );
        stream
            .write_all(format!("{first}{second}").as_bytes())
            .await
            .unwrap();
        let mut output = String::new();
        tokio::time::timeout(
            std::time::Duration::from_secs(5),
            stream.read_to_string(&mut output),
        )
        .await
        .unwrap()
        .unwrap();
        assert_eq!(output.matches("HTTP/1.1 200 OK").count(), 2, "{output}");
        let first_body = output.find("7\r\n{\"a\":1}\r\n0\r\n\r\n").unwrap();
        let second_body = output.find("7\r\n{\"b\":2}\r\n0\r\n\r\n").unwrap();
        assert!(first_body < second_body);
        let seen = upstream.seen.lock().unwrap().clone();
        assert_eq!(seen[0].body, b"{\"a\":1}");
        assert_eq!(seen[1].body, b"{\"b\":2}");
        assert!(seen
            .iter()
            .all(|request| request.header("authorization") == Some("Bearer token")));
    }

    /// Raw exchange with the relay: everything it writes before closing.
    async fn exchange(proxy: &McpProxy, request: &[u8]) -> String {
        let mut stream = TcpStream::connect(proxy.address).await.unwrap();
        stream.write_all(request).await.unwrap();
        let mut output = Vec::new();
        tokio::time::timeout(
            std::time::Duration::from_secs(5),
            stream.read_to_end(&mut output),
        )
        .await
        .expect("the relay must answer without waiting for the body")
        .unwrap();
        String::from_utf8_lossy(&output).into_owned()
    }

    #[tokio::test]
    async fn an_unauthenticated_head_is_refused_before_its_body_is_read() {
        let upstream = upstream(|_| Reply {
            status: 200,
            headers: vec![],
            chunks: vec![(None, "ok".into())],
        })
        .await;
        let proxy = McpProxy::start(resolver(
            upstream.url.clone(),
            Some("token"),
            Arc::new(AtomicUsize::new(0)),
        ))
        .await
        .unwrap();
        let host = format!("127.0.0.1:{}", proxy.address.port());
        // Announced bodies are never sent: a refusal that waited for them
        // would time out.
        for framing in [
            format!("Content-Length: {MAX_BODY}"),
            "Transfer-Encoding: chunked".into(),
        ] {
            for authorization in ["", "Authorization: Bearer guess\r\n"] {
                let output = exchange(
                    &proxy,
                    format!("POST /mcp/abc123 HTTP/1.1\r\nHost: {host}\r\n{authorization}{framing}\r\n\r\n")
                        .as_bytes(),
                )
                .await;
                assert!(output.starts_with("HTTP/1.1 403 "), "{output}");
                assert!(output.contains("connection: close"), "{output}");
            }
        }
        assert!(upstream.seen.lock().unwrap().is_empty());
    }

    #[tokio::test]
    async fn oversized_or_overflowing_chunk_sizes_are_refused_without_panicking() {
        for size in [
            "ffffffffffffffff",
            "fffffffffffffffffffffff",
            &format!("{:x}", MAX_BODY + 1),
        ] {
            let request = format!("POST /mcp/abc123 HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n\r\n1\r\na\r\n{size}\r\n");
            assert_eq!(
                read_request(&mut request.as_bytes()).await.err(),
                Some(413),
                "{size}"
            );
        }
        for size in ["+5", "-1", "", "zz"] {
            let request = format!("POST /mcp/abc123 HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n\r\n{size}\r\n");
            assert_eq!(
                read_request(&mut request.as_bytes()).await.err(),
                Some(400),
                "{size:?}"
            );
        }
        let request = "POST /mcp/abc123 HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n\r\n003\r\nabc\r\n0\r\n\r\n";
        assert_eq!(
            read_request(&mut request.as_bytes())
                .await
                .unwrap()
                .unwrap()
                .body,
            b"abc"
        );
    }

    #[tokio::test]
    async fn idle_connections_and_stalled_bodies_are_closed() {
        let upstream = upstream(|_| Reply {
            status: 200,
            headers: vec![],
            chunks: vec![(None, "ok".into())],
        })
        .await;
        let quick = std::time::Duration::from_millis(200);
        let proxy = McpProxy::start_with(
            resolver(
                upstream.url.clone(),
                Some("token"),
                Arc::new(AtomicUsize::new(0)),
            ),
            quick,
            quick,
        )
        .await
        .unwrap();
        let host = format!("127.0.0.1:{}", proxy.address.port());
        // An idle connection, then a head that never completes, are closed.
        assert_eq!(exchange(&proxy, b"").await, "");
        assert_eq!(exchange(&proxy, b"POST /mcp/abc123 HTTP/1.1\r\n").await, "");
        // An admitted head whose body stalls receives 408 and is closed.
        let output = exchange(
            &proxy,
            format!(
                "POST /mcp/abc123 HTTP/1.1\r\nHost: {host}\r\nAuthorization: {}\r\nContent-Length: 10\r\n\r\n{{}}",
                proxy.authorization()
            )
            .as_bytes(),
        )
        .await;
        assert!(output.starts_with("HTTP/1.1 408 "), "{output}");
        assert!(upstream.seen.lock().unwrap().is_empty());
    }
}
