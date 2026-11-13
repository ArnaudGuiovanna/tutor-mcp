# [3] Gate Controller — Design (Phase 1)

> Composant 5/7 du pipeline de régulation. S'insère *avant* `[4]
> ConceptSelector` et `[5] ActionSelector` : filtre les candidats,
> impose des vetos durs, et peut court-circuiter le pipeline avec une
> *escape action* (e.g. fin de session forcée sur OVERLOAD).
>
> Référence architecture : `docs/regulation-architecture.md` §3 [3],
> Q5 (anti-répétition).

---

## 1. Nature du composant

`[3] Gate` est une **fonction pure** :

```go
func ApplyGate(input GateInput) GateResult
```

Aucun accès store, aucun side-effect, aucun appel réseau. Les signaux
d'entrée (alerts, misconceptions actives, interactions récentes) sont
pré-dérivés par le caller depuis le store. Permet le test unitaire en
fixtures.

### Ordre dans le pipeline

L'ordre Q6 (`7 → 1 → 5 → 4 → 3 → 2 → 6`) place `[3]` après `[4]` et
`[5]` au plan d'implémentation, mais *avant* eux à l'exécution :

```
[2] Phase  →  [3] Gate  →  [4] Concept  →  [5] Action
                              ^                ^
                              |                |
                  candidats filtrés    actions restreintes
```

Pourquoi cet ordre d'implémentation : `[3]` consomme la signature de
`[4]` (frange, états par concept) et de `[5]` (`ActivityType` enum).
Implémenter `[3]` après garantit que les contrats d'interface sont
figés.

### Comment `[3]` arrive dans le router

État courant :

1. `get_next_activity` appelle l'orchestrateur de phase.
2. L'orchestrateur appelle `ApplyGate(input)` :
   - Si `gate.EscapeAction != nil` → emit l'escape directement, fin du
     pipeline.
   - Si `gate.NoCandidate` → il bascule de phase ou émet REST.
   - Sinon → il passe `gate.AllowedConcepts` à `[4] SelectConcept`,
     et `gate.PerConceptActionRestriction` à `[5] SelectAction`.

`REGULATION_GATE` ne contrôle pas ce câblage runtime. Le flag ne fait
qu'inclure ou retirer l'appendix explicatif dans `tools/prompt.go`.

---

## 2. Signaux consommés

| Source | Champ | Usage |
|--------|-------|-------|
| Phase courante | `models.Phase` | Modifie l'application des règles (DIAGNOSTIC bypasse certains vetos — voir OQ-3.6) |
| Liste candidats | `[]string` (typiquement `graph.Concepts`) | Pool d'entrée à filtrer |
| États par concept | `map[string]*models.ConceptState` | Lookup mastery pour règle prereq |
| Graphe | `models.KnowledgeSpace.Prerequisites` | Règle 2 (KST) |
| Misconceptions actives | `map[string]bool` (concept → has-active) | Règle 1 (action restriction) |
| Concepts récents | `[]string` (last N pratiqués, DESC) | Règle 4 (anti-répétition) |
| Alerts | `[]models.Alert` | Règle 3 (OVERLOAD escape), exception FORGETTING (anti-rép bypass) |
| `AntiRepeatWindow` | `int` (paramétrable) | Profondeur de la fenêtre anti-répétition |

**Ne consomme PAS** :
- **`goal_relevance`** : pas pertinent au filtrage ; `[4]` l'utilise pour scorer.
- **BKT params (P(S)/P(G))** : non pertinents pour les vetos.
- **Affect / autonomy** : `[6]` les module, pas `[3]`.

---

## 3. Décision produite

```go
type GateResult struct {
    EscapeAction  *EscapeAction               // non-nil → court-circuit pipeline
    NoCandidate   bool                        // signal pour [2] (bascule phase)
    AllowedConcepts []string                  // pool filtré
    ActionRestriction map[string][]models.ActivityType
                                              // concept → set des actions autorisées
                                              // absent ≡ pas de restriction
    Rationale     string
}

type EscapeAction struct {
    Type      models.ActivityType  // ActivityCloseSession (OQ-3.2) ou ActivityRest
    Format    string               // "session_overload" / etc.
    Rationale string
}
```

Trois modes mutuellement exclusifs :

1. **EscapeAction** non-nil : règle 3 (OVERLOAD) a tiré. Le caller emet
   l'escape sans appeler `[4]`/`[5]`.
2. **NoCandidate** = true : tous les candidats ont été filtrés. Le
   caller bascule de phase.
3. **AllowedConcepts** non-vide : `[4]` doit s'y restreindre.
   `ActionRestriction[c]`, si présent, contraint `[5]` à n'émettre que
   ces ActivityTypes pour `c` (sinon défaut implicite =
   `DEBUG_MISCONCEPTION` via la cascade interne de `[5]`, mais
   l'expression explicite via Gate clarifie le contrat).

---

## 4. Algorithme — composition des règles

```
ApplyGate(input):
    // Règle 3 (escape) en premier — l'apprenant doit s'arrêter
    // (OQ-3.5 = priorité OVERLOAD).
    for alert in input.Alerts:
        if alert.Type == AlertOverload:
            return GateResult{
                EscapeAction: &EscapeAction{
                    Type: ActivityCloseSession (OQ-3.2),
                    Format: "session_overload",
                    Rationale: "session >= 45 min (OVERLOAD)",
                },
            }

    // Construire le set initial — phase-dépendant
    candidates = input.Concepts (copie)
    actionRestrictions = map[string][]ActivityType{}

    // Règle 2 — Prereq KST (sauf DIAGNOSTIC)
    if input.Phase != PhaseDiagnostic:
        candidates = filter(candidates, prereqsSatisfied)

    // Règle 4 — Anti-répétition (avec exception FORGETTING)
    forgettingConcepts = set of concepts in input.Alerts with Type==FORGETTING
    recentSet = set(input.RecentConcepts[:AntiRepeatWindow])
    candidates = filter(candidates, c => !recentSet[c] || forgettingConcepts[c])

    // Règle 1 — Misconception (action restriction, ne filtre PAS)
    // Sub : misconception bypasse aussi anti-répétition (OQ-3.5 sub).
    // Note : si misconception sur concept déjà filtré par anti-rép,
    // on le réintroduit.
    for c in input.Concepts:
        if input.ActiveMisconceptions[c]:
            if !contains(candidates, c):
                candidates = append(candidates, c)  // réintroduction
            actionRestrictions[c] = [ActivityDebugMisconception]

    // Cas dégénéré : tout filtré
    if len(candidates) == 0:
        return GateResult{
            NoCandidate: true,
            Rationale: "tous candidats filtres",
        }

    return GateResult{
        AllowedConcepts: candidates,
        ActionRestriction: actionRestrictions,
        Rationale: ...,
    }
```

### Notes d'algorithme

- **Ordre des vetos** (OQ-3.5) : OVERLOAD (escape) > misconception
  (action lock) > prereq (concept exclusion) > anti-répétition (concept
  exclusion). La misconception *bypasse* l'anti-répétition : on tolère
  la répétition d'un concept en debug pour fixer l'erreur.
- **FORGETTING bypass** (Q5) : un concept en alerte FORGETTING est
  ré-introduit même s'il est dans la fenêtre anti-rép.
- **DIAGNOSTIC** : seule la règle de prereq est bypassée (le diagnostic
  teste tout). OVERLOAD reste prioritaire ; misconception reste active ;
  anti-répétition reste active (voir OQ-3.6).

### Composition formelle

```
escape(alerts) > restrict(misconception) > exclude(prereq) > exclude(anti-rep, except forgetting)
```

Avec : misconception bypasse anti-rep en réintroduisant.

---

## 5. Cas dégénérés

| Cas | Comportement | Garantie |
|-----|---------------|----------|
| `input.Concepts` vide | `NoCandidate=true` immédiat | Caller gère |
| Aucune alerte, aucune misconception, anti-rep désactivée (N=0) | Tous candidats passent (sauf prereq) | Cohérent |
| OVERLOAD + misconception sur même concept | Escape gagne — l'apprenant s'arrête, la misconception sera traitée à la session suivante | OQ-3.5 |
| OVERLOAD + FORGETTING + misconception | Escape gagne | OQ-3.5 |
| Concept absent de `States` | Traité `mastery=0` pour prereq parent ; non filtré par mastery (le Gate ne filtre pas par mastery — c'est `[4]`) | OK |
| Tous concepts en récent buffer + aucune alerte FORGETTING | `NoCandidate=true` (ou `[2]` peut décider d'ignorer la fenêtre) | Voir OQ-3.4 |
| Misconception sur concept dont les prereqs ne sont pas OK | OQ-3.5 sub : misconception réintroduit-elle aussi un concept exclu par prereq ? Mon défaut : **non** (les prereqs sont une vérité KST plus structurelle que la session-locale anti-rép) | Voir OQ-3.5 |
| `AntiRepeatWindow` > `len(RecentConcepts)` | Pas de pad, on prend ce qu'il y a (caller responsable de la cohérence) | OK |
| `Phase` invalide | Erreur explicite (cohérent avec [4] OQ-4.1) | Signature `(GateResult, error)` |
| `RecentConcepts == nil` | Anti-rep no-op (set vide) | OK |
| `Alerts == nil` | Pas d'escape, pas d'exception FORGETTING | OK |

---

## 6. Stratégie de test

### 6.1 Unit — règles individuelles

```go
TestApplyGate_OverloadEscape_TakesPrecedence
TestApplyGate_OverloadEscape_OverridesMisconception
TestApplyGate_OverloadEscape_OverridesForgetting
TestApplyGate_PrereqFilter_Excludes
TestApplyGate_PrereqFilter_BypassedInDiagnostic
TestApplyGate_AntiRepeat_ExcludesRecent
TestApplyGate_AntiRepeat_BypassedByForgetting
TestApplyGate_AntiRepeat_BypassedByMisconception
TestApplyGate_AntiRepeat_RespectsWindowSize
TestApplyGate_Misconception_RestrictsActions
TestApplyGate_Misconception_DoesNotFilterConcept
TestApplyGate_Misconception_ReintroducesAfterAntiRep
```

### 6.2 Unit — composition

```go
TestApplyGate_PrereqAndAntiRepCombined
TestApplyGate_AllRulesCombined
TestApplyGate_EmptyCandidatesPool_NoCandidate
TestApplyGate_AllFilteredOut_NoCandidate
TestApplyGate_PhaseInvalid_ReturnsError
```

### 6.3 Unit — phase-spécifique

```go
TestApplyGate_Diagnostic_BypassesPrereqs
TestApplyGate_Diagnostic_StillRespectsOverload
TestApplyGate_Diagnostic_StillRestrictsMisconception
TestApplyGate_Maintenance_AllRulesApply
```

### 6.4 Cas dégénérés

```go
TestApplyGate_NilAlerts
TestApplyGate_NilRecentConcepts
TestApplyGate_AntiRepeatWindowZero
TestApplyGate_AntiRepeatWindowExceedsRecent
```

### 6.5 Régression

`ApplyGate` reste une fonction pure et testable isolément, même
lorsqu'elle est appelée par l'orchestrateur.

---

## 7. Interaction amont/aval

### Amont

- **`[2] PhaseController`** : caller runtime. Pré-derive les
  inputs (alerts via `engine/alert.go`, misconceptions via
  `db.GetActiveMisconceptions`, recent concepts via
  `db.GetRecentInteractionsByLearner`).

### Aval

- **`[4] ConceptSelector`** : reçoit `gate.AllowedConcepts` comme pool
  réduit ; sa logique d'argmax tourne dessus. Câblage en PR `[2]`.
- **`[5] ActionSelector`** : reçoit `gate.ActionRestriction[c]` ; si
  non-vide, sa cascade interne doit s'aligner. Aujourd'hui, `[5]`
  honore déjà la priorité misconception via son override interne — le
  Gate ne fait que rendre la contrainte explicite. Pas de modification
  de `[5]` requise dans cette PR.

### Avec `[5]` — alignement de la priorité misconception

`[5]` priorise déjà `DEBUG_MISCONCEPTION` quand un misconception est
passé en argument. Si la couche d'orchestration (PR `[2]`) lui passe
le bon misconception pour le concept choisi par `[4]` (lui-même
contraint par `[3]`), tout s'aligne sans glue supplémentaire. Le test
d'intégration end-to-end de `[2]` validera ce chaînage.

---

## 8. Décisions ouvertes

### OQ-3.1 — Forme de l'output

**A.** **GateResult struct unifié** (`EscapeAction *`,
`NoCandidate bool`, `AllowedConcepts []string`, `ActionRestriction
map[]`). Mon défaut. Lit clair, trois modes mutuellement exclusifs
documentés.

**B.** **Trois fonctions séparées** (`ShouldEscape()`, `Filter()`,
`Restrictions()`). Plus modulaire ; coût de duplication des entrées et
des dérivations intermédiaires.

**C.** **Closures retournées** (e.g. une fonction `IsEligible(concept
string) bool` + un escape pointer). Idiomatique GoLang minoritaire ;
moins testable.

**Mon défaut** : **A**. Cohérent avec `Selection` de `[4]` et `Action`
de `[5]` (struct typé clair). Tests directs sur les champs.

### OQ-3.2 — `CloseSession` : nouveau enum vs réutiliser `ActivityRest` ?

Le router legacy émet `ActivityRest` sur OVERLOAD avec un message
"travaillé > 45 min, suggère pause". Sémantiquement c'est *« pause
intra-session »*. La cadrage utilisateur parle de *« CloseSession »* —
sémantique distincte de *« mettre fin à la session »*.

**A.** Introduire `ActivityCloseSession` (« CLOSE_SESSION »). Distinction
claire avec `ActivityRest`. Aligné avec l'outil MCP existant
`record_session_close`. Le LLM peut router différemment (un Rest
suggère continuer après pause ; un CloseSession émet le recap_brief
et clôt).

**B.** Réutiliser `ActivityRest` avec `Format: "session_overload"`.
Surface enum minimale ; LLM lit le format pour discriminer. Risque :
ambiguïté côté audit (« quel rest était une vraie pause vs une fin
forcée ? »).

**C.** Réutiliser `ActivityRest` mais ajouter un champ
`CloseSession bool` dans `EscapeAction`. Hybride.

**Mon défaut** : **A**. La sémantique est distincte (close ≠ rest),
le LLM-side handling est différent (recap, queue webhook, etc.), et
l'audit gagne en lisibilité.

### OQ-3.3 — Durée du veto misconception

Aujourd'hui, `db.computeMisconceptionStatus` (cf
`db/misconceptions.go:113`) regarde les 3 dernières interactions et
retourne `"active"` si l'une porte le misconception, sinon
`"resolved"`. C'est déjà une fenêtre interaction-based de N=3.

Question : `[3]` doit-il introduire sa propre durée, ou consommer
le statut existant ?

**A.** **Consommer le statut existant.** `[3]` reçoit la map
`ActiveMisconceptions[concept] bool` dérivée par le caller via
`db.GetActiveMisconceptions`. La durée est implicite (3 interactions
sans le misconception → resolved → plus dans la map). Mon défaut.

**B.** **Time-based** : expirer le veto après N jours, indépendamment
des interactions. Simple, mais rompt la sémantique « le misconception
est résolu quand l'apprenant ne le commet plus ».

**C.** **Interaction-based explicite dans `[3]`** : compter les N
dernières interactions sur le concept. Duplique la logique de
`db.computeMisconceptionStatus`.

**Mon défaut** : **A**. Le veto suit le statut DB. La règle dans `[3]`
devient : *« si misconception actif (au sens de la couche DB), action
restreinte »*. Cohérence avec le reste du système.

### OQ-3.4 — Valeur par défaut du `AntiRepeatWindow` paramétrable

Q5 a validé une fenêtre paramétrable mais sans fixer la valeur par
défaut.

**A.** N=3. Aligné sur la fenêtre misconception ; l'apprenant peut
revoir le même concept toutes les 3 activités.

**B.** N=2. Très permissif — autorise concept→autre→concept.

**C.** N=5. Force plus de diversité.

**D.** N=0 par défaut (anti-rep désactivée par défaut, à activer par
le caller).

**Mon défaut** : **A** = 3. Empirique, raisonnable, exposé via
constante `DefaultAntiRepeatWindow = 3`. Le caller peut surcharger
(le terrain `AntiRepeatWindow` du `GateInput` est explicite). Si
l'eval révèle dérive (ennui ou répétition excessive), revisiter.

### OQ-3.5 — Composition / priorité des vetos

Ordre proposé :

```
escape(OVERLOAD) > restrict(misconception) > exclude(prereq) > exclude(anti-rep)
```

Avec deux sub-questions :

(a) **Misconception bypasse-t-elle anti-rep ?** (concept récent + misconception → garde-le pour DEBUG)

- **A**. Oui — fixer l'erreur prime sur la diversité.
- **B**. Non — laisse refroidir, l'erreur réapparaîtra.

**Mon défaut sub (a)** : **A**. Cohérent avec l'exception FORGETTING
(les vetos pédagogiques importants bypassent l'anti-rép).

(b) **Misconception bypasse-t-elle prereq ?** (concept dont les
prereqs ne sont pas OK + misconception active)

- **A**. Oui — l'apprenant a *fait* le concept (sinon pas de
  misconception), donc même si prereqs « théoriquement » manquent, la
  réalité empirique prime.
- **B**. Non — les prereqs sont une vérité KST structurelle ; si on
  a un misconception sur un concept aux prereqs absents, c'est un
  signe de mauvaise initialisation, pas une raison pédagogique de
  router dessus.

**Mon défaut sub (b)** : **B**. Cas pathologique rare ; vaut mieux
le surfacer comme bug d'initialisation que le masquer en routant
dessus. La misconception ne bypasse PAS prereq.

### OQ-3.6 — Quels vetos s'appliquent en DIAGNOSTIC

**A.** **OVERLOAD oui, misconception oui, anti-rep oui, prereq non**
(seul prereq est bypassé). Mon défaut.

**B.** **Tout sauf OVERLOAD** (DIAGNOSTIC est une phase de découverte
pure ; ignorer les vetos pédagogiques pour cartographier le state).

**C.** **Tout** (DIAGNOSTIC respecte tous les vetos comme INSTRUCTION).

**Mon défaut** : **A**. DIAGNOSTIC teste les zones ambiguës — bypasser
prereq permet de tester un concept "avancé" pour calibrer l'IRT
même si les prereqs ne sont pas formellement validés. Mais
OVERLOAD reste une règle hygiène (toujours), misconception reste
correctif (on ne diagnostique pas par-dessus une erreur active),
anti-rep reste pertinent (varier les diagnostics).

---

## 9. Plan de PR

### 9.1 Fichiers touchés

| Action | Fichier | Notes |
|--------|---------|-------|
| **Création** | `engine/gate.go` | `ApplyGate`, types `GateInput`/`GateResult`/`EscapeAction`, helpers privés (~280 lignes) |
| **Création** | `engine/gate_test.go` | ~400 lignes (règles individuelles + composition + phase-spécifique + dégénérés) |
| **Modif** | `models/domain.go` | ajouter constante `ActivityCloseSession` (OQ-3.2 = A) |
| **Modif** | `tools/prompt.go` | `regulationGateEnabled()` + `gateAppendix` documentant le contrat des vetos |
| **Câblage courant** | `engine/orchestrator.go` | l'orchestrateur appelle `ApplyGate`; `REGULATION_GATE` reste limité à l'appendix prompt |

### 9.2 Critères de merge

- [ ] `go test ./...` PASS sans flag
- [ ] `REGULATION_GATE=on go test ./...` PASS
- [ ] `REGULATION_THRESHOLD=off go test ./...` PASS
- [ ] Aucun literal `0.85` ou `0.70` couplé à `mastery` dans
      `engine/gate.go` (drift test `[7]`)
- [ ] Tests OQ-3.5 (composition) couvrent les 4 combinaisons
      (overload×misconception, misconception×anti-rep, prereq×anti-rep,
      forgetting bypass)
- [ ] Test OQ-3.6 explicite par phase × règle (matrix 3×4)

### 9.3 Pas inclus dans cette PR

- **Câblage runtime** : reporté à PR `[2]`.
- **Test d'intégration end-to-end** (apprenant simulé sur 20 sessions
  avec OVERLOAD + misconception + FORGETTING) : sera dans PR `[2]`.
- **Mise à jour de `[5]` pour honorer explicitement
  `ActionRestriction`** : non requis aujourd'hui — `[5]` honore déjà
  la priorité misconception. Si l'eval de PR `[2]` révèle un cas où
  l'orchestrateur doit overrider `[5]`, on étendra à ce moment.

---

## 10. Récap

| Aspect | Décision (mon défaut) |
|--------|------------------------|
| **Signature** | `ApplyGate(GateInput) (GateResult, error)` — fonction pure |
| **3 modes de sortie** | `EscapeAction` (court-circuit), `NoCandidate` (signal phase), `AllowedConcepts + ActionRestriction` (cas standard) |
| **Output shape** | OQ-3.1 = A (GateResult struct unifié) |
| **CloseSession** | OQ-3.2 = A (nouveau `ActivityCloseSession`) |
| **Durée veto misconception** | OQ-3.3 = A (consomme le statut DB existant — fenêtre 3 interactions implicite) |
| **AntiRepeatWindow défaut** | OQ-3.4 = A (N=3, paramétrable via `GateInput`) |
| **Composition** | OQ-3.5 = OVERLOAD > misconception (restrict) > prereq > anti-rep ; misconception bypasse anti-rep mais PAS prereq |
| **Phase DIAGNOSTIC** | OQ-3.6 = A (seul prereq bypassé) |
| **Flag** | `REGULATION_GATE=off` retire seulement l'appendix prompt ; `ApplyGate` reste exécuté par l'orchestrateur |
| **Problèmes résolus** | protections de routage (prérequis, anti-répétition, priorité de l'oubli critique) matérialisées par le Gate |

---

**STOP.** Design `[3] Gate Controller` complet. En attente de
validation, ou amendements sur les 6 décisions ouvertes (OQ-3.1
output shape, OQ-3.2 CloseSession enum, OQ-3.3 durée veto misconception,
OQ-3.4 défaut AntiRepeatWindow, OQ-3.5 composition vetos avec sub-questions,
OQ-3.6 phase DIAGNOSTIC). Composant suivant après `[3]` : `[2] PhaseController`
(orchestrateur — c'est là que tout se câble).
