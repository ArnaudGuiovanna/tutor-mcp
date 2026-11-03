// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! Local timing records contain only opaque local IDs and fixed vocabulary.
//! They never modify MCP configuration, serialize a request, or inspect stderr.

use serde_json::{json, Value};
use std::collections::{HashSet, VecDeque};
use std::sync::{Arc, Mutex};
use std::time::Instant;

pub type TimingSink = Arc<dyn Fn(&str, Value) + Send + Sync>;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum TurnPhase {
    Preparing,
    WaitingLock,
    Context,
    LoadingSession,
    WaitingModel,
    Finalizing,
}

impl TurnPhase {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Preparing => "preparing",
            Self::WaitingLock => "waiting_lock",
            Self::Context => "context",
            Self::LoadingSession => "loading_session",
            Self::WaitingModel => "waiting_model",
            Self::Finalizing => "finalizing",
        }
    }
}

#[derive(Clone, Copy, Debug)]
pub enum TimingOutcome {
    Succeeded,
    Failed,
    Cancelled,
    TimedOut,
}

impl TimingOutcome {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Succeeded => "succeeded",
            Self::Failed => "failed",
            Self::Cancelled => "cancelled",
            Self::TimedOut => "timed_out",
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub enum TimingMarker {
    FirstEvent,
    FirstToken,
}

impl TimingMarker {
    fn as_str(self) -> &'static str {
        match self {
            Self::FirstEvent => "first_event",
            Self::FirstToken => "first_token",
        }
    }
}

struct TurnState {
    phase: TurnPhase,
    phase_started: Instant,
    conversation_id: Option<String>,
    finished: bool,
    markers: HashSet<TimingMarker>,
}

struct TurnInner {
    emit: TimingSink,
    operation_id: String,
    started: Instant,
    emission: Mutex<()>,
    state: Mutex<TurnState>,
    records: TimingJournal,
}

/// A cloneable turn clock. Call `finish` once for the actual terminal outcome.
/// Dropping the last unfinished handle records cancellation, including an
/// aborted future. Later phases/markers cannot overwrite a terminal record.
#[derive(Clone)]
pub struct OperationTiming(Arc<TurnInner>);

impl OperationTiming {
    pub fn new(emit: TimingSink, operation_id: String, conversation_id: Option<String>) -> Self {
        let started = Instant::now();
        let inner = TurnInner {
            emit,
            operation_id: local_id(&operation_id),
            started,
            emission: Mutex::new(()),
            state: Mutex::new(TurnState {
                phase: TurnPhase::Preparing,
                phase_started: started,
                conversation_id: conversation_id.map(|id| local_id(&id)),
                finished: false,
                markers: HashSet::new(),
            }),
            records: TimingJournal::default(),
        };
        let timing = Self(Arc::new(inner));
        let state = timing.0.state.lock().unwrap_or_else(|e| e.into_inner());
        let event = timing.0.event(&state, "started", started, None);
        drop(state);
        timing.0.emit(event);
        timing
    }

    pub fn operation_id(&self) -> &str {
        &self.0.operation_id
    }

    pub fn set_conversation_id(&self, conversation_id: String) {
        let mut state = self.0.state.lock().unwrap_or_else(|e| e.into_inner());
        if !state.finished {
            state.conversation_id = Some(local_id(&conversation_id));
        }
    }

    pub fn phase(&self, phase: TurnPhase) {
        self.phase_at(phase, Instant::now());
    }

    fn phase_at(&self, phase: TurnPhase, now: Instant) {
        let _emission = self.0.emission.lock().unwrap_or_else(|e| e.into_inner());
        let mut state = self.0.state.lock().unwrap_or_else(|e| e.into_inner());
        if state.finished || state.phase == phase {
            return;
        }
        let completed = self.0.event(&state, "completed", now, None);
        state.phase = phase;
        state.phase_started = now;
        let started = self.0.event(&state, "started", now, None);
        drop(state);
        self.0.emit(completed);
        self.0.emit(started);
    }

    pub fn mark(&self, marker: TimingMarker) {
        let _emission = self.0.emission.lock().unwrap_or_else(|e| e.into_inner());
        let mut state = self.0.state.lock().unwrap_or_else(|e| e.into_inner());
        if state.finished || !state.markers.insert(marker) {
            return;
        }
        let event = json!({"type":"turn_timing","operationId":self.0.operation_id,
            "conversationId":state.conversation_id,"marker":marker.as_str(),
            "elapsedMs":millis(self.0.started,Instant::now())});
        drop(state);
        self.0.emit(event);
    }

    pub fn finish(&self, outcome: TimingOutcome) {
        self.finish_at(outcome, Instant::now());
    }

    fn finish_at(&self, outcome: TimingOutcome, now: Instant) {
        let _emission = self.0.emission.lock().unwrap_or_else(|e| e.into_inner());
        let mut state = self.0.state.lock().unwrap_or_else(|e| e.into_inner());
        if state.finished {
            return;
        }
        state.finished = true;
        let event = self.0.event(&state, "completed", now, Some(outcome));
        drop(state);
        self.0.emit(event);
    }

    pub fn records(&self) -> Vec<Value> {
        self.0.records.snapshot()
    }
}

impl TurnInner {
    fn event(
        &self,
        state: &TurnState,
        status: &str,
        now: Instant,
        outcome: Option<TimingOutcome>,
    ) -> Value {
        let mut event = json!({"type":"turn_phase","operationId":self.operation_id,
            "conversationId":state.conversation_id,"phase":state.phase.as_str(),
            "state":status,"elapsedMs":millis(self.started,now),
            "phaseElapsedMs":millis(state.phase_started,now)});
        if let Some(outcome) = outcome {
            event["outcome"] = json!(outcome.as_str());
        }
        event
    }

    fn emit(&self, event: Value) {
        self.records.push(event.clone());
        (self.emit)("acp:event", event);
    }
}

impl Drop for TurnInner {
    fn drop(&mut self) {
        let state = self.state.lock().unwrap_or_else(|e| e.into_inner());
        if !state.finished {
            let event = self.event(
                &state,
                "completed",
                Instant::now(),
                Some(TimingOutcome::Cancelled),
            );
            drop(state);
            self.emit(event);
        }
    }
}

pub(crate) fn millis(start: Instant, now: Instant) -> u64 {
    now.saturating_duration_since(start)
        .as_millis()
        .min(u64::MAX as u128) as u64
}

/// These identifiers originate in the client. Keep diagnostics bounded even
/// when a caller accidentally passes malformed local state.
pub(crate) fn local_id(id: &str) -> String {
    if !id.is_empty()
        && id.len() <= 128
        && id
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.'))
    {
        id.to_owned()
    } else {
        "invalid-local-id".into()
    }
}

/// In-memory diagnostic export, bounded independently of conversation length.
/// Callers only insert records constructed from this module's fixed schema.
#[derive(Default)]
pub(crate) struct TimingJournal(Mutex<VecDeque<Value>>);

impl TimingJournal {
    const CAPACITY: usize = 512;

    pub(crate) fn push(&self, record: Value) {
        let mut entries = self.0.lock().unwrap_or_else(|e| e.into_inner());
        if entries.len() == Self::CAPACITY {
            entries.pop_front();
        }
        entries.push_back(record);
    }

    pub(crate) fn snapshot(&self) -> Vec<Value> {
        self.0
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .iter()
            .cloned()
            .collect()
    }
}

pub(crate) fn rpc_method(method: &str) -> &'static str {
    // Protocol callers may use arbitrary extension methods; even a method name
    // must not become an accidental channel for private data in diagnostics.
    match method {
        "initialize" => "initialize",
        "authenticate" => "authenticate",
        "auth/login" => "auth/login",
        "session/new" => "session/new",
        "session/load" => "session/load",
        "session/resume" => "session/resume",
        "session/prompt" => "session/prompt",
        "session/set_config_option" => "session/set_config_option",
        "session/set_mode" => "session/set_mode",
        "session/set_model" => "session/set_model",
        "tools/list" => "tools/list",
        "tools/call" => "tools/call",
        "_tutor/rebind" => "_tutor/rebind",
        _ => "other",
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::Duration;

    #[test]
    fn phase_durations_are_distinct_from_total_and_terminal_is_final() {
        let timing = OperationTiming::new(Arc::new(|_, _| {}), "operation".into(), None);
        let started = timing.0.started;
        timing.phase_at(TurnPhase::LoadingSession, started + Duration::from_secs(20));
        timing.finish_at(TimingOutcome::Succeeded, started + Duration::from_secs(21));
        timing.phase(TurnPhase::WaitingModel);
        timing.finish(TimingOutcome::Failed);
        let records = timing.records();
        assert_eq!(records.len(), 4);
        assert_eq!(records[1]["phaseElapsedMs"], 20000);
        assert_eq!(records[2]["phaseElapsedMs"], 0);
        assert_eq!(records[3]["phaseElapsedMs"], 1000);
        assert_eq!(records[3]["elapsedMs"], 21000);
        assert_eq!(records[3]["outcome"], "succeeded");
    }

    #[test]
    fn aborting_the_last_handle_finishes_and_markers_are_emitted_once() {
        let events = Arc::new(Mutex::new(Vec::new()));
        let observed = events.clone();
        let timing = OperationTiming::new(
            Arc::new(move |_, e| observed.lock().unwrap().push(e)),
            "operation".into(),
            None,
        );
        timing.mark(TimingMarker::FirstToken);
        timing.mark(TimingMarker::FirstToken);
        let second = timing.clone();
        drop(timing);
        assert_eq!(events.lock().unwrap().len(), 2);
        drop(second);
        let events = events.lock().unwrap();
        assert_eq!(events.len(), 3);
        assert_eq!(events[2]["outcome"], "cancelled");
    }

    #[test]
    fn journal_is_bounded_and_extension_method_content_is_not_recorded() {
        let journal = TimingJournal::default();
        for id in 0..600 {
            journal.push(json!({"requestId":id}));
        }
        let records = journal.snapshot();
        assert_eq!(records.len(), 512);
        assert_eq!(records[0]["requestId"], 88);
        assert_eq!(rpc_method("secret/token/extension"), "other");
        assert_eq!(rpc_method("session/load"), "session/load");
        assert_eq!(local_id(&"x".repeat(129)), "invalid-local-id");
        assert_eq!(local_id("Bearer private-token"), "invalid-local-id");
    }
}
