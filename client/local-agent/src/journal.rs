// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! Exact, account-bound journal, independent of the model context/workspace.
//!
//! Dependency: rusqlite = { version = "=0.41.1", default-features = false,
//! features = ["bundled"] }. This module uses synchronous disk IO; the ACP
//! integration should own it on a blocking worker. No network call occurs here.
//!
//! `prepare_operation` records immutable input. Only a successful
//! `mark_dispatched` commit issues a non-Clone `DispatchPermit`; the executor
//! must consume that permit before making ONE MCP call, using its exact tool,
//! arguments and idempotency key. Never call MCP if that commit returns Err.
//! An uncertain commit is deliberately conservative: reopening converts every
//! Dispatched operation to Unknown, and neither Prepared nor Unknown is replayed.
//! Unknown is terminal in this API; externally verified reconciliation belongs
//! to a later, explicit workflow. SQLite EXCLUSIVE locking prevents another
//! live owner from mistaking an active dispatch for a crashed one.
//!
//! Turns/events/results are exact blobs, not summaries. Arguments use recursive
//! lexicographic key ordering, preserving array order and serde_json's numeric
//! values (no float coercion); this is not RFC 8785 numeric normalization.
//! Arguments must be an object with at most 64 nested levels.
//! Each record is bounded to 8 MiB; the database has no workspace-size ceiling.
//! Binding stores only a domain-separated hash of stable identifiers, never
//! access tokens. The caller must use a private local profile directory (with
//! appropriate Windows ACLs); Unix files must be owner-only. This is not an
//! encrypted store and secure_delete is not a forensic filesystem wipe.
//!
//! Explicit retention refuses all unresolved mutations. It keeps binding and
//! opaque retired-ID hashes so deleted operation IDs can never authorize replay.
//! `delete_all` clears records, not the database file or those replay guards.
//!
//! Projection integration: `content_ref(kind, id)` captures an immutable source
//! identity; `resolve_content(&reference)` returns its exact bytes, checking the
//! owner binding and SHA256. `turn_refs` and `event_refs` list bounded metadata
//! newest-first without loading payloads into memory. A committed turn remains
//! discoverable even if the process dies before its first event. The caller
//! classifies that unfinished turn as interrupted; the journal never replays it.
//! The caller decides which evidence is
//! mandatory and which history is optional; this module never summarizes either.

use rusqlite::{params, Connection, OpenFlags, OptionalExtension, TransactionBehavior};
use serde_json::Value;
use sha2::{Digest, Sha256};
use std::{
    fmt,
    fs::OpenOptions,
    io::Write,
    path::Path,
    time::{Duration, SystemTime, UNIX_EPOCH},
};

pub const MAX_RECORD_BYTES: usize = 8 * 1024 * 1024;
const APP_ID: i32 = 0x54554a31;
const SCHEMA_VERSION: i32 = 1;
pub type Result<T> = std::result::Result<T, JournalError>;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum JournalError {
    InvalidInput,
    InvalidArguments,
    RecordTooLarge,
    BindingMismatch,
    UnsupportedSchema,
    InUse,
    Storage,
    NotFound,
    Conflict,
    InvalidTransition,
    RetentionBlocked,
    RetiredId,
    UnsafeFile,
}

impl fmt::Display for JournalError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            Self::InvalidInput => "journal_invalid_input",
            Self::InvalidArguments => "journal_invalid_arguments",
            Self::RecordTooLarge => "journal_record_too_large",
            Self::BindingMismatch => "journal_binding_mismatch",
            Self::UnsupportedSchema => "journal_unsupported_schema",
            Self::InUse => "journal_in_use",
            Self::Storage => "journal_storage_failed",
            Self::NotFound => "journal_record_missing",
            Self::Conflict => "journal_immutable_record_conflict",
            Self::InvalidTransition => "journal_invalid_transition",
            Self::RetentionBlocked => "journal_unresolved_mutation",
            Self::RetiredId => "journal_identifier_retired",
            Self::UnsafeFile => "journal_unsafe_file",
        })
    }
}
impl std::error::Error for JournalError {}
impl From<rusqlite::Error> for JournalError {
    fn from(error: rusqlite::Error) -> Self {
        match error.sqlite_error_code() {
            Some(rusqlite::ErrorCode::DatabaseBusy | rusqlite::ErrorCode::DatabaseLocked) => {
                Self::InUse
            }
            _ => Self::Storage,
        }
    }
}

/// Stable identifiers only: never pass authentication headers or bearer tokens.
pub struct Binding<'a> {
    pub profile_id: &'a str,
    pub account_id: &'a str,
    pub tenant_id: &'a str,
    pub server_id: &'a str,
}
impl Binding<'_> {
    fn digest(&self) -> Result<Vec<u8>> {
        let mut hash = Sha256::new();
        hash.update(b"tutor-journal-binding-v1\0");
        for value in [
            self.profile_id,
            self.account_id,
            self.tenant_id,
            self.server_id,
        ] {
            identifier(value, 2048)?;
            hash.update((value.len() as u64).to_le_bytes());
            hash.update(value.as_bytes());
        }
        Ok(hash.finalize().to_vec())
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum OperationState {
    Prepared,
    Dispatched,
    Completed,
    Unknown,
    Rejected,
}
impl OperationState {
    fn text(self) -> &'static str {
        match self {
            Self::Prepared => "prepared",
            Self::Dispatched => "dispatched",
            Self::Completed => "completed",
            Self::Unknown => "unknown",
            Self::Rejected => "rejected",
        }
    }
    fn parse(text: &str) -> Result<Self> {
        match text {
            "prepared" => Ok(Self::Prepared),
            "dispatched" => Ok(Self::Dispatched),
            "completed" => Ok(Self::Completed),
            "unknown" => Ok(Self::Unknown),
            "rejected" => Ok(Self::Rejected),
            _ => Err(JournalError::Storage),
        }
    }
}

pub struct OperationRequest<'a> {
    pub id: &'a str,
    pub turn_id: Option<&'a str>,
    pub tool: &'a str,
    pub arguments: &'a Value,
    pub idempotency_key: Option<&'a str>,
    pub mutating: bool,
}

#[derive(Clone, PartialEq, Eq)]
pub struct Operation {
    pub id: String,
    pub turn_id: Option<String>,
    pub tool: String,
    pub canonical_arguments: Vec<u8>,
    pub idempotency_key: Option<String>,
    pub mutating: bool,
    pub state: OperationState,
    pub created_at_ms: i64,
    pub result: Option<Vec<u8>>,
}

/// Issued only after dispatch intent commits; intentionally not Clone/Serialize.
pub struct DispatchPermit {
    operation: Operation,
}
impl DispatchPermit {
    pub fn operation(&self) -> &Operation {
        &self.operation
    }
    pub fn into_operation(self) -> Operation {
        self.operation
    }
}

#[derive(Clone, PartialEq, Eq)]
pub struct Turn {
    pub id: String,
    pub created_at_ms: i64,
    pub payload: Vec<u8>,
}
#[derive(Clone, PartialEq, Eq)]
pub struct Event {
    pub sequence: i64,
    pub id: String,
    pub turn_id: Option<String>,
    pub kind: String,
    pub created_at_ms: i64,
    pub payload: Vec<u8>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ContentKind {
    Turn,
    Event,
    OperationArguments,
    OperationResult,
}

/// Created only by an already-bound Journal. References survive reopening the
/// same binding, but cannot be resolved against another account/tenant/server.
/// A reference is provenance, not authorization to invoke a tool or reveal data.
/// Binding does not include the database filename/session UUID. With one file
/// per ACP session, keep references in a session-local registry (or namespace
/// record IDs with that session UUID); do not mix references across sessions.
#[derive(Clone, PartialEq, Eq)]
pub struct ContentRef {
    binding: Vec<u8>,
    kind: ContentKind,
    id: String,
    sha256: [u8; 32],
}
impl ContentRef {
    pub fn kind(&self) -> ContentKind {
        self.kind
    }
    pub fn id(&self) -> &str {
        &self.id
    }
    pub fn sha256(&self) -> [u8; 32] {
        self.sha256
    }
}

/// Metadata only: fetching a page never copies the potentially 8 MiB payloads.
pub struct TurnRef {
    pub sequence: i64,
    pub created_at_ms: i64,
    pub bytes: usize,
    pub content: ContentRef,
}

/// Metadata only: fetching a page never copies the potentially 8 MiB payloads.
pub struct EventRef {
    pub sequence: i64,
    pub turn_id: Option<String>,
    pub kind: String,
    pub created_at_ms: i64,
    pub bytes: usize,
    pub content: ContentRef,
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Transition {
    pub sequence: i64,
    pub from: Option<OperationState>,
    pub to: OperationState,
    pub at_ms: i64,
}
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct Deleted {
    pub turns: usize,
    pub events: usize,
    pub operations: usize,
}

pub struct Journal {
    connection: Connection,
    recovered_unknown: usize,
    binding: Vec<u8>,
}

impl Journal {
    /// Opens an existing/private file in an already-created, trusted directory.
    /// Independent session files can be open together, even with the same owner
    /// binding. A second connection to THIS file is refused until its owner drops.
    /// Each Journal is synchronous and must not be used concurrently.
    pub fn open(path: &Path, binding: &Binding<'_>) -> Result<Self> {
        let owner = binding.digest()?;
        private_database_file(path)?;
        let mut connection = Connection::open_with_flags(
            path,
            OpenFlags::SQLITE_OPEN_READ_WRITE | OpenFlags::SQLITE_OPEN_NO_MUTEX,
        )?;
        connection.busy_timeout(Duration::from_millis(100))?;
        connection.execute_batch(
            "PRAGMA foreign_keys=ON; PRAGMA trusted_schema=OFF;
            PRAGMA locking_mode=EXCLUSIVE; PRAGMA synchronous=FULL;
            PRAGMA secure_delete=ON; PRAGMA journal_size_limit=0;",
        )?;
        let mode: String =
            connection.query_row("PRAGMA journal_mode=DELETE", [], |row| row.get(0))?;
        let sync: i64 = connection.query_row("PRAGMA synchronous", [], |row| row.get(0))?;
        if mode != "delete" || sync != 2 {
            return Err(JournalError::Storage);
        }
        let tx = connection.transaction_with_behavior(TransactionBehavior::Exclusive)?;
        let app: i32 = tx.query_row("PRAGMA application_id", [], |row| row.get(0))?;
        let version: i32 = tx.query_row("PRAGMA user_version", [], |row| row.get(0))?;
        if app == 0 && version == 0 {
            let count: i64 = tx.query_row(
                "SELECT count(*) FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'",
                [],
                |row| row.get(0),
            )?;
            if count != 0 {
                return Err(JournalError::UnsupportedSchema);
            }
            tx.execute_batch(SCHEMA)?;
            tx.pragma_update(None, "application_id", APP_ID)?;
            tx.pragma_update(None, "user_version", SCHEMA_VERSION)?;
            tx.execute("INSERT INTO owner(id,binding) VALUES(1,?1)", params![owner])?;
        } else if app != APP_ID || version != SCHEMA_VERSION {
            return Err(JournalError::UnsupportedSchema);
        }
        let recorded: Vec<u8> =
            tx.query_row("SELECT binding FROM owner WHERE id=1", [], |row| row.get(0))?;
        if recorded != owner {
            return Err(JournalError::BindingMismatch);
        }
        // Ownership is verified BEFORE recovery mutates any state. The lock is
        // held until this connection closes, including between transactions.
        let now = now_ms()?;
        tx.execute(
            "INSERT INTO transitions(operation_id,from_state,to_state,at_ms)
            SELECT id,'dispatched','unknown',?1 FROM operations WHERE state='dispatched'",
            [now],
        )?;
        let recovered_unknown = tx.execute(
            "UPDATE operations SET state='unknown' WHERE state='dispatched'",
            [],
        )?;
        tx.commit()?;
        Ok(Self {
            connection,
            recovered_unknown,
            binding: owner,
        })
    }

    pub fn recovered_unknown_count(&self) -> usize {
        self.recovered_unknown
    }

    pub fn append_turn(&mut self, id: &str, payload: &[u8]) -> Result<Turn> {
        identifier(id, 128)?;
        record_size(&[id.len(), payload.len(), 32])?;
        let tx = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Immediate)?;
        not_retired(&tx, "turn", id)?;
        if let Some(old) = read_turn(&tx, id)? {
            if old.payload != payload {
                return Err(JournalError::Conflict);
            }
            return Ok(old);
        }
        let created_at_ms = now_ms()?;
        let digest = Sha256::digest(payload).to_vec();
        tx.execute(
            "INSERT INTO turns(id,created_at_ms,payload,payload_sha256) VALUES(?1,?2,?3,?4)",
            params![id, created_at_ms, payload, digest],
        )?;
        tx.commit()?;
        Ok(Turn {
            id: id.into(),
            created_at_ms,
            payload: payload.into(),
        })
    }

    pub fn turn(&self, id: &str) -> Result<Option<Turn>> {
        read_turn(&self.connection, id)
    }

    /// All actual turns, including those without events. Newest-first, with an
    /// exclusive cursor advanced using the last returned sequence and limit
    /// 1..=128. Sequences are never reused after retention. No events are created.
    pub fn turn_refs(&self, before_sequence: Option<i64>, limit: usize) -> Result<Vec<TurnRef>> {
        if !(1..=128).contains(&limit) || before_sequence.is_some_and(|value| value <= 0) {
            return Err(JournalError::InvalidInput);
        }
        let upper = before_sequence.map_or(i64::MAX, |sequence| sequence - 1);
        let mut statement = self.connection.prepare(
            "SELECT sequence,id,created_at_ms,payload_sha256,length(payload)
            FROM turns WHERE sequence<=?1 ORDER BY sequence DESC LIMIT ?2",
        )?;
        let mut rows = statement.query(params![upper, limit as i64])?;
        let mut out = Vec::new();
        while let Some(row) = rows.next()? {
            let digest: Vec<u8> = row.get(3)?;
            let bytes: i64 = row.get(4)?;
            let bytes = usize::try_from(bytes).map_err(|_| JournalError::Storage)?;
            if bytes > MAX_RECORD_BYTES {
                return Err(JournalError::Storage);
            }
            out.push(TurnRef {
                sequence: row.get(0)?,
                created_at_ms: row.get(2)?,
                bytes,
                content: ContentRef {
                    binding: self.binding.clone(),
                    kind: ContentKind::Turn,
                    id: row.get(1)?,
                    sha256: digest.try_into().map_err(|_| JournalError::Storage)?,
                },
            });
        }
        Ok(out)
    }

    /// All actual turns in chronological order, including those without events.
    /// Start with `None`; advance the exclusive cursor to the last sequence in
    /// each page. Cursors must be positive and limit must be 1..=128. Only
    /// metadata is read; use `resolve_content` to load one exact payload at a time.
    pub fn turn_refs_after(
        &self,
        after_sequence: Option<i64>,
        limit: usize,
    ) -> Result<Vec<TurnRef>> {
        if !(1..=128).contains(&limit) || after_sequence.is_some_and(|value| value <= 0) {
            return Err(JournalError::InvalidInput);
        }
        let mut statement = self.connection.prepare(
            "SELECT sequence,id,created_at_ms,payload_sha256,length(payload)
            FROM turns WHERE sequence>?1 ORDER BY sequence ASC LIMIT ?2",
        )?;
        let mut rows = statement.query(params![after_sequence.unwrap_or(0), limit as i64])?;
        let mut out = Vec::new();
        while let Some(row) = rows.next()? {
            let digest: Vec<u8> = row.get(3)?;
            let bytes: i64 = row.get(4)?;
            let bytes = usize::try_from(bytes).map_err(|_| JournalError::Storage)?;
            if bytes > MAX_RECORD_BYTES {
                return Err(JournalError::Storage);
            }
            out.push(TurnRef {
                sequence: row.get(0)?,
                created_at_ms: row.get(2)?,
                bytes,
                content: ContentRef {
                    binding: self.binding.clone(),
                    kind: ContentKind::Turn,
                    id: row.get(1)?,
                    sha256: digest.try_into().map_err(|_| JournalError::Storage)?,
                },
            });
        }
        Ok(out)
    }

    pub fn append_event(
        &mut self,
        id: &str,
        turn_id: Option<&str>,
        kind: &str,
        payload: &[u8],
    ) -> Result<Event> {
        identifier(id, 128)?;
        optional_identifier(turn_id, 128)?;
        identifier(kind, 128)?;
        record_size(&[
            id.len(),
            turn_id.map_or(0, str::len),
            kind.len(),
            payload.len(),
            32,
        ])?;
        let tx = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Immediate)?;
        not_retired(&tx, "event", id)?;
        if let Some(old) = read_event(&tx, id)? {
            if old.turn_id.as_deref() != turn_id || old.kind != kind || old.payload != payload {
                return Err(JournalError::Conflict);
            }
            return Ok(old);
        }
        require_turn(&tx, turn_id)?;
        let created_at_ms = now_ms()?;
        let digest = Sha256::digest(payload).to_vec();
        tx.execute(
            "INSERT INTO events(id,turn_id,kind,created_at_ms,payload,payload_sha256) VALUES(?1,?2,?3,?4,?5,?6)",
            params![id, turn_id, kind, created_at_ms, payload, digest],
        )?;
        let sequence = tx.last_insert_rowid();
        tx.commit()?;
        Ok(Event {
            sequence,
            id: id.into(),
            turn_id: turn_id.map(str::to_owned),
            kind: kind.into(),
            created_at_ms,
            payload: payload.into(),
        })
    }

    pub fn event(&self, id: &str) -> Result<Option<Event>> {
        read_event(&self.connection, id)
    }

    /// Stable newest-first cursor. `None` means newest; advance using the last
    /// returned sequence. `turn_id=None` lists all events in this bound journal.
    /// AUTOINCREMENT prevents cursor reuse after retention, even after delete_all.
    pub fn event_refs(
        &self,
        turn_id: Option<&str>,
        before_sequence: Option<i64>,
        limit: usize,
    ) -> Result<Vec<EventRef>> {
        optional_identifier(turn_id, 128)?;
        if !(1..=128).contains(&limit) || before_sequence.is_some_and(|value| value <= 0) {
            return Err(JournalError::InvalidInput);
        }
        // Direct range predicates let SQLite seek into the sequence index,
        // rather than scan all newer history on every subsequent page.
        let query = if turn_id.is_some() {
            "SELECT sequence,id,turn_id,kind,created_at_ms,payload_sha256,length(payload)
             FROM events WHERE turn_id=?1 AND sequence<=?2 ORDER BY sequence DESC LIMIT ?3"
        } else {
            "SELECT sequence,id,turn_id,kind,created_at_ms,payload_sha256,length(payload)
             FROM events WHERE ?1 IS NULL AND sequence<=?2 ORDER BY sequence DESC LIMIT ?3"
        };
        let mut statement = self.connection.prepare(query)?;
        let upper = before_sequence.map_or(i64::MAX, |sequence| sequence - 1);
        let mut rows = statement.query(params![turn_id, upper, limit as i64])?;
        let mut out = Vec::new();
        while let Some(row) = rows.next()? {
            let digest: Vec<u8> = row.get(5)?;
            let bytes: i64 = row.get(6)?;
            let bytes = usize::try_from(bytes).map_err(|_| JournalError::Storage)?;
            if bytes > MAX_RECORD_BYTES {
                return Err(JournalError::Storage);
            }
            out.push(EventRef {
                sequence: row.get(0)?,
                turn_id: row.get(2)?,
                kind: row.get(3)?,
                created_at_ms: row.get(4)?,
                bytes,
                content: ContentRef {
                    binding: self.binding.clone(),
                    kind: ContentKind::Event,
                    id: row.get(1)?,
                    sha256: digest.try_into().map_err(|_| JournalError::Storage)?,
                },
            });
        }
        Ok(out)
    }

    pub fn content_ref(&self, kind: ContentKind, id: &str) -> Result<ContentRef> {
        identifier(id, 128)?;
        let stored_digest = match kind {
            ContentKind::Turn => Some("SELECT payload_sha256 FROM turns WHERE id=?1"),
            ContentKind::Event => Some("SELECT payload_sha256 FROM events WHERE id=?1"),
            _ => None,
        };
        let sha256 = if let Some(query) = stored_digest {
            let digest: Vec<u8> = self
                .connection
                .query_row(query, [id], |row| row.get(0))
                .optional()?
                .ok_or(JournalError::NotFound)?;
            digest.try_into().map_err(|_| JournalError::Storage)?
        } else {
            Sha256::digest(self.content_bytes(kind, id)?).into()
        };
        Ok(ContentRef {
            binding: self.binding.clone(),
            kind,
            id: id.into(),
            sha256,
        })
    }

    /// Never returns a shortened/decoded/summarized replacement. Deleted sources
    /// are NotFound, changed bytes Conflict, and foreign bindings BindingMismatch.
    pub fn resolve_content(&self, reference: &ContentRef) -> Result<Vec<u8>> {
        if reference.binding != self.binding {
            return Err(JournalError::BindingMismatch);
        }
        let content = self.content_bytes(reference.kind, &reference.id)?;
        let digest: [u8; 32] = Sha256::digest(&content).into();
        if digest != reference.sha256 {
            return Err(JournalError::Conflict);
        }
        Ok(content)
    }

    fn content_bytes(&self, kind: ContentKind, id: &str) -> Result<Vec<u8>> {
        let query = match kind {
            ContentKind::Turn => "SELECT payload FROM turns WHERE id=?1",
            ContentKind::Event => "SELECT payload FROM events WHERE id=?1",
            ContentKind::OperationArguments => "SELECT arguments FROM operations WHERE id=?1",
            ContentKind::OperationResult => "SELECT payload FROM results JOIN operations ON operations.id=results.operation_id WHERE operations.id=?1 AND operations.state='completed'",
        };
        let bytes: Vec<u8> = self
            .connection
            .query_row(query, [id], |row| row.get(0))
            .optional()?
            .ok_or(JournalError::NotFound)?;
        if bytes.len() > MAX_RECORD_BYTES {
            return Err(JournalError::Storage);
        }
        Ok(bytes)
    }

    /// Repeating an identical ID returns its existing state, NEVER a new permit.
    /// It does not reset Completed/Unknown/Rejected operations to Prepared.
    pub fn prepare_operation(&mut self, request: OperationRequest<'_>) -> Result<Operation> {
        identifier(request.id, 128)?;
        optional_identifier(request.turn_id, 128)?;
        identifier(request.tool, 256)?;
        optional_identifier(request.idempotency_key, 256)?;
        let arguments = canonical_arguments(request.arguments)?;
        record_size(&[
            request.id.len(),
            request.turn_id.map_or(0, str::len),
            request.tool.len(),
            request.idempotency_key.map_or(0, str::len),
            arguments.len(),
        ])?;
        let tx = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Immediate)?;
        not_retired(&tx, "operation", request.id)?;
        if let Some(old) = read_operation(&tx, request.id)? {
            if old.turn_id.as_deref() != request.turn_id
                || old.tool != request.tool
                || old.canonical_arguments != arguments
                || old.mutating != request.mutating
                || old.idempotency_key.as_deref() != request.idempotency_key
            {
                return Err(JournalError::Conflict);
            }
            return Ok(old);
        }
        require_turn(&tx, request.turn_id)?;
        let created_at_ms = now_ms()?;
        tx.execute("INSERT INTO operations(id,turn_id,tool,arguments,idempotency_key,mutating,state,created_at_ms)
            VALUES(?1,?2,?3,?4,?5,?6,'prepared',?7)", params![request.id, request.turn_id, request.tool,
                arguments, request.idempotency_key, request.mutating, created_at_ms])?;
        append_transition(
            &tx,
            request.id,
            None,
            OperationState::Prepared,
            created_at_ms,
        )?;
        tx.commit()?;
        Ok(Operation {
            id: request.id.into(),
            turn_id: request.turn_id.map(str::to_owned),
            tool: request.tool.into(),
            canonical_arguments: arguments,
            idempotency_key: request.idempotency_key.map(str::to_owned),
            mutating: request.mutating,
            state: OperationState::Prepared,
            created_at_ms,
            result: None,
        })
    }

    pub fn operation(&self, id: &str) -> Result<Option<Operation>> {
        read_operation(&self.connection, id)
    }

    pub fn mark_dispatched(&mut self, id: &str) -> Result<DispatchPermit> {
        let operation =
            self.transition(id, OperationState::Prepared, OperationState::Dispatched)?;
        Ok(DispatchPermit { operation })
    }

    pub fn reject_prepared(&mut self, id: &str) -> Result<()> {
        self.transition(id, OperationState::Prepared, OperationState::Rejected)
            .map(|_| ())
    }

    pub fn mark_unknown(&mut self, id: &str) -> Result<()> {
        self.transition(id, OperationState::Dispatched, OperationState::Unknown)
            .map(|_| ())
    }

    fn transition(
        &mut self,
        id: &str,
        from: OperationState,
        to: OperationState,
    ) -> Result<Operation> {
        let tx = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Immediate)?;
        let mut operation = read_operation(&tx, id)?.ok_or(JournalError::NotFound)?;
        if operation.state != from {
            return Err(JournalError::InvalidTransition);
        }
        let changed = tx.execute(
            "UPDATE operations SET state=?1 WHERE id=?2 AND state=?3",
            params![to.text(), id, from.text()],
        )?;
        if changed != 1 {
            return Err(JournalError::InvalidTransition);
        }
        append_transition(&tx, id, Some(from), to, now_ms()?)?;
        tx.commit()?;
        operation.state = to;
        Ok(operation)
    }

    /// Exact same-result repetition is harmless; a different completed result
    /// is rejected. An Unknown operation cannot be completed by guessing.
    pub fn complete_operation(&mut self, id: &str, result: &[u8]) -> Result<()> {
        record_size(&[id.len(), result.len()])?;
        let tx = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Immediate)?;
        let operation = read_operation(&tx, id)?.ok_or(JournalError::NotFound)?;
        if operation.state == OperationState::Completed {
            return if operation.result.as_deref() == Some(result) {
                Ok(())
            } else {
                Err(JournalError::Conflict)
            };
        }
        if operation.state != OperationState::Dispatched {
            return Err(JournalError::InvalidTransition);
        }
        tx.execute(
            "INSERT INTO results(operation_id,payload) VALUES(?1,?2)",
            params![id, result],
        )?;
        let changed = tx.execute(
            "UPDATE operations SET state='completed' WHERE id=?1 AND state='dispatched'",
            [id],
        )?;
        if changed != 1 {
            return Err(JournalError::InvalidTransition);
        }
        append_transition(
            &tx,
            id,
            Some(OperationState::Dispatched),
            OperationState::Completed,
            now_ms()?,
        )?;
        tx.commit()?;
        Ok(())
    }

    pub fn transitions(&self, id: &str) -> Result<Vec<Transition>> {
        let mut statement = self.connection.prepare("SELECT sequence,from_state,to_state,at_ms FROM transitions WHERE operation_id=?1 ORDER BY sequence")?;
        let mut rows = statement.query([id])?;
        let mut out = Vec::new();
        while let Some(row) = rows.next()? {
            let from: Option<String> = row.get(1)?;
            let to: String = row.get(2)?;
            out.push(Transition {
                sequence: row.get(0)?,
                from: from.as_deref().map(OperationState::parse).transpose()?,
                to: OperationState::parse(&to)?,
                at_ms: row.get(3)?,
            });
        }
        Ok(out)
    }

    /// Bounded cursor over IDs only; callers fetch exact operation bodies singly.
    pub fn operation_ids(
        &self,
        state: OperationState,
        after_id: &str,
        limit: usize,
    ) -> Result<Vec<String>> {
        if !(1..=128).contains(&limit) {
            return Err(JournalError::InvalidInput);
        }
        let mut statement = self
            .connection
            .prepare("SELECT id FROM operations WHERE state=?1 AND id>?2 ORDER BY id LIMIT ?3")?;
        let rows = statement.query_map(params![state.text(), after_id, limit as i64], |row| {
            row.get(0)
        })?;
        rows.collect::<std::result::Result<Vec<String>, _>>()
            .map_err(Into::into)
    }

    /// Delete records strictly older than the cutoff. Keep any old turn with
    /// surviving newer children, and refuse if ANY mutation is unresolved.
    pub fn retain_since(&mut self, cutoff_ms: i64) -> Result<Deleted> {
        if cutoff_ms < 0 {
            return Err(JournalError::InvalidInput);
        }
        self.delete_before(cutoff_ms)
    }

    pub fn delete_all(&mut self) -> Result<Deleted> {
        self.delete_before(i64::MAX)
    }

    fn delete_before(&mut self, cutoff: i64) -> Result<Deleted> {
        let tx = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Immediate)?;
        let unresolved: bool = tx.query_row("SELECT EXISTS(SELECT 1 FROM operations WHERE mutating=1 AND state IN ('prepared','dispatched','unknown'))", [], |row| row.get(0))?;
        if unresolved {
            return Err(JournalError::RetentionBlocked);
        }
        // A recently completed result/transition keeps the whole operation,
        // even when its initial preparation predates the retention boundary.
        const OLD_OPERATIONS: &str = "SELECT id FROM operations WHERE created_at_ms<?1 AND NOT EXISTS(SELECT 1 FROM transitions WHERE operation_id=operations.id AND at_ms>=?1)";
        retire(&tx, "operation", OLD_OPERATIONS, cutoff)?;
        let operations = tx.execute("DELETE FROM operations WHERE created_at_ms<?1 AND NOT EXISTS(SELECT 1 FROM transitions WHERE operation_id=operations.id AND at_ms>=?1)", [cutoff])?;
        retire(
            &tx,
            "event",
            "SELECT id FROM events WHERE created_at_ms<?1",
            cutoff,
        )?;
        let events = tx.execute("DELETE FROM events WHERE created_at_ms<?1", [cutoff])?;
        const OLD_TURNS: &str = "SELECT id FROM turns WHERE created_at_ms<?1 AND NOT EXISTS(SELECT 1 FROM events WHERE turn_id=turns.id) AND NOT EXISTS(SELECT 1 FROM operations WHERE turn_id=turns.id)";
        retire(&tx, "turn", OLD_TURNS, cutoff)?;
        let turns = tx.execute("DELETE FROM turns WHERE id IN (SELECT id FROM turns WHERE created_at_ms<?1 AND NOT EXISTS(SELECT 1 FROM events WHERE turn_id=turns.id) AND NOT EXISTS(SELECT 1 FROM operations WHERE turn_id=turns.id))", [cutoff])?;
        tx.commit()?;
        Ok(Deleted {
            turns,
            events,
            operations,
        })
    }
}

fn now_ms() -> Result<i64> {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .ok()
        .and_then(|d| i64::try_from(d.as_millis()).ok())
        .ok_or(JournalError::Storage)
}
fn identifier(value: &str, max: usize) -> Result<()> {
    if value.trim().is_empty() || value.len() > max || value.chars().any(char::is_control) {
        Err(JournalError::InvalidInput)
    } else {
        Ok(())
    }
}
fn optional_identifier(value: Option<&str>, max: usize) -> Result<()> {
    value.map_or(Ok(()), |v| identifier(v, max))
}
fn record_size(sizes: &[usize]) -> Result<()> {
    if sizes
        .iter()
        .try_fold(32usize, |total, size| total.checked_add(*size))
        .is_none_or(|total| total > MAX_RECORD_BYTES)
    {
        Err(JournalError::RecordTooLarge)
    } else {
        Ok(())
    }
}
fn private_database_file(path: &Path) -> Result<()> {
    if !path.is_absolute() || path.file_name().is_none() {
        return Err(JournalError::UnsafeFile);
    }
    if !path.exists() {
        let mut options = OpenOptions::new();
        options.write(true).create_new(true);
        #[cfg(unix)]
        {
            use std::os::unix::fs::OpenOptionsExt;
            options.mode(0o600);
        }
        match options.open(path) {
            Ok(_) => (),
            Err(error) if error.kind() == std::io::ErrorKind::AlreadyExists => (),
            Err(_) => return Err(JournalError::Storage),
        }
    }
    let metadata = path.symlink_metadata().map_err(|_| JournalError::Storage)?;
    if !metadata.is_file() {
        return Err(JournalError::UnsafeFile);
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        if metadata.permissions().mode() & 0o077 != 0 {
            return Err(JournalError::UnsafeFile);
        }
    }
    Ok(())
}
fn require_turn(connection: &Connection, id: Option<&str>) -> Result<()> {
    if let Some(id) = id {
        let exists: bool = connection.query_row(
            "SELECT EXISTS(SELECT 1 FROM turns WHERE id=?1)",
            [id],
            |row| row.get(0),
        )?;
        if !exists {
            return Err(JournalError::NotFound);
        }
    }
    Ok(())
}
fn read_turn(connection: &Connection, id: &str) -> Result<Option<Turn>> {
    Ok(connection
        .query_row(
            "SELECT id,created_at_ms,payload FROM turns WHERE id=?1",
            [id],
            |row| {
                Ok(Turn {
                    id: row.get(0)?,
                    created_at_ms: row.get(1)?,
                    payload: row.get(2)?,
                })
            },
        )
        .optional()?)
}
fn read_event(connection: &Connection, id: &str) -> Result<Option<Event>> {
    Ok(connection
        .query_row(
            "SELECT id,turn_id,kind,created_at_ms,payload,sequence FROM events WHERE id=?1",
            [id],
            |row| {
                Ok(Event {
                    sequence: row.get(5)?,
                    id: row.get(0)?,
                    turn_id: row.get(1)?,
                    kind: row.get(2)?,
                    created_at_ms: row.get(3)?,
                    payload: row.get(4)?,
                })
            },
        )
        .optional()?)
}
fn read_operation(connection: &Connection, id: &str) -> Result<Option<Operation>> {
    let row = connection.query_row("SELECT o.id,o.turn_id,o.tool,o.arguments,o.idempotency_key,o.mutating,o.state,o.created_at_ms,r.payload
        FROM operations o LEFT JOIN results r ON r.operation_id=o.id WHERE o.id=?1", [id], |row| {
        Ok((Operation { id: row.get(0)?, turn_id: row.get(1)?, tool: row.get(2)?, canonical_arguments: row.get(3)?,
            idempotency_key: row.get(4)?, mutating: row.get(5)?, state: OperationState::Prepared,
            created_at_ms: row.get(7)?, result: row.get(8)? }, row.get::<_, String>(6)?))
    }).optional()?;
    row.map(|(mut operation, state)| {
        operation.state = OperationState::parse(&state)?;
        if (operation.state == OperationState::Completed) != operation.result.is_some() {
            return Err(JournalError::Storage);
        }
        Ok(operation)
    })
    .transpose()
}
fn append_transition(
    connection: &Connection,
    id: &str,
    from: Option<OperationState>,
    to: OperationState,
    at: i64,
) -> Result<()> {
    connection.execute(
        "INSERT INTO transitions(operation_id,from_state,to_state,at_ms) VALUES(?1,?2,?3,?4)",
        params![id, from.map(OperationState::text), to.text(), at],
    )?;
    Ok(())
}
fn retired_hash(id: &str) -> Vec<u8> {
    Sha256::digest(id.as_bytes()).to_vec()
}
fn not_retired(connection: &Connection, kind: &str, id: &str) -> Result<()> {
    let found: bool = connection.query_row(
        "SELECT EXISTS(SELECT 1 FROM retired_ids WHERE kind=?1 AND id_hash=?2)",
        params![kind, retired_hash(id)],
        |row| row.get(0),
    )?;
    if found {
        Err(JournalError::RetiredId)
    } else {
        Ok(())
    }
}
fn retire(connection: &Connection, kind: &str, query: &str, cutoff: i64) -> Result<()> {
    // Only static SQL owned by this module is used for the query argument.
    let mut statement = connection.prepare(query)?;
    let mut rows = statement.query([cutoff])?;
    while let Some(row) = rows.next()? {
        let id: String = row.get(0)?;
        connection.execute(
            "INSERT INTO retired_ids(kind,id_hash) VALUES(?1,?2)",
            params![kind, retired_hash(&id)],
        )?;
    }
    Ok(())
}

struct LimitedBuffer {
    bytes: Vec<u8>,
    exceeded: bool,
}
impl Write for LimitedBuffer {
    fn write(&mut self, bytes: &[u8]) -> std::io::Result<usize> {
        if self.bytes.len().saturating_add(bytes.len()) > MAX_RECORD_BYTES {
            self.exceeded = true;
            return Err(std::io::Error::other("journal_record_too_large"));
        }
        self.bytes.extend_from_slice(bytes);
        Ok(bytes.len())
    }
    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}
impl LimitedBuffer {
    fn push(&mut self, bytes: &[u8]) -> Result<()> {
        self.write_all(bytes)
            .map_err(|_| JournalError::RecordTooLarge)
    }
    fn scalar(&mut self, value: &Value) -> Result<()> {
        serde_json::to_writer(&mut *self, value).map_err(|_| {
            if self.exceeded {
                JournalError::RecordTooLarge
            } else {
                JournalError::InvalidArguments
            }
        })
    }
}
pub fn canonical_arguments(value: &Value) -> Result<Vec<u8>> {
    if !value.is_object() {
        return Err(JournalError::InvalidArguments);
    }
    let mut out = LimitedBuffer {
        bytes: Vec::new(),
        exceeded: false,
    };
    fn write(value: &Value, out: &mut LimitedBuffer, depth: usize) -> Result<()> {
        if depth > 64 {
            return Err(JournalError::InvalidArguments);
        }
        match value {
            Value::Object(fields) => {
                out.push(b"{")?;
                let mut names: Vec<_> = fields.keys().collect();
                names.sort_unstable();
                for (index, name) in names.into_iter().enumerate() {
                    if index > 0 {
                        out.push(b",")?;
                    }
                    out.scalar(&Value::String(name.clone()))?;
                    out.push(b":")?;
                    write(&fields[name], out, depth + 1)?;
                }
                out.push(b"}")
            }
            Value::Array(items) => {
                out.push(b"[")?;
                for (index, item) in items.iter().enumerate() {
                    if index > 0 {
                        out.push(b",")?;
                    }
                    write(item, out, depth + 1)?;
                }
                out.push(b"]")
            }
            _ => out.scalar(value),
        }
    }
    write(value, &mut out, 0)?;
    Ok(out.bytes)
}

const SCHEMA: &str = r#"
CREATE TABLE owner(id INTEGER PRIMARY KEY CHECK(id=1), binding BLOB NOT NULL CHECK(length(binding)=32));
CREATE TABLE turns(sequence INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT UNIQUE NOT NULL, created_at_ms INTEGER NOT NULL,
    payload BLOB NOT NULL CHECK(length(payload)<=8388608), payload_sha256 BLOB NOT NULL CHECK(length(payload_sha256)=32));
CREATE TABLE events(sequence INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT UNIQUE NOT NULL, turn_id TEXT REFERENCES turns(id), kind TEXT NOT NULL,
    created_at_ms INTEGER NOT NULL, payload BLOB NOT NULL CHECK(length(payload)<=8388608),
    payload_sha256 BLOB NOT NULL CHECK(length(payload_sha256)=32));
CREATE INDEX events_turn ON events(turn_id,sequence);
CREATE TABLE operations(id TEXT PRIMARY KEY NOT NULL, turn_id TEXT REFERENCES turns(id), tool TEXT NOT NULL,
    arguments BLOB NOT NULL CHECK(length(arguments)<=8388608), idempotency_key TEXT, mutating INTEGER NOT NULL CHECK(mutating IN(0,1)),
    state TEXT NOT NULL CHECK(state IN('prepared','dispatched','completed','unknown','rejected')), created_at_ms INTEGER NOT NULL);
CREATE INDEX operations_state ON operations(state,id);
CREATE INDEX operations_turn ON operations(turn_id);
CREATE TABLE results(operation_id TEXT PRIMARY KEY NOT NULL REFERENCES operations(id) ON DELETE CASCADE,
    payload BLOB NOT NULL CHECK(length(payload)<=8388608));
CREATE TABLE transitions(sequence INTEGER PRIMARY KEY AUTOINCREMENT, operation_id TEXT NOT NULL REFERENCES operations(id) ON DELETE CASCADE,
    from_state TEXT, to_state TEXT NOT NULL, at_ms INTEGER NOT NULL);
CREATE INDEX transitions_operation ON transitions(operation_id,sequence);
CREATE TABLE retired_ids(kind TEXT NOT NULL, id_hash BLOB NOT NULL CHECK(length(id_hash)=32), PRIMARY KEY(kind,id_hash));
CREATE TRIGGER owner_immutable BEFORE UPDATE ON owner BEGIN SELECT RAISE(ABORT,'immutable'); END;
CREATE TRIGGER turns_immutable BEFORE UPDATE ON turns BEGIN SELECT RAISE(ABORT,'immutable'); END;
CREATE TRIGGER events_immutable BEFORE UPDATE ON events BEGIN SELECT RAISE(ABORT,'immutable'); END;
CREATE TRIGGER results_immutable BEFORE UPDATE ON results BEGIN SELECT RAISE(ABORT,'immutable'); END;
CREATE TRIGGER transitions_immutable BEFORE UPDATE ON transitions BEGIN SELECT RAISE(ABORT,'immutable'); END;
CREATE TRIGGER operation_identity_immutable BEFORE UPDATE OF id,turn_id,tool,arguments,idempotency_key,mutating,created_at_ms ON operations
    BEGIN SELECT RAISE(ABORT,'immutable'); END;
CREATE TRIGGER valid_transition BEFORE UPDATE OF state ON operations
    WHEN NOT ((OLD.state='prepared' AND NEW.state IN('dispatched','rejected')) OR (OLD.state='dispatched' AND NEW.state IN('completed','unknown')))
    BEGIN SELECT RAISE(ABORT,'invalid_transition'); END;
CREATE TRIGGER result_requires_dispatch BEFORE INSERT ON results
    WHEN NOT EXISTS(SELECT 1 FROM operations WHERE id=NEW.operation_id AND state='dispatched')
    BEGIN SELECT RAISE(ABORT,'invalid_transition'); END;
CREATE TRIGGER completed_requires_result BEFORE UPDATE OF state ON operations
    WHEN NEW.state='completed' AND NOT EXISTS(SELECT 1 FROM results WHERE operation_id=NEW.id)
    BEGIN SELECT RAISE(ABORT,'missing_result'); END;
"#;

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn binding() -> Binding<'static> {
        Binding {
            profile_id: "profile-élève",
            account_id: "account-public-id",
            tenant_id: "school-public-id",
            server_id: "https://tutor.example/mcp",
        }
    }
    fn request<'a>(id: &'a str, arguments: &'a Value) -> OperationRequest<'a> {
        OperationRequest {
            id,
            turn_id: Some("turn"),
            tool: "submit_answer",
            arguments,
            idempotency_key: Some("immutable-request-key"),
            mutating: true,
        }
    }
    fn setup() -> (tempfile::TempDir, std::path::PathBuf, Journal) {
        let directory = tempfile::Builder::new()
            .prefix("tutor journal élève 日本語 ")
            .tempdir()
            .unwrap();
        let path = directory.path().join("exact.sqlite");
        let mut journal = Journal::open(&path, &binding()).unwrap();
        journal
            .append_turn("turn", b"original learner request\0\xff")
            .unwrap();
        (directory, path, journal)
    }

    #[test]
    fn exact_turn_event_and_result_survive_reopening() {
        let (_directory, path, mut journal) = setup();
        let text = "élève 日本語\n  answer  \0".as_bytes();
        journal
            .append_event("answer", Some("turn"), "learner_answer", text)
            .unwrap();
        let arguments = json!({"answer": "  answer  ", "score": 1.0});
        journal
            .prepare_operation(request("operation", &arguments))
            .unwrap();
        let permit = journal.mark_dispatched("operation").unwrap();
        assert_eq!(
            permit.operation().idempotency_key.as_deref(),
            Some("immutable-request-key")
        );
        assert_eq!(permit.into_operation().state, OperationState::Dispatched);
        let result = b"{\n  \"actual\": true\n}\0\xff";
        journal.complete_operation("operation", result).unwrap();
        drop(journal);
        let journal = Journal::open(&path, &binding()).unwrap();
        assert_eq!(
            journal.turn("turn").unwrap().unwrap().payload,
            b"original learner request\0\xff"
        );
        assert_eq!(journal.event("answer").unwrap().unwrap().payload, text);
        let operation = journal.operation("operation").unwrap().unwrap();
        assert_eq!(operation.result.as_deref(), Some(result.as_slice()));
        assert_eq!(operation.state, OperationState::Completed);
        assert_eq!(journal.recovered_unknown_count(), 0);
        assert_eq!(
            journal
                .transitions("operation")
                .unwrap()
                .iter()
                .map(|t| t.to)
                .collect::<Vec<_>>(),
            vec![
                OperationState::Prepared,
                OperationState::Dispatched,
                OperationState::Completed
            ]
        );
    }

    #[test]
    fn exact_content_references_survive_reopen_and_cover_each_source_kind() {
        let (_directory, path, mut journal) = setup();
        let answer = b"  exact\nanswer\0\xff";
        journal
            .append_event("answer", Some("turn"), "learner_answer", answer)
            .unwrap();
        let arguments = json!({"answer":"  exact\nanswer"});
        journal
            .prepare_operation(request("operation", &arguments))
            .unwrap();
        assert_eq!(
            journal
                .content_ref(ContentKind::OperationResult, "operation")
                .err(),
            Some(JournalError::NotFound)
        );
        journal.mark_dispatched("operation").unwrap();
        journal
            .complete_operation("operation", b"{\n\"result\": true\n}")
            .unwrap();
        let sources = [
            (
                ContentKind::Turn,
                "turn",
                b"original learner request\0\xff".to_vec(),
            ),
            (ContentKind::Event, "answer", answer.to_vec()),
            (
                ContentKind::OperationArguments,
                "operation",
                canonical_arguments(&arguments).unwrap(),
            ),
            (
                ContentKind::OperationResult,
                "operation",
                b"{\n\"result\": true\n}".to_vec(),
            ),
        ];
        let references: Vec<_> = sources
            .iter()
            .map(|(kind, id, bytes)| {
                let reference = journal.content_ref(*kind, id).unwrap();
                assert_eq!(reference.kind(), *kind);
                assert_eq!(reference.id(), *id);
                assert_eq!(reference.sha256(), <[u8; 32]>::from(Sha256::digest(bytes)));
                reference
            })
            .collect();
        drop(journal);
        let journal = Journal::open(&path, &binding()).unwrap();
        for (reference, (_, _, bytes)) in references.iter().zip(sources) {
            assert_eq!(journal.resolve_content(reference).unwrap(), bytes);
        }
    }

    #[test]
    fn content_references_cannot_cross_profile_binding_even_with_identical_ids() {
        let (_directory, _path, mut journal) = setup();
        journal
            .append_event("answer", Some("turn"), "answer", b"shared bytes")
            .unwrap();
        let reference = journal.content_ref(ContentKind::Event, "answer").unwrap();
        let other_directory = tempfile::tempdir().unwrap();
        let owner = Binding {
            profile_id: "different-profile",
            ..binding()
        };
        let mut other =
            Journal::open(&other_directory.path().join("different.sqlite"), &owner).unwrap();
        other
            .append_event("answer", None, "answer", b"shared bytes")
            .unwrap();
        assert_eq!(
            other.resolve_content(&reference),
            Err(JournalError::BindingMismatch)
        );
        assert_eq!(
            journal.resolve_content(&reference).unwrap(),
            b"shared bytes"
        );
    }

    #[test]
    fn event_reference_pages_follow_insertion_order_and_keep_stable_cursors() {
        let (_directory, _path, mut journal) = setup();
        for id in ["z", "a", "m"] {
            journal
                .append_event(id, Some("turn"), "answer", id.as_bytes())
                .unwrap();
        }
        journal.append_turn("other-turn", b"other").unwrap();
        journal
            .append_event("other", Some("other-turn"), "answer", b"other")
            .unwrap();
        let first = journal.event_refs(Some("turn"), None, 2).unwrap();
        assert_eq!(
            first
                .iter()
                .map(|entry| entry.content.id())
                .collect::<Vec<_>>(),
            ["m", "a"]
        );
        assert_eq!(first[0].bytes, 1);
        assert_eq!(first[0].kind, "answer");
        assert_eq!(first[0].content.kind(), ContentKind::Event);
        let cursor = first.last().unwrap().sequence;
        journal
            .append_event("new", Some("turn"), "answer", b"new")
            .unwrap();
        let second = journal.event_refs(Some("turn"), Some(cursor), 2).unwrap();
        assert_eq!(
            second
                .iter()
                .map(|entry| entry.content.id())
                .collect::<Vec<_>>(),
            ["z"]
        );
        assert_eq!(journal.resolve_content(&second[0].content).unwrap(), b"z");
        assert_eq!(
            journal.event_refs(None, None, 1).unwrap()[0].content.id(),
            "new"
        );
        for limit in [0, 129] {
            assert_eq!(
                journal.event_refs(None, None, limit).err(),
                Some(JournalError::InvalidInput)
            );
        }
        assert_eq!(
            journal.event_refs(None, Some(0), 1).err(),
            Some(JournalError::InvalidInput)
        );
        let last_sequence = journal.event_refs(None, None, 1).unwrap()[0].sequence;
        journal.delete_all().unwrap();
        let later = journal
            .append_event("later", None, "answer", b"later")
            .unwrap();
        assert!(later.sequence > last_sequence);
    }

    #[test]
    fn turn_reference_pages_include_orphans_and_keep_monotonic_cursors() {
        let (_directory, _path, mut journal) = setup();
        for id in ["z", "a", "m"] {
            journal.append_turn(id, id.as_bytes()).unwrap();
        }
        let first = journal.turn_refs(None, 2).unwrap();
        assert_eq!(
            first
                .iter()
                .map(|entry| entry.content.id())
                .collect::<Vec<_>>(),
            ["m", "a"]
        );
        assert_eq!(first[0].content.kind(), ContentKind::Turn);
        assert_eq!(first[0].bytes, 1);
        assert_eq!(journal.resolve_content(&first[0].content).unwrap(), b"m");
        let cursor = first.last().unwrap().sequence;
        let previous_sequence = first[0].sequence;
        journal.append_turn("m", b"m").unwrap();
        assert_eq!(
            journal.turn_refs(None, 1).unwrap()[0].sequence,
            previous_sequence
        );
        journal
            .append_turn("new", b"new turn, no event yet")
            .unwrap();
        let next = journal.turn_refs(Some(cursor), 2).unwrap();
        assert_eq!(
            next.iter()
                .map(|entry| entry.content.id())
                .collect::<Vec<_>>(),
            ["z", "turn"]
        );
        assert!(journal
            .turn_refs(Some(next.last().unwrap().sequence), 2)
            .unwrap()
            .is_empty());
        assert!(journal.event_refs(None, None, 128).unwrap().is_empty());
        for limit in [0, 129] {
            assert_eq!(
                journal.turn_refs(None, limit).err(),
                Some(JournalError::InvalidInput)
            );
        }
        for cursor in [0, -1] {
            assert_eq!(
                journal.turn_refs(Some(cursor), 1).err(),
                Some(JournalError::InvalidInput)
            );
        }
        let last_sequence = journal.turn_refs(None, 1).unwrap()[0].sequence;
        journal.delete_all().unwrap();
        journal.append_turn("later", b"later turn").unwrap();
        let later = journal.turn_refs(None, 1).unwrap();
        assert!(later[0].sequence > last_sequence);
        assert_eq!(
            journal.resolve_content(&later[0].content).unwrap(),
            b"later turn"
        );
    }

    #[test]
    fn forward_turn_pages_include_orphans_with_exclusive_validated_cursors() {
        let (_directory, path, mut journal) = setup();
        for id in ["z", "a", "m"] {
            journal.append_turn(id, id.as_bytes()).unwrap();
        }
        let first = journal.turn_refs_after(None, 2).unwrap();
        assert_eq!(
            first
                .iter()
                .map(|entry| entry.content.id())
                .collect::<Vec<_>>(),
            ["turn", "z"]
        );
        assert!(first[0].sequence < first[1].sequence);
        assert_eq!(first[1].content.kind(), ContentKind::Turn);
        assert_eq!(first[1].bytes, 1);
        assert_eq!(journal.resolve_content(&first[1].content).unwrap(), b"z");
        let cursor = first.last().unwrap().sequence;
        drop(first);
        // Reopening and a repeated append cannot shift or duplicate the cursor.
        drop(journal);
        let mut journal = Journal::open(&path, &binding()).unwrap();
        journal.append_turn("z", b"z").unwrap();
        journal
            .append_turn("new", b"new turn, no event yet")
            .unwrap();
        let next = journal.turn_refs_after(Some(cursor), 2).unwrap();
        assert_eq!(
            next.iter()
                .map(|entry| entry.content.id())
                .collect::<Vec<_>>(),
            ["a", "m"]
        );
        let cursor = next.last().unwrap().sequence;
        drop(next);
        let last = journal.turn_refs_after(Some(cursor), 128).unwrap();
        assert_eq!(last.len(), 1);
        assert_eq!(last[0].content.id(), "new");
        let cursor = last[0].sequence;
        drop(last);
        assert!(journal.turn_refs_after(Some(cursor), 1).unwrap().is_empty());
        assert!(journal.event_refs(None, None, 128).unwrap().is_empty());
        for limit in [0, 129, usize::MAX] {
            assert_eq!(
                journal.turn_refs_after(None, limit).err(),
                Some(JournalError::InvalidInput)
            );
        }
        for invalid_cursor in [0, -1, i64::MIN] {
            assert_eq!(
                journal.turn_refs_after(Some(invalid_cursor), 1).err(),
                Some(JournalError::InvalidInput)
            );
        }
        assert!(journal
            .turn_refs_after(Some(i64::MAX), 1)
            .unwrap()
            .is_empty());
        journal.delete_all().unwrap();
        assert!(journal.turn_refs_after(None, 128).unwrap().is_empty());
        journal.append_turn("later", b"later turn").unwrap();
        let later = journal.turn_refs_after(Some(cursor), 1).unwrap();
        assert_eq!(later.len(), 1);
        assert!(later[0].sequence > cursor);
        assert_eq!(
            journal.resolve_content(&later[0].content).unwrap(),
            b"later turn"
        );
    }

    #[test]
    fn deleted_or_changed_evidence_never_resolves_as_a_replacement_summary() {
        let (_directory, _path, mut journal) = setup();
        journal
            .append_event("answer", Some("turn"), "answer", b"exact answer")
            .unwrap();
        let reference = journal.event_refs(None, None, 1).unwrap().remove(0).content;
        // Deliberate corruption beyond the public API: a captured reference
        // must reject changed bytes even if a SQL immutability trigger is removed.
        journal
            .connection
            .execute_batch(
                "DROP TRIGGER events_immutable; UPDATE events SET payload=X'00' WHERE id='answer';",
            )
            .unwrap();
        assert_eq!(
            journal.resolve_content(&reference),
            Err(JournalError::Conflict)
        );
        let page = journal.event_refs(None, None, 1).unwrap();
        assert_eq!(
            journal.resolve_content(&page[0].content),
            Err(JournalError::Conflict)
        );
        let fresh_reference = journal.content_ref(ContentKind::Event, "answer").unwrap();
        assert_eq!(
            journal.resolve_content(&fresh_reference),
            Err(JournalError::Conflict)
        );
        journal.delete_all().unwrap();
        assert_eq!(
            journal.resolve_content(&reference),
            Err(JournalError::NotFound)
        );
    }

    #[test]
    fn every_binding_dimension_is_verified_before_recovery() {
        let (_directory, path, mut journal) = setup();
        journal
            .prepare_operation(request("operation", &json!({})))
            .unwrap();
        journal.mark_dispatched("operation").unwrap();
        drop(journal);
        for field in 0..4 {
            let mut wrong = binding();
            match field {
                0 => wrong.profile_id = "other",
                1 => wrong.account_id = "other",
                2 => wrong.tenant_id = "other",
                _ => wrong.server_id = "https://different.example",
            }
            assert_eq!(
                Journal::open(&path, &wrong).err(),
                Some(JournalError::BindingMismatch)
            );
        }
        let journal = Journal::open(&path, &binding()).unwrap();
        assert_eq!(journal.recovered_unknown_count(), 1);
        assert_eq!(
            journal.operation("operation").unwrap().unwrap().state,
            OperationState::Unknown
        );
    }

    #[test]
    fn concurrent_open_cannot_reclassify_an_active_dispatch() {
        let (_directory, path, mut journal) = setup();
        journal
            .prepare_operation(request("operation", &json!({})))
            .unwrap();
        journal.mark_dispatched("operation").unwrap();
        assert_eq!(
            Journal::open(&path, &binding()).err(),
            Some(JournalError::InUse)
        );
        assert_eq!(
            journal.operation("operation").unwrap().unwrap().state,
            OperationState::Dispatched
        );
        journal.complete_operation("operation", b"result").unwrap();
    }

    #[test]
    fn independent_session_files_with_same_binding_can_remain_open_together() {
        let (directory, first_path, mut first) = setup();
        let second_path = directory.path().join("second-session.sqlite");
        let mut second = Journal::open(&second_path, &binding()).unwrap();
        second.append_turn("turn", b"second session input").unwrap();
        let arguments = json!({"answer": "exact"});
        first
            .prepare_operation(request("operation", &arguments))
            .unwrap();
        second
            .prepare_operation(request("operation", &arguments))
            .unwrap();
        first.mark_dispatched("operation").unwrap();
        assert_eq!(
            second.operation("operation").unwrap().unwrap().state,
            OperationState::Prepared
        );
        assert_eq!(
            second.turn("turn").unwrap().unwrap().payload,
            b"second session input"
        );
        drop(first);
        // Recovering the first file must neither block on nor reclassify work
        // in the other session, whose SQLite connection is still open.
        let first = Journal::open(&first_path, &binding()).unwrap();
        assert_eq!(first.recovered_unknown_count(), 1);
        assert_eq!(
            first.operation("operation").unwrap().unwrap().state,
            OperationState::Unknown
        );
        assert_eq!(
            second.operation("operation").unwrap().unwrap().state,
            OperationState::Prepared
        );
        second.mark_dispatched("operation").unwrap();
        second
            .complete_operation("operation", b"second session result")
            .unwrap();
        assert!(first
            .operation("operation")
            .unwrap()
            .unwrap()
            .result
            .is_none());
        assert_eq!(
            Journal::open(&second_path, &binding()).err(),
            Some(JournalError::InUse)
        );
    }

    #[test]
    fn unknown_is_not_replayed_and_prepared_stays_prepared() {
        let (_directory, path, mut journal) = setup();
        let args = json!({"answer": "exact"});
        journal
            .prepare_operation(request("prepared", &args))
            .unwrap();
        journal
            .prepare_operation(request("uncertain", &args))
            .unwrap();
        journal.mark_dispatched("uncertain").unwrap();
        drop(journal);
        let mut journal = Journal::open(&path, &binding()).unwrap();
        assert_eq!(
            journal
                .prepare_operation(request("prepared", &args))
                .unwrap()
                .state,
            OperationState::Prepared
        );
        assert_eq!(
            journal
                .prepare_operation(request("uncertain", &args))
                .unwrap()
                .state,
            OperationState::Unknown
        );
        assert_eq!(
            journal.mark_dispatched("uncertain").err(),
            Some(JournalError::InvalidTransition)
        );
        assert_eq!(
            journal.complete_operation("uncertain", b"guessed"),
            Err(JournalError::InvalidTransition)
        );
        assert_eq!(
            journal
                .operation_ids(OperationState::Unknown, "", 5)
                .unwrap(),
            ["uncertain"]
        );
        drop(journal);
        let journal = Journal::open(&path, &binding()).unwrap();
        assert_eq!(journal.recovered_unknown_count(), 0);
        assert_eq!(journal.transitions("uncertain").unwrap().len(), 3);
    }

    #[test]
    fn canonical_arguments_preserve_numbers_unicode_arrays_and_whitespace_values() {
        let a: Value = serde_json::from_str(
            r#"{"z":[3,2,1],"a":{"z":1.0,"x":18446744073709551615,"v":"  élève  "}}"#,
        )
        .unwrap();
        let b: Value = serde_json::from_str(
            r#"{"a":{"v":"  élève  ","x":18446744073709551615,"z":1.0},"z":[3,2,1]}"#,
        )
        .unwrap();
        let bytes = canonical_arguments(&a).unwrap();
        assert_eq!(bytes, canonical_arguments(&b).unwrap());
        let text = String::from_utf8(bytes).unwrap();
        assert!(
            text.contains("18446744073709551615")
                && text.contains("1.0")
                && text.contains("  élève  ")
        );
        assert_ne!(
            canonical_arguments(&json!({"n":1.0})).unwrap(),
            canonical_arguments(&json!({"n":1})).unwrap()
        );
        assert_ne!(
            canonical_arguments(&json!({"a":[1,2]})).unwrap(),
            canonical_arguments(&json!({"a":[2,1]})).unwrap()
        );
        // Server request hashing preserves arbitrary JSON integers as well.
        // A float conversion here would conflate distinct idempotent writes.
        let huge: Value = serde_json::from_str(r#"{"n":184467440737095516160000000001}"#).unwrap();
        let adjacent: Value =
            serde_json::from_str(r#"{"n":184467440737095516160000000002}"#).unwrap();
        assert_eq!(
            canonical_arguments(&huge).unwrap(),
            br#"{"n":184467440737095516160000000001}"#
        );
        assert_ne!(
            canonical_arguments(&huge).unwrap(),
            canonical_arguments(&adjacent).unwrap()
        );
    }

    #[test]
    fn duplicate_operation_ids_preserve_all_identity_fields() {
        let (_directory, _path, mut journal) = setup();
        journal.append_turn("other-turn", b"another").unwrap();
        let arguments = json!({"answer": "exact", "nested": {"b":2,"a":1}});
        journal
            .prepare_operation(request("operation", &arguments))
            .unwrap();
        let reordered: Value =
            serde_json::from_str(r#"{"nested":{"a":1,"b":2},"answer":"exact"}"#).unwrap();
        journal
            .prepare_operation(request("operation", &reordered))
            .unwrap();
        assert_eq!(journal.transitions("operation").unwrap().len(), 1);
        let changed = json!({"answer": "different"});
        for field in 0..5 {
            let mut next = request("operation", &arguments);
            match field {
                0 => next.arguments = &changed,
                1 => next.idempotency_key = Some("new-key"),
                2 => next.tool = "other_tool",
                3 => next.turn_id = Some("other-turn"),
                _ => next.mutating = false,
            }
            assert_eq!(
                journal.prepare_operation(next).err(),
                Some(JournalError::Conflict)
            );
        }
        journal.mark_dispatched("operation").unwrap();
        assert_eq!(
            journal.mark_dispatched("operation").err(),
            Some(JournalError::InvalidTransition)
        );
        journal
            .complete_operation("operation", b"exact result")
            .unwrap();
        assert_eq!(
            journal
                .prepare_operation(request("operation", &arguments))
                .unwrap()
                .state,
            OperationState::Completed
        );
        assert_eq!(journal.transitions("operation").unwrap().len(), 3);
    }

    #[test]
    fn immutable_records_cannot_be_overwritten_through_api_or_sql() {
        let (_directory, _path, mut journal) = setup();
        journal
            .append_event("event", Some("turn"), "answer", b"original")
            .unwrap();
        journal
            .append_event("event", Some("turn"), "answer", b"original")
            .unwrap();
        assert_eq!(
            journal.append_turn("turn", b"different").err(),
            Some(JournalError::Conflict)
        );
        assert_eq!(
            journal
                .append_event("event", Some("turn"), "answer", b"different")
                .err(),
            Some(JournalError::Conflict)
        );
        journal
            .prepare_operation(request("operation", &json!({})))
            .unwrap();
        assert_eq!(
            journal.complete_operation("operation", b"too early"),
            Err(JournalError::InvalidTransition)
        );
        journal.mark_dispatched("operation").unwrap();
        journal
            .complete_operation("operation", b"original result")
            .unwrap();
        journal
            .complete_operation("operation", b"original result")
            .unwrap();
        assert_eq!(
            journal.complete_operation("operation", b"replacement"),
            Err(JournalError::Conflict)
        );
        for statement in [
            "UPDATE turns SET payload=X'00'",
            "UPDATE events SET payload=X'00'",
            "UPDATE results SET payload=X'00'",
            "UPDATE operations SET arguments=X'00'",
            "UPDATE operations SET state='prepared'",
            "UPDATE transitions SET at_ms=0",
        ] {
            assert!(journal.connection.execute(statement, []).is_err());
        }
        assert_eq!(journal.transitions("operation").unwrap().len(), 3);
    }

    #[test]
    fn failed_audit_write_rolls_back_dispatch_and_never_issues_a_permit() {
        let (_directory, path, mut journal) = setup();
        journal
            .prepare_operation(request("operation", &json!({})))
            .unwrap();
        journal.connection.execute_batch("CREATE TRIGGER fail_dispatch BEFORE INSERT ON transitions WHEN NEW.to_state='dispatched'
            BEGIN SELECT RAISE(ABORT,'PRIVATE_SECRET /private/user/path'); END;").unwrap();
        let outcome = journal.mark_dispatched("operation");
        assert!(outcome.is_err());
        let error = outcome.err().unwrap();
        assert_eq!(error, JournalError::Storage);
        assert!(!error.to_string().contains("PRIVATE") && !format!("{error:?}").contains("path"));
        assert!(std::error::Error::source(&error).is_none());
        assert_eq!(
            journal.operation("operation").unwrap().unwrap().state,
            OperationState::Prepared
        );
        assert_eq!(journal.transitions("operation").unwrap().len(), 1);
        drop(journal);
        let journal = Journal::open(&path, &binding()).unwrap();
        assert_eq!(journal.recovered_unknown_count(), 0);
    }

    #[test]
    fn failed_result_transition_does_not_leave_a_partial_result() {
        let (_directory, _path, mut journal) = setup();
        journal
            .prepare_operation(request("operation", &json!({})))
            .unwrap();
        journal.mark_dispatched("operation").unwrap();
        journal.connection.execute_batch("CREATE TRIGGER fail_complete BEFORE INSERT ON transitions WHEN NEW.to_state='completed'
            BEGIN SELECT RAISE(ABORT,'private contents'); END;").unwrap();
        assert_eq!(
            journal.complete_operation("operation", b"result"),
            Err(JournalError::Storage)
        );
        let saved = journal.operation("operation").unwrap().unwrap();
        assert_eq!(saved.state, OperationState::Dispatched);
        assert!(saved.result.is_none());
        assert_eq!(journal.transitions("operation").unwrap().len(), 2);
    }

    #[test]
    fn storage_read_only_failure_blocks_prepare_and_dispatch() {
        let (_directory, _path, mut journal) = setup();
        journal
            .connection
            .pragma_update(None, "query_only", true)
            .unwrap();
        assert_eq!(
            journal
                .prepare_operation(request("operation", &json!({})))
                .err(),
            Some(JournalError::Storage)
        );
        assert_eq!(
            journal.mark_dispatched("operation").err(),
            Some(JournalError::Storage)
        );
        assert!(journal.operation("operation").unwrap().is_none());
    }

    #[test]
    fn unresolved_mutations_block_all_retention_without_partial_deletion() {
        for state in [
            OperationState::Prepared,
            OperationState::Dispatched,
            OperationState::Unknown,
        ] {
            let (_directory, _path, mut journal) = setup();
            journal
                .append_event("event", Some("turn"), "answer", b"keep")
                .unwrap();
            journal
                .prepare_operation(request("operation", &json!({})))
                .unwrap();
            if state != OperationState::Prepared {
                journal.mark_dispatched("operation").unwrap();
            }
            if state == OperationState::Unknown {
                journal.mark_unknown("operation").unwrap();
            }
            assert_eq!(journal.delete_all(), Err(JournalError::RetentionBlocked));
            assert_eq!(
                journal.retain_since(i64::MAX),
                Err(JournalError::RetentionBlocked)
            );
            assert!(journal.turn("turn").unwrap().is_some());
            assert!(journal.event("event").unwrap().is_some());
            assert_eq!(
                journal.operation("operation").unwrap().unwrap().state,
                state
            );
        }
    }

    #[test]
    fn explicit_deletion_preserves_binding_and_replay_guards() {
        let (_directory, path, mut journal) = setup();
        journal
            .append_event("event", Some("turn"), "answer", b"exact")
            .unwrap();
        let args = json!({"answer":"private"});
        journal
            .prepare_operation(request("operation", &args))
            .unwrap();
        journal.reject_prepared("operation").unwrap();
        assert_eq!(
            journal.delete_all().unwrap(),
            Deleted {
                turns: 1,
                events: 1,
                operations: 1
            }
        );
        drop(journal);
        let mut journal = Journal::open(&path, &binding()).unwrap();
        assert!(journal.turn("turn").unwrap().is_none());
        assert!(journal.operation("operation").unwrap().is_none());
        assert_eq!(
            journal.append_turn("turn", b"new contents").err(),
            Some(JournalError::RetiredId)
        );
        assert_eq!(
            journal
                .append_event("event", None, "other", b"new contents")
                .err(),
            Some(JournalError::RetiredId)
        );
        assert_eq!(
            journal.prepare_operation(request("operation", &args)).err(),
            Some(JournalError::RetiredId)
        );
        drop(journal);
        let mut wrong = binding();
        wrong.account_id = "someone-else";
        assert_eq!(
            Journal::open(&path, &wrong).err(),
            Some(JournalError::BindingMismatch)
        );
    }

    #[test]
    fn retention_keeps_parent_turns_needed_by_newer_children() {
        let (_directory, _path, mut journal) = setup();
        let cutoff = journal.turn("turn").unwrap().unwrap().created_at_ms + 1;
        std::thread::sleep(Duration::from_millis(5));
        journal
            .append_event("new", Some("turn"), "feedback", b"newer evidence")
            .unwrap();
        assert_eq!(journal.retain_since(cutoff).unwrap(), Deleted::default());
        assert!(journal.turn("turn").unwrap().is_some());
        let arguments = json!({});
        let mut read = request("read", &arguments);
        read.mutating = false;
        journal.prepare_operation(read).unwrap();
        journal.mark_dispatched("read").unwrap();
        journal.mark_unknown("read").unwrap();
        assert_eq!(
            journal.delete_all().unwrap(),
            Deleted {
                turns: 1,
                events: 1,
                operations: 1
            }
        );
    }

    #[test]
    fn recent_result_keeps_the_entire_older_operation_and_its_turn() {
        let (_directory, _path, mut journal) = setup();
        let operation = journal
            .prepare_operation(request("operation", &json!({})))
            .unwrap();
        let cutoff = operation.created_at_ms + 1;
        std::thread::sleep(Duration::from_millis(5));
        journal.mark_dispatched("operation").unwrap();
        journal
            .complete_operation("operation", b"new result")
            .unwrap();
        assert_eq!(journal.retain_since(cutoff).unwrap(), Deleted::default());
        assert_eq!(
            journal
                .operation("operation")
                .unwrap()
                .unwrap()
                .result
                .as_deref(),
            Some(b"new result".as_slice())
        );
        assert!(journal.turn("turn").unwrap().is_some());
    }

    #[test]
    fn failed_retention_rolls_back_deleted_records_and_retired_identifiers() {
        let (_directory, _path, mut journal) = setup();
        journal
            .append_event("event", Some("turn"), "kind", b"keep")
            .unwrap();
        journal
            .prepare_operation(request("operation", &json!({})))
            .unwrap();
        journal.reject_prepared("operation").unwrap();
        journal
            .connection
            .execute_batch(
                "CREATE TRIGGER fail_delete BEFORE DELETE ON events
            BEGIN SELECT RAISE(ABORT,'private data'); END;",
            )
            .unwrap();
        assert_eq!(journal.delete_all(), Err(JournalError::Storage));
        assert!(journal.event("event").unwrap().is_some());
        assert_eq!(
            journal.operation("operation").unwrap().unwrap().state,
            OperationState::Rejected
        );
        let retired: i64 = journal
            .connection
            .query_row("SELECT count(*) FROM retired_ids", [], |row| row.get(0))
            .unwrap();
        assert_eq!(retired, 0);
        assert_eq!(
            journal
                .prepare_operation(request("operation", &json!({})))
                .unwrap()
                .state,
            OperationState::Rejected
        );
    }

    #[test]
    fn oversized_records_and_excessive_json_depth_do_not_create_records() {
        let (_directory, _path, mut journal) = setup();
        let huge = vec![b'x'; MAX_RECORD_BYTES];
        assert_eq!(
            journal.append_turn("oversized", &huge).err(),
            Some(JournalError::RecordTooLarge)
        );
        assert_eq!(
            journal.append_event("oversized", None, "kind", &huge).err(),
            Some(JournalError::RecordTooLarge)
        );
        let args = json!({"huge":"x".repeat(MAX_RECORD_BYTES)});
        assert_eq!(
            journal.prepare_operation(request("oversized", &args)).err(),
            Some(JournalError::RecordTooLarge)
        );
        assert!(journal.operation("oversized").unwrap().is_none());
        journal
            .prepare_operation(request("valid", &json!({})))
            .unwrap();
        journal.mark_dispatched("valid").unwrap();
        assert_eq!(
            journal.complete_operation("valid", &huge),
            Err(JournalError::RecordTooLarge)
        );
        let saved = journal.operation("valid").unwrap().unwrap();
        assert_eq!(saved.state, OperationState::Dispatched);
        assert!(saved.result.is_none());
        let mut nested = json!({});
        for _ in 0..66 {
            nested = json!({"nested":nested});
        }
        assert_eq!(
            canonical_arguments(&nested),
            Err(JournalError::InvalidArguments)
        );
    }

    #[test]
    fn journal_can_exceed_workspace_size_without_truncating_records() {
        let (_directory, path, mut journal) = setup();
        let payload = vec![b'X'; 7 * 1024 * 1024];
        for index in 0..5 {
            journal
                .append_event(
                    &format!("event-{index}"),
                    Some("turn"),
                    "tool_result",
                    &payload,
                )
                .unwrap();
        }
        assert!(path.metadata().unwrap().len() > 32 * 1024 * 1024);
        assert_eq!(
            journal.event("event-4").unwrap().unwrap().payload.len(),
            payload.len()
        );
        drop(journal);
        let journal = Journal::open(&path, &binding()).unwrap();
        assert_eq!(journal.event("event-0").unwrap().unwrap().payload, payload);
    }

    #[test]
    fn owner_identifiers_are_hashed_and_unrelated_databases_are_not_replaced() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("private.sqlite");
        let owner = Binding {
            account_id: "PRIVATE_BINDING_SHOULD_NOT_BE_STORED_VERBATIM",
            ..binding()
        };
        drop(Journal::open(&path, &owner).unwrap());
        let bytes = std::fs::read(&path).unwrap();
        assert!(!bytes
            .windows(owner.account_id.len())
            .any(|window| window == owner.account_id.as_bytes()));
        let other = directory.path().join("foreign.sqlite");
        private_database_file(&other).unwrap();
        let connection = Connection::open(&other).unwrap();
        connection
            .execute_batch(
                "CREATE TABLE unrelated(value TEXT); INSERT INTO unrelated VALUES('preserve');",
            )
            .unwrap();
        drop(connection);
        assert_eq!(
            Journal::open(&other, &binding()).err(),
            Some(JournalError::UnsupportedSchema)
        );
        let connection = Connection::open(&other).unwrap();
        assert_eq!(
            connection
                .query_row("SELECT value FROM unrelated", [], |r| r.get::<_, String>(0))
                .unwrap(),
            "preserve"
        );
    }

    #[cfg(unix)]
    #[test]
    fn unsafe_files_are_rejected_and_new_files_are_private() {
        use std::os::unix::fs::{symlink, PermissionsExt};
        let (_directory, path, journal) = setup();
        assert_eq!(path.metadata().unwrap().permissions().mode() & 0o777, 0o600);
        drop(journal);
        let link = path.with_extension("link");
        symlink(&path, &link).unwrap();
        assert_eq!(
            Journal::open(&link, &binding()).err(),
            Some(JournalError::UnsafeFile)
        );
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o644)).unwrap();
        assert_eq!(
            Journal::open(&path, &binding()).err(),
            Some(JournalError::UnsafeFile)
        );
    }

    #[test]
    fn crash_child() {
        let Some(path) = std::env::var_os("TUTOR_JOURNAL_CRASH_PATH") else {
            return;
        };
        let phase = std::env::var("TUTOR_JOURNAL_CRASH_PHASE").unwrap();
        let mut journal = Journal::open(Path::new(&path), &binding()).unwrap();
        journal
            .append_turn("turn", b"exact pre-crash turn")
            .unwrap();
        if phase == "turn_only" {
            // User input committed, but no generation event was ever written.
            std::process::exit(23);
        }
        journal
            .prepare_operation(request("operation", &json!({"answer":"exact"})))
            .unwrap();
        if phase == "dispatched" {
            journal.mark_dispatched("operation").unwrap();
        } else if phase == "uncommitted" {
            journal.connection.execute_batch("BEGIN IMMEDIATE;
                UPDATE operations SET state='dispatched' WHERE id='operation';
                INSERT INTO transitions(operation_id,from_state,to_state,at_ms) VALUES('operation','prepared','dispatched',1);").unwrap();
        }
        // Simulate process death: no destructors/SQLite rollback/close run.
        std::process::exit(23);
    }

    #[test]
    fn process_death_after_turn_commit_before_any_event_leaves_discoverable_input() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("orphan-turn.sqlite");
        let output = std::process::Command::new(std::env::current_exe().unwrap())
            .args(["--exact", "journal::tests::crash_child"])
            .env("TUTOR_JOURNAL_CRASH_PATH", &path)
            .env("TUTOR_JOURNAL_CRASH_PHASE", "turn_only")
            .output()
            .unwrap();
        assert_eq!(output.status.code(), Some(23));
        let journal = Journal::open(&path, &binding()).unwrap();
        let turns = journal.turn_refs(None, 128).unwrap();
        assert_eq!(turns.len(), 1);
        assert_eq!(turns[0].content.id(), "turn");
        assert_eq!(turns[0].content.kind(), ContentKind::Turn);
        assert_eq!(
            journal.resolve_content(&turns[0].content).unwrap(),
            b"exact pre-crash turn"
        );
        assert!(journal
            .event_refs(Some("turn"), None, 128)
            .unwrap()
            .is_empty());
        assert_eq!(journal.recovered_unknown_count(), 0);
        for state in [
            OperationState::Prepared,
            OperationState::Dispatched,
            OperationState::Unknown,
        ] {
            assert!(journal.operation_ids(state, "", 128).unwrap().is_empty());
        }
        // Reopening only exposes the exact evidence; no event or tool action is
        // manufactured. The ACP integration appends its explicit interrupted event.
        drop(journal);
        let journal = Journal::open(&path, &binding()).unwrap();
        assert_eq!(journal.turn_refs(None, 128).unwrap().len(), 1);
        assert!(journal.event_refs(None, None, 128).unwrap().is_empty());
    }

    #[test]
    fn process_death_recovers_only_committed_dispatch_intents() {
        for (phase, state, transitions) in [
            ("prepared", OperationState::Prepared, 1),
            ("dispatched", OperationState::Unknown, 3),
            ("uncommitted", OperationState::Prepared, 1),
        ] {
            let directory = tempfile::tempdir().unwrap();
            let path = directory.path().join("crashed.sqlite");
            let output = std::process::Command::new(std::env::current_exe().unwrap())
                .args(["--exact", "journal::tests::crash_child"])
                .env("TUTOR_JOURNAL_CRASH_PATH", &path)
                .env("TUTOR_JOURNAL_CRASH_PHASE", phase)
                .output()
                .unwrap();
            assert_eq!(output.status.code(), Some(23));
            let journal = Journal::open(&path, &binding()).unwrap();
            assert_eq!(
                journal.operation("operation").unwrap().unwrap().state,
                state
            );
            assert_eq!(journal.transitions("operation").unwrap().len(), transitions);
        }
    }
}
