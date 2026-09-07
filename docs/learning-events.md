# Réponse, feedback et enseignement

La politique `2026-09-events-v5` émet `learning_event_protocol: response-feedback-v1`
dans les nouveaux contrats de décision. Une tentative liée dérive ce protocole
de la décision figée au stockage ; le tuteur ne peut pas le choisir ou le modifier.
Les décisions anciennes et les tentatives standalone gardent le chemin legacy.

## Événements conservés

- `submit_assessment_attempt` persiste atomiquement la réponse et un événement
  `response` de source `committed_response` pour ce protocole. La dernière
  exposition enregistrée sur la compétence est figée dans `prior_exposure_at`.
- Après avoir livré une correction ou un enseignement, l'hôte appelle
  `record_learning_event` avec `kind: feedback` ou `kind: instruction`, le domaine,
  la compétence, un `event_key` stable et, si pertinent, l'`attempt_id` lié.
  `feedback` lié à une tentative exige une réponse déjà soumise.
- Les événements déclarés ont `source: host_reported`. Le serveur fixe l'heure
  de réception ; aucun timestamp client, score ou texte de leçon n'est accepté.
  La réception d'une déclaration n'est pas une preuve de présentation réelle.

```json
{
  "event_key": "correction-session-opaque-1",
  "domain_id": "domain-opaque",
  "concept": "competence",
  "attempt_id": "attempt-opaque",
  "kind": "feedback"
}
```

Une relance avec la même clé et les mêmes paramètres restitue le même événement.
Un autre contenu est refusé. Les requêtes étrangères, domaines archivés,
compétences retirées et tentatives invalidées sont refusés. La réponse conserve
`presentation_verified: false` et `model_updated: false`.

## Effets sur les modèles

Pour les nouvelles tentatives liées, FSRS traite la **date de soumission** de la
réponse, même si sa notation survient plus tard. Une réponse plus ancienne que
la dernière réponse déjà traitée ne réécrit pas la carte FSRS : le snapshot
indique `fsrs_update_applied: false`. BKT garde l'ordre de traitement des
observations ; ce lot n'introduit pas de replay rétroactif du modèle.

Feedback et enseignement ne fournissent pas de note FSRS : leur enregistrement
n'ajoute ni répétition réussie, ni transition BKT, ni stabilité inventée. Ils
réinitialisent la fenêtre minimale utilisée pour reconnaître un rappel différé,
sans repousser automatiquement une révision prévue. La simple notation ne
constitue plus une exposition implicite dans le nouveau protocole.

Les anciennes tentatives conservent l'hypothèse conservatrice associant notation
et exposition, sans reconstruction de dialogues passés. Pour chaque nouvelle
tentative, les interactions historiques disponibles restent aussi des expositions
à considérer. Une absence de déclaration de feedback reste une limite du client,
pas une preuve d'absence de feedback.

## Stockage

SQLite `0068_learning_events` et PostgreSQL `postgres_0059_learning_events`
ajoutent le journal, les contraintes de portée, les colonnes de protocole et
les protections d'immutabilité. La RLS est forcée sur PostgreSQL. Une révision
sémantique invalide les événements concernés avec les autres preuves. Les
événements n'ont pas de prose à purger ; ils restent avec l'historique jusqu'à
l'effacement DSAR de l'apprenant. Le manifeste d’export DSAR les dénombre ; les anciens checkpoints d’effacement les incluent.

Ces événements permettent une mesure plus explicite. Le calibrage des effets
de réponse, feedback et enseignement sur BKT/FSRS requiert encore des données,
selon le [protocole d'évaluation](learning-policy-evaluation.md).
