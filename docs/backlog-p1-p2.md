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

- **Statut / effort :** `DONE` / `M` — clôturé le 2026-08-10 avec deux
  serveurs MCP et deux pools Store indépendants, un routage requête par requête
  sans session de transport, et les parcours `2026-07-28` et legacy.
- **Actions :** migrer le SDK Go v1.4.1 vers la version stable supportant
  `2026-07-28`, activer `Stateless`, propager l'annulation HTTP et mettre à jour
  les en-têtes CORS/protocole.
- **Critères d'acceptation :** round-robin sans sticky session, négociation
  `2026-07-28` et compatibilité legacy explicitement testée.

### P0-PROTO-03 — Publier l'autorisation et la sûreté par outil

- **Statut / effort :** `DONE` / `M` — clôturé le 2026-08-10 avec policy
  exhaustive des 45 outils, scopes lecture/écriture (ou les deux), rollout
  OAuth en deux phases, challenges HTTP/MCP et annotations testées.
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

- **Statut / effort :** `DONE` / `L` — la création reste inactive jusqu'à la
  preuve de boîte mail ; le détenteur choisit ensuite lui-même le mot de passe
  et confirme le client/destination exacts. La récupération révoque codes et
  familles de refresh tokens dans la même transaction.
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

- **Statut / effort :** `DONE` / `S` — absent, mauvais mot de passe et compte
  inactif suivent la même réponse et deux travaux bcrypt sous un budget CPU
  commun. Un padding complémentaire égalise aussi les anciens hashes à faible
  coût ; un test médian tolérant répété couvre l'oracle temporel.
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

- **Statut / effort :** `DONE` / `M` — le login vérifie désormais
  toujours le bon mot de passe avant la pénalité, avec `Retry-After`
  progressif sans sommeil serveur. Un budget bcrypt process-wide configurable
  rejette immédiatement la saturation sur tous les handlers. Les compteurs
  locaux sont bornés et les contextes HTTP atteignent le backend partagé.
  PostgreSQL/SQLite conservent une seule fenêtre atomique plafonnée par compte,
  au lieu d'une ligne par tentative. Après le seuil, un mot de passe correct
  depuis un appareil inconnu exige un challenge boîte mail à usage unique,
  confirmé par POST+CSRF ; l'appareil approuvé reste lié au compte et expire.
- **Problème :** cinq erreurs ciblées bloquent une adresse dix minutes dans
  `auth/login_failures.go` et `auth/oauth.go:483`.
- **Actions :** délais progressifs, signaux compte/IP/device, challenge ou MFA
  adaptatif et notification de sécurité ; ne jamais imposer un verrou global
  uniquement à partir de l'email.
- **Critères d'acceptation :** une attaque distribuée ne bloque pas durablement
  la victime ; le bruteforce reste borné ; les compteurs expirent.
- **Tests :** sources multiples, horloge contrôlée, succès après challenge et
  backend partagé indisponible.
- **Preuves :** incréments concurrents et cap testés sur SQLite/PostgreSQL ;
  sources IP et compte restent partagées entre nœuds ; expiration, backend
  indisponible et mémoire locale bornée sont couverts. Le parcours adaptatif
  teste notification, confirmation, cookie sécurisé lié à l'apprenant, succès
  après challenge et refus du replay, y compris sous `-race`.

### P1-OAUTH-01 — Protéger l'enregistrement dynamique des clients

- **Statut / effort :** `DONE` / `M` — modes `open|token|disabled`, capacités
  IAT hachées et partagées, admission avant corps/rate-limit, discovery/route
  conditionnelles et activation transactionnelle au premier code exchange
  sont livrés. Chaque IAT porte quota, expiration et révocation ; les rotations
  se chevauchent entre nœuds sans ressusciter un token révoqué.
- **Actions :** initial access token ou software statement, allowlist de modes
  autorisés, TTL des clients jamais utilisés, quotas et commande d'administration.
- **Critères d'acceptation :** un appel non autorisé ne crée aucun client ; un
  acteur ne peut épuiser la capacité globale ; suppression et rotation sont
  auditées.
- **Tests :** saturation simulée, concurrence, expiration et suppression.
- **Preuves :** le registre PostgreSQL/SQLite revalide le token et consomme son
  quota dans la transaction de création. L'empreinte canonique des métadonnées
  donne un seul `client_id` à 12 requêtes concurrentes équivalentes et un replay
  à quota plein ne recompte pas. Les secrets confidentiels rejouables sont des
  enveloppes AES-256-GCM liées au client/empreinte, authentifiées et rotatives ;
  sans keyring, le replay est refusé plutôt que dupliqué. Deux serveurs voient
  simultanément ancien et nouvel IAT, puis la révocation partagée est immédiate.
  `tutor-dcr-admin` fournit preview, create, rotate, revoke, list et audit ; le
  secret brut est généré sur stdout une seule fois. Créations, débuts de
  rotation, révocations, créations de clients et suppressions TTL sont audités
  durablement, ces dernières dans la transaction de cleanup. Suites complètes
  auth/db, matrice PostgreSQL et tests ciblés `-race` verts.

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

- **Statut / effort :** `DONE` / `M` — clôturé le 2026-08-10 avec erreurs
  typées par dépendance, enrichissements dégradés conservant le brief et
  composants stables exposés dans `get_next_activity`.
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

- **Statut / effort :** `DONE` / `S` — clôturé le 2026-08-10 avec
  `store.ErrNotFound`, erreurs backend expurgées et tests absence, timeout,
  connexion fermée, propriété et succès.
- **Problème :** `record_session_close` ignore l'erreur de
  `GetActiveLearningSession` dans `tools/session_close.go:48`.
- **Actions :** introduire un sentinel `ErrNotFound`, propager toute autre
  erreur via `safeErrorResult` et normaliser ce contrat dans le Store.
- **Critères d'acceptation :** seule l'absence réelle ouvre le chemin de
  compatibilité ; timeout et panne DB produisent une erreur applicative.
- **Tests :** not-found, timeout, connexion fermée et succès.

### P1-ERR-03 — Ne pas convertir une panne DB en « configuration requise »

- **Statut / effort :** `DONE` / `S` — clôturé le 2026-08-10 : seules les
  absences confirmées produisent `needs_domain_setup`; panne, timeout et erreur
  de sélection suivent des résultats distincts. Archivage et domaine étranger
  restent volontairement sur le chemin d'absence non révélateur.
- **Problème :** certains chemins de `tools/activity.go` peuvent présenter une
  erreur de résolution comme `needs_domain_setup=true`.
- **Actions :** réserver ce résultat à `sql.ErrNoRows`/absence confirmée et
  propager les erreurs de backend.
- **Critères d'acceptation :** aucun incident DB n'est présenté comme une
  précondition métier normale.
- **Tests :** aucun domaine, domaine archivé, panne DB et timeout.

### P1-MEM-01 — Rendre l'état mémoire honnête en cas d'I/O dégradée

- **Statut / effort :** `DONE` / `M` — clôturé le 2026-08-10 avec inventaire
  strict, valeurs optionnelles nulles en dégradation, composants explicites et
  aucune fuite de chemin ou contenu dans les résultats MCP.
- **Problème :** `tools/learner_memory.go:185` ignore des erreurs de listing,
  lecture et `stat`, puis peut retourner `ok:true` avec des zéros.
- **Actions :** faire retourner `(valeur, erreur)` aux helpers et publier
  `ok:false` ou `degraded_components`.
- **Critères d'acceptation :** permission refusée, fichier corrompu et stockage
  indisponible sont visibles sans fuite de chemin local.
- **Tests :** filesystem injecté, permission, contenu invalide et absence normale.

### P1-JOB-01 — Remplacer le claim permanent par un lease récupérable

- **Statut / effort :** `DONE` / `L` — le store et le scheduler utilisent
  désormais des leases clôturables avec fencing par tentative, heartbeat,
  reprise bornée, backoff, succès terminal et DLQ. Les migrations préservent
  les anciens claims comme succès pour ne pas dupliquer des notifications.
  Tous les handlers enregistrés retournent un résultat métier explicite et
  sanitisé ; un échec partiel suit le même budget de retry borné qu'un crash.
- **Problème :** `db/scheduler.go:12` insère seulement `(name, window_key)` ;
  un crash après le claim perd le travail.
- **Actions :** modèle `jobs` avec `status`, `owner`, `leased_until`, heartbeat,
  `attempts`, `next_attempt_at`, `last_error`, clé d'idempotence et DLQ.
- **Critères d'acceptation :** un worker mort est repris après expiration ; un
  worker vivant renouvelle son lease ; un job réussi n'est pas rejoué ; les
  échecs dépassant le budget vont en DLQ.
- **Tests :** crash après claim, horloge contrôlée, workers concurrents,
  redelivery et poison job sur SQLite/PostgreSQL selon les capacités.
- **Preuves :** reprise après lease expiré, heartbeat concurrent, fencing,
  succès terminal, panic, échec métier, DLQ et retry webhook sans dépassement
  du backoff durable sont testés sur SQLite/PostgreSQL et sous `-race`.

### P1-JOB-02 — Supprimer les scans globaux et le fan-out synchrone

- **Statut / effort :** `DONE` / `L` — les anciennes API de listes globales ont
  été retirées. Les notifications et consolidations utilisent des projections
  minimales paginées par keyset (128 lignes par défaut), alimentent un pool
  borné (8 workers par défaut), propagent une deadline par cible et publient la
  progression (`page`, `page_size`, `has_more`, `processed`, `failed`).
- **Problème :** `engine/scheduler.go:238` charge tous les apprenants, effectue
  des N+1 et envoie les webhooks séquentiellement ; les crons peuvent se chevaucher.
- **Actions :** sélection SQL minimale, pagination keyset, production de jobs
  bornés, pool de workers, deadlines et `SkipIfStillRunning` durant la transition.
- **Critères d'acceptation :** aucune liste complète en mémoire ; concurrence
  bornée ; progression et lag observables ; arrêt gracieux sans perdre les jobs.
- **Tests :** charge à 10k/100k identités synthétiques, chevauchement, annulation
  et noisy-neighbour.
- **Preuves :** le gate opt-in
  `TUTOR_TEST_SCHEDULER_CARDINALITY=10000|100000` passe sur PostgreSQL : 79 puis
  782 pages, jamais plus de 128 DTO étroits en une page ; au palier 100k, le
  parcours webhook prend 1,56 s et la consolidation 0,50 s sur l'environnement
  local du 2026-08-11. Les tests couvrent également la concurrence maximale,
  l'absence de famine par une cible lente, la deadline, l'arrêt, le
  `SkipIfStillRunning` et le détecteur de courses. Les suites complètes `db` et
  `engine` passent sur SQLite et PostgreSQL.

### P1-WEBHOOK-01 — Unifier réservation, queue et statut de livraison

- **Statut / effort :** `DONE` / `M` — la réservation, la queue et l'historique
  de transitions partagent désormais une machine d'état transactionnelle ; les
  fallbacks générés sont eux-mêmes persistés et claimés avant tout appel HTTP.
- **Problème :** certains messages créent une réservation sans lien durable
  vers la ligne de queue (`db/webhook_queue.go:172`).
- **Actions :** stocker `reservation_id`, utiliser les états explicites
  `reserved`, `processing`, `sent`, `failed`, `delivery_unknown` et journaliser
  chaque transition.
- **Critères d'acceptation :** une livraison réussie termine réservation et
  queue ; aucun booléen ambigu ; réconciliation possible après crash.
- **Tests :** chaque transition, concurrence, rollback et reprise.

### P1-WEBHOOK-02 — Formaliser l'at-least-once et l'idempotence

- **Statut / effort :** `DONE` / `M` — l'`event_id` reste stable entre les
  tentatives, les réponses HTTP non-2xx sont des échecs connus, et toute erreur
  transport après la frontière HTTP passe en `delivery_unknown` sans retry
  automatique. La réconciliation et la résolution opérateur sont documentées
  dans `docs/webhook-delivery-operations.md`.
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

- **Statut / effort :** `DONE` / `L` — le backend `database` stocke des objets
  narratifs chiffrés et versionnés sous une identité structurée
  apprenant/domaine/scope/clé. Les écritures utilisent CAS, quotas
  transactionnels et journal de mutations idempotent ; le profil distribué ou
  production refuse désormais une mémoire active locale.
- **Problème :** `memory/store.go:64` utilise fichiers locaux et verrous
  intra-processus, incompatibles avec plusieurs nœuds.
- **Actions :** interface `NarrativeStore`, implémentation object storage ou DB,
  métadonnées indexées, ETag/version optimiste, checksum et chiffrement.
- **Critères d'acceptation :** deux nœuds lisent le même état ; conflit détecté ;
  écriture idempotente ; migration locale réconciliée par checksum.
- **Tests :** concurrence multi-writer, retry, objet absent/corrompu et backfill.
- **Dépendance :** prérequis du backlog multi-tenant pour l'horizontalisation.
- **Preuves :** deux instances `Store` partagent immédiatement mémoire,
  pending, sessions et concepts ; un seul multi-writer gagne une version et
  les appends concurrents sont tous préservés une fois. Les tests SQLite et
  PostgreSQL couvrent conflit de version et de mutation, replay exact, quotas,
  ciphertext/checksum/AAD, objet absent ou corrompu, rotation rolling sans
  modifier la version, et backfill create-only réconcilié par checksum sans
  suppression de la source. Un test MCP rejoue un append, lit les statistiques
  partagées et prouve qu'aucun fichier local n'est créé. Les suites ciblées
  passent aussi sous `-race`; le rollout, les conflits et le rollback sont
  documentés dans `narrative-memory-operations.md`.

### P1-QUERY-01 — Borner les requêtes de contexte apprenant

- **Statut / effort :** `DONE` / `M` — le chemin actif est passé de 13 à
  5 requêtes constantes avec DTO étroits et agrégats SQL. À 100 concepts et
  5 000 interactions sous SQLite : environ -72 % d'octets, -79 %
  d'allocations et -22 % de latence médiane ; l'équivalence SQLite/PostgreSQL
  et l'isolation sont testées.
- **Problème :** `tools/context.go:52` charge domaines, états et interactions,
  puis filtre en Go.
- **Actions :** requêtes scopées, agrégations SQL, pagination/limites et DTO
  dédiés sans hash de mot de passe ni colonnes inutiles.
- **Critères d'acceptation :** volume lu borné ; plan PostgreSQL indexé ; budget
  p95 défini à 200, 10k et 100k apprenants synthétiques.
- **Tests :** cardinalités élevées et `EXPLAIN` contrôlé en benchmark/staging.
- **Preuves :** le gate PostgreSQL isolé
  `TUTOR_TEST_LEARNER_CONTEXT_CARDINALITY=200|10000|100000` fixe un budget dur
  de 75 ms pour les cinq lectures, après warmup et sur 50 mesures. Le
  2026-08-11, les p95 observés sont respectivement 56,6 ms, 59,5 ms et 55,7 ms
  avec 100 concepts et 5 000 interactions sur la cible. Le test capture
  `EXPLAIN (ANALYZE, BUFFERS)` sur le SQL exact de production et exige au
  palier 100k un plan indexé ; celui-ci utilise les index interactions par
  apprenant/domaine. Les tests d'équivalence et d'isolation restent verts sur
  SQLite/PostgreSQL.

### P1-MCP-01 — Préparer le transport MCP stateless récent

- **Statut / effort :** `DONE` / `M` — clôturé le 2026-08-10 avec SDK Go
  v1.7.0, transport stateless et test round-robin entre deux serveurs, handlers
  et pools DB indépendants pour le protocole courant et le client legacy.
- **Problème :** le projet reste sur le SDK v1.4.1 et le transport stateful,
  désormais borné mais local au nœud.
- **Actions :** évaluer puis migrer vers une version stable compatible avec le
  protocole stateless, garder un test de clients legacy et mesurer les écarts.
- **Critères d'acceptation :** chaque requête peut atteindre n'importe quel
  nœud ; les clients supportés négocient correctement ; aucun état métier ne
  dépend d'un contexte de transport caché.
- **Tests :** matrice de versions, load balancer round-robin et reprise de nœud.

### P1-SEC-01 — Chiffrer et faire tourner les secrets d'intégration

- **Statut / effort :** `DONE` / `M` — les credentials Discord sont des
  enveloppes AES-256-GCM versionnées, liées par AAD à la ligne apprenant et à
  la colonne. Le keyring est injecté par le gestionnaire de secrets, obligatoire
  en production et conserve les anciennes versions pendant une rotation.
- **Problème :** `learners.webhook_url` contient un secret en clair dans
  `db/schema_pg.sql:10`.
- **Actions :** chiffrement enveloppe KMS, version de clé, rotation et
  déchiffrement juste avant envoi ; séparer le secret du profil learner.
- **Critères d'acceptation :** dump DB inexploitable seul ; rotation sans
  interruption ; secret absent des logs, erreurs et traces.
- **Tests :** round-trip, mauvaise clé, rotation et redaction.
- **Preuves :** le profil `Learner` et la page scheduler ne portent plus le
  credential ; le point unique de livraison le charge et le déchiffre une fois,
  après le claim durable et juste avant la frontière HTTP. La rotation
  authentifie aussi les enveloppes déjà marquées avec la clé courante, valide
  toutes les lignes avant sa transaction et reste atomique sur secret legacy
  invalide. Les tests SQLite/PostgreSQL et `-race` couvrent dump sans plaintext,
  ancienne/mauvaise clé, tag GCM altéré, rotation/rollback, redaction des erreurs
  transport et redaction des panics/stacks.

### P1-DEPLOY-01 — Imposer TLS dans les profils de production

- **Statut / effort :** `DONE` / `S`
- **Problème :** HTTP public et DSN PostgreSQL non vérifié restent acceptés.
- **Actions :** profil `production`, refus de HTTP non-loopback, validation du
  proxy TLS et exigence `sslmode=verify-full`/CA explicite.
- **Critères d'acceptation :** démarrage refusé pour toute configuration
  production en clair ; staging couvre terminaison TLS et rotation certificat.
- **Tests :** matrice de configurations et DSN.

### P1-PRIV-01 — Rendre la rétention idempotente et auditable

- **Statut / effort :** `DONE` / `L` — chaque apply possède un manifeste
  durable et immuable (politique, cutoff, acteur et preuve de sauvegarde de
  moins de 24 h), un bail récupérable et des checkpoints `database` puis
  `narrative`. Les rapports par catégorie distinguent `eligible`, `applied` et
  `held`; les échecs partiels et leur cause restent interrogeables.
- **Problème :** DB et fichiers sont supprimés en phases non atomiques ; les
  erreurs partielles sont difficiles à réconcilier (`db/retention.go:101`).
- **Actions :** manifeste de job, checkpoints, dry-run, phases idempotentes,
  rapport partiel, legal hold et alignement des sauvegardes.
- **Critères d'acceptation :** reprise après crash ; preuve des objets supprimés
  ou conservés ; aucune suppression hors politique ; restauration documentée.
- **Tests :** crash à chaque phase, rerun, legal hold et sauvegarde en retard.
- **Preuves :** migrations SQLite/PostgreSQL pour jobs/phases/legal holds ;
  mutations relationnelles et checkpoint atomiques ; suppression narrative
  idempotente après panne partielle ; un seul propriétaire de bail concurrent ;
  reprise sans rejouer la phase DB ; legal hold relationnel, fichier local et
  backend narratif partagé ; rejet d'une sauvegarde périmée et d'une dérive de
  manifeste. Les suites de rétention passent sur SQLite/PostgreSQL et sous
  `-race`; le runbook documente statut, reprise, release auditée et
  réconciliation obligatoire après restauration.

### P1-TEST-01 — Étendre la matrice d'intégration PostgreSQL

- **Statut / effort :** `DONE` / `L` — la CI exécute `go test -race ./...`
  avec PostgreSQL 17, pools bornés et un gate de 2 000 écritures concurrentes.
  Des parcours dédiés exercent les handlers OAuth, la boucle MCP et la reprise
  scheduler sur le dialecte réel, en plus de la conformance Store.
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
- **Preuves :** la matrice exacte du workflow passe en 9 min 58 s avec
  `TUTOR_LOAD_TEST=1` : `db` 598,5 s, `engine` 488,2 s, `auth` 394,6 s et
  `tools` 130,7 s sous race. Le pool de fixture reproduit la borne production
  et le test de 124 incréments login concurrents ne sature pas PostgreSQL. Les
  tests de migration, crash/lease/redelivery, rollback/panic et réutilisation
  du pool sont verts. La suite négative RLS A/B reste le gate du Goal 2, au
  moment où le schéma tenant et `FORCE RLS` existent réellement.

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
| P2-OAUTH-01 | `DONE` / `M` | Les champs `registration_access_token` et `registration_client_uri` ont été retirés tant que RFC 7592 n'est pas implémenté. | Aucun champ ou endpoint de gestion client n'est annoncé sans implémentation. |
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
