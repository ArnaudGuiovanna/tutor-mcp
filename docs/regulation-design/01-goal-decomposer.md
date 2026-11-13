# [1] GoalDecomposer — Design (Phase 1)

> Composant 2/7 du pipeline de régulation. Produit le vecteur
> `goal_relevance: map[concept]float64 ∈ [0,1]` qui matérialise la
> promesse README *« calibrated in real time to the learner's mastery,
> ability, affect, and personal goal »*.
>
> Référence architecture : `docs/regulation-architecture.md` §3 [1],
> §6 Q1, §6 Q2.

---

## 1. Nature du composant

Le runtime **ne génère pas** le vecteur `goal_relevance` (cohérent avec
le garde-fou « LLM = content engine »). Le composant `[1]` est :

1. Un **schéma de persistance** (colonne JSON versionnée sur `domains`).
2. Un **outil MCP** `set_goal_relevance` que le LLM appelle pour écrire
   le vecteur après avoir lu le `personal_goal` et la liste de concepts.
3. Un **accessor de lecture** côté runtime (`Domain.GoalRelevance()`)
   avec **fallback uniforme** quand le vecteur est absent ou stale.
4. Une **instruction dans la réponse de `init_domain` et `add_concepts`**
   qui indique au LLM d'appeler `set_goal_relevance` (asynchrone, non
   bloquant — Q2 = B).

Pas de signal cognitif consommé. Pas de boucle de décision propre. Le
composant est *amont* du pipeline de régulation : il pose un signal
qui sera lu par `[4] ConceptSelector` quand celui-ci sera implémenté.

### Pourquoi ce composant en deuxième

La séquence d'implémentation `7 → 1 → 5 → 4 → 3 → 2 → 6` (validée Q6)
place `[1]` après `[7]` et avant `[5]`. Raison :

- `[5] ActionSelector` n'a pas besoin de `goal_relevance` (il décide un
  type d'activité étant donné un concept déjà choisi).
- `[4] ConceptSelector` consomme `goal_relevance`. Il a besoin du
  signal pour fonctionner.
- En implémentant `[1]` avant `[4]`, on dispose du signal en prod
  pendant ~3-4 PRs avant qu'il ait un consommateur. Ça permet
  d'observer le comportement LLM (les valeurs envoyées,
  l'idempotence, l'absence d'appel) en conditions réelles avant que
  ces données nourrissent une décision.

---

## 2. Signal consommé

| Source | Champ | Localisation actuelle |
|--------|-------|------------------------|
| `Domain.PersonalGoal` | texte libre, ≤ 2000 chars (cap existant) | `models/domain.go:73`, persisté `domains.personal_goal` (`db/schema.sql`) |
| `Domain.Graph.Concepts` | `[]string`, ≤ 500 concepts (cap existant) | `models/domain.go:74`, persisté `domains.graph_json` |

Aucun nouveau signal cognitif. Aucune dépendance aux autres composants.

## 3. Décision produite

```go
type GoalRelevance struct {
    ForGraphVersion int                `json:"for_graph_version"`
    Relevance       map[string]float64 `json:"relevance"` // concept → score ∈ [0,1]
    SetAt           time.Time          `json:"set_at"`
}
```

- `Relevance` : pour chaque concept du domaine, score de pertinence
  vis-à-vis du `personal_goal`. 1.0 = goal-critique, 0.0 = orthogonal.
  Concepts manquants → fallback uniforme (= 1.0) à la lecture.
- `ForGraphVersion` : numéro de version du graphe au moment du set.
  Permet de détecter si le vecteur est devenu stale (un `add_concepts`
  ultérieur a augmenté la version du graphe).
- `SetAt` : horodatage UTC du dernier set, pour observabilité.

### Signature des accessors

```go
// Côté store (db/store.go ou nouveau db/goal_relevance.go) :
func (s *Store) UpdateDomainGoalRelevance(domainID string, rel map[string]float64) error
func (s *Store) GetDomainGoalRelevance(domainID string) (*models.GoalRelevance, error)

// Côté domain model (helper sur Domain) :
func (d *Domain) GoalRelevance() map[string]float64
//   - Returns nil si JSON vide ou parse fails (silent fallback).
//   - Caller treats nil as "all concepts uniform = 1.0".

// Côté domain model (helper de staleness) :
func (d *Domain) IsGoalRelevanceStale() bool
//   - True si GoalRelevanceVersion < GraphVersion.
```

---

## 4. Outil MCP `set_goal_relevance`

Nouveau handler dans `tools/goal_relevance.go`.

### Schéma d'entrée

```go
type SetGoalRelevanceParams struct {
    DomainID  string             `json:"domain_id,omitempty" jsonschema:"ID du domaine cible (optionnel)"`
    Relevance map[string]float64 `json:"relevance" jsonschema:"Map concept → score [0,1]. Concepts manquants traités comme uniformes."`
}
```

### Schéma de sortie

```json
{
  "domain_id": "dom_abc",
  "for_graph_version": 3,
  "concepts_updated": 5,         // concepts touchés par cet appel (créés ou écrasés)
  "concepts_clamped": 1,         // valeurs hors [0,1] ramenées à la borne (avec log)
  "uncovered_concepts": ["C12"], // concepts du graph encore sans entry après ce merge
  "covered_concepts_count": 13,  // concepts avec entry dans le merge final
  "all_concepts_count": 14,      // taille de Domain.Graph.Concepts
  "stale_after_set": false       // true si graph_version a entre-temps avancé (race rare)
}
```

### Algorithme du handler — sémantique incrémentale (merge)

```
1. Auth → learner_id
2. Resolve domain (resolveDomain, comme tous les autres tools)
3. Si REGULATION_GOAL != "on" → return error "feature flag disabled"
4. Validate input :
   - Relevance non-nil, len ≥ 1 (set vide rejeté)
   - len(Relevance) ≤ 500 (cap concepts)
   - Pour chaque (k, v) : k non-vide, len(k) ≤ 200
5. Validate ownership (OQ-1.4 strictness) :
   - Pour chaque k dans Relevance : k DOIT être présent dans
     Domain.Graph.Concepts. Sinon → error "unknown concept: <k>"
     citant le premier nom inconnu rencontré, sans persister.
   - Sécurise contre l'hallucination LLM (concept inventé).
6. Clamp valeurs hors [0,1] :
   - score < 0 → 0, score > 1 → 1, NaN → erreur "NaN score for <k>".
   - Compteur concepts_clamped pour observabilité.
7. Merge incrémental + persist :
   - tx := store.Begin()
   - existing, _ := GetDomainGoalRelevance(domainID)
   - merged := existing.Relevance ?? {}
   - for k, v := range Relevance { merged[k] = v }
   - newJSON := {for_graph_version: domain.GraphVersion,
                 relevance: merged, set_at: now}
   - UPDATE domains SET goal_relevance_json = ?, goal_relevance_version =
     goal_relevance_version + 1 WHERE id = ?
   - tx.Commit()
8. Compute uncovered :
   - uncovered := Domain.Graph.Concepts \ keys(merged)
9. Return response payload (incl. uncovered_concepts list).
```

**Sémantique merge importante.** `set_goal_relevance({"C5": 0.4})` sur
un domaine ayant déjà `{C1: 0.9, C2: 0.4}` produit `{C1: 0.9, C2: 0.4,
C5: 0.4}`. Aucune entrée existante n'est effacée. Pour « effacer » une
relevance (revenir au fallback uniforme), le LLM peut envoyer
`score = 1.0` (équivalent sémantique au fallback). Pas de syntaxe
"delete" — on garde simple.

### Description outil (system prompt)

À ajouter dans `tools/prompt.go` quand `REGULATION_GOAL=on` :

> `set_goal_relevance(domain_id, relevance)` — **Appelle ce tool après
> `init_domain` ou `add_concepts`** pour décomposer le `personal_goal`
> du learner contre la liste des concepts du domaine. Pour chaque
> concept, fournis un score ∈ [0,1] : 1.0 = central au goal, 0.0 =
> orthogonal. Sémantique **incrémentale** : seuls les concepts fournis
> sont mis à jour, les autres conservent leur score précédent. Concept
> inconnu (non présent dans le graph) → erreur explicite. Si tu
> n'appelles pas ce tool, le système suppose tous les concepts
> également pertinents.
>
> `get_goal_relevance(domain_id)` — Lis le vecteur stocké et la
> liste des concepts encore sans relevance. Outil d'observation : à
> utiliser pour décider s'il faut compléter avec un nouveau
> `set_goal_relevance` (e.g. après `add_concepts`).

---

## 5. Persistance — schéma et migration

### Colonnes ajoutées à `domains`

```sql
ALTER TABLE domains ADD COLUMN graph_version INTEGER NOT NULL DEFAULT 1;
ALTER TABLE domains ADD COLUMN goal_relevance_json TEXT NOT NULL DEFAULT '';
ALTER TABLE domains ADD COLUMN goal_relevance_version INTEGER NOT NULL DEFAULT 0;
```

### Migration semantics

- **`graph_version`** :
  - Initialisé à 1 sur les domaines existants.
  - Incrémenté à `init_domain` (création → 1) et `add_concepts`
    (incrément +1).
- **`goal_relevance_json`** :
  - `''` (vide) sur les domaines existants → lecture retourne `nil` →
    accessor renvoie fallback uniforme. Comportement identique à
    pré-PR.
- **`goal_relevance_version`** :
  - 0 sur les domaines existants → est < 1 (graph_version par défaut)
    → `IsGoalRelevanceStale()` retourne `true`. Cohérent : ces
    domaines n'ont jamais eu de décomposition.

Migration ajoutée à `db/migrations.go` (idempotent par convention via
`_, _ = db.Exec(...)` du codebase).

### Modèle Go

```go
// models/domain.go (extension)
type Domain struct {
    // ... champs existants ...
    GraphVersion           int       `json:"graph_version"`
    GoalRelevanceJSON      string    `json:"-"`  // raw, parsed via helper
    GoalRelevanceVersion   int       `json:"goal_relevance_version"`
}

type GoalRelevance struct {
    ForGraphVersion int                `json:"for_graph_version"`
    Relevance       map[string]float64 `json:"relevance"`
    SetAt           time.Time          `json:"set_at"`
}
```

---

## 6. Comportement async + fallback uniforme + replacement à chaud (Q2)

### Async

- `init_domain` retourne **immédiatement**.
- La réponse JSON contient un champ `next_action` dirigé au LLM :
  ```json
  {
    "domain_id": "...",
    "concept_count": 14,
    "message": "...",
    "next_action": {
      "tool": "set_goal_relevance",
      "reason": "Décompose le personal_goal contre les 14 concepts pour activer le goal-aware routing.",
      "required": false
    }
  }
  ```
- `required: false` matérialise l'async : le LLM peut zapper sans
  casser la première session.

### Fallback uniforme

Quand `goal_relevance` est absent (`nil`) ou stale (graph version
ancienne) :

```go
// Pseudocode pour [4] ConceptSelector (à implémenter plus tard) :
relevance := domain.GoalRelevance()
if relevance == nil {
    // Fallback : 1.0 partout. Tous les concepts considérés également
    // goal-relevants. Le ConceptSelector dégénère en "argmax(1 - mastery)"
    // sur la frange.
    return uniformScore(1.0)
}
score := relevance[concept]
if !ok { score = 1.0 } // concepts non décomposés → uniform
return score
```

Cohérence : avant l'implémentation de `[4]`, le fallback uniforme est
le comportement actuel (concepts indifférenciés). Pas de régression.

### Replacement à chaud

- Chaque `set_goal_relevance` réécrit le JSON et incrémente
  `goal_relevance_version`.
- Pas de verrou : SQLite WAL sérialise l'UPDATE.
- La prochaine lecture retourne le nouveau vecteur. Pas de cache
  in-memory à invalider.
- Pas de read-modify-write : pour set_goal_relevance, un seul UPDATE
  atomique suffit (contrairement à record_interaction, qui regroupe sa
  lecture-modification-écriture dans une transaction).

---

## 7. Cas dégénérés

| Cas | Comportement | Garantie |
|-----|---------------|----------|
| `personal_goal` vide à `init_domain` | `next_action.reason` adapté : « Goal vide — décomposition optionnelle, ignorer ce tool ». Tool reste appelable mais traité comme ON noop. | Pas d'instruction ambiguë au LLM. |
| LLM n'appelle jamais `set_goal_relevance` | `goal_relevance_json` reste vide → fallback uniforme indéfini → comportement identique à pré-PR. | Aucune dégradation. |
| LLM envoie un score = -0.3 ou 1.7 | Server clamp à [0,1], compté dans `concepts_clamped`. | Pas de NaN propagé. |
| LLM envoie un concept qui n'existe pas dans `Domain.Graph.Concepts` | **Erreur explicite** citant le concept inconnu, transaction abandonnée, aucune persistance partielle. (OQ-1.4) | Sécurise contre l'hallucination LLM. |
| LLM envoie un score pour seulement 3 concepts sur 14 | Les 3 sont mergés dans le vecteur existant, les 11 autres absents → `uncovered_concepts` les liste, fallback 1.0 à la lecture. | Pas de règle « tout ou rien ». Incrémental. |
| LLM envoie un score pour C5 alors que C5 a déjà un score | Écrasement : nouvelle valeur prend la place de l'ancienne. Les autres concepts ne sont pas touchés. (OQ-1.2) | Per-concept update sans collateral. |
| LLM envoie 1000 concepts (synthèse fictive) | Validation len ≤ 500 → erreur. | Cap respecté (cohérent `tools/domain.go`). |
| Domain avec 500 concepts, JSON ~10 KB | OK, sous le cap. | Pas de pression schema. |
| 2 appels `set_goal_relevance` simultanés | Last-writer-wins via UPDATE atomique. | Acceptable, idempotent jusqu'au bruit de race. |
| `add_concepts` après `set_goal_relevance` | `graph_version` avance, `goal_relevance_version` reste, donc stale. Lecture renvoie l'ancien vecteur (les nouveaux concepts → fallback 1.0 individuel). `IsGoalRelevanceStale()` retourne true. | Pas de plantage. Re-décomposition à la discrétion du LLM. |
| Goal de 2000 chars + 500 concepts | LLM doit produire 500 paires (k,v). Faisable en un tool call mais lourd. Si timeout, l'apprenant garde fallback uniforme. | Pas de mode dégradé silencieux. |
| LLM appelle avec `relevance: nil` | Validation refuse (len(nil) == 0 → erreur "empty relevance map"). | Sémantique « set with no data » non autorisée. |

---

## 8. Stratégie de test

### 8.1 Unit — store

```go
// db/goal_relevance_test.go
TestStore_UpdateDomainGoalRelevance_PersistsJSON
TestStore_GetDomainGoalRelevance_RoundtripWithVersion
TestStore_GetDomainGoalRelevance_EmptyReturnsNil
TestStore_UpdateDomainGoalRelevance_IncrementsVersion
TestStore_UpdateDomainGoalRelevance_AddConceptsBumpsGraphVersion
TestStore_IsGoalRelevanceStale_TrueAfterAddConcepts
```

### 8.2 Unit — model accessor

```go
// models/domain_goal_relevance_test.go
TestDomain_GoalRelevance_NilWhenJSONEmpty
TestDomain_GoalRelevance_ParsesValidJSON
TestDomain_GoalRelevance_NilOnMalformedJSON  // silent fallback
TestDomain_IsGoalRelevanceStale_VersionComparison
```

### 8.3 Integration — MCP tool `set_goal_relevance`

```go
// tools/goal_relevance_test.go (set side)
TestSetGoalRelevance_Roundtrip
TestSetGoalRelevance_ClampsOutOfRange
TestSetGoalRelevance_UnknownConceptReturnsError    // OQ-1.4 strictness
TestSetGoalRelevance_IncrementalMergeKeepsExisting // OQ-1.2 per-concept
TestSetGoalRelevance_OverwritesSameConcept         // OQ-1.2 update C5 alone
TestSetGoalRelevance_PartialLeavesOthersUncovered  // returns uncovered_concepts list
TestSetGoalRelevance_FlagOff_RejectsCall
TestSetGoalRelevance_EmptyMapRejected
TestSetGoalRelevance_NaNScoreRejected
TestSetGoalRelevance_AddConceptsAfterDoesNotInvalidatePrior // OQ-1.1 contract
```

### 8.4 Integration — MCP tool `get_goal_relevance`

```go
// tools/goal_relevance_test.go (get side)
TestGetGoalRelevance_EmptyDomainReturnsAllUncovered
TestGetGoalRelevance_ReturnsStoredVector
TestGetGoalRelevance_StaleFlagAfterAddConcepts     // OQ-1.1 visibility
TestGetGoalRelevance_FlagOff_RejectsCall
TestGetGoalRelevance_OwnershipEnforced              // ne lit pas le domaine d'un autre learner
```

### 8.5 Integration — init_domain interaction

```go
TestInitDomain_ResponseIncludesNextActionWhenFlagOn
TestInitDomain_NoNextActionWhenFlagOff
TestInitDomain_NextActionEmptyGoalIsOptionalHint
TestAddConcepts_BumpsGraphVersion
```

### 8.6 Régression — fallback uniforme avant `[4]`

Aucun test runtime n'utilise `goal_relevance` aujourd'hui (pas de
consommateur). La PR ne doit pas casser de test existant. Vérification :
`go test ./...` PASS sans flag (le tool `set_goal_relevance` n'est pas
enregistré → invisible aux tests qui n'en parlent pas).

### 8.7 Pas de fixture-shift sur les tests existants

Comme pour `[7]` : on n'altère pas les fixtures de tests existants.
Toutes les nouvelles assertions vivent dans des fichiers `*_test.go`
nouveaux ou dans des sous-tests dédiés.

---

## 9. Interaction avec les composants amont/aval

### Amont

- `init_domain` (tools/domain.go) : modifié pour incrémenter
  `graph_version` à 1 et inclure `next_action` dans la réponse si
  `REGULATION_GOAL=on`.
- `add_concepts` (tools/domain.go) : modifié pour incrémenter
  `graph_version` (+1).

### Aval (futurs composants)

- `[4] ConceptSelector` : consommera `domain.GoalRelevance()`. Tant
  que `[4]` n'est pas implémenté, le signal est dormant.
- `tools/cockpit.go` (`get_cockpit_state`) : peut surfacer
  `goal_relevance_version` et un flag `goal_relevance_stale` pour
  observabilité — **pas dans cette PR**, à ouvrir comme issue
  dérivée si demandé.
- `tools/context.go` (`get_learner_context`) : idem, observabilité
  optionnelle, hors PR.

### Pas d'interaction avec [7]

Les seuils et la pertinence-au-goal sont des dimensions orthogonales.
`[1]` peut tourner indépendamment du profil `MasteryThreshold`.

---

## 10. Décisions arbitrées (Phase 1 close)

| # | Question | Décision | Conséquence |
|---|----------|----------|-------------|
| **OQ-1.1** | `add_concepts` bloquant si goal_relevance non-vide ? | **Non bloquant.** Contrat : nouveaux concepts arrivent avec relevance absent (= null à la lecture, fallback uniforme), traitement à trancher en `[4] ConceptSelector`. | `add_concepts` incrémente `graph_version` mais ne touche pas au JSON. La réponse de `add_concepts` indique en `next_action` les concepts désormais découverts (cf. revision OQ-1.3 ci-dessous). |
| **OQ-1.2** | Signal de staleness ? | **Oui, par-concept** (pas global). Une mise à jour de C5 ne marque pas C6 stale. La staleness = « concept présent dans `Domain.Graph.Concepts` ET absent de la map `relevance` ». | La sémantique de `set_goal_relevance` devient **incrémentale** (merge), pas remplacement. Réponses retournent `uncovered_concepts: []string`. |
| **OQ-1.3** | `next_action` structuré ou texte ? | **Structuré, avec champ de version.** `next_action: {version: 1, tool: "set_goal_relevance", reason: "...", required: false}`. | Le `version: 1` permet d'évoluer le payload sans casser les clients qui parseraient strictement. Les versions futures peuvent ajouter des champs ; les clients qui ignorent v>1 retombent sur le legacy. |
| **OQ-1.4** | Map vs liste ? | **Map**, avec **validation explicite** côté serveur : concept inconnu → erreur explicite citant le nom, **pas d'ignore silencieux**. | Sécurise contre les hallucinations LLM (synthétise un concept inexistant). L'erreur précise quel concept est inconnu, ce qui permet au LLM de corriger ou de demander à l'utilisateur. |

### Ajout demandé — outil de lecture `get_goal_relevance`

**Motif** : sans surface de lecture, on découvre la qualité des
vecteurs LLM seulement au moment où `[4] ConceptSelector` les consomme.
Un endpoint read-only permet l'**observation empirique** de ce que le
LLM produit, en amont de toute décision pédagogique. Évite la classe
de bug « hallucination de relevance détectée trop tard ».

**Spec** :

```go
type GetGoalRelevanceParams struct {
    DomainID string `json:"domain_id,omitempty"`
}
```

Réponse :

```json
{
  "domain_id": "dom_abc",
  "graph_version": 3,
  "for_graph_version": 2,
  "stale": true,
  "all_concepts_count": 14,
  "covered_concepts_count": 11,
  "uncovered_concepts": ["C12", "C13", "C14"],
  "relevance": {
    "Goroutines": 0.9,
    "Channels": 0.7,
    "...": ...
  },
  "set_at": "2026-05-04T22:00:00Z"
}
```

- Read-only, ownership check (le learner doit posséder le domaine).
- Pas de logique de décision : retourne ce qui est stocké, point.
- `stale = true` si `for_graph_version < graph_version`.
- Visible côté outils MCP même quand `REGULATION_GOAL=off` ? **Non** :
  même flag-gating que `set_goal_relevance`. Cohérent : si la couche
  goal n'est pas active, l'outil de lecture est silencieux. Sinon
  on observerait une surface inutile.

---

## 11. Plan de PR

### 11.1 Fichiers touchés

| Action | Fichier | Notes |
|--------|---------|-------|
| **Création** | `tools/goal_relevance.go` | handlers MCP `set_goal_relevance` + `get_goal_relevance` |
| **Création** | `tools/goal_relevance_test.go` | ~250 lignes (set + get + flag-gating) |
| **Création** | `db/goal_relevance.go` | `UpdateDomainGoalRelevance` (merge), `GetDomainGoalRelevance` |
| **Création** | `db/goal_relevance_test.go` | round-trip, merge, version, stale |
| **Modif** | `models/domain.go` | ajouter 3 champs + `GoalRelevance()` parsé, `IsGoalRelevanceStale()`, `UncoveredConcepts(graph)` |
| **Modif** | `db/migrations.go` | 3 ALTER TABLE idempotents |
| **Modif** | `db/store.go` | `CreateDomain`, `UpdateDomainGraph` incrémentent `graph_version` |
| **Modif** | `tools/domain.go` | `init_domain` + `add_concepts` retournent `next_action` (versionné, OQ-1.3) quand `REGULATION_GOAL=on` |
| **Modif** | `tools/tools.go` | enregistrer `set_goal_relevance` + `get_goal_relevance` (gated par flag) |
| **Modif** | `tools/prompt.go` | documenter les deux outils quand le flag est on |
| **Modif** | `db/schema.sql` | ajouter colonnes au CREATE TABLE pour les fresh installs |

### 11.2 Critères de merge

- [ ] `go test ./...` PASS sans flag (REGULATION_GOAL non défini)
- [ ] `REGULATION_GOAL=on go test ./...` PASS
- [ ] Migration appliquée idempotente sur DB existante (test migrations)
- [ ] Tool `set_goal_relevance` invisible dans la liste tools si flag off
- [ ] `init_domain` répond identique à pré-PR si flag off (no `next_action`)
- [ ] Doc design citée dans le commit message

### 11.3 Pas inclus dans cette PR (et pourquoi)

- Pas de consommateur runtime du vecteur (`[4] ConceptSelector` reste
  dormant). Le signal est *écrit* mais pas *lu* par les décisions.
  C'est l'objectif Q6 : observer le comportement LLM avant de brancher.
- Pas de surfaces d'observabilité dans cockpit/context (OQ-1.2 défaut
  A le mentionne mais reporte). Issue dérivée si demandée.
- Pas de validation cyclique du graphe (hors périmètre de ce
  composant).

### 11.4 Test d'intégration goal-aware (réservé pour PR `[4]`)

Conformément à la décision Q6 : *un apprenant simulé sur 20 sessions
avec `goal_relevance` non-uniforme produit une trajectoire sensiblement
différente d'un apprenant identique avec relevance uniforme. Sinon,
signal mort, on stoppe avant `[3]`.* Ce test ne peut pas exister tant
que `[4] ConceptSelector` n'est pas implémenté (pas de consommateur).
Sera ajouté dans la PR `[4]`.

---

## 12. Récapitulatif

| Aspect | Décision |
|--------|----------|
| **API** | 1 tool MCP (`set_goal_relevance`), 2 helpers store, 2 helpers model |
| **Persistance** | colonne JSON `goal_relevance_json` + 2 colonnes `int` (graph_version, goal_relevance_version) sur `domains` |
| **Flag** | `REGULATION_GOAL=on` gate registration du tool + injection de `next_action` |
| **Async** | Q2=B : init_domain retourne immédiatement, instruction non bloquante |
| **Fallback** | uniforme (1.0 partout) si JSON vide, parse error ou stale |
| **Hot-swap** | UPDATE atomique, pas de cache, last-writer-wins sur race |
| **Problème résolu** | pondération par l'objectif personnel (sous flag ON, **et** sous condition `[4]` implémenté pour que le signal soit lu) |
| **Tests** | ~6 fichiers de test, ~400 lignes |

---

**STOP.** Design `[1] GoalDecomposer` complet. En attente de validation,
ou amendements sur les 4 décisions ouvertes (OQ-1.1 add_concepts
blocking, OQ-1.2 staleness signal, OQ-1.3 next_action structuré,
OQ-1.4 map vs liste). Composant suivant après validation+implémentation :
`[5] ActionSelector`.
