# [5] ActionSelector — Design (Phase 1)

> **Archive de conception — non normative.** Les références au routeur
> `engine/router.go` décrivent l'ancien chemin de migration, aujourd'hui
> supprimé. Le runtime supporté compose ce sélecteur depuis
> `engine/orchestrator.go`.

> Composant 3/7 du pipeline de régulation. Reçoit un `concept_id` déjà
> choisi par `[4] ConceptSelector` dans l'orchestrateur courant et
> décide *quoi* faire dessus : type d'activité +
> `DifficultyTarget`. Phase-agnostique : ne consomme ni `phase` ni
> `goal_relevance`.
>
> Référence architecture : `docs/regulation-architecture.md` §3 [5],
> §6 Q4 (extension d'enum).

---

## 1. Nature du composant

`[5] ActionSelector` est une **fonction pure** :

```go
func SelectAction(concept string, cs *models.ConceptState, mc *models.Misconception) Action
```

Aucun accès store, aucun side-effect, aucun appel réseau. Pas de logger
non plus — la décision est entièrement dérivée des paramètres en
entrée. Ça permet de la tester unitairement avec des fixtures, et de
la composer trivialement par les composants amont.

### Pourquoi *avant* `[4] ConceptSelector`

L'ordre Q6 (`7 → 1 → 5 → 4 → 3 → 2 → 6`) place `[5]` avant `[4]` parce
que :

- `[5]` n'a pas besoin de `goal_relevance` (le signal de `[1]`).
- `[5]` n'a pas besoin de `phase` (le signal de `[2]`).
- `[5]` est strictement local au concept choisi.
- L'orchestrateur courant appelle `[5]` après le choix de concept par
  `[4]`, ce qui garde la décision d'action isolée du choix de concept.

Cette séparation amont/aval est explicitement le bénéfice de l'ordre
choisi.

### Comment `[5]` est utilisé par le runtime

État courant : `get_next_activity` appelle l'orchestrateur de phase,
qui compose `[4] SelectConcept`, `[5] SelectAction` et `[3] ApplyGate`.
Le router legacy n'est plus le chemin normal de sélection.

`REGULATION_ACTION` ne contrôle pas ce câblage runtime. Le flag ne fait
qu'inclure ou retirer l'appendix explicatif dans `tools/prompt.go`.

---

## 2. Signaux consommés

| Source | Champ | Localisation actuelle |
|--------|-------|------------------------|
| BKT mastery | `cs.PMastery` | `models/learner.go:33` |
| FSRS state | `cs.Stability`, `cs.ElapsedDays`, `cs.CardState` | `models/learner.go:24-30` |
| FSRS Retrievability dérivée | `algorithms.Retrievability(cs.ElapsedDays, cs.Stability)` | calcul, pas de stockage |
| IRT theta | `cs.Theta` | `models/learner.go:38` |
| Misconception active sur le concept | `*models.Misconception` (status="active") | `db/misconceptions.go` (méthode existante : `GetActiveMisconceptions(learnerID, concept)`) |

Aucune dépendance à `goal_relevance`, à la `phase`, à
`autonomy_score`, ou aux interactions historiques globales. Les
signaux historiques (rotation Mastery → Feynman → Transfer) sont
re-dérivés du concept_state via un compteur d'interactions par type
d'activité — voir §4.

## 3. Décision produite

```go
// engine/action_selector.go
type Action struct {
    Type             models.ActivityType
    DifficultyTarget float64 // ∈ [0,1]
    Format           string  // hint au LLM, optionnel
    EstimatedMinutes int
    Rationale        string  // message lisible humain (audit, cockpit)
}

func SelectAction(concept string, cs *models.ConceptState, active *models.Misconception, history ActionHistory) Action
```

Où `ActionHistory` est une mini-structure pour la rotation high-mastery :

```go
type ActionHistory struct {
    MasteryChallengeCount int    // sur ce concept, combien de MasteryChallenge déjà émis
    FeynmanCount          int
    TransferCount         int
    LastMasteryChallenge  *time.Time
    LastFeynman           *time.Time
    LastTransfer          *time.Time
}
```

Cette structure est **dérivée par le caller** depuis `interactions`
filtrées sur le concept. ActionSelector ne l'agrège pas — il la
consomme. (Voir OQ-5.2 sur l'algorithme de rotation.)

---

## 4. Algorithme — cascade de règles

```
SelectAction(concept, cs, mc, history):
    // Override 1 — misconception active prend la priorité absolue
    if mc != nil:
        return Action{
            Type: ActivityDebugMisconception,           // OQ-5.1
            DifficultyTarget: 0.55,
            Format: "misconception_targeted",
            EstimatedMinutes: 12,
            Rationale: "misconception active : " + mc.Type,
        }

    // Override 2 — FSRS retention basse (oubli imminent ou consommé)
    if cs.CardState != "new":
        retention = Retrievability(cs.ElapsedDays, cs.Stability)
        if retention < retentionForgettingThreshold():    // OQ-5.4 (cf §6)
            return Action{
                Type: ActivityRecall,
                DifficultyTarget: 0.65,
                Format: "code_completion",
                EstimatedMinutes: 8,
                Rationale: fmt.Sprintf("retention FSRS basse (%.0f%%)", retention*100),
            }

    // Cas par mastery — pas de chevauchement, ordre du seuil bas vers haut
    p = cs.PMastery
    switch:
        case p < 0.30:
            return Action{
                Type: ActivityNewConcept,
                DifficultyTarget: 0.55,
                Format: "introduction",
                EstimatedMinutes: 15,
                Rationale: "introduction : mastery < 0.30",
            }
        case p < 0.70:
            return Action{
                Type: ActivityPractice,                    // NEW Q4
                DifficultyTarget: 0.55,                    // milieu, building up
                Format: "practice_standard",
                EstimatedMinutes: 10,
                Rationale: fmt.Sprintf("practice : mastery %.2f", p),
            }
        case p < MasteryBKT():                              // 0.85 (legacy/unified)
            // ZPD calibrée par IRT (la plage 0.7–0.85 est la zone
            // "presque maîtrisé" — on serre la difficulté pour cibler
            // pCorrect ~ 0.70, qui est le centre de la ZPD selon
            // IRTIsInZPD (algorithms/irt.go:38).
            d = zpdDifficultyFromTheta(cs.Theta)            // OQ-5.3
            return Action{
                Type: ActivityPractice,
                DifficultyTarget: d,
                Format: "practice_zpd",
                EstimatedMinutes: 12,
                Rationale: fmt.Sprintf("ZPD via IRT θ=%.2f → diff=%.2f", cs.Theta, d),
            }
        default:  // p ≥ MasteryBKT()
            return selectHighMasteryAction(concept, cs, history)
}

selectHighMasteryAction(concept, cs, history):
    // Rotation gated : MasteryChallenge → Feynman → Transfer
    // (voir OQ-5.2 pour la stratégie de rotation)
    if history.MasteryChallengeCount == 0:
        return Action{
            Type: ActivityMasteryChallenge,
            DifficultyTarget: 0.75,
            Format: "build_challenge",
            EstimatedMinutes: 45,
            Rationale: "mastery >= 0.85 : premier mastery challenge",
        }
    if history.FeynmanCount < history.MasteryChallengeCount:
        return Action{
            Type: ActivityFeynmanPrompt,                  // NEW Q4
            DifficultyTarget: 0.50,                        // pas vraiment difficulté, neutre
            Format: "feynman_explanation",
            EstimatedMinutes: 15,
            Rationale: "consolidation post-mastery via Feynman",
        }
    // Sinon : Transfer
    return Action{
        Type: ActivityTransferProbe,                       // NEW Q4
        DifficultyTarget: 0.65,
        Format: "transfer_novel_context",
        EstimatedMinutes: 20,
        Rationale: "transfert hors contexte initial",
    }
```

### Notes d'algorithme

- **Misconception override > FSRS retention** (OQ-5.4) : si une
  misconception est active ET la rétention est basse, on traite la
  misconception d'abord. Justification : la rétention basse renvoie
  l'apprenant à un savoir potentiellement faux (la misconception) ;
  fixer la misconception est prérequis à un recall sain.
- **Pas de session-dedup ici.** Le sessionRepeat est dans `[3] Gate`
  (Q5). `[5]` ne consulte pas la session.
- **`zpdDifficultyFromTheta(theta)`** : voir §5.

---

## 5. DifficultyTarget pour PRACTICE en plage [0.70, 0.85] — dérivation IRT

### Cible pédagogique

ZPD selon `algorithms/irt.go:38` (`IRTIsInZPD`) : `pCorrect ∈ [0.55, 0.80]`.
Centre confortable : 0.70.

### Modèle 2PL utilisé

```
P(correct | θ, b) = 1 / (1 + exp(-a·(θ - b)))
```

Avec a=1 (paramètre de discrimination, simplification — on ne
modélise pas a per item ici), pour viser pCorrect = 0.70 :

```
0.70 = 1 / (1 + exp(-(θ - b)))
1 / 0.70 = 1 + exp(-(θ - b))
exp(-(θ - b)) = 0.428...
-(θ - b) = ln(0.428) ≈ -0.847
b = θ - 0.847
```

### Mapping `b ∈ ℝ` vers `DifficultyTarget ∈ [0,1]`

`DifficultyTarget` est borné dans le format `Activity`. Le mapping
naturel est la sigmoïde :

```
DifficultyTarget = sigmoid(b) = 1 / (1 + exp(-b))
                = sigmoid(θ - 0.847)
```

### Plage observée

| θ | DifficultyTarget |
|---|------------------|
| -1.0 | 0.135 (clamp 0.30) |
| 0.0 | 0.300 |
| 0.5 | 0.414 |
| 1.0 | 0.538 |
| 2.0 | 0.760 |
| 3.0 | 0.895 (clamp 0.85) |

Avec `cs.Theta` clampé à [-4, 4] par `IRTUpdateTheta` (algorithms/irt.go),
ce mapping reste sain. Clamp final dans `[0.30, 0.85]` cohérent avec
le router legacy (`engine/router.go:149-154`).

### Implémentation en Go

```go
// zpdDifficultyFromTheta computes a DifficultyTarget that aims for
// pCorrect ≈ 0.70 in a 2PL IRT model with discrimination a=1, then maps
// the latent difficulty to [0,1] via the logistic and clamps to the
// established [0.30, 0.85] envelope. See docs/regulation-design/05-action-selector.md §5.
func zpdDifficultyFromTheta(theta float64) float64 {
    const targetLogit = 0.847 // ln(0.7/0.3)
    b := theta - targetLogit
    d := 1.0 / (1.0 + math.Exp(-b))
    return clamp(d, 0.30, 0.85)
}
```

(`clamp` est déjà défini dans `algorithms/`.)

---

## 6. Cas dégénérés

| Cas | Comportement | Garantie |
|-----|---------------|----------|
| `cs == nil` | Erreur retournée par le caller (concept introuvable). `[5]` panique sur nil deref → caller doit valider. | Contractuel. |
| `cs.CardState == "new"` (jamais pratiqué) | Saute l'override FSRS retention (pas de Stability significative). Tombe dans le branch mastery. PMastery=0.1 par défaut → mastery < 0.30 → NewConcept. | Cohérent avec `engine/alert.go:20` qui exclut les `card_state="new"` de FORGETTING. |
| `cs.PMastery = NaN` (cf F-1.3 audit) | Toutes les comparaisons NaN sont false → tombe dans le `default` (high-mastery branch). Avec `history.MasteryChallengeCount=0` → MasteryChallenge. **Mauvais.** | Voir OQ-5.6 — défense optionnelle. |
| `cs.Theta = NaN` ou clampé à ±4 | `zpdDifficultyFromTheta` retourne 0.30 (theta=-4) ou 0.85 (theta=4). Clamp cohérent. NaN → math.Exp(NaN) = NaN → clamp NaN — clamp doit gérer. | Voir OQ-5.6. |
| Concept jamais interagi mais `mc != nil` | Misconception override gagne, retourne DebugMisconception. Cohérent. | OK. |
| `MasteryBKT()` change (REGULATION_THRESHOLD) | Le seuil 0.85 utilisé dans le case statement lit l'accesseur ; la cascade respecte le profil actif. | OK — pas de literal hardcoded. |
| `history` toutes les `Count == 0` mais `mastery >= 0.85` | Première branche : MasteryChallenge. | OK. |
| `history.MasteryChallengeCount == 5, FeynmanCount == 5, TransferCount == 5` | Tombe sur Transfer (default branch). Pas d'épuisement. | Pédagogiquement défensable : on tourne indéfiniment sur le concept maîtrisé. (Le routeur amont, `[4]`/router legacy, choisira plus rarement ce concept à mesure qu'il devient maîtrisé. La rotation n'est pas censée tourner à l'infini.) |

---

## 7. Stratégie de test

### 7.1 Unit — fonction pure

```go
// engine/action_selector_test.go
TestSelectAction_Misconception_OverridesAll
TestSelectAction_RetentionLow_TriggersRecall
TestSelectAction_NewCardSkipsRetentionCheck
TestSelectAction_MasteryUnder30_NewConcept
TestSelectAction_Mastery30To70_PracticeStandard
TestSelectAction_Mastery70To85_PracticeZPD
TestSelectAction_HighMastery_FirstIsMasteryChallenge
TestSelectAction_HighMastery_RotatesToFeynman
TestSelectAction_HighMastery_RotatesToTransfer
TestSelectAction_HighMastery_StaysOnTransferAfterCycle
TestSelectAction_RespectsMasteryBKTAccessor // assert via t.Setenv("REGULATION_THRESHOLD", "off") then "on"
```

### 7.2 Unit — formule ZPD

```go
TestZPDDifficulty_TargetsPCorrect70
TestZPDDifficulty_ClampsLow
TestZPDDifficulty_ClampsHigh
TestZPDDifficulty_NaNHandling
```

Vérification numérique de la formule :
- θ = 0.847 → DifficultyTarget ≈ 0.50 (b=0, sigmoid(0)=0.5)
- θ = 0 → DifficultyTarget = 0.30 (clamp bas atteint à b=-0.847)
- θ = 4 → DifficultyTarget = 0.85 (clamp haut)

### 7.3 Régression — fonction pure câblée par l'orchestrateur

`SelectAction` reste une fonction pure et testable isolément, même
lorsqu'elle est appelée par l'orchestrateur. Les tests dédiés vivent
dans `engine/action_selector_test.go`.

### 7.4 Pas de fixture-shift sur les tests existants

Comme `[7]` et `[1]` : les fixtures existantes ne sont pas modifiées.

---

## 8. Interaction amont/aval

### Amont

- **`[4] ConceptSelector`** : choisit un `concept_id`; l'orchestrateur
  appelle ensuite `SelectAction` pour l'envelopper.
- **`[2] PhaseController`** : en `INSTRUCTION`/`MAINTENANCE`, délègue
  à `SelectAction` après que `[4]` a choisi le concept. En
  `DIAGNOSTIC`, `[2]` émet directement `DIAGNOSTIC_PROBE` sans passer
  par `[5]`.

### Aval (consommateurs de l'output)

- `tools/activity.go` : aujourd'hui, post-traite l'activity (tutor_mode
  multiplicateurs, calibration_bias, motivation_brief, misconception
  enrichment). Cette couche reste en place après `[5]` — elle traite
  l'output de `[5]` exactement comme aujourd'hui elle traite l'output
  de `Route`. Pas de changement requis dans cette PR.

### Pas d'interaction avec `[1]` ni `[7]`

- `[1]` (`goal_relevance`) n'est pas consommé par `[5]`. Confirmé par
  la cadrage utilisateur.
- `[7]` est consommé via l'accesseur `algorithms.MasteryBKT()` à un
  seul endroit (la branche high-mastery). Cohérent avec la sémantique
  de rebranding du seuil.

---

## 9. Décisions ouvertes

### OQ-5.1 — `DEBUG_MISCONCEPTION` distinct ou réutiliser `DEBUGGING_CASE` ?

`DEBUGGING_CASE` est aujourd'hui émis par le router legacy uniquement
pour les `PLATEAU` alerts (rotation `debugging` / `real_world_case` /
`teaching_exercise` / `creative_application` — `engine/router.go:47-67`).
Sémantiquement c'est « casser un plateau via un format challenging ».

Le cas `[5]` est différent : « la dernière interaction a révélé une
misconception active, l'apprenant tient une croyance fausse, l'exercice
doit la confronter ». Pas un plateau, un debug ciblé.

**A.** Introduire une nouvelle constante `ActivityDebugMisconception`
(« DEBUG_MISCONCEPTION »). Ajout au PR `[5]`. Le system prompt et le
LLM-side handling distinguent les deux.

**B.** Réutiliser `ActivityDebuggingCase` mais distinguer via `Format`
(plateau → "debugging"/"real_world_case"/...; misconception →
"misconception_targeted"). Le LLM lit `Format` pour comprendre le
contexte.

**Mon défaut** : **A**. Sémantique distincte → enum distinct, plus lisible
côté audit, ne brouille pas le rationale dans le cockpit. Coût : 1
constante, 1 mention dans le system prompt, ~10 lignes de doc.

### OQ-5.2 — Rotation `MasteryChallenge → Feynman → Transfer`

Ordre attendu : Mastery (build), Feynman (explain), Transfer (apply).
Question : sur quoi se base la rotation ? Trois propositions :

**A.** Cascade gated par compteur (mon défaut implicite ci-dessus) :
si `MCcount == 0` → Mastery ; sinon si `FeynmanCount < MCcount` → Feynman ;
sinon Transfer. Simple, déterministe, mais peut "stagner" sur Transfer
indéfiniment.

**B.** Round-robin par compteurs égalisés : la rotation choisit le type
qui a le compteur le plus bas, tie-break sur l'âge du dernier appel
(le plus ancien d'abord). Plus uniforme, garantit qu'on ne reste pas
sur Transfer.

**C.** Calendrier temporel : Mastery une fois, puis Feynman si
`time.Since(LastMasteryChallenge) > 7d`, puis Transfer si
`time.Since(LastFeynman) > 7d`. Découpé dans le temps.

**Mon défaut** : **A**, parce que c'est la stratégie la plus simple à
tester et la plus prédictive en synthétique. La "stagnation sur
Transfer" évoquée n'est pas réelle : le routeur amont (`[4]`/legacy)
choisit moins souvent un concept déjà maîtrisé, donc on ne reverra ce
concept que ponctuellement, jamais à fréquence rapprochée. **B** et **C**
sont raffinements sans bénéfice mesurable au stade synthétique.

### OQ-5.3 — Confirmation de la formule ZPD

Détaillée en §5. Repose sur :

- IRT 2PL avec `a=1` (approximation, pas de discrimination per item).
- pCorrect cible = 0.70 (centre de la ZPD `IRTIsInZPD` 0.55–0.80).
- Mapping `b → DifficultyTarget` via sigmoïde.
- Clamp final dans `[0.30, 0.85]`.

**A.** Confirmer la formule telle que (mon défaut). Simple, défendable,
testable.

**B.** Modifier la cible de pCorrect (e.g. 0.65 ou 0.75) pour ajuster la
difficulté. Réversible, faible coût.

**C.** Modéliser `a` per concept ou per learner. Hors scope (pas de
calibration par item dans le runtime actuel — IRT y est utilisé en mode
"global" sur cs.Theta uniquement).

**Mon défaut** : **A**. La formule est défendable, et la cible 0.70
correspond au milieu publié de la ZPD pour la pratique délibérée. À
revisiter empiriquement si l'eval harness révèle une dérive.

### OQ-5.4 — Précédence misconception vs retention

Si misconception active ET retention < seuil sur le même concept :

**A.** Misconception gagne (mon défaut, codé dans le pseudo-code §4).
Justification : le recall renvoie l'apprenant à une croyance
potentiellement fausse ; mieux vaut fixer la misconception d'abord.

**B.** Retention gagne. Justification : si on a oublié, le concept
est inopérant ; ré-ancrer d'abord, fixer la misconception au prochain
cycle quand le savoir est réactivé.

**Mon défaut** : **A**. Une misconception active est un signal fort
(pas un alea de mémoire) ; ne pas la confronter alors qu'on a la
preuve qu'elle est vivante serait perdre un cycle de remédiation.

### OQ-5.5 — Stabilité temporelle pour entrer dans la branche high-mastery

Le pseudo-code §4 entre en `selectHighMasteryAction` dès que
`PMastery >= 0.85`, sans contrainte de stabilité. La spec architecture
disait « mastery ≥ 0.85 + temps stable ». Que veut dire « temps stable » ?

**A.** Pas de contrainte temporelle — dès que PMastery franchit 0.85,
on est éligible à MasteryChallenge. Risque : oscillation autour de 0.85
(0.86 → MasteryChallenge → échec → 0.84 → branche Practice ZPD →
succès → 0.86 → re-MasteryChallenge → ...). Ping-pong.

**B.** Exiger N interactions consécutives au-dessus de 0.85 avant
d'entrer (par ex. N=3). Le caller passe ces N dans `ActionHistory`.
Pas d'oscillation.

**C.** Exiger un temps écoulé depuis la dernière interaction (e.g.
24h) ET PMastery >= 0.85. Combine "récemment maîtrisé" et "consolidé
par sommeil".

**Mon défaut** : **B** (N=3). Simple, déterministe, évite le ping-pong.
Un compteur supplémentaire dans `ActionHistory` (`InteractionsAboveBKT
int`) que le caller dérive depuis l'historique.

### OQ-5.6 — Défense contre `NaN`/`nil` (cf F-1.3 dette différée)

`cs.PMastery = NaN` est observable si F-1.3 (BKT division par zéro)
n'est pas corrigé. Que fait `[5]` ?

**A.** Aucune défense. NaN tombe dans le default (high-mastery) →
MasteryChallenge sur un concept qui n'a probablement pas été maîtrisé.
Mauvais.

**B.** Garde explicite : `if math.IsNaN(cs.PMastery) || math.IsNaN(cs.Theta)
{ return Action{Type: ActivityRest, Rationale: "concept_state corrupted"} }`.
Échec gracieux. Coût : 3 lignes.

**C.** Logger la corruption et tomber sur un fallback "Practice à
difficulté 0.55". Compromis entre A et B.

**Mon défaut** : **B**. Coût trivial, signal clair en log, évite la
décision fausse. Note inline pointant vers F-1.3 dans la dette.

---

## 10. Plan de PR

### 10.1 Fichiers touchés

| Action | Fichier | Notes |
|--------|---------|-------|
| **Création** | `engine/action_selector.go` | `SelectAction`, `zpdDifficultyFromTheta`, types `Action` et `ActionHistory` |
| **Création** | `engine/action_selector_test.go` | ~250 lignes (cascade + ZPD + dégénérés) |
| **Modif** | `models/domain.go` | ajouter constantes `ActivityPractice`, `ActivityDebugMisconception`, `ActivityFeynmanPrompt`, `ActivityTransferProbe` |
| **Modif** | `tools/prompt.go` | ajouter un appendix `actionSelectorAppendix` documentant les 4 nouvelles activity types quand `REGULATION_ACTION=on` |
| **Câblage courant** | `engine/orchestrator.go` | l'orchestrateur appelle `SelectAction`; `REGULATION_ACTION` reste limité à l'appendix prompt. |
| **Pas modifié** | tests existants | aucune fixture touchée |

### 10.2 Critères de merge

- [ ] `go test ./...` PASS sans flag
- [ ] `REGULATION_ACTION=on go test ./...` PASS (n'a pas d'effet runtime mais rajoute la doc d'appendix au prompt)
- [ ] `REGULATION_THRESHOLD=off go test ./...` PASS (vérifie que la branche high-mastery utilise bien `MasteryBKT()` accesseur, pas un literal)
- [ ] Aucune mention de `0.85` literal dans `engine/action_selector.go` (drift test du composant `[7]` doit toujours passer)
- [ ] Tests OQ-5.6 (NaN guard) présents et verts

### 10.3 Historique de périmètre

- Le câblage runtime a été réalisé ensuite par l'orchestrateur de phase.
- `REGULATION_ACTION` n'a pas été transformé en kill switch runtime :
  il reste un flag d'appendix prompt.
- La documentation utilisateur des ActivityType est portée par le
  README et par `tools/prompt.go`.

### 10.4 Test d'intégration goal-aware (rappel)

Le test exigé par Q6 (apprenant simulé sur 20 sessions, comparaison
goal-uniform vs goal-relevant) est **PR [4] ConceptSelector** —
ActionSelector seul ne consomme pas `goal_relevance`.

---

## 11. Récapitulatif

| Aspect | Décision |
|--------|----------|
| **Signature** | `SelectAction(concept, *ConceptState, *Misconception, ActionHistory) Action` — fonction pure |
| **Algorithme** | Cascade : misconception → retention → mastery brackets → high-mastery rotation |
| **Activity types nouveaux** | `PRACTICE`, `DEBUG_MISCONCEPTION` (OQ-5.1), `FEYNMAN_PROMPT`, `TRANSFER_PROBE`. Total 4 (Q4 listait `DIAGNOSTIC_PROBE` aussi mais c'est `[2]`) |
| **ZPD formula** | `sigmoid(θ - 0.847)` clampée à `[0.30, 0.85]` |
| **Misconception > retention** | OQ-5.4 = A |
| **Stabilité high-mastery** | OQ-5.5 = B (N=3 interactions au-dessus de seuil) |
| **NaN guard** | OQ-5.6 = B (Rest fallback) |
| **Flag** | `REGULATION_ACTION=off` retire seulement l'appendix prompt ; `SelectAction` reste exécuté par l'orchestrateur |
| **Findings résolus** | F-3.1 (θ → DifficultyTarget), F-3.5 (Feynman/Transfer routables serveur via l'orchestrateur) |

---

**STOP.** Design `[5] ActionSelector` complet. En attente de validation,
ou amendements sur les 6 décisions ouvertes (OQ-5.1 enum DEBUG_MISCONCEPTION,
OQ-5.2 stratégie de rotation, OQ-5.3 formule ZPD, OQ-5.4 précédence
misconception/retention, OQ-5.5 stabilité high-mastery, OQ-5.6 NaN
guard). Composant suivant : `[4] ConceptSelector`.
