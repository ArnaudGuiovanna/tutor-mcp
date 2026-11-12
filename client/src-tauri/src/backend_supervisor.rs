// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT
//! Supervised reconnection of an unexpectedly stopped harness, and the
//! "limited until T" state reported by providers. Neither ever replays a
//! prompt: sessions are loaded again on demand by the next learner message.
use super::*;
use crate::acp::ErrorKind;
use std::collections::VecDeque;
use std::time::{Duration, Instant};

const BACKOFF_CAP: Duration = Duration::from_secs(60);
const HEALTHY_AFTER: Duration = Duration::from_secs(60);
const FAILURE_WINDOW: Duration = Duration::from_secs(5 * 60);
const MAX_FAILURES: usize = 5;
const MAX_LIMIT: Duration = Duration::from_secs(7 * 24 * 3600);
/// Background memory maintenance only runs after this much learner silence.
pub(super) const MAINTENANCE_IDLE: Duration = Duration::from_secs(120);

#[derive(Clone, Copy, PartialEq, Eq, Debug, Default)]
pub(super) enum ReconnectState {
    #[default]
    Idle,
    Waiting,
    Connecting,
    Failed,
}

impl ReconnectState {
    fn as_str(self) -> &'static str {
        match self {
            Self::Idle => "idle",
            Self::Waiting => "waiting",
            Self::Connecting => "connecting",
            Self::Failed => "failed",
        }
    }
}

#[derive(Clone, Copy, Debug)]
pub(super) struct AgentLimit {
    pub kind: ErrorKind,
    pub until: chrono::DateTime<chrono::Utc>,
}

#[derive(Default)]
pub(super) struct Supervisor {
    attempt: u32,
    failures: VecDeque<Instant>,
    connected_at: Option<Instant>,
    state: ReconnectState,
    next_attempt_at: Option<chrono::DateTime<chrono::Utc>>,
    cause: Option<String>,
    /// Bumped by every explicit user connection choice; stale loops stop.
    epoch: u64,
    /// Identifies the reconnection loop allowed to change `state`. A loop
    /// that finished its work never overwrites the state of a newer one.
    run: u64,
    limit: Option<AgentLimit>,
    last_turn: Option<Instant>,
}

impl Supervisor {
    /// Count one failure; true when the retry budget is exhausted.
    fn record_failure(&mut self, cause: String, now: Instant) -> bool {
        if self
            .connected_at
            .take()
            .is_some_and(|since| now.duration_since(since) >= HEALTHY_AFTER)
        {
            self.attempt = 0;
            self.failures.clear();
        }
        self.failures.push_back(now);
        while self
            .failures
            .front()
            .is_some_and(|first| now.duration_since(*first) > FAILURE_WINDOW)
        {
            self.failures.pop_front();
        }
        self.cause = Some(cause);
        if self.failures.len() >= MAX_FAILURES {
            self.state = ReconnectState::Failed;
            self.next_attempt_at = None;
            true
        } else {
            false
        }
    }

    /// A live process ends the reconnection state. After a definitive
    /// failure the retry budget starts again; otherwise the attempt count is
    /// kept until a healthy minute so that a crash loop still backs off.
    fn record_connection(&mut self, now: Instant) {
        if self.state == ReconnectState::Failed {
            self.attempt = 0;
            self.failures.clear();
        }
        self.connected_at = Some(now);
        self.state = ReconnectState::Idle;
        self.next_attempt_at = None;
        self.cause = None;
    }

    /// Enter the waiting state for the next attempt.
    fn schedule_next(&mut self) -> (Duration, u64) {
        let delay = backoff_delay(self.attempt, rand::random::<f64>() * 0.25);
        self.attempt = self.attempt.saturating_add(1);
        self.state = ReconnectState::Waiting;
        self.next_attempt_at =
            Some(chrono::Utc::now() + chrono::Duration::from_std(delay).unwrap_or_default());
        (delay, self.epoch)
    }

    /// Claim the reconnection state for a new loop.
    fn start_run(&mut self) -> u64 {
        self.run = self.run.wrapping_add(1);
        self.run
    }

    fn owns(&self, epoch: u64, run: u64) -> bool {
        self.epoch == epoch && self.run == run
    }

    /// The loop `run` stops without a definitive failure.
    fn abandon(&mut self, epoch: u64, run: u64) {
        if self.owns(epoch, run) && self.state != ReconnectState::Failed {
            self.state = ReconnectState::Idle;
            self.next_attempt_at = None;
        }
    }

    /// A restart requested by Tutor itself (unconfirmed cancellation,
    /// interrupted preparation) is not a crash: it consumes no retry budget
    /// and also leaves a definitive failure. None when a loop already runs.
    fn schedule_restart(&mut self, cause: String) -> Option<(Duration, u64, u64)> {
        if matches!(
            self.state,
            ReconnectState::Waiting | ReconnectState::Connecting
        ) {
            return None;
        }
        if self.state == ReconnectState::Failed {
            self.attempt = 0;
            self.failures.clear();
        }
        self.connected_at = None;
        self.cause = Some(cause);
        self.state = ReconnectState::Waiting;
        self.next_attempt_at = Some(chrono::Utc::now());
        Some((Duration::ZERO, self.epoch, self.start_run()))
    }

    #[cfg(test)]
    pub(super) fn failures_for_test(&self) -> usize {
        self.failures.len()
    }

    fn view(&self) -> Value {
        json!({"state":self.state.as_str(),"attempt":self.attempt,
            "nextAttemptAt":self.next_attempt_at.map(|at| at.to_rfc3339_opts(chrono::SecondsFormat::Millis, true)),
            "cause":self.cause})
    }

    fn active_limit(&mut self) -> Option<AgentLimit> {
        if self
            .limit
            .is_some_and(|limit| limit.until <= chrono::Utc::now())
        {
            self.limit = None;
        }
        self.limit
    }
}

/// 1, 2, 4, 8, 16… seconds capped at 60, plus up to 25 % random jitter.
pub(super) fn backoff_delay(attempt: u32, jitter: f64) -> Duration {
    let base = Duration::from_secs(1_u64 << attempt.min(6)).min(BACKOFF_CAP);
    base.mul_f64(1.0 + jitter.clamp(0.0, 0.25)).min(BACKOFF_CAP)
}

/// Default pause when the provider gives no reset time.
fn default_limit(kind: ErrorKind) -> Duration {
    match kind {
        ErrorKind::Overloaded => Duration::from_secs(20),
        ErrorKind::UsageLimit => Duration::from_secs(5 * 60),
        _ => Duration::from_secs(60),
    }
}

impl Backend {
    pub(super) fn supervisor_view(&self) -> (Value, Option<AgentLimit>) {
        let mut supervisor = self.supervisor.lock().unwrap();
        (supervisor.view(), supervisor.active_limit())
    }

    /// An explicit user choice (connect or disconnect) resets the budget.
    pub(super) fn reset_supervisor(&self) {
        let mut supervisor = self.supervisor.lock().unwrap();
        supervisor.epoch += 1;
        supervisor.attempt = 0;
        supervisor.failures.clear();
        supervisor.state = ReconnectState::Idle;
        supervisor.next_attempt_at = None;
        supervisor.cause = None;
    }

    /// A successful connection, automatic or manual, ends any reconnection
    /// state, including a definitive failure, and is announced to the UI.
    pub(super) async fn note_agent_connected(&self) {
        self.supervisor
            .lock()
            .unwrap()
            .record_connection(Instant::now());
        self.publish_reconnect().await;
    }

    async fn publish_reconnect(&self) {
        let view = self.supervisor.lock().unwrap().view();
        let mut event = json!({"type":"agent_reconnect"});
        for (key, value) in view.as_object().unwrap() {
            event[key] = value.clone();
        }
        (self.emit)("acp:event", event);
        self.state_changed().await;
    }

    #[cfg(test)]
    pub(super) fn exhaust_reconnection_for_test(&self, cause: &str) {
        let mut supervisor = self.supervisor.lock().unwrap();
        let now = Instant::now();
        while !supervisor.record_failure(cause.into(), now) {}
    }

    /// The current process stopped without a user request. Reconnect with
    /// backoff unless the retry budget is exhausted.
    pub(super) async fn agent_lost(&self, launch: (AgentTarget, String), cause: String) {
        if !self.agent_requested.load(Ordering::Acquire) || self.retired.load(Ordering::Acquire) {
            return;
        }
        let start = {
            let mut supervisor = self.supervisor.lock().unwrap();
            let failed = supervisor.record_failure(cause, Instant::now());
            (!failed
                && !matches!(
                    supervisor.state,
                    ReconnectState::Waiting | ReconnectState::Connecting
                ))
            .then(|| {
                let (delay, epoch) = supervisor.schedule_next();
                (delay, epoch, supervisor.start_run())
            })
        };
        if let Some(next) = start {
            let weak = self.weak_self.clone();
            tokio::spawn(async move { reconnect_loop(weak, launch, next).await });
        }
        self.publish_reconnect().await;
    }

    /// Replace a stuck process without marking the disconnection voluntary.
    /// The relaunch is immediate and is not counted as a failure.
    pub(super) async fn restart_agent(&self, cause: &str) {
        let launch = self.agent_launch().await;
        let _ = self.stop_agent().await;
        let Some(launch) = launch else {
            return;
        };
        if !self.agent_requested.load(Ordering::Acquire) || self.retired.load(Ordering::Acquire) {
            return;
        }
        let start = self
            .supervisor
            .lock()
            .unwrap()
            .schedule_restart(cause.into());
        if let Some(next) = start {
            let weak = self.weak_self.clone();
            tokio::spawn(async move { reconnect_loop(weak, launch, next).await });
        }
        self.publish_reconnect().await;
    }

    pub(super) async fn set_agent_limit(&self, kind: ErrorKind, retry_after: Option<Duration>) {
        if !kind.is_limit() {
            return;
        }
        let wait = retry_after
            .unwrap_or_else(|| default_limit(kind))
            .min(MAX_LIMIT);
        let until = chrono::Utc::now()
            + chrono::Duration::from_std(wait).unwrap_or_else(|_| chrono::Duration::seconds(60));
        let limit = {
            let mut supervisor = self.supervisor.lock().unwrap();
            let current = supervisor.active_limit();
            let limit = AgentLimit {
                kind,
                until: current.map_or(until, |current| current.until.max(until)),
            };
            supervisor.limit = Some(limit);
            limit
        };
        let message = self.limit_message(&limit).await;
        (self.emit)(
            "acp:event",
            json!({"type":"agent_limited","kind":limit.kind.as_str(),
                "until":limit.until.to_rfc3339_opts(chrono::SecondsFormat::Millis, true),
                "message":message}),
        );
        self.state_changed().await;
    }

    pub(super) fn clear_agent_limit(&self) -> bool {
        self.supervisor.lock().unwrap().limit.take().is_some()
    }

    pub(super) fn active_agent_limit(&self) -> Option<AgentLimit> {
        self.supervisor.lock().unwrap().active_limit()
    }

    pub(super) async fn limit_message(&self, limit: &AgentLimit) -> String {
        let english = self.store.lock().await.data.settings.language == "en";
        let at = limit
            .until
            .with_timezone(&chrono::Local)
            .format("%H:%M")
            .to_string();
        match (limit.kind, english) {
            (ErrorKind::Overloaded, false) => format!(
                "Le fournisseur de l’agent est momentanément surchargé. Réessayez après {at}."
            ),
            (ErrorKind::Overloaded, true) => {
                format!("The agent’s provider is temporarily overloaded. Try again after {at}.")
            }
            (ErrorKind::UsageLimit, false) => format!(
                "Le harness signale une limite d’utilisation atteinte. Réessayez après {at}."
            ),
            (ErrorKind::UsageLimit, true) => {
                format!("The harness reports a usage limit. Try again after {at}.")
            }
            (_, false) => {
                format!("La limite de requêtes de l’agent est atteinte. Réessayez après {at}.")
            }
            (_, true) => format!("The agent’s request limit is reached. Try again after {at}."),
        }
    }

    /// Refuse a new model turn immediately while a provider limit holds.
    pub(super) async fn check_agent_limit(&self) -> Result<(), String> {
        match self.active_agent_limit() {
            Some(limit) => Err(self.limit_message(&limit).await),
            None => Ok(()),
        }
    }

    pub(super) fn note_learner_activity(&self) {
        self.supervisor.lock().unwrap().last_turn = Some(Instant::now());
    }

    /// Remaining silence needed before background maintenance may start.
    pub(super) fn maintenance_wait(&self) -> Duration {
        self.supervisor
            .lock()
            .unwrap()
            .last_turn
            .map_or(Duration::ZERO, |last| {
                MAINTENANCE_IDLE.saturating_sub(last.elapsed())
            })
    }
}

/// Boxed with an explicit `Send` bound: this future (re)connects the agent,
/// whose event task can call `agent_lost`, which spawns this future again.
fn reconnect_loop(
    weak: Weak<Backend>,
    launch: (AgentTarget, String),
    first: (Duration, u64, u64),
) -> std::pin::Pin<Box<dyn std::future::Future<Output = ()> + Send>> {
    Box::pin(reconnect_attempts(weak, launch, first))
}

async fn reconnect_attempts(
    weak: Weak<Backend>,
    launch: (AgentTarget, String),
    first: (Duration, u64, u64),
) {
    let (mut delay, epoch, run) = first;
    loop {
        tokio::time::sleep(delay).await;
        let Some(backend) = weak.upgrade() else {
            return;
        };
        let abandon = |backend: &Backend| backend.supervisor.lock().unwrap().abandon(epoch, run);
        let current = backend.supervisor.lock().unwrap().owns(epoch, run);
        if !current
            || !backend.agent_requested.load(Ordering::Acquire)
            || backend.retired.load(Ordering::Acquire)
            || backend.agent.lock().await.is_some()
        {
            abandon(&backend);
            backend.publish_reconnect().await;
            return;
        }
        {
            let mut supervisor = backend.supervisor.lock().unwrap();
            if !supervisor.owns(epoch, run) {
                return;
            }
            supervisor.state = ReconnectState::Connecting;
            supervisor.next_attempt_at = None;
        }
        backend.publish_reconnect().await;
        let result = {
            let _operation = backend.operation.lock().await;
            let settings = backend.settings().await;
            let (target, cwd) = launch.clone();
            let same_target = settings.target_id == target.id
                && settings.manual_target.as_ref().is_none_or(|selected| {
                    serde_json::to_value(selected).ok() == serde_json::to_value(&target).ok()
                });
            if !backend.supervisor.lock().unwrap().owns(epoch, run) || !same_target {
                None
            } else if backend.agent.lock().await.is_some() {
                Some(Ok(()))
            } else {
                Some(backend.connect_agent_locked(target, cwd).await.map(|_| ()))
            }
        };
        match result {
            None => {
                abandon(&backend);
                backend.publish_reconnect().await;
                return;
            }
            Some(Ok(())) => {
                abandon(&backend);
                backend.publish_reconnect().await;
                return;
            }
            Some(Err(error)) => {
                let next = {
                    let mut supervisor = backend.supervisor.lock().unwrap();
                    if !supervisor.owns(epoch, run) {
                        return;
                    }
                    (!supervisor.record_failure(error, Instant::now()))
                        .then(|| supervisor.schedule_next().0)
                };
                backend.publish_reconnect().await;
                match next {
                    Some(next) => delay = next,
                    None => return,
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn backoff_doubles_with_bounded_jitter_and_a_cap() {
        let seconds: Vec<u64> = (0..5).map(|a| backoff_delay(a, 0.0).as_secs()).collect();
        assert_eq!(seconds, [1, 2, 4, 8, 16]);
        assert_eq!(backoff_delay(6, 0.0), BACKOFF_CAP);
        assert_eq!(backoff_delay(30, 0.25), BACKOFF_CAP);
        let jittered = backoff_delay(2, 0.25);
        assert!(jittered >= Duration::from_secs(4) && jittered <= Duration::from_secs(5));
        assert_eq!(backoff_delay(2, 9.0), Duration::from_secs(5));
    }

    #[test]
    fn five_failures_in_five_minutes_stop_reconnection_with_their_cause() {
        let mut supervisor = Supervisor::default();
        let start = Instant::now();
        for index in 0..4 {
            assert!(!supervisor.record_failure(format!("crash {index}"), start));
        }
        assert!(supervisor.record_failure("crash 4".into(), start));
        assert_eq!(supervisor.state, ReconnectState::Failed);
        assert_eq!(supervisor.view()["cause"], "crash 4");
        assert_eq!(supervisor.view()["state"], "failed");
    }

    #[test]
    fn a_healthy_minute_resets_attempts_and_failures() {
        let mut supervisor = Supervisor {
            attempt: 4,
            ..Default::default()
        };
        let start = Instant::now();
        for _ in 0..4 {
            supervisor.record_failure("crash".into(), start);
        }
        supervisor.connected_at = Some(start);
        assert!(!supervisor.record_failure("late crash".into(), start + HEALTHY_AFTER));
        assert_eq!(supervisor.attempt, 0);
        assert_eq!(supervisor.failures.len(), 1);
    }

    #[test]
    fn a_connection_after_a_definitive_failure_returns_to_idle() {
        let mut supervisor = Supervisor::default();
        let start = Instant::now();
        for index in 0..MAX_FAILURES {
            supervisor.record_failure(format!("crash {index}"), start);
            if supervisor.state != ReconnectState::Failed {
                supervisor.schedule_next();
            }
        }
        assert_eq!(supervisor.state, ReconnectState::Failed);
        supervisor.record_connection(start);
        let view = supervisor.view();
        assert_eq!(view["state"], "idle");
        assert_eq!(view["attempt"], 0);
        assert!(view["cause"].is_null());
        assert!(view["nextAttemptAt"].is_null());
        assert!(supervisor.failures.is_empty());
        // A fresh budget: the next crash is retried instead of failing again.
        assert!(!supervisor.record_failure("crash".into(), start));
    }

    #[test]
    fn a_reconnection_keeps_backing_off_a_crash_loop() {
        let mut supervisor = Supervisor::default();
        let start = Instant::now();
        supervisor.record_failure("crash".into(), start);
        supervisor.schedule_next();
        supervisor.record_connection(start);
        assert_eq!(supervisor.view()["state"], "idle");
        assert!(supervisor.view()["cause"].is_null());
        assert_eq!(supervisor.attempt, 1);
        assert_eq!(supervisor.failures.len(), 1);
    }

    #[test]
    fn a_finished_loop_never_overwrites_the_state_of_a_newer_one() {
        let mut supervisor = Supervisor::default();
        let start = Instant::now();
        // Loop A reconnects the harness...
        supervisor.record_failure("crash".into(), start);
        let (_, epoch, first) = (
            supervisor.schedule_next().0,
            supervisor.epoch,
            supervisor.start_run(),
        );
        supervisor.state = ReconnectState::Connecting;
        supervisor.record_connection(start);
        // ...which crashes again before A ends: loop B starts waiting.
        supervisor.record_failure("crash again".into(), start);
        supervisor.schedule_next();
        let second = supervisor.start_run();
        // A's late cleanup is ignored; B still owns the waiting state.
        supervisor.abandon(epoch, first);
        assert_eq!(supervisor.state, ReconnectState::Waiting);
        assert!(supervisor.next_attempt_at.is_some());
        assert!(!supervisor.owns(epoch, first));
        supervisor.abandon(epoch, second);
        assert_eq!(supervisor.state, ReconnectState::Idle);
    }

    #[test]
    fn a_requested_restart_is_not_a_failure_and_leaves_a_definitive_one() {
        let mut supervisor = Supervisor::default();
        let start = Instant::now();
        for _ in 0..MAX_FAILURES - 1 {
            supervisor.record_failure("crash".into(), start);
        }
        let (delay, _, _) = supervisor.schedule_restart("stop".into()).unwrap();
        assert_eq!(delay, Duration::ZERO);
        assert_eq!(supervisor.failures.len(), MAX_FAILURES - 1);
        assert_eq!(supervisor.state, ReconnectState::Waiting);
        // An ongoing loop already relaunches the harness.
        assert!(supervisor.schedule_restart("stop".into()).is_none());
        supervisor.record_connection(start);
        supervisor.record_failure("crash".into(), start);
        assert_eq!(supervisor.state, ReconnectState::Failed);
        assert!(supervisor.schedule_restart("stop".into()).is_some());
        assert_eq!(supervisor.state, ReconnectState::Waiting);
        assert!(supervisor.failures.is_empty());
        assert_eq!(supervisor.attempt, 0);
    }

    #[test]
    fn old_failures_leave_the_window() {
        let mut supervisor = Supervisor::default();
        let start = Instant::now();
        for _ in 0..4 {
            supervisor.record_failure("crash".into(), start);
        }
        assert!(!supervisor.record_failure(
            "crash".into(),
            start + FAILURE_WINDOW + Duration::from_secs(1)
        ));
        assert_eq!(supervisor.failures.len(), 1);
    }

    #[test]
    fn expired_limits_clear_themselves() {
        let mut supervisor = Supervisor {
            limit: Some(AgentLimit {
                kind: ErrorKind::RateLimit,
                until: chrono::Utc::now() - chrono::Duration::seconds(1),
            }),
            ..Default::default()
        };
        assert!(supervisor.active_limit().is_none());
        supervisor.limit = Some(AgentLimit {
            kind: ErrorKind::RateLimit,
            until: chrono::Utc::now() + chrono::Duration::seconds(30),
        });
        assert!(supervisor.active_limit().is_some());
    }
}
