# Runtime pédagogique : corrections et trajectoire

État des lots de correction, séparation BKT, réconciliation du curriculum et
validation partagée de la notation,
septembre 2026. Ce document distingue les corrections
implémentées d'une validation pédagogique, qui nécessite encore des données
d'apprentissage. Il prévaut sur les anciennes descriptions PFA/IRT/fade des
notes de conception historiques.

## Principe conservé

Le runtime fixe les transitions, les règles de preuve et les droits de mutation.
L'IA génère curriculum, tâches, références de correction, explications et
dialogues. Une tâche générée devient un artefact figé avant la réponse : conserver
cet artefact pour l'audit ne constitue pas une banque de contenu pédagogique statique.

Le déterminisme porte sur les règles et leurs entrées, pas sur la véracité d'une
note produite par l'IA. Un contrat respecté structurellement ne prouve ni la
qualité de la tâche ni la fidélité de sa présentation à l'apprenant.

## Implémenté

| Problème | Changement |
|---|---|
| Équations FSRS mélangées, difficulté inversée, mauvais bonus Easy | Version FSRS-5 explicite, poids et équations cohérents, branche avant 24 h, tests numériques de référence. Historique conservé, sans recalibration rétroactive. |
| Faux rappel différé après réenseignement ou indices | Délai depuis la dernière exposition enregistrée, réponse sans indice, timestamp de soumission et non de notation. Égalités temporelles et observations futures traitées prudemment. |
| Maîtrise décrite comme une échelle obligatoire | Estimation, rétention et démonstration sont des axes distincts. Le transfert exige toujours des preuves de démonstration fiables. |
| Anti-répétition bloquant la seule compétence accessible | Préférence relâchable, désactivation effective avec fenêtre 0, seconde sélection sans diversité si nécessaire. |
| Oubli entraînant une oscillation de phase | L'entretien traite le rappel sans retourner automatiquement à l'acquisition. |
| Faux plateau PFA | Suppression du modèle inutilisé et du producteur d'alerte. Lecture des anciennes alertes préservée. |
| Difficulté conceptuelle FSRS assimilée à celle d'un item IRT | Retrait du couplage dans l'actualisation, la sélection et les alertes prédictives. Theta historique conservé, non actualisé ; cible de génération explicitement heuristique. |
| Erreurs de calibration qui se compensent | Moyenne des erreurs absolues, effectifs et couverture visibles ; absence de données distincte d'une bonne calibration. |
| Score composite assimilé à l'autonomie | Sortie descriptive seulement : pas de diagnostic de dépendance ni de retrait automatique d'aide sur cette base. |
| Miroirs pédagogiques préécrits | Observations structurées, fenêtre, statut descriptif et intention de dialogue. L'IA formule le message ; le runtime n'envoie pas un miroir sans texte généré. |
| Décision et évaluation sans liaison durable | Journal de décisions, contrat mécanique, version de politique, compétence et version de curriculum figées avec la tentative. |
| Rubriques liées appauvries silencieusement | Schéma strict, critères décrits, answer_key et anchors conservés ; champs non supportés rejetés ; observation obligatoire par critère lors de la notation. |
| Une réponse de diagnostic déclenchant une transition d'apprentissage | Inférence bayésienne séparée de la transition ; politique explicite selon le type d'activité ; posterior et contribution de la transition audités. |
| Une définition de compétence modifiée conservant des estimations et preuves périmées | Réconciliation atomique, remise à l'état initial des estimations concernées, invalidation des observations et tentatives antérieures ; historique conservé. |
| Prérequis impossibles à réparer explicitement | Opération `repair_prerequisites` par IDs stables, listes de remplacement explicites, validation du graphe complet. |
| Notation liée passant par un normaliseur permissif ; résultat non recalculé au stockage | Contrat partagé entre MCP et stockage, rejet des JSON ambigus et agrégats contradictoires, calcul déterministe sans epsilon fixe. |

## BKT : observation et opportunité d'apprentissage

La séparation introduite par `2026-09-observation-v2`, conservée dans la politique
courante `2026-09-scoring-v4`, distingue deux opérations :

- `BKTObserve` : calcul de la probabilité de maîtrise conditionnée par la réponse,
  avec les probabilités de slip et guess ; aucun terme d'apprentissage ou d'oubli.
- `BKTTransition` : passage au prochain état, avec
  `posterior × (1 − PForget) + (1 − posterior) × PLearn`.
  `PForget` désigne ici une probabilité par transition, pas l'oubli selon le temps écoulé.

Cette séparation correspond à la distinction entre observation et transition du
[modèle KT décrit par CMU](https://www.cs.cmu.edu/~listen/BNT-SM/kt.html).
Omettre la transition sur une mesure est toutefois **notre politique de mesure**,
pas une preuve qu'un test ne provoque jamais d'apprentissage.

| Type d'activité | `bkt_update_mode` | Actualisation |
|---|---|---|
| `DIAGNOSTIC_ASSESSMENT`, `MASTERY_CHALLENGE`, `TRANSFER_PROBE` | `observation_only` | Posterior uniquement ; paramètres effectifs de transition à zéro. |
| `NEW_CONCEPT`, `PRACTICE`, `RECALL_EXERCISE`, `DEBUGGING_CASE`, `DEBUG_MISCONCEPTION`, `FEYNMAN_PROMPT` | `learning_opportunity` | Observation puis une transition modélisée. |

Le mode est choisi par le runtime à partir du type d'activité, jamais par une
déclaration de maîtrise de l'hôte, le succès de la réponse ou la seule présence
d'une rubrique. Une pratique avec rubrique reste une pratique ; un diagnostic
correct reste une mesure. Les actions non cognitives sont toujours refusées
par le chemin d'actualisation du modèle.

Exemple avec les paramètres initiaux : à partir de 10 % de maîtrise estimée,
une réponse incorrecte en diagnostic produit un posterior de 1,37 %, au lieu
des 16,10 % obtenus auparavant après ajout automatique de la transition.
Ce sont des probabilités internes au modèle, pas un pourcentage de savoir mesuré.

Le contrat émis et enregistré contient `bkt_update_mode`. Le journal de chaque
réponse conserve `bkt_posterior`, `bkt_transition_applied`,
`bkt_transition_delta` et les paramètres effectivement appliqués. Les paramètres
de base conservés dans l'état ne sont pas effacés par une mesure. Les nouvelles
observations utilisent la politique courante et en tracent la version, même
si la tentative a été préparée sous une ancienne version. Aucun historique n'est
rejoué, aucune estimation ancienne réinitialisée et aucune migration SQL ajoutée
pour ce lot BKT.

L'hôte doit enregistrer la mesure avant la correction pédagogique. Une activité
d'enseignement ultérieure nécessite sa propre observation de l'apprenant :
rejouer la même réponse pour déclencher une transition n'est pas un enseignement.
Cette règle d'ordre est une consigne à l'hôte, pas une vérification de ce qui a
réellement été montré dans le dialogue. FSRS continue à traiter les réponses
comme des expositions ; distinguer finement réponse, feedback et enseignement
reste un chantier séparé. Les coefficients individualisés restent heuristiques.

## Réconciliation lors d'une révision du curriculum

La politique de réconciliation `2026-09-curriculum-v1` compare le contrat déclaré,
pas le sens réel d'un texte généré. Elle est calculée dans la couche de stockage
à partir du parent effectif : le client ne peut pas affirmer une équivalence pour
conserver une maîtrise lors d'un changement détecté.

| Changement | Estimations et preuves existantes |
|---|---|
| Renommage de présentation ; métadonnées identiques ; simple réordonnancement des outcomes/critères | Conservées. Le renommage ne doit pas servir à changer la compétence. |
| Description, niveau, contenu ou identité d'un outcome/critère modifié | BKT, FSRS et theta de cette compétence reviennent aux valeurs initiales ; ses anciennes observations, tentatives et mesures de transfert sont invalidées pour le pilotage. |
| Retrait, scission, fusion | Sources invalidées ; les cibles nouvelles commencent sans maîtrise transférée. Identités retirées et versions antérieures restent dans l'historique. |
| Réactivation par une mise à jour legacy du graphe | Nouveau départ, sans résurrection d'anciennes preuves. |
| Réparation des arêtes de prérequis uniquement | Compétences inchangées : leurs preuves restent applicables ; le routage utilise le nouveau graphe. |

Un changement de description purement rédactionnel peut donc provoquer une remise
à l'état initial. C'est un compromis conservateur explicite, pas une détection
sémantique « intelligente ». Les compétences dépendantes ne sont pas invalidées
en cascade si leur propre définition n'a pas changé. Les prérequis modifiés
restent des contraintes de routage, pas une preuve sur leur maîtrise.

La version du graphe, l'invalidation et la remise à l'état initial sont publiées
dans une seule transaction. Les lectures de mutation verrouillent le domaine
avant la tentative et l'état de compétence. Une observation concurrente est soit
commise avant la révision et invalidée, soit traitée sous le nouveau contrat.
Une erreur ultérieure de publication annule tous ces effets.

`curriculum_invalidated_version` conserve la première version d'invalidation sur
les interactions, tentatives et transferts. Les réponses, résultats, rubriques,
versions d'origine et décisions de confiance ne sont pas réécrits. Les anciennes
tentatives ne peuvent plus être soumises ou évaluées ; leur annulation reste
possible. Les lectures servant BKT, maîtrise, rétention, transfert, diagnostic,
erreurs récurrentes et routage filtrent ces lignes **avant** les limites et
agrégations. Les compteurs d'activité journalière et séries de jours restent
historiques : une révision ne signifie pas que le travail passé n'a pas eu lieu.

Le journal du curriculum contient uniquement la règle et les IDs concernés, sans
copie supplémentaire des valeurs de l'apprenant. Les snapshots pédagogiques
gardent leur historique selon leur politique de rétention ; leur lecture ajoute
`curriculum_applicability: superseded` et la version d'invalidation. Un marqueur
`not_invalidated` ne signifie pas « évaluation fiable ». Export, effacement DSAR
et rétention continuent à utiliser les tables historiques non filtrées.

Réparation explicite d'un prérequis devenu inutile :

```json
{
  "domain_id": "domaine",
  "expected_version": 4,
  "operation": "repair_prerequisites",
  "source_concept_ids": ["competence_dependante"],
  "prerequisites": {"competence_dependante": []},
  "provenance": {
    "source_type": "learner",
    "rationale": "Justification générée de la correction du graphe"
  }
}
```

Chaque source doit avoir une liste explicite. Les IDs inconnus, retirés,
dupliqués, les auto-dépendances et cycles sont refusés. Une compétence avec des
dépendants ne peut pas être retirée avant réparation. L'héritage des arêtes lors
d'une scission/fusion reste mécanique et peut être corrigé ensuite : le moteur
ne certifie pas la pertinence pédagogique des arêtes générées.

Portée temporelle : les changements antérieurs à cette politique ne sont pas
rejoués rétroactivement à la migration. Les observations legacy/non liées restent
non vérifiées ; elles ne prouvent pas quelle définition a réellement été
présentée. Les estimations déjà contaminées par d'anciennes révisions nécessitent
un examen et une réévaluation explicites, sans inventer une attribution fiable.

## Contrat d'exécution

1. `get_next_activity` renvoie `decision_id`, `policy_version`,
   `curriculum_version` et la définition complète de `competency` dans le contrat.
2. L'hôte génère la tâche et sa rubrique, puis appelle
   `prepare_assessment_attempt` avec ce `decision_id`. Si la compétence définit
   des outcomes, il sélectionne leurs IDs dans `outcome_ids`.
3. Le serveur vérifie propriétaire/tenant, domaine, session, compétence, type
   d'activité et version courante. Une décision ne peut produire qu'une tentative,
   même après annulation ; une nouvelle intention nécessite une nouvelle décision.
4. La réponse est soumise puis évaluée contre la rubrique figée. La trace
   d'actualisation du modèle contient le lien décision/tentative/curriculum.

Compatibilité : une tentative sans `decision_id` reste possible et renvoie
`binding_status: standalone_unbound`. Elle n'atteste pas le suivi d'une décision
du moteur. Elle conserve néanmoins la définition courante de la compétence.
Une observation sans tentative reste une observation de pilotage non vérifiée.
Les anciennes tentatives conservent `curriculum_version = 0`, provenance inconnue.

Une rubrique liée accepte exactement :

```json
{
  "criteria": [{
    "id": "reasoning",
    "description": "Critère généré définissant la réussite observable",
    "max_score": 2,
    "anchors": [{"score": 2, "description": "Ancrage de notation généré"}]
  }],
  "passing_score": 2,
  "answer_key": "Référence générée avant la réponse"
}
```

`anchors` et `answer_key` sont facultatifs. `levels`, `required` et les autres
extensions non implémentées échouent explicitement dans le chemin lié. Le chemin
legacy conserve sa normalisation permissive pour compatibilité ; il n'offre pas
le contrat strict du chemin lié.

### Notation partagée : préparation de la revue indépendante

Depuis `2026-09-scoring-v4`, le package `assessment` porte le contrat des rubriques
liées et de leurs scores. Il est utilisé par les outils MCP et par le stockage,
pas seulement par une instruction donnée au tuteur.

- Les JSON ont une seule valeur, des clés uniques à chaque niveau, des IDs exacts
  et des nombres JSON finis. Les alias historiques, nombres sous forme de texte
  et règles inconnues sont refusés, sans correction silencieuse.
- Chaque critère figé reçoit exactement un score dans ses bornes et une
  observation `evidence` non vide. Les champs facultatifs `summary`, `confidence`
  et `error_type` restent descriptifs ; ils ne donnent pas de confiance au réviseur.
- `total`, `max_total` et le `max_score` de chaque critère, s'ils sont transmis,
  doivent correspondre aux valeurs dérivées. Aucun agrégat ne remplace la rubrique.
- Les représentations décimales canoniques des valeurs float64 stockées sont
  additionnées exactement. Le seuil est comparé sans epsilon fixe : un zéro ne
  passe plus un seuil positif minuscule, et `0.1 + 0.7` atteint `0.8`.
  Cela ne promet pas une précision arbitraire des nombres JSON d'entrée.
- Les documents JSON et leur forme canonique sont limités à 16 384 octets ;
  la profondeur JSON est bornée. La canonicalisation conserve les textes générés.
- Le stockage vérifie la rubrique à la préparation puis recalcule le résultat
  sous les verrous curriculum/tentative. Une contradiction est rejetée et les
  écritures pédagogiques composées sont annulées avec la transaction.

Les tentatives standalone/legacy gardent leur normalisation de compatibilité.
Aucune évaluation passée n'est rejouée et aucune migration n'est nécessaire.
Une ancienne tentative liée dont la rubrique serait invalide doit être annulée
si elle est encore ouverte, puis remplacée par une préparation valide ; le
contrat figé n'est pas réparé après la réponse.

Ce sous-lot ne fournit **pas encore** de canal de revue indépendante. Il ne choisit
ni fournisseur, ni identité de réviseur, ni mécanisme d'adjudication. Le canal
public reste `host_llm` non fiable ; il ne peut pas se promouvoir en revue humaine
ou externe en ajoutant un champ au score. Une observation textuelle non vide ne
prouve pas davantage sa fidélité à la réponse de l'apprenant.

## Données et migrations

- Migrations additives : SQLite `0064_pedagogical_contracts`, PostgreSQL
  `postgres_0055_pedagogical_contracts`. Aucune ancienne migration réécrite,
  aucune migration appliquée à une base applicative par ce chantier.
- Lot curriculum : SQLite `0065_curriculum_reconciliation`, PostgreSQL
  `postgres_0056_curriculum_reconciliation`. Colonnes d'applicabilité additives ;
  extension du type d'opération autorisé. SQLite reconstruit la table des versions
  sans réécrire ses lignes, selon la
  [procédure de changement de schéma SQLite](https://www.sqlite.org/lang_altertable.html#otheralter).
  Sur une base existante avec clés étrangères activées, leur désactivation est
  limitée à la connexion réservée du migrateur, avant sa transaction exclusive ;
  vérification avant commit et restauration après succès ou échec. Prévoir une
  sauvegarde et une fenêtre de migration ; aucun déploiement effectué ici.
- Après migration, réappliquer `deploy/postgres-roles.sql` avec le rôle propriétaire
  afin d'accorder au worker la lecture/purge des décisions et l'ajout des
  checkpoints d'effacement manquants. Ne pas déployer le nouveau worker sans ces droits.
- Décisions immuables et cloisonnées par tenant. Le journal ne duplique pas le
  prompt de présentation, les souvenirs, les notes libres ni la prose explicative.
  Il conserve le contrat mécanique, la définition du curriculum et les signaux
  de décision ; ce n'est pas un enregistrement intégral du dialogue.
- Export et effacement DSAR incluent les décisions, dans l'ordre des dépendances.
  Les demandes antérieures à cette migration reçoivent le checkpoint manquant
  lors de leur reprise, sans perdre leur progression.
- Le TTL `pedagogical_snapshot_days` purge aussi les anciennes décisions non
  référencées. Les décisions référencées restent avec les preuves ; l'effacement
  DSAR les supprime. Les gels de conservation sont respectés.

## Vérifications effectuées

- Suite complète après le sous-lot notation partagée : `go test -p 1 ./... -count=1` réussie.
- Analyse statique : `go vet ./...` réussie ; `git diff --check` sans erreur.
- Tests ciblés avec `-race` : FSRS, axes de maîtrise, calibration, contrats de
  décision et rubriques liées réussis dans `algorithms`, `engine`, `db`, `tools`.
- Lot BKT : posteriors numériques, paramètres de transition neutralisés sur les
  mesures, modes par activité, audit reconstituable, contrat de diagnostic lié,
  usage unique et pratique avec rubrique vérifiés ; tests ciblés `-race` réussis
  dans `algorithms`, `engine` et `tools`.
- Lot curriculum : comparaison des définitions, invalidation ciblée, conservation
  de l'audit, annulation transactionnelle, concurrence observation/révision et
  réparation des prérequis vérifiées ; tests ciblés `-race` réussis dans `models`,
  `db` et `tools`.
- Migration SQLite d'une base antérieure : références et JSON conservés,
  immutabilité maintenue, restauration des clés étrangères après succès et échec,
  refus atomique d'une base contenant une référence orpheline vérifiés.
- PostgreSQL, lot curriculum : migrations, réconciliation sous un rôle non
  privilégié avec RLS, concurrence, lectures de preuves, index et export/effacement
  des preuves invalidées vérifiés dans des schémas isolés.
- PostgreSQL (vérifié au premier lot), dans des schémas isolés : migrations et contrats, usage unique
  concurrent, immutabilité, rétention, export/effacement DSAR (dont demandes
  antérieures), et isolation RLS avec un rôle non privilégié réussis.
- Notation partagée : cas ambigus/contradictoires, seuils décimaux, non-consommation
  des tentatives rejetées et absence d'actualisation du modèle vérifiés. Tests
  ciblés `-race` réussis dans `assessment`, `db` et `tools` ; tests PostgreSQL
  de notation, usage unique concurrent, rollback, curriculum et RLS réussis.
- Parseur/notation : essai de fuzzing de dix secondes, environ 17 000 exécutions,
  sans échec détecté. Ce résultat n'est pas une preuve exhaustive de sûreté.

La dernière exécution complète utilise un répertoire temporaire sur disque et
le binaire Go direct : le `/tmp` en mémoire avait saturé lors de relances
parallèles, et le lanceur Snap avait rencontré un blocage de profils. Les tests
HTTP utilisent l'autorisation d'ouvrir des ports locaux temporaires. Ces
incidents d'environnement ont été suivis de relances réussies, sans modifier
les attentes fonctionnelles pour les contourner.

## Limites et prochain lot

Ce lot ne rend pas le système « optimal » ni validé expérimentalement.

1. **Évaluation fiable opérationnelle.** Le canal public reste `host_llm`, non
   fiable pour une revendication de démonstration. Le contrat commun de notation
   est implémenté ; il faut encore une vraie frontière de
   revue indépendante, avec identité, audit et adjudication. Une seconde requête
   au même modèle n'est pas automatiquement une évaluation indépendante. En
   contexte à enjeux élevés, la règle de revue humaine reste inchangée.
2. **Qualité et historique du curriculum.** La réconciliation prospective et la
   réparation explicite des arêtes sont implémentées. Restent la revue de leur
   pertinence sémantique, le traitement des changements antérieurs à la politique
   et la mesure du coût des invalidations conservatrices. Une formulation changée
   n'est pas nécessairement une compétence différente, et l'inverse reste possible.
3. **Affiner les événements d'apprentissage.** La séparation BKT est implémentée
   au niveau des types d'activité. Il reste à distinguer les événements de réponse,
   feedback et enseignement réellement présentés, à revoir leur effet sur FSRS et
   à calibrer les paramètres individualisés sur des données.
4. **Évaluer les politiques.** Construire des familles de tâches générées et des
   évaluations indépendantes de rappel/transfert. Mesurer les résultats sans aide,
   à délai fixé, et comparer à une politique simple. Les tests logiciels ne
   mesurent ni l'apprentissage ni la validité psychométrique.
5. **Configuration et calibration.** Revoir seuils de délai, budget de session,
   cibles de difficulté et dimensions de transfert à partir d'observations ; ne
   pas remplacer des constantes arbitraires par un modèle plus opaque. Le
   calendrier reste en jours malgré les équations FSRS court terme.

Référence des équations implémentées :
[FSRS-5, spécification officielle](https://github.com/open-spaced-repetition/awesome-fsrs/wiki/The-Algorithm#fsrs-5),
[go-fsrs v3.3.1](https://github.com/open-spaced-repetition/go-fsrs/tree/v3.3.1).
