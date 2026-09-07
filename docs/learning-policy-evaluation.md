# Mesurer les politiques pédagogiques

Le script `scripts/evaluate_learning_policies.py` prépare une attribution stable
des participants à deux politiques et analyse des résultats de rappel/transfert
à délai fixé. Il ne sollicite pas d'IA, ne fournit pas une banque de questions,
ne change aucun paramètre du runtime et ne conclut pas à son efficacité.

## Avant de recueillir les résultats

1. Définir la population, le consentement, la durée, les critères d'arrêt et la
   politique de comparaison simple. Conserver les versions exactes des deux
   politiques. L'outil n'implémente pas la politique de comparaison à votre place.
2. Définir des familles de tâches **générées**, leur couverture et leurs rubriques.
   Les tâches de mesure sont réservées à la mesure, avec formes alternatives
   pour limiter la mémorisation de l'item. Leur pertinence exige une revue externe.
3. Fixer avant attribution une fenêtre de délai depuis la dernière exposition
   effectivement enregistrée, les critères d'exclusion et les endpoints.
4. Choisir un seul résultat principal par participant et endpoint. La règle de
   sélection doit précéder la collecte ; envoyer plusieurs résultats puis retenir
   le meilleur est refusé par l'analyseur. Pour une randomisation par cohorte ou
   des mesures répétées, prévoir une analyse statistique adaptée : ce script ne
   les traite pas comme des observations indépendantes supplémentaires.
5. Faire noter les réponses sans aide par une autorité distincte du tuteur.
   Conserver tâche, rubrique, réponse, version du curriculum et adjudication.
   Documenter les changements de curriculum et l'adhésion à la politique affectée.

Exemple de protocole à adapter **avant** attribution :

```json
{
  "version": 1,
  "experiment_id": "retention-pilot-v1",
  "assignment_salt": "REMPLACER_PAR_UN_ALEA_DE_32_CARACTERES_MINIMUM",
  "registered_at": "2026-09-07T12:00:00Z",
  "arms": {"baseline": "simple-fixed-review-v1", "candidate": "2026-09-events-v5"},
  "followup_min_hours": 168,
  "followup_max_hours": 192,
  "task_families": ["generated-recall-heldout", "generated-transfer-heldout"],
  "endpoints": ["retention", "transfer"]
}
```

Les identifiants de participants sont des pseudonymes stables, sans email ni nom.
L'attribution est reproductible à partir du protocole et du pseudonyme. Choisir
un sel aléatoire, figer le registre avant collecte et conserver les refus et
abandons. Changer le protocole invalide les empreintes du registre. Le script
n'est pas un registre inviolable : l'opérateur doit conserver ses versions et
dates dans un système d'audit indépendant.

```bash
python3 scripts/evaluate_learning_policies.py assign \
  --protocol protocol.json --participants participants.txt \
  --assigned-at 2026-09-07T13:00:00Z > assignments.jsonl
```

## Contrat des résultats

Chaque ligne JSON de `outcomes.jsonl` contient :

```json
{
  "participant_id": "participant-opaque",
  "endpoint": "retention",
  "attempt_id": "attempt-opaque",
  "task_family": "generated-recall-heldout",
  "last_exposure_at": "2026-09-08T10:00:00Z",
  "submitted_at": "2026-09-15T10:00:00Z",
  "adjudicated_at": "2026-09-15T12:00:00Z",
  "adjudication_id": "adjudication-opaque",
  "evaluation_method": "external_service",
  "trusted_evaluation": true,
  "hints_requested": 0,
  "passed": true,
  "predicted_probability": 0.7,
  "predicted_at": "2026-09-15T09:59:00Z"
}
```

La prédiction et sa date sont facultatives ensemble. Elles doivent provenir
d'une estimation conservée avant la réponse, jamais du posterior obtenu après.
La provenance doit être contrôlée contre les journaux du service : les champs
de ce fichier sont des déclarations de l'opérateur. Le script ne vérifie pas les
signatures des adjudications, la réalité du délai ou la présentation du dialogue.
Un export marqué `trusted_evaluation: true` ne crée aucune confiance dans le serveur.

```bash
python3 scripts/evaluate_learning_policies.py analyze \
  --protocol protocol.json --assignments assignments.jsonl \
  --outcomes outcomes.jsonl > result.json
```

## Lire le rapport

- Le dénominateur conserve **tous les participants attribués**, y compris ceux
  dont le résultat manque ou est inéligible. Les exclusions sont comptées par motif.
- Le taux observé et sa différence portent sur les seules observations éligibles.
  Des bornes pour tous les participants rendent visible l'incertitude due aux
  données manquantes ; elles ne les assimilent pas silencieusement à des échecs.
- L'intervalle de Wilson à 95 % décrit un taux binomial sous une hypothèse
  d'indépendance des participants. Ce n'est pas un intervalle de l'effet causal,
  ni une correction des effets de cohorte, de sélection ou de multiplicité.
- Le score de Brier mesure les prédictions disponibles, avec leur effectif.
  Il ne suffit pas à choisir des seuils, estimer des paramètres BKT/FSRS ou
  confirmer la validité du curriculum. Conserver un jeu de validation distinct
  avant toute proposition de calibration.

Sans données réelles et protocole effectivement suivi, aucun résultat empirique
n'est fourni. Les données synthétiques des tests vérifient uniquement les calculs
et les rejets. Le protocole scientifique, la collecte et l'interprétation restent
à réaliser pour chaque population et domaine.
