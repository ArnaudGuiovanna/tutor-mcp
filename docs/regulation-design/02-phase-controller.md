# [2] PhaseController — Design (Phase 1)

> **Archive de conception — non normative.** Les passages sur
> `REGULATION_PHASE`, `engine.Route` et `engine/router.go` documentent une
> migration désormais terminée. Le pipeline supporté est toujours actif dans
> `engine/orchestrator.go`; le code, les schémas MCP, `README.md` et
> `OPERATIONS.md` font foi.

> Voir aussi `docs/regulation-design/00-pipeline-vs-legacy.md` pour le mapping des 7 priorités du router legacy vers le pipeline orchestrateur (audit #15).

> Composant 6/7 du pipeline de régulation. **L'orchestrateur** : porte
> trois rôles distincts qui se composent — FSM des phases, runtime
> coordinator du pipeline, layer de migration vis-à-vis du router
> legacy. C'est le composant le plus risqué du MVP, parce que c'est
> ici que `[3]/[4]/[5]/[7]/[1]` cessent d'être des fonctions pures
> isolées et deviennent un système.
>
> Référence architecture : `docs/regulation-architecture.md` §3 [2],
> Q1 (FSM), Q6 (ordre), dette différée §C (migration legacy).

---

## 1. Nature du composant

`[2] PhaseController` n'est PAS une fonction pure — c'est un
orchestrateur avec accès store. Trois rôles à séparer
mentalement :

### Rôle A — FSM des phases (pur)

Une fonction *pure* qui, à partir d'un `PhaseState` et d'un
`PhaseObservables` (snapshot des états + goal_relevance + interactions
récentes), retourne la phase suivante :

```go
func EvaluatePhase(current models.Phase, obs PhaseObservables) (next models.Phase, transitioned bool, rationale string)
```

Pas de DB ici. Testable en isolation comme `[4]`/`[5]`.

### Rôle B — Runtime coordinator (impur)

Une fonction *avec store* qui :

1. Lit la phase courante depuis la DB.
2. Pré-fetch les inputs nécessaires aux composants.
3. Évalue le FSM (rôle A).
4. Persiste la phase si transition.
5. Appelle `[3] Gate` → `[4] ConceptSelector` → `[5] ActionSelector`.
6. Retourne un `models.Activity` (compat avec router legacy).

```go
func Orchestrate(store *db.Store, input OrchestratorInput) (models.Activity, error)
```

### Rôle C — Layer de migration (entry point)

Le toggle `REGULATION_PHASE=on` détermine si `tools/activity.go`
appelle `Orchestrate` (nouveau) ou `Route` (legacy). Ce toggle est
fait à l'entrée du `get_next_activity` MCP tool, *pas* dans
`Orchestrate` lui-même (séparation claire des responsabilités).

### Pourquoi `[2]` arrive en dernier dans l'ordre `[7] → [1] → [5] → [4] → [3] → [2]`

Architecturalement : tous les composants amont sont des fonctions
pures testables. `[2]` les compose. Le tester nécessite que `[3]`,
`[4]`, `[5]` soient validés indépendamment (déjà fait). L'ordre Q6
n'est pas une coïncidence — c'est la stratégie de réduction du
risque d'intégration.

---

## 2. Signaux consommés / état

### Lecture (depuis store)

| Source | Champ | Usage |
|--------|-------|-------|
| `domains` | `phase` (nouvelle colonne) | État FSM courant |
| `domains` | `phase_changed_at` (nouvelle colonne) | Borne temporelle de la couverture diagnostique qualifiée |
| `domains` | `goal_relevance_json` | Critères INSTRUCTION→MAINTENANCE et MAINTENANCE→INSTRUCTION |
| `concept_states` (par learner × domaine) | `PMastery`, `PSlip`, `PGuess`, `Stability`, `ElapsedDays`, `CardState` | Entropie, mastery, retention |
| `interactions` + `assessment_attempts` | learner × domain × fenêtre de phase, `DIAGNOSTIC_ASSESSMENT`, sans hint, tentative évaluée | `COUNT(DISTINCT concept)` (rôle FSM) ; recent concepts (rôle Gate) |
| `interactions` | tag `misconception_type` | Map des misconceptions actives via `db.GetActiveMisconceptions` |
| Alerts | `engine.ComputeAlerts(...)` (déjà appelé en aval de `get_next_activity`) | Pour `[3] Gate` (OVERLOAD, FORGETTING) |

### Écriture (vers store)

| Cible | Quand |
|-------|-------|
| `domains.phase` | Lors d'une transition FSM |
| `domains.phase_changed_at` | Idem (timestamp UTC) |

Pas d'écriture sur `interactions`, `concept_states` — `[2]` lit
seulement (les écritures restent dans `record_interaction` côté
runtime).

### État éphémère (en cours de session, pas en DB)

- `sessionConcepts map[string]int` — déjà géré par le caller
  (`tools/activity.go`). `[2]` consomme.
- Time-of-call — passé via `OrchestratorInput.Now` pour la
  testabilité.

---

## 3. FSM — états et transitions

### États (3)

| État | Sémantique | Comportement attendu |
|------|------------|----------------------|
| `DIAGNOSTIC` | Cartographier le state, P(L) initial peu informé | `[4]` route via info-gain ; pas de filter prereq dans `[3]` |
| `INSTRUCTION` (défaut) | Apprentissage actif, frange externe | `[4]` route via `goal_relevance × (1-mastery)` |
| `MAINTENANCE` | Concepts goal-relevants maîtrisés, vigilance contre l'oubli | `[4]` route via `(1-retention) × goal_relevance` |

### Diagramme

```
       ┌────────────┐
       │ DIAGNOSTIC │  initial pour nouveaux domaines (flag on)
       └─────┬──────┘
             │ couverture qualifiée distincte >= min(|concepts|, 8)
             ▼
       ┌────────────┐
       │INSTRUCTION │ ◀──┐  défaut pour anciens domaines (NULL phase)
       └─────┬──────┘    │
             │           │
             │ ∀ goal-relevant : │ ∃ goal-relevant :
             │   mastery >= 0.85 │   retention < seuil
             ▼           │
       ┌────────────┐    │
       │MAINTENANCE │────┘
       └────────────┘
```

### Table de transitions

| From | To | Condition | Calcul |
|------|----|-----------|--------|
| DIAGNOSTIC | INSTRUCTION | `qualified_distinct_concepts >= min(domain_concept_count, N_DIAG_MAX)` | JOIN interaction/tentative évaluée, sans hint; l'entropie est conservée pour l'audit |
| INSTRUCTION | MAINTENANCE | `∀ c ∈ goal_relevant_concepts : PMastery(c) >= MasteryBKT()` | `goal_relevant` : voir OQ-2.7 |
| MAINTENANCE | INSTRUCTION | `∃ c ∈ goal_relevant_concepts : Retrievability(c) < RETENTION_RECALL_THRESHOLD` | Retention : `algorithms.Retrievability` |

Pas de transition cyclique non décrite (e.g. INSTRUCTION → DIAGNOSTIC
re-diagnostic ne fait pas partie du MVP). Si l'apprenant repart de
zéro sur un domaine, la voie est : archive_domain + new domain.

### Initialisation

| Condition | Phase initiale |
|-----------|----------------|
| Domaine créé avec `REGULATION_PHASE=on` au moment du `init_domain` | DIAGNOSTIC |
| Domaine existant (créé avant le flag), `phase` colonne NULL | INSTRUCTION (default fallback, voir OQ-2.1 sub-b) |
| Domaine créé avec `REGULATION_PHASE=off` puis flag activé | INSTRUCTION (NULL → INSTRUCTION, pas de migration rétroactive vers DIAGNOSTIC) |

Justification : pas de re-diagnostic rétroactif sur les domaines
existants (qui ont déjà accumulé des observations). Cohérent avec
la politique de migration *« opt-in pour les nouveaux »*.

---

## 4. Décision produite

### Output public

```go
// Orchestrate retourne le models.Activity pour rester compat avec le
// router legacy. Le rationale incorpore la phase utilisée pour audit.
func Orchestrate(store *db.Store, input OrchestratorInput) (models.Activity, error)
```

### Diagnostic interne (échangé par les sous-fonctions)

```go
type PhaseEvaluation struct {
    From         models.Phase
    To           models.Phase
    Transitioned bool
    Rationale    string
}

type PipelineResult struct {
    Activity     models.Activity
    GateResult   GateResult        // pour audit
    Selection    Selection
    Action       Action
}
```

L'`Activity` retournée par `Orchestrate` au caller est juste l'output
de `[5]` *enveloppé* avec `Concept` (de `[4]`) et `PromptForLLM` (que
l'orchestrateur compose à partir de `Action.Format` + le concept). La
couche `tools/activity.go` post-traite ensuite normalement (tutor_mode,
calibration_bias, motivation_brief, etc. — toutes les couches
existantes restent valides).

### Cas EscapeAction du Gate

Si `[3] Gate` retourne `EscapeAction != nil`, `Orchestrate`
court-circuite `[4]/[5]` et retourne directement un `models.Activity`
construit depuis l'EscapeAction (Type=ActivityCloseSession, etc.).

### Cas NoCandidate du Gate

`[3]` peut retourner `NoCandidate=true`. Deux sources possibles :

1. **Anti-rep + prereq filtre tout** : `[2]` doit décider quoi faire.
   Politique : émettre `ActivityRest` avec rationale "no_candidate
   from gate" (aujourd'hui). Le LLM lit la session_history et propose
   continuation/exit. **Pas de bascule de phase ici** — c'est un
   problème de contraintes intra-phase, pas un problème de phase.

2. **Pool goal_relevant vide** : ne devrait pas arriver à ce niveau
   (la frange externe de `[4]` retourne `NoFringe`, qui est un signal
   de `[4]`, pas de `[3]`). Si `[4]` retourne `NoFringe`, `[2]`
   considère une bascule de phase :
   - INSTRUCTION + NoFringe (tout maîtrisé) → bascule MAINTENANCE
     immédiate, retry (rare — couvert par le prochain appel naturel).
   - MAINTENANCE + NoFringe (rien maîtrisé) → bascule INSTRUCTION
     immédiate, retry.

Pour limiter la complexité : le retry est *one-shot* (max 1
re-évaluation FSM dans le même `Orchestrate`). Si le 2e appel échoue
encore, retourne `ActivityRest` + rationale.

---

## 5. Algorithme — orchestrateur runtime

### Pseudo-code

```
Orchestrate(store, input):
    domain = store.GetDomain(input.DomainID)
    if domain == nil: return error("unknown domain")
    
    currentPhase = domain.Phase
    if currentPhase == "": currentPhase = INSTRUCTION  // legacy fallback
    
    # 1. Évaluer le FSM
    obs = fetchPhaseObservables(store, input.LearnerID, input.DomainID,
                                domain.PhaseChangedAt, input.Now)
    eval = EvaluatePhase(currentPhase, obs)  // pure
    if eval.Transitioned:
        store.UpdateDomainPhase(input.DomainID, eval.To, input.Now)
        currentPhase = eval.To
    
    # 2. Pipeline (max 1 retry sur NoFringe)
    for retry = 0; retry < 2; retry++:
        result, err = runPipeline(store, input, currentPhase)
        if err != nil: return err
        if result.IsNoFringe:
            # FSM doit basculer
            nextPhase = noFringeFallbackPhase(currentPhase)
            if nextPhase == currentPhase or retry >= 1:
                return Activity{Type: REST, Rationale: "no_candidate" }
            store.UpdateDomainPhase(input.DomainID, nextPhase, input.Now)
            currentPhase = nextPhase
            continue
        return result.Activity
    
    return Activity{Type: REST, Rationale: "pipeline_exhausted" }


runPipeline(store, input, phase):
    # Pré-fetch
    states = store.GetConceptStatesByLearner(input.LearnerID)  // filtré domaine
    graph = domain.Graph
    goalRelevance = parseGoalRelevance(domain.GoalRelevanceJSON)
    activeMisc = store.GetActiveMisconceptionsBatch(input.LearnerID, domain.Concepts)
    recentConcepts = store.GetRecentConceptsByDomain(input.LearnerID, domain.ID, 10)
    alerts = engine.ComputeAlerts(states, recent...)  // déjà fait dans le runtime actuel
    
    # 1. Gate
    gateResult, err = engine.ApplyGate(GateInput{
        Phase: phase,
        Concepts: graph.Concepts,
        States: stateMap,
        Graph: graph,
        ActiveMisconceptions: activeMisc,
        RecentConcepts: recentConcepts,
        Alerts: alerts,
        AntiRepeatWindow: engine.DefaultAntiRepeatWindow,
    })
    if err: return err
    if gateResult.EscapeAction != nil:
        return wrapEscape(gateResult.EscapeAction)
    if gateResult.NoCandidate:
        return PipelineResult{IsNoFringe: true}  // signaler au caller
    
    # 2. ConceptSelector — restreint au pool autorisé
    filteredGraph = filterGraphTo(graph, gateResult.AllowedConcepts)
    selection, err = engine.SelectConcept(phase, states, filteredGraph, goalRelevance)
    if err: return err
    if selection.NoFringe:
        return PipelineResult{IsNoFringe: true}
    
    # 3. ActionSelector — sur le concept choisi
    cs = stateMap[selection.Concept]
    var mc *db.MisconceptionGroup
    if activeMisc[selection.Concept]:
        mc = fetchMisconception(store, input.LearnerID, selection.Concept)
    history = deriveActionHistory(store, input.LearnerID, selection.Concept)
    action = engine.SelectAction(selection.Concept, cs, mc, history)
    
    # 4. Honorer les ActionRestriction du Gate
    if restrictions, ok := gateResult.ActionRestriction[selection.Concept]; ok:
        if !contains(restrictions, action.Type):
            # Override : forcer la première restriction (typiquement DEBUG_MISCONCEPTION)
            action = forceActionType(restrictions[0], cs, mc)
    
    # 5. Composer l'Activity finale
    return PipelineResult{
        Activity: composeActivity(action, selection, phase),
    }
```

### Notes d'algorithme

- **Le filtrage du graphe pour `[4]`** : on construit un sous-graphe
  qui ne contient que `gateResult.AllowedConcepts` (pour que le tri
  alphabétique de `[4]` ne réintègre pas un concept filtré par le
  Gate).
- **L'override d'`ActionRestriction`** : `[5]` priorise déjà
  misconception via son override interne, donc en pratique l'action
  émise sera *déjà* `DEBUG_MISCONCEPTION` quand restriction l'impose.
  L'override explicite est une ceinture+bretelles défensive.
- **`fetchMisconception`** : récupère le premier misconception actif
  pour le concept (status="active", ordre par count desc). API à
  ajouter sur le Store si elle n'existe pas (cf §11).
- **`deriveActionHistory`** : interroge `interactions` pour compter
  les MasteryChallenge / Feynman / Transfer émis sur le concept *et*
  les interactions consécutives au-dessus de seuil. À ajouter (helper
  store-level).

---

## 6. Migration — `REGULATION_PHASE=on`

### Entry point

Dans `tools/activity.go`, là où `engine.Route(...)` est appelé
aujourd'hui :

```go
var activity models.Activity
if regulationPhaseEnabled() {
    activity, err = engine.Orchestrate(store, engine.OrchestratorInput{
        LearnerID: learnerID,
        DomainID:  domainID,
        Now:       time.Now().UTC(),
    })
} else {
    activity = engine.Route(alerts, frontier, states, recentInteractions, sessionConcepts)
}
```

Le legacy `Route` reste intouché tant que `REGULATION_PHASE != "on"`.
Strict equality opt-in (cohérent avec les autres flags
`REGULATION_*`).

### Compatibilité descendante

| Cas | Comportement |
|-----|--------------|
| Domaine créé avant le flag, flag jamais activé | Legacy `Route`. Aucun changement. |
| Domaine créé avant le flag, flag activé | Phase NULL → INSTRUCTION fallback. Pipeline tourne en INSTRUCTION. Pas de re-diagnostic rétroactif. |
| Domaine créé après activation du flag | Phase initialisée à DIAGNOSTIC dans `init_domain`. |
| Flag désactivé en cours de session | Le prochain `get_next_activity` route via legacy. La phase persiste en DB mais n'est plus consultée. |
| Flag réactivé | Lecture de la phase en DB ; si elle existe, le pipeline reprend là où elle est. |

### Idempotence off → on → off → on

La séquence est sûre car :

1. La phase en DB est *purement informative* en mode legacy (pas
   lue, pas écrite par `Route`).
2. `Orchestrate` lit toujours la phase fraîche en DB, ne cache rien
   au niveau session.
3. `[5]/[4]/[3]` sont des fonctions pures — re-appelables sans état
   résiduel.
4. La FSM est ré-évaluée à chaque appel (ré-évaluation = lazy, voir
   OQ-2.5).

### Migration de la DB

`docs/regulation-architecture.md` §C exige une *idempotent migration*.
Schéma :

```sql
ALTER TABLE domains ADD COLUMN phase TEXT;
ALTER TABLE domains ADD COLUMN phase_changed_at TIMESTAMP;
```

NULL par défaut sur les anciens domaines. Le code applicatif lit NULL
comme INSTRUCTION (fallback documenté). Ajout via la même mécanique
que les autres ALTER de la PR `[1]` (`db.applyMigrations` — fichier
existant).

---

## 7. Cas dégénérés

| Cas | Comportement | Garantie |
|-----|---------------|----------|
| `domain_id` inconnu | Erreur `"unknown domain"` | Caller gère (devrait pas arriver) |
| Phase NULL en DB | INSTRUCTION fallback (cohérent avec migration) | Documenté |
| Phase invalide en DB (chaîne corrompue) | `slog.Error` + INSTRUCTION fallback (au cas d'écriture corrompue ; sécurité défensive — *différent* de `ApplyGate` qui rejette ; ici on a une session live à servir) | Voir OQ-2.5 |
| `goal_relevance_json` corrompu | Fallback uniforme (cohérent avec `ParseGoalRelevance` de `[1]`) | Couvert |
| `concept_states` vide (apprenant débute) | INSTRUCTION default ; `[4]` peut retourner `NoFringe` ; `Orchestrate` retourne `ActivityNewConcept` sur la frange ou `REST` si vide. | OK |
| Pipeline retourne `NoFringe` 2× (retry échoue) | `ActivityRest` + rationale "pipeline_exhausted" | Borné |
| Erreur DB pendant `UpdateDomainPhase` | `slog.Error` + continue (la transition est *informative*, pas critique pour servir l'activité) | Tradeoff : perte de l'historique de transition vs perte de la session |
| FSM transition vers la même phase | `Transitioned=false`, no-op write | OK |
| Toutes les sources vides (états + alerts + interactions) | INSTRUCTION + `[3]` `NoCandidate` → retry → INSTRUCTION → `[4]` `NoFringe` → REST | Caller gère |
| `Now` zéro (test mal configuré) | `phase_changed_at` valide, juste UTC zéro — pas de panic | OK |

---

## 8. Stratégie de test

### 8.1 Unit — FSM (pure, `EvaluatePhase`)

```go
TestEvaluatePhase_DiagnosticToInstruction_EntropyBelowThreshold
TestEvaluatePhase_DiagnosticToInstruction_NItemsReached
TestEvaluatePhase_DiagnosticToInstruction_NeitherCondition_Stays
TestEvaluatePhase_InstructionToMaintenance_AllGoalMastered
TestEvaluatePhase_InstructionToMaintenance_OneGoalNotMastered_Stays
TestEvaluatePhase_MaintenanceToInstruction_OneGoalRetentionLow
TestEvaluatePhase_MaintenanceToInstruction_AllRetentionHigh_Stays
TestEvaluatePhase_NoTransitionPath  // e.g. INSTRUCTION → DIAGNOSTIC
TestEvaluatePhase_GoalRelevanceNil_TreatsAllAsRelevant
TestEvaluatePhase_GoalRelevanceUncoveredConcepts_IgnoredOrIncluded  // OQ-2.7
```

### 8.2 Unit — pipeline (impur, mais sqlite in-memory)

```go
TestRunPipeline_GateEscape_ShortCircuits
TestRunPipeline_GateNoCandidate_PropagatesSignal
TestRunPipeline_NormalFlow_Gate_Concept_Action
TestRunPipeline_ActionRestriction_OverridesActionType
TestRunPipeline_FilteredGraphPassedToConceptSelector
```

### 8.3 Integration — orchestrateur full

```go
TestOrchestrate_E2E_DiagnosticToInstruction
TestOrchestrate_E2E_InstructionToMaintenance
TestOrchestrate_E2E_MaintenanceToInstruction
TestOrchestrate_NoFringeRetryOneShot
TestOrchestrate_PhaseInvalidInDB_FallsBackToInstruction
TestOrchestrate_DomainPhaseNull_DefaultsToInstruction
```

### 8.4 Migration

```go
TestEntryPoint_FlagOff_UsesLegacyRouter
TestEntryPoint_FlagOn_UsesOrchestrator
TestEntryPoint_FlagFlipMidSession_NoCorruption
TestEntryPoint_OffOnOff_Idempotent
TestMigration_AddPhaseColumn_Idempotent
```

### 8.5 Artefact lisible (exigence utilisateur)

Le test E2E sur 30 sessions doit produire un artefact en plus du
PASS/FAIL Go, pour servir d'observation système et de base de
calibration future. Format double :

- **JSON structuré** (`eval/orchestrator_e2e_<scenario>_<date>.json`)
  : per-session entries avec `session_num`, `phase_before`,
  `phase_after`, `transitioned`, `mean_entropy`, `phase_entry_entropy`,
  `mastered_count`, `mastered_goal_count`, `activity_emitted`,
  `concept`, `learner_response_correct`. Machine-readable pour
  scripts d'analyse.
- **Markdown summary** (`eval/orchestrator_e2e_<scenario>_<date>.md`)
  : tableau lisible humain avec les transitions principales et un
  résumé final par phase.

Les fichiers sont écrits par le test lui-même via `t.TempDir()` puis
copiés en `eval/` pour persistance. Si le test passe, l'artefact est
disponible. Si le test échoue, l'artefact est inspectable pour
debug.

### 8.6 Apprenant simulé sur N sessions

Le test d'intégration end-to-end exigé par la cadrage :

```go
TestOrchestrate_SimulatedLearner_30Sessions(t *testing.T) {
    // 1. Setup : sqlite in-memory, learner + domaine 5 concepts +
    //    prereqs simples + goal_relevance.
    // 2. Boucle 30 fois :
    //    a. Orchestrate → Activity
    //    b. Simuler une réponse (correct selon mastery cible)
    //    c. Record interaction (BKT/FSRS update normal)
    // 3. Assertions :
    //    - Phase commence DIAGNOSTIC
    //    - Phase passe INSTRUCTION quand entropy ou N items atteint
    //    - Phase passe MAINTENANCE quand tous goal-relevants mastered
    //    - Aucune erreur le long du parcours
    //    - phase_changed_at est cohérent avec les transitions
}
```

C'est le test critique de cette PR — il valide *toute* l'intégration
[3]+[4]+[5]+[1]+[7] sous l'orchestrateur. Si ça passe, on a très haute
confiance que le pipeline fonctionne en prod.

### 8.7 Régression legacy

```go
TestLegacyRoute_AllExistingTests  // déjà existants — doivent passer
```

Aucun changement à `engine/router.go` ; aucun test legacy ne devrait
casser. Vérifier que `go test ./engine/ -run "TestRoute"` passe avec
flag off.

---

## 9. Interaction amont/aval

### Amont (callers)

- **`tools/activity.go`** (`get_next_activity` MCP tool handler) :
  l'unique caller. Branchement par flag.

### Aval (callees)

- **`db.Store`** : lectures (`GetDomain`, `GetConceptStatesByLearner`,
  `GetRecentInteractionsByLearner`, `GetActiveMisconceptions`) et
  écriture (`UpdateDomainPhase`, à ajouter).
- **`engine.ApplyGate`** ([3])
- **`engine.SelectConcept`** ([4])
- **`engine.SelectAction`** ([5])
- **`engine.ComputeAlerts`** (existant, pour pré-fetch des alerts)
- **`algorithms.Retrievability`** (déjà utilisé par alert.go,
  réutilisé pour la transition MAINTENANCE→INSTRUCTION)

### Pas modifié

- `engine/router.go` (legacy) — coexiste, pas touché.
- `engine/alert.go` — réutilisé tel quel.
- `tools/activity.go` — modifié seulement à l'entry point pour le
  flag.

---

## 10. Décisions ouvertes (toutes arbitrées)

> Bloc d'arbitrage final. Chaque OQ porte le défaut Phase 1 *et* la
> décision validée + raffinements demandés par l'utilisateur.

### OQ-2.1 — Stockage de la phase + initialisation

(a) **Schéma DB** :

- **A.** Deux colonnes sur `domains` : `phase TEXT`, `phase_changed_at
  TIMESTAMP`. Simple, atomique avec le domaine, lecture sans JOIN.
- **B.** Table dédiée `domain_phases (domain_id, phase, started_at)`
  avec INSERT à chaque transition. History native, mais lecture
  nécessite ORDER BY DESC LIMIT 1 — coût mineur, complexité
  conceptuelle plus élevée.
- **C.** JSONB column `phase_state` sur `domains`. Flexible, mais
  pas de typage et requêtes plus lourdes pour le calcul de
  la couverture diagnostique qualifiée.

**Mon défaut** : **A**. SQLite n'a pas JSONB natif puissant ; deux
colonnes typées sont plus simples à debug, à requêter, à migrer. Le
besoin d'historique est faible au MVP — on peut l'ajouter en `B` plus
tard sans casser `A`.

(b) **Initial value pour nouveaux domaines avec flag on** :

- **A.** DIAGNOSTIC. La phase initiale signifie *« on ne sait pas
  encore »* — cohérent avec un nouvel apprenant.
- **B.** INSTRUCTION. Bypass DIAGNOSTIC, direct au routage usuel.
  Plus simple mais perd le bénéfice de la phase de diagnostic.

**Mon défaut sub** : **A** pour les domaines créés *avec* le flag on ;
INSTRUCTION (NULL → fallback) pour les domaines existants. Cohérent
avec l'opt-in *« nouveaux uniquement »*.

**Décision validée** : **A** sur les deux. Test explicite ajouté à
la matrice de migration : un domaine pré-existant (créé avant le
flag) NE doit PAS se retrouver en DIAGNOSTIC erroné après promotion
du flag — il reste en INSTRUCTION via le fallback NULL → INSTRUCTION.
Test `TestMigration_PreExistingDomain_StaysInInstruction`.

### OQ-2.2 — Calcul de l'entropie globale du domaine

Pour la transition DIAGNOSTIC → INSTRUCTION, on a besoin d'un score
*« incertitude résiduelle »*.

**A.** **Mean binary entropy de P(L) sur tous les concepts** :
`mean over c in graph.Concepts of H(P(L_c))` où `H(p) = -p log2 p -
(1-p) log2 (1-p)`. Threshold par défaut : 0.5 bits/concept moyen.
Indépendant de Slip/Guess. **Mon défaut.**

**B.** **Réutiliser `BKTInfoGain` moyenné** : `mean BKTInfoGain(cs)`.
Capture le bénéfice attendu d'observer la prochaine réponse,
incorpore Slip/Guess. Plus complexe — on diagnostique pour réduire
P(L) uncertainty, pas pour maximiser l'info-gain attendu.

**C.** **Max H(P(L_c))** au lieu de mean. Stricter : exit DIAGNOSTIC
seulement quand AUCUN concept ne reste très incertain. Bénéfice :
diagnostic plus complet ; coût : risque d'épuiser N_DIAG_MAX souvent.

**Mon défaut** : **A**. Mean binary entropy. Threshold 0.5 — un
choix qu'on pourra ajuster avec l'eval. Justification :
l'incertitude *globale* du domaine est le signal pertinent, pas
l'incertitude max ou l'info-gain attendu.

**Décision révisée (learning integrity)** : l'entropie reste un signal
d'audit/calibration, mais ne peut plus terminer le diagnostic. Un événement ne
compte que s'il s'agit d'un `DIAGNOSTIC_ASSESSMENT` sans hint, lié à une
tentative soumise puis évaluée. Le compteur porte sur les concepts distincts :

```
exit DIAGNOSTIC quand :
  qualified_distinct_concepts >= min(domain_concept_count, NDiagnosticMax)
```

Justification :

1. **Couverture réelle.** Huit répétitions sur un même concept ne peuvent pas
   simuler la cartographie d'un domaine.
2. **Protocole froid vérifiable.** Une observation libre, une introduction ou
   une réponse aidée ne consomme pas la couverture.
3. **Borne explicite.** Tous les concepts sont couverts jusqu'à huit; les
   domaines plus larges exigent huit concepts distincts.
4. **Entropie conservée pour l'audit.** La colonne
   `phase_entry_entropy REAL` permet toujours de mesurer l'information gagnée
   sans créer de raccourci d'intégrité.

**Constantes par défaut** : `DeltaHThreshold = 0.2 bits`,
`NDiagnosticMax = 8`.

**Edge cases** :
- `add_concepts` mid-DIAGNOSTIC : nouveau concept à P(L)=0.1
  (entropie ≈ 0.47), proche du baseline → effet borné par le
  facteur 1/N. Acceptable, à observer dans l'artefact E2E.
- `phase_entry_entropy IS NULL` : la couverture qualifiée reste suffisante;
  seul le détail de rationale entropique est omis.

### OQ-2.3 — Stratégie de migration : flag global vs per-learner

**A.** **Flag global `REGULATION_PHASE=on`** : tous les learners
affectés. Simple, pas de table de "feature flags per user", pas de
canary granulaire.

**B.** **Per-learner flag** : table `learner_flags` ou colonne sur
`learners`. Permet un canary 10/50/100 % au déploiement. Coût :
table, queries, UI admin pour basculer.

**C.** **Per-domain flag** : encore plus granulaire. Justifié si on
suspectait des comportements pathologiques sur des domaines
spécifiques.

**Mon défaut** : **A** au MVP. La complexité d'un canary granulaire
n'est pas justifiée tant que le flag est validé en pré-prod. On
peut migrer en **B** plus tard, sans casser **A** (le flag global
agit en OR avec le per-learner).

**Décision validée** : **A**. Tests de rollback off→on→off
*observable côté apprenant* requis : pas seulement la cohérence
interne du store, mais l'output réel d'`Activity` retourné à
chaque bascule, pour vérifier qu'aucun comportement aberrant
n'apparaît. Test `TestEntryPoint_OffOnOff_ApprenantObservable`.

### OQ-2.4 — Sessions en cours pendant la bascule du flag

**A.** **Re-évaluer à chaque appel `get_next_activity`** : pas de
cache, le flag est lu à chaque entrée. **Mon défaut** : si flag bascule
mid-session, le prochain appel utilise le mode actif. La cohérence
intra-session n'est pas garantie mais la cohérence par-appel l'est.

**B.** **Cache session-level** : à `record_session_close`, on capture
le mode actif au début de la session et le maintient jusqu'à la fin.
Plus complexe (nécessite un session_state), mais cohérence.

**C.** **Flag immuable post-démarrage** : ne re-lit pas, valeur
chargée au boot du process. Évite la bascule entièrement — perte de
flexibilité.

**Mon défaut** : **A**. Pas de cache. Simple. Si le ops bascule le
flag mid-session, l'apprenant peut subir un changement de routeur,
mais l'état (DB) reste cohérent — `[3]/[4]/[5]/legacy` opèrent tous
sur la même DB. Pas de corruption possible.

**Décision validée** : **A**. Confirmé.

### OQ-2.5 — FSM eager vs lazy : quand re-évaluer ?

**A.** **Eager — transitions calculées + persistées au moment de
chaque event** : à chaque `record_interaction`, ré-évaluer la phase
et persister. Avantage : phase à jour en permanence. Coût : couplage
fort entre `record_interaction` et le FSM.

**B.** **Lazy — la phase est ré-évaluée seulement à l'entrée de
`Orchestrate`** (au début de chaque `get_next_activity`). La
persistance est *au plus* 1 transition par appel. **Mon défaut.**

**C.** **Hybride — lazy à `get_next_activity`, eager-cleanup à
`record_session_close`** (re-évalue à la fin pour capturer l'effet
de la session).

**Mon défaut** : **B**. Lazy. La phase n'a aucun rôle hors de
`get_next_activity` — pas la peine de la maintenir live ailleurs.
Simplicité maximale, latence minimale par appel
`record_interaction`.

**Décision validée** : **B** (lazy à l'entrée d'`Orchestrate`).
Confirmé.

### OQ-2.6 — Seuils et constantes paramétrables

Tous les seuils sont exposés comme constantes Go privées au package
`engine` (lisibles, modifiables sans flag) :

```go
const (
    PhaseEntropyThreshold        = 0.5  // OQ-2.2 (mean H bits)
    PhaseDiagnosticItemsMax      = 8    // cadrage utilisateur
    PhaseRetentionRecallThreshold = 0.5  // aligné FORGETTING
)
```

Question : *exposer comme variables de package* (`var`, modifiable en
test via `t.Cleanup`) ou *constantes* (`const`, immuables) ?

**A.** Constantes Go. Tests utilisent les vraies valeurs, pas de
mocking nécessaire (les fixtures s'adaptent). **Mon défaut.**

**B.** Variables `var` package-level — modifiables en test via
`PhaseEntropyThreshold = 0.7; defer reset`. Plus flexible, plus
fragile.

**C.** Champs sur la struct `OrchestratorInput` — passable par appel.
Le caller doit toujours passer ; pas de défaut implicite.

**Mon défaut** : **A**. Constantes. Les tests construisent les
fixtures pour le seuil voulu, pas l'inverse. Plus auditable.

**Décision validée** : **PhaseConfig struct** (option **C** raffinée).
Pas pour ouvrir la modification runtime au MVP, mais pour
permettre :
1. **L'injection de configs alternatives en tests d'intégration**
   (un test E2E peut utiliser un `NDiagnosticMax=3` pour borner la
   couverture plus bas ; un test de calibration peut explorer plusieurs
   `DeltaHThreshold`).
2. **Le logging de la config au démarrage du serveur** — un opérateur
   voit immédiatement les seuils actifs.
3. **L'extensibilité future per-domain sans refonte** — si on
   souhaite un jour des seuils différents par domaine (taille,
   expertise), la struct est déjà là.

```go
// engine/phase_config.go
type PhaseConfig struct {
    DeltaHThreshold          float64 // OQ-2.2 : ex. 0.2 bits
    NDiagnosticMax           int     // borne de couverture distincte, ex. 8
    RetentionRecallThreshold float64 // ex. 0.5
    GoalRelevantCutoff       float64 // OQ-2.7 : ex. 0.0 (>0 strict)
}

func NewDefaultPhaseConfig() PhaseConfig {
    return PhaseConfig{
        DeltaHThreshold:          0.2,
        NDiagnosticMax:           8,
        RetentionRecallThreshold: 0.5,
        GoalRelevantCutoff:       0.0,
    }
}
```

`Orchestrate` reçoit la config en input :

```go
type OrchestratorInput struct {
    LearnerID, DomainID string
    Now                 time.Time
    Config              PhaseConfig // injection en test ; default en runtime
}
```

### OQ-2.7 — Définition de "goal-relevant" pour les transitions

INSTRUCTION → MAINTENANCE et MAINTENANCE → INSTRUCTION mentionnent
*« concepts goal-relevants »*. Quel cutoff définit le set ?

**A.** `goal_relevance[c] > 0` (toute valeur strictement positive).
Inclusif — capture l'intention de "ce concept compte un peu".

**B.** `goal_relevance[c] >= 0.5` (substantielle). Stricter — exclut
les concepts à pertinence très basse de la transition.

**C.** `goal_relevance[c] >= mean(scores)`. Relatif. Adaptatif au
domaine.

**D.** *Tous* les concepts du graphe (ignore goal_relevance).
Cohérent quand le vecteur est nil ; mais explicite, on parle de
*goal-relevant*.

Sub-question : que faire des concepts *uncovered* (absents du
vecteur, OQ-1.1) ?

- **(a)** Exclus du set "goal-relevant" (cohérent avec OQ-4.3 = B' :
  uncovered ≡ pas selectable).
- **(b)** Inclus avec relevance par défaut (cohérent avec [1] qui a
  un fallback uniforme).

**Mon défaut** : **A** + **(a)**. `goal_relevance > 0` ; uncovered
exclus. Cohérence forte avec [1] et [4]. Le cutoff > 0 est
techniquement n'importe quoi de positif, ce qui inclut les concepts
décomposés à très basse pertinence — la transition MAINTENANCE
attend qu'ils soient *aussi* mastered, ce qui peut ralentir mais
reste sémantiquement correct.

Si l'eval révèle que la transition est trop lente parce que des
concepts à 0.1 de pertinence bloquent, on relève à `>= 0.3` (B
adapté).

**Décision validée** : **A + (a)**. Test E2E à doubler avec deux
scénarios :

- **Goal restrictif** : domaine de 20 concepts, seuls 3 ont
  `goal_relevance > 0` → l'apprenant passe rapidement en
  MAINTENANCE après les 3 maîtrisés, même si 17 autres restent
  inchangés. Atteste que les non-pertinents ne bloquent pas la
  transition.
- **Goal large** : tous les 20 concepts ont `goal_relevance > 0` →
  l'apprenant reste en INSTRUCTION longtemps, jusqu'à maîtrise
  complète. Atteste que les goal-relevants tous décomposés sont
  effectivement requis.

Tests : `TestOrchestrate_E2E_RestrictiveGoal_FastMaintenance` et
`TestOrchestrate_E2E_BroadGoal_LongInstruction`.

---

## 11. Plan de PR

### 11.1 Fichiers touchés

| Action | Fichier | Notes |
|--------|---------|-------|
| **Création** | `engine/orchestrator.go` | `Orchestrate`, `runPipeline`, `EvaluatePhase`, `OrchestratorInput`, types internes (~350 lignes) |
| **Création** | `engine/orchestrator_test.go` | unit tests FSM + pipeline + cas dégénérés (~400 lignes) |
| **Création** | `engine/orchestrator_integration_test.go` | E2E avec sqlite in-memory (~300 lignes) |
| **Création** | `engine/phase_observables.go` | Helpers de pré-fetch (méthodes du Store) ou wrappers (~120 lignes) |
| **Modif** | `db/store.go` | `UpdateDomainPhase`, `GetActiveMisconceptionsBatch` (helper), si nécessaire `GetActionHistoryForConcept` |
| **Modif** | `db/migrations.go` | `ALTER TABLE domains ADD COLUMN phase TEXT`, `phase_changed_at TIMESTAMP` |
| **Modif** | `models/domain.go` | Ajouter `Phase string` et `PhaseChangedAt time.Time` au struct `Domain` |
| **Modif** | `tools/activity.go` | Branchement `regulationPhaseEnabled() ? Orchestrate(...) : Route(...)` |
| **Modif** | `tools/prompt.go` | `regulationPhaseEnabled()` + `phaseControllerAppendix` (description du FSM côté LLM si pertinent — voir §11.4) |
| **Modif** | `tools/domain.go` | Lors de `init_domain` avec flag on, set `phase=DIAGNOSTIC` |

### 11.2 Critères de merge

- [ ] `go test ./...` PASS sans flag (legacy intact)
- [ ] `REGULATION_PHASE=on go test ./...` PASS
- [ ] `REGULATION_THRESHOLD=off go test ./...` PASS (legacy thresholds)
- [ ] Test E2E sur 30 sessions valide les 3 transitions de phase
- [ ] Test idempotence flag on→off→on confirme aucune corruption
- [ ] Test legacy `Route` couvert par tous ses tests existants
- [ ] Migration DB idempotente (`ALTER ... ADD COLUMN` sur DB déjà
      migrée passe)
- [ ] Drift test `[7]` PASS (pas de literal threshold)
- [ ] `go vet ./...` PASS
- [ ] Aucun mock du Store dans les tests d'orchestrator (sqlite
      in-memory pour réalisme)

### 11.3 Pas inclus dans cette PR

- **Désactivation finale du router legacy** : reporté à une PR
  ultérieure (`[6] FadeController` Phase 2, ou plus tard). Tant que
  le legacy est testé et fonctionnel, on le garde comme ceinture.
- **Phase d'intérêt Hidi-Renninger** (Q3 architecture) : orthogonal,
  hors scope.
- **Eval bootstrap CI sur la PR `[2]`** : la cadrage utilisateur
  n'a pas mentionné d'éval similaire à `[7]` — l'E2E sur apprenant
  simulé tient lieu de validation. Si après merge on observe des
  régressions en pré-prod, on refait un harness eval comme pour
  `[7]`.

### 11.4 Faut-il un `phaseControllerAppendix` dans le system prompt ?

Différent des autres composants : `[2]` ne change pas
l'ActivityType set ni les outils MCP. Le LLM voit *exactement* la
même surface qu'avant — l'activité retournée par
`get_next_activity`, les alerts, etc.

Choix :

- **A.** Pas d'appendix. Le LLM n'a pas besoin de savoir qu'un FSM
  tourne en arrière-plan. **Mon défaut.**
- **B.** Appendix court mentionnant qu'une FSM régule les phases
  d'apprentissage et que le LLM peut influencer la phase via
  `learning_negotiation` (déjà existant). Documentation de cohérence.

Je penche pour **A** — éviter d'alourdir le prompt avec des
informations non actionnable côté LLM. Si on veut donner un signal
de phase au LLM, on l'inclut dans le `rationale` de l'Activity (ce
qui est déjà fait par `Orchestrate`).

---

## 12. Récap

| Aspect | Décision (mon défaut) |
|--------|------------------------|
| **Architecture** | 3 rôles séparés : FSM pure (`EvaluatePhase`), runtime coordinator (`Orchestrate`), entry-point migration (in `tools/activity.go`) |
| **FSM** | 3 états (DIAGNOSTIC / INSTRUCTION / MAINTENANCE) + 3 transitions définies |
| **Stockage** | OQ-2.1 = A (2 colonnes sur `domains`), DIAGNOSTIC initial pour nouveaux domaines avec flag on |
| **Entropie** | OQ-2.2 = A (mean binary entropy de P(L), seuil 0.5 bits) |
| **Migration** | OQ-2.3 = A (flag global), OQ-2.4 = A (re-évaluation par appel, pas de cache) |
| **FSM lazy/eager** | OQ-2.5 = B (lazy, ré-évaluée à `Orchestrate` entry) |
| **Seuils** | OQ-2.6 = A (constantes Go) |
| **Goal-relevant cutoff** | OQ-2.7 = A (`goal_relevance > 0`), uncovered exclus |
| **Test E2E** | Apprenant simulé sur 30 sessions, valide les 3 transitions |
| **Compatibilité** | Backward : NULL phase → INSTRUCTION ; flag off → legacy `Route` ; idempotence on/off |
| **Findings résolus** | F-3.X (le pipeline complet), F-X.X (orchestration centralisée), validation empirique de [1]/[4]/[3]/[5] sous régime intégré |

---

**STOP.** Design `[2] PhaseController` complet. En attente de validation,
ou amendements sur les 7 décisions ouvertes (OQ-2.1 stockage + initial,
OQ-2.2 formule entropie, OQ-2.3 stratégie migration, OQ-2.4 sessions
mid-bascule, OQ-2.5 FSM lazy/eager, OQ-2.6 seuils paramétrables,
OQ-2.7 cutoff goal-relevant). 

Composant suivant après `[2]` : `[6] FadeController` (Phase 2 — module
fine la verbosité des activités selon l'autonomy_score). Une fois `[2]`
mergé, `[6]` est l'optimisation finale du MVP.
