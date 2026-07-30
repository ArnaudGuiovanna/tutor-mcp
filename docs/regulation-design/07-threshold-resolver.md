# [7] ThresholdResolver — Design (Phase 1)

> **Archive de conception — non normative.** Les références à
> `engine/router.go` sont des traces du chemin de migration supprimé. Le
> runtime supporté utilise le pipeline permanent de
> `engine/orchestrator.go`.

> Composant 1/7 du pipeline de régulation. Refacto pure + flag de
> bascule sémantique. Aucune logique runtime nouvelle ; débloque la
> cohérence de tous les composants suivants.
>
> Référence architecture : `docs/regulation-architecture.md` §3 [7],
> §6 Q3, §6 Q5.

---

## 1. Nature du composant

`[7] ThresholdResolver` n'est **pas** un composant runtime au sens
fonction-de-décision. C'est :

1. Une **API d'accès aux seuils de maîtrise** (3 fonctions exportées
   depuis le package `algorithms`).
2. Un **flag de bascule** entre deux profils sémantiques :
   - **Legacy** (défaut) : `BKT=0.85`, `KST=0.70`, `Mid=0.80` — préserve
     l'état actuel du runtime, fait passer tous les tests existants.
   - **Unifié** (`REGULATION_THRESHOLD=on`) : `BKT=KST=Mid=0.85` —
     résout F-2.3, F-3.10, F-1.8.
3. Un **chantier de refactor** qui remplace 11 sites du codebase par
   des appels à cette API.

Il n'a donc ni « signal consommé » ni « décision produite » au sens des
composants runtime. La structure de design ci-dessous est adaptée en
conséquence.

---

## 2. Sites refactorés (exhaustif)

Sites de production qui comparent une mastery à un seuil :

| # | Fichier:ligne | Construit actuel | Sémantique | Remplacement |
|---|----------------|-------------------|-------------|---------------|
| 1 | `algorithms/bkt.go:15` | `const BKTMasteryThreshold = 0.85` | BKT mastery (challenge eligibility) | constante supprimée — voir §3 |
| 2 | `algorithms/bkt.go:61` | `state.PMastery >= BKTMasteryThreshold` | id. | `state.PMastery >= MasteryBKT()` |
| 3 | `algorithms/kst.go:7` | `const KSTMasteryThreshold = 0.70` | KST gate (frontière prereq) | constante supprimée — voir §3 |
| 4 | `algorithms/kst.go:17,21,29,32` | `mastery[concept] >= KSTMasteryThreshold` | id. | `mastery[c] >= MasteryKST()` |
| 5 | `engine/alert.go:50` | `cs.PMastery >= algorithms.BKTMasteryThreshold` | MASTERY_READY alerte | `cs.PMastery >= algorithms.MasteryBKT()` |
| 6 | `engine/alert.go:271` | `if cs.PMastery >= 0.85 {` (literal) | comptage concepts maîtrisés (`ComputeMetacognitiveAlerts`) | `cs.PMastery >= algorithms.MasteryBKT()` |
| 7 | `engine/metacognition.go:50` | `if cs.PMastery >= 0.80 {` (literal) | hint independence — concept réputé maîtrisé pour pénaliser hint demandé | `cs.PMastery >= algorithms.MasteryMid()` |
| 8 | `engine/metacognition.go:214` | `if cs.PMastery >= 0.80 {` (literal) | hint overuse — id. | `cs.PMastery >= algorithms.MasteryMid()` |
| 9 | `engine/olm.go:60` (commentaire) + `:94` | référence à `KSTMasteryThreshold (0.70)` | docstring/cluster mastery | mise à jour comm + `algorithms.MasteryKST()` |
| 10 | `engine/olm.go:442` | `case p < 0.70:` (literal) | OLM display "Solid" cutoff | `case p < algorithms.MasteryKST():` |
| 11 | `tools/negotiation.go:123` | `if mastery[p] < 0.80 {` (literal) | sanity check prereq dans `learning_negotiation` | `mastery[p] < algorithms.MasteryMid()` |
| 12 | `db/store.go:1144` | `const masteryThreshold = 0.70` | filtre query agrégation cockpit | `algorithms.MasteryKST()` lu au point d'appel |

**Décompte** : 12 sites de production. Le brief en évoquait ~5 ; l'audit
détaillé montre que la dette est plus diffuse (Mid à 0.80 réplique sur
3 sites, KST à 0.70 sur 5).

### Sites eval/ — refactor secondaire

| # | Fichier:ligne | Sémantique | Remplacement |
|---|----------------|-------------|---------------|
| E1 | `eval/synthetic/simulator.go:34` | `const masteredCutoff = 0.70` (réplique du seuil système) | `algorithms.MasteryKST()` |
| E2 | `eval/synthetic/learner.go:26-27` | commentaire référence 0.85 / 0.70 | mise à jour commentaire |

L'eval lit le seuil pour simuler ce que fait le système. Si on bump
KST à 0.85, le simulateur doit suivre — sinon le harness mesure une
sémantique différente de celle exécutée.

### Sites NON refactorés (false positives)

Explicitement écartés de cette PR :

| Fichier:ligne | Pourquoi pas |
|----------------|--------------|
| `engine/router.go:152-153` | DifficultyTarget ceiling (0.85 max), pas une mastery threshold |
| `engine/router.go:87` | Texte de Rationale (string), pas une comparaison |
| `algorithms/irt.go:38` | `IRTIsInZPD` — bornes pCorrect ZPD (0.55-0.80), concept différent |
| `engine/motivation.go:35` | `mastery > 0.85` pour interest phase « individual » — borderline mais l'interest phase est un concept Hidi-Renninger orthogonal, **à laisser** |
| `engine/motivation.go:47` | Milestones (0.5/0.7/0.85) — palettes de progression, pas seuils de maîtrise |
| `eval/harness/metrics.go:422` | V4 bar (0.70) — bar d'évaluation, pas seuil système |
| `auth/pages.go` | Valeurs CSS |
| `tools/mastery.go:24`, `tools/transfer.go:52` | Strings de description outil — devront être mis à jour si bump promu, mais hors PR refacto |

---

## 3. API publique proposée

Nouveau fichier : **`algorithms/thresholds.go`**.

```go
// Package algorithms exposes mastery threshold accessors with a
// runtime-controlled bascule between legacy multi-threshold and unified
// single-threshold profiles.
//
// Profiles:
//   - "legacy"  (default): BKT=0.85, KST=0.70, Mid=0.80
//   - "unified" (REGULATION_THRESHOLD=on): BKT=KST=Mid=0.85
//
// The bascule resolves audit findings F-1.8, F-2.3, F-3.10 (multiple
// incompatible mastery thresholds) — see docs/audit-report.md.
//
// Promotion of "unified" to default is gated on eval/VERDICT_THRESHOLD.md
// matching or exceeding the baseline eval/VERDICT.md on V3.
package algorithms

import "os"

// MasteryBKT returns the threshold for "concept mastered per BKT P(L)".
// Used for: MASTERY_READY alert, mastery_challenge eligibility,
// transfer_challenge gate, ComputeMetacognitiveAlerts mastered count.
func MasteryBKT() float64 {
    if isUnified() { return 0.85 }
    return 0.85 // unchanged in legacy
}

// MasteryKST returns the threshold for "prereq considered satisfied to
// unlock a successor in the KST frontier". Used by ComputeFrontier,
// ConceptStatus, OLM cluster classification, cockpit aggregations.
func MasteryKST() float64 {
    if isUnified() { return 0.85 }
    return 0.70
}

// MasteryMid returns the intermediate threshold used by hint-independence
// checks and learning_negotiation prereq sanity. In the unified profile,
// it collapses to MasteryBKT.
func MasteryMid() float64 {
    if isUnified() { return 0.85 }
    return 0.80
}

// isUnified reads the REGULATION_THRESHOLD env at each call. Cheap, no
// cache, allows tests to use t.Setenv without a reset helper. If/when
// profiling shows this is hot, replace with sync.OnceValue.
func isUnified() bool {
    return os.Getenv("REGULATION_THRESHOLD") == "on"
}
```

### Conventions de nommage retenues

- `MasteryBKT` / `MasteryKST` / `MasteryMid` — accesseurs nommés par
  *rôle* sémantique, pas par valeur. Le call site dit ce qu'il pense
  vérifier (« est-ce que ce concept est BKT-maîtrisé ? »), pas ce qu'il
  compare numériquement.
- Pas de pluriel `Thresholds()` qui retourne un struct — verbosité
  excessive (cf §6 Q1 du présent doc).
- Pas d'enum `MasteryRole` paramétrant une fn unique — un appel reste
  une seule fn lisible.

### Constantes supprimées

`BKTMasteryThreshold` (`algorithms/bkt.go:15`) et `KSTMasteryThreshold`
(`algorithms/kst.go:7`) sont **supprimées** dans la même PR. Aucune
extension externe ne les consomme (vérifié : `grep -rn
BKTMasteryThreshold` et `KSTMasteryThreshold` retournent uniquement
des sites internes au repo, listés en §2).

Pas de période de dépréciation — c'est un repo single-tenant
self-hosted, l'opérateur déploie HEAD.

---

## 4. Mécanisme du flag

### 4.1 Read-each-call vs cache

**Choix retenu** : lecture de `os.Getenv` à chaque appel d'accesseur.
Coût : ~50 ns par appel (négligeable). Bénéfices :

- Tests utilisent `t.Setenv("REGULATION_THRESHOLD", "on")` sans helper
  de reset.
- Pas de variable globale mutable, pas d'init() à coupler.
- Toggle à chaud possible (utile en dev).

Si profiling montre que c'est hot (peu probable au niveau
`get_next_activity` qui appelle ces fonctions ≤ 50 fois), passer à
`sync.OnceValue` ou `sync.OnceValuesGo123+`.

### 4.2 Strict equality

`os.Getenv("REGULATION_THRESHOLD") == "on"` — strict. Tout autre
valeur (`true`, `1`, `enabled`, `ON`, `yes`) → legacy. Documenté dans
le commentaire de package. Évite les bugs subtils où le déploiement
pense activer mais ne l'a pas fait.

### 4.3 Pas de fallback partiel

Le flag est all-or-nothing : tous les seuils basculent ensemble, ou
aucun. Pas de `REGULATION_THRESHOLD_KST=on` séparé. Raison : la
cohérence inter-seuils est précisément ce qu'on essaie de garantir ;
un sous-flag ré-introduirait la pathologie.

---

## 5. Cas dégénérés

| Cas | Comportement | Garantie |
|-----|--------------|----------|
| `REGULATION_THRESHOLD` non défini | Legacy (0.85/0.70/0.80) | Tous les tests existants passent. |
| `REGULATION_THRESHOLD=on` | Unifié (0.85/0.85/0.85) | Nouvelle suite de tests `*_unified_test.go` couvre. |
| `REGULATION_THRESHOLD=ON` (caps) | Legacy | Documenté en commentaire de package. |
| `REGULATION_THRESHOLD=true` | Legacy | id. |
| `REGULATION_THRESHOLD=""` (string vide) | Legacy | id. |
| Env supprimée mid-process | Bascule à legacy au prochain appel | Acceptable pour dev ; en prod, env stable au boot. |
| `mastery = 0.85` exactement | `MasteryBKT()` retourne `≥ 0.85` true | Comparaison `>=`, pas `>`. |
| `mastery = NaN` (cf F-1.3) | Comparaison NaN ≥ 0.85 → `false` | Reste cohérent avec comportement actuel. Hors scope (issue dédiée). |

### 5.1 Risque de drift partiel pendant la PR

Pendant le refactor, si un site est oublié, il garde un literal `0.70`
ou `0.80` au lieu de lire l'accesseur. Sous flag ON, on a alors un
système partiellement unifié. **Mitigation** : la PR inclut un test de
non-régression qui grep le repo pour les literals de mastery (voir
§7.3).

---

## 6. Stratégie de test

### 6.1 Régression — flag OFF (défaut)

Tous les tests existants tournent **inchangés**. La PR ne modifie pas
les fixtures. Si un test casse, c'est un bug du refacto, pas une
volonté.

```bash
go test ./...
# tous PASS, identique à pré-PR
```

### 6.2 Tests dédiés flag ON

Nouveau fichier `algorithms/thresholds_test.go` :

```go
func TestMasteryThresholds_Legacy(t *testing.T) {
    // env not set
    if MasteryBKT() != 0.85 { t.Fail() }
    if MasteryKST() != 0.70 { t.Fail() }
    if MasteryMid() != 0.80 { t.Fail() }
}

func TestMasteryThresholds_Unified(t *testing.T) {
    t.Setenv("REGULATION_THRESHOLD", "on")
    if MasteryBKT() != 0.85 { t.Fail() }
    if MasteryKST() != 0.85 { t.Fail() }
    if MasteryMid() != 0.85 { t.Fail() }
}

func TestMasteryThresholds_StrictEquality(t *testing.T) {
    cases := []string{"ON", "true", "1", "yes", "enabled", ""}
    for _, v := range cases {
        t.Setenv("REGULATION_THRESHOLD", v)
        if MasteryKST() != 0.70 {
            t.Fatalf("flag=%q should be ignored, got %f", v, MasteryKST())
        }
    }
}
```

### 6.3 Garde anti-drift — meta-test

Un test qui parcourt le repo et échoue si un literal mastery (`0.70`,
`0.80`, `0.85` dans un contexte de comparaison `PMastery` ou
`mastery`) est trouvé hors d'`algorithms/thresholds.go`.

Implémentation : grep par regex sur `*.go` excluant `_test.go` et
`thresholds.go`. Liste blanche pour les false positives identifiés en
§2 (motivation milestones, IRT bornes, router clamp).

```go
// algorithms/thresholds_drift_test.go
//go:build !no_drift_check
func TestNoLiteralMasteryThresholds(t *testing.T) {
    allowed := map[string]bool{
        "engine/router.go:152":       true, // difficulty clamp
        "engine/router.go:153":       true,
        "engine/motivation.go:35":    true, // interest phase
        "engine/motivation.go:47":    true, // milestones comment
        "algorithms/irt.go:38":       true, // ZPD bounds
    }
    // walk repo, regex "[<>=]+\s*0\.(70|80|85)" within ".*[Mm]astery.*",
    // assert each match is in `allowed` or in algorithms/thresholds.go
}
```

Coût : 1 fichier, ~50 lignes. Bénéfice : aucun drift ne passe en
silence ; la maintenance future doit explicitement étendre la liste.

### 6.4 Tests d'intégration — sémantique sous flag ON

Nouveau dossier `algorithms/threshold_integration_test.go` qui exerce
chaque algo (BKT, KST) avec un fixture ambigu :

- Fixture A : concept à `mastery = 0.75`, KST graph `{B requires A}`.
  - Flag OFF : `ComputeFrontier` inclut B (A passe le seuil 0.70).
  - Flag ON : `ComputeFrontier` n'inclut PAS B (A ne passe pas 0.85).
- Fixture B : `BKTIsMastered` sur PMastery = 0.81.
  - Flag OFF : false.
  - Flag ON : false (BKT n'a pas changé, toujours 0.85).
- Fixture C : `metacognition` hint independence sur PMastery = 0.83.
  - Flag OFF : « mastered » (≥ 0.80).
  - Flag ON : « not mastered » (< 0.85).

Ces 3 cas isolent la bascule de chaque accesseur indépendamment.

### 6.5 Pas de modification des fixtures existantes

Garde-fou critique : toutes les modifs de tests existants
(*_test.go déjà au repo) sont **interdites** dans cette PR. Si un test
existant suppose KST=0.70, il continue à le supposer (flag OFF). Si on
veut un test qui suppose KST=0.85, on en crée un nouveau (flag ON).
Cela garantit la régression par construction.

---

## 7. Eval harness gate (Q3 résolu)

Avant promotion de `unified` en défaut, protocole obligatoire :

### 7.1 Run baseline

```bash
unset REGULATION_THRESHOLD
cd eval && go run ./cmd/harness -seed 42 -learners 100 -experiments 1,2,3,4
# produces eval/output/{summary,per_learner,...}.json
# Save eval/VERDICT.md baseline (current = 2026-05-03)
```

### 7.2 Run unified

```bash
export REGULATION_THRESHOLD=on
cd eval && go run ./cmd/harness -seed 42 -learners 100 -experiments 1,2,3,4
# produces same outputs
# Compute V1/V2/V3/V4 via metrics.go
# Save as eval/VERDICT_THRESHOLD.md
```

### 7.3 Critère de promotion

Promotion par défaut autorisée **si et seulement si** :

| Métrique | Condition |
|----------|-----------|
| **V3 (routing helps)** | `Δmastery_unified ≥ Δmastery_baseline - 0.01` (tolérance 1 point absolu) |
| **V1 (BKT calibrated)** | ECE_unified ≤ ECE_baseline + 0.005 |
| **V4 (FSRS hits)** | hit_rate_unified ≥ hit_rate_baseline - 0.02 |
| **V1' (cold-start tail)** | doit toujours passer (≤ 0.10) |

V2 (Spearman ρ) ignoré pour la promotion — déjà connu instable sous
routing focalisé (cf `eval/PLEARN_FINDINGS.md`).

Si une métrique décroche au-delà de la tolérance, la PR de promotion
default n'est **pas mergée**. Le flag reste opt-in et on ouvre une issue
diagnostic.

### 7.4 Découplage refacto / promotion

Important : la **PR de refacto** (cette PR) **n'inclut pas** la
promotion par défaut. Elle :

1. Ajoute `algorithms/thresholds.go` + tests.
2. Refactor les 12 sites de production.
3. Refactor les 2 sites eval secondaires (E1, E2).
4. Garde le flag par défaut OFF (legacy).

La promotion par défaut sera une PR séparée (changement d'1 ligne :
le défaut du `isUnified()`) déclenchée après run du harness §7.1-7.3.

---

## 8. Décisions ouvertes spécifiques au composant

### OQ-7.1 — Sites eval/ dans la même PR ou séparée ?

**A.** Refacto E1+E2 (eval/synthetic) dans la même PR que le refacto
production. Bénéfice : le harness lit l'accesseur, donc tourne
fidèlement le profil actif. Inconvénient : couple le PR à eval/.

**B.** PR séparée pour eval/. La refacto production ne touche pas eval/.
Inconvénient : pendant la fenêtre entre les deux PR, le harness mesure
sur 0.70 hardcodé même si flag ON.

**Mon défaut** : A. Le harness gate (§7) requiert que eval/ lise le
profil actif ; sans ça, VERDICT_THRESHOLD.md ne mesure pas ce qu'il
prétend mesurer. La couplage de PR est le moindre mal.

### OQ-7.2 — `engine/motivation.go:35` (interest phase « individual »)

`mastery > 0.85` déclenche le passage en interest phase « individual »
(Hidi-Renninger). Est-ce conceptuellement une mastery threshold ?

**A.** Oui — toute comparaison `mastery > X` doit lire un accesseur. À
inclure dans le refacto avec `algorithms.MasteryBKT()`.

**B.** Non — c'est un déclencheur d'interest phase, théoriquement
indépendant de la définition « concept BKT-maîtrisé ». Garder literal,
documenter le choix.

**Mon défaut** : B. Hidi-Renninger 2006 propose ces phases avec leurs
propres seuils empiriques ; les coupler à BKT est une décision
théorique non démontrée. Garder literal, ajouter commentaire référant.

### OQ-7.3 — Emplacement de `VERDICT_THRESHOLD.md`

**A.** `eval/VERDICT_THRESHOLD.md` — adjacent au baseline. Lisible par
qui regarde déjà eval/.

**B.** `docs/regulation-design/07-threshold-eval-verdict.md` — co-localisé
avec ce design doc. Cohérent côté traçabilité Phase 1.

**Mon défaut** : A. eval/ est l'instrument R&D du projet ; les VERDICT
résident là, c'est la convention. Lien depuis ce doc suffit.

### OQ-7.4 — Granularité `MasteryMid` vs futurs split

`MasteryMid` regroupe deux usages : hint-independence (engine/metacognition)
et negotiation prereq sanity (tools/negotiation). Ils pourraient
diverger sémantiquement à terme.

**A.** Garder une seule fn. Si divergence, splitter à ce moment-là.
**B.** Splitter dès maintenant : `MasteryHintIndependence`,
`MasteryNegotiationPrereq`. Plus précis, plus verbeux.

**Mon défaut** : A. YAGNI ; pas d'évidence aujourd'hui que les deux
doivent diverger. Le coût de splitter plus tard est faible (renommage
de 3 sites).

---

## 9. Plan de PR

### 9.1 Diff attendu

| Action | Fichier | Lignes |
|--------|---------|--------|
| **Création** | `algorithms/thresholds.go` | ~50 |
| **Création** | `algorithms/thresholds_test.go` | ~80 |
| **Création** | `algorithms/threshold_integration_test.go` | ~100 |
| **Création** | `algorithms/thresholds_drift_test.go` | ~50 |
| **Modif** | `algorithms/bkt.go` | -1 const, 1 ligne refactor |
| **Modif** | `algorithms/kst.go` | -1 const, 4 lignes refactor |
| **Modif** | `engine/alert.go` | 2 sites refactor |
| **Modif** | `engine/metacognition.go` | 2 sites refactor |
| **Modif** | `engine/olm.go` | 2 sites refactor + comment |
| **Modif** | `tools/negotiation.go` | 1 site refactor |
| **Modif** | `db/store.go` | -1 const, 1 ligne refactor |
| **Modif** | `eval/synthetic/simulator.go` | -1 const, 1 site refactor |
| **Modif** | `eval/synthetic/learner.go` | comment update |
| **Doc** | commit message | cite F-1.8, F-2.3, F-3.10 + lien design doc |

Total ~12 fichiers touchés, ~280 lignes nettes ajoutées (gros morceau =
les tests).

### 9.2 Critères de merge

- [ ] `go test ./...` PASS sans flag
- [ ] `REGULATION_THRESHOLD=on go test ./...` PASS
- [ ] `algorithms/thresholds_drift_test.go` PASS (aucun literal hors
      whitelist)
- [ ] `tools/prompt.go` non modifié (pas de promesses ITS révisées
      avant rollout — Theme E hors scope)
- [ ] commit message cite F-1.8, F-2.3, F-3.10 et lie ce design doc
- [ ] aucune fixture de test existante modifiée

### 9.3 Post-merge — promotion default (PR séparée)

Après merge de la PR refacto, le run harness peut être déclenché à
tout moment. La promotion par défaut suit le protocole §7.

Pas de timeline imposée. Le runtime tourne en legacy par défaut tant
que le harness n'a pas validé.

---

## 10. Récapitulatif

| Aspect | Décision |
|--------|----------|
| **API** | 3 fns `MasteryBKT/KST/Mid` dans `algorithms/thresholds.go` |
| **Flag** | `REGULATION_THRESHOLD=on` strict equality, lookup-each-call |
| **Constantes legacy** | Supprimées (pas de période de dépréciation) |
| **Sites refactorés** | 12 prod + 2 eval = 14 |
| **Anti-drift** | Test meta-greppant les literals, whitelist explicite |
| **Promotion default** | Conditionnée à eval/VERDICT_THRESHOLD.md sur V1, V3, V4 |
| **Findings résolus** | F-1.8, F-2.3, F-3.10 (sous flag ON) |

---

**STOP.** Design [7] ThresholdResolver complet. En attente de
validation, ou amendements sur les 4 décisions ouvertes
(OQ-7.1 PR couplée eval/, OQ-7.2 motivation.go:35, OQ-7.3 emplacement
VERDICT_THRESHOLD, OQ-7.4 split MasteryMid). Composant suivant :
`[1] GoalDecomposer`.
