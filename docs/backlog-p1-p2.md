# Backlog P1/P2 — sécurité, fiabilité et exploitation

> **Orchestration.** Ce backlog est exécuté dans le cadre des quatre Goals
> décrits dans [`saas-goals.md`](./saas-goals.md). Le Goal 1 est le gate de
> sécurité et conformité MCP/OAuth avant le démarrage de l'isolation tenant.
>
> **Périmètre.** Ce document suit les corrections restant après le lot P0 de
> sécurisation MCP et de transactions. La refonte d'isolation multi-tenant est
> volontairement exclue et suivie dans
> [`backlog-multitenant-saas.md`](./backlog-multitenant-saas.md).
>
> **Référence.** Les numéros de ligne sont des points de départ issus de
> l'audit du commit `d6d62da248afcffde815124a9cecc812aa5edcd7`; ils peuvent
> dériver. Le nom du symbole et les tests constituent la référence durable.

## Convention de suivi

- Statuts : `TODO`, `READY`, `IN_PROGRESS`, `BLOCKED`, `DONE`.
- Effort relatif : `S` (petit), `M` (moyen), `L` (grand), `XL` (programme).
- Une tâche n'est `DONE` que si ses critères d'acceptation et ses tests sont
  intégrés à la CI.
- Aucun ticket ne doit affaiblir les garanties déjà acquises : liaison
  utilisateur/session MCP, limite des corps `/mcp`, expiration des sessions,
  scopes JWT et rollback après panic.

## Ordre de livraison

| Lot | Objectif | Tickets | Gate de sortie |
|---|---|---|---|
| 1 | Sécuriser le cycle de compte | P1-AUTH-01 à 03, P1-OAUTH-01 à 02 | Inscription vérifiée, réponses non énumérables, récupération testée |
| 2 | Rendre les erreurs visibles | P1-ERR-01 à 03, P1-MEM-01 | Une panne de dépendance ne peut plus être présentée comme un état métier normal |
| 3 | Fiabiliser le travail asynchrone | P1-JOB-01 à 02, P1-WEBHOOK-01 à 02, P1-TEST-01 | Crash/reprise et redelivery testés sur PostgreSQL |
| 4 | Préparer l'exploitation distribuée | P1-MEM-02, P1-QUERY-01, P1-MCP-01, P1-TEST-01 | Nœuds API remplaçables et requêtes bornées |
| 5 | Gate production | P1-SEC-01, P1-DEPLOY-01, P1-PRIV-01 | TLS, secrets et rétention vérifiés en staging |
| 6 | Dette P2 | Tous les P2 | Interfaces réduites, observabilité et matrice PostgreSQL en CI |

## P1 — à terminer avant une mise en production SaaS

### P0-PROTO-01 — Lier OAuth à la ressource MCP canonique

- **Statut / effort :** `DONE` / `M`
- **Problème :** la protected resource metadata annonce `${BASE_URL}/mcp`, mais
  les handlers ignorent le paramètre RFC 8707 `resource` et les JWT utilisent
  encore l'audience fixe `tutor-mcp/mcp`.
- **Actions :** valider l'URL canonique exacte dans `/authorize` et `/token`, la
  persister sur codes et refresh tokens, l'émettre dans `aud` et la vérifier à
  chaque requête MCP.
- **Critères d'acceptation :** mauvais/absent `resource`, code ou refresh token
  inter-resource et audience legacy sont refusés ; migration legacy bornée et
  documentée.

### P0-PROTO-02 — Passer à MCP 2026-07-28 stateless

- **Statut / effort :** `IN_PROGRESS` / `M` — rouvert le 2026-08-10 : le
  preflight CORS et le test round-robin ne prouvaient pas encore le gate.
- **Actions :** migrer le SDK Go v1.4.1 vers la version stable supportant
  `2026-07-28`, activer `Stateless`, propager l'annulation HTTP et mettre à jour
  les en-têtes CORS/protocole.
- **Critères d'acceptation :** round-robin sans sticky session, négociation
  `2026-07-28` et compatibilité legacy explicitement testée.

### P0-PROTO-03 — Publier l'autorisation et la sûreté par outil

- **Statut / effort :** `IN_PROGRESS` / `M` — rouvert le 2026-08-10 : deux
  outils annoncés read-only ont des effets d'écriture et le scope reste
  générique.
- **Actions :** définir des scopes cohérents, `securitySchemes`, challenges
  d'élévation et annotations read-only/destructive/idempotent/open-world pour
  les outils, puis faire appliquer l'autorisation dans chaque handler.
- **Critères d'acceptation :** aucun outil de mutation ne fonctionne avec un
  scope lecture ; les métadonnées publiées correspondent aux effets réels.

### P0-DEPLOY-01 — Verrouiller les permissions SQLite

- **Statut / effort :** `DONE` / `S`
- **Problème :** un répertoire existant n'est pas corrigé et les fichiers DB,
  WAL et SHM héritent de l'umask ; l'audit a observé `0775` et `0664`.
- **Actions :** imposer/vérifier ownership, `0700` sur le répertoire et `0600`
  sur DB/WAL/SHM ; refuser le démarrage si le durcissement est impossible.
- **Critères d'acceptation :** tests avec permissions trop larges, fichier
  existant, création neuve et erreur de chmod.

### P0-ABUSE-01 — Borner mémoire narrative et concurrence MCP

- **Statut / effort :** `DONE` / `M`
- **Actions :** quotas cumulés d'objets/octets/écritures, taille maximale par
  fichier, concurrence MCP globale et par principal, métriques et backpressure.
- **Critères d'acceptation :** disque, goroutines, sockets et descripteurs
  restent bornés sous comptes multiples et appels concurrents.

### P1-AUTH-01 — Vérifier l'identité et permettre la récupération

- **Statut / effort :** `IN_PROGRESS` / `L` — rouvert le 2026-08-10 : la
  vérification pouvait finaliser un compte, un mot de passe et un grant initiés
  par un tiers.
- **Problème :** `auth/oauth.go:425` crée immédiatement un compte à partir
  d'un email non vérifié. Il n'existe pas de récupération de mot de passe.
- **Actions :**
  - créer `pending_registrations` avec token haché, expiration et nombre
    d'essais borné ;
  - envoyer un OTP/lien à usage unique avant activation ;
  - ajouter récupération, révocation des sessions et journal d'audit ;
  - permettre de désactiver l'inscription publique au profit d'invitations.
- **Critères d'acceptation :**
  - un email non vérifié ne crée aucun learner actif ;
  - un token expiré, rejoué ou destiné à un autre email est refusé ;
  - une récupération révoque les anciennes familles de refresh tokens ;
  - les réponses HTTP ne révèlent pas l'existence d'un compte.
- **Tests :** succès, expiration, replay, concurrence de deux activations,
  récupération et révocation.

### P1-AUTH-02 — Supprimer l'énumération et l'oracle temporel

- **Statut / effort :** `IN_PROGRESS` / `S`
- **Problème :** `auth/oauth.go:443` annonce qu'un email existe et le login
  d'une adresse absente évite bcrypt (`auth/oauth.go:492`).
- **Actions :** réponse uniforme, hash bcrypt factice à coût courant et
  métriques internes sans PII.
- **Critères d'acceptation :** même statut, message et chemin cryptographique
  pour email absent, mot de passe faux et compte inactif ; aucun email brut
  dans les logs.
- **Tests :** assertions de réponse et test statistique tolérant sur les durées
  des chemins absent/existant.

### P1-AUTH-03 — Remplacer le verrouillage de compte contrôlable par l'attaquant

- **Statut / effort :** `IN_PROGRESS` / `M`
- **Problème :** cinq erreurs ciblées bloquent une adresse dix minutes dans
  `auth/login_failures.go` et `auth/oauth.go:483`.
- **Actions :** délais progressifs, signaux compte/IP/device, challenge ou MFA
  adaptatif et notification de sécurité ; ne jamais imposer un verrou global
  uniquement à partir de l'email.
- **Critères d'acceptation :** une attaque distribuée ne bloque pas durablement
  la victime ; le bruteforce reste borné ; les compteurs expirent.
- **Tests :** sources multiples, horloge contrôlée, succès après challenge et
  backend partagé indisponible.

### P1-OAUTH-01 — Protéger l'enregistrement dynamique des clients

- **Statut / effort :** `IN_PROGRESS` / `M` — rouvert le 2026-08-10 : les
  bornes de stockage ne constituent pas encore une politique d'admission et de
  saturation conforme au critère d'acceptation.
- **Problème :** `/register` est public, le plafond global de 10 000 clients
  est durable et il n'existe ni TTL ni suppression opérationnelle
  (`auth/oauth.go:58`, `auth/oauth.go:965`).
- **Actions :** initial access token ou software statement, allowlist de modes
  autorisés, TTL des clients jamais utilisés, quotas et commande d'administration.
- **Critères d'acceptation :** un appel non autorisé ne crée aucun client ; un
  acteur ne peut épuiser la capacité globale ; suppression et rotation sont
  auditées.
- **Tests :** saturation simulée, concurrence, expiration et suppression.

### P1-OAUTH-02 — Unifier le câblage sécurisé des routes OAuth

- **Statut / effort :** `DONE` / `S`
- **Problème :** `OAuthServer.RegisterRoutes` permet de câbler les endpoints
  sensibles sans les limiteurs appliqués explicitement dans `main.go`.
- **Actions :** supprimer ce chemin alternatif ou en faire l'unique fonction de
  montage exigeant les limiteurs et middlewares de sécurité.
- **Critères d'acceptation :** aucun code de production, test d'intégration ou
  consommateur interne ne peut monter `/authorize`, `/token` ou `/register`
  sans les protections attendues.
- **Tests :** test de composition du routeur et assertion 429 sur chaque endpoint
  coûteux après dépassement du budget.

### P1-ERR-01 — Propager les erreurs du moteur de motivation

- **Statut / effort :** `TODO` / `M`
- **Problème :** `MotivationEngine.Build` ignore des erreurs de lecture, de
  parsing et d'écriture dans `engine/motivation.go:287`.
- **Actions :** distinguer dépendances obligatoires et enrichissements
  facultatifs, retourner des erreurs typées ou `errors.Join`, exposer les
  composants dégradés sans données sensibles.
- **Critères d'acceptation :** une lecture obligatoire en échec remonte une
  erreur ; un enrichissement optionnel dégradé est observable ; un échec de
  rotation d'axe n'est pas silencieux.
- **Tests :** store injectant une erreur sur chaque dépendance.

### P1-ERR-02 — Distinguer absence de session et panne DB

- **Statut / effort :** `TODO` / `S`
- **Problème :** `record_session_close` ignore l'erreur de
  `GetActiveLearningSession` dans `tools/session_close.go:48`.
- **Actions :** introduire un sentinel `ErrNotFound`, propager toute autre
  erreur via `safeErrorResult` et normaliser ce contrat dans le Store.
- **Critères d'acceptation :** seule l'absence réelle ouvre le chemin de
  compatibilité ; timeout et panne DB produisent une erreur applicative.
- **Tests :** not-found, timeout, connexion fermée et succès.

### P1-ERR-03 — Ne pas convertir une panne DB en « configuration requise »

- **Statut / effort :** `TODO` / `S`
- **Problème :** certains chemins de `tools/activity.go` peuvent présenter une
  erreur de résolution comme `needs_domain_setup=true`.
- **Actions :** réserver ce résultat à `sql.ErrNoRows`/absence confirmée et
  propager les erreurs de backend.
- **Critères d'acceptation :** aucun incident DB n'est présenté comme une
  précondition métier normale.
- **Tests :** aucun domaine, domaine archivé, panne DB et timeout.

### P1-MEM-01 — Rendre l'état mémoire honnête en cas d'I/O dégradée

- **Statut / effort :** `TODO` / `M`
- **Problème :** `tools/learner_memory.go:185` ignore des erreurs de listing,
  lecture et `stat`, puis peut retourner `ok:true` avec des zéros.
- **Actions :** faire retourner `(valeur, erreur)` aux helpers et publier
  `ok:false` ou `degraded_components`.
- **Critères d'acceptation :** permission refusée, fichier corrompu et stockage
  indisponible sont visibles sans fuite de chemin local.
- **Tests :** filesystem injecté, permission, contenu invalide et absence normale.

### P1-JOB-01 — Remplacer le claim permanent par un lease récupérable

- **Statut / effort :** `TODO` / `L`
- **Problème :** `db/scheduler.go:12` insère seulement `(name, window_key)` ;
  un crash après le claim perd le travail.
- **Actions :** modèle `jobs` avec `status`, `owner`, `leased_until`, heartbeat,
  `attempts`, `next_attempt_at`, `last_error`, clé d'idempotence et DLQ.
- **Critères d'acceptation :** un worker mort est repris après expiration ; un
  worker vivant renouvelle son lease ; un job réussi n'est pas rejoué ; les
  échecs dépassant le budget vont en DLQ.
- **Tests :** crash après claim, horloge contrôlée, workers concurrents,
  redelivery et poison job sur SQLite/PostgreSQL selon les capacités.

### P1-JOB-02 — Supprimer les scans globaux et le fan-out synchrone

- **Statut / effort :** `TODO` / `L`
- **Problème :** `engine/scheduler.go:238` charge tous les apprenants, effectue
  des N+1 et envoie les webhooks séquentiellement ; les crons peuvent se chevaucher.
- **Actions :** sélection SQL minimale, pagination keyset, production de jobs
  bornés, pool de workers, deadlines et `SkipIfStillRunning` durant la transition.
- **Critères d'acceptation :** aucune liste complète en mémoire ; concurrence
  bornée ; progression et lag observables ; arrêt gracieux sans perdre les jobs.
- **Tests :** charge à 10k/100k identités synthétiques, chevauchement, annulation
  et noisy-neighbour.

### P1-WEBHOOK-01 — Unifier réservation, queue et statut de livraison

- **Statut / effort :** `TODO` / `M`
- **Problème :** certains messages créent une réservation sans lien durable
  vers la ligne de queue (`db/webhook_queue.go:172`).
- **Actions :** stocker `reservation_id`, utiliser les états explicites
  `reserved`, `processing`, `sent`, `failed`, `delivery_unknown` et journaliser
  chaque transition.
- **Critères d'acceptation :** une livraison réussie termine réservation et
  queue ; aucun booléen ambigu ; réconciliation possible après crash.
- **Tests :** chaque transition, concurrence, rollback et reprise.

### P1-WEBHOOK-02 — Formaliser l'at-least-once et l'idempotence

- **Statut / effort :** `TODO` / `M`
- **Problème :** un crash après succès HTTP mais avant `MarkWebhookSent` peut
  provoquer une duplication.
- **Actions :** `event_id` interne stable, backoff, DLQ, historique de livraison
  et état `delivery_unknown`. Utiliser une idempotency key uniquement lorsqu'un
  destinataire ou un relais contrôlé la supporte. Discord direct ne sait ni
  dédupliquer cet identifiant ni vérifier une signature applicative maison.
- **Critères d'acceptation :** un retry conserve le même événement interne ;
  une livraison incertaine n'est jamais déclarée non envoyée ; le risque de
  doublon Discord est assumé et documenté, ou supprimé via un relais contrôlé.
- **Tests :** crash aux frontières avant/après HTTP, timeout et réponse 2xx tardive.

### P1-MEM-02 — Abstraire la mémoire narrative hors du disque local

- **Statut / effort :** `TODO` / `L`
- **Problème :** `memory/store.go:64` utilise fichiers locaux et verrous
  intra-processus, incompatibles avec plusieurs nœuds.
- **Actions :** interface `NarrativeStore`, implémentation object storage ou DB,
  métadonnées indexées, ETag/version optimiste, checksum et chiffrement.
- **Critères d'acceptation :** deux nœuds lisent le même état ; conflit détecté ;
  écriture idempotente ; migration locale réconciliée par checksum.
- **Tests :** concurrence multi-writer, retry, objet absent/corrompu et backfill.
- **Dépendance :** prérequis du backlog multi-tenant pour l'horizontalisation.

### P1-QUERY-01 — Borner les requêtes de contexte apprenant

- **Statut / effort :** `TODO` / `M`
- **Problème :** `tools/context.go:52` charge domaines, états et interactions,
  puis filtre en Go.
- **Actions :** requêtes scopées, agrégations SQL, pagination/limites et DTO
  dédiés sans hash de mot de passe ni colonnes inutiles.
- **Critères d'acceptation :** volume lu borné ; plan PostgreSQL indexé ; budget
  p95 défini à 200, 10k et 100k apprenants synthétiques.
- **Tests :** cardinalités élevées et `EXPLAIN` contrôlé en benchmark/staging.

### P1-MCP-01 — Préparer le transport MCP stateless récent

- **Statut / effort :** `IN_PROGRESS` / `M` — rouvert le 2026-08-10 : le test
  round-robin partageait la même instance serveur entre les deux handlers et
  ne simulait pas deux processus indépendants.
- **Problème :** le projet reste sur le SDK v1.4.1 et le transport stateful,
  désormais borné mais local au nœud.
- **Actions :** évaluer puis migrer vers une version stable compatible avec le
  protocole stateless, garder un test de clients legacy et mesurer les écarts.
- **Critères d'acceptation :** chaque requête peut atteindre n'importe quel
  nœud ; les clients supportés négocient correctement ; aucun état métier ne
  dépend d'un contexte de transport caché.
- **Tests :** matrice de versions, load balancer round-robin et reprise de nœud.

### P1-SEC-01 — Chiffrer et faire tourner les secrets d'intégration

- **Statut / effort :** `IN_PROGRESS` / `M`
- **Problème :** `learners.webhook_url` contient un secret en clair dans
  `db/schema_pg.sql:10`.
- **Actions :** chiffrement enveloppe KMS, version de clé, rotation et
  déchiffrement juste avant envoi ; séparer le secret du profil learner.
- **Critères d'acceptation :** dump DB inexploitable seul ; rotation sans
  interruption ; secret absent des logs, erreurs et traces.
- **Tests :** round-trip, mauvaise clé, rotation et redaction.

### P1-DEPLOY-01 — Imposer TLS dans les profils de production

- **Statut / effort :** `DONE` / `S`
- **Problème :** HTTP public et DSN PostgreSQL non vérifié restent acceptés.
- **Actions :** profil `production`, refus de HTTP non-loopback, validation du
  proxy TLS et exigence `sslmode=verify-full`/CA explicite.
- **Critères d'acceptation :** démarrage refusé pour toute configuration
  production en clair ; staging couvre terminaison TLS et rotation certificat.
- **Tests :** matrice de configurations et DSN.

### P1-PRIV-01 — Rendre la rétention idempotente et auditable

- **Statut / effort :** `TODO` / `L`
- **Problème :** DB et fichiers sont supprimés en phases non atomiques ; les
  erreurs partielles sont difficiles à réconcilier (`db/retention.go:101`).
- **Actions :** manifeste de job, checkpoints, dry-run, phases idempotentes,
  rapport partiel, legal hold et alignement des sauvegardes.
- **Critères d'acceptation :** reprise après crash ; preuve des objets supprimés
  ou conservés ; aucune suppression hors politique ; restauration documentée.
- **Tests :** crash à chaque phase, rerun, legal hold et sauvegarde en retard.

### P1-TEST-01 — Étendre la matrice d'intégration PostgreSQL

- **Statut / effort :** `TODO` / `L`
- **Problème :** la CI PostgreSQL couvre principalement `db`, alors que les
  garanties de jobs, outils et scheduler dépendent du comportement réel de
  PostgreSQL et de son pool.
- **Actions :** exécuter `engine`, `tools`, scheduler, OAuth et scénarios de
  panne/redelivery sous PostgreSQL ; ajouter race detector et données de charge
  multi-organisme synthétiques.
- **Critères d'acceptation :** même contrat fonctionnel SQLite/PostgreSQL ;
  leases, outbox, RLS future, timeouts et réutilisation du pool validés sur
  PostgreSQL avant clôture de P1-JOB ou des jalons multi-tenant M2/M4.
- **Tests :** crash worker, lease expiré, redelivery, rollback/panic, saturation
  du pool et suite négative A/B lorsque RLS sera introduite.

## P2 — durcissement et réduction de dette

| ID | Statut / effort | Correction | Critères de clôture |
|---|---|---|---|
| P2-PROXY-01 | `TODO` / `S` | Parcourir `X-Forwarded-For` de droite à gauche en retirant les proxies approuvés (`auth/ratelimit.go:252`) ou imposer l'écrasement au proxy. | Tests avec XFF forgé, chaîne multi-proxy et pair non trusted. |
| P2-OPS-01 | `TODO` / `S` | Séparer `/live` sans DB et `/ready` avec dépendances, accès interne ou cache court. | Une attaque sur liveness ne charge pas la DB ; readiness reflète réellement les dépendances. |
| P2-OPS-02 | `TODO` / `S` | Remplacer les `context.Background()` des backends de rate-limit/login par contexte propagé et deadline courte. | Aucun appel backend ne peut immobiliser indéfiniment une goroutine. |
| P2-AUTH-01 | `TODO` / `S` | Rendre `RateLimiter.Stop` idempotent avec `sync.Once`/cancel. | Appels répétés et concurrents sans panic, testés sous `-race`. |
| P2-AUTH-02 | `TODO` / `S` | Borner la map locale des échecs de connexion avec TTL global et LRU. | Mémoire bornée sous flot d'emails uniques ; expiration déterministe. |
| P2-TX-01 | `TODO` / `M` | Introduire une interface `TxStore` métier au lieu du Store complet dans `WithTx`. | Impossible d'appeler `Close`, `Migrate`, `Ping` ou `WithTx` depuis le callback. |
| P2-DB-01 | `TODO` / `S` | Retirer `Migrate` du Store runtime ou dispatcher selon le dialecte en conservant le contexte (`db/store.go:72`). | PostgreSQL n'appelle jamais le migrateur SQLite ; annulation respectée. |
| P2-DATA-01 | `TODO` / `M` | Rendre le parsing des timestamps strict et migrer explicitement les valeurs legacy. | Valeur corrompue mesurée/quarantinée, jamais convertie silencieusement en zéro. |
| P2-OAUTH-01 | `TODO` / `M` | Implémenter réellement RFC 7592 ou retirer `registration_access_token` et `registration_client_uri`. | Aucun champ ou endpoint annoncé sans implémentation et persistance sécurisée. |
| P2-DEAD-01 | `TODO` / `S` | Déprécier puis supprimer les méthodes Store sans appel listées ci-dessous. | Deux releases de dépréciation si API publique ; interface et mocks nettoyés. |
| P2-LOG-01 | `TODO` / `M` | Centraliser la redaction des erreurs/panics et envoyer les stacks vers un sink restreint. | Tests avec PII, DSN, chemin et webhook ; aucune fuite dans le log standard. |
| P2-OBS-01 | `TODO` / `L` | Ajouter OpenTelemetry, request/trace IDs et métriques DB/jobs/webhooks/MCP. | Dashboards et alertes sur p95/p99, saturation pool, queue lag, DLQ et sessions. |

### Méthodes candidates à P2-DEAD-01

- `CountInteractionsByConceptInDomain`
- `GetConceptStateForUpdateInDomain`
- `GetConceptsDueForReviewByDomain`
- `GetSessionInteractionsInDomain`
- `GetTransferRecordsByDomain`

Avant suppression, confirmer qu'aucune intégration externe ne compile contre
ces méthodes exportées. `staticcheck` seul ne peut pas conclure sur cette
surface publique. Supprimer également le faux usage `var _ = sql.NullTime{}`
de `db/phase.go` dès que l'import n'est réellement plus nécessaire.

## Gate de validation globale

Chaque lot doit conserver les commandes suivantes vertes :

```bash
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
staticcheck ./...
govulncheck ./...
git diff --check
```

Pour les changements PostgreSQL, exécuter aussi la matrice avec
`TUTOR_TEST_PG_DSN`, puis joindre au ticket le plan de migration, le plan de
rollback et les mesures avant/après.
