// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! Resident connection and catalog snapshots behind opt-in policies.
//!
//! HOST-ONLY: Scope is built after the ACP Owner and HostContext are verified.
//! This module cannot turn model text into authority, grant permissions, issue
//! workflow permits, or replay a request. The existing executor stays in charge.
//!
//! Wiring contract for the parent:
//! - One Manager for the agent lifetime, not one Manager per turn.
//! - FreshEachLease preserves the shipped behavior. ReviewedStdio can reuse
//!   only an exact scoped/generation/TTL snapshot with observable notifications.
//!   Every call still requires the existing executor and ACP permission; a
//!   catalog hit is never an authorization grant. HTTP always fetches on list.
//! - invalidate synchronously on host authority/config changes; it advances the
//!   epoch before returning and schedules idle cleanup without awaiting IO.
//!   shutdown is asynchronous and must precede manager replacement.
//! - After a turn, release even on workflow failure; collect every observation.
//! - After abandoning a lease future, drain deferred observations before the
//!   next acquire (wait_ready does that atomically once cleanup is ready).
//!   Do not construct a new Manager to bypass quarantine.
//! - Keep a runtime alive for cleanup. Drop is the existing transport backstop,
//!   not proof that an OS process tree finished. No raw Client/Deref is exposed.
//!
//! Deliberately incomplete: HTTP idle events/cache, idle eviction,
//! and typed lower-level connect/cleanup outcomes remain follow-up work. In
//! particular, cancelled/dropped stdio connect can hide a cleanup failure. Its
//! slot is quarantined, never silently reopened. This is not LH-02 completion.
//! The fingerprint covers explicit host config, not unlisted inherited process
//! environment or replacement of an executable in place. Host changes of that
//! kind must also invalidate the manager before reuse.

use crate::{
    catalog::ToolEntry,
    mcp,
    mcp_client::{
        CatalogMissReason, CatalogObservation, Client, Error as ClientError, McpObservation,
        Transport,
    },
    mcp_stdio,
    session_store::Owner,
};
use serde::Serialize;
use serde_json::Value;
use sha2::{Digest, Sha256};
use std::{
    path::Path,
    sync::{
        atomic::{AtomicBool, Ordering},
        Arc, Mutex,
    },
    time::{Duration, Instant},
};
use tokio::{runtime::Handle, sync::Notify};
use uuid::Uuid;

#[derive(Clone, Copy, PartialEq, Eq)]
enum Kind {
    Http,
    Stdio,
}

/// Cache reuse is opt-in. FreshEachLease preserves the shipped behavior.
#[derive(Clone, Copy)]
pub(crate) struct CatalogPolicy {
    ttl: Option<Duration>,
}

impl CatalogPolicy {
    pub(crate) fn fresh_each_lease() -> Self {
        Self { ttl: None }
    }

    pub(crate) fn reviewed_stdio(ttl: Duration) -> Result<Self, Error> {
        if ttl.is_zero() || ttl > Duration::from_secs(30) {
            return Err(Error::InvalidCatalogPolicy);
        }
        Ok(Self { ttl: Some(ttl) })
    }
}

// This is a private exact schema snapshot, not an authority/approval receipt.
// No Debug/Serialize and no model projection. TTL starts BEFORE page 1 fetch.
struct CatalogSnapshot {
    scope: Scope,
    generation: u64,
    notification_revision: u64,
    post_call_revision: u64,
    fetched_at: Instant,
    entries: Vec<ToolEntry>,
}

/// Opaque host-issued scope. No Debug, Serialize, Deserialize or hash accessor.
/// Digests include private config (credentials too), but are never exported.
#[derive(Clone, PartialEq, Eq)]
pub(crate) struct Scope {
    authority: [u8; 32],
    config: [u8; 32],
    kind: Kind,
}

impl Scope {
    pub(crate) fn from_host(
        owner: &Owner,
        acp_session: &str,
        conversation: Uuid,
        config: &Value,
        cwd: &Path,
    ) -> Result<Self, Error> {
        if [
            &owner.profile_id,
            &owner.account_id,
            &owner.tenant_id,
            &owner.server_id,
        ]
        .iter()
        .any(|id| !bounded_id(id, 2048))
            || !bounded_id(acp_session, 128)
            || config.get("name").and_then(Value::as_str) != Some("tutor")
        {
            return Err(Error::InvalidScope);
        }
        let kind = match config.get("type").and_then(Value::as_str) {
            Some("http") => {
                mcp::ServerConfig::from_acp(config).map_err(|_| Error::InvalidScope)?;
                Kind::Http
            }
            None | Some("stdio") => {
                mcp_stdio::StdioConfig::from_acp(config, cwd).map_err(|_| Error::InvalidScope)?;
                Kind::Stdio
            }
            _ => return Err(Error::InvalidScope),
        };
        let (items, field, expected_name) = match kind {
            Kind::Http => ("headers", "name", "Tutor-Conversation-ID"),
            Kind::Stdio => ("env", "name", "TUTOR_CONVERSATION_ID"),
        };
        let entries = config[items].as_array().ok_or(Error::InvalidScope)?;
        let scoped: Vec<_> = entries
            .iter()
            .filter(|entry| {
                entry[field].as_str().is_some_and(|name| match kind {
                    Kind::Http => name.eq_ignore_ascii_case(expected_name),
                    Kind::Stdio => name == expected_name,
                })
            })
            .filter_map(|entry| entry["value"].as_str())
            .collect();
        let conversation = conversation.to_string();
        if scoped.as_slice() != [conversation.as_str()] {
            return Err(Error::InvalidScope);
        }
        Ok(Self {
            authority: digest(&("tutor-mcp-authority-v1", owner, acp_session, conversation))?,
            config: config_fingerprint(config, cwd)?,
            kind,
        })
    }
}

fn bounded_id(value: &str, maximum: usize) -> bool {
    !value.trim().is_empty() && value.len() <= maximum && !value.chars().any(char::is_control)
}

fn digest(value: &impl Serialize) -> Result<[u8; 32], Error> {
    let bytes = serde_json::to_vec(value).map_err(|_| Error::InvalidScope)?;
    Ok(Sha256::digest(bytes).into())
}

fn config_fingerprint(config: &Value, cwd: &Path) -> Result<[u8; 32], Error> {
    let cwd = cwd.to_str().ok_or(Error::InvalidScope)?;
    if !Path::new(cwd).is_absolute() || cwd.len() > 4096 || cwd.contains('\0') {
        return Err(Error::InvalidScope);
    }
    // Include the FULL object, not just URL/command: args, env, headers, policy,
    // conversation binding and future fields cannot silently share a channel.
    let bytes = serde_json::to_vec(config).map_err(|_| Error::InvalidScope)?;
    if bytes.len() > 128 * 1024 {
        return Err(Error::InvalidScope);
    }
    digest(&("tutor-mcp-config-v1", cwd, config))
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum Error {
    InvalidScope,
    InvalidCatalogPolicy,
    ConfigMismatch,
    Busy,
    Closed,
    Revoked,
    Cancelled,
    Quarantined,
    ObservationPending,
    Synchronization,
    EpochExhausted,
    Transport(ClientError),
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            Self::InvalidScope => "mcp_scope_invalid",
            Self::InvalidCatalogPolicy => "mcp_catalog_policy_invalid",
            Self::ConfigMismatch => "mcp_scope_configuration_changed",
            Self::Busy => "mcp_slot_not_quiescent",
            Self::Closed => "mcp_manager_shut_down",
            Self::Revoked => "mcp_lease_revoked",
            Self::Cancelled => "mcp_acquire_cancelled",
            Self::Quarantined => "mcp_cleanup_not_proven",
            Self::ObservationPending => "mcp_observation_not_harvested",
            Self::Synchronization => "mcp_manager_state_unavailable",
            Self::EpochExhausted => "mcp_manager_epoch_exhausted",
            Self::Transport(_) => "mcp_transport_failed",
        })
    }
}
impl std::error::Error for Error {}

impl Error {
    /// Root maps this to a localized, payload-free agent restart instruction.
    /// There is deliberately no clear-quarantine operation.
    pub(crate) fn requires_agent_restart(self) -> bool {
        matches!(
            self,
            Self::Quarantined | Self::Synchronization | Self::EpochExhausted
        )
    }
}

/// Only content-free timing/counters escape in reports; scopes never do.
#[derive(Debug)]
pub(crate) struct AcquireFailure {
    pub error: Error,
    pub observations: Vec<McpObservation>,
}

#[derive(Clone, Copy)]
pub(crate) enum ReleasePolicy {
    RetainIfHealthy,
    Close,
}

pub(crate) struct ReleaseReport {
    pub observations: Vec<McpObservation>,
    /// An existing Client was acquired, not necessarily a live network peer.
    pub connection_reused: bool,
    pub retained: bool,
    pub error: Option<Error>,
    pub catalog: CatalogObservation,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum RetirementStatus {
    Quiescent,
    /// A lease, acquisition, or asynchronous cleanup still occupies the slot.
    Pending,
    Quarantined,
}

pub(crate) struct Retirement {
    pub status: RetirementStatus,
    pub observations: Vec<McpObservation>,
    pub error: Option<Error>,
}

struct Idle {
    scope: Scope,
    client: Client,
    catalog: Option<CatalogSnapshot>,
}

enum Slot {
    Vacant,
    Idle(Box<Idle>),
    Busy(u64),
    Quarantined,
}

struct State {
    epoch: u64,
    next_ticket: u64,
    closed: bool,
    slot: Slot,
    // At most one abandoned lease can finish before the next acquire. Pending
    // observations block acquire rather than being silently overwritten.
    deferred: Vec<McpObservation>,
}

struct Shared {
    state: Mutex<State>,
    changed: Notify,
    runtime: Handle,
    catalog_policy: CatalogPolicy,
}

impl Shared {
    fn current(&self, ticket: u64, epoch: u64) -> bool {
        self.state.lock().is_ok_and(|state| {
            !state.closed
                && state.epoch == epoch
                && matches!(state.slot, Slot::Busy(active) if active == ticket)
        })
    }

    async fn revoked(&self, ticket: u64, epoch: u64) {
        loop {
            let notified = self.changed.notified();
            tokio::pin!(notified);
            notified.as_mut().enable();
            if !self.current(ticket, epoch) {
                return;
            }
            notified.await;
        }
    }

    fn finish(
        &self,
        ticket: u64,
        quiescent: bool,
        observations: Vec<McpObservation>,
    ) -> Result<(), Error> {
        {
            let mut state = self.state.lock().map_err(|_| Error::Synchronization)?;
            if !matches!(state.slot, Slot::Busy(active) if active == ticket) {
                return Err(Error::Synchronization);
            }
            state.slot = if quiescent {
                Slot::Vacant
            } else {
                Slot::Quarantined
            };
            state.deferred.extend(observations);
        }
        self.changed.notify_waiters();
        Ok(())
    }
}

#[derive(Clone)]
pub(crate) struct Manager {
    shared: Arc<Shared>,
}

impl Manager {
    pub(crate) fn new(runtime: Handle) -> Self {
        Self::new_with_catalog_policy(runtime, CatalogPolicy::fresh_each_lease())
    }

    pub(crate) fn new_with_catalog_policy(runtime: Handle, catalog_policy: CatalogPolicy) -> Self {
        Self {
            shared: Arc::new(Shared {
                state: Mutex::new(State {
                    epoch: 1,
                    next_ticket: 0,
                    closed: false,
                    slot: Slot::Vacant,
                    deferred: Vec::new(),
                }),
                changed: Notify::new(),
                runtime,
                catalog_policy,
            }),
        }
    }

    pub(crate) fn take_deferred_observations(&self) -> Result<Vec<McpObservation>, Error> {
        let mut state = self
            .shared
            .state
            .lock()
            .map_err(|_| Error::Synchronization)?;
        Ok(std::mem::take(&mut state.deferred))
    }

    /// Wait for local cleanup/admission only: no connect, list or call is sent.
    /// Ready observations are drained atomically with the readiness check.
    /// On error they remain available to take_deferred_observations(). Acquire
    /// retains its immediate Busy/ObservationPending contract for other users.
    pub(crate) async fn wait_ready(
        &self,
        cancel: &AtomicBool,
        timeout: Duration,
    ) -> Result<Vec<McpObservation>, Error> {
        let deadline = tokio::time::Instant::now() + timeout;
        loop {
            let notified = self.shared.changed.notified();
            tokio::pin!(notified);
            notified.as_mut().enable();
            if cancel.load(Ordering::Acquire) {
                return Err(Error::Cancelled);
            }
            {
                let mut state = self
                    .shared
                    .state
                    .lock()
                    .map_err(|_| Error::Synchronization)?;
                if state.closed {
                    return Err(Error::Closed);
                }
                match state.slot {
                    Slot::Quarantined => return Err(Error::Quarantined),
                    Slot::Vacant | Slot::Idle(_) => return Ok(std::mem::take(&mut state.deferred)),
                    Slot::Busy(_) => {}
                }
            }
            tokio::select! {
                biased;
                _ = async {
                    while !cancel.load(Ordering::Acquire) {
                        tokio::time::sleep(Duration::from_millis(10)).await;
                    }
                } => return Err(Error::Cancelled),
                _ = tokio::time::sleep_until(deadline) => return Err(Error::Busy),
                _ = notified => {},
            }
        }
    }

    pub(crate) async fn acquire(
        &self,
        scope: &Scope,
        config: &Value,
        cwd: &Path,
        cancel: &AtomicBool,
    ) -> Result<Lease, AcquireFailure> {
        let failure = |error| AcquireFailure {
            error,
            observations: Vec::new(),
        };
        if cancel.load(Ordering::Acquire) {
            return Err(failure(Error::Cancelled));
        }
        if config_fingerprint(config, cwd).map_err(failure)? != scope.config {
            return Err(failure(Error::ConfigMismatch));
        }
        // No guard survives this block. In particular, close/connect are NOT
        // performed while holding the mutex, even for mismatched idle scopes.
        let (ticket, epoch, idle) = {
            let mut state = self
                .shared
                .state
                .lock()
                .map_err(|_| failure(Error::Synchronization))?;
            if state.closed {
                return Err(failure(Error::Closed));
            }
            if !state.deferred.is_empty() {
                return Err(failure(Error::ObservationPending));
            }
            match state.slot {
                Slot::Busy(_) => return Err(failure(Error::Busy)),
                Slot::Quarantined => return Err(failure(Error::Quarantined)),
                Slot::Vacant | Slot::Idle(_) => {}
            }
            let ticket = state
                .next_ticket
                .checked_add(1)
                .ok_or_else(|| failure(Error::EpochExhausted))?;
            state.next_ticket = ticket;
            let previous = std::mem::replace(&mut state.slot, Slot::Busy(ticket));
            let idle = match previous {
                Slot::Idle(idle) => Some(*idle),
                _ => None,
            };
            (ticket, state.epoch, idle)
        };
        let mut lease = Lease {
            shared: self.shared.clone(),
            ticket,
            epoch,
            scope: scope.clone(),
            client: None,
            healthy: true,
            live: true,
            empty_safe: true,
            connection_reused: false,
            preceding: Vec::new(),
            connecting_started: None,
            catalog: None,
            catalog_invalidated: None,
            catalog_observation: CatalogObservation::default(),
        };
        if let Some(idle) = idle {
            let same_scope = idle.scope == *scope;
            lease.client = Some(idle.client);
            if same_scope {
                lease.catalog = idle.catalog;
                lease.connection_reused = true;
                return Ok(lease);
            }
            // Reserve the slot THROUGH old-client cleanup. No second Client
            // is created until close confirms the old channel is quiescent.
            lease.healthy = false;
            let closed = lease.client.as_mut().expect("owned client").close().await;
            let mut old = lease.client.take().expect("owned client");
            lease.preceding.push(old.take_observation());
            drop(old);
            if closed.is_err() {
                lease.empty_safe = false;
                return Err(lease.failure(Error::Quarantined));
            }
            lease.healthy = true;
        }
        if cancel.load(Ordering::Acquire) || !self.shared.current(ticket, epoch) {
            let error = if cancel.load(Ordering::Acquire) {
                Error::Cancelled
            } else {
                Error::Revoked
            };
            return Err(lease.failure(error));
        }
        let started = Instant::now();
        lease.connecting_started = Some(started);
        lease.empty_safe = false;
        let connected = tokio::select! {
            biased;
            _ = self.shared.revoked(ticket, epoch) => None,
            result = Client::connect(config, cwd, cancel) => Some(result),
        };
        lease.connecting_started = None;
        match connected {
            Some(Ok(client)) => {
                lease.client = Some(client);
                lease.empty_safe = true;
                if cancel.load(Ordering::Acquire) || !self.shared.current(ticket, epoch) {
                    let error = if cancel.load(Ordering::Acquire) {
                        Error::Cancelled
                    } else {
                        Error::Revoked
                    };
                    let report = lease.release(ReleasePolicy::Close, cancel).await;
                    return Err(AcquireFailure {
                        error: report.error.unwrap_or(error),
                        observations: report.observations,
                    });
                }
                // Do not drain begin_turn_observation here: the first connect
                // belongs to this lease and MUST survive until release.
                Ok(lease)
            }
            Some(Err(error)) => {
                // No Client exists on this path; record the real wrapper wait.
                lease.preceding.push(McpObservation {
                    connect_total_ms: Some(started.elapsed().as_secs_f64() * 1000.0),
                    ..McpObservation::default()
                });
                // A completed HTTP failure drops all local wrapper ownership.
                // Stdio Connection hides whether close/reap succeeded, so its
                // slot cannot safely be reopened on the available evidence.
                lease.empty_safe = scope.kind == Kind::Http || error == ClientError::InvalidConfig;
                let reason = if lease.empty_safe {
                    Error::Transport(error)
                } else {
                    Error::Quarantined
                };
                Err(lease.failure(reason))
            }
            None => {
                lease.preceding.push(McpObservation {
                    connect_total_ms: Some(started.elapsed().as_secs_f64() * 1000.0),
                    ..McpObservation::default()
                });
                Err(lease.failure(Error::Quarantined))
            }
        }
    }

    /// Synchronous epoch barrier for ACP rebind. Never waits for network or
    /// process cleanup. Acquisitions remain Busy until that cleanup completes.
    pub(crate) fn invalidate(&self) -> Retirement {
        match self.begin_retirement(false) {
            Ok(closing) => {
                // Lease::drop schedules cleanup and leaves the slot occupied.
                drop(closing);
                self.retirement_snapshot()
            }
            Err(error) => retirement_failure(error),
        }
    }

    /// Permanent EOF shutdown. Parent must concurrently let the active prompt
    /// worker observe cancellation/revocation and release/drop its lease.
    /// A bounded wait never clears a non-quiescent slot or claims OS cleanup.
    pub(crate) async fn shutdown(&self) -> Retirement {
        let closing = match self.begin_retirement(true) {
            Ok(closing) => closing,
            Err(error) => {
                let mut result = retirement_failure(error);
                if let Ok(observations) = self.take_deferred_observations() {
                    result.observations = observations;
                }
                return result;
            }
        };
        let mut observations = Vec::new();
        if let Some(lease) = closing {
            let report = lease
                .release(ReleasePolicy::Close, &AtomicBool::new(false))
                .await;
            observations.extend(report.observations);
        }
        let waited = tokio::time::timeout(Duration::from_secs(10), async {
            loop {
                let notified = self.shared.changed.notified();
                tokio::pin!(notified);
                notified.as_mut().enable();
                let snapshot = self.retirement_snapshot();
                if snapshot.status != RetirementStatus::Pending {
                    return snapshot;
                }
                notified.await;
            }
        })
        .await;
        let mut result = match waited {
            Ok(result) => result,
            Err(_) => Retirement {
                status: RetirementStatus::Pending,
                observations: Vec::new(),
                error: Some(Error::Busy),
            },
        };
        match self.take_deferred_observations() {
            Ok(deferred) => observations.extend(deferred),
            Err(error) => result.error = Some(error),
        }
        result.observations = observations;
        result
    }

    fn begin_retirement(&self, permanent: bool) -> Result<Option<Lease>, Error> {
        let closing = (|| {
            let mut state = self
                .shared
                .state
                .lock()
                .map_err(|_| Error::Synchronization)?;
            state.closed |= permanent;
            let Some(next_epoch) = state.epoch.checked_add(1) else {
                state.closed = true;
                return Err(Error::EpochExhausted);
            };
            state.epoch = next_epoch;
            match &state.slot {
                Slot::Busy(_) => Ok(None),
                Slot::Quarantined => Err(Error::Quarantined),
                Slot::Vacant => Ok(None),
                Slot::Idle(_) => {
                    let Some(ticket) = state.next_ticket.checked_add(1) else {
                        state.closed = true;
                        return Err(Error::EpochExhausted);
                    };
                    state.next_ticket = ticket;
                    let previous = std::mem::replace(&mut state.slot, Slot::Busy(ticket));
                    let Slot::Idle(idle) = previous else {
                        unreachable!()
                    };
                    Ok(Some((ticket, state.epoch, *idle)))
                }
            }
        })();
        // Notify even on overflow/poison: no waiter may keep assuming that its
        // old epoch is authoritative after the synchronous barrier returns.
        self.shared.changed.notify_waiters();
        Ok(closing?.map(|(ticket, epoch, idle)| Lease {
            shared: self.shared.clone(),
            ticket,
            epoch,
            scope: idle.scope,
            client: Some(idle.client),
            healthy: false,
            live: true,
            empty_safe: false,
            connection_reused: false,
            preceding: Vec::new(),
            connecting_started: None,
            catalog: None,
            catalog_invalidated: None,
            catalog_observation: CatalogObservation::default(),
        }))
    }

    fn retirement_snapshot(&self) -> Retirement {
        let status = match self.shared.state.lock() {
            Ok(state) => match state.slot {
                Slot::Busy(_) => RetirementStatus::Pending,
                Slot::Quarantined => RetirementStatus::Quarantined,
                _ => RetirementStatus::Quiescent,
            },
            Err(_) => return retirement_failure(Error::Synchronization),
        };
        Retirement {
            status,
            observations: Vec::new(),
            error: if status == RetirementStatus::Quarantined {
                Some(Error::Quarantined)
            } else {
                None
            },
        }
    }
}

fn retirement_failure(error: Error) -> Retirement {
    Retirement {
        status: RetirementStatus::Quarantined,
        observations: Vec::new(),
        error: Some(error),
    }
}

pub(crate) struct Lease {
    shared: Arc<Shared>,
    ticket: u64,
    epoch: u64,
    scope: Scope,
    client: Option<Client>,
    healthy: bool,
    live: bool,
    empty_safe: bool,
    connection_reused: bool,
    preceding: Vec<McpObservation>,
    connecting_started: Option<Instant>,
    catalog: Option<CatalogSnapshot>,
    catalog_invalidated: Option<CatalogMissReason>,
    catalog_observation: CatalogObservation,
}

impl Lease {
    fn failure(mut self, error: Error) -> AcquireFailure {
        AcquireFailure {
            error,
            observations: std::mem::take(&mut self.preceding),
        }
        // Drop keeps the reservation until cleanup, or quarantines an unknown
        // partial connect. Returned timings never imply cleanup succeeded.
    }

    pub(crate) fn is_current(&self) -> bool {
        self.live && self.healthy && self.shared.current(self.ticket, self.epoch)
    }

    fn before_io(&mut self, cancel: &AtomicBool) -> bool {
        let allowed = self.is_current() && !cancel.load(Ordering::Acquire);
        // Dirty BEFORE constructing/polling the transport future: dropping the
        // future leaves the lease dirty, even if the caller suppresses errors.
        self.healthy = false;
        allowed
    }

    fn invalidate_catalog(&mut self, reason: CatalogMissReason) {
        self.catalog = None;
        self.catalog_invalidated = Some(reason);
        self.catalog_observation.last_miss_reason = Some(reason);
    }

    // Policy-independent stamp check. Even fallback transports can reject
    // an observed notification or an expired approval-time snapshot.
    fn snapshot_reason(&self) -> Option<CatalogMissReason> {
        let ttl = self.shared.catalog_policy.ttl?;
        if !self.is_current() {
            return Some(CatalogMissReason::UnusableTransport);
        }
        let state = self.client.as_ref().expect("owned client").catalog_state();
        if !state.ready {
            return Some(CatalogMissReason::UnusableTransport);
        }
        if let Some(reason) = self.catalog_invalidated {
            return Some(reason);
        }
        let Some(snapshot) = &self.catalog else {
            return Some(CatalogMissReason::Cold);
        };
        if snapshot.scope != self.scope || snapshot.generation != self.epoch {
            return Some(CatalogMissReason::ScopeChanged);
        }
        if state.notification_revision != snapshot.notification_revision {
            return Some(CatalogMissReason::RealNotification);
        }
        if state.post_call_revision != snapshot.post_call_revision {
            return Some(CatalogMissReason::UnreviewedCall);
        }
        if snapshot.fetched_at.elapsed() >= ttl {
            return Some(CatalogMissReason::Expired);
        }
        None
    }

    fn cache_miss(&self) -> Option<CatalogMissReason> {
        if self.shared.catalog_policy.ttl.is_none() {
            return Some(CatalogMissReason::Disabled);
        }
        let state = self.client.as_ref().expect("owned client").catalog_state();
        if !state.ready {
            return Some(CatalogMissReason::UnusableTransport);
        }
        if let Some(reason) = self.catalog_invalidated {
            return Some(reason);
        }
        if !state.idle_notifications_observable {
            return Some(CatalogMissReason::IdleNotificationsUnobservable);
        }
        if !state.notifications_supported {
            return Some(CatalogMissReason::CapabilityMissing);
        }
        self.snapshot_reason()
    }

    pub(crate) fn require_catalog_refresh(&mut self) {
        self.invalidate_catalog(CatalogMissReason::PermissionBoundary);
    }

    /// Must be called by the Connection adapter AFTER a matched allow response,
    /// BEFORE changed()/refresh and executor dispatch. A cached HTTP/fallback
    /// snapshot younger than TTL is still insufficient at this boundary.
    /// Observable stdio keeps its normal notification/TTL checks instead.
    pub(crate) fn require_catalog_refresh_after_approval(&mut self) -> bool {
        if self.shared.catalog_policy.ttl.is_none() {
            return false;
        }
        let state = self.client.as_ref().expect("owned client").catalog_state();
        let required = !state.idle_notifications_observable || !state.notifications_supported;
        if required {
            self.require_catalog_refresh();
        }
        required
    }

    /// Metadata only; this does not drain/invalidate the actual snapshot.
    /// Root should normally use ReleaseReport.catalog after release instead.
    pub(crate) fn take_catalog_observation(&mut self) -> CatalogObservation {
        std::mem::take(&mut self.catalog_observation)
    }

    pub(crate) async fn list_tools(
        &mut self,
        cancel: &AtomicBool,
    ) -> Result<Vec<ToolEntry>, ClientError> {
        if !self.is_current() || cancel.load(Ordering::Acquire) {
            self.healthy = false;
            return Err(ClientError::Catalog);
        }
        let mut reason = if self.shared.catalog_policy.ttl.is_none() {
            Some(CatalogMissReason::Disabled)
        } else {
            self.cache_miss()
        };
        if reason.is_none() {
            let entries = self
                .catalog
                .as_ref()
                .expect("validated catalog")
                .entries
                .clone();
            // Recheck after the clone: a stdio notification may have arrived
            // concurrently. Never return a hit using the now-obsolete stamp.
            reason = self.cache_miss();
            if reason.is_none() && !cancel.load(Ordering::Acquire) {
                self.catalog_observation.cache_hits =
                    self.catalog_observation.cache_hits.saturating_add(1);
                return Ok(entries);
            }
        }
        let reason = reason.unwrap_or(CatalogMissReason::UnusableTransport);
        self.catalog_observation.cache_misses =
            self.catalog_observation.cache_misses.saturating_add(1);
        self.invalidate_catalog(reason); // no stale fallback after error/drop
        let before = self.client.as_ref().expect("owned client").catalog_state();
        let fetched_at = Instant::now();
        if !self.before_io(cancel) {
            return Err(ClientError::Catalog);
        }
        let shared = self.shared.clone();
        let result = tokio::select! {
            biased;
            _ = shared.revoked(self.ticket, self.epoch) => Err(ClientError::Catalog),
            result = self.client.as_mut().expect("owned client").list_tools(cancel) => result,
        };
        self.healthy = result.is_ok()
            && !cancel.load(Ordering::Acquire)
            && self.shared.current(self.ticket, self.epoch);
        let entries = match result {
            Ok(entries) if self.healthy => entries,
            Ok(_) => {
                self.invalidate_catalog(CatalogMissReason::FetchChanged);
                return Err(ClientError::Catalog);
            }
            Err(error) => {
                let after = self.client.as_ref().expect("owned client").catalog_state();
                let reason = if before.notification_revision != after.notification_revision {
                    CatalogMissReason::FetchChanged
                } else {
                    CatalogMissReason::FetchFailure
                };
                self.invalidate_catalog(reason);
                return Err(error);
            }
        };
        let after = self.client.as_ref().expect("owned client").catalog_state();
        if !after.ready
            || before.notification_revision != after.notification_revision
            || before.post_call_revision != after.post_call_revision
        {
            self.invalidate_catalog(CatalogMissReason::FetchChanged);
            self.healthy = false;
            return Err(ClientError::Catalog);
        }
        if let Some(ttl) = self.shared.catalog_policy.ttl {
            if fetched_at.elapsed() >= ttl {
                self.invalidate_catalog(CatalogMissReason::Expired);
                self.healthy = false;
                return Err(ClientError::Catalog);
            }
            self.catalog = Some(CatalogSnapshot {
                scope: self.scope.clone(),
                generation: self.epoch,
                notification_revision: after.notification_revision,
                post_call_revision: after.post_call_revision,
                fetched_at,
                entries: entries.clone(),
            });
        }
        self.catalog_invalidated = None;
        Ok(entries)
    }

    pub(crate) fn take_tools_changed(&mut self) -> bool {
        if !self.is_current() {
            self.healthy = false;
            return true;
        }
        if self.shared.catalog_policy.ttl.is_some() {
            // Sticky until a successful wire refresh; never consume a real
            // notification by merely asking this question.
            return self.snapshot_reason().is_some();
        }
        self.catalog_invalidated.is_some()
            || self
                .client
                .as_mut()
                .expect("owned client")
                .take_tools_changed()
    }

    pub(crate) async fn release(
        mut self,
        policy: ReleasePolicy,
        cancel: &AtomicBool,
    ) -> ReleaseReport {
        if matches!(policy, ReleasePolicy::RetainIfHealthy)
            && self.is_current()
            && !cancel.load(Ordering::Acquire)
        {
            // Store the drained observation in self BEFORE any future await.
            // If a race forces close and release is then dropped, cleanup still
            // retains the first connection and all preceding call observations.
            let observation = self
                .client
                .as_mut()
                .expect("owned client")
                .take_observation();
            self.preceding.push(observation);
            let retained = {
                match self.shared.state.lock() {
                    Ok(mut state)
                        if !state.closed
                            && state.epoch == self.epoch
                            && !cancel.load(Ordering::Acquire)
                            && matches!(state.slot, Slot::Busy(active) if active == self.ticket) =>
                    {
                        state.slot = Slot::Idle(Box::new(Idle {
                            scope: self.scope.clone(),
                            client: self.client.take().expect("owned client"),
                            catalog: self.catalog.take(),
                        }));
                        true
                    }
                    _ => false,
                }
            };
            if retained {
                self.live = false;
                self.shared.changed.notify_waiters();
                return ReleaseReport {
                    observations: std::mem::take(&mut self.preceding),
                    connection_reused: self.connection_reused,
                    retained: true,
                    error: None,
                    catalog: self.take_catalog_observation(),
                };
            }
        }
        // Ownership stays in self over await. Dropping release schedules the
        // same client for cleanup; it never clears the occupied slot early.
        let closed = self.client.as_mut().expect("owned client").close().await;
        let mut client = self.client.take().expect("owned client");
        self.preceding.push(client.take_observation());
        drop(client);
        let finished = self.shared.finish(self.ticket, closed.is_ok(), Vec::new());
        self.live = false;
        ReleaseReport {
            observations: std::mem::take(&mut self.preceding),
            connection_reused: self.connection_reused,
            retained: false,
            error: if closed.is_err() {
                Some(Error::Quarantined)
            } else {
                finished.err()
            },
            catalog: self.take_catalog_observation(),
        }
    }
}

impl Transport for Lease {
    fn server_id(&self) -> &str {
        self.client.as_ref().expect("owned client").server_id()
    }

    async fn call_tool(
        &mut self,
        name: &str,
        args: Value,
        key: Option<&str>,
        cancel: &AtomicBool,
    ) -> Result<Value, ClientError> {
        // Final local freshness check before transport IO. The executor still
        // validates the exact prepared schema and actual ACP approval first.
        if self.shared.catalog_policy.ttl.is_some() && self.snapshot_reason().is_some() {
            let reason = self
                .snapshot_reason()
                .unwrap_or(CatalogMissReason::FetchChanged);
            self.invalidate_catalog(reason);
            self.healthy = false;
            return Err(ClientError::Call); // never auto-refresh/replay a call
        }
        let before = self.client.as_ref().expect("owned client").catalog_state();
        if !self.before_io(cancel) {
            return Err(ClientError::Call);
        }
        let shared = self.shared.clone();
        let result = tokio::select! {
            biased;
            _ = shared.revoked(self.ticket, self.epoch) => Err(ClientError::Call),
            result = self.client.as_mut().expect("owned client").call_tool(name, args, key, cancel) => result,
        };
        self.healthy = result.is_ok()
            && !cancel.load(Ordering::Acquire)
            && self.shared.current(self.ticket, self.epoch);
        // Disabled preserves the existing result/dirty-marker path without
        // cloning or classifying private tool payloads for an inactive cache.
        if self.shared.catalog_policy.ttl.is_none() {
            return result;
        }
        let after = self.client.as_ref().expect("owned client").catalog_state();
        if self.healthy {
            let valid_read = result
                .as_ref()
                .is_ok_and(|envelope| reviewed_catalog_read(name, envelope));
            let ttl = self.shared.catalog_policy.ttl;
            let can_acknowledge = ttl.is_some()
                && after.idle_notifications_observable
                && after.notifications_supported
                && after.ready
                && valid_read;
            if can_acknowledge {
                if let Some(snapshot) = &mut self.catalog {
                    if snapshot.scope == self.scope
                        && snapshot.generation == self.epoch
                        && snapshot.notification_revision == before.notification_revision
                        && before.notification_revision == after.notification_revision
                        && snapshot.post_call_revision == before.post_call_revision
                        && before.post_call_revision.checked_add(1)
                            == Some(after.post_call_revision)
                        && snapshot.fetched_at.elapsed() < ttl.expect("checked TTL")
                    {
                        // Acknowledge only THIS reviewed read's conservative
                        // counter. The lower tools_changed flag stays intact.
                        snapshot.post_call_revision = after.post_call_revision;
                    }
                }
            } else if result
                .as_ref()
                .is_ok_and(|value| value.get("isError").and_then(Value::as_bool) == Some(true))
            {
                self.invalidate_catalog(CatalogMissReason::ToolError);
            } else {
                self.invalidate_catalog(CatalogMissReason::UnreviewedCall);
            }
        } else {
            self.invalidate_catalog(CatalogMissReason::UnusableTransport);
        }
        // Keep a completed envelope intact even if cancellation/revocation
        // arrives just after it. Its transport may be retired; the executor
        // still needs the exact reply to journal success/error/uncertainty.
        // isError:true is transport Ok, never a business success proof.
        result
    }
}

// These exact handlers were reviewed in tools/context.go, alerts.go, olm.go
// and their DB/engine callees. This ONLY controls schema-cache retention; it
// never marks an operation read-only for authorization, grants consent or
// interprets a model-proposed tool. All other names invalidate conservatively.
fn reviewed_catalog_read(name: &str, envelope: &Value) -> bool {
    if !matches!(
        name,
        "get_learner_context" | "get_pending_alerts" | "get_olm_snapshot"
    ) {
        return false;
    }
    let Ok(payload) = crate::call_validation::result_payload(envelope) else {
        return false;
    };
    if payload.get("error").is_some() {
        return false;
    }
    match name {
        "get_learner_context" => {
            payload.get("profile").is_some_and(Value::is_object)
                && payload.get("domains").is_some_and(Value::is_array)
                && payload
                    .get("needs_domain_setup")
                    .is_some_and(Value::is_boolean)
        }
        "get_pending_alerts" => {
            payload.get("alerts").is_some_and(Value::is_array)
                && payload.get("has_critical").is_some_and(Value::is_boolean)
                && payload
                    .get("needs_domain_setup")
                    .is_some_and(Value::is_boolean)
        }
        "get_olm_snapshot" => {
            payload.get("domain_id").is_some_and(Value::is_string)
                || payload.get("domains").is_some_and(Value::is_array)
        }
        _ => false,
    }
}

impl Drop for Lease {
    fn drop(&mut self) {
        if !self.live {
            return;
        }
        self.live = false;
        if let Some(started) = self.connecting_started.take() {
            self.preceding.push(McpObservation {
                connect_total_ms: Some(started.elapsed().as_secs_f64() * 1000.0),
                ..McpObservation::default()
            });
        }
        if let Some(client) = self.client.take() {
            let cleanup = Cleanup {
                shared: self.shared.clone(),
                ticket: self.ticket,
                client: Some(client),
                observations: std::mem::take(&mut self.preceding),
                live: true,
            };
            // The slot stays Busy while this task owns the client. Runtime
            // shutdown drops Cleanup, which quarantines instead of reopening.
            drop(self.shared.runtime.spawn(cleanup.run()));
        } else {
            let _ = self.shared.finish(
                self.ticket,
                self.empty_safe,
                std::mem::take(&mut self.preceding),
            );
        }
    }
}

struct Cleanup {
    shared: Arc<Shared>,
    ticket: u64,
    client: Option<Client>,
    observations: Vec<McpObservation>,
    live: bool,
}

impl Cleanup {
    async fn run(mut self) {
        let closed = self
            .client
            .as_mut()
            .expect("owned cleanup client")
            .close()
            .await;
        let mut client = self.client.take().expect("owned cleanup client");
        self.observations.push(client.take_observation());
        drop(client);
        let _ = self.shared.finish(
            self.ticket,
            closed.is_ok(),
            std::mem::take(&mut self.observations),
        );
        self.live = false;
    }
}

impl Drop for Cleanup {
    fn drop(&mut self) {
        if !self.live {
            return;
        }
        if let Some(mut client) = self.client.take() {
            self.observations.push(client.take_observation());
            drop(client); // existing stdio supervisor/kill-on-drop backstop
        }
        let _ = self
            .shared
            .finish(self.ticket, false, std::mem::take(&mut self.observations));
    }
}

// TEST MATRIX (implemented below, not run/compiled until parent's CPU signal):
// 1. Two leases same scope: exactly one initialize/stdio child; first release
//    has connectTotalMs, second has null. Fresh tools/list each turn, no replay.
// 2. Owner/account/tenant/server/conversation/ACP-session/cwd/args/env/headers
//    changes never reuse a Client; changed idle connection closes BEFORE spawn.
// 3. Busy lease blocks acquire, including different scope, cancelled IO and
//    dropped acquire/release/cleanup futures. No second child before reap.
// 4. Invalidation before IO rejects old lease; mid-IO revocation/invalidation
//    integration and Unknown/permission semantics remain executor-owned.
// 5. RPC error, pre-cancel, stdio EOF while idle, process death and dropped call
//    future poison reuse. Idle death is detected by first fresh tools/list.
// 6. isError:true envelope preserved; transport counters and toolResultErrors
//    stay separate. No permission flow is implemented or tested in this module.
// 7. Deferred observation retains first connect after drop; pending harvest
//    blocks acquire. No scopes/config/private IDs in any export or error.
// 8. Shutdown with idle/leased/closing/quarantined slot, Windows Job and Unix
//    process cleanup; never call a new Manager a successful reconciliation.

#[cfg(test)]
mod tests {
    use super::*;
    use crate::mcp::tests::{initialization, mock, Mock, Reply};
    use serde_json::json;
    use std::sync::OnceLock;

    fn owner() -> Owner {
        Owner {
            profile_id: "fixture-private-profile".into(),
            account_id: "fixture-private-account".into(),
            tenant_id: "fixture-private-tenant".into(),
            server_id: "fixture-private-server".into(),
        }
    }

    fn config(url: &str, conversation: Uuid) -> Value {
        json!({"type":"http","name":"tutor","url":url,"headers":[
            {"name":"Authorization","value":"Bearer fixture-private-token"},
            {"name":"Tutor-Conversation-ID","value":conversation.to_string()}
        ]})
    }

    async fn http() -> Mock {
        mock(|request| {
            initialization(request).unwrap_or_else(|| {
                let result = match request["method"].as_str().unwrap() {
                    "tools/list" => json!({"tools":[]}),
                    "tools/call" => json!({"content":[],"structuredContent":{"fixture":true}}),
                    _ => panic!("unexpected fixture method"),
                };
                Reply::json(json!({"jsonrpc":"2.0","id":request["id"],"result":result}))
            })
        })
        .await
    }

    fn count(server: &Mock, method: &str) -> usize {
        server
            .observed
            .lock()
            .unwrap()
            .iter()
            .filter(|(r, _)| r["method"] == method)
            .count()
    }

    fn assert_error(result: Result<Lease, AcquireFailure>, expected: Error) {
        let failure = result.err().expect("acquire unexpectedly succeeded");
        private_free(&failure.observations);
        assert_eq!(failure.error, expected);
    }

    fn private_free(observations: &[McpObservation]) {
        for observation in observations {
            let value = serde_json::to_value(observation).unwrap();
            assert_eq!(value["schemaVersion"], 1);
            assert!(value
                .as_object()
                .unwrap()
                .values()
                .all(|v| v.is_number() || v.is_null()));
            assert!(value["initializeTotalMs"].is_null());
            assert!(value["catalogPages"].is_null());
            let exported = value.to_string();
            assert!(!exported.contains("fixture-private"));
            assert!(!exported.contains("http://"));
        }
    }

    async fn cleanup_finished(manager: &Manager) {
        tokio::time::timeout(Duration::from_secs(6), async {
            loop {
                match manager.retirement_snapshot().status {
                    RetirementStatus::Quiescent => return,
                    RetirementStatus::Quarantined => panic!("fixture cleanup was not confirmed"),
                    RetirementStatus::Pending => tokio::time::sleep(Duration::from_millis(2)).await,
                }
            }
        })
        .await
        .expect("fixture cleanup did not finish");
    }

    #[tokio::test]
    async fn two_http_leases_reuse_one_initialize_and_keep_per_turn_observations() {
        let directory = tempfile::tempdir().unwrap();
        let server = http().await;
        let conversation = Uuid::new_v4();
        let config = config(&server.url, conversation);
        let scope =
            Scope::from_host(&owner(), "acp", conversation, &config, directory.path()).unwrap();
        let scope_again =
            Scope::from_host(&owner(), "acp", conversation, &config, directory.path()).unwrap();
        assert!(scope == scope_again);
        let manager = Manager::new(Handle::current());
        let cancel = AtomicBool::new(false);
        for turn in 0..2 {
            let mut lease = manager
                .acquire(&scope, &config, directory.path(), &cancel)
                .await
                .unwrap();
            assert_eq!(lease.server_id(), "tutor");
            lease.list_tools(&cancel).await.unwrap();
            let report = lease.release(ReleasePolicy::RetainIfHealthy, &cancel).await;
            assert!(report.retained);
            assert_eq!(report.connection_reused, turn == 1);
            assert_eq!(report.error, None);
            assert_eq!(report.observations.len(), 1);
            let observation = &report.observations[0];
            assert_eq!(observation.connect_total_ms.is_some(), turn == 0);
            assert!(observation.catalog_total_ms.is_some());
            assert_eq!(observation.catalog_attempts, 1);
            assert_eq!(observation.call_attempts, 0);
            assert_eq!(observation.close_total_ms, None);
            private_free(&report.observations);
        }
        assert_eq!(count(&server, "initialize"), 1);
        assert_eq!(count(&server, "notifications/initialized"), 1);
        assert_eq!(count(&server, "tools/list"), 2);
        assert_eq!(count(&server, "tools/call"), 0);
        let report = manager.shutdown().await;
        assert_eq!(report.status, RetirementStatus::Quiescent);
        assert_eq!(report.error, None);
        assert_eq!(report.observations.len(), 1);
        assert!(report.observations[0].close_total_ms.is_some());
        assert_eq!(report.observations[0].connect_total_ms, None);
        private_free(&report.observations);
    }

    #[tokio::test]
    async fn changed_authority_and_config_never_reuse_a_channel_and_mismatch_keeps_idle_intact() {
        let directory = tempfile::tempdir().unwrap();
        let server = http().await;
        let conversation = Uuid::new_v4();
        let original = config(&server.url, conversation);
        let scope =
            Scope::from_host(&owner(), "acp", conversation, &original, directory.path()).unwrap();
        let manager = Manager::new(Handle::current());
        let cancel = AtomicBool::new(false);
        let lease = manager
            .acquire(&scope, &original, directory.path(), &cancel)
            .await
            .unwrap();
        assert!(
            lease
                .release(ReleasePolicy::RetainIfHealthy, &cancel)
                .await
                .retained
        );
        let mut rotated = original.clone();
        rotated["headers"][0]["value"] = json!("Bearer fixture-private-rotated-token");
        assert_error(
            manager
                .acquire(&scope, &rotated, directory.path(), &cancel)
                .await,
            Error::ConfigMismatch,
        );
        let same = manager
            .acquire(&scope, &original, directory.path(), &cancel)
            .await
            .unwrap();
        assert!(
            same.release(ReleasePolicy::RetainIfHealthy, &cancel)
                .await
                .connection_reused
        );
        assert_eq!(count(&server, "initialize"), 1);
        let mut changed_owner = owner();
        changed_owner.account_id.push_str("-other");
        let other_conversation = Uuid::new_v4();
        let other_config = config(&server.url, other_conversation);
        let cases = [
            (owner(), "acp", conversation, rotated),
            (changed_owner, "acp", conversation, original.clone()),
            (owner(), "acp", other_conversation, other_config),
            (owner(), "another-acp-session", conversation, original),
        ];
        for (index, (owner, acp, conversation, config)) in cases.into_iter().enumerate() {
            let scope =
                Scope::from_host(&owner, acp, conversation, &config, directory.path()).unwrap();
            let mut lease = manager
                .acquire(&scope, &config, directory.path(), &cancel)
                .await
                .unwrap();
            assert!(!lease.connection_reused);
            // HTTP close is local (no DELETE): the manager report attests its
            // close wrapper. Real OS stop-before-spawn is tested with stdio.
            assert_eq!(lease.preceding.len(), 1);
            assert!(lease.preceding[0].close_total_ms.is_some());
            lease.list_tools(&cancel).await.unwrap();
            let report = lease.release(ReleasePolicy::RetainIfHealthy, &cancel).await;
            assert!(report.retained);
            assert!(!report.connection_reused);
            assert_eq!(report.observations.len(), 2);
            assert!(report.observations[1].connect_total_ms.is_some());
            assert_eq!(count(&server, "initialize"), index + 2);
            private_free(&report.observations);
        }
        assert_eq!(manager.shutdown().await.status, RetirementStatus::Quiescent);
    }

    #[tokio::test]
    async fn busy_revocation_and_deferred_observation_are_real_acquisition_barriers() {
        let directory = tempfile::tempdir().unwrap();
        let server = http().await;
        let conversation = Uuid::new_v4();
        let config = config(&server.url, conversation);
        let scope =
            Scope::from_host(&owner(), "acp", conversation, &config, directory.path()).unwrap();
        let manager = Manager::new(Handle::current());
        let cancel = AtomicBool::new(false);
        let mut lease = manager
            .acquire(&scope, &config, directory.path(), &cancel)
            .await
            .unwrap();
        lease.list_tools(&cancel).await.unwrap();
        assert_error(
            manager
                .acquire(&scope, &config, directory.path(), &cancel)
                .await,
            Error::Busy,
        );
        let revoked = manager.invalidate();
        assert_eq!(revoked.status, RetirementStatus::Pending);
        assert!(!lease.is_current());
        assert!(lease.take_tools_changed());
        assert_eq!(
            lease.call_tool("fixture", json!({}), None, &cancel).await,
            Err(ClientError::Call)
        );
        assert_error(
            manager
                .acquire(&scope, &config, directory.path(), &cancel)
                .await,
            Error::Busy,
        );
        let released = lease.release(ReleasePolicy::RetainIfHealthy, &cancel).await;
        assert!(!released.retained);
        assert!(released.observations[0].close_total_ms.is_some());
        assert_eq!(count(&server, "tools/call"), 0);
        let lease = manager
            .acquire(&scope, &config, directory.path(), &cancel)
            .await
            .unwrap();
        drop(lease);
        // Current-thread runtime: cleanup has been scheduled, but cannot have
        // run before this immediately-ready acquisition checks the reservation.
        assert_error(
            manager
                .acquire(&scope, &config, directory.path(), &cancel)
                .await,
            Error::Busy,
        );
        cleanup_finished(&manager).await;
        assert_error(
            manager
                .acquire(&scope, &config, directory.path(), &cancel)
                .await,
            Error::ObservationPending,
        );
        let deferred = manager.take_deferred_observations().unwrap();
        assert_eq!(deferred.len(), 1);
        assert!(deferred[0].connect_total_ms.is_some());
        assert!(deferred[0].close_total_ms.is_some());
        private_free(&deferred);
        let lease = manager
            .acquire(&scope, &config, directory.path(), &cancel)
            .await
            .unwrap();
        assert!(!lease.release(ReleasePolicy::Close, &cancel).await.retained);
        assert_eq!(count(&server, "initialize"), 3);
        assert_eq!(manager.shutdown().await.status, RetirementStatus::Quiescent);
    }

    #[tokio::test]
    async fn idle_invalidation_is_synchronous_and_does_not_open_a_second_client() {
        let directory = tempfile::tempdir().unwrap();
        let server = http().await;
        let conversation = Uuid::new_v4();
        let config = config(&server.url, conversation);
        let scope =
            Scope::from_host(&owner(), "acp", conversation, &config, directory.path()).unwrap();
        let manager = Manager::new(Handle::current());
        let cancel = AtomicBool::new(false);
        let lease = manager
            .acquire(&scope, &config, directory.path(), &cancel)
            .await
            .unwrap();
        lease.release(ReleasePolicy::RetainIfHealthy, &cancel).await;
        assert_eq!(manager.invalidate().status, RetirementStatus::Pending);
        assert_error(
            manager
                .acquire(&scope, &config, directory.path(), &cancel)
                .await,
            Error::Busy,
        );
        assert_eq!(count(&server, "initialize"), 1);
        cleanup_finished(&manager).await;
        let deferred = manager.take_deferred_observations().unwrap();
        assert_eq!(deferred.len(), 1);
        assert_eq!(deferred[0].connect_total_ms, None);
        let lease = manager
            .acquire(&scope, &config, directory.path(), &cancel)
            .await
            .unwrap();
        assert!(
            !lease
                .release(ReleasePolicy::Close, &cancel)
                .await
                .connection_reused
        );
        assert_eq!(count(&server, "initialize"), 2);
        manager.shutdown().await;
    }

    #[tokio::test]
    async fn wait_ready_waits_for_local_cleanup_and_drains_prior_observation_without_connecting() {
        let directory = tempfile::tempdir().unwrap();
        let server = http().await;
        let conversation = Uuid::new_v4();
        let config = config(&server.url, conversation);
        let scope =
            Scope::from_host(&owner(), "acp", conversation, &config, directory.path()).unwrap();
        let manager = Manager::new(Handle::current());
        let cancel = AtomicBool::new(false);
        let lease = manager
            .acquire(&scope, &config, directory.path(), &cancel)
            .await
            .unwrap();
        let first = lease.release(ReleasePolicy::RetainIfHealthy, &cancel).await;
        assert!(first.observations[0].connect_total_ms.is_some());
        assert_eq!(manager.invalidate().status, RetirementStatus::Pending);
        let prior = manager
            .wait_ready(&cancel, Duration::from_secs(1))
            .await
            .unwrap();
        assert_eq!(prior.len(), 1);
        assert!(prior[0].close_total_ms.is_some());
        assert_eq!(prior[0].connect_total_ms, None);
        assert_eq!(count(&server, "initialize"), 1);
        assert_eq!(count(&server, "tools/list"), 0);
        assert_eq!(count(&server, "tools/call"), 0);
        let lease = manager
            .acquire(&scope, &config, directory.path(), &cancel)
            .await
            .unwrap();
        assert_eq!(count(&server, "initialize"), 2);
        lease.release(ReleasePolicy::Close, &cancel).await;
        manager.shutdown().await;
    }

    #[tokio::test]
    async fn wait_ready_timeout_cancel_and_shutdown_never_bypass_an_active_lease() {
        let directory = tempfile::tempdir().unwrap();
        let server = http().await;
        let conversation = Uuid::new_v4();
        let config = config(&server.url, conversation);
        let scope =
            Scope::from_host(&owner(), "acp", conversation, &config, directory.path()).unwrap();
        let manager = Manager::new(Handle::current());
        let cancel = AtomicBool::new(false);
        let lease = manager
            .acquire(&scope, &config, directory.path(), &cancel)
            .await
            .unwrap();
        assert_eq!(
            manager
                .wait_ready(&cancel, Duration::from_millis(5))
                .await
                .err(),
            Some(Error::Busy)
        );
        assert_error(
            manager
                .acquire(&scope, &config, directory.path(), &cancel)
                .await,
            Error::Busy,
        );
        let mut wait = Box::pin(manager.wait_ready(&cancel, Duration::from_secs(3)));
        tokio::select! {
            biased;
            _ = &mut wait => panic!("active lease was admitted"),
            _ = std::future::ready(()) => {},
        }
        cancel.store(true, Ordering::Release);
        assert_eq!(
            tokio::time::timeout(Duration::from_millis(500), wait)
                .await
                .unwrap()
                .err(),
            Some(Error::Cancelled)
        );
        assert_eq!(count(&server, "initialize"), 1);
        assert_eq!(count(&server, "tools/call"), 0);
        lease.release(ReleasePolicy::Close, &cancel).await;
        manager.shutdown().await;
        cancel.store(false, Ordering::Release);
        assert_eq!(
            manager
                .wait_ready(&cancel, Duration::from_secs(1))
                .await
                .err(),
            Some(Error::Closed)
        );
    }

    #[tokio::test]
    async fn business_error_envelope_is_exact_transport_ok_and_rpc_failure_never_replays() {
        let directory = tempfile::tempdir().unwrap();
        let envelope = json!({"isError":true,"content":[{"type":"text","text":"fixture-private-error"}],
            "structuredContent":{"error":"fixture-private-business-rejected","unchanged":[1,2,3]}});
        let response = envelope.clone();
        let server = mock(move |request| initialization(request).unwrap_or_else(|| {
            if request["method"] == "tools/list" {
                return Reply::json(json!({"jsonrpc":"2.0","id":request["id"],"result":{"tools":[]}}));
            }
            assert_eq!(request["params"]["arguments"], json!({"private":"fixture-private-argument"}));
            assert_eq!(request["params"]["_meta"]["idempotency_key"], "fixture-private-key");
            if request["params"]["name"] == "fixture_rpc_error" {
                Reply::json(json!({"jsonrpc":"2.0","id":request["id"],"error":{"code":-32000,"message":"fixture-private-rpc-error"}}))
            } else {
                Reply::json(json!({"jsonrpc":"2.0","id":request["id"],"result":response}))
            }
        })).await;
        let conversation = Uuid::new_v4();
        let config = config(&server.url, conversation);
        let scope =
            Scope::from_host(&owner(), "acp", conversation, &config, directory.path()).unwrap();
        let manager = Manager::new(Handle::current());
        let cancel = AtomicBool::new(false);
        let mut lease = manager
            .acquire(&scope, &config, directory.path(), &cancel)
            .await
            .unwrap();
        lease.list_tools(&cancel).await.unwrap();
        let result = lease
            .call_tool(
                "fixture_business_error",
                json!({"private":"fixture-private-argument"}),
                Some("fixture-private-key"),
                &cancel,
            )
            .await
            .unwrap();
        assert_eq!(result, envelope);
        let first = lease.release(ReleasePolicy::RetainIfHealthy, &cancel).await;
        assert!(first.retained); // channel reuse, not a business success
        assert_eq!(first.observations[0].call_transport_successes, 1);
        assert_eq!(first.observations[0].tool_result_errors, 1);
        assert_eq!(first.observations[0].call_transport_failures, 0);
        let mut lease = manager
            .acquire(&scope, &config, directory.path(), &cancel)
            .await
            .unwrap();
        lease.list_tools(&cancel).await.unwrap();
        assert_eq!(
            lease
                .call_tool(
                    "fixture_rpc_error",
                    json!({"private":"fixture-private-argument"}),
                    Some("fixture-private-key"),
                    &cancel
                )
                .await,
            Err(ClientError::Call)
        );
        let second = lease.release(ReleasePolicy::RetainIfHealthy, &cancel).await;
        assert!(!second.retained);
        assert_eq!(second.observations[0].call_transport_failures, 1);
        assert_eq!(second.observations[0].tool_result_errors, 0);
        assert_eq!(count(&server, "tools/call"), 2); // two explicit calls, zero replay
        private_free(&first.observations);
        private_free(&second.observations);
        manager.shutdown().await;
        // No permission policy is asserted here: these are direct transport
        // tests. Approval and pedagogical mutation guards belong to executor.
    }

    #[tokio::test]
    async fn shutdown_revokes_active_lease_waits_for_release_and_closes_permanently() {
        let directory = tempfile::tempdir().unwrap();
        let server = http().await;
        let conversation = Uuid::new_v4();
        let config = config(&server.url, conversation);
        let scope =
            Scope::from_host(&owner(), "acp", conversation, &config, directory.path()).unwrap();
        let manager = Manager::new(Handle::current());
        let cancel = AtomicBool::new(false);
        let mut lease = manager
            .acquire(&scope, &config, directory.path(), &cancel)
            .await
            .unwrap();
        lease.list_tools(&cancel).await.unwrap();
        let shutdown_manager = manager.clone();
        let shutdown = tokio::spawn(async move { shutdown_manager.shutdown().await });
        tokio::time::timeout(Duration::from_secs(2), async {
            while lease.is_current() {
                tokio::task::yield_now().await;
            }
        })
        .await
        .unwrap();
        assert!(!shutdown.is_finished());
        assert_error(
            manager
                .acquire(&scope, &config, directory.path(), &cancel)
                .await,
            Error::Closed,
        );
        let report = lease.release(ReleasePolicy::RetainIfHealthy, &cancel).await;
        assert!(!report.retained);
        assert_eq!(report.error, None);
        private_free(&report.observations);
        let result = tokio::time::timeout(Duration::from_secs(3), shutdown)
            .await
            .unwrap()
            .unwrap();
        assert_eq!(result.status, RetirementStatus::Quiescent);
        assert_eq!(result.error, None);
        private_free(&result.observations);
        assert_error(
            manager
                .acquire(&scope, &config, directory.path(), &cancel)
                .await,
            Error::Closed,
        );
        assert!(!Error::Busy.requires_agent_restart());
        assert!(Error::Quarantined.requires_agent_restart());
        assert!(!Error::Quarantined.to_string().contains("fixture-private"));
    }

    // A std-only portable process fixture is compiled ONCE when these tests
    // are explicitly run. Unlike the lower transport tests, it records process
    // creation and checks stop-before-next-spawn for manager lifetime tests.
    const STDIO_FIXTURE: &str = r###"
use std::{io::{self, BufRead, Write}, time::Duration, fs::OpenOptions};
fn record(event: &str) { let mut log=OpenOptions::new().create(true).append(true).open(std::env::var_os("FIXTURE_LOG").unwrap()).unwrap(); writeln!(log,"{event}").unwrap(); }
fn send(text: &str) { let mut out=io::stdout().lock(); writeln!(out,"{text}").unwrap(); out.flush().unwrap(); }
#[cfg(unix)] fn stopped(pid:u32)->bool { unsafe extern "C" { fn kill(pid:i32, signal:i32)->i32; } unsafe { kill(pid as i32,0) != 0 } }
#[cfg(windows)] fn stopped(pid:u32)->bool {
    #[link(name="kernel32")] unsafe extern "system" { fn OpenProcess(access:u32,inherit:i32,pid:u32)->*mut std::ffi::c_void; fn WaitForSingleObject(handle:*mut std::ffi::c_void,ms:u32)->u32; fn CloseHandle(handle:*mut std::ffi::c_void)->i32; }
    unsafe { let handle=OpenProcess(0x00100000,0,pid); if handle.is_null(){ return true; } let done=WaitForSingleObject(handle,0)==0; CloseHandle(handle); done }
}
fn main() {
    let pid=std::process::id(); record(&format!("spawn {pid}"));
    let mode=std::env::var("FIXTURE_MODE").unwrap();
    let changed=std::sync::Arc::new(std::sync::atomic::AtomicBool::new(false));
    if let Ok(trigger)=std::env::var("FIXTURE_NOTIFY_TRIGGER") {
        let changed=changed.clone();
        std::thread::spawn(move || { while !std::path::Path::new(&trigger).exists() { std::thread::sleep(Duration::from_millis(2)); }
            changed.store(true,std::sync::atomic::Ordering::Release);
            send(r#"{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}"#); record("notified"); });
    }
    if let Ok(previous)=std::env::var("FIXTURE_PREVIOUS_PID") { record(&format!("previous_stopped {}", stopped(previous.parse().unwrap()))); }
    for line in io::stdin().lock().lines() {
        let line=line.unwrap();
        if line.contains("\"method\":\"notifications/initialized\"") { continue; }
        let start=line.find("\"id\":").unwrap()+5;
        let id:u64=line[start..].trim_start().chars().take_while(|c|c.is_ascii_digit()).collect::<String>().parse().unwrap();
        if line.contains("\"method\":\"initialize\"") {
            record("initialize");
            if mode=="hang_init" { loop { std::thread::sleep(Duration::from_secs(60)); } }
            let caps=if mode.starts_with("catalog_") && mode!="catalog_no_cap" { r#"{"listChanged":true}"# } else { "{}" };
            send(&format!(r#"{{"jsonrpc":"2.0","id":{id},"result":{{"protocolVersion":"2025-06-18","capabilities":{{"tools":{caps}}}}}}}"#)); continue;
        }
        if line.contains("\"method\":\"tools/list\"") {
            record("list");
            if mode.starts_with("catalog_") {
                let page=if line.contains("\"cursor\"") {
                    if mode=="catalog_notify_fetch" { send(r#"{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}"#); }
                    r#"{"tools":[{"name":"get_pending_alerts","inputSchema":{"type":"object","properties":{}}},{"name":"opaque_unreviewed","inputSchema":{"type":"object","properties":{}},"annotations":{"readOnlyHint":true}}]}"#.to_owned()
                } else {
                    let kind=if changed.load(std::sync::atomic::Ordering::Acquire) { "integer" } else { "string" };
                    format!(r#"{{"tools":[{{"name":"get_learner_context","inputSchema":{{"type":"object","properties":{{"flavor":{{"type":"{kind}"}}}}}}}}],"nextCursor":"p2"}}"#)
                };
                send(&format!(r#"{{"jsonrpc":"2.0","id":{id},"result":{page}}}"#));
            } else { send(&format!(r#"{{"jsonrpc":"2.0","id":{id},"result":{{"tools":[]}}}}"#)); }
            continue;
        }
        if line.contains("\"name\":\"hang\"") { record("hang_call"); loop { std::thread::sleep(Duration::from_secs(60)); } }
        record("call");
        let error=mode=="catalog_tool_error";
        let payload=if line.contains("\"name\":\"get_learner_context\"") { r#"{"profile":{},"domains":[],"needs_domain_setup":false,"objective":"fixture"}"#.to_owned() }
            else if line.contains("\"name\":\"get_pending_alerts\"") { r#"{"alerts":[],"has_critical":false,"needs_domain_setup":false}"#.to_owned() }
            else { format!(r#"{{"pid":{pid}}}"#) };
        send(&format!(r#"{{"jsonrpc":"2.0","id":{id},"result":{{"content":[],"isError":{error},"structuredContent":{payload}}}}}"#));
        if line.contains("\"name\":\"exit\"") { return; }
    }
}
"###;

    fn stdio_binary() -> &'static Path {
        static DIRECTORY: OnceLock<tempfile::TempDir> = OnceLock::new();
        let directory = DIRECTORY.get_or_init(|| {
            let directory = tempfile::tempdir().unwrap();
            let source = directory.path().join("fixture.rs");
            let binary = directory.path().join(if cfg!(windows) {
                "session-fixture.exe"
            } else {
                "session-fixture"
            });
            std::fs::write(&source, STDIO_FIXTURE).unwrap();
            let rustc = std::env::var_os("RUSTC").unwrap_or_else(|| "rustc".into());
            let output = std::process::Command::new(rustc)
                .arg("--edition=2021")
                .arg("--crate-name=session_fixture")
                .arg(&source)
                .arg("-o")
                .arg(&binary)
                .output()
                .unwrap();
            assert!(
                output.status.success(),
                "fixture compilation failed: {}",
                String::from_utf8_lossy(&output.stderr)
            );
            directory
        });
        // Store only a path to the child executable, never launch it here.
        // The following OnceLock gives a stable path reference for the tests.
        static BINARY: OnceLock<std::path::PathBuf> = OnceLock::new();
        BINARY
            .get_or_init(|| {
                directory.path().join(if cfg!(windows) {
                    "session-fixture.exe"
                } else {
                    "session-fixture"
                })
            })
            .as_path()
    }

    fn stdio_config(log: &Path, conversation: Uuid, mode: &str) -> Value {
        json!({"name":"tutor","command":stdio_binary(),"args":[],"env":[
            {"name":"TUTOR_CONVERSATION_ID","value":conversation.to_string()},
            {"name":"FIXTURE_LOG","value":log}, {"name":"FIXTURE_MODE","value":mode}
        ]})
    }

    fn events(log: &Path) -> String {
        std::fs::read_to_string(log).unwrap_or_default()
    }

    async fn event(log: &Path, text: &str) {
        tokio::time::timeout(Duration::from_secs(3), async {
            while !events(log).lines().any(|line| line == text) {
                tokio::time::sleep(Duration::from_millis(2)).await;
            }
        })
        .await
        .expect("stdio fixture did not reach event");
    }

    fn spawn_count(log: &Path) -> usize {
        events(log)
            .lines()
            .filter(|line| line.starts_with("spawn "))
            .count()
    }

    fn spawned_pid(log: &Path) -> u32 {
        events(log)
            .lines()
            .find_map(|line| line.strip_prefix("spawn ").and_then(|pid| pid.parse().ok()))
            .unwrap()
    }

    // This observes only the private child PID. Unix ESRCH proves even a
    // zombie is gone (not merely exited); Windows retains a stable OS handle.
    struct ProcessObserver {
        #[cfg(unix)]
        pid: u32,
        #[cfg(windows)]
        handle: std::os::windows::io::OwnedHandle,
    }
    impl ProcessObserver {
        fn new(pid: u32) -> Self {
            #[cfg(unix)]
            {
                Self { pid }
            }
            #[cfg(windows)]
            {
                use std::os::windows::io::FromRawHandle;
                use windows_sys::Win32::System::Threading::{OpenProcess, PROCESS_SYNCHRONIZE};
                // SAFETY: PID is returned by this test's own live child.
                let raw = unsafe { OpenProcess(PROCESS_SYNCHRONIZE, 0, pid) };
                assert!(!raw.is_null());
                // SAFETY: ownership of the newly opened process handle.
                Self {
                    handle: unsafe { std::os::windows::io::OwnedHandle::from_raw_handle(raw) },
                }
            }
        }
        fn stopped(&self) -> bool {
            #[cfg(unix)]
            {
                // SAFETY: signal zero only checks this fixture's process.
                (unsafe { libc::kill(self.pid as libc::pid_t, 0) }) != 0
                    && std::io::Error::last_os_error().raw_os_error() == Some(libc::ESRCH)
            }
            #[cfg(windows)]
            {
                use std::os::windows::io::AsRawHandle;
                // SAFETY: this observer retains the owned fixture process handle.
                unsafe {
                    windows_sys::Win32::System::Threading::WaitForSingleObject(
                        self.handle.as_raw_handle(),
                        0,
                    ) == windows_sys::Win32::Foundation::WAIT_OBJECT_0
                }
            }
        }
        async fn assert_stopped(&self) {
            tokio::time::timeout(Duration::from_secs(5), async {
                while !self.stopped() {
                    tokio::time::sleep(Duration::from_millis(2)).await;
                }
            })
            .await
            .expect("fixture was not stopped/reaped");
        }
    }

    async fn pid(lease: &mut Lease, cancel: &AtomicBool) -> u32 {
        lease
            .call_tool("pid", json!({}), None, cancel)
            .await
            .unwrap()["structuredContent"]["pid"]
            .as_u64()
            .unwrap() as u32
    }

    #[tokio::test]
    async fn stdio_reuse_scope_switch_reaps_before_next_spawn_and_idle_eof_is_not_retried() {
        let directory = tempfile::tempdir().unwrap();
        let log = directory.path().join("events.log");
        let conversation = Uuid::new_v4();
        let config = stdio_config(&log, conversation, "normal");
        let scope =
            Scope::from_host(&owner(), "acp", conversation, &config, directory.path()).unwrap();
        let manager = Manager::new(Handle::current());
        let cancel = AtomicBool::new(false);
        let mut lease = manager
            .acquire(&scope, &config, directory.path(), &cancel)
            .await
            .unwrap();
        lease.list_tools(&cancel).await.unwrap();
        let first_pid = pid(&mut lease, &cancel).await;
        let first = ProcessObserver::new(first_pid);
        let report = lease.release(ReleasePolicy::RetainIfHealthy, &cancel).await;
        assert!(report.observations[0].connect_total_ms.is_some());
        let mut lease = manager
            .acquire(&scope, &config, directory.path(), &cancel)
            .await
            .unwrap();
        lease.list_tools(&cancel).await.unwrap();
        assert_eq!(pid(&mut lease, &cancel).await, first_pid);
        let report = lease.release(ReleasePolicy::RetainIfHealthy, &cancel).await;
        assert!(report.connection_reused);
        assert_eq!(report.observations[0].connect_total_ms, None);
        assert_eq!(spawn_count(&log), 1);
        assert_eq!(
            events(&log).lines().filter(|line| *line == "list").count(),
            2
        );
        let mut changed = config.clone();
        changed["args"] = json!(["fixture-literal-argument"]);
        changed["env"]
            .as_array_mut()
            .unwrap()
            .push(json!({"name":"FIXTURE_PREVIOUS_PID","value":first_pid.to_string()}));
        let changed_scope =
            Scope::from_host(&owner(), "acp", conversation, &changed, directory.path()).unwrap();
        let mut lease = manager
            .acquire(&changed_scope, &changed, directory.path(), &cancel)
            .await
            .unwrap();
        first.assert_stopped().await;
        assert!(events(&log)
            .lines()
            .any(|line| line == "previous_stopped true"));
        assert!(!events(&log)
            .lines()
            .any(|line| line == "previous_stopped false"));
        assert_eq!(spawn_count(&log), 2);
        lease.list_tools(&cancel).await.unwrap();
        let second = ProcessObserver::new(pid(&mut lease, &cancel).await);
        lease
            .call_tool("exit", json!({}), None, &cancel)
            .await
            .unwrap();
        second.assert_stopped().await;
        // Cache the wrapper after a complete reply, then discover real EOF by
        // the mandatory fresh list on the next lease. No business call replay.
        lease.release(ReleasePolicy::RetainIfHealthy, &cancel).await;
        let mut stale = manager
            .acquire(&changed_scope, &changed, directory.path(), &cancel)
            .await
            .unwrap();
        assert!(stale.connection_reused);
        assert_eq!(
            stale.list_tools(&cancel).await.err(),
            Some(ClientError::Catalog)
        );
        assert!(
            !stale
                .release(ReleasePolicy::RetainIfHealthy, &cancel)
                .await
                .retained
        );
        assert_eq!(spawn_count(&log), 2);
        let mut fresh = manager
            .acquire(&changed_scope, &changed, directory.path(), &cancel)
            .await
            .unwrap();
        fresh.list_tools(&cancel).await.unwrap();
        let third = ProcessObserver::new(pid(&mut fresh, &cancel).await);
        let report = fresh.release(ReleasePolicy::Close, &cancel).await;
        assert_eq!(report.error, None);
        third.assert_stopped().await;
        assert_eq!(spawn_count(&log), 3);
        private_free(&report.observations);
        assert_eq!(manager.shutdown().await.status, RetirementStatus::Quiescent);
    }

    #[tokio::test]
    async fn dropped_stdio_call_keeps_busy_until_cleanup_then_preserves_deferred_observation() {
        let directory = tempfile::tempdir().unwrap();
        let log = directory.path().join("events.log");
        let conversation = Uuid::new_v4();
        let config = stdio_config(&log, conversation, "normal");
        let scope =
            Scope::from_host(&owner(), "acp", conversation, &config, directory.path()).unwrap();
        let manager = Manager::new(Handle::current());
        let cancel = AtomicBool::new(false);
        let mut lease = manager
            .acquire(&scope, &config, directory.path(), &cancel)
            .await
            .unwrap();
        lease.list_tools(&cancel).await.unwrap();
        let process = ProcessObserver::new(pid(&mut lease, &cancel).await);
        let mut call = Box::pin(lease.call_tool("hang", json!({}), None, &cancel));
        tokio::select! {
            _ = &mut call => panic!("fixture returned instead of waiting"),
            _ = event(&log, "hang_call") => {}
        }
        assert_error(
            manager
                .acquire(&scope, &config, directory.path(), &cancel)
                .await,
            Error::Busy,
        );
        drop(call);
        assert!(!lease.is_current());
        drop(lease);
        assert_error(
            manager
                .acquire(&scope, &config, directory.path(), &cancel)
                .await,
            Error::Busy,
        );
        assert_eq!(spawn_count(&log), 1);
        cleanup_finished(&manager).await;
        process.assert_stopped().await;
        assert_error(
            manager
                .acquire(&scope, &config, directory.path(), &cancel)
                .await,
            Error::ObservationPending,
        );
        let observations = manager.take_deferred_observations().unwrap();
        assert_eq!(observations.len(), 1);
        assert!(observations[0].connect_total_ms.is_some());
        assert!(observations[0].close_total_ms.is_some());
        assert_eq!(observations[0].call_attempts, 2); // pid + explicit hang
        assert_eq!(observations[0].call_interrupted, 1);
        private_free(&observations);
        let lease = manager
            .acquire(&scope, &config, directory.path(), &cancel)
            .await
            .unwrap();
        assert_eq!(spawn_count(&log), 2);
        assert!(!lease.release(ReleasePolicy::Close, &cancel).await.retained);
        assert_eq!(manager.shutdown().await.status, RetirementStatus::Quiescent);
    }

    #[tokio::test]
    async fn dropped_stdio_acquire_quarantines_unknown_cleanup_without_second_spawn() {
        let directory = tempfile::tempdir().unwrap();
        let log = directory.path().join("events.log");
        let conversation = Uuid::new_v4();
        let config = stdio_config(&log, conversation, "hang_init");
        let scope =
            Scope::from_host(&owner(), "acp", conversation, &config, directory.path()).unwrap();
        let manager = Manager::new(Handle::current());
        let cancel = AtomicBool::new(false);
        let mut acquire = Box::pin(manager.acquire(&scope, &config, directory.path(), &cancel));
        tokio::select! {
            _ = &mut acquire => panic!("fixture initialize unexpectedly returned"),
            _ = event(&log, "initialize") => {}
        }
        let process = ProcessObserver::new(spawned_pid(&log));
        assert_error(
            manager
                .acquire(&scope, &config, directory.path(), &cancel)
                .await,
            Error::Busy,
        );
        drop(acquire);
        // Lower stdio Drop stops its own child, but the manager cannot observe
        // its hidden cleanup result: keep quarantine even after our test's OS
        // observer proves this particular fixture was reaped.
        assert_error(
            manager
                .acquire(&scope, &config, directory.path(), &cancel)
                .await,
            Error::ObservationPending,
        );
        let observations = manager.take_deferred_observations().unwrap();
        assert_eq!(observations.len(), 1);
        assert!(observations[0].connect_total_ms.is_some());
        private_free(&observations);
        assert_error(
            manager
                .acquire(&scope, &config, directory.path(), &cancel)
                .await,
            Error::Quarantined,
        );
        process.assert_stopped().await;
        assert_eq!(spawn_count(&log), 1);
        let report = manager.shutdown().await;
        assert_eq!(report.status, RetirementStatus::Quarantined);
        assert!(report.error.unwrap().requires_agent_restart());
        assert_eq!(spawn_count(&log), 1);
    }

    fn catalog_policy() -> CatalogPolicy {
        CatalogPolicy::reviewed_stdio(Duration::from_secs(30)).unwrap()
    }

    fn catalog_scope(directory: &Path, config: &Value, conversation: Uuid) -> Scope {
        Scope::from_host(&owner(), "catalog-acp", conversation, config, directory).unwrap()
    }

    #[test]
    fn catalog_policy_ttl_is_bounded_and_registry_reads_are_not_whitelisted() {
        assert!(CatalogPolicy::reviewed_stdio(Duration::ZERO).is_err());
        assert!(CatalogPolicy::reviewed_stdio(Duration::from_secs(31)).is_err());
        assert!(CatalogPolicy::reviewed_stdio(Duration::from_secs(30)).is_ok());
        let payload = json!({"content":[],"structuredContent":{"profile":{},"domains":[],"needs_domain_setup":false}});
        assert!(reviewed_catalog_read("get_learner_context", &payload));
        for name in [
            "list_training_library",
            "get_training",
            "get_conversation_context",
            "opaque_unreviewed",
        ] {
            assert!(!reviewed_catalog_read(name, &payload));
        }
        assert!(!reviewed_catalog_read(
            "get_learner_context",
            &json!({"content":[],"isError":true,"structuredContent":{}})
        ));
    }

    #[tokio::test]
    async fn catalog_stdio_hit_acknowledges_only_reviewed_post_call_not_real_notification() {
        let directory = tempfile::tempdir().unwrap();
        let log = directory.path().join("catalog.log");
        let conversation = Uuid::new_v4();
        let config = stdio_config(&log, conversation, "catalog_normal");
        let scope = catalog_scope(directory.path(), &config, conversation);
        let manager = Manager::new_with_catalog_policy(Handle::current(), catalog_policy());
        let cancel = AtomicBool::new(false);
        let mut lease = manager
            .acquire(&scope, &config, directory.path(), &cancel)
            .await
            .unwrap();
        let entries = lease.list_tools(&cancel).await.unwrap();
        assert_eq!(entries.len(), 3);
        let before = lease.client.as_ref().unwrap().catalog_state();
        lease
            .call_tool("get_learner_context", json!({}), None, &cancel)
            .await
            .unwrap();
        let after = lease.client.as_ref().unwrap().catalog_state();
        assert_eq!(before.notification_revision, after.notification_revision);
        assert_eq!(before.post_call_revision + 1, after.post_call_revision);
        assert!(lease.client.as_mut().unwrap().take_tools_changed()); // global marker preserved
        assert!(!lease.take_tools_changed()); // reviewed local acknowledgment only
        let first = lease.release(ReleasePolicy::RetainIfHealthy, &cancel).await;
        assert_eq!(first.catalog.cache_misses, 1);
        let mut lease = manager
            .acquire(&scope, &config, directory.path(), &cancel)
            .await
            .unwrap();
        assert_eq!(lease.list_tools(&cancel).await.unwrap().len(), 3);
        let second = lease.release(ReleasePolicy::RetainIfHealthy, &cancel).await;
        assert_eq!(second.catalog.cache_hits, 1);
        assert_eq!(second.catalog.cache_misses, 0);
        assert_eq!(second.observations[0].catalog_attempts, 0);
        assert_eq!(second.observations[0].catalog_total_ms, None);
        assert_eq!(
            events(&log)
                .lines()
                .filter(|line| *line == "initialize")
                .count(),
            1
        );
        assert_eq!(
            events(&log).lines().filter(|line| *line == "list").count(),
            2
        ); // one 2-page snapshot
        assert_eq!(
            events(&log).lines().filter(|line| *line == "call").count(),
            1
        );
        manager.shutdown().await;
    }

    #[tokio::test]
    async fn catalog_http_unobservable_idle_events_keep_wire_refresh_each_lease() {
        let directory = tempfile::tempdir().unwrap();
        let server = http().await;
        let conversation = Uuid::new_v4();
        let config = config(&server.url, conversation);
        let scope = catalog_scope(directory.path(), &config, conversation);
        let manager = Manager::new_with_catalog_policy(Handle::current(), catalog_policy());
        let cancel = AtomicBool::new(false);
        for _ in 0..2 {
            let mut lease = manager
                .acquire(&scope, &config, directory.path(), &cancel)
                .await
                .unwrap();
            lease.list_tools(&cancel).await.unwrap();
            let report = lease.release(ReleasePolicy::RetainIfHealthy, &cancel).await;
            assert_eq!(report.catalog.cache_hits, 0);
            assert_eq!(
                report.catalog.last_miss_reason,
                Some(CatalogMissReason::IdleNotificationsUnobservable)
            );
        }
        assert_eq!(count(&server, "initialize"), 1);
        assert_eq!(count(&server, "tools/list"), 2);
        manager.shutdown().await;
    }

    #[tokio::test]
    async fn catalog_unreviewed_or_error_result_never_uses_annotation_as_authority() {
        for mode in ["catalog_normal", "catalog_tool_error"] {
            let directory = tempfile::tempdir().unwrap();
            let log = directory.path().join("catalog.log");
            let conversation = Uuid::new_v4();
            let config = stdio_config(&log, conversation, mode);
            let scope = catalog_scope(directory.path(), &config, conversation);
            let manager = Manager::new_with_catalog_policy(Handle::current(), catalog_policy());
            let cancel = AtomicBool::new(false);
            let mut lease = manager
                .acquire(&scope, &config, directory.path(), &cancel)
                .await
                .unwrap();
            let entries = lease.list_tools(&cancel).await.unwrap();
            let (name, reason) = if mode == "catalog_normal" {
                assert_eq!(
                    entries
                        .iter()
                        .find(|tool| tool.name == "opaque_unreviewed")
                        .unwrap()
                        .annotations["readOnlyHint"],
                    true
                );
                ("opaque_unreviewed", CatalogMissReason::UnreviewedCall)
            } else {
                ("get_learner_context", CatalogMissReason::ToolError)
            };
            let envelope = lease
                .call_tool(name, json!({}), None, &cancel)
                .await
                .unwrap();
            assert_eq!(envelope["isError"], mode == "catalog_tool_error");
            assert!(lease.take_tools_changed());
            assert!(lease.take_tools_changed()); // sticky, polling never acknowledges
            lease.list_tools(&cancel).await.unwrap();
            let report = lease.release(ReleasePolicy::Close, &cancel).await;
            assert_eq!(report.catalog.cache_misses, 2);
            assert_eq!(report.catalog.last_miss_reason, Some(reason));
            assert_eq!(report.observations[0].call_transport_successes, 1);
            assert_eq!(
                report.observations[0].tool_result_errors,
                if mode == "catalog_tool_error" { 1 } else { 0 }
            );
            assert_eq!(
                events(&log).lines().filter(|line| *line == "call").count(),
                1
            );
            manager.shutdown().await;
        }
    }

    #[tokio::test]
    async fn catalog_expiry_and_explicit_permission_boundary_require_fresh_snapshot() {
        let directory = tempfile::tempdir().unwrap();
        let log = directory.path().join("catalog.log");
        let conversation = Uuid::new_v4();
        let config = stdio_config(&log, conversation, "catalog_normal");
        let scope = catalog_scope(directory.path(), &config, conversation);
        let manager = Manager::new_with_catalog_policy(Handle::current(), catalog_policy());
        let cancel = AtomicBool::new(false);
        let mut lease = manager
            .acquire(&scope, &config, directory.path(), &cancel)
            .await
            .unwrap();
        lease.list_tools(&cancel).await.unwrap();
        // Deterministic elapsed-age injection, not a latency benchmark.
        lease.catalog.as_mut().unwrap().fetched_at = Instant::now() - Duration::from_secs(31);
        assert!(lease.take_tools_changed());
        lease.list_tools(&cancel).await.unwrap();
        assert_eq!(
            lease.catalog_observation.last_miss_reason,
            Some(CatalogMissReason::Expired)
        );
        lease.require_catalog_refresh();
        assert!(lease.take_tools_changed());
        lease.list_tools(&cancel).await.unwrap();
        assert_eq!(
            lease.catalog_observation.last_miss_reason,
            Some(CatalogMissReason::PermissionBoundary)
        );
        let report = lease.release(ReleasePolicy::Close, &cancel).await;
        assert_eq!(report.catalog.cache_hits, 0);
        assert_eq!(report.catalog.cache_misses, 3);
        assert_eq!(
            events(&log).lines().filter(|line| *line == "list").count(),
            6
        );
        manager.shutdown().await;
    }

    #[tokio::test]
    async fn catalog_notification_during_pagination_rejects_every_partial_page() {
        let directory = tempfile::tempdir().unwrap();
        let log = directory.path().join("catalog.log");
        let conversation = Uuid::new_v4();
        let config = stdio_config(&log, conversation, "catalog_notify_fetch");
        let scope = catalog_scope(directory.path(), &config, conversation);
        let manager = Manager::new_with_catalog_policy(Handle::current(), catalog_policy());
        let cancel = AtomicBool::new(false);
        let mut lease = manager
            .acquire(&scope, &config, directory.path(), &cancel)
            .await
            .unwrap();
        assert_eq!(
            lease.list_tools(&cancel).await.err(),
            Some(ClientError::Catalog)
        );
        assert!(lease.catalog.is_none());
        let report = lease.release(ReleasePolicy::RetainIfHealthy, &cancel).await;
        assert!(!report.retained);
        assert_eq!(report.catalog.cache_hits, 0);
        assert_eq!(
            report.catalog.last_miss_reason,
            Some(CatalogMissReason::FetchChanged)
        );
        assert_eq!(
            events(&log).lines().filter(|line| *line == "list").count(),
            2
        );
        assert_eq!(
            events(&log).lines().filter(|line| *line == "call").count(),
            0
        );
        manager.shutdown().await;
    }

    #[tokio::test]
    async fn catalog_changed_type_while_approval_pending_rejects_old_prepared_call() {
        use crate::{
            call_validation::{self, VerifiedFields},
            catalog::ToolCatalog,
            executor::{self, Preparation},
            session_store::SessionStore,
        };
        let directory = tempfile::tempdir().unwrap();
        let log = directory.path().join("catalog.log");
        let trigger = directory.path().join("notify.trigger");
        let conversation = Uuid::new_v4();
        let mut config = stdio_config(&log, conversation, "catalog_idle_notify");
        config["env"]
            .as_array_mut()
            .unwrap()
            .push(json!({"name":"FIXTURE_NOTIFY_TRIGGER","value":trigger}));
        let scope = catalog_scope(directory.path(), &config, conversation);
        let manager = Manager::new_with_catalog_policy(Handle::current(), catalog_policy());
        let cancel = AtomicBool::new(false);
        let mut lease = manager
            .acquire(&scope, &config, directory.path(), &cancel)
            .await
            .unwrap();
        let mut catalog = ToolCatalog::new("catalog-epoch".into()).unwrap();
        catalog
            .replace_server_tools(
                "catalog-epoch",
                "tutor",
                lease.list_tools(&cancel).await.unwrap(),
            )
            .unwrap();
        let mut store = SessionStore::create(directory.path(), &owner(), &"a".repeat(64)).unwrap();
        let turn = store.begin_turn("Read my own fixture profile").unwrap();
        let fields = VerifiedFields::new("catalog-epoch".into(), store.journal()).unwrap();
        let call = call_validation::validate_call(
            &catalog,
            "catalog-epoch",
            "tutor/get_learner_context",
            &json!({}),
            &fields,
            Uuid::new_v4(),
        )
        .unwrap();
        let operation = call.operation_id().to_owned();
        let Preparation::Ready(prepared) =
            executor::prepare(store.journal_mut(), &turn, call, None).unwrap()
        else {
            panic!("fresh call expected");
        };
        // Approval is pending at the host. Server changes a type and emits a
        // real idle notification; the old allow response cannot authorize it.
        std::fs::write(&trigger, b"notify").unwrap();
        tokio::time::timeout(Duration::from_secs(3), async {
            while !lease.take_tools_changed() {
                tokio::time::sleep(Duration::from_millis(2)).await;
            }
        })
        .await
        .unwrap();
        assert!(lease.take_tools_changed());
        catalog
            .replace_server_tools(
                "catalog-epoch",
                "tutor",
                lease.list_tools(&cancel).await.unwrap(),
            )
            .unwrap();
        assert_eq!(
            catalog
                .resolve("catalog-epoch", "tutor/get_learner_context")
                .unwrap()
                .input_schema["properties"]["flavor"]["type"],
            "integer"
        );
        let approval = json!({"outcome":{"outcome":"selected","optionId":"allow"}});
        assert_eq!(
            prepared
                .execute(
                    store.journal_mut(),
                    &catalog,
                    "catalog-epoch",
                    &approval,
                    &mut lease,
                    &cancel
                )
                .await
                .err(),
            Some(executor::Error::Stale)
        );
        assert_eq!(
            store
                .journal()
                .operation(&operation)
                .unwrap()
                .unwrap()
                .state,
            crate::journal::OperationState::Rejected
        );
        assert_eq!(
            events(&log).lines().filter(|line| *line == "call").count(),
            0
        );
        let report = lease.release(ReleasePolicy::Close, &cancel).await;
        assert_eq!(
            report.catalog.last_miss_reason,
            Some(CatalogMissReason::RealNotification)
        );
        manager.shutdown().await;
    }

    #[tokio::test]
    async fn catalog_scope_and_generation_changes_cannot_keep_previous_snapshot() {
        let directory = tempfile::tempdir().unwrap();
        let log = directory.path().join("catalog.log");
        let conversation = Uuid::new_v4();
        let config = stdio_config(&log, conversation, "catalog_normal");
        let scope = catalog_scope(directory.path(), &config, conversation);
        let manager = Manager::new_with_catalog_policy(Handle::current(), catalog_policy());
        let cancel = AtomicBool::new(false);
        let mut lease = manager
            .acquire(&scope, &config, directory.path(), &cancel)
            .await
            .unwrap();
        lease.list_tools(&cancel).await.unwrap();
        lease.release(ReleasePolicy::RetainIfHealthy, &cancel).await;
        manager.invalidate();
        manager
            .wait_ready(&cancel, Duration::from_secs(3))
            .await
            .unwrap();
        let mut lease = manager
            .acquire(&scope, &config, directory.path(), &cancel)
            .await
            .unwrap();
        lease.list_tools(&cancel).await.unwrap();
        assert_eq!(lease.catalog_observation.cache_hits, 0);
        lease.release(ReleasePolicy::RetainIfHealthy, &cancel).await;
        let mut another = owner();
        another.account_id.push_str("-other");
        let changed = Scope::from_host(
            &another,
            "catalog-acp",
            conversation,
            &config,
            directory.path(),
        )
        .unwrap();
        let mut lease = manager
            .acquire(&changed, &config, directory.path(), &cancel)
            .await
            .unwrap();
        lease.list_tools(&cancel).await.unwrap();
        assert_eq!(lease.catalog_observation.cache_hits, 0);
        assert_eq!(spawn_count(&log), 3);
        assert_eq!(
            events(&log).lines().filter(|line| *line == "list").count(),
            6
        );
        let report = lease.release(ReleasePolicy::Close, &cancel).await;
        let exported = serde_json::to_string(&report.catalog).unwrap();
        assert!(!exported.contains("fixture-private"));
        assert!(!exported.contains("inputSchema"));
        assert!(!exported.contains("catalog-acp"));
        manager.shutdown().await;
    }

    #[tokio::test]
    async fn catalog_after_approval_refreshes_http_and_unobservable_stdio_only() {
        let directory = tempfile::tempdir().unwrap();
        let server = http().await;
        let conversation = Uuid::new_v4();
        let config = config(&server.url, conversation);
        let scope = catalog_scope(directory.path(), &config, conversation);
        let manager = Manager::new_with_catalog_policy(Handle::current(), catalog_policy());
        let cancel = AtomicBool::new(false);
        let mut lease = manager
            .acquire(&scope, &config, directory.path(), &cancel)
            .await
            .unwrap();
        lease.list_tools(&cancel).await.unwrap();
        assert!(!lease.take_tools_changed()); // young HTTP stamp alone misses idle events
        assert!(lease.require_catalog_refresh_after_approval());
        assert!(lease.take_tools_changed());
        lease.list_tools(&cancel).await.unwrap();
        assert_eq!(count(&server, "tools/list"), 2);
        let report = lease.release(ReleasePolicy::Close, &cancel).await;
        assert_eq!(report.catalog.cache_hits, 0);
        assert_eq!(report.catalog.cache_misses, 2);
        manager.shutdown().await;

        for mode in ["catalog_normal", "catalog_no_cap"] {
            let directory = tempfile::tempdir().unwrap();
            let log = directory.path().join("approval.log");
            let conversation = Uuid::new_v4();
            let config = stdio_config(&log, conversation, mode);
            let scope = catalog_scope(directory.path(), &config, conversation);
            let manager = Manager::new_with_catalog_policy(Handle::current(), catalog_policy());
            let mut lease = manager
                .acquire(&scope, &config, directory.path(), &cancel)
                .await
                .unwrap();
            lease.list_tools(&cancel).await.unwrap();
            let needs_wire_refresh = mode == "catalog_no_cap";
            assert_eq!(
                lease.require_catalog_refresh_after_approval(),
                needs_wire_refresh
            );
            assert_eq!(lease.take_tools_changed(), needs_wire_refresh);
            lease.list_tools(&cancel).await.unwrap();
            let report = lease.release(ReleasePolicy::Close, &cancel).await;
            assert_eq!(
                report.catalog.cache_hits,
                if needs_wire_refresh { 0 } else { 1 }
            );
            assert_eq!(
                events(&log)
                    .lines()
                    .filter(|event| *event == "list")
                    .count(),
                if needs_wire_refresh { 4 } else { 2 }
            );
            manager.shutdown().await;
        }
    }

    #[tokio::test]
    async fn catalog_disabled_keeps_existing_approval_and_result_path() {
        let directory = tempfile::tempdir().unwrap();
        let server = http().await;
        let conversation = Uuid::new_v4();
        let config = config(&server.url, conversation);
        let scope = catalog_scope(directory.path(), &config, conversation);
        let manager = Manager::new(Handle::current());
        let cancel = AtomicBool::new(false);
        let mut lease = manager
            .acquire(&scope, &config, directory.path(), &cancel)
            .await
            .unwrap();
        lease.list_tools(&cancel).await.unwrap();
        assert!(!lease.require_catalog_refresh_after_approval());
        assert!(!lease.take_tools_changed());
        let reply = lease
            .call_tool("get_learner_context", json!({}), None, &cancel)
            .await
            .unwrap();
        assert!(reply.is_object());
        assert!(lease.take_tools_changed()); // original conservative post-call marker
        assert!(lease.catalog.is_none());
        assert!(lease.catalog_invalidated.is_none());
        assert_eq!(count(&server, "tools/list"), 1);
        assert_eq!(count(&server, "tools/call"), 1);
        let report = lease.release(ReleasePolicy::Close, &cancel).await;
        assert_eq!(report.catalog.cache_hits, 0);
        manager.shutdown().await;
    }

    #[tokio::test]
    #[ignore = "Requires an explicitly selected local Tutor binary; disposable profile only, no model"]
    async fn catalog_actual_tutor_development_probe() {
        use crate::{
            call_validation::{self, VerifiedFields},
            catalog::ToolCatalog,
            executor::{self, Preparation},
            journal::OperationState,
            session_store::SessionStore,
        };
        let server = std::path::PathBuf::from(
            std::env::var_os("TUTOR_CATALOG_PROBE_SERVER").expect("explicit local Tutor binary"),
        );
        assert!(server.is_absolute() && server.is_file());
        let output = std::path::PathBuf::from(
            std::env::var_os("TUTOR_CATALOG_PROBE_OUTPUT").expect("new report path"),
        );
        assert!(output.is_absolute() && !output.exists());
        let directory = tempfile::tempdir().unwrap();
        let conversation = Uuid::new_v4();
        let config = json!({"name":"tutor", "command":server,
            "args":["--local","--data-dir",directory.path().join("disposable-server")],
            "env":[{"name":"TUTOR_CONVERSATION_ID","value":conversation.to_string()}]});
        let scope = catalog_scope(directory.path(), &config, conversation);
        let manager = Manager::new_with_catalog_policy(Handle::current(), catalog_policy());
        let cancel = AtomicBool::new(false);
        let mut store =
            SessionStore::create(&directory.path().join("journal"), &owner(), &"b".repeat(64))
                .unwrap();
        store.bind_conversation(conversation).unwrap();
        let mut observations = Vec::new();
        let mut same_entries: Option<Vec<ToolEntry>> = None;
        let mut checks = Vec::new();
        for index in 0..2 {
            let turn = store
                .begin_turn("Development probe: consult the disposable local profile")
                .unwrap();
            let started = Instant::now();
            let mut lease = manager
                .acquire(&scope, &config, directory.path(), &cancel)
                .await
                .unwrap();
            let connected_ms = started.elapsed().as_secs_f64() * 1000.0;
            let transport_state = lease.client.as_ref().unwrap().catalog_state();
            let list_started = Instant::now();
            let entries = lease.list_tools(&cancel).await.unwrap();
            let list_ms = list_started.elapsed().as_secs_f64() * 1000.0;
            checks.push(json!({"name":"real_server_declares_list_changed", "passed":transport_state.notifications_supported}));
            checks.push(json!({"name":"stdio_idle_reader_observable", "passed":transport_state.idle_notifications_observable}));
            checks.push(json!({"name":"nonempty_actual_catalog", "passed":!entries.is_empty()}));
            let same = same_entries.as_ref().is_none_or(|prior| *prior == entries);
            checks.push(json!({"name":"exact_same_catalog_entries", "passed":same}));
            same_entries = Some(entries.clone());
            let binding = "real-catalog-development-epoch";
            let mut catalog = ToolCatalog::new(binding.into()).unwrap();
            catalog
                .replace_server_tools(binding, "tutor", entries.clone())
                .unwrap();
            let fields = VerifiedFields::new(binding.into(), store.journal()).unwrap();
            let call = call_validation::validate_call(
                &catalog,
                binding,
                "tutor/get_learner_context",
                &json!({}),
                &fields,
                Uuid::new_v4(),
            )
            .unwrap();
            let operation = call.operation_id().to_owned();
            let Preparation::Ready(prepared) =
                executor::prepare(store.journal_mut(), &turn, call, None).unwrap()
            else {
                panic!("new operation required");
            };
            // Controlled approval fixture, not an assertion about the ACP/UI.
            let approval = json!({"outcome":{"outcome":"selected","optionId":"allow"}});
            let forced = lease.require_catalog_refresh_after_approval();
            if lease.take_tools_changed() {
                catalog
                    .replace_server_tools(
                        binding,
                        "tutor",
                        lease.list_tools(&cancel).await.unwrap(),
                    )
                    .unwrap();
            }
            let executed = prepared
                .execute(
                    store.journal_mut(),
                    &catalog,
                    binding,
                    &approval,
                    &mut lease,
                    &cancel,
                )
                .await
                .unwrap();
            let payload = call_validation::result_payload(&executed.result).unwrap();
            checks.push(json!({"name":"actual_profile_payload", "passed":payload.get("profile").is_some_and(Value::is_object) && payload.get("domains").is_some_and(Value::is_array) && payload.get("needs_domain_setup").is_some_and(Value::is_boolean)}));
            checks.push(json!({"name":"durable_completed_operation", "passed":store.journal().operation(&operation).unwrap().unwrap().state==OperationState::Completed}));
            store
                .finish_turn(&turn, crate::session_store::Completion::Complete)
                .unwrap();
            let released = lease.release(ReleasePolicy::RetainIfHealthy, &cancel).await;
            checks.push(json!({"name":"expected_catalog_reuse", "passed":if index==0 { released.catalog.cache_misses==1 && released.catalog.cache_hits==0 } else { released.catalog.cache_hits==1 && released.catalog.cache_misses==0 }}));
            checks.push(json!({"name":"expected_connection_reuse", "passed":released.connection_reused==(index==1) && released.retained}));
            observations.push(json!({"index":index,"toolsCount":entries.len(),"connectOrLeaseMs":connected_ms,"listOrCacheMs":list_ms,"afterApprovalForcedRefresh":forced,"catalog":released.catalog,"mcp":released.observations,"restartRequired":released.error.is_some_and(|e|e.requires_agent_restart())}));
        }
        let shutdown = manager.shutdown().await;
        checks.push(json!({"name":"shutdown_quiescent", "passed":shutdown.status==RetirementStatus::Quiescent && shutdown.error.is_none()}));
        let passed = checks.iter().all(|check| check["passed"] == true);
        let report = json!({"schemaVersion":1,"scope":"Development: resident manager and real executor with unchanged local Tutor on disposable data. No ACP/UI/model/Windows/P2000 qualification.","status":if passed {"checks_passed"}else{"checks_failed"},"llmInvoked":false,"windowsP2000Qualified":false,"pedagogyQualified":false,"usefulResponseMeasured":false,"cacheTtlSeconds":30,"observations":observations,"checks":checks,"limits":["Only two sequential profile consultations; descriptive timings, no p95 or end-user latency.","Approval is a controlled fixture; no actual ACP host or UI.","Observed declared listChanged and cache reuse, not an induced real server list-change event.","Disposable local identity; no institution/account authentication qualification."]});
        use std::io::Write;
        let mut file = std::fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .open(output)
            .unwrap();
        file.write_all(serde_json::to_string_pretty(&report).unwrap().as_bytes())
            .unwrap();
        assert!(passed, "see new development report for failed checks");
    }
}
