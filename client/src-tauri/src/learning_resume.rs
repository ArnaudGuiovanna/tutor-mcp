// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT
//! Explicit learning commands. General conversation and answers stay untouched.
use super::*;

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(super) struct ResumeRequest {
    pub version: u8,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub training_id: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub training_name: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub domain_id: Option<String>,
}

pub(super) fn resume_request(message: &str) -> Result<Option<ResumeRequest>, String> {
    if let Some(request) = payload::<ResumeRequest>(message, "tutor-resume-request")? {
        if request.version != 1
            || !optional_bounded(&request.training_id, 256)
            || !optional_bounded(&request.training_name, 200)
            || !optional_bounded(&request.domain_id, 256)
            || (request.domain_id.is_some() && request.training_id.is_none())
        {
            return Err(INVALID.into());
        }
        return Ok(Some(request));
    }
    // Recognize complete, short commands, not fragments in quoted history,
    // attachments, multi-intent messages or requests to rewrite an explanation.
    if message.len() > 1200 || message.contains(['\n', '\r', '`', ';']) {
        return Ok(None);
    }
    let text = message.trim().trim_end_matches(['.', '!', '?']).trim();
    let normalized = text.to_lowercase();
    if [" puis ", " ensuite ", " et ", " then ", " and "]
        .iter()
        .any(|word| normalized.contains(word))
    {
        return Ok(None);
    }
    let mut command = text;
    for prefix in [
        "peux-tu ",
        "peux tu ",
        "je souhaite ",
        "je veux ",
        "can you ",
        "please ",
    ] {
        if normalized.starts_with(prefix) {
            command = &text[prefix.len()..];
            break;
        }
    }
    let lower = command.to_lowercase();
    // A bare "continue" often asks to continue an explanation, not to choose
    // another pedagogical activity. Require a course target for that verb.
    if lower == "continue" {
        return Ok(None);
    }
    let mut remainder = None;
    for verb in ["reprendre", "reprends", "reprenons", "resume", "continue"] {
        if lower == verb {
            remainder = Some("");
            break;
        }
        if lower.starts_with(&format!("{verb} ")) {
            remainder = Some(command[verb.len()..].trim());
            break;
        }
    }
    let Some(rest) = remainder else {
        return Ok(None);
    };
    let mut name = if rest.is_empty() { Some("") } else { None };
    let rest_lower = rest.to_lowercase();
    for prefix in [
        "la formation",
        "ma formation",
        "cette formation",
        "formation",
        "le cours",
        "mon cours",
        "ce cours",
        "cours",
        "the course",
        "my course",
        "this course",
        "course",
        "training",
    ] {
        if rest_lower == prefix {
            name = Some("");
            break;
        }
        if rest_lower.starts_with(&format!("{prefix} ")) {
            name = Some(rest[prefix.len()..].trim());
            break;
        }
    }
    let Some(name) = name else { return Ok(None) };
    let name = name.trim_matches(['«', '»', '"', '“', '”']).trim();
    if !name.is_empty() && !bounded(name, 200) {
        return Err(INVALID.into());
    }
    Ok(Some(ResumeRequest {
        version: 1,
        training_id: None,
        training_name: (!name.is_empty()).then(|| name.to_string()),
        domain_id: None,
    }))
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(super) struct Introduction {
    pub version: u8,
    pub training_id: String,
    pub domain_id: String,
    pub concept: String,
    pub session_id: String,
    pub decision_id: String,
    pub body: String,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn explicit_resume_commands_keep_target_and_never_parse_answers_or_help() {
        for message in [
            "Reprendre la formation harness.",
            "Peux-tu reprendre la formation « harness » ?",
            "Resume the course harness",
        ] {
            assert_eq!(
                resume_request(message)
                    .unwrap()
                    .unwrap()
                    .training_name
                    .as_deref(),
                Some("harness")
            );
        }
        assert!(resume_request("Reprenons")
            .unwrap()
            .unwrap()
            .training_name
            .is_none());
        for message in [
            "Continue.",
            "Comment reprendre la formation ?",
            "Reprends cette explication",
            "Je veux un indice",
            "Reprends la formation Python puis corrige mon code",
            "Reprendre la formation X\nVoici ma réponse",
            "x = 4",
        ] {
            assert!(resume_request(message).unwrap().is_none(), "{message}");
        }
    }

    #[test]
    fn structured_resume_is_bounded_and_never_a_client_side_name_lookup() {
        let raw = "```tutor-resume-request\n{\"version\":1,\"trainingId\":\"training_A\",\"domainId\":\"domain_A\",\"trainingName\":\"Python\"}\n```";
        assert_eq!(
            resume_request(raw).unwrap().unwrap().training_id.as_deref(),
            Some("training_A")
        );
        for data in [
            r#"{"version":2}"#,
            r#"{"version":1,"trainingId":""}"#,
            r#"{"version":1,"domainId":"other"}"#,
            r#"{"version":1,"format":"quiz"}"#,
        ] {
            assert!(resume_request(&format!("```tutor-resume-request\n{data}\n```")).is_err());
        }
    }
}
