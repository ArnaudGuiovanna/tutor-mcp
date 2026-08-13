# Exploitation des livraisons webhook durables

Ce runbook couvre la machine d'état de `webhook_message_queue` et la commande
`tutor-webhook-admin`. Il ne transforme pas Discord en destinataire exactement
une fois : l'identifiant `event_id` est une preuve interne stable, pas une clé
de déduplication comprise par un webhook Discord direct.

Les intégrations SaaS de `tenant_integrations` suivent un contrat distinct :
elles sont livrées par outbox/job, à une origine HTTPS allowlistée, avec
signature et clé de déduplication comprises par le destinataire. Elles sont
décrites plus bas et dans le
[`runbook SaaS`](./saas-runtime-operations.md#workers-webhooks-et-rgpd).

## Contrat de livraison

Le runtime suit ces transitions :

```text
pending -> processing -> dispatching -> sent
                         |             |
                         |             +-- 2xx reçu et transaction DB validée
                         +-- non-2xx connu -> pending/failed avec backoff/DLQ
                         +-- transport ambigu -> delivery_unknown
```

- `processing` est strictement avant la frontière HTTP. Un worker mort à ce
  stade peut être rejoué avec le budget normal.
- `dispatching` est persisté avant le premier octet de la tentative HTTP. Une
  ligne stale dans cet état devient `delivery_unknown`, jamais `pending`.
- seul un statut HTTP final `200..299` est un succès connu ; tout autre statut
  final est un échec connu et borné par le backoff/la DLQ ;
- timeout, erreur de transport ou erreur de redirection après `dispatching`
  sont ambigus, même si la bibliothèque cliente rapporte aussi une réponse ;
- si Discord répond 2xx mais que la transaction de complétion échoue, la ligne
  reste `dispatching`, puis la réconciliation la met en quarantaine ;
- la réservation `scheduled_alerts` et la ligne de queue sont terminées dans
  la même transaction sur succès ou échec connu.

L'historique `webhook_delivery_transitions` ne contient ni payload, ni URL, ni
credential. Ne jamais copier ces valeurs dans un ticket, une alerte ou un log.

## Détection et inspection

La réconciliation horaire traite au plus 1 000 claims stale par passage. Une
alerte d'exploitation doit surveiller le nombre et l'âge maximal des lignes
`delivery_unknown`, ainsi que la DLQ `failed`. Une quarantaine non vide demande
une décision humaine ; elle ne doit pas déclencher un retry automatique.

Lister les quarantaines d'un apprenant, en lecture seule :

```bash
go run ./cmd/tutor-webhook-admin \
  --action=list \
  --learner='<learner-id>' \
  --limit=100
```

La commande reprend `DB_DRIVER`, `DB_PATH`, `DATABASE_URL` et `DB_MAX_CONNS` du
serveur. En SQLite, la cible doit déjà exister. La sortie est volontairement
expurgée du contenu et du secret webhook.

## Décision opérateur

1. Relever `id` et `event_id` dans la sortie expurgée.
2. Corréler l'heure de `dispatch_started_at` avec les journaux d'un relais
   contrôlé ou une preuve destinataire indépendante.
3. Si la livraison est prouvée, choisir `delivered`.
4. Si l'absence de livraison est prouvée, choisir `not-delivered` ; le même
   `event_id` sera réutilisé après le backoff.
5. Si aucun résultat n'est prouvable, conserver `delivery_unknown`. Avec un
   webhook Discord direct, l'identifiant interne ne permet pas d'interroger
   Discord après coup et l'absence de message visible n'est pas une preuve.

Toujours prévisualiser la résolution :

```bash
go run ./cmd/tutor-webhook-admin \
  --action=resolve \
  --learner='<learner-id>' \
  --id='<queue-id>' \
  --event-id='<event-id>' \
  --outcome=delivered
```

Appliquer seulement après revue de la preuve :

```bash
go run ./cmd/tutor-webhook-admin \
  --action=resolve \
  --learner='<learner-id>' \
  --id='<queue-id>' \
  --event-id='<event-id>' \
  --outcome=delivered \
  --apply
```

Pour autoriser un retry prouvé non livré, remplacer l'outcome par
`not-delivered`. La commande exige à la fois l'ID numérique, le propriétaire et
l'`event_id` stable afin d'empêcher la résolution accidentelle d'une autre
ligne. Elle refuse aussi de créer une base SQLite manquante.

## Discord direct et relais contrôlé

Discord direct ne garantit pas la déduplication d'un `event_id` fourni par le
client et ne permet pas de vérifier une signature applicative ajoutée au
payload. Ajouter un header maison ne change donc pas le contrat. Deux choix
seulement sont honnêtes :

- conserver la quarantaine et accepter une intervention humaine rare ;
- interposer un relais contrôlé qui persiste `event_id`, déduplique les appels
  et expose un journal de résultat interrogeable.

Le passage à un relais doit garder l'identité d'événement actuelle et ajouter
des tests de replay avant d'autoriser la résolution automatique.

## Déploiement et rollback

La migration ajoute des colonnes et une table sans supprimer l'ancien schéma.
Pour un rollout, migrer d'abord, déployer le runtime raccordé, puis activer les
alertes de quarantaine. Avant un rollback applicatif, arrêter les workers
récents et traiter toutes les lignes `dispatching`/`delivery_unknown` : un
ancien runtime ne connaît pas cette frontière et ne doit pas les reclasser.

Après toute intervention, vérifier que :

- la ligne ciblée est `sent` ou `pending/failed` selon la décision ;
- sa réservation est `delivered` ou libérée ;
- une transition `operator_confirmed_delivered` ou
  `operator_retry_authorized` existe ;
- aucune autre ligne du même apprenant n'a changé.

## Webhooks SaaS signés

Le worker envoie `Content-Type: application/json` et les headers :

- `X-Tutor-Event-ID` : identifiant stable à dédupliquer ;
- `X-Tutor-Event-Type` : type souscrit par l'intégration ;
- `X-Tutor-Timestamp` : secondes Unix UTC ;
- `X-Tutor-Secret-Version` : version de clé attendue ;
- `X-Tutor-Signature` : `v1=<hex HMAC-SHA256>`.

Le message signé est la concaténation exacte
`timestamp + "." + event_id + "." + corps brut`. Le receveur compare le HMAC
en temps constant, refuse un timestamp hors de sa fenêtre anti-replay, puis
insère `event_id` sous contrainte unique avant d'appliquer l'effet. Il doit
retourner 2xx pour un replay déjà appliqué. Le worker borne la réponse à 64 KiB,
l'appel à 12 secondes et les retries à huit tentatives exponentielles ; une
livraison acceptée est retrouvée après crash sans nouvel envoi.

La rotation produit un secret aléatoire affiché une fois. Le receveur accepte
la nouvelle version et la précédente pendant une fenêtre choisie entre cinq
minutes et sept jours, puis retire l'ancienne à `valid_until`. La base conserve
uniquement des enveloppes AES-256-GCM liées au tenant/intégration/version.
