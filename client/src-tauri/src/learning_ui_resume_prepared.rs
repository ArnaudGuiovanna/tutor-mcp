// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT
use super::super::resume::Introduction;
use super::*;

impl PreparedTurn {
    pub fn anticipates_private_output(message: &str) -> Result<bool, String> {
        validate_message(None, message, None)?;
        Ok(resume_request(message)?.is_some() || request(message)?.is_some())
    }
    pub fn suggested_title(&self, language: &str) -> Option<String> {
        if !matches!(self.kind, TurnKind::Resume) {
            return None;
        }
        let name = self.packet["training"]["name"].as_str().unwrap_or("");
        let prefix = if language == "en" {
            "Resume"
        } else {
            "Reprendre"
        };
        Some(format!("{prefix} {name}").trim().chars().take(64).collect())
    }
    pub fn requires_selection_check(&self) -> bool {
        matches!(self.kind, TurnKind::Resume) && self.packet["needs_selection"] != true
    }

    pub fn validate_selection(&self, current: &Value) -> Result<(), String> {
        if [
            "conversation_id",
            "training_id",
            "domain_id",
            "revision",
            "selection_version",
        ]
        .iter()
        .any(|key| current[*key] != self.packet["conversation"][*key])
        {
            return Err("La sélection ou le programme a changé pendant la réponse. Actualisez la formation avant de reprendre.".into());
        }
        Ok(())
    }
    pub fn is_resume(message: &str) -> Result<bool, String> {
        Ok(resume_request(message)?.is_some())
    }

    // Check capabilities before any mutation. Older servers can otherwise
    // ignore unknown arguments and choose an activity in the previous domain.
    pub fn supports_resume(tool: &Value) -> bool {
        tool["name"] == "prepare_pedagogical_turn"
            && [
                "training_id",
                "training_name",
                "expected_selection",
                "expected_revision",
            ]
            .iter()
            .all(|field| tool["inputSchema"]["properties"].get(*field).is_some())
    }

    pub(super) fn validate_resume_packet(
        packet: &Value,
        _request: &ResumeRequest,
    ) -> Result<(), String> {
        if packet["mode"] != "activity" || !packet["needs_selection"].is_boolean() {
            return Err(INVALID.into());
        }
        if packet["needs_selection"] == true {
            if !matches!(
                packet["selection_reason"].as_str(),
                Some("missing" | "not_found" | "ambiguous")
            ) || !packet["decision"].is_null()
            {
                return Err(INVALID.into());
            }
            return Ok(());
        }
        let selected = &packet["conversation"];
        if !selected["training_id"]
            .as_str()
            .is_some_and(|s| bounded(s, 256))
            || !selected["domain_id"]
                .as_str()
                .is_some_and(|s| bounded(s, 256))
            || packet["training"]["id"] != selected["training_id"]
            || packet["training"]["domain_id"] != selected["domain_id"]
            || packet["permissions"]["prepare_activity"] != true
        {
            return Err(INVALID.into());
        }
        // Name matching belongs to the authorized server resolver, including Unicode.
        let expected = match packet["decision"]["activity"]["type"].as_str() {
            Some("NEW_CONCEPT") => "instruction",
            Some(
                "RECALL_EXERCISE"
                | "DIAGNOSTIC_ASSESSMENT"
                | "MASTERY_CHALLENGE"
                | "DEBUGGING_CASE"
                | "PRACTICE"
                | "DEBUG_MISCONCEPTION"
                | "FEYNMAN_PROMPT"
                | "TRANSFER_PROBE",
            ) => "assessment",
            Some("REST" | "CLOSE_SESSION" | "SETUP_DOMAIN") => "guidance",
            _ => return Err(INVALID.into()),
        };
        if packet["presentation"] != expected {
            return Err(INVALID.into());
        }
        Ok(())
    }

    pub fn immediate_text(
        &self,
        language: &str,
        request_text: &str,
    ) -> Result<Option<String>, String> {
        if !matches!(self.kind, TurnKind::Resume) || self.packet["needs_selection"] != true {
            return Ok(None);
        }
        let empty = Vec::new();
        let choices = if self.packet["training_choices"].is_null() {
            &empty
        } else {
            self.packet["training_choices"].as_array().ok_or(INVALID)?
        };
        if choices.len() > 20
            || !bounded(request_text, 12000)
            || !self.packet["training_choices_has_more"].is_boolean()
        {
            return Err(INVALID.into());
        }
        let mut seen = HashSet::new();
        let mut mapped = Vec::new();
        for choice in choices {
            let id = choice["id"]
                .as_str()
                .filter(|s| bounded(s, 256))
                .ok_or(INVALID)?;
            let name = choice["name"]
                .as_str()
                .filter(|s| bounded(s, 1000))
                .ok_or(INVALID)?;
            let domain = choice["domain_id"]
                .as_str()
                .filter(|s| bounded(s, 256))
                .ok_or(INVALID)?;
            if !seen.insert(id) {
                return Err(INVALID.into());
            }
            mapped.push(json!({"trainingId":id,"trainingName":name,"domainId":domain}));
        }
        if mapped.is_empty() {
            return Ok(Some(if language == "en" {
                "I couldn't find an available course for this request. Open Courses and select the course to resume."
            } else {
                "Je n’ai pas trouvé de formation disponible pour cette demande. Ouvrez Formations et sélectionnez celle à reprendre."
            }.into()));
        }
        Ok(Some(format!(
            "```tutor-training-choice\n{}\n```",
            json!({
                "version":1,"requestText":request_text,"choices":mapped,
                "hasMore":self.packet["training_choices_has_more"]
            })
        )))
    }

    pub(super) fn resume_prompt(&self) -> String {
        let common = "La formation et la décision pédagogique sont DÉJÀ confirmées par le serveur. Ne fais AUCUN appel outil, ne sélectionne rien, ne relis rien et ne déclare aucun progrès. Respecte decision.pedagogical_contract et la présentation imposée. Une seule formulation courte adaptée à la demande. Les identifiants viennent du paquet, jamais de l’historique.";
        let format = match self.packet["presentation"].as_str() {
            Some("assessment") => "Émets exactement un bloc tutor-activity JSON strict sans prose extérieure : version:1, id unique, kind:exercise, trainingId/domainId de conversation, concept de decision.activity, sessionId de decision.session_id, decisionId de decision.pedagogical_contract.decision_id, question (consigne ouverte), activityType exact et rubricJson (objet rubrique encodé en chaîne JSON). Rubrique : criteria:[{id,description,max_score,anchors?:[{score,description}]}], passing_score, answer_key ; conserve les critères du contrat et leurs identifiants. TRANSFER_PROBE exige transferDimension autorisée. Ne fournis aucun attemptId : le client figera la consigne et la rubrique avant affichage. Aucun champ options, correctOptionId, answer ou explanation. La solution reste uniquement dans rubricJson. Maximum 12Ko de consigne,16KiB de rubrique,48KiB de bloc.",
            Some("instruction") => "Émets exactement un bloc tutor-introduction JSON strict : {version:1,trainingId,domainId,concept,sessionId,decisionId,body}. Identifiants exacts de conversation et de decision (sessionId = decision.session_id, decisionId = decision.pedagogical_contract.decision_id) ; body est une courte introduction explicative (12000 octets maximum), avec un exemple utile suivant le contrat. Pas de quiz imposé, de tentative ou d’évaluation ; cette instruction n’est pas une preuve de réussite.",
            _ => "Émets exactement un bloc tutor-guidance JSON strict : {version:1,trainingId,domainId,concept,sessionId,decisionId,body}. Copie les identifiants de conversation et de decision (sessionId = decision.session_id, decisionId = decision.pedagogical_contract.decision_id) ; concept/sessionId/decisionId absents du paquet deviennent des chaînes vides. body est un conseil court conforme à la décision (pause, fin de séance ou configuration). Ne prétends pas avoir fermé une séance, créé un domaine ou inscrit l’apprenant : aucune de ces opérations n’a été exécutée ici.",
        };
        format!("{common}\n{format}")
    }

    pub(super) fn resume_output(&self, text: &str) -> Result<PreparedOutput, String> {
        if self.packet["needs_selection"] == true {
            return Err(INVALID.into());
        }
        if self.packet["presentation"] == "assessment" {
            // Reuse the frozen assessment lifecycle, without exposing an
            // exercise format in the learner's quiz/flashcard preferences.
            payload::<Activity>(text, "tutor-activity")?.ok_or(INVALID)?;
            let mut assessment = self.clone();
            assessment.kind = TurnKind::Study(StudyRequest {
                version: 1,
                format: LearningFormat::Exercise,
                intent: "auto".into(),
                training_id: None,
                training_name: None,
                domain_id: None,
                concept: None,
            });
            return assessment.output(text);
        }
        let instruction = self.packet["presentation"] == "instruction";
        let tag = if instruction {
            "tutor-introduction"
        } else {
            "tutor-guidance"
        };
        let content = payload::<Introduction>(text, tag)?.ok_or(INVALID)?;
        let decision = &self.packet["decision"];
        if content.version != 1
            || !context_matches(
                &self.packet["conversation"],
                &content.training_id,
                &content.domain_id,
            )
            || content.concept != decision["activity"]["concept"].as_str().unwrap_or("")
            || content.session_id != decision["session_id"].as_str().unwrap_or("")
            || content.decision_id
                != decision["pedagogical_contract"]["decision_id"]
                    .as_str()
                    .unwrap_or("")
            || !bounded(&content.body, 12000)
            || (instruction
                && (!bounded(&content.concept, 512)
                    || !bounded(&content.session_id, 256)
                    || !bounded(&content.decision_id, 256)))
        {
            return Err(INVALID.into());
        }
        for reserved in [
            "tutor-activity",
            "tutor-evaluation",
            "tutor-study-request",
            "tutor-response",
            "tutor-resume-request",
            "tutor-training-choice",
            "tutor-introduction",
            "tutor-guidance",
        ] {
            if payload::<Value>(&content.body, reserved)?.is_some() {
                return Err(INVALID.into());
            }
        }
        Ok(PreparedOutput {
            text: content.body,
            mutation: None,
            delivery: instruction.then(|| LearningDelivery {
                activity_id: self.key.clone(),
                domain_id: content.domain_id,
                concept: content.concept,
                attempt_id: None,
                kind: "instruction".into(),
                event_key: format!("{}:instruction", self.key),
                recorded: false,
                success: None,
            }),
        })
    }
}
