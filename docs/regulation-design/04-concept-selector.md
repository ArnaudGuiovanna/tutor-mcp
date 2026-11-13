# [4] ConceptSelector — Design (Phase 1)

> Composant 4/7 du pipeline de régulation. Reçoit un *contexte*
> (phase courante, états par concept, graphe, vecteur `goal_relevance`)
> et choisit *quel concept* travailler. Premier composant qui consomme
> `goal_relevance` en condition réelle — c'est la validation empirique
> du décomposeur `[1]`.
>
> Référence architecture : `docs/regulation-architecture.md` §3 [4],
> Q5 (frange externe), Q6 (ordre).

>
> **État courant (2026-11).** Ce document reste un dossier de conception
> historique. Écarts avec le code actuel :
>
> - le point d'entrée est `SelectConceptAtWithContext(phase, states, graph,
>   goalRelevance, now, SelectionContext)` ; les wrappers `SelectConcept` et
>   `SelectConceptAt` ont été supprimés ;
> - la frange externe est calculée par `externalFringeVerified` (§4.1) ;
> - DIAGNOSTIC n'utilise plus l'info-gain : la sélection est le placement
>   adaptatif de `engine/placement.go` (§4.4). `BKTInfoGain` et
>   `algorithms/bkt_info_gain.go` ont été supprimés ; seule `BinaryEntropy`
>   subsiste (`algorithms/entropy.go`) pour le signal d'audit de la phase.

---

## 1. Nature du composant

`[4] ConceptSelector` est une fonction quasi pure :

```go
func SelectConcept(
    phase models.Phase,
    states []*models.ConceptState,
    graph models.KnowledgeSpace,
    goalRelevance map[string]float64, // nil → uniform 1.0 fallback
) Selection
```

Aucun accès store, aucun side-effect, aucun appel réseau. Le « quasi »
tient à la défense en profondeur (NaN, états corrompus) qui produit un
`slog.Error` — cohérent avec OQ-5.6 dans `[5]`.

### Pourquoi *après* `[5] ActionSelector`

L'ordre Q6 (`7 → 1 → 5 → 4 → 3 → 2 → 6`) place `[4]` après `[5]`
pour deux raisons :

- `[5]` est strictement local au concept — ses tests sont indépendants
  du choix amont.
- `[4]` est plus chargé (3 phases, 3 signaux multipliés, plusieurs cas
  dégénérés). Le tester quand `[5]` est déjà validé clarifie l'interface
  amont/aval ("tu choisis le concept ; `[5]` choisit l'action").

L'output de `[4]` (un `Selection.Concept`) est l'input direct de `[5]`.
Ordre amont→aval naturel — pas une coïncidence.

### Comment `[4]` est utilisé par le runtime

État courant : `get_next_activity` appelle l'orchestrateur de phase,
qui compose `[4] SelectConcept`, `[5] SelectAction` et `[3] ApplyGate`.
Le router legacy n'est plus le chemin normal de sélection.

`REGULATION_CONCEPT` ne contrôle pas ce câblage runtime. Le flag ne
fait qu'inclure ou retirer l'appendix explicatif dans `tools/prompt.go`.

---

## 2. Signaux consommés

| Source | Champ | Phases utilisatrices |
|--------|-------|----------------------|
| `goal_relevance` (lu via `[1]`) | `map[string]float64` ou `nil` | INSTRUCTION (multiplicateur), MAINTENANCE (multiplicateur), DIAGNOSTIC (option — voir OQ-4.2) |
| BKT mastery | `cs.PMastery` | INSTRUCTION (filtre fringe + score), MAINTENANCE (filtre mastered), DIAGNOSTIC (exclusion des états NaN uniquement) |
| BKT params | `cs.PSlip`, `cs.PGuess` | DIAGNOSTIC (vraisemblance des observations rejouées par le placement) |
| Observations diagnostiques qualifiées | interactions + tentatives évaluées | DIAGNOSTIC (rejeu du placement) |
| KST prereqs | `graph.Prerequisites` | INSTRUCTION (calcul fringe externe) |
| FSRS state | `cs.Stability`, `cs.ElapsedDays`, `cs.CardState` | MAINTENANCE (urgence = 1 − retention) |
| Seuils unifiés | `algorithms.MasteryBKT()`, `MasteryKST()` | INSTRUCTION (filtre fringe), définition « mastered » pour MAINTENANCE |

**Ne consomme PAS** :

- **Misconceptions** : `[5]` les voit *après* le choix de concept et
  émet `DEBUG_MISCONCEPTION` si nécessaire. Séparation des responsabilités —
  pas de double conditionnement.
- **Affect** (énergie/confiance/satisfaction) : modulé par `[6]
  FadeController` en Phase 2 — ajustement *de la difficulté* via le
  multiplicateur de fade, pas du *choix de concept*.
- **Autonomy score** : idem — `[6]` ajuste la verbosité.
- **Phase d'intérêt Hidi-Renninger** : Q3 architecture a tranché qu'elle
  est orthogonale au routage de concept (modulation côté présentation).
- **Session history** : la dédup intra-session est dans `[3] Gate`.
  `[4]` ignore complètement la session.

---

## 3. Décision produite

```go
// engine/concept_selector.go
type Selection struct {
    Concept   string       // "" si NoFringe
    Score     float64      // 0 si NoFringe
    NoFringe  bool         // signal pour [2]
    Phase     models.Phase // echo de la phase utilisée (audit)
    Rationale string       // human-readable
}

func SelectConcept(phase models.Phase, states []*models.ConceptState,
    graph models.KnowledgeSpace, goalRelevance map[string]float64) Selection
```

### Le signal `NoFringe`

`NoFringe == true` signifie *« aucun concept éligible selon la phase
courante »*. C'est un **signal**, pas une **erreur** :

- En **INSTRUCTION** : tous les concepts ont `mastery >= MasteryBKT()`
  (apprenant a tout maîtrisé) — `[2]` doit basculer en MAINTENANCE.
- En **MAINTENANCE** : aucun concept maîtrisé (apprenant débute) —
  `[2]` doit basculer en INSTRUCTION.
- En **DIAGNOSTIC** : aucun concept non classé dans le pool éligible —
  le placement est terminé ou le gate a tout filtré.

Le caller (l'orchestrateur `[2]`) interprète `NoFringe` comme
une *suggestion de transition de phase*, pas comme un échec d'API.

`Concept == ""` est l'indicateur opérationnel ; `NoFringe == true` le
double pour clarifier l'intention. Le caller checke `NoFringe`
(lisible), pas `Concept == ""` (fragile).

---

## 4. Algorithmes par phase

### 4.1 Calcul de la frange externe (helper privé)

Implémentation actuelle : `externalFringeVerified(states, graph, verified)`
dans `engine/concept_selector.go`.

```
externalFringeVerified(states, graph, verified):
    stateByConcept = map[concept] → state
    fringe = []
    for concept in graph.Concepts:
        cs = stateByConcept[concept]
        mastery = 0
        if cs != nil:
            if math.IsNaN(cs.PMastery): continue   // NaN exclu (defensive)
            mastery = cs.PMastery

        if mastery >= MasteryBKT():
            continue                               // estimation déjà haute

        prereqs_ok = true
        for p in graph.Prerequisites[concept]:
            cs_p = stateByConcept[p]
            mp = 0  // pas de state → prereq non satisfait
            if cs_p != nil:
                if math.IsNaN(cs_p.PMastery): prereqs_ok = false; break
                // un prérequis sans succès vérifié (tentative évaluée
                // réussie ou succès FSRS espacé) compte au plus 0.84
                mp = PrerequisiteRoutingMastery(p, cs_p, verified)
            if mp < MasteryKST():
                prereqs_ok = false
                break
        if !prereqs_ok:
            continue

        fringe.append((concept, mastery))
    return fringe trié alphabétiquement   // tie-break OQ-4.4
```

Notes :

- Un concept présent dans `graph.Concepts` mais absent de `states`
  (apprenant pas encore exposé) est traité `mastery=0`. Cohérent avec
  la sémantique KST : un concept *peut être appris* dès que ses prereqs
  sont satisfaits, qu'il y ait eu une interaction ou non.
- NaN dans `cs.PMastery` exclut le concept de la frange, et un
  prérequis NaN ferme la frange en aval. Comportement conservateur : on
  ne pousse pas un concept dont l'état est corrompu.
- `verified == nil` désactive le plafond des prérequis non vérifiés.

### 4.2 INSTRUCTION

```
INSTRUCTION(states, graph, goalRelevance):
    fringe = externalFringeVerified(states, graph, verified)
    if len(fringe) == 0:
        return {NoFringe: true, Phase: INSTRUCTION,
                Rationale: "tous concepts maîtrisés ou prereqs absents"}

    bestConcept = ""
    bestScore   = -1
    for (concept, mastery) in fringe sorted alphabetically by concept:
        rel = relevance(goalRelevance, concept)   // OQ-4.3
        score = rel * (1 - mastery)                // OQ-4.6
        if score > bestScore:
            bestScore = score
            bestConcept = concept

    return {Concept: bestConcept, Score: bestScore, Phase: INSTRUCTION,
            Rationale: fmt.Sprintf("argmax(rel=%.2f × (1-mastery)=%.2f) sur %d candidats",
                       rel, 1-mastery, len(fringe))}
```

Le tri alphabétique préalable garantit le tie-break déterministe
(OQ-4.4).

### 4.3 MAINTENANCE

```
MAINTENANCE(states, graph, goalRelevance):
    mastered = []
    for cs in states:
        if cs == nil: continue
        if math.IsNaN(cs.PMastery): continue
        if cs.PMastery < MasteryBKT(): continue
        mastered.append(cs)

    if len(mastered) == 0:
        return {NoFringe: true, Phase: MAINTENANCE, ...}

    bestConcept = ""
    bestScore   = -1
    for cs in mastered sorted alphabetically by Concept:
        if cs.CardState == "new":
            urgency = 0  // pas de Stability significative
        else:
            retention = Retrievability(cs.ElapsedDays, cs.Stability)
            urgency = 1 - retention                // OQ-4.5

        rel = relevance(goalRelevance, cs.Concept)
        score = urgency * rel
        if score > bestScore:
            bestScore = score
            bestConcept = cs.Concept

    return {Concept: bestConcept, Score: bestScore, Phase: MAINTENANCE,
            Rationale: ...}
```

Note : si tous les concepts maîtrisés sont en `CardState=="new"` (cas
théorique : `mastery >= 0.85` mais jamais reviewé FSRS — devrait pas
arriver), tous les scores sont 0 ; le tie-break alphabétique pousse
le premier. Acceptable comme dégénéré.

### 4.4 DIAGNOSTIC (placement adaptatif)

La conception initiale (argmax de `BKTInfoGain` sur les concepts non
saturés, tie-break alphabétique) a été remplacée : tous les concepts
partent du même P(L)=0.1, donc l'info-gain était identique partout et
l'ordre devenait alphabétique ; un diagnostic réussi ne montait qu'à
0.33 (moins qu'une pratique réussie) et rien n'était propagé dans le
graphe.

Implémentation actuelle (`engine/placement.go`, `selectDiagnostic`) :

```
Placement = ReplayPlacement(domainGraph, states, observations, budget)
    // chaque concept part de PlacementPrior = 0.5
    for obs in observations qualifiées (sans indice, liée à une tentative
                                        évaluée, depuis l'entrée en phase):
        BKTObserve(obs.concept, obs.success)          // item direct
        if obs.success: BKTObserve(ancêtres(obs.concept), true)
        else:           BKTObserve(descendants(obs.concept), false)

classé(c) = itemsDirects(c) >= 2
         || (observé(c) && (P(c) <= 0.15 || P(c) >= 0.85))

DIAGNOSTIC(pool, placement):
    candidats = non classés ∩ pool (gate), hors états NaN
    trier par (profondeur de prérequis, nom)
    d* = profondeur du médian inférieur
    parmi les candidats de profondeur d*, argmax du nombre d'ancêtres et
    de descendants encore non classés ; tie-break alphabétique
```

- La profondeur est la longueur du plus long chemin de prérequis sous le
  concept (racines = 0) ; les cycles ne comptent pas.
- Une réussite place le concept testé et ses prérequis, un échec le
  concept et ce qui en dépend : c'est une dichotomie sur la profondeur.
- Les mises à jour d'observation à slip/guess fixes commutent : le
  rejeu ne dépend pas de l'ordre des réponses.
- `goal_relevance` reste ignoré (décision A1 ci-dessous) : le placement
  situe l'apprenant sur tout le graphe.
- La phase se termine quand tout est classé ou quand le budget
  `PhaseConfig.NDiagnosticMax` (12 items qualifiés) est atteint
  (`EvaluatePhase`). L'entropie n'est plus qu'un signal d'audit.
- `ApplyPlacementEstimates` écrit les estimations du placement (P(L)
  seulement, jamais les champs FSRS) pour les concepts dont toute
  l'évidence vient du placement ; il doit être appelé par
  `record_interaction` dans la transaction d'un DIAGNOSTIC_ASSESSMENT lié
  à une tentative.

## 5. Cas dégénérés

| Cas | Comportement | Garantie |
|-----|---------------|----------|
| `states == nil` ou vide | `NoFringe: true` | Caller gère |
| `goalRelevance == nil` | Fallback uniforme 1.0 partout | Couvert par helper `relevance()` |
| `goalRelevance` non-nil, concept absent | OQ-4.3 (par défaut : 0.5) | Voir arbitrage |
| Concept dans graphe mais pas dans `states` | Traité `mastery=0`, éligible si prereqs OK | Cohérent KST |
| Tous concepts en INSTRUCTION mastered | `NoFringe` → caller bascule MAINTENANCE | Architectural |
| Tous concepts en MAINTENANCE non-mastered | `NoFringe` → caller bascule INSTRUCTION | Architectural |
| `cs.PMastery = NaN` | Exclu de toute frange (defensive) | Couvert |
| Tie sur l'argmax | Tie-break alphabétique (OQ-4.4) | Reproductible |
| `goalRelevance` tous à 0 | Tous scores = 0 ; tie-break alphabétique sélectionne le premier ; `Score=0` dans Selection | Acceptable, signal lisible |
| Phase invalide | Erreur de programmation : panic ? Erreur retournée ? | Voir OQ-4.1 sub-question |
| `MasteryBKT()`/`MasteryKST()` change (REGULATION_THRESHOLD) | Lus via accesseurs ; cascade respecte le profil actif | OK — pas de literal |

---

## 6. Stratégie de test

### 6.1 Unit — INSTRUCTION

```go
TestSelectConcept_Instruction_PicksHighestScore
TestSelectConcept_Instruction_GoalRelevanceDominatesAtEqualMastery
TestSelectConcept_Instruction_LowMasteryWinsAtEqualRelevance
TestSelectConcept_Instruction_RespectsExternalFringe_PrereqsBlock
TestSelectConcept_Instruction_RespectsExternalFringe_MasteredExcluded
TestSelectConcept_Instruction_NoFringe_AllMastered
TestSelectConcept_Instruction_NoFringe_NoPrereqsSatisfied
TestSelectConcept_Instruction_UniformGoalRelevance_NilVector
TestSelectConcept_Instruction_TieBreakAlphabetical
TestSelectConcept_Instruction_ConceptNotInStatesIsEligible
```

### 6.2 Unit — MAINTENANCE

```go
TestSelectConcept_Maintenance_PicksLowestRetention
TestSelectConcept_Maintenance_GoalRelevanceWeights
TestSelectConcept_Maintenance_NoFringe_NothingMastered
TestSelectConcept_Maintenance_NewCardSkippedAsZeroUrgency
TestSelectConcept_Maintenance_TieBreakAlphabetical
```

### 6.3 Unit — DIAGNOSTIC (placement)

```go
// engine/placement_test.go
TestPlacementSingleSuccessBeatsColdPrior
TestPlacementFailureClassifiesAfterOneItem
TestPlacementPropagatesSuccessToAncestorsAndFailureToDescendants
TestPlacementNextConceptBisectsTheChain
TestPlacementCompletesOnClassificationOrBudget
TestSelectDiagnosticUsesPlacementAndDomainGraph
// engine/orchestrator_test.go
TestOrchestrate_AdaptivePlacementBisectsPrerequisiteChain
TestApplyPlacementEstimatesPropagatesWithoutCreatingCards
```

### 6.4 Unit — info-gain (algorithms/)

Supprimé avec `BKTInfoGain`.

### 6.5 Unit — helper `relevance()`

```go
TestRelevance_NilVectorReturnsUniformOne
TestRelevance_PresentReturnsValue
TestRelevance_MissingReturnsDefault   // OQ-4.3
```

### 6.6 Cas dégénérés

```go
TestSelectConcept_NaN_PMastery_ExcludedFromAllPhases
TestSelectConcept_EmptyStatesEmptyGraph
TestSelectConcept_RespectsMasteryBKTAccessor  // t.Setenv REGULATION_THRESHOLD
```

### 6.7 Régression

`SelectConcept` reste une fonction pure et testable isolément, même
lorsqu'elle est appelée par l'orchestrateur.

---

## 7. Interaction amont/aval

### Amont

- **`[2] PhaseController`** : décide la phase courante et appelle
  `SelectConcept(phase, ...)`. C'est le caller runtime.

### Aval (consommateurs de l'output)

- **`[5] ActionSelector`** : reçoit le concept choisi, décide l'action.
  Déjà fusionné, pas de changement.
- **`[3] Gate`** (PR `[3]`) : peut filtrer/rejeter le choix de `[4]`
  (ex : déjà pratiqué cette session, sauf critique). `[4]` ne consulte
  pas la session.

### Interaction avec `[1]`

**Forte.** `[4]` consomme le vecteur produit par `set_goal_relevance`.
C'est la première vérification empirique du décomposeur LLM. Si la
décomposition est mauvaise (LLM met `0.1` partout, ou ne décompose
pas), `[4]` le révèlera : routage indistinguable d'un fallback uniforme.

### Interaction avec `[7]`

Via accesseurs `MasteryBKT()` et `MasteryKST()`. Aucun literal — drift
test couvert.

---

## 8. Décisions ouvertes (toutes arbitrées)

> Bloc d'arbitrage final. Chaque OQ porte le défaut proposé en Phase 1
> *et* la décision validée par l'utilisateur, avec le raisonnement
> retenu en cas d'amendement.

### OQ-4.1 — Définition stricte de la frange externe

Frange externe = (1) prereqs satisfaits ET (2) propre mastery <
seuil. Sub-questions :

- (a) Concept dans `graph.Concepts` mais pas dans `states` (apprenant
  jamais exposé) : éligible (mastery=0) ou exclu ?
- (b) Phase invalide (string non reconnue) : panic, erreur, ou
  fallback INSTRUCTION ?

**A.** (a) éligible mastery=0, (b) fallback INSTRUCTION + slog.Warn
("regulation/concept-selector: unknown phase, defaulting to
INSTRUCTION"). Maximise la couverture, défensif vs futurs ajouts de
phase. **Mon défaut.**

**B.** (a) exclu tant que pas créé, (b) erreur retournée. Plus
conservateur ; force le caller à initialiser explicitement.

**C.** Mix : (a) éligible (cohérent KST), (b) panic — erreur de
programmation à corriger.

**Mon défaut** : **A**. Couverture maximale du graphe en (a) ;
robustesse opérationnelle en (b). La phase « inconnue » serait
introduite uniquement par bug de version ; un fallback log+continue
évite un crash de session pour une typo de constante.

**Décision validée** : **A pour (a), correction sur (b)** — phase
invalide retourne une *erreur explicite* (signature évolue en
`(Selection, error)`) et `slog.Error` (pas WARN). Pas de fallback
INSTRUCTION silencieux. Raisonnement : si demain `[2]` introduit une
nouvelle phase et oublie un cas, on veut le voir immédiatement, pas
masqué par un défaut. Le coût d'un crash bien typé est inférieur au
coût d'un comportement silencieusement dégradé.

### OQ-4.2 — Implémenter info-gain dans `[4]` ou différer en `[2]` ?

> Remplacée par le placement adaptatif (§4.4). La sous-décision A1
> (ignorer `goal_relevance` en DIAGNOSTIC) est conservée.

**A.** Implémenter `BKTInfoGain` dans `algorithms/bkt_info_gain.go`
+ tests isolés + branche DIAGNOSTIC dans `SelectConcept`. Bénéfice :
la formule reste validée isolément en plus de son usage par
l'orchestrateur — plus facile à debugger isolée que sous le pipeline
complet. Coût : ~80 lignes algorithms +
~50 lignes test.

**B.** Reporter à PR `[2]`. Plus simple pour cette PR. Inconvénient :
on testera la formule sous l'orchestrateur, plus difficile à
debugger en cas de dérive numérique.

**Mon défaut** : **A** (validé par cadrage utilisateur). Sub-question :
DIAGNOSTIC info-gain consomme-t-il `goal_relevance` ?

- **A1.** Pure info-gain (mon défaut). Diagnostiquer = réduire
  l'incertitude, indépendamment du goal — le diagnostic est *au
  service* du goal mais ne s'y subordonne pas.
- **A2.** `score = info_gain × sqrt(goal_relevance)`. Atténue, ne
  surdétermine pas. Évite de poser des questions diag sur des
  concepts complètement orthogonaux au goal.

**Mon défaut sub** : **A1**. Le diagnostic doit *cartographier* le
state ; couper les concepts low-relevance reviendrait à diagnostiquer
partiellement — peu utile en début de session où DIAGNOSTIC est
typiquement déclenché.

**Décision validée** : **A + A1**, avec annotation explicite dans le
code : *« v1 : DIAGNOSTIC ignore goal_relevance ; à ré-arbitrer avec
données réelles ».* La formule peut tirer un multiplicateur léger une
fois qu'on aura observé si le diagnostic dérive sur des concepts
orthogonaux au goal en pratique.

### OQ-4.3 — Concept manquant dans `goal_relevance`

Si `goalRelevance != nil` mais concept C absent du vecteur (uncovered
concept, OQ-1.1) :

**A.** **Exclure de la frange.** Force le LLM à appeler
`set_goal_relevance` avant que C soit éligible. Strict, mais peut
*starver* un concept récemment ajouté tant que le LLM n'a pas
re-décomposé.

**B.** **Inclure avec relevance = 1.0** (uniforme, comme si nil-vector).
Conséquence perverse : les concepts non-décomposés deviennent
*prioritaires* (max relevance), créant une incitation à *ne pas*
décomposer.

**C.** **Inclure avec relevance = 0.5** (centre du range [0,1]).
Visible, neutre. Pas d'incitation perverse. Pas de starvation.

**D.** **Inclure avec relevance = mean(scores existants)**. Neutre
relativement à la distribution actuelle. Plus complexe à expliquer.

**Mon défaut** : **C** (0.5). Simple, neutre, défensif contre les
deux dérives (starvation A et perverse B). Documenté clairement,
testable. Si l'eval révèle qu'on rate trop de concepts pertinents,
on raffinera.

**Décision validée** : **B' (exclusion + re-décomposition forcée
via NoFringe)**. Sur retour utilisateur : un défaut neutre crée un
faux positif — les concepts absents seraient choisis avant des
concepts à pertinence faible explicite, ce qui inverse le contrat.
Quatre arguments décisifs en faveur de B' :

1. **Cohérence avec `[1]`.** `[1]` expose `next_action` après
   `add_concepts` et `get_goal_relevance` pour la visibilité — ce
   sont des *forcing functions*. B' les rend porteuses : sans
   décomposition, le concept est invisible au régulateur. Avec C,
   la décomposition deviendrait optionnelle pour la *sélectionnabilité*
   et ne servirait qu'à régler la priorité — contrat affaibli.

2. **Test plus net du décomposeur LLM.** `[4]` est explicitement « le
   test du décomposeur LLM par les faits ». Si le LLM peut omettre
   `set_goal_relevance` et que le système « marche quand même » via
   un défaut 0.2-0.5, le signal d'eval est brouillé. Avec B', une
   décomposition manquante produit un `NoFringe` immédiat → bascule
   de phase → prompt explicite à appeler `get_goal_relevance` puis
   `set_goal_relevance`. Détection rapide, débogage facile.

3. **Voie de récupération bien définie, pas de deadlock.**
   `NoFringe` + `get_goal_relevance` + appendix prompt donnent au
   LLM tous les leviers pour récupérer. Le risque de starvation est
   borné dans le temps : un seul appel `set_goal_relevance` débloque.
   La séquence `add_concepts → next_action → set_goal_relevance` est
   le contrat, B' la rend exécutoire.

4. **Sémantique préservée.** « Absent du vecteur » porte exactement
   la sémantique tracée par OQ-1.1 (uncovered). Traduire en « default
   X » perd l'information : le régulateur ne distingue plus
   *« inconnu »* de *« explicitement à faible pertinence »*. Avec
   B', les deux états restent distincts et observables.

**Risque mitigé** : si le LLM oublie de décomposer après
`add_concepts`, le système reste vide. Mitigation : l'appendix
prompt énonce explicitement « *les concepts non couverts par
set_goal_relevance ne sont pas sélectionnables — appelle
get_goal_relevance après chaque add_concepts pour identifier les
manquants* ».

### OQ-4.4 — Tie-breaking déterministe

Quand l'argmax retourne plusieurs concepts à score égal :

**A.** **Alphabétique** (par concept name). Déterministe, indépendant
du signal, audit-friendly.

**B.** **Mastery la plus basse** (focus sur ce qu'il reste à
apprendre). Introduit un signal supplémentaire au tie-break.

**C.** **Goal_relevance la plus haute** (en cas de produit égal,
argmax sur le facteur le plus pédagogiquement saillant). Idem.

**D.** **Aléatoire seedé par session_id** (variété pour éviter
mécanique). Sacrifie la reproductibilité des tests.

**Mon défaut** : **A**. Tests reproductibles ; audit déterministe ;
n'introduit pas de signal supplémentaire au moment du tie-break — le
tie est un cas dégénéré, on le résout, point.

**Décision validée** : **A** (alphabétique). Test explicite à ajouter
avec concepts `alpha` / `beta` / `gamma` à scores égaux pour figer
le contrat — la sortie attendue est `alpha`, et toute régression
signalera un changement de tie-break.

### OQ-4.5 — Formule MAINTENANCE urgency

Pour ranger les concepts maîtrisés en MAINTENANCE :

**A.** `urgency = 1 − retention`. Direct, retention basse → priorité
haute. Aligné sur la sémantique de l'alerte FORGETTING
(`engine/alert.go`).

**B.** `urgency = -retention`. Équivalent à A à constante près.

**C.** `urgency = elapsed_days / stability`. Capture la « vitesse
d'oubli » plus fidèlement qu'un proxy de retention.

**D.** `urgency = max(0, 1 - retention)`. Identique à A pour
retention ∈ [0,1] (toujours le cas vu la formule FSRS).

**Mon défaut** : **A** = `1 - retention`. Aligné FORGETTING, simple,
lisible.

**Décision validée** : **A**. Note dans le code : *« v2 possible —
l'usage de la dérivée du déclin (e.g. `elapsed_days/stability`)
serait plus sensible aux écarts à proximité du seuil ; à ré-évaluer
si l'eval révèle que MAINTENANCE rate des concepts à oubli rapide ».*

### OQ-4.6 — Confirmation formule INSTRUCTION

`score = goal_relevance × (1 − mastery)` :

**A.** Confirmer la formule (mon défaut, cohérent avec cadrage
utilisateur). Comportement : à `goal_relevance` égal, préfère les
concepts les *moins* maîtrisés (breadth, ouvre le front d'apprentissage).
À mastery égal, préfère les concepts les plus pertinents.

**B.** Variante exposant : `goal_relevance × (1 − mastery)^2`. Amplifie
l'effet mastery → préfère encore plus fort les concepts non-démarrés.

**C.** Variante racine : `sqrt(goal_relevance) × (1 − mastery)`.
Atténue l'effet relevance → tolère les concepts low-relevance si
mastery est très basse.

**D.** Inverser le signe sur mastery : `goal_relevance × mastery`.
Préfère les concepts presque maîtrisés (depth, ferme avant d'ouvrir).

**Mon défaut** : **A**. KISS, défendable, testable. Si l'eval révèle
une dérive (ex : breadth excessive, élève qui ne consolide rien),
on revisitera. Pas dans cette PR.

**Décision validée** : **A** (confirmer). Tests explicites requis sur
les trois cas dégénérés :
- `goal_relevance ≈ 0` : concept à pertinence quasi-nulle ne gagne
  jamais sauf s'il est seul candidat (et que score=0 reste positif
  parce que `1-mastery` est non nul).
- `mastery ≈ MasteryBKT()` (juste sous le seuil) : score = `gr × 0.15`,
  très bas — un concept moins maîtrisé à pertinence égale gagne.
- Frange « normale » : 3-4 candidats avec spread sur les deux axes,
  vérification que l'argmax est bien le bon.

---

## 9. Plan de PR

### 9.1 Fichiers touchés

| Action | Fichier | Notes |
|--------|---------|-------|
| **Création** | `models/regulation.go` | type `Phase` + constantes (`PhaseInstruction`, `PhaseDiagnostic`, `PhaseMaintenance`). Sera consommé aussi par `[2]`. |
| **Création** | `engine/concept_selector.go` | `SelectConcept`, `Selection`, frange externe (aujourd'hui `externalFringeVerified`), helper `relevance` (~280 lignes) |
| **Création** | `engine/concept_selector_test.go` | ~380 lignes (3 phases + degenerate + tie-break) |
| **Création** (supprimé depuis) | `algorithms/bkt_info_gain.go` | `BKTInfoGain` — remplacé par `engine/placement.go` |
| **Modif** | `tools/prompt.go` | `regulationConceptEnabled()` + `conceptSelectorAppendix` |
| **Câblage courant** | `engine/orchestrator.go` | l'orchestrateur appelle `SelectConcept`; `REGULATION_CONCEPT` reste limité à l'appendix prompt |

### 9.2 Critères de merge

- [ ] `go test ./...` PASS sans flag
- [ ] `REGULATION_CONCEPT=on go test ./...` PASS
- [ ] `REGULATION_THRESHOLD=off go test ./...` PASS (vérifie usage des
      accesseurs, pas de literal)
- [ ] Aucun literal `0.85` ou `0.70` couplé à `mastery` dans
      `engine/concept_selector.go` (drift test `[7]` passe)
- [ ] Tests OQ-4.3 (uncovered concept default) présents et verts
- [ ] Tests d'oscillation/dégénérescence (NaN, empty, all-mastered) verts

### 9.3 Pas inclus dans cette PR

- **Câblage runtime** (router → `SelectConcept`) : reporté à PR `[2]`.
- **Test d'intégration end-to-end goal-aware** (apprenant simulé sur
  20 sessions, comparaison goal-uniform vs goal-relevant exigée par
  Q6 architecture) : sera dans PR `[2]` quand le pipeline complet
  tourne.
- **`[2] PhaseController`** : un fichier `models/regulation.go` est créé
  pour exposer `Phase` mais aucune logique de transition de phase n'est
  implémentée ici.

---

## 10. Récap

| Aspect | Décision |
|--------|----------|
| **Signature** | `SelectConcept(phase, states, graph, goalRelevance) Selection` — quasi pure |
| **3 branches** | INSTRUCTION (argmax `gr × (1-m)` sur frange), MAINTENANCE (argmax `(1-R) × gr` sur mastered), DIAGNOSTIC (placement adaptatif : médian de profondeur parmi les non classés) |
| **Signal NoFringe** | Distinct de l'erreur ; suggère bascule de phase à `[2]` |
| **Frange externe** | OQ-4.1 = A pour (a) ; phase invalide → erreur explicite + slog.Error (signature `(Selection, error)`) |
| **DIAGNOSTIC** | Info-gain (OQ-4.2 = A) remplacé par le placement adaptatif. Sub A1 conservée : pas de goal_relevance en DIAGNOSTIC |
| **Concept manquant goal_rel** | OQ-4.3 = **B'** (exclusion + re-décomposition forcée via NoFringe) |
| **Tie-break** | OQ-4.4 = A (alphabétique). Test alpha/beta/gamma à scores égaux requis |
| **MAINTENANCE urgency** | OQ-4.5 = A (`1 - retention`). Note v2 : dérivée `elapsed/stability` possible |
| **INSTRUCTION formula** | OQ-4.6 = A (confirmer `gr × (1 - mastery)`). Tests dégénérés : `gr≈0`, `mastery≈BKT`, frange normale |
| **Flag** | `REGULATION_CONCEPT=off` retire seulement l'appendix prompt ; `SelectConcept` reste exécuté par l'orchestrateur |
| **Problèmes résolus** | validation empirique de [1] ; pose la fondation de `[2]` qui consommera `Phase` |

---

**STOP.** Design `[4] ConceptSelector` complet. En attente de validation,
ou amendements sur les 6 décisions ouvertes (OQ-4.1 frange définition,
OQ-4.2 info-gain placement, OQ-4.3 missing goal_relevance, OQ-4.4
tie-break, OQ-4.5 MAINTENANCE urgency, OQ-4.6 INSTRUCTION formula).
Composant suivant : `[3] Gate Controller`.
