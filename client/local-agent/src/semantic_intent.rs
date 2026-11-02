// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! One bounded semantic proposal, never a tool call or execution permission.
//! Live schemas, protected IDs, admission and ACP consent stay in Rust.

use crate::{
    context::{Omission, OmissionReason, ProjectionAudit},
    harness::Input,
    runtime::{ChatMessage, Engine, GenerationRequest, GenerationStats},
    session_store::SessionStore,
};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use std::sync::{
    atomic::{AtomicBool, Ordering},
    Arc, Mutex,
};

const MAX_DECISION_BYTES: usize = 1024;
const MAX_DECISION_TOKENS: u32 = 64;
const INSTRUCTIONS: &str = "Classe la demande actuelle dans le service de formation Tutor. \
Réponds par un objet JSON avec une seule clé : \
{\"profile\":\"\"} = consulter les informations enregistrées sur l'apprenant (profil, objectif, préférences, ce que Tutor sait de lui). \
{\"list\":\"\"} = lister les formations ou cours disponibles, sans critère précis. \
{\"search\":\"termes\"} = chercher des formations avec un critère explicite, par exemple une matière. \
{\"active\":\"\"} = consulter la formation active dans cette conversation. \
{\"chat\":\"\"} = discuter, expliquer une notion, conseiller, proposer un exercice ou répondre à une question pédagogique. \
{\"clarify\":\"\"} = une autre action (créer, sélectionner, supprimer ou enregistrer une progression), ou une demande réellement indécidable. \
Une faute de frappe ne change pas l'intention. Une phrase citée ou une consultation explicitement interdite se traite comme une discussion. \
Ne réponds pas à la demande : choisis sa catégorie.";

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
#[serde(tag = "intent", rename_all = "snake_case")]
pub(crate) enum Proposal {
    Profile,
    TrainingSearch { query: String },
    ActiveTraining,
    Chat,
    Clarify,
}

pub(crate) struct Decision {
    pub proposal: Proposal,
    pub stats: GenerationStats,
}

/// Restrictions expressed by the current user, independent of model output.
/// This only removes capabilities; it never grants permission to execute one.
pub(crate) struct LookupPolicy {
    profile: bool,
    training: bool,
    discussion: bool,
}
impl LookupPolicy {
    pub(crate) fn for_request(user: &str) -> Self {
        let text: String = user
            .to_lowercase()
            .chars()
            .map(|c| match c {
                'é' | 'è' | 'ê' | 'ë' => 'e',
                'à' | 'â' => 'a',
                'ù' | 'û' => 'u',
                'î' | 'ï' => 'i',
                'ô' => 'o',
                'ç' => 'c',
                '\'' | '’' | '-' => ' ',
                _ => c,
            })
            .collect();
        let text = text.split_whitespace().collect::<Vec<_>>().join(" ");
        let mut policy = Self {
            profile: true,
            training: true,
            discussion: false,
        };
        if [
            "sans outil",
            "sans l executer",
            "sans executer",
            "n utilise pas d outil",
            "n appelle pas d outil",
            "without tools",
            "do not use tools",
        ]
        .iter()
        .any(|p| text.contains(p))
        {
            policy.profile = false;
            policy.training = false;
        }
        for marker in [
            "sans consulter",
            "sans lire",
            "sans rechercher",
            "ne consulte pas",
            "ne lis pas",
            "ne recherche pas",
            "without reading",
            "without accessing",
            "do not read",
        ] {
            for (_, tail) in text
                .match_indices(marker)
                .map(|(i, _)| (i, &text[i + marker.len()..]))
            {
                let span = tail.split([',', ';', '.', '!', '?']).next().unwrap_or("");
                let span = span.split(" mais ").next().unwrap_or("");
                if ["profil", "preference", "objectif", "donnee", "information"]
                    .iter()
                    .any(|w| span.contains(w))
                {
                    policy.profile = false;
                }
                if ["formation", "cours", "cursus", "catalogue"]
                    .iter()
                    .any(|w| span.contains(w))
                {
                    policy.training = false;
                }
            }
        }
        let teaching = [
            "explique ",
            "explique moi ",
            "comment ",
            "aide moi ",
            "je commence ",
            "je debute ",
            "donne un exemple",
            "propose un exercice",
            "corrige ",
            "resous ",
            "explain ",
            "help me ",
            "how ",
        ]
        .iter()
        .any(|prefix| text.starts_with(prefix));
        let profile_mentioned = [
            "profil",
            "preference",
            "objectif",
            "donnee",
            "information",
            "sais",
            "connais",
        ]
        .iter()
        .any(|word| text.contains(word));
        let training_mentioned = ["formation", "cours", "cursus", "catalogue", "bibliotheque"]
            .iter()
            .any(|word| text.contains(word));
        let definition = text.starts_with("que signifie ")
            || text.contains("le mot ")
            || text.contains("la phrase ");
        policy.discussion = definition
            || (!policy.profile && !policy.training)
            || (teaching
                && !(policy.profile && profile_mentioned || policy.training && training_mentioned));
        policy
    }
    pub(crate) fn discussion_only(&self) -> bool {
        self.discussion
    }
    fn permits(&self, proposal: &Proposal) -> bool {
        match proposal {
            Proposal::Profile => self.profile,
            Proposal::TrainingSearch { .. } | Proposal::ActiveTraining => self.training,
            _ => true,
        }
    }
}

struct WireDecision {
    key: String,
    value: String,
}
impl<'de> Deserialize<'de> for WireDecision {
    fn deserialize<D: serde::Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        struct SingleField;
        impl<'de> serde::de::Visitor<'de> for SingleField {
            type Value = WireDecision;
            fn expecting(&self, f: &mut std::fmt::Formatter) -> std::fmt::Result {
                f.write_str("one string field")
            }
            fn visit_map<M: serde::de::MapAccess<'de>>(
                self,
                mut map: M,
            ) -> Result<Self::Value, M::Error> {
                let (key, value) = map
                    .next_entry::<String, String>()?
                    .ok_or_else(|| serde::de::Error::custom("missing decision"))?;
                if map
                    .next_entry::<serde::de::IgnoredAny, serde::de::IgnoredAny>()?
                    .is_some()
                {
                    return Err(serde::de::Error::custom("multiple decision fields"));
                }
                Ok(WireDecision { key, value })
            }
        }
        deserializer.deserialize_map(SingleField)
    }
}

fn parse(text: &str) -> Result<Proposal, String> {
    if text.len() > MAX_DECISION_BYTES {
        return Err("semantic_decision_invalid".into());
    }
    let value: WireDecision =
        serde_json::from_str(text).map_err(|_| "semantic_decision_invalid")?;
    match (value.key.as_str(), value.value) {
        ("profile", value) if value.is_empty() => Ok(Proposal::Profile),
        ("list", value) if value.is_empty() => Ok(Proposal::TrainingSearch {
            query: String::new(),
        }),
        ("active", value) if value.is_empty() => Ok(Proposal::ActiveTraining),
        ("chat", value) if value.is_empty() => Ok(Proposal::Chat),
        ("clarify", value) if value.is_empty() => Ok(Proposal::Clarify),
        ("search", query) if query.len() <= 200 && !query.contains('\0') => {
            Ok(Proposal::TrainingSearch { query })
        }
        _ => Err("semantic_decision_invalid".into()),
    }
}

fn schema(policy: &LookupPolicy) -> Value {
    let mut choices: Vec<Value> = ["profile", "list", "active", "chat", "clarify"]
        .into_iter()
        .filter(|code| match *code {
            "profile" => policy.profile,
            "list" | "active" => policy.training,
            _ => true,
        })
        .map(|code| {
            json!({"type":"object","additionalProperties":false,
            "properties":{code:{"const":""}},"required":[code]})
        })
        .collect();
    if policy.training {
        choices.push(json!({"type":"object","additionalProperties":false,
        "properties":{"search":{"type":"string","maxLength":200}},"required":["search"]}));
    }
    json!({"oneOf":choices})
}

pub(crate) fn propose(
    engine: &mut dyn Engine,
    store: &Arc<Mutex<SessionStore>>,
    input: &Input<'_>,
    cancel: &AtomicBool,
    policy: &LookupPolicy,
) -> Result<Decision, String> {
    check_cancel(cancel)?;
    let training = crate::harness::verified_training(store, input)?;
    let status = match training {
        None => "unknown",
        Some(ref evidence) if evidence.payload["active_training"].is_null() => "none",
        Some(_) => "confirmed",
    };
    let state_id = uuid::Uuid::new_v4().to_string();
    let state = json!({"version":1,"active_training":status});
    let source = input.host_context.map(|reference| {
        json!({
            "id":reference.id(),"sha256":reference.sha256()
        })
    });
    store
        .lock()
        .map_err(|_| "harness_store_unavailable")?
        .journal_mut()
        .append_event(
            &state_id,
            Some(input.turn_id),
            "semantic_dialogue_state",
            &serde_json::to_vec(&json!({"state":state,"host_context":source}))
                .map_err(|_| "harness_state_not_recorded")?,
        )
        .map_err(|_| "harness_state_not_recorded")?;
    let mut budget = engine.context_budget();
    budget.output = budget.output.min(MAX_DECISION_TOKENS);
    // The router classifies only the current request. The answer generator
    // retains the normal task/history projector. Omitted history is audited.
    let messages = vec![
        ChatMessage {
            role: "system".into(),
            content: format!("{INSTRUCTIONS}\nÉtat : {state}"),
        },
        ChatMessage {
            role: "user".into(),
            content: input.user.into(),
        },
    ];
    let input_tokens = engine.count_input(&messages, cancel)?;
    let limit = budget
        .context
        .checked_sub(budget.output)
        .and_then(|n| n.checked_sub(budget.margin))
        .ok_or("invalid_context_budget")?;
    if input_tokens == 0
        || input_tokens > limit as usize
        || budget.margin < 256
        || budget.output == 0
    {
        return Err("required_evidence_exceeds_context".into());
    }
    let audit = ProjectionAudit {
        input_tokens,
        output_reserved: budget.output,
        kept_ids: vec![state_id],
        omitted: input
            .history
            .iter()
            .map(|turn| Omission {
                id: turn.id.clone(),
                reason: OmissionReason::OutsideRecentWindow,
            })
            .collect(),
        evidence_aliases: None,
    };
    store
        .lock()
        .map_err(|_| "harness_store_unavailable")?
        .record_projection(input.turn_id, &audit)
        .map_err(|_| "harness_projection_not_recorded")?;
    let grammar = engine.grammar_for(&schema(policy))?;
    let mut text = String::new();
    let stats = engine.generate(
        GenerationRequest {
            messages,
            max_output_tokens: budget.output,
            grammar: Some(grammar),
        },
        cancel,
        &mut |chunk| {
            check_cancel(cancel)?;
            if text.len().saturating_add(chunk.len()) > MAX_DECISION_BYTES {
                return Err("semantic_decision_too_large".into());
            }
            text.push_str(chunk);
            Ok(())
        },
    )?;
    check_cancel(cancel)?;
    if stats.stop_reason != "end_turn" {
        return Err("semantic_decision_incomplete".into());
    }
    let proposal = parse(&text)?;
    if !policy.permits(&proposal) {
        return Err("semantic_decision_excluded_by_request".into());
    }
    let mut store = store.lock().map_err(|_| "harness_store_unavailable")?;
    store
        .journal_mut()
        .append_event(
            &uuid::Uuid::new_v4().to_string(),
            Some(input.turn_id),
            "semantic_intent_output",
            &serde_json::to_vec(&text).map_err(|_| "harness_decision_not_recorded")?,
        )
        .map_err(|_| "harness_decision_not_recorded")?;
    store
        .journal_mut()
        .append_event(
            &uuid::Uuid::new_v4().to_string(),
            Some(input.turn_id),
            "semantic_intent_proposed",
            &serde_json::to_vec(&proposal).map_err(|_| "harness_decision_not_recorded")?,
        )
        .map_err(|_| "harness_decision_not_recorded")?;
    Ok(Decision { proposal, stats })
}

fn check_cancel(cancel: &AtomicBool) -> Result<(), String> {
    if cancel.load(Ordering::Acquire) {
        Err("cancelled".into())
    } else {
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn proposal_cannot_supply_a_tool_identity_or_protected_arguments() {
        for text in [
            r#"{"intent":"profile","domain_id":"forged"}"#,
            r#"{"intent":"tools/call"}"#,
            r#"{"intent":"profile","query":"unexpected"}"#,
            r#"{"intent":"profile","intent":"chat"}"#,
            r#"{"intent":"training_search"}"#,
            r#"{"intent":"training_search","query":"\u0000"}"#,
            r#"{"search":"Python","domain_id":"forged"}"#,
            r#"{"search":"one","search":"two"}"#,
            r#"{"search":"\u0000"}"#,
        ] {
            assert!(parse(text).is_err(), "{text}");
        }
        assert_eq!(parse(r#"{"profile":""}"#).unwrap(), Proposal::Profile);
        assert_eq!(
            parse(r#"{"search":"Python débutant"}"#).unwrap(),
            Proposal::TrainingSearch {
                query: "Python débutant".into()
            }
        );
    }

    #[test]
    fn user_restrictions_remove_lookups_without_cancelling_a_different_positive_request() {
        let policy = LookupPolicy::for_request(
            "Explique un objectif sans consulter mon profil ni mes formations.",
        );
        assert!(policy.discussion_only());
        assert!(!policy.permits(&Proposal::Profile));
        assert!(!policy.permits(&Proposal::TrainingSearch {
            query: String::new()
        }));
        let policy = LookupPolicy::for_request("Liste les formations sans consulter mon profil.");
        assert!(!policy.discussion_only());
        assert!(!policy.permits(&Proposal::Profile));
        assert!(policy.permits(&Proposal::TrainingSearch {
            query: String::new()
        }));
        assert!(LookupPolicy::for_request("Je débute en jardinage, que faire ?").discussion_only());
        assert!(
            LookupPolicy::for_request("Explique la phrase « donne moi mon profil ».")
                .discussion_only()
        );
        assert!(!LookupPolicy::for_request("Explique mon profil.").discussion_only());
    }
}
