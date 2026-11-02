// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! Bounded projections of sealed consultation receipts. No content-reference
//! rehydration API, model-provided IDs, selection mutation or automatic paging.

#[cfg(test)]
use crate::{call_validation::VerifiedFields, journal::ContentRef};
use crate::{
    consultation::{Error as ConsultationError, ScopedReceipt},
    mcp_session::Scope,
    session_store::SessionStore,
};
use serde_json::{json, Value};

// Leave room within the existing 64 KiB ACP/session output boundary. Refuse
// the whole projection rather than truncate names or silently omit rows.
const MAX_RENDER_BYTES: usize = 48 * 1024;

#[derive(Clone, Copy, PartialEq, Eq)]
pub(crate) enum Filter {
    All,
    Personal,
    Institutional,
    Shared,
    Mine,
    Review,
    Archived,
}
impl Filter {
    fn text(self) -> &'static str {
        match self {
            Self::All => "",
            Self::Personal => "personal",
            Self::Institutional => "institutional",
            Self::Shared => "shared",
            Self::Mine => "mine",
            Self::Review => "review",
            Self::Archived => "archived",
        }
    }
    fn parse(value: &str) -> Result<Self, Error> {
        match value {
            "" => Ok(Self::All),
            "personal" => Ok(Self::Personal),
            "institutional" => Ok(Self::Institutional),
            "shared" => Ok(Self::Shared),
            "mine" => Ok(Self::Mine),
            "review" => Ok(Self::Review),
            "archived" => Ok(Self::Archived),
            _ => Err(Error::Request),
        }
    }
}

#[derive(Clone, PartialEq, Eq)]
pub(crate) struct LibraryRequest {
    query: String,
    filter: Filter,
    limit: u8,
}
impl LibraryRequest {
    pub(crate) fn new(query: &str, filter: Filter, limit: u8) -> Result<Self, Error> {
        // Server bounds UTF-8 bytes. Always send an explicit bounded limit.
        if query.len() > 200 || query.contains('\0') || !(1..=100).contains(&limit) {
            return Err(Error::Request);
        }
        Ok(Self {
            query: query.to_owned(),
            filter,
            limit,
        })
    }
    pub(crate) fn first_arguments(&self) -> Value {
        self.arguments("")
    }
    fn arguments(&self, after: &str) -> Value {
        let mut args = json!({"limit":self.limit});
        if !self.query.is_empty() {
            args["query"] = json!(self.query);
        }
        if self.filter != Filter::All {
            args["filter"] = json!(self.filter.text());
        }
        if !after.is_empty() {
            args["after"] = json!(after);
        }
        args
    }
    fn from_arguments(args: &Value) -> Result<(Self, String), Error> {
        let object = args.as_object().ok_or(Error::Request)?;
        if object
            .keys()
            .any(|key| !matches!(key.as_str(), "query" | "filter" | "after" | "limit"))
        {
            return Err(Error::Request);
        }
        let text = |key: &str| -> Result<&str, Error> {
            match object.get(key) {
                None => Ok(""),
                Some(value) => value.as_str().ok_or(Error::Request),
            }
        };
        let limit = object
            .get("limit")
            .and_then(Value::as_u64)
            .and_then(|limit| u8::try_from(limit).ok())
            .ok_or(Error::Request)?;
        let request = Self::new(text("query")?, Filter::parse(text("filter")?)?, limit)?;
        let after = text("after")?;
        if after.len() > 255 || after.contains('\0') || request.arguments(after) != *args {
            return Err(Error::Request);
        }
        Ok((request, after.to_owned()))
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum Error {
    Request,
    Scope,
    Receipt,
    Payload,
    #[cfg(test)]
    NoNextPage,
    #[cfg(test)]
    Entry,
    OutputTooLarge,
}
impl From<ConsultationError> for Error {
    fn from(error: ConsultationError) -> Self {
        if error == ConsultationError::Scope {
            Self::Scope
        } else {
            Self::Receipt
        }
    }
}
impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            Self::Request => "library_request_invalid",
            Self::Scope => "library_scope_changed",
            Self::Receipt => "library_receipt_invalid",
            Self::Payload => "library_payload_invalid",
            #[cfg(test)]
            Self::NoNextPage => "library_no_next_page",
            #[cfg(test)]
            Self::Entry => "library_entry_invalid",
            Self::OutputTooLarge => "library_projection_too_large",
        })
    }
}
impl std::error::Error for Error {}

pub(crate) struct Entry {
    id: String,
    #[cfg(test)]
    revision: String,
    name: String,
    kind: String,
    status: String,
    #[cfg(test)]
    can_learn: bool,
}
impl Entry {
    fn read(item: &Value) -> Result<Self, Error> {
        let field = |name: &str, maximum: usize| -> Result<String, Error> {
            item.get(name)
                .and_then(Value::as_str)
                .filter(|s| !s.is_empty() && s.len() <= maximum && !s.contains('\0'))
                .map(str::to_owned)
                .ok_or(Error::Payload)
        };
        let id = field("id", 255)?;
        // Review the concrete Go ID union; a changed contract needs review.
        if id
            .strip_prefix("personal:")
            .or_else(|| id.strip_prefix("formation:"))
            .is_none_or(|suffix| suffix.is_empty())
        {
            return Err(Error::Payload);
        }
        let kind = field("kind", 128)?;
        if id.starts_with("personal:") != (kind == "personal")
            || !matches!(kind.as_str(), "personal" | "institutional" | "shared")
        {
            return Err(Error::Payload);
        }
        // These server fields remain REQUIRED and validated in production;
        // only the unintegrated details projection keeps them for tests.
        let revision = field("revision", 1024)?;
        let can_learn = item
            .get("can_learn")
            .and_then(Value::as_bool)
            .ok_or(Error::Payload)?;
        #[cfg(not(test))]
        let _ = (revision, can_learn);
        Ok(Self {
            id,
            #[cfg(test)]
            revision,
            name: field("name", 8192)?,
            kind,
            status: field("status", 128)?,
            #[cfg(test)]
            can_learn,
        })
    }
    #[cfg(test)]
    pub(crate) fn id(&self) -> &str {
        &self.id
    }
    #[cfg(test)]
    pub(crate) fn revision(&self) -> &str {
        &self.revision
    }
    pub(crate) fn name(&self) -> &str {
        &self.name
    }
    #[cfg(test)]
    pub(crate) fn kind(&self) -> &str {
        &self.kind
    }
    #[cfg(test)]
    pub(crate) fn status(&self) -> &str {
        &self.status
    }
    /// Historical projection only; never a live server grant.
    #[cfg(test)]
    pub(crate) fn can_learn(&self) -> bool {
        self.can_learn
    }
}

pub(crate) struct LibraryPage {
    receipt: ScopedReceipt,
    #[cfg(test)]
    request: LibraryRequest,
    #[cfg(test)]
    after: String,
    entries: Vec<Entry>,
    next_after: String,
}
impl LibraryPage {
    pub(crate) fn read(
        store: &SessionStore,
        current: &Scope,
        receipt: ScopedReceipt,
    ) -> Result<Self, Error> {
        if receipt.tool_id() != "tutor/list_training_library" {
            return Err(Error::Receipt);
        }
        let payload = receipt.payload(store, current)?;
        let (request, after) = LibraryRequest::from_arguments(receipt.arguments())?;
        let (entries, next_after) = decode_page(&payload, &request, &after)?;
        Ok(Self {
            receipt,
            #[cfg(test)]
            request,
            #[cfg(test)]
            after,
            entries,
            next_after,
        })
    }
    /// Revalidate scope before projecting; the returned fields are display data,
    /// not authority to select, teach, enroll or call another tool.
    pub(crate) fn entries(&self, store: &SessionStore, current: &Scope) -> Result<&[Entry], Error> {
        self.receipt.payload(store, current)?;
        Ok(&self.entries)
    }

    /// Deterministic display of THIS sealed page, in exact retained row order.
    /// No model, tool IO, selection, continuation permission or UI ACK. Names
    /// are untrusted plain labels, never Markdown/protocol fragments. The host
    /// remains responsible for current scope/epoch invalidation.
    pub(crate) fn render_text(
        &self,
        store: &SessionStore,
        current: &Scope,
        language: Option<&str>,
    ) -> Result<String, Error> {
        let entries = self.entries(store, current)?;
        let english = language == Some("en");
        if entries.is_empty() {
            return Ok(if english {
                "No training found for this search."
            } else {
                "Aucune formation trouvée pour cette recherche."
            }
            .to_owned());
        }
        let mut text = if english {
            "Training found:\n\n"
        } else {
            "Formations trouvées :\n\n"
        }
        .to_owned();
        for (index, entry) in entries.iter().enumerate() {
            if index != 0 {
                text.push('\n');
            }
            text.push_str(&(index + 1).to_string());
            text.push_str(". ");
            text.push_str(&escape_name(entry.name()));
            if text.len() > MAX_RENDER_BYTES {
                return Err(Error::OutputTooLarge);
            }
        }
        if !self.next_after.is_empty() {
            text.push_str(if english {
                "\n\nPartial results: this page is not exhaustive."
            } else {
                "\n\nRésultats partiels : cette page n’est pas exhaustive."
            });
        }
        if text.len() > MAX_RENDER_BYTES {
            return Err(Error::OutputTooLarge);
        }
        Ok(text)
    }

    #[cfg(test)]
    pub(crate) fn source(&self) -> &ContentRef {
        self.receipt.source()
    }
    #[cfg(test)]
    pub(crate) fn is_first_page(&self) -> bool {
        self.after.is_empty()
    }
    #[cfg(test)]
    pub(crate) fn entry_proof(
        &self,
        store: &SessionStore,
        current: &Scope,
        index: usize,
    ) -> Result<EntryProof, Error> {
        self.receipt.payload(store, current)?;
        let entry = self.entries.get(index).ok_or(Error::Entry)?;
        Ok(EntryProof {
            receipt: self.receipt.clone(),
            index,
            id: entry.id.clone(),
        })
    }
    /// DATA ONLY: this candidate cannot be passed to AdmittedConsultation or
    /// PreparedConsultation. No next-page admission/dispatch API exists yet.
    /// Future state owner must check current acknowledged page/search generation,
    /// scope, exact request and fresh positive user intent BEFORE admitting it.
    #[cfg(test)]
    pub(crate) fn continuation_candidate(
        &self,
        store: &SessionStore,
        current: &Scope,
    ) -> Result<ContinuationCandidate, Error> {
        self.receipt.payload(store, current)?;
        if self.next_after.is_empty() {
            return Err(Error::NoNextPage);
        }
        Ok(ContinuationCandidate {
            source: self.receipt.source().clone(),
            arguments: self.request.arguments(&self.next_after),
        })
    }
}

fn escape_name(name: &str) -> String {
    let mut escaped = String::with_capacity(name.len());
    for character in name.chars() {
        match character {
            // Both desktop protocol scanners inspect raw text before Markdown.
            // No external label may create a new line/fence or hidden control.
            '\u{2028}' | '\u{2029}' => escaped.push(' '),
            character if character.is_control() => escaped.push(' '),
            '\u{061c}'
            | '\u{200e}'
            | '\u{200f}'
            | '\u{202a}'..='\u{202e}'
            | '\u{2066}'..='\u{2069}' => escaped.push('\u{fffd}'),
            '&' => escaped.push_str("&amp;"),
            '<' => escaped.push_str("&lt;"),
            '>' => escaped.push_str("&gt;"),
            // Ampersands are encoded above: keep the original entity's
            // semicolon literal so its spelling survives Markdown rendering.
            ';' => escaped.push(';'),
            // CommonMark permits escaping every ASCII punctuation character.
            // This also neutralizes backtick/tilde fences, links and autolinks.
            character if character.is_ascii_punctuation() => {
                escaped.push('\\');
                escaped.push(character);
            }
            character => escaped.push(character),
        }
    }
    escaped
}

fn decode_page(
    payload: &Value,
    request: &LibraryRequest,
    after: &str,
) -> Result<(Vec<Entry>, String), Error> {
    if after.len() > 255 || after.contains('\0') {
        return Err(Error::Request);
    }
    let items = payload
        .get("items")
        .and_then(Value::as_array)
        .ok_or(Error::Payload)?;
    let next_after = payload
        .get("next_after")
        .and_then(Value::as_str)
        .filter(|s| s.len() <= 255 && !s.contains('\0'))
        .ok_or(Error::Payload)?;
    if items.len() > usize::from(request.limit)
        || (!next_after.is_empty() && items.len() != usize::from(request.limit))
    {
        return Err(Error::Payload);
    }
    let mut entries = Vec::<Entry>::with_capacity(items.len());
    for item in items {
        let entry = Entry::read(item)?;
        let previous = entries
            .last()
            .map(|entry| entry.id.as_str())
            .unwrap_or(after);
        if entry.id.as_str() <= previous {
            return Err(Error::Payload);
        }
        if (request.filter == Filter::Personal && entry.kind != "personal")
            || (request.filter == Filter::Institutional && entry.kind != "institutional")
            || (request.filter == Filter::Shared && entry.kind != "shared")
            || (request.filter == Filter::Archived && entry.status != "archived")
        {
            return Err(Error::Payload);
        }
        // Strict order rejects duplicates. Mine/review grants and query search
        // collation are server-owned; do not infer authorization from metadata.
        entries.push(entry);
    }
    if !next_after.is_empty() && entries.last().map(|entry| entry.id.as_str()) != Some(next_after) {
        return Err(Error::Payload);
    }
    Ok((entries, next_after.to_owned()))
}

/// Sealed reference to one exact row. No constructor or Deserialize. A host
/// resolver chooses the index from verified state, never a model-provided ID.
#[cfg(test)]
pub(crate) struct EntryProof {
    receipt: ScopedReceipt,
    index: usize,
    id: String,
}
#[cfg(test)]
impl EntryProof {
    pub(crate) fn check(
        &self,
        store: &SessionStore,
        current: &Scope,
    ) -> Result<(), ConsultationError> {
        let payload = self.receipt.payload(store, current)?;
        if self.receipt.tool_id() != "tutor/list_training_library"
            || payload
                .get("items")
                .and_then(Value::as_array)
                .and_then(|items| items.get(self.index))
                .and_then(|entry| entry.get("id"))
                .and_then(Value::as_str)
                != Some(self.id.as_str())
        {
            return Err(ConsultationError::Receipt);
        }
        Ok(())
    }
    pub(crate) fn verified_fields(
        &self,
        store: &SessionStore,
        current: &Scope,
        binding: &str,
    ) -> Result<VerifiedFields, ConsultationError> {
        self.check(store, current)?;
        let mut fields = VerifiedFields::new(binding.to_owned(), store.journal())?;
        let pointer = format!("/items/{}/id", self.index);
        fields.bind_result(store.journal(), self.receipt.source(), &[("id", &pointer)])?;
        Ok(fields)
    }
    pub(crate) fn arguments(
        &self,
        store: &SessionStore,
        current: &Scope,
    ) -> Result<Value, ConsultationError> {
        self.check(store, current)?;
        Ok(json!({"id":self.id}))
    }
}

#[cfg(test)]
pub(crate) struct ContinuationCandidate {
    source: ContentRef,
    arguments: Value,
}
#[cfg(test)]
impl ContinuationCandidate {
    pub(crate) fn source(&self) -> &ContentRef {
        &self.source
    }
    pub(crate) fn arguments(&self) -> &Value {
        &self.arguments
    }
}

/// Only the reviewed display metadata is projected here. Programme/progress
/// interpretation, active selection and teaching remain outside this package.
#[cfg(test)]
pub(crate) struct TrainingDetails {
    receipt: ScopedReceipt,
    training: Entry,
}
#[cfg(test)]
impl TrainingDetails {
    pub(crate) fn read(
        store: &SessionStore,
        current: &Scope,
        receipt: ScopedReceipt,
    ) -> Result<Self, Error> {
        if receipt.tool_id() != "tutor/get_training" {
            return Err(Error::Receipt);
        }
        let payload = receipt.payload(store, current)?;
        let training = Entry::read(payload.get("training").ok_or(Error::Payload)?)?;
        if receipt.arguments() != &json!({"id":training.id}) {
            return Err(Error::Receipt);
        }
        Ok(Self { receipt, training })
    }
    pub(crate) fn training(&self, store: &SessionStore, current: &Scope) -> Result<&Entry, Error> {
        self.receipt.payload(store, current)?;
        Ok(&self.training)
    }
    pub(crate) fn source(&self) -> &ContentRef {
        self.receipt.source()
    }
}

#[cfg(test)]
mod tests;
