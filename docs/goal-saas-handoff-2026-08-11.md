# Handoff du programme SaaS — arrêt du 2026-08-11

Ce document est le point de reprise durable du goal actif décrit dans
[`saas-goals.md`](./saas-goals.md). Il a été écrit à la demande de l'utilisateur
au moment d'interrompre volontairement les travaux. Il ne vaut ni validation
finale, ni release, ni autorisation de déploiement.

## État de contrôle

- Goal orchestrateur : **`paused`**.
- Branche : `agent/ci-security-hardening`.
- HEAD : `bcaaa74 feat: harden OAuth scopes and dependency failures`.
- La branche est en avance de deux commits sur
  `origin/agent/ci-security-hardening` :
  - `7e3bfbe feat: harden Goal 1 MCP runtime` ;
  - `bcaaa74 feat: harden OAuth scopes and dependency failures`.
- Aucun push ni déploiement n'a été effectué pendant cette phase.
- Le worktree contient un grand lot **non commité** et partiellement validé.
  Ne pas faire de `reset`, `checkout` destructif ou réécriture globale : les
  modifications appartiennent au travail en cours.
- `HANDOFF_2026-08-04.md` est un ancien fichier non suivi, potentiellement
  sensible. Il a été préservé tel quel et ne doit pas être ajouté à un commit
  sans revue humaine explicite.

Au point d'arrêt, `git diff --check` est vert. Aucun processus de test ou de
benchmark ne tourne encore. En revanche, aucun gate global final n'a été lancé
après les derniers changements webhook.

## Alerte environnement avant toute reprise

Au moment de l'arrêt :

- `/tmp` : environ 97 % utilisé, 139 MiB libres ;
- filesystem du workspace : environ 99 % utilisé, 1,4 GiB libre.

Le premier geste de la prochaine session doit être un inventaire en lecture
seule de l'espace. Ne supprimer que des caches temporaires identifiés comme
tels ; ne jamais supprimer récursivement le dépôt, `data/`, un répertoire
utilisateur ou un artefact inconnu. Une première compilation avait échoué avec
`no space left on device`, puis une compilation à caches déplacés avait validé
la majorité des packages sans produire de résultat global final avant l'arrêt.

## Travail déjà commité et à conserver

Les deux checkpoints locaux ci-dessus contiennent notamment :

- surface MCP réelle confirmée à 45 outils lorsque `REGULATION_GOAL` est actif
  (43 lorsqu'il est désactivé) ;
- transport MCP stateless sur le SDK courant, compatibilité legacy et test
  round-robin ;
- CORS MCP 2026, limite de corps, concurrence globale/par principal et deadline
  coopérative de `tools/call` ;
- annotations de sûreté corrigées et idempotence déclarée honnêtement ;
- OAuth `resource`/audience, PKCE, CSRF, refresh rotation et scopes par outil ;
- déploiement en deux phases de `OAUTH_GRANULAR_SCOPES`, sans widening de grant
  lors du refresh ;
- challenge `insufficient_scope` HTTP/MCP et exigences ALL pour les quatre
  outils mixtes lecture/écriture ;
- cycle d'activation de compte corrigé contre le pré-hijacking DCR : le tiers
  initiateur ne choisit plus le mot de passe ni le consentement de la victime ;
- erreurs de session/domaine/mémoire et motivation rendues observables sans
  exposer les causes brutes ;
- chiffrement AES-GCM/keyring des secrets d'intégration, profil production
  fail-closed, TLS/DSN durcis et redaction logs/panics ;
- tests SQLite/PostgreSQL des migrations OAuth/scopes et nettoyage des schémas
  de test.

La dernière base globale connue avant le gros lot non commité était verte pour
`go test ./...`, `go vet ./...`, `staticcheck ./...`, le race detector et la
suite PostgreSQL DB. Cette preuve ne couvre pas les changements non commités
décrits ci-dessous.

## Lots non commités au point d'arrêt

### 1. Budget bcrypt et anti-bruteforce — implémenté, ciblé vert

Fichiers principaux :

- `auth/bcrypt_budget.go`, `auth/bcrypt_budget_test.go` ;
- `auth/accounts.go`, `auth/accounts_test.go` ;
- `auth/login_failures.go`, `auth/login_failures_test.go` ;
- `auth/oauth.go`, `auth/oauth_extra_test.go`, `auth/pages.go` ;
- `startup_config.go`, `startup_config_test.go`, `main.go`, `README.md`.

État :

- budget CPU process-wide `AUTH_BCRYPT_MAX_CONCURRENT`, admission non bloquante ;
- tous les points bcrypt de production passent par le budget ;
- absent, mauvais mot de passe et compte inactif suivent une réponse uniforme ;
- une bonne authentification reste vérifiée avant toute pénalité attaquant ;
- `Retry-After` progressif sans sommeil serveur ;
- compteurs locaux bornés et contextes propagés au backend partagé ;
- erreurs backend journalisées par classe, sans cause brute.

Preuves acquises avant l'arrêt : suites `auth` et root ciblées, race ciblée,
`go vet ./auth .` et diff-check verts.

Résiduel : `P1-AUTH-03` reste `IN_PROGRESS`. PostgreSQL utilise encore un
journal d'événements par échec ; le remplacer par un agrégat fenêtré atomique
et borné. Les signaux device/challenge/MFA et notifications de sécurité restent
à concevoir. `P1-AUTH-02` doit encore recevoir son gate temporel tolérant final.

### 2. Cache CIMD — implémenté et validé

Fichiers : `auth/cimd.go`, `auth/cimd_test.go`.

- cache process-local borné à 256 entrées avec LRU et purge des expirations ;
- singleflight par client, y compris partage d'un fetch `no-store` sans le
  persister ;
- annulation d'un waiter indépendante du fetch partagé ;
- erreurs partagées puis retry possible ;
- tests ciblés, race et vet verts.

### 3. Read-model `get_learner_context` — implémenté, gate PG incomplet

Fichiers :

- `store/store.go` (DTO/interface étroits) ;
- `db/learner_context.go`, tests et benchmark ;
- `tools/context.go`, `tools/context_test.go`.

Le chemin actif passe de 13 à 5 requêtes constantes, sans charger email, hash
ou webhook. Sur SQLite, le benchmark 100 concepts / 5 000 interactions a
mesuré environ -22 % de latence médiane, -72 % d'octets et -79 %
d'allocations. L'équivalence fonctionnelle SQLite/PostgreSQL et l'isolation ont
été testées.

Benchmark PostgreSQL isolé interrompu proprement :

- 200 apprenants : cinq requêtes p50 **11,644 ms**, p95 **12,037 ms** ;
- 10 000 apprenants : p50 **13,311 ms**, p95 **21,436 ms** ;
- le CTE narratif domine : p95 11,194 ms puis 20,547 ms ;
- les quatre autres requêtes restent sous 0,55 ms ;
- 50 mesures après 5 warmups, cible dense 100 concepts / 5 000 interactions ;
- schéma isolé supprimé, vérification : zéro schéma résiduel.

SLO provisoire recommandé : p95 SQL cumulé des cinq lectures inférieur à
50 ms. Ne pas passer `P1-QUERY-01` à `DONE` avant le palier 100 000 et la
capture des `EXPLAIN (ANALYZE, BUFFERS)`.

### 4. Leases récupérables des jobs — cœur implémenté, résultat métier manquant

Fichiers :

- migration SQLite `0036_recoverable_scheduled_job_leases` ;
- migration PostgreSQL `postgres_0027_recoverable_scheduled_job_leases` ;
- `store/store.go`, `db/scheduler.go`, `engine/scheduler.go` ;
- `db/scheduler_lease_test.go`, `engine/scheduler_lease_test.go`.

État : lease, owner/fencing par tentative, heartbeat, reprise, backoff,
`succeeded` terminal, tentative maximale et DLQ sont présents. Les anciennes
lignes sont migrées comme succès pour éviter de rejouer des notifications.
Les tests ciblés SQLite, PostgreSQL et race étaient verts.

Résiduel bloquant `P1-JOB-01` : les fonctions métiers de scheduler retournent
encore souvent `void`, journalisent une panne puis terminent normalement. Le
lease peut donc déclarer une fenêtre réussie après un échec partiel. Introduire
un résultat explicite/sanitisé jusqu'à `runDistributedJob`, puis prouver que les
retries sont sûrs et bornés.

### 5. DCR production — sous-lot implémenté, gates globaux restants

Fichiers ajoutés :

- `auth/dcr_policy.go`, `auth/dcr_policy_test.go` ;
- `docs/oauth-dcr-production.md`.

Fichiers modifiés : `auth/oauth.go`, `startup_config.go`,
`startup_config_test.go`, `main.go`, `db/store.go`, `db/store_test.go`,
`README.md`, `docs/backlog-p1-p2.md`.

État :

- `OAUTH_DCR_MODE=open|token|disabled` ;
- développement sans valeur : `open` ; production refuse valeur absente ou
  `open` ;
- mode token : IAT base64url non paddé, au moins 32 octets décodés, maximum
  512 caractères ; seul SHA-256 est retenu ; comparaison constant-time ;
- Authorization dupliqué/comma-joined refusé ; 401 uniforme avec challenge ;
- admission placée avant le rate limiter : une IAT invalide ne lit pas le corps
  et ne touche ni limiteur, DB, bcrypt ni cleanup ;
- mode disabled : route non montée, non annoncée, handler direct fail-closed ;
- cleanup retiré du chemin `/register` ;
- échange code : consommation, création refresh et activation du client
  (`expires_at=NULL`) dans une transaction ; 0 ligne accepté pour CIMD ;
- activation sans filtre TTL, ce qui supprime la course à la frontière
  d'expiration ; rollback testé.

Gates verts : compilation ciblée auth/db/root, tests DCR/config, échange SQLite,
échange PostgreSQL. Non exécutés après le dernier gofmt : race final, vet,
suites complètes et diff-check final (le diff-check global exécuté lors de cet
arrêt est toutefois vert).

`P1-OAUTH-01` doit rester `IN_PROGRESS` : IAT statique sans chevauchement de
rotation/révocation/quota par IAT, configuration seulement au boot, pas encore
de commande d'administration ni d'audit durable de suppression/rotation.

### 6. Machine d'état webhook — structure DB posée, lot incomplet

Fichiers touchés et gofmtés :

- `models/motivation.go`, `models/webhook.go` ;
- `store/store.go` ;
- `db/availability.go`, `db/webhook_queue.go` ;
- migrations SQLite `0037_webhook_delivery_state_machine` et PostgreSQL
  `postgres_0028_webhook_delivery_state_machine`.

Structure présente :

- `event_id` stable, `reservation_id`, `dispatch_started_at` ;
- états `processing -> dispatching -> sent | failed/pending | delivery_unknown` ;
- liaison queue/réservation, transitions sans payload ni URL secrète ;
- résolution opérateur de l'état incertain ;
- stale `processing` récupérable, stale `dispatching` quarantiné ;
- expiration/reconciliation bornées.

État au STOP : la structure DB/interface était complète et la compilation à
caches déplacés n'avait montré aucune erreur dans root/algorithms/auth/cmd/db/
engine/memory/models/store/conformance, mais le résultat global final n'avait
pas été rendu. **Le runtime n'utilise pas encore cette machine d'état** :
`engine/scheduler.go` suit encore les chemins legacy et ne franchit pas
`BeginWebhookDelivery` / `CompleteWebhookDelivery` / `Mark...Unknown`.

Il manque :

- classification HTTP stricte : pré-boundary, réponse non-2xx connue,
  transport/timeout ambigu, 2xx suivi d'une panne DB ;
- raccord scheduler pour tous les types (daily, OLM, mirror, metacog) ;
- tests dédiés SQLite/PostgreSQL : crash aux frontières, concurrence,
  rollback, event_id stable, résolution, isolation et stale claims ;
- runbook opérateur et contrat Discord direct (pas de déduplication distante).

Ne jamais auto-retry une ligne `dispatching` devenue stale : elle doit devenir
`delivery_unknown`. Un timeout/erreur de transport après franchissement de la
frontière HTTP est également incertain, pas un échec connu.

## Worktree exact à l'arrêt

Fichiers suivis modifiés :

```text
README.md
auth/accounts.go
auth/accounts_test.go
auth/cimd.go
auth/cimd_test.go
auth/login_failures.go
auth/login_failures_test.go
auth/oauth.go
auth/oauth_extra_test.go
auth/pages.go
db/availability.go
db/migrations_checksum.go
db/postgres.go
db/scheduler.go
db/store.go
db/store_test.go
db/webhook_queue.go
docs/backlog-p1-p2.md
engine/scheduler.go
main.go
models/motivation.go
models/webhook.go
startup_config.go
startup_config_test.go
store/store.go
tools/context.go
tools/context_test.go
```

Fichiers non suivis du lot :

```text
auth/bcrypt_budget.go
auth/bcrypt_budget_test.go
auth/dcr_policy.go
auth/dcr_policy_test.go
db/learner_context.go
db/learner_context_benchmark_test.go
db/learner_context_test.go
db/scheduler_lease_test.go
docs/oauth-dcr-production.md
engine/scheduler_lease_test.go
```

Le fichier historique `HANDOFF_2026-08-04.md` est aussi non suivi, mais ne fait
pas partie de ce lot.

## Ordre de reprise recommandé

1. Vérifier espace disque et processus ; ne nettoyer que les caches identifiés.
2. Relire ce handoff, `docs/saas-goals.md`, `docs/backlog-p1-p2.md` et le diff.
3. Exécuter un gate de compilation sans mutation métier :

   ```bash
   git diff --check
   go test ./... -run '^$' -count=1
   ```

4. Finir **d'abord** le raccord webhook et ses tests. C'est le seul lot laissé
   volontairement à mi-intégration et il partage `engine/scheduler.go` avec les
   jobs.
5. Faire remonter les résultats explicites des handlers scheduler pour clôturer
   `P1-JOB-01` sans marquer les erreurs partielles comme succès.
6. Rejouer les gates ciblés DCR, webhook, leases, bcrypt, CIMD et read-model,
   puis PostgreSQL pour toute migration/persistance.
7. Terminer le benchmark read-model à 100 000 et capturer les plans.
8. Réaliser un checkpoint local unique seulement lorsque tous les gates sont
   verts. Ne pas inclure `HANDOFF_2026-08-04.md`. Ne pas pousser sans nouvelle
   demande explicite.

Gates de checkpoint attendus :

```bash
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
staticcheck ./...
govulncheck ./...
git diff --check
```

Ajouter les suites PostgreSQL ciblées avec `TUTOR_TEST_PG_DSN` depuis
l'environnement de test local ; ne pas recopier de credential dans les docs ou
les logs.

## Goal 1 restant après ce checkpoint

Ne pas déclarer Goal 1 terminé après le seul lot courant. L'ordre de réduction
de risque recommandé reste :

1. `P1-AUTH-02/03` : preuve temporelle + agrégat PostgreSQL atomique borné ;
2. `P1-JOB-01` : résultat métier explicite ;
3. `P1-WEBHOOK-01/02` : runtime + réconciliation + preuves crash ;
4. `P1-JOB-02` et `P1-SEC-01` ensemble : pagination keyset, pool borné,
   deadlines et déchiffrement du webhook uniquement juste avant l'envoi ;
5. `P1-MEM-02` : `NarrativeStore` partagé, version/CAS, checksum, chiffrement
   et backfill local vérifié ;
6. `P1-PRIV-01` : manifeste durable, reprise après crash, legal hold et
   alignement sauvegardes. Le code actuel possède déjà dry-run, transaction DB
   atomique, exécution idempotente et commande séparée, mais la phase fichiers
   reste hors transaction/manifeste ;
7. `P1-OAUTH-01`, `P1-TEST-01` et audit final exigence par exigence ;
8. gates globaux complets et seulement ensuite passage de Goal 1 à `DONE`.

## Suite du programme après Goal 1

Conserver l'ordre déjà validé, sans introduire prématurément des éléments du
control plane :

- **Goal 2 / M0-M3** : contrat tenant, IAM/RBAC, `Principal`, migration
  `tenant_id`, clés composites, PostgreSQL `FORCE RLS`, formations/cohortes/
  enrollments et état cognitif re-cléé ;
- **Goal 3 / M4** : API stateless, worker, migrateur, outbox, jobs récupérables,
  mémoire partagée et redelivery ;
- **Goal 4 / M5** : control plane, plans/entitlements/quotas, usage/billing,
  audit privilégié, OpenTelemetry/SLO, rétention, PITR et restauration logique
  tenant ;
- **M6 post-MVP** seulement après mesures de charge réelles.

Les décisions d'architecture conservées restent celles de
[`saas-goals.md`](./saas-goals.md) et
[`backlog-multitenant-saas.md`](./backlog-multitenant-saas.md) : monolithe Go
modulaire avant microservices, PostgreSQL pooled + `FORCE RLS`, URL MCP publique
unique, tenant personnel par défaut, et migrations expand/backfill/contract.
