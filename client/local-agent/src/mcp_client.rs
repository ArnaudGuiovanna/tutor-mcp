// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! A single serial MCP connection. Configuration comes from the ACP host;
//! neither a model response nor a tool result can supply a command or URL.
//! Observations measure wrapper operations, not remote pedagogical success.

use crate::{catalog::ToolEntry, mcp, mcp_stdio};
use serde::Serialize;
use serde_json::Value;
use std::{path::Path, sync::atomic::AtomicBool, time::Instant};

// Repository callers use connect, not enum variants. Keep transport details
// private so the observation is owned by the same serial connection.
pub struct Client {
    inner: Inner,
    observation: McpObservation,
}

enum Inner {
    Http(mcp::McpClient),
    Stdio(mcp_stdio::StdioClient),
}

/// Non-consuming transport facts. No authorization, payload or configuration.
/// notification_revision changes ONLY on real tools/list_changed messages;
/// post_call_revision changes on the conservative tools/call boundary.
#[derive(Clone, Copy)]
pub(crate) struct CatalogState {
    pub notification_revision: u64,
    pub post_call_revision: u64,
    pub ready: bool,
    pub notifications_supported: bool,
    pub idle_notifications_observable: bool,
}

/// Exportable cache metadata only; never contains schemas, tools or scope IDs.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum CatalogMissReason {
    Disabled,
    Cold,
    ScopeChanged,
    Expired,
    RealNotification,
    UnreviewedCall,
    ToolError,
    UnusableTransport,
    IdleNotificationsUnobservable,
    CapabilityMissing,
    PermissionBoundary,
    FetchChanged,
    FetchFailure,
}

#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct CatalogObservation {
    pub schema_version: u32,
    pub cache_hits: u64,
    pub cache_misses: u64,
    pub last_miss_reason: Option<CatalogMissReason>,
}

impl Default for CatalogObservation {
    fn default() -> Self {
        Self {
            schema_version: 1,
            cache_hits: 0,
            cache_misses: 0,
            last_miss_reason: None,
        }
    }
}

/// Schema 1, aggregate monotonic elapsed times in milliseconds. No payloads,
/// names, credentials, paths, URLs or private identifiers are retained here.
///
/// `None` means not observed; `Some(0.0)` would mean measured as zero. Totals
/// include returned errors and interrupted futures. The wrappers cannot split
/// initialize from connect, count catalog pages, or time internal cleanup.
/// close_total_ms covers explicit close only, not cleanup in call/connect or
/// Client::drop. Capture before dropping the Client; a dropped whole Client
/// cannot expose its observation after the fact.
///
/// Attempts count *polled wrapper invocations*, including a call rejected by
/// the transport before sending. They are not a count of wire requests.
/// Transport successes count `Result::Ok`, including `isError: true` envelopes;
/// `tool_result_errors` counts those envelopes separately. Neither counter
/// establishes a successful business operation or a pedagogical outcome.
/// Interrupted futures have no returned Result and a separate counter.
#[derive(Clone, Debug, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct McpObservation {
    pub schema_version: u32,
    pub connect_total_ms: Option<f64>,
    pub initialize_total_ms: Option<f64>,
    pub catalog_total_ms: Option<f64>,
    pub catalog_pages: Option<u64>,
    pub tools_call_total_ms: Option<f64>,
    pub close_total_ms: Option<f64>,
    pub catalog_attempts: u64,
    pub catalog_transport_successes: u64,
    pub catalog_transport_failures: u64,
    pub catalog_interrupted: u64,
    pub call_attempts: u64,
    pub call_transport_successes: u64,
    pub call_transport_failures: u64,
    pub call_interrupted: u64,
    pub tool_result_errors: u64,
    pub close_attempts: u64,
    pub close_transport_successes: u64,
    pub close_transport_failures: u64,
    pub close_interrupted: u64,
}

impl Default for McpObservation {
    fn default() -> Self {
        Self {
            schema_version: 1,
            connect_total_ms: None,
            initialize_total_ms: None,
            catalog_total_ms: None,
            catalog_pages: None,
            tools_call_total_ms: None,
            close_total_ms: None,
            catalog_attempts: 0,
            catalog_transport_successes: 0,
            catalog_transport_failures: 0,
            catalog_interrupted: 0,
            call_attempts: 0,
            call_transport_successes: 0,
            call_transport_failures: 0,
            call_interrupted: 0,
            tool_result_errors: 0,
            close_attempts: 0,
            close_transport_successes: 0,
            close_transport_failures: 0,
            close_interrupted: 0,
        }
    }
}

#[derive(Clone, Copy)]
enum Phase {
    Catalog,
    Call,
    Close,
}

enum Outcome {
    Interrupted,
    Returned { success: bool, tool_error: bool },
}

// A dropped future still records its local observed interval, without claiming
// a transport failure, undoing a remote mutation, or changing cancellation.
struct ObservationSpan<'a> {
    observation: &'a mut McpObservation,
    phase: Phase,
    start: Instant,
    outcome: Outcome,
}

impl<'a> ObservationSpan<'a> {
    fn begin(observation: &'a mut McpObservation, phase: Phase) -> Self {
        let attempts = match phase {
            Phase::Catalog => &mut observation.catalog_attempts,
            Phase::Call => &mut observation.call_attempts,
            Phase::Close => &mut observation.close_attempts,
        };
        *attempts = attempts.saturating_add(1);
        Self {
            observation,
            phase,
            start: Instant::now(),
            outcome: Outcome::Interrupted,
        }
    }

    fn finish(mut self, success: bool, tool_error: bool) {
        self.outcome = Outcome::Returned {
            success,
            tool_error,
        };
    }
}

impl Drop for ObservationSpan<'_> {
    fn drop(&mut self) {
        let elapsed_ms = self.start.elapsed().as_secs_f64() * 1000.0;
        let (total, successes, failures, interrupted) = match self.phase {
            Phase::Catalog => (
                &mut self.observation.catalog_total_ms,
                &mut self.observation.catalog_transport_successes,
                &mut self.observation.catalog_transport_failures,
                &mut self.observation.catalog_interrupted,
            ),
            Phase::Call => (
                &mut self.observation.tools_call_total_ms,
                &mut self.observation.call_transport_successes,
                &mut self.observation.call_transport_failures,
                &mut self.observation.call_interrupted,
            ),
            Phase::Close => (
                &mut self.observation.close_total_ms,
                &mut self.observation.close_transport_successes,
                &mut self.observation.close_transport_failures,
                &mut self.observation.close_interrupted,
            ),
        };
        *total = Some(total.unwrap_or(0.0) + elapsed_ms);
        match self.outcome {
            Outcome::Interrupted => *interrupted = interrupted.saturating_add(1),
            Outcome::Returned { success: false, .. } => {
                *failures = failures.saturating_add(1);
            }
            Outcome::Returned {
                success: true,
                tool_error,
            } => {
                *successes = successes.saturating_add(1);
                if matches!(self.phase, Phase::Call) && tool_error {
                    self.observation.tool_result_errors =
                        self.observation.tool_result_errors.saturating_add(1);
                }
            }
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Error {
    InvalidConfig,
    Connection,
    Catalog,
    Call,
    Close,
}
impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            Self::InvalidConfig => "mcp_configuration_invalid",
            Self::Connection => "mcp_connection_failed",
            Self::Catalog => "mcp_catalog_failed",
            Self::Call => "mcp_call_outcome_unknown",
            Self::Close => "mcp_close_failed",
        })
    }
}
impl std::error::Error for Error {}

pub trait Transport {
    fn server_id(&self) -> &str;
    fn call_tool<'a>(
        &'a mut self,
        name: &'a str,
        args: Value,
        key: Option<&'a str>,
        cancel: &'a AtomicBool,
    ) -> impl std::future::Future<Output = Result<Value, Error>> + Send + 'a;
}

impl Client {
    /// Includes config validation, transport setup and initialize/initialized.
    /// On Err there is no Client to harvest: the caller must time this await
    /// externally to retain connect duration on that path. Errors stay Copy
    /// and unchanged; no timing is invented for their unobservable subphases.
    pub async fn connect(config: &Value, cwd: &Path, cancel: &AtomicBool) -> Result<Self, Error> {
        let start = Instant::now();
        let inner = match config.get("type").and_then(Value::as_str) {
            Some("http") => mcp::McpClient::connect(
                mcp::ServerConfig::from_acp(config).map_err(|_| Error::InvalidConfig)?,
                cancel,
            )
            .await
            .map(Inner::Http)
            .map_err(|_| Error::Connection),
            None | Some("stdio") => mcp_stdio::StdioClient::connect(
                mcp_stdio::StdioConfig::from_acp(config, cwd).map_err(|_| Error::InvalidConfig)?,
                cancel,
            )
            .await
            .map(Inner::Stdio)
            .map_err(|_| Error::Connection),
            _ => Err(Error::InvalidConfig),
        }?;
        Ok(Self {
            inner,
            observation: McpObservation {
                connect_total_ms: Some(start.elapsed().as_secs_f64() * 1000.0),
                ..McpObservation::default()
            },
        })
    }

    /// Drain the current observation, including completed errors and explicit
    /// close, then reset to a fresh schema-1 observation. This never closes or
    /// retries the transport. Harvest before dropping the Client.
    pub fn take_observation(&mut self) -> McpObservation {
        std::mem::take(&mut self.observation)
    }

    /// Begin a new observation interval and return the previous one so that
    /// connection costs or earlier errors are not silently discarded.
    pub fn begin_turn_observation(&mut self) -> McpObservation {
        self.take_observation()
    }

    pub async fn list_tools(&mut self, cancel: &AtomicBool) -> Result<Vec<ToolEntry>, Error> {
        let span = ObservationSpan::begin(&mut self.observation, Phase::Catalog);
        let result = match &mut self.inner {
            Inner::Http(client) => client.list_tools(cancel).await.map_err(|_| Error::Catalog),
            Inner::Stdio(client) => client.list_tools(cancel).await.map_err(|_| Error::Catalog),
        };
        span.finish(result.is_ok(), false);
        result
    }

    pub fn take_tools_changed(&mut self) -> bool {
        match &mut self.inner {
            Inner::Http(client) => client.take_tools_changed(),
            Inner::Stdio(client) => client.take_tools_changed(),
        }
    }

    pub(crate) fn catalog_state(&self) -> CatalogState {
        match &self.inner {
            Inner::Http(client) => client.catalog_state(),
            Inner::Stdio(client) => client.catalog_state(),
        }
    }

    pub async fn close(&mut self) -> Result<(), Error> {
        let span = ObservationSpan::begin(&mut self.observation, Phase::Close);
        let result = match &mut self.inner {
            Inner::Http(client) => {
                client.close();
                Ok(())
            }
            Inner::Stdio(client) => client.close().await.map_err(|_| Error::Close),
        };
        span.finish(result.is_ok(), false);
        result
    }
}
impl Transport for Client {
    fn server_id(&self) -> &str {
        match &self.inner {
            Inner::Http(client) => client.server_id(),
            Inner::Stdio(client) => client.server_id(),
        }
    }

    async fn call_tool(
        &mut self,
        name: &str,
        args: Value,
        key: Option<&str>,
        cancel: &AtomicBool,
    ) -> Result<Value, Error> {
        let span = ObservationSpan::begin(&mut self.observation, Phase::Call);
        let result = match &mut self.inner {
            Inner::Http(client) => client
                .call_tool(name, args, key, cancel)
                .await
                .map_err(|_| Error::Call),
            Inner::Stdio(client) => client
                .call_tool(name, args, key, cancel)
                .await
                .map_err(|_| Error::Call),
        };
        let tool_error = result
            .as_ref()
            .is_ok_and(|value| value.get("isError").and_then(Value::as_bool) == Some(true));
        span.finish(result.is_ok(), tool_error);
        result
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::mcp::tests::{initialization, mock, Reply};
    use serde_json::json;
    use std::sync::{atomic::Ordering, Arc, Condvar, Mutex};
    use std::time::Duration;

    fn config(url: &str) -> Value {
        json!({"type":"http","name":"tutor","url":url,"headers":[
            {"name":"Authorization","value":"Bearer fixture-secret-not-metrics"},
            {"name":"Tutor-Conversation-ID","value":"fixture-private-conversation"}
        ]})
    }

    fn assert_private_free(observation: &McpObservation) {
        let value = serde_json::to_value(observation).unwrap();
        assert_eq!(value["schemaVersion"], 1);
        // Every exported value is numeric/null. No unbounded or private text
        // can be introduced by error bodies, tool names, args or credentials.
        assert!(value
            .as_object()
            .unwrap()
            .values()
            .all(|value| value.is_null() || value.is_number()));
        assert!(value["initializeTotalMs"].is_null());
        assert!(value["catalogPages"].is_null());
    }

    #[test]
    fn unobserved_phases_are_null_instead_of_zero() {
        let observation = McpObservation::default();
        assert_private_free(&observation);
        assert_eq!(observation.connect_total_ms, None);
        assert_eq!(observation.catalog_total_ms, None);
        assert_eq!(observation.tools_call_total_ms, None);
        assert_eq!(observation.close_total_ms, None);
        assert_eq!(observation.call_attempts, 0);
    }

    #[tokio::test]
    async fn loopback_measures_pagination_and_rpc_results_without_business_success_claims() {
        let server = mock(|request| {
            if let Some(reply) = initialization(request) {
                return reply;
            }
            let result = match request["method"].as_str().unwrap() {
                "tools/list" if request["params"].get("cursor").is_none() => {
                    json!({"tools":[],"nextCursor":"fixture-private-cursor"})
                }
                "tools/list" => {
                    assert_eq!(request["params"]["cursor"], "fixture-private-cursor");
                    json!({"tools":[]})
                }
                "tools/call" => {
                    assert_eq!(request["params"]["arguments"]["private"], "fixture-argument");
                    assert_eq!(request["params"]["_meta"]["idempotency_key"], "fixture-key");
                    match request["params"]["name"].as_str().unwrap() {
                        "fixture_business_error" => {
                            json!({"content":[],"isError":true,"structuredContent":{"error":"fixture-private-result"}})
                        }
                        "fixture_rpc_error" => {
                            return Reply::json(json!({"jsonrpc":"2.0","id":request["id"],
                                "error":{"code":-32000,"message":"fixture-private-error"}}));
                        }
                        _ => json!({"content":[],"isError":false}),
                    }
                }
                _ => panic!("unexpected method"),
            };
            Reply::json(json!({"jsonrpc":"2.0","id":request["id"],"result":result}))
        })
        .await;
        let cancel = AtomicBool::new(false);
        let mut client = Client::connect(&config(&server.url), Path::new("."), &cancel)
            .await
            .unwrap();
        assert_eq!(client.server_id(), "tutor");
        assert!(client.take_tools_changed());
        assert!(!client.take_tools_changed());
        assert!(client.list_tools(&cancel).await.unwrap().is_empty());
        assert!(!client.take_tools_changed());
        for name in ["fixture_ok", "fixture_business_error", "fixture_rpc_error"] {
            let result = client
                .call_tool(
                    name,
                    json!({"private":"fixture-argument"}),
                    Some("fixture-key"),
                    &cancel,
                )
                .await;
            if name == "fixture_rpc_error" {
                assert_eq!(result, Err(Error::Call));
            } else {
                let value = result.unwrap();
                assert_eq!(value["isError"], name == "fixture_business_error");
            }
            assert!(client.take_tools_changed());
        }
        client.close().await.unwrap();
        let observation = client.take_observation();
        assert_private_free(&observation);
        assert!(observation.connect_total_ms.unwrap() > 0.0);
        assert!(observation.catalog_total_ms.unwrap() > 0.0);
        assert!(observation.tools_call_total_ms.unwrap() > 0.0);
        assert!(observation.close_total_ms.is_some());
        assert_eq!(observation.catalog_attempts, 1);
        assert_eq!(observation.catalog_transport_successes, 1);
        assert_eq!(observation.call_attempts, 3);
        assert_eq!(observation.call_transport_successes, 2);
        assert_eq!(observation.call_transport_failures, 1);
        assert_eq!(observation.call_interrupted, 0);
        assert_eq!(observation.tool_result_errors, 1);
        assert_eq!(observation.close_transport_successes, 1);
        let seen = server.observed.lock().unwrap();
        assert_eq!(
            seen.iter()
                .filter(|(r, _)| r["method"] == "tools/list")
                .count(),
            2
        );
        assert_eq!(
            seen.iter()
                .filter(|(r, _)| r["method"] == "tools/call")
                .count(),
            3
        );
        // We actually saw two catalog pages in the fixture, but the Client
        // cannot observe that detail: its serialized page count remains null.
        assert_eq!(observation.catalog_pages, None);
        assert_eq!(client.take_observation(), McpObservation::default());
    }

    #[tokio::test]
    async fn loopback_catalog_error_is_harvestable_after_close_without_retry() {
        let server = mock(|request| {
            initialization(request).unwrap_or_else(|| {
                Reply::json(json!({"jsonrpc":"2.0","id":request["id"],
                    "result":{"tools":"fixture-invalid-private-catalog"}}))
            })
        })
        .await;
        let cancel = AtomicBool::new(false);
        let mut client = Client::connect(&config(&server.url), Path::new("."), &cancel)
            .await
            .unwrap();
        let connection = client.begin_turn_observation();
        assert!(connection.connect_total_ms.is_some());
        assert_eq!(client.list_tools(&cancel).await.err(), Some(Error::Catalog));
        let closed = client.close().await;
        let observation = client.take_observation();
        assert!(closed.is_ok());
        assert_private_free(&observation);
        assert_eq!(observation.connect_total_ms, None);
        assert!(observation.catalog_total_ms.unwrap() > 0.0);
        assert_eq!(observation.catalog_attempts, 1);
        assert_eq!(observation.catalog_transport_successes, 0);
        assert_eq!(observation.catalog_transport_failures, 1);
        assert_eq!(observation.call_attempts, 0);
        assert_eq!(observation.tools_call_total_ms, None);
        assert_eq!(server.observed.lock().unwrap().len(), 3);
    }

    #[tokio::test]
    async fn cancellation_preserves_error_and_counts_invocation_without_claiming_a_send() {
        let server = mock(|request| initialization(request).expect("no tool call expected")).await;
        let cancel = AtomicBool::new(false);
        let mut client = Client::connect(&config(&server.url), Path::new("."), &cancel)
            .await
            .unwrap();
        cancel.store(true, Ordering::Release);
        assert_eq!(
            client.call_tool("fixture", json!({}), None, &cancel).await,
            Err(Error::Call)
        );
        client.close().await.unwrap();
        let observation = client.take_observation();
        assert_private_free(&observation);
        assert_eq!(observation.call_attempts, 1);
        assert_eq!(observation.call_transport_failures, 1);
        assert_eq!(observation.call_transport_successes, 0);
        assert_eq!(observation.tool_result_errors, 0);
        assert_eq!(server.observed.lock().unwrap().len(), 2);
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn dropping_inflight_call_keeps_interrupted_timing_and_never_replays() {
        let arrived = Arc::new(AtomicBool::new(false));
        let release = Arc::new((Mutex::new(false), Condvar::new()));
        let (signal, gate) = (arrived.clone(), release.clone());
        let server = mock(move |request| {
            if let Some(reply) = initialization(request) {
                return reply;
            }
            signal.store(true, Ordering::Release);
            let (lock, condition) = &*gate;
            // Bound the fixture's wait even if the test fails. The other
            // runtime worker remains available to drop the client future.
            drop(
                condition
                    .wait_timeout_while(lock.lock().unwrap(), Duration::from_secs(3), |done| !*done)
                    .unwrap(),
            );
            Reply::json(json!({"jsonrpc":"2.0","id":request["id"],"result":{"content":[]}}))
        })
        .await;
        let cancel = AtomicBool::new(false);
        let mut client = Client::connect(&config(&server.url), Path::new("."), &cancel)
            .await
            .unwrap();
        let mut call = Box::pin(client.call_tool("fixture", json!({}), None, &cancel));
        tokio::select! {
            _ = &mut call => panic!("fixture returned before release"),
            waited = tokio::time::timeout(Duration::from_secs(1), async {
                while !arrived.load(Ordering::Acquire) {
                    tokio::time::sleep(Duration::from_millis(1)).await;
                }
            }) => waited.expect("fixture did not receive tools/call"),
        }
        drop(call);
        let (lock, condition) = &*release;
        *lock.lock().unwrap() = true;
        condition.notify_one();
        let observation = client.take_observation();
        assert_private_free(&observation);
        assert!(observation.tools_call_total_ms.unwrap() > 0.0);
        assert_eq!(observation.call_attempts, 1);
        assert_eq!(observation.call_interrupted, 1);
        assert_eq!(observation.call_transport_successes, 0);
        assert_eq!(observation.call_transport_failures, 0);
        assert_eq!(observation.tool_result_errors, 0);
        // Existing HTTP readiness semantics prevent reuse after dropping a
        // pending request. Instrumentation neither reconnects nor retries it.
        assert_eq!(
            client.call_tool("fixture", json!({}), None, &cancel).await,
            Err(Error::Call)
        );
        client.close().await.unwrap();
        let after = client.take_observation();
        assert_eq!(after.call_attempts, 1);
        assert_eq!(after.call_transport_failures, 1);
        assert_eq!(after.close_transport_successes, 1);
        assert_eq!(server.observed.lock().unwrap().len(), 3);
    }
}
