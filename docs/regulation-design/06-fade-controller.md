# [6] FadeController — Design (Phase 1)

> **Document historique, non normatif pour le runtime actuel.** `Decide` reste
> une API autonome de compatibilité ; ses paramètres ne pilotent ni les aides,
> ni la sélection d'activité, ni la cadence webhook. `REGULATION_FADE` expose
> seulement des diagnostics descriptifs. Voir les
> [notes runtime actuelles](../runtime-pedagogique-2026-09.md) et la
> [configuration](../configuration.md). Les branchements décrits ci-dessous
> appartiennent à l'ancienne proposition.

> Composant 6/7 du pipeline de regulation. Module **post-decision** :
> isole de la chaine de selection (Phase FSM -> Concept -> Action ->
> Gate). Lit `autonomy_score` et `autonomy.Trend` pour produire 4
> parametres de "handover" qui sont consommes en aval (verbosite des
> hints, cadence webhook, agressivite ZPD, reviews proactifs).
>
> Comble la lacune ou `autonomy_score` etait calcule mais non
> consomme en aval, et donne a l'intent design "le systeme se rend
> progressivement inutile" une implementation concrete.
>
> Reference architecture : `README.md` §"Regulation Pipeline" ligne 89,
> `engine/metacognition.go` (calcul autonomy + trend), `engine/olm.go`
> (champs `AutonomyTrend`).

---

## 1. Nature du composant

`[6] FadeController` est un **module post-decision**. Il ne modifie ni
le concept choisi, ni le type d'activite, ni la difficulte cible
calculee par le pipeline amont. Il produit un **bundle de parametres de
handover** (`FadeParams`) que les couches de presentation et de
scheduling consomment :

- la couche `motivation_brief` reduit la verbosite et peut supprimer
  les hints quand l'apprenant n'en a plus besoin ;
- le scheduler de webhooks abaisse la frequence des nudges quand
  l'apprenant gere lui-meme sa pratique ;
- le selecteur d'action (`[5]`) peut, dans une iteration future, lire
  `ZPDAggressiveness` pour ajuster la pCorrect cible ;
- la cadence de reviews proactifs (FSRS) peut activer ou desactiver
  les rappels.

C'est une fonction **pure** : `Decide(score float64, trend
AutonomyTrend) FadeParams`. Aucun acces store, aucun side-effect. La
collecte des inputs et l'application des outputs vivent dans
`tools/activity.go` (chaine d'orchestration), strictement post-
`engine.Orchestrate`.

### Pourquoi post-decision et non integre au PhaseController

Trois raisons :

1. **Separation des roles**. La phase FSM repond a "ou en est
   l'apprenant dans la maitrise du graphe ?". Le fade repond a "de
   combien de tutorat l'apprenant a-t-il encore besoin ?". Ces deux
   questions sont orthogonales : un apprenant en MAINTENANCE peut
   encore avoir besoin d'aide ; un apprenant en INSTRUCTION peut deja
   etre tres autonome sur la portion qu'il connait.
2. **Couplage minimal**. En vivant apres `Orchestrate`, le fade ne
   peut pas casser la garantie d'invariance des outputs orchestrateur
   sous le flag OFF. C'est ce qui permet le test "byte-identique
   activity output flag-off vs pre-PR".
3. **Independance de cycle de vie**. Le fade peut evoluer sans
   toucher aux fonctions pures `SelectConcept` / `SelectAction` /
   `ApplyGate`. Il est plus jeune que ces fonctions et son
   parametrage est plus susceptible de bouger.

---

## 2. Inputs

| Champ | Type | Source | Notes |
|-------|------|--------|-------|
| `score` | `float64` | `models.AutonomyMetrics.Score` calcule par `engine.ComputeAutonomyMetrics` (`engine/metacognition.go`) | Borne `[0, 1]`. 4 composantes a 25% chacune : initiative, calibration accuracy, hint independence, proactive review rate. |
| `trend` | `AutonomyTrend` (alias `string`) | `engine.ComputeAutonomyTrendExported` (`engine/metacognition.go:173`) | Valeurs : `"improving"`, `"stable"`, `"declining"`. Calcule en comparant la moyenne des 5 derniers scores aux 5 precedents (delta > 0.05). |

### 2.1 Cold start

Si `score == 0` ET il n'existe aucun affect record en base, on est en
*cold start*. Le score retourne par `ComputeAutonomyMetrics` peut etre
~0.25 (calibration accuracy = 1.0 si bias = 0, autres composantes = 0)
voire indefini.

**Strategie** : le `FadeController` ne distingue pas explicitement
`cold_start`. Tout score `< 0.3` tombe dans le tier `low` qui retourne
des defaults conservateurs (full hints, daily webhooks, gentle ZPD,
proactive reviews ON). C'est exactement le comportement souhaite pour
un nouvel apprenant. La distinction "vraiment cold" vs "score genuinely
low" est laissee aux couches amont (qui peuvent injecter un score = 0
si elles veulent forcer le tier low).

### 2.2 Contrat d'appel

Le `FadeController` est appele **une fois par `get_next_activity`**,
apres que l'orchestrateur a produit l'`Activity`. Le cout est
negligeable (lecture des affect records pour calculer score+trend ; ces
lectures se font deja pour `metacognitive_mirror`). Pas de cache ; pas
de memoization.

---

## 3. Outputs

`FadeParams` est un struct a 4 champs :

```go
type FadeParams struct {
    HintLevel              HintLevel              // full | partial | none
    WebhookFrequency       WebhookFrequency       // daily | weekly | off
    ZPDAggressiveness      ZPDAggressiveness      // gentle | normal | push
    ProactiveReviewEnabled bool
}
```

### 3.1 Consommateurs

| Champ | Consommateur | Effet |
|-------|--------------|-------|
| `HintLevel` | `engine/motivation.go` (via `tools/activity.go`) | `none` -> brief.Kind = "" et brief.Instruction reduit a une ligne minimaliste. `partial` -> brief retenu mais Instruction abregee. `full` -> brief inchange (comportement actuel). |
| `WebhookFrequency` | `engine/scheduler.go` (via persistence sur la ligne domain ou learner) | `off` -> aucun nudge envoye. `weekly` -> dispatch reduit (1x/semaine au lieu de 1x/jour). `daily` -> comportement actuel. **NB** : la PR initiale persiste le champ et l'attache au resultat de `get_next_activity` ; l'integration scheduler effective est issue de suivi (cf §9). |
| `ZPDAggressiveness` | `engine/action_selector.go` (futur) | `gentle` -> baisse pCorrect cible (~0.78). `normal` -> 0.70 actuel. `push` -> ~0.62, plus de challenge. **NB** : la PR initiale ne wire pas ; champ expose pour permettre la consommation future sans changement de signature. |
| `ProactiveReviewEnabled` | `engine/scheduler.go` (FSRS recall jobs) | `false` -> pas de proactive review surface. `true` -> comportement actuel. **NB** : scope reporte au suivi. |

Le **scope d'implementation initial** (cette PR) est : pure function +
wiring `HintLevel` -> motivation. Les autres champs sont presents dans
`FadeParams` mais leur consommation est documentee comme follow-up.
C'est la bonne segmentation : on livre la table de mapping et la
machine pure ; chaque consommateur sera cable dans une PR dediee qui
peut s'occuper des subtilites de migration de schema (scheduler) et de
calibration (action selector).

---

## 4. Mapping autonomy -> FadeParams

### 4.1 Tiers de score

Trois tiers de score, frontieres a `0.3` et `0.7`. Choix justifie par
le calcul de `ComputeAutonomyMetrics` : 4 composantes a 25% chacune
signifie qu'une valeur a 0.3 correspond a "1 composante au max, 3 a
zero" -> apprenant qui demarre. 0.7 correspond a "3 composantes au max,
1 a zero ou faible" -> apprenant largement autonome. Au-dessus de 0.7,
on est dans le regime de fading actif.

| Tier | Score | Defaults (trend = stable) |
|------|-------|----------------------------|
| **Low** | `score < 0.3` | `HintLevel = full`, `WebhookFrequency = daily`, `ZPDAggressiveness = gentle`, `ProactiveReviewEnabled = true` |
| **Mid** | `0.3 <= score < 0.7` | `HintLevel = partial`, `WebhookFrequency = weekly`, `ZPDAggressiveness = normal`, `ProactiveReviewEnabled = true` |
| **High** | `score >= 0.7` | `HintLevel = none`, `WebhookFrequency = off`, `ZPDAggressiveness = push`, `ProactiveReviewEnabled = false` |

### 4.2 Modulation par trend

Le score donne le tier de base. La trend module **un cran** dans la
direction qui aide l'apprenant :

- `improving` : on **avance** d'un cran vers plus d'autonomie. Un
  apprenant `Mid` qui s'ameliore obtient les defaults `High` sur
  `HintLevel` et `WebhookFrequency`. Un `High` qui s'ameliore reste
  `High` (pas de cran au-dessus).
- `stable` : tier de base inchange.
- `declining` : on **recule** d'un cran. Un apprenant `Mid` qui decline
  obtient les defaults `Low` sur `HintLevel`, `WebhookFrequency`. Un
  `High` qui decline retombe a `Mid`. C'est le **fast fallback** vers
  plus de support.

Cette modulation s'applique aux 4 outputs simultanement (pas de
sub-modulation par champ). C'est un choix de simplicite et de
lisibilite — l'operator ou l'apprenant peut interpreter "je suis Mid
declining" comme "le systeme se comporte temporairement comme si
j'etais Low".

### 4.3 Table complete (9 cellules)

| Score \ Trend | declining | stable | improving |
|---------------|-----------|--------|-----------|
| **Low** (`<0.3`) | Low | Low | Mid |
| **Mid** (`0.3-0.7`) | Low | Mid | High |
| **High** (`>=0.7`) | Mid | High | High |

Chaque cellule resout vers le tier indique, qui determine les 4
parametres selon §4.1.

### 4.4 Hysteresis et oscillations

`computeAutonomyTrend` necessite >=6 scores pour produire autre chose
que `"stable"` (cf `engine/metacognition.go:137`). En-dessous de ce
seuil, la trend est `"stable"` et le tier de base s'applique. C'est
notre premier garde-fou contre l'oscillation precoce.

**Risque d'oscillation autour des frontieres** (e.g. score = 0.298
declining vs 0.302 improving) : la trend window de 5+5 scores avec
delta > 0.05 introduit deja un seuil non-trivial sur les variations
courtes. Pas de hysteresis additionnel sur le score lui-meme dans la
v1 — si le pattern se manifeste en prod, on l'ajoutera (typiquement
+/- 0.02 autour des frontieres).

### 4.5 Edge cases

| Cas | Comportement | Justification |
|-----|--------------|---------------|
| `score < 0` | clamp a 0, tier Low | Robustesse ; ne devrait pas arriver mais on ne plante pas. |
| `score > 1` | clamp a 1, tier High | Idem. |
| `score = NaN` | comportement Go : NaN < 0.3 = false, NaN >= 0.7 = false -> tier Mid | Acceptable. NaN est traite comme Mid faute de mieux ; isole une eventuelle bug amont sans escalader. Cas hors scope, issue dediee si reproductible. |
| `trend = ""` (vide) | traite comme `stable` | Cohesion avec le default de `computeAutonomyTrend` quand <6 scores. |
| `trend = "garbage"` | traite comme `stable` | Robustesse. Switch defensif. |

---

## 5. Mecanisme du flag

### 5.1 `REGULATION_FADE=on` strict

Convention identique aux autres flags de regulation
(cf `tools/prompt.go` : `regulationPhaseEnabled`,
`regulationActionEnabled`, etc.). Le flag est lu via
`os.Getenv("REGULATION_FADE") == "on"`. Toute autre valeur
(unset, "ON", "true", "1") signifie OFF.

**Difference avec les autres flags** : `REGULATION_FADE` est
**default-OFF**. Les autres regulation flags sont default-on
(opt-out via "off"). Le fade est plus jeune, son effet se voit
directement sur l'apprenant (verbosite reduite, webhooks coupes), donc
on ne l'active pas par defaut tant que le harness eval n'a pas
valide.

```go
// tools/activity.go
func regulationFadeEnabled() bool {
    return os.Getenv("REGULATION_FADE") == "on"
}
```

### 5.2 Comportement sous flag OFF

Aucun appel a `Decide`. Aucun champ ajoute au resultat de
`get_next_activity`. `MotivationBrief` produit comme avant. Le
scheduler ne lit aucun champ. C'est strictement le comportement
pre-PR. **Test de non-regression** : `tools/activity_test.go`
verifie que les outputs `activity`, `motivation_brief`, `tutor_mode`
etc. sont inchanges flag OFF (cf §7.1).

### 5.3 Comportement sous flag ON

`Decide` est appele post-Orchestrate. `FadeParams` est :
1. utilise pour moduler `motivation_brief` (verbosite / suppression
   selon `HintLevel`),
2. ajoute au resultat JSON sous la cle `fade_params` pour observer
   l'effet et permettre au scheduler de le lire.

---

## 6. API

### 6.1 Types

```go
// engine/fade_controller.go
package engine

type AutonomyTrend string

const (
    AutonomyTrendImproving AutonomyTrend = "improving"
    AutonomyTrendStable    AutonomyTrend = "stable"
    AutonomyTrendDeclining AutonomyTrend = "declining"
)

type HintLevel string

const (
    HintLevelFull    HintLevel = "full"
    HintLevelPartial HintLevel = "partial"
    HintLevelNone    HintLevel = "none"
)

type WebhookFrequency string

const (
    WebhookFrequencyDaily  WebhookFrequency = "daily"
    WebhookFrequencyWeekly WebhookFrequency = "weekly"
    WebhookFrequencyOff    WebhookFrequency = "off"
)

type ZPDAggressiveness string

const (
    ZPDAggressivenessGentle ZPDAggressiveness = "gentle"
    ZPDAggressivenessNormal ZPDAggressiveness = "normal"
    ZPDAggressivenessPush   ZPDAggressiveness = "push"
)

type FadeParams struct {
    HintLevel              HintLevel              `json:"hint_level"`
    WebhookFrequency       WebhookFrequency       `json:"webhook_frequency"`
    ZPDAggressiveness      ZPDAggressiveness      `json:"zpd_aggressiveness"`
    ProactiveReviewEnabled bool                   `json:"proactive_review_enabled"`
}

// Decide is the pure mapping. No store access, no clock, deterministic.
func Decide(score float64, trend AutonomyTrend) FadeParams { ... }
```

### 6.2 Convention de nommage

Le constructeur s'appelle `Decide` (pas `DecideFade` ou `Apply`) — il
vit dans `package engine` mais utilise dans le package est court. Aligne
sur `engine.Orchestrate`, `engine.Route` (pas `OrchestrateRegulation`).

Les enums sont des `type Foo string` plutot que des `int`. Choix :
serialisation JSON lisible cote LLM et tests, coherent avec
`models.ActivityType` et `models.MotivationKind*` qui sont des
strings.

---

## 7. Strategie de test

### 7.1 Tests unitaires (`engine/fade_controller_test.go`)

Trois groupes :

1. **Table-test couvrant les 9 cellules** de §4.3. Chaque cellule
   verifie les 4 champs `FadeParams` retournes. Score d'entree pris
   au milieu du tier (0.15, 0.5, 0.85) pour eviter les frontieres.
2. **Tests de frontiere** : score = 0.3 exactement (Mid), score = 0.7
   exactement (High), score = 0.0, score = 1.0, score = -0.1 (clamp),
   score = 1.1 (clamp).
3. **Trend defensifs** : `""`, `"garbage"`, `"IMPROVING"` (case-sensitive
   donc traite comme stable).

Total : ~25 cas. Pure function, pas de fixture, pas de t.Setenv.

### 7.2 Test d'integration (`tools/activity_test.go`)

Scenario : on insert 12 affect_states avec autonomy_score croissant
(0.1 -> 0.85, lineaire). Avec `REGULATION_FADE=on`, on appelle
`get_next_activity` apres avoir insert progressivement plus d'affect
records (3, 6, 9, 12). On extrait le `motivation_brief.instruction` et
on assert :

- la **longueur de Instruction** est non-croissante au fur et a mesure
  que le score monte,
- le `fade_params.hint_level` decroit (`full` -> `partial` -> `none`)
  au passage des seuils.

Cf `engine/computeAutonomyTrend` qui necessite >=6 scores pour donner
autre chose que `stable` : le test insert dans cet ordre pour
declencher `improving` au step 4.

### 7.3 Test de non-regression flag OFF

`TestGetNextActivity_FlagOff_NoFadeFields` : sans `REGULATION_FADE=on`,
le resultat JSON ne contient pas `fade_params`, et `motivation_brief`
est strictement equivalent au comportement pre-PR. Garantit que la
chaine d'orchestration est byte-identique.

---

## 8. Plan de PR

### 8.1 Fichiers crees

| Fichier | Lignes |
|---------|--------|
| `docs/regulation-design/06-fade-controller.md` | ~400 (ce doc) |
| `engine/fade_controller.go` | ~120 |
| `engine/fade_controller_test.go` | ~180 |

### 8.2 Fichiers modifies

| Fichier | Changement |
|---------|------------|
| `tools/prompt.go` | +1 fonction `regulationFadeEnabled` |
| `tools/activity.go` | wiring post-Orchestrate, gate sur flag, application HintLevel a motivation_brief |
| `tools/activity_test.go` | +1 test integration, +1 test non-regression flag OFF |
| `README.md` | ligne 89 : status Pending -> Shipped (opt-in) ; tableau flags : ajout `REGULATION_FADE` |

### 8.3 Critere de merge

- [x] design doc complet
- [ ] `go test ./...` PASS sans flag (non-regression)
- [ ] `REGULATION_FADE=on go test ./...` PASS (integration)
- [ ] tests table-driven sur les 9 cellules + edges
- [ ] commit message lie ce design doc
- [ ] pas de modification d'`engine.Orchestrate` ni des fonctions
      pures `SelectConcept`/`SelectAction`/`ApplyGate`/`EvaluatePhase`
- [ ] flag default OFF, strict equality `"on"`

---

## 9. Decisions ouvertes

### OQ-6.1 — Wiring scheduler dans cette PR ?

**A.** Oui. La PR persiste `WebhookFrequency` sur la ligne `domains`
ou `learners`, et `engine/scheduler.go:dispatchQueued` filtre sur ce
champ. Le scope croit d'une migration de schema + 30 lignes
scheduler.

**B.** Non, follow-up. La PR attache `fade_params` au resultat de
`get_next_activity` mais ne touche ni schema, ni scheduler. Le LLM
peut deja consommer le champ via le system prompt (futur appendix
`fadeAppendix`). Le scheduler bouge dans une PR dediee qui peut traiter
proprement la migration et les tests scheduler.

**Defaut retenu** : **B**. Raisons : (1) la migration de schema force
une release coordonnee, (2) le scheduler a sa propre suite de tests
(`engine/scheduler_*_test.go`) qui meriterait une mise a jour
specifique, (3) le brief du ticket autorise explicitement de scoper a
"motivation-only wiring + follow-up".

### OQ-6.2 — `ZPDAggressiveness` consomme par `[5]` ActionSelector dans cette PR ?

**A.** Oui. Modifie `engine/action_selector.go` pour ajuster la
pCorrect cible selon `ZPDAggressiveness`.

**B.** Non, follow-up. Le champ est present dans `FadeParams` et serialise,
mais `engine/action_selector.go` ne le lit pas encore.

**Defaut retenu** : **B**. Raisons : (1) le calibre `IRT_θ + 0.847`
est un parametre central documente dans plusieurs design docs ; le
modifier merite une PR dediee avec eval harness avant/apres, (2) on
respecte le principe "ship la pure function + flag stub, evite
d'alterer le routage core dans la meme PR".

### OQ-6.3 — Granularite des cellules : modulation par champ vs par tier ?

**A.** Modulation **par tier** (choix actuel) : la trend deplace le
tier ; les 4 outputs suivent ensemble.

**B.** Modulation **par champ** : par exemple `improving` modifie
`HintLevel` mais pas `WebhookFrequency`.

**Defaut retenu** : **A**. Plus simple a expliquer, plus simple a
tester (9 cellules au lieu de 81), reflete une intuition coherente
("l'apprenant glisse vers plus / moins d'autonomie globalement"). Si
des ajustements par champ deviennent necessaires en pratique, la
fonction `Decide` peut etre etendue sans casser sa signature.

### OQ-6.4 — Frontieres `0.3` et `0.7` ou `0.4` et `0.75` ?

**A.** `0.3 / 0.7` (choix actuel) — symetrique autour de 0.5, aligne
sur la decomposition 4 composantes a 25%.

**B.** `0.4 / 0.75` — biaise vers un fading plus tardif (l'apprenant
doit prouver davantage avant que le systeme se retire).

**Defaut retenu** : **A**. Les 4 composantes a 25% donnent une
intuition naturelle des paliers (1, 3 composantes max). Si l'eval
harness montre une retraite trop precoce, c'est ce qu'on bumpera
en priorite (PR a 1 ligne).

---

## 10. Recapitulatif

| Aspect | Decision |
|--------|----------|
| **Position pipeline** | Post-`Orchestrate`, isole de la chaine de selection |
| **API** | `engine.Decide(score, trend) FadeParams` pure function |
| **Flag** | `REGULATION_FADE=on`, default OFF, strict equality |
| **Tiers de score** | 3 (Low <0.3, Mid 0.3-0.7, High >=0.7) |
| **Modulation trend** | +/-1 cran sur le tier (cf table 9 cellules §4.3) |
| **Outputs** | 4 champs : HintLevel, WebhookFrequency, ZPDAggressiveness, ProactiveReviewEnabled |
| **Wiring initial** | HintLevel -> motivation_brief, fade_params attache au resultat JSON |
| **Wiring follow-up** | scheduler (WebhookFrequency, ProactiveReviewEnabled), action selector (ZPDAggressiveness) |

---

**STOP.** Design [6] FadeController complet. Implementation
`engine/fade_controller.go` + tests + wiring `tools/activity.go` dans
la meme PR. Composants suivants (post-merge) : (a) integration
scheduler de `WebhookFrequency` (migration schema +
filtre `dispatchQueued`), (b) integration `ZPDAggressiveness` dans
`engine/action_selector.go`, (c) integration
`ProactiveReviewEnabled` dans le job FSRS du scheduler.
