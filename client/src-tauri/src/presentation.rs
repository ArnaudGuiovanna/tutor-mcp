//! Small display metadata; never interpreted as a tool, permission or learning result.
use serde::Deserialize;

pub const TITLE_INSTRUCTION: &str = include_str!("../../protocol/conversation-title.txt");

pub fn clean_title(value: &str) -> Option<String> {
    let title = value.trim();
    if title.is_empty() || title.chars().count() > 100 || title.chars().any(char::is_control) {
        return None;
    }
    Some(title.to_owned())
}

pub fn response_title(text: &str) -> Option<String> {
    #[derive(Deserialize)]
    #[serde(deny_unknown_fields)]
    struct Title {
        title: String,
    }
    crate::learning_ui::payload::<Title>(text, "tutor-conversation-title")
        .ok()
        .flatten()
        .and_then(|value| clean_title(&value.title))
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn title_is_bounded_and_only_taken_from_a_complete_top_level_envelope() {
        assert_eq!(response_title("Bonjour.\n```tutor-conversation-title\n{\"title\":\"Mon profil d’apprenant\"}\n```"), Some("Mon profil d’apprenant".into()));
        assert!(response_title("```tutor-conversation-title\n{\"title\":\"Incomplet\"}").is_none());
        assert!(response_title(
            "````markdown\n```tutor-conversation-title\n{\"title\":\"Exemple\"}\n```\n````"
        )
        .is_none());
        assert!(response_title(
            "```tutor-conversation-title\n{\"title\":\"Titre\",\"tool\":\"call\"}\n```"
        )
        .is_none());
        assert!(clean_title("bad\nline").is_none());
        assert!(clean_title(&"é".repeat(101)).is_none());
    }
}
