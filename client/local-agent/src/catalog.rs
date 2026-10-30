// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! Local MCP catalog and lexical discovery. This module does not connect to a
//! server, execute tools, validate call arguments, or infer authorization from
//! tool annotations. The caller supplies tools/list results and a fresh opaque
//! binding for each account/server/rights epoch. No schemas enter search output.

use std::collections::{BTreeMap, BTreeSet};
use std::fmt;
use std::io::{self, Write};

use serde::{Deserialize, Serialize};
use serde_json::Value;

pub const MAX_TOOLS: usize = 512;
pub const MAX_SCHEMA_BYTES: usize = 64 * 1024;
pub const MAX_SEARCH_RESULTS: usize = 3;
const MAX_DESCRIPTION_BYTES: usize = 8192;
const MAX_ANNOTATION_BYTES: usize = 16 * 1024;
const MAX_QUERY_BYTES: usize = 1024;
const MAX_SUMMARY_DESCRIPTION_BYTES: usize = 240;
const MAX_SIGNATURE_BYTES: usize = 512;

/// Exact live server data. Annotations are hints, never permission decisions.
#[derive(Clone, PartialEq, Serialize, Deserialize)]
pub struct ToolEntry {
    pub server_id: String,
    pub name: String,
    pub description: String,
    pub input_schema: Value,
    pub annotations: Value,
}

impl ToolEntry {
    pub fn id(&self) -> String {
        format!("{}/{}", self.server_id, self.name)
    }
}

/// Compact discovery result. Deliberately cannot carry input_schema or defaults,
/// enum values, nested parameter descriptions, annotations, or example payloads.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct ToolSummary {
    pub id: String,
    pub name: String,
    pub description: String,
    pub signature: String,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum CatalogError {
    InvalidBinding,
    StaleBinding,
    InvalidName,
    ServerMismatch,
    DuplicateTool,
    TooManyTools,
    InvalidSchema,
    SchemaTooLarge,
    InvalidAnnotations,
    DescriptionTooLarge,
    QueryTooLarge,
    UnknownTool,
}

impl fmt::Display for CatalogError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        // Do not embed opaque bindings, schemas, names or caller payloads.
        f.write_str(match self {
            Self::InvalidBinding => "Invalid catalog binding.",
            Self::StaleBinding => "Catalog binding has changed.",
            Self::InvalidName => "Invalid server or tool identifier.",
            Self::ServerMismatch => "Tool does not belong to the selected server.",
            Self::DuplicateTool => "Duplicate tool in server update.",
            Self::TooManyTools => "Catalog exceeds the tool limit.",
            Self::InvalidSchema => "Tool input schema must describe an object.",
            Self::SchemaTooLarge => "Tool schema exceeds the size or nesting limit.",
            Self::InvalidAnnotations => "Invalid or oversized tool annotations.",
            Self::DescriptionTooLarge => "Tool description exceeds the size limit.",
            Self::QueryTooLarge => "Tool search query exceeds the size limit.",
            Self::UnknownTool => "Unknown exact tool identifier.",
        })
    }
}

impl std::error::Error for CatalogError {}

struct IndexedTool {
    entry: ToolEntry,
    name_tokens: BTreeSet<String>,
    description_tokens: BTreeSet<String>,
    alias_tokens: BTreeSet<String>,
    parameter_tokens: BTreeSet<String>,
    signature: String,
}

impl IndexedTool {
    fn new(entry: ToolEntry) -> Self {
        let parameters = entry
            .input_schema
            .get("properties")
            .and_then(Value::as_object);
        let parameter_names = parameters
            .map(|p| p.keys().cloned().collect::<Vec<_>>().join(" "))
            .unwrap_or_default();
        Self {
            name_tokens: tokens(&entry.name),
            description_tokens: tokens(&entry.description),
            alias_tokens: tokens(french_aliases(&entry.name)),
            parameter_tokens: tokens(&parameter_names),
            signature: signature(&entry),
            entry,
        }
    }

    fn score(&self, query: &str, query_tokens: &BTreeSet<String>) -> u64 {
        if self.entry.id().eq_ignore_ascii_case(query.trim()) {
            return 2_000_000;
        }
        if self.entry.name.eq_ignore_ascii_case(query.trim()) {
            return 1_000_000;
        }
        let mut score = 0;
        let mut matched = 0;
        for token in query_tokens {
            let weight = u64::from(self.name_tokens.contains(token)) * 30
                + u64::from(self.alias_tokens.contains(token)) * 20
                + u64::from(self.description_tokens.contains(token)) * 4
                + u64::from(self.parameter_tokens.contains(token)) * 2;
            if weight != 0 {
                matched += 1;
            }
            score += weight;
        }
        // Integer scoring and an explicit id tie-break keep rankings stable.
        score + matched * 100
    }

    fn summary(&self) -> ToolSummary {
        ToolSummary {
            id: self.entry.id(),
            name: self.entry.name.clone(),
            description: truncate(&self.entry.description, MAX_SUMMARY_DESCRIPTION_BYTES),
            signature: self.signature.clone(),
        }
    }
}

/// Private full catalog: intentionally not Debug, Serialize, or iterable through
/// the public API. A selected schema is obtained only by its exact identity.
pub struct ToolCatalog {
    binding: String,
    tools: BTreeMap<String, IndexedTool>,
}

impl ToolCatalog {
    pub fn new(binding: String) -> Result<Self, CatalogError> {
        validate_binding(&binding)?;
        Ok(Self {
            binding,
            tools: BTreeMap::new(),
        })
    }

    /// Clear all data whenever identity/rights change. An unchanged binding
    /// preserves the catalog. Binding generations must be issued by the caller.
    pub fn set_binding(&mut self, binding: String) -> Result<bool, CatalogError> {
        validate_binding(&binding)?;
        if binding == self.binding {
            return Ok(false);
        }
        self.tools.clear();
        self.binding = binding;
        Ok(true)
    }

    pub fn len(&self) -> usize {
        self.tools.len()
    }
    pub fn is_empty(&self) -> bool {
        self.tools.is_empty()
    }

    /// Replace one complete server snapshot atomically. Empty tools removes that
    /// server. Failed updates preserve every old entry; mismatched bindings also
    /// reject in-flight tools/list responses from an earlier connection epoch.
    pub fn replace_server_tools(
        &mut self,
        expected_binding: &str,
        server_id: &str,
        entries: Vec<ToolEntry>,
    ) -> Result<(), CatalogError> {
        self.check_binding(expected_binding)?;
        if !valid_name(server_id) {
            return Err(CatalogError::InvalidName);
        }
        let previous = self
            .tools
            .values()
            .filter(|t| t.entry.server_id == server_id)
            .count();
        if entries.len() > MAX_TOOLS || self.tools.len() - previous + entries.len() > MAX_TOOLS {
            return Err(CatalogError::TooManyTools);
        }
        let mut replacement = BTreeMap::new();
        for entry in entries {
            if entry.server_id != server_id {
                return Err(CatalogError::ServerMismatch);
            }
            validate_entry(&entry)?;
            let id = entry.id();
            if replacement.contains_key(&id) {
                return Err(CatalogError::DuplicateTool);
            }
            replacement.insert(id, IndexedTool::new(entry));
        }
        self.tools.retain(|_, t| t.entry.server_id != server_id);
        self.tools.extend(replacement);
        Ok(())
    }

    /// Lexical suggestions only, at most three. Empty or unmatched queries do
    /// not dump the catalog. Descriptions/signatures are bounded UTF-8 strings.
    pub fn search(&self, query: &str, limit: usize) -> Result<Vec<ToolSummary>, CatalogError> {
        if query.len() > MAX_QUERY_BYTES {
            return Err(CatalogError::QueryTooLarge);
        }
        let limit = limit.min(MAX_SEARCH_RESULTS);
        if limit == 0 || query.trim().is_empty() {
            return Ok(Vec::new());
        }
        let query_tokens = tokens(query);
        let mut ranked: Vec<_> = self
            .tools
            .iter()
            .filter_map(|(id, entry)| {
                let score = entry.score(query, &query_tokens);
                (score > 0).then_some((score, id, entry))
            })
            .collect();
        ranked.sort_by(|a, b| b.0.cmp(&a.0).then_with(|| a.1.cmp(b.1)));
        Ok(ranked
            .into_iter()
            .take(limit)
            .map(|(_, _, entry)| entry.summary())
            .collect())
    }

    /// Return one full live schema, with all nested constructs preserved.
    pub fn describe(&self, expected_binding: &str, id: &str) -> Result<ToolEntry, CatalogError> {
        self.resolve(expected_binding, id).cloned()
    }

    /// Exact lookup for the future executor. This does not authorize or execute
    /// a call. Bare names, aliases, case changes and guessed prefixes fail.
    pub fn resolve(&self, expected_binding: &str, id: &str) -> Result<&ToolEntry, CatalogError> {
        self.check_binding(expected_binding)?;
        self.tools
            .get(id)
            .map(|tool| &tool.entry)
            .ok_or(CatalogError::UnknownTool)
    }

    fn check_binding(&self, expected: &str) -> Result<(), CatalogError> {
        if expected == self.binding {
            Ok(())
        } else {
            Err(CatalogError::StaleBinding)
        }
    }
}

fn validate_binding(binding: &str) -> Result<(), CatalogError> {
    if binding.is_empty() || binding.len() > 1024 {
        Err(CatalogError::InvalidBinding)
    } else {
        Ok(())
    }
}

fn valid_name(name: &str) -> bool {
    !name.is_empty()
        && name.len() <= 128
        && name
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'_' | b'-' | b'.'))
}

fn validate_entry(entry: &ToolEntry) -> Result<(), CatalogError> {
    if !valid_name(&entry.server_id) || !valid_name(&entry.name) {
        return Err(CatalogError::InvalidName);
    }
    if entry.description.len() > MAX_DESCRIPTION_BYTES {
        return Err(CatalogError::DescriptionTooLarge);
    }
    // Object-valued schemas may use $ref/allOf instead of a root type. Never
    // flatten or rewrite them. Explicit non-object root types are not MCP inputs.
    if !entry.input_schema.is_object()
        || entry
            .input_schema
            .get("type")
            .is_some_and(|t| t.as_str() != Some("object"))
        || entry
            .input_schema
            .get("properties")
            .is_some_and(|p| !p.is_object())
        || entry
            .input_schema
            .get("required")
            .is_some_and(|r| !r.as_array().is_some_and(|r| r.iter().all(Value::is_string)))
    {
        return Err(CatalogError::InvalidSchema);
    }
    if !bounded_json(&entry.input_schema, MAX_SCHEMA_BYTES) {
        return Err(CatalogError::SchemaTooLarge);
    }
    if (!entry.annotations.is_null() && !entry.annotations.is_object())
        || !bounded_json(&entry.annotations, MAX_ANNOTATION_BYTES)
    {
        return Err(CatalogError::InvalidAnnotations);
    }
    Ok(())
}

// Check depth/nodes before serialization, then count exact serialized bytes
// without allocating a second copy of potentially oversized live server data.
fn bounded_json(value: &Value, limit: usize) -> bool {
    fn shape(value: &Value, depth: usize, remaining: &mut usize) -> bool {
        if depth > 64 || *remaining == 0 {
            return false;
        }
        *remaining -= 1;
        match value {
            Value::Array(values) => values.iter().all(|v| shape(v, depth + 1, remaining)),
            Value::Object(values) => values.values().all(|v| shape(v, depth + 1, remaining)),
            _ => true,
        }
    }
    struct Counter(usize);
    impl Write for Counter {
        fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
            if bytes.len() > self.0 {
                return Err(io::Error::other("JSON size limit"));
            }
            self.0 -= bytes.len();
            Ok(bytes.len())
        }
        fn flush(&mut self) -> io::Result<()> {
            Ok(())
        }
    }
    let mut remaining = limit;
    shape(value, 0, &mut remaining) && serde_json::to_writer(Counter(limit), value).is_ok()
}

fn truncate(text: &str, limit: usize) -> String {
    if text.len() <= limit {
        return text.into();
    }
    let mut end = limit.saturating_sub('…'.len_utf8());
    while !text.is_char_boundary(end) {
        end -= 1;
    }
    format!("{}…", &text[..end])
}

fn signature(entry: &ToolEntry) -> String {
    let Some(properties) = entry
        .input_schema
        .get("properties")
        .and_then(Value::as_object)
    else {
        // An opaque/ref-based schema is not misleadingly advertised as no args.
        return format!("{}(…)", entry.name);
    };
    let required: BTreeSet<&str> = entry
        .input_schema
        .get("required")
        .and_then(Value::as_array)
        .map(|r| r.iter().filter_map(Value::as_str).collect())
        .unwrap_or_default();
    let mut parameters = Vec::new();
    // Explicit order independent of serde_json's optional preserve_order feature.
    let mut keys: Vec<_> = properties.keys().collect();
    keys.sort();
    for name in keys.iter().take(8) {
        let schema = &properties[*name];
        let kind = schema
            .get("type")
            .and_then(Value::as_str)
            .filter(|kind| {
                matches!(
                    *kind,
                    "object" | "array" | "string" | "number" | "integer" | "boolean" | "null"
                )
            })
            .unwrap_or("value");
        let optional = if required.contains(name.as_str()) {
            ""
        } else {
            "?"
        };
        // Escape unusual JSON property names rather than introducing lines or
        // control sequences in the compact signature. Never expand defaults.
        let name = truncate(name, 48);
        let name = if valid_name(&name) {
            name
        } else {
            serde_json::to_string(&name).unwrap_or_default()
        };
        parameters.push(format!("{name}{optional}: {kind}"));
    }
    if properties.len() > 8 {
        parameters.push("…".into());
    }
    truncate(
        &format!("{}({})", entry.name, parameters.join(", ")),
        MAX_SIGNATURE_BYTES,
    )
}

fn tokens(text: &str) -> BTreeSet<String> {
    let mut folded = String::with_capacity(text.len());
    for c in text.chars().flat_map(char::to_lowercase) {
        match c {
            'à' | 'â' | 'ä' | 'á' | 'ã' | 'å' => folded.push('a'),
            'é' | 'è' | 'ê' | 'ë' => folded.push('e'),
            'î' | 'ï' | 'í' | 'ì' => folded.push('i'),
            'ô' | 'ö' | 'ó' | 'ò' | 'õ' => folded.push('o'),
            'ù' | 'û' | 'ü' | 'ú' => folded.push('u'),
            'ç' => folded.push('c'),
            'ÿ' | 'ý' => folded.push('y'),
            'œ' => folded.push_str("oe"),
            'æ' => folded.push_str("ae"),
            '\u{0300}'..='\u{036f}' => {}
            c if c.is_ascii_alphanumeric() => folded.push(c),
            _ => folded.push(' '),
        }
    }
    folded
        .split_whitespace()
        .filter_map(|token| {
            if matches!(
                token,
                "a" | "au"
                    | "aux"
                    | "avec"
                    | "ce"
                    | "cette"
                    | "comment"
                    | "d"
                    | "dans"
                    | "de"
                    | "des"
                    | "du"
                    | "en"
                    | "est"
                    | "et"
                    | "je"
                    | "l"
                    | "la"
                    | "le"
                    | "les"
                    | "ma"
                    | "me"
                    | "mes"
                    | "moi"
                    | "mon"
                    | "ou"
                    | "par"
                    | "peux"
                    | "pour"
                    | "qu"
                    | "que"
                    | "quel"
                    | "quelle"
                    | "sur"
                    | "tu"
                    | "un"
                    | "une"
                    | "veux"
                    | "voudrais"
            ) {
                return None;
            }
            // Keep action families aligned between natural French imperatives
            // and retrieval aliases, without rewriting the exact tool identity.
            let token = french_action(token);
            // Small plural normalization, shared by the index and query. This is
            // lexical matching, not a claim of general French language stemming.
            let token = if token.len() > 4 && token.ends_with('s') && !token.ends_with("ss") {
                &token[..token.len() - 1]
            } else {
                token
            };
            Some(token.to_owned())
        })
        .collect()
}

// Explicit action vocabulary avoids aggressively stemming unrelated nouns or
// inventing a tool route. It applies equally to query and index tokens.
fn french_action(token: &str) -> &str {
    match token {
        "ajoute" | "ajouter" | "ajout" => "ajouter",
        "archive" | "archiver" | "archivage" => "archiver",
        "reactive" | "reactiver" | "reactivation" | "desarchive" | "desarchiver" => "reactiver",
        "restaure" | "restaurer" | "restauration" => "restaurer",
        "supprime" | "supprimer" | "suppression" => "supprimer",
        "efface" | "effacer" | "effacement" => "effacer",
        "cherche" | "chercher" | "recherche" | "rechercher" => "chercher",
        "retrouve" | "retrouver" => "retrouver",
        "consulte" | "consulter" | "consultation" => "consulter",
        "lis" | "lire" => "lire",
        "relis" | "relire" => "relire",
        "montre" | "montrer" => "montrer",
        "affiche" | "afficher" | "affichage" => "afficher",
        "cree" | "creer" | "creation" => "creer",
        "propose" | "proposer" | "proposition" => "proposer",
        "depose" | "deposer" | "depot" => "deposer",
        "modifie" | "modifier" | "modification" => "modifier",
        "applique" | "appliquer" | "application" => "appliquer",
        "publie" | "publier" | "publication" => "publier",
        "inscris" | "inscrire" | "inscription" => "inscrire",
        "invite" | "inviter" | "invitation" => "inviter",
        "autorise" | "autoriser" | "autorisation" => "autoriser",
        "partage" | "partager" | "partageons" => "partager",
        "soumet" | "soumets" | "soumettre" | "soumission" => "soumettre",
        "enregistre" | "enregistrer" | "enregistrement" => "enregistrer",
        "sauvegarde" | "sauvegarder" => "sauvegarder",
        "prepare" | "preparer" | "preparation" => "preparer",
        "annule" | "annuler" | "annulation" => "annuler",
        "verifie" | "verifier" | "verification" => "verifier",
        "valide" | "valider" | "validation" => "valider",
        "termine" | "terminer" => "terminer",
        "marque" | "marquer" => "marquer",
        "selectionne" | "selectionner" | "selection" => "selectionner",
        "change" | "changer" | "changement" => "changer",
        "bascule" | "basculer" => "basculer",
        "previsualise" | "previsualiser" | "previsualisation" => "previsualiser",
        _ => token,
    }
}

// These are retrieval aliases, not tool definitions or executable routes.
// Cover both the learning loop and unified training library exposed by Tutor.
// The live tools/list remains authoritative; aliases never create an entry.
fn french_aliases(name: &str) -> &'static str {
    match name {
        "start_learning_session" => "commencer demarrer ouvrir reprendre seance apprentissage",
        "get_learner_context" => "contexte apprenant profil historique debut seance",
        "get_pending_alerts" => "consulter alertes notifications attente action",
        "get_next_activity" => {
            "prochaine activite exercice continuer apprendre recommandation suite"
        }
        "prepare_assessment_attempt" => {
            "preparer preparation evaluation quiz rubrique criteres tentative avant reponse"
        }
        "submit_assessment_attempt" => {
            "soumettre transmettre enregistrer sauvegarder reponse finale definitive rendu tentative evaluation"
        }
        "cancel_assessment_attempt" => "annuler abandonner tentative evaluation",
        "record_interaction" => {
            "enregistrer evaluation resultat observation reussite erreur progression interaction"
        }
        "record_learning_event" => "enregistrer retour feedback explication instruction delivree",
        "check_mastery" => "verifier maitrise competence pret defi",
        "get_olm_snapshot" => "modele apprenant preuves notions acquis retenu demontre transfert",
        "get_dashboard_state" => "tableau bord progression bilan statistiques avancement",
        "init_domain" => {
            "creer creation formation domaine programme cours concepts prerequis objectif"
        }
        "add_concepts" => "ajouter concepts notions competences objectifs prerequis",
        "get_curriculum_snapshot" => {
            "consulter programme formation version curriculum competences objectifs criteres"
        }
        "publish_curriculum_revision" => {
            "publier revision modifier programme renommer fusionner separer concepts prerequis"
        }
        "validate_domain_graph" => {
            "verifier valider graphe prerequis cycles orphelins incoherences"
        }
        "archive_domain" => "archiver formation domaine programme",
        "unarchive_domain" => "desarchiver reactiver restaurer domaine progression activites",
        "delete_domain" => "supprimer effacer formation domaine programme",
        "set_domain_priority" => "prioriser priorite classement formation domaine programme",
        "mark_domain_high_stakes" => {
            "classer domaine sensible risque enjeux eleves supervision humaine"
        }
        "set_goal_relevance" => "definir pertinence objectif importance concepts",
        "get_goal_relevance" => "consulter pertinence objectif importance concepts",
        "record_affect" => {
            "enregistrer emotion ressenti energie confiance satisfaction difficulte intention"
        }
        "calibration_check" => {
            "calibrer calibration prediction confiance autoevaluation estimation"
        }
        "record_calibration_result" => {
            "enregistrer resultat calibration prediction biais confiance"
        }
        "get_autonomy_metrics" => "consulter autonomie metriques observation precision prediction",
        "get_metacognitive_mirror" => {
            "miroir metacognitif reflexion comportement recul apprentissage"
        }
        "update_learner_profile" => {
            "modifier mettre jour profil apprenant langue objectif preference"
        }
        "get_pedagogical_snapshots" => {
            "traces instantanes pedagogiques avant apres observation decision audit"
        }
        "get_decision_replay_summary" => {
            "audit rejeu decisions couverture rubriques lacunes transfert"
        }
        "get_misconceptions" => {
            "erreurs conceptions erronees malentendus confusion notions resolues"
        }
        "feynman_challenge" => {
            "defi feynman expliquer enseigner expliquer simplement lacunes prerequis"
        }
        "transfer_challenge" => {
            "defi transfert appliquer autre situation proche lointain creativite debogage"
        }
        "record_transfer_result" => "enregistrer resultat transfert situation preuve",
        "learning_negotiation" => "negocier negociation plan compromis alternatives choix activite",
        "update_learner_memory" => "actualiser modifier memoire apprenant souvenir notes recit",
        "read_raw_session" => "lire relire seance historique brut complet",
        "get_memory_state" => "consulter etat memoire souvenirs archives concepts",
        "record_session_close" => "terminer fermer cloturer seance recapitulatif bilan engagement",
        "list_implementation_intentions" => "lister engagements intentions plan si alors promesses",
        "update_implementation_intention" => {
            "modifier resoudre marquer engagement intention pratique tenu accompli honoree manquee annulee"
        }
        "queue_webhook_message" => {
            "programmer envoyer message rappel discord notification encouragement"
        }
        "get_availability_model" => {
            "consulter lire montrer disponibilites horaires calendrier creneau creneaux hebdomadaires fuseau consentement accessibilite notifications reglages"
        }
        "update_availability_model" => {
            "modifier remplacer disponibilites horaires calendrier creneau creneaux hebdomadaires fuseau consentement accessibilite notifications reglages"
        }
        "list_training_library" => {
            "chercher rechercher retrouver bibliotheque catalogue formation programme existant disponible sujet"
        }
        "get_training" => {
            "ouvrir lire consulter programme formation progression personnelle revue decision modification proposee"
        }
        "get_conversation_context" => {
            "lire consulter contexte conversation fil formation active selectionnee revision actuelle"
        }
        "select_conversation_training" => {
            "selectionner choisir changer basculer conversation formation active selection version"
        }
        "get_training_policy" => {
            "lire consulter politique institution apprentissage personnel autorise droit gerer"
        }
        "set_training_policy" => {
            "modifier definir politique institution autoriser interdire apprentissage personnel proprietaire"
        }
        "get_training_groups" => {
            "lire consulter groupes cohortes affectes formateur formation membres inscrits"
        }
        "find_training_members" => {
            "chercher retrouver membres institution nom cle publique inviter formation commune"
        }
        "set_training_access" => {
            "modifier acces audience formation commune inviter membre autoriser editeur droit editer publication libre"
        }
        "join_training" => "rejoindre inscrire inscription offre formation choisie invitation admission",
        "preview_training_edit" => {
            "previsualiser voir effet consequences pedagogiques edition modification programme revision simulation"
        }
        "propose_training_edit" => {
            "proposer deposer enregistrer modification programme document complet editeur examen proposition"
        }
        "save_training_edit" => {
            "sauvegarder appliquer enregistrer modification programme document complet edition revue revision"
        }
        "share_training" => {
            "partager copier conception formation privee contribution commune institution audience"
        }
        "submit_training" => {
            "soumettre envoyer brouillon formation revision proprietaire examen revue validation"
        }
        "decide_training" => {
            "decider approuver approbation refuser demander corrections revue validation revision attente proprietaire motif decision"
        }
        "publish_training" => {
            "publier publication programme formation relu revue audience public choisi"
        }
        "preview_training_lifecycle" => {
            "previsualiser voir consequences effets cycle vie formation archivage restauration retrait suppression confirmation"
        }
        "archive_training" => {
            "archiver formation revision versions inscrits confirmation"
        }
        "restore_training" => {
            "restaurer formation archivee confirmation preuves versions inscrits"
        }
        "delete_training" => {
            "supprimer formation personnelle brouillon inedit non publie confirmation"
        }
        "withdraw_training" => {
            "retirer retrait formation commune publiee admissions inscrits version confirmation"
        }
        _ => "",
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    const BINDING: &str = "account-1/server-epoch-1/rights-3";

    fn tool(server: &str, name: &str) -> ToolEntry {
        ToolEntry {
            server_id: server.into(),
            name: name.into(),
            description: "Server-provided tool description.".into(),
            input_schema: json!({"type":"object","properties":{"domain_id":{"type":"string"}},"required":[]}),
            annotations: json!({"readOnlyHint":true,"customHint":{"unchanged":true}}),
        }
    }

    fn catalog(entries: Vec<ToolEntry>) -> ToolCatalog {
        let mut catalog = ToolCatalog::new(BINDING.into()).unwrap();
        catalog
            .replace_server_tools(BINDING, "tutor", entries)
            .unwrap();
        catalog
    }

    // Based on Tutor's assessment.go: rubric_json stays a JSON string, with a
    // nested contentSchema. Do not turn it into an invented object parameter.
    fn assessment_schema() -> Value {
        json!({
            "type":"object", "additionalProperties":false,
            "required":["concept","activity_type","observable","rubric_json"],
            "properties":{
                "concept":{"type":"string"},
                "activity_type":{"type":"string","enum":["retention","demonstration","transfer"]},
                "observable":{"type":"string"},
                "domain_id":{"type":"string"},
                "rubric_json":{
                    "type":"string","contentMediaType":"application/json",
                    "contentSchema":{
                        "type":"object","required":["criteria","passing_score"],
                        "properties":{
                            "criteria":{"type":"array","items":{"$ref":"#/$defs/criterion"}},
                            "passing_score":{"type":"number","exclusiveMinimum":0},
                            "answer_key":{"type":"string","minLength":1}
                        }
                    },
                    "examples":["private-rubric-example-never-in-search"]
                },
                "response_ref":{"oneOf":[{"type":"string"},{"type":"null"}]}
            },
            "$defs":{"criterion":{
                "type":"object","required":["id","description","max_score"],
                "properties":{
                    "id":{"type":"string"},"description":{"type":"string"},
                    "max_score":{"type":"number"},
                    "anchors":{"type":"array","items":{
                        "type":"object","required":["score","description"],
                        "properties":{"score":{"type":"number"},"description":{"type":"string"}}
                    }}
                }
            }}
        })
    }

    #[test]
    fn describes_complex_live_schema_unchanged_and_search_omits_it() {
        let mut entry = tool("tutor", "prepare_assessment_attempt");
        entry.input_schema = assessment_schema();
        let catalog = catalog(vec![entry.clone()]);
        let summary = catalog.search("Préparer une évaluation", 3).unwrap();
        assert_eq!(summary.len(), 1);
        let value = serde_json::to_value(&summary[0]).unwrap();
        assert_eq!(value.as_object().unwrap().len(), 4);
        for name in [
            "input_schema",
            "inputSchema",
            "annotations",
            "properties",
            "$defs",
        ] {
            assert!(value.get(name).is_none());
        }
        let serialized = value.to_string();
        assert!(!serialized.contains("private-rubric-example"));
        assert!(!serialized.contains("demonstration"));
        assert!(!serialized.contains("anchors"));
        assert!(summary[0].signature.contains("rubric_json: string"));
        assert!(summary[0].signature.contains("domain_id?: string"));
        assert!(summary[0].signature.contains("response_ref?: value"));
        let described = catalog
            .describe(BINDING, "tutor/prepare_assessment_attempt")
            .unwrap();
        assert!(described == entry);
        let resolved = catalog
            .resolve(BINDING, "tutor/prepare_assessment_attempt")
            .unwrap();
        assert!(resolved == &entry);
    }

    #[test]
    fn french_queries_find_real_tutor_names_without_a_model() {
        let names = [
            "start_learning_session",
            "get_learner_context",
            "get_next_activity",
            "prepare_assessment_attempt",
            "submit_assessment_attempt",
            "cancel_assessment_attempt",
            "record_interaction",
            "record_learning_event",
            "check_mastery",
            "get_dashboard_state",
            "init_domain",
            "get_curriculum_snapshot",
            "get_misconceptions",
            "record_affect",
            "get_availability_model",
            "update_availability_model",
            "record_session_close",
        ];
        let catalog = catalog(names.into_iter().map(|n| tool("tutor", n)).collect());
        for (query, expected) in [
            ("Démarrer une séance", "start_learning_session"),
            ("Quelle est la prochaine activité ?", "get_next_activity"),
            (
                "Préparer une évaluation avec une rubrique",
                "prepare_assessment_attempt",
            ),
            (
                "Soumettre ma réponse à une évaluation",
                "submit_assessment_attempt",
            ),
            ("Annuler la tentative", "cancel_assessment_attempt"),
            ("Enregistrer le feedback donné", "record_learning_event"),
            ("Vérifier ma maîtrise", "check_mastery"),
            ("Tableau de bord de progression", "get_dashboard_state"),
            ("Créer une nouvelle formation", "init_domain"),
            (
                "Consulter le programme et sa version",
                "get_curriculum_snapshot",
            ),
            ("Conceptions erronées et malentendus", "get_misconceptions"),
            ("Enregistrer mon énergie et ma confiance", "record_affect"),
            (
                "Modifier mes disponibilités horaires",
                "update_availability_model",
            ),
            ("Clôturer la séance", "record_session_close"),
        ] {
            let found = catalog.search(query, 3).unwrap();
            assert_eq!(found[0].name, expected, "query: {query}");
        }
    }

    #[test]
    fn folds_composed_and_decomposed_french_accents_and_ligatures() {
        assert_eq!(
            tokens("ÉVALUATIONS maîtrise clôturer"),
            tokens("evaluations maitrise cloturer")
        );
        assert_eq!(tokens("e\u{301}valuation"), tokens("évaluation"));
        assert_eq!(
            tokens("ŒUVRE sœur ÆTHER français"),
            tokens("oeuvre soeur aether francais")
        );
        assert_eq!(
            tokens("activités notions réponses"),
            tokens("activité notion réponse")
        );
    }

    #[test]
    fn french_actions_match_imperatives_without_stemming_unrelated_words() {
        assert_eq!(
            tokens("cherche archive supprime soumets"),
            tokens("chercher archiver suppression soumettre")
        );
        assert_eq!(
            tokens("Prévisualise la modification"),
            tokens("previsualisation modifier")
        );
        assert_eq!(
            tokens("inscris réactive"),
            tokens("inscription reactivation")
        );
        assert_eq!(
            tokens("mer hiver dossier"),
            BTreeSet::from(["mer".into(), "hiver".into(), "dossier".into()])
        );
        // Identifiers and arguments still require exact live tool identities.
        let catalog = catalog(vec![tool("tutor", "delete_training")]);
        assert_eq!(
            catalog.search("Supprime le brouillon", 3).unwrap()[0].id,
            "tutor/delete_training"
        );
        assert_eq!(
            catalog.resolve(BINDING, "supprimer").err(),
            Some(CatalogError::UnknownTool)
        );
        assert_eq!(
            catalog.resolve(BINDING, "tutor/supprimer").err(),
            Some(CatalogError::UnknownTool)
        );
        assert!(catalog
            .search("retrouve la bibliothèque", 3)
            .unwrap()
            .is_empty());
    }

    #[test]
    fn live_local_catalog_reaches_95_percent_on_the_unchanged_french_regression_corpus() {
        // Fitted regression, not a held-out French language or model benchmark.
        // Public synthetic queries and live metadata; never execute these tools.
        let snapshot: Value =
            serde_json::from_str(include_str!("../fixtures/catalog-local.json")).unwrap();
        let corpus: Value =
            serde_json::from_str(include_str!("../fixtures/discovery-fr.json")).unwrap();
        assert_eq!(snapshot["server_id"], "tutor");
        let entries: Vec<_> = snapshot["tools"]
            .as_array()
            .unwrap()
            .iter()
            .map(|tool| ToolEntry {
                server_id: "tutor".into(),
                name: tool["name"].as_str().unwrap().into(),
                description: tool["description"].as_str().unwrap().into(),
                input_schema: tool["inputSchema"].clone(),
                annotations: tool["annotations"].clone(),
            })
            .collect();
        assert_eq!(entries.len(), 68);
        let catalog = catalog(entries);
        let cases = corpus["queries"].as_array().unwrap();
        assert_eq!(cases.len(), 80);
        assert_eq!(corpus["query_count"], 80);
        let mut expected_ids = BTreeSet::new();
        let mut misses = Vec::new();
        // Preserve all 62 baseline successes too; increasing the aggregate must
        // not silently exchange an old success for a new one.
        let baseline_misses = [
            8, 9, 11, 28, 29, 31, 34, 38, 39, 50, 56, 62, 64, 75, 76, 77, 78, 79,
        ];
        for (index, case) in cases.iter().enumerate() {
            let id = case["id"].as_str().unwrap();
            assert_eq!(id, format!("fr-{:03}", index + 1));
            let expected = case["expected_tool_id"].as_str().unwrap();
            expected_ids.insert(expected);
            catalog.resolve(BINDING, expected).unwrap();
            for distractor in case["distractor_tool_ids"].as_array().unwrap() {
                assert_ne!(distractor.as_str().unwrap(), expected);
                catalog
                    .resolve(BINDING, distractor.as_str().unwrap())
                    .unwrap();
            }
            let results = catalog.search(case["query"].as_str().unwrap(), 3).unwrap();
            assert!(results.len() <= 3);
            if !results.iter().any(|tool| tool.id == expected) {
                assert!(
                    baseline_misses.contains(&(index + 1)),
                    "new regression on {id}"
                );
                misses.push((
                    id,
                    expected,
                    results
                        .iter()
                        .map(|tool| tool.id.clone())
                        .collect::<Vec<_>>(),
                ));
            }
        }
        assert_eq!(expected_ids.len(), 68);
        assert!(
            (cases.len() - misses.len()) * 100 >= cases.len() * 95,
            "top-three misses: {misses:?}"
        );
    }

    #[test]
    fn identical_names_across_servers_remain_distinct_and_execution_is_exact() {
        let mut catalog = catalog(vec![tool("tutor", "get_next_activity")]);
        catalog
            .replace_server_tools(
                BINDING,
                "another",
                vec![tool("another", "get_next_activity")],
            )
            .unwrap();
        let found = catalog.search("prochaine activité", 3).unwrap();
        assert_eq!(
            found.iter().map(|s| s.id.as_str()).collect::<Vec<_>>(),
            vec!["another/get_next_activity", "tutor/get_next_activity"]
        );
        assert_eq!(
            catalog
                .resolve(BINDING, "tutor/get_next_activity")
                .unwrap()
                .server_id,
            "tutor"
        );
        assert_eq!(
            catalog
                .resolve(BINDING, "another/get_next_activity")
                .unwrap()
                .server_id,
            "another"
        );
        for id in [
            "get_next_activity",
            "prochaine activité",
            "TUTOR/get_next_activity",
            "tutor/get_next_activit",
            "tutor/get_next_activity/extra",
        ] {
            assert_eq!(
                catalog.resolve(BINDING, id).err(),
                Some(CatalogError::UnknownTool)
            );
        }
        assert_eq!(
            catalog.search("tutor/get_next_activity", 3).unwrap()[0].id,
            "tutor/get_next_activity"
        );
    }

    #[test]
    fn changed_binding_clears_tools_and_rejects_stale_updates_and_selection() {
        let mut catalog = catalog(vec![tool("tutor", "get_next_activity")]);
        assert!(!catalog.set_binding(BINDING.into()).unwrap());
        assert_eq!(catalog.len(), 1);
        let fresh = "account-2/server-epoch-2/rights-8";
        assert!(catalog.set_binding(fresh.into()).unwrap());
        assert!(catalog.is_empty());
        assert!(catalog.search("prochaine activité", 3).unwrap().is_empty());
        assert_eq!(
            catalog.replace_server_tools(
                BINDING,
                "tutor",
                vec![tool("tutor", "get_next_activity")]
            ),
            Err(CatalogError::StaleBinding)
        );
        catalog
            .replace_server_tools(fresh, "tutor", vec![tool("tutor", "get_next_activity")])
            .unwrap();
        assert_eq!(
            catalog.describe(BINDING, "tutor/get_next_activity").err(),
            Some(CatalogError::StaleBinding)
        );
        assert!(catalog.resolve(fresh, "tutor/get_next_activity").is_ok());
        assert_eq!(
            catalog.set_binding(String::new()),
            Err(CatalogError::InvalidBinding)
        );
        assert!(catalog.resolve(fresh, "tutor/get_next_activity").is_ok());
    }

    #[test]
    fn duplicate_or_invalid_updates_preserve_previous_snapshot_atomically() {
        let old = tool("tutor", "get_next_activity");
        let mut catalog = catalog(vec![old.clone()]);
        catalog
            .replace_server_tools(BINDING, "other", vec![tool("other", "unchanged")])
            .unwrap();
        let new = tool("tutor", "prepare_assessment_attempt");
        assert_eq!(
            catalog.replace_server_tools(BINDING, "tutor", vec![new.clone(), new.clone()]),
            Err(CatalogError::DuplicateTool)
        );
        let mut invalid = new.clone();
        invalid.input_schema = json!({"type":"string"});
        assert_eq!(
            catalog.replace_server_tools(BINDING, "tutor", vec![new.clone(), invalid]),
            Err(CatalogError::InvalidSchema)
        );
        assert_eq!(
            catalog.replace_server_tools(
                BINDING,
                "tutor",
                vec![new, tool("foreign", "wrong_server")]
            ),
            Err(CatalogError::ServerMismatch)
        );
        assert!(catalog.resolve(BINDING, "tutor/get_next_activity").unwrap() == &old);
        assert!(catalog
            .resolve(BINDING, "tutor/prepare_assessment_attempt")
            .is_err());
        assert!(catalog.resolve(BINDING, "other/unchanged").is_ok());
        assert_eq!(catalog.len(), 2);
    }

    #[test]
    fn complete_replacement_revokes_removed_tools_without_touching_other_servers() {
        let mut catalog = catalog(vec![tool("tutor", "old"), tool("tutor", "retained")]);
        catalog
            .replace_server_tools(BINDING, "other", vec![tool("other", "old")])
            .unwrap();
        let mut changed = tool("tutor", "retained");
        changed.input_schema = assessment_schema();
        catalog
            .replace_server_tools(BINDING, "tutor", vec![changed.clone()])
            .unwrap();
        assert!(catalog.resolve(BINDING, "tutor/old").is_err());
        assert!(catalog.resolve(BINDING, "other/old").is_ok());
        assert!(catalog.resolve(BINDING, "tutor/retained").unwrap() == &changed);
        catalog
            .replace_server_tools(BINDING, "tutor", vec![])
            .unwrap();
        assert_eq!(catalog.len(), 1);
        assert!(catalog.resolve(BINDING, "other/old").is_ok());
    }

    #[test]
    fn catalog_capacity_counts_replacements_and_servers_not_stale_snapshots() {
        let full: Vec<_> = (0..MAX_TOOLS)
            .map(|i| tool("tutor", &format!("operation_{i:03}")))
            .collect();
        let mut catalog = catalog(full.clone());
        assert_eq!(catalog.len(), MAX_TOOLS);
        catalog
            .replace_server_tools(BINDING, "tutor", full)
            .unwrap();
        assert_eq!(
            catalog.replace_server_tools(BINDING, "other", vec![tool("other", "overflow")]),
            Err(CatalogError::TooManyTools)
        );
        assert_eq!(catalog.len(), MAX_TOOLS);
        let too_many: Vec<_> = (0..=MAX_TOOLS)
            .map(|i| tool("tutor", &format!("new_{i:03}")))
            .collect();
        assert_eq!(
            catalog.replace_server_tools(BINDING, "tutor", too_many),
            Err(CatalogError::TooManyTools)
        );
        assert!(catalog.resolve(BINDING, "tutor/operation_000").is_ok());
        catalog
            .replace_server_tools(BINDING, "tutor", vec![])
            .unwrap();
        catalog
            .replace_server_tools(BINDING, "other", vec![tool("other", "now_fits")])
            .unwrap();
        assert_eq!(catalog.len(), 1);
    }

    #[test]
    fn schema_serialized_byte_limit_is_exact_including_escaped_values() {
        let mut catalog = ToolCatalog::new(BINDING.into()).unwrap();
        let mut entry = tool("tutor", "large_schema");
        entry.input_schema = json!({"type":"object","properties":{},"x-padding":""});
        let overhead = serde_json::to_vec(&entry.input_schema).unwrap().len();
        entry.input_schema["x-padding"] = json!("x".repeat(MAX_SCHEMA_BYTES - overhead));
        assert_eq!(
            serde_json::to_vec(&entry.input_schema).unwrap().len(),
            MAX_SCHEMA_BYTES
        );
        catalog
            .replace_server_tools(BINDING, "tutor", vec![entry.clone()])
            .unwrap();
        let accepted = entry.clone();
        entry.input_schema["x-padding"] = json!("x".repeat(MAX_SCHEMA_BYTES - overhead + 1));
        assert_eq!(
            catalog.replace_server_tools(BINDING, "tutor", vec![entry]),
            Err(CatalogError::SchemaTooLarge)
        );
        assert!(catalog.resolve(BINDING, "tutor/large_schema").unwrap() == &accepted);
        let mut escaped = tool("tutor", "escaped");
        escaped.input_schema["x-padding"] = json!("\n".repeat(MAX_SCHEMA_BYTES / 2));
        assert_eq!(
            catalog.replace_server_tools(BINDING, "tutor", vec![escaped]),
            Err(CatalogError::SchemaTooLarge)
        );
    }

    #[test]
    fn validates_names_shapes_descriptions_and_annotation_bounds() {
        let mut catalog = ToolCatalog::new(BINDING.into()).unwrap();
        for name in ["", "x/y", "with space", "évaluation", "line\nbreak"] {
            assert_eq!(
                catalog.replace_server_tools(BINDING, "tutor", vec![tool("tutor", name)]),
                Err(CatalogError::InvalidName)
            );
        }
        assert_eq!(
            catalog.replace_server_tools(BINDING, "invalid/server", vec![]),
            Err(CatalogError::InvalidName)
        );
        for schema in [
            json!(true),
            json!([]),
            json!({"type":"array"}),
            json!({"properties":[]}),
            json!({"required":[123]}),
        ] {
            let mut entry = tool("tutor", "invalid");
            entry.input_schema = schema;
            assert_eq!(
                catalog.replace_server_tools(BINDING, "tutor", vec![entry]),
                Err(CatalogError::InvalidSchema)
            );
        }
        let mut entry = tool("tutor", "too_large");
        entry.description = "x".repeat(MAX_DESCRIPTION_BYTES + 1);
        assert_eq!(
            catalog.replace_server_tools(BINDING, "tutor", vec![entry]),
            Err(CatalogError::DescriptionTooLarge)
        );
        for annotations in [json!([]), json!({"large":"x".repeat(MAX_ANNOTATION_BYTES)})] {
            let mut entry = tool("tutor", "invalid");
            entry.annotations = annotations;
            assert_eq!(
                catalog.replace_server_tools(BINDING, "tutor", vec![entry]),
                Err(CatalogError::InvalidAnnotations)
            );
        }
        assert!(catalog.is_empty());
    }

    #[test]
    fn supports_composed_object_schemas_without_flattening_and_limits_depth() {
        let mut entry = tool("tutor", "composed");
        entry.input_schema = json!({"allOf":[{"$ref":"#/$defs/input"}],"$defs":{"input":{"type":"object","additionalProperties":false}}});
        let mut catalog = catalog(vec![entry.clone()]);
        assert!(catalog.describe(BINDING, "tutor/composed").unwrap() == entry);
        assert_eq!(
            catalog.search("composed", 1).unwrap()[0].signature,
            "composed(…)"
        );
        let mut nested = json!({});
        for _ in 0..65 {
            nested = json!({"nested":nested});
        }
        entry.input_schema = nested;
        assert_eq!(
            catalog.replace_server_tools(BINDING, "tutor", vec![entry]),
            Err(CatalogError::SchemaTooLarge)
        );
    }

    #[test]
    fn search_is_bounded_and_deterministic_independent_of_update_order() {
        let mut entries = vec![
            tool("tutor", "z"),
            tool("tutor", "a"),
            tool("tutor", "m"),
            tool("tutor", "b"),
        ];
        for entry in &mut entries {
            entry.description = "formation identique".into();
        }
        let first = catalog(entries.clone());
        entries.reverse();
        let second = catalog(entries);
        let a = first.search("formation", usize::MAX).unwrap();
        let b = second.search("formation", usize::MAX).unwrap();
        assert_eq!(a, b);
        assert_eq!(
            a.iter().map(|t| t.name.as_str()).collect::<Vec<_>>(),
            vec!["a", "b", "m"]
        );
        assert!(first.search("formation", 0).unwrap().is_empty());
        assert!(first.search("   ", 3).unwrap().is_empty());
        assert!(first.search("unmatchedword", 3).unwrap().is_empty());
        assert_eq!(
            first.search(&"x".repeat(MAX_QUERY_BYTES + 1), 3),
            Err(CatalogError::QueryTooLarge)
        );
    }

    #[test]
    fn unicode_descriptions_and_large_property_lists_stay_compact() {
        let mut entry = tool("tutor", "unicode_tool");
        entry.description = "Élève 🦀 ".repeat(200);
        let properties = (0..30)
            .map(|i| {
                (
                    format!("argument_{i:02}_{}", "é".repeat(30)),
                    json!({"type":"string","default":"private-default"}),
                )
            })
            .collect::<serde_json::Map<_, _>>();
        entry.input_schema = json!({"type":"object","properties":properties});
        let catalog = catalog(vec![entry.clone()]);
        let found = catalog.search("élève", 3).unwrap();
        let summary = &found[0];
        assert!(summary.description.len() <= MAX_SUMMARY_DESCRIPTION_BYTES);
        assert!(summary.description.ends_with('…'));
        assert!(summary.signature.len() <= MAX_SIGNATURE_BYTES);
        assert!(summary.signature.contains('…'));
        assert!(!summary.signature.contains("private-default"));
        assert!(!summary.signature.contains("argument_29"));
        assert!(catalog.describe(BINDING, "tutor/unicode_tool").unwrap() == entry);
    }

    #[test]
    fn errors_never_echo_binding_or_server_payloads() {
        let mut catalog = catalog(vec![tool("tutor", "get_next_activity")]);
        let error = catalog
            .replace_server_tools("private-secret-binding", "tutor", vec![])
            .unwrap_err();
        assert_eq!(error, CatalogError::StaleBinding);
        assert!(!error.to_string().contains("private-secret"));
        assert!(!format!("{error:?}").contains("private-secret"));
        assert_eq!(
            ToolCatalog::new(String::new()).err(),
            Some(CatalogError::InvalidBinding)
        );
        assert_eq!(
            ToolCatalog::new("x".repeat(1025)).err(),
            Some(CatalogError::InvalidBinding)
        );
    }
}
