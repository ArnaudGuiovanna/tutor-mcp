# Programme SaaS — quatre Goals successifs

Ce document est la mémoire durable du programme de transformation de Tutor MCP.
Il complète les backlogs détaillés :

- [`backlog-p1-p2.md`](./backlog-p1-p2.md) pour la sécurité, la fiabilité et
  l'exploitation ;
- [`backlog-multitenant-saas.md`](./backlog-multitenant-saas.md) pour le modèle
  tenant, l'isolation des données et le plan de contrôle SaaS.

Dernier point de reprise :
[`goal-saas-handoff-2026-08-11.md`](./goal-saas-handoff-2026-08-11.md). Ce
handoff est la photographie de reprise ; l'état courant et les preuves de
clôture de chaque lot sont tenus dans les backlogs détaillés ci-dessus.

Les Goals sont exécutés dans l'ordre. Un Goal n'est clôturé que lorsque son gate
est prouvé par les tests et artefacts indiqués ; le suivant ne doit pas masquer
une dette du précédent.

## Goal 1 — Sécurité actuelle et conformité MCP/OAuth

**Statut : `DONE` — démarré le 2026-08-09, clôturé le 2026-08-12.**

Audit de sortie :
[`goal1-final-audit-2026-08-12.md`](./goal1-final-audit-2026-08-12.md).

### Résultat attendu

Le serveur mono-tenant peut être exposé pour une bêta contrôlée sans les
vulnérabilités confirmées lors de l'audit du 2026-08-09, et son endpoint public
canonique `/mcp` respecte le protocole MCP et le profil OAuth actuels. Ce Goal
ne crée pas encore le modèle tenant ni la RLS.

### Périmètre obligatoire

1. **Identité et cycle de compte**
   - aucune identité active ni aucun token à partir d'un email non vérifié ;
   - réponses non énumérables et chemin cryptographique comparable ;
   - récupération de compte sûre et révocation des familles de refresh tokens ;
   - protection anti-bruteforce sans verrouillage global contrôlable par un tiers.
2. **OAuth/MCP**
   - liaison RFC 8707 du paramètre `resource` à l'autorisation, au code, au
     refresh token et à l'audience exacte de l'access token ;
   - CIMD prioritaire, DCR public conservé comme fallback compatible, borné et
     nettoyable ; aucun endpoint ou credential RFC 7592 fictif ;
   - SDK Go compatible MCP `2026-07-28`, transport stateless et compatibilité
     explicitement testée avec les clients legacy retenus ;
   - scopes, `securitySchemes` et annotations de sûreté exactes par outil.
3. **Abus et disponibilité**
   - permissions SQLite locales `0700`/`0600` vérifiées au démarrage ;
   - quotas bornant mémoire narrative, fichiers, écritures et concurrence MCP ;
   - chaîne proxy non usurpable ; état rate-limit/login nettoyé et appels
     backend dotés de deadlines ; `/live` distinct de `/ready`.
4. **Gate production sécurité**
   - HTTPS public et PostgreSQL TLS fail-closed dans le profil production ;
   - secrets d'intégration chiffrés/rotatifs et logs/panics expurgés ;
   - documentation d'exploitation et procédure de rollback à jour.

### Preuves de sortie

- tests unitaires et d'intégration de chaque invariant ci-dessus ;
- tests OAuth négatifs : mauvais `resource`, audience, client, redirect URI,
  PKCE, replay et refresh inter-resource ;
- tests MCP legacy + `2026-07-28`, round-robin sans sticky session et charge
  bornée ;
- matrice SQLite/PostgreSQL lorsque le changement touche la persistance ;
- `go test ./... -count=1`, `go test -race ./... -count=1`, `go vet ./...`,
  `staticcheck ./...`, `govulncheck ./...` et `git diff --check` verts ;
- audit final exigence par exigence, sans se contenter de l'absence d'alerte.

## Goal 2 — Fondations tenant et isolation PostgreSQL

**Statut : `DONE` — démarré et clôturé le 2026-08-12.**

Audit de sortie :
[`goal2-final-audit-2026-08-12.md`](./goal2-final-audit-2026-08-12.md).

Ce Goal livre explicitement les jalons **M0 à M3** du backlog multitenant.
Introduire utilisateurs globaux, identités externes, tenants, memberships,
RBAC et `Principal` tenant-aware ; migrer par expand/backfill/contract vers
`tenant_id NOT NULL`, clés/FK composites et PostgreSQL `FORCE RLS`. Puis
séparer le contenu partagé de la progression individuelle avec formations
versionnées immuables, cohortes/course runs et enrollments ; re-cléer l'état
cognitif par `(tenant_id, enrollment_id, concept_id)` avant d'horizontaliser
le runtime. Le gate exige une suite négative tenant A/B couvrant lecture et
mutation, y compris commit, rollback, panic, annulation et réutilisation du
pool, ainsi qu'une migration legacy réconciliée sans association silencieuse.

## Goal 3 — Runtime horizontal et données partagées

**Statut : `DONE` — clôturé le 2026-08-12.**

Audit de sortie :
[`goal3-final-audit-2026-08-12.md`](./goal3-final-audit-2026-08-12.md).

Ce Goal livre le jalon **M4**. Séparer API/MCP, worker et migrateur ; déplacer
les rôles de déploiement autour du stockage narratif partagé versionné livré au
Goal 1 ; généraliser outbox, queue, leases récupérables, workers idempotents,
DLQ et cache/rate limits partagés. Le gate exige du round-robin sans affinité,
reprise après perte de nœud et tests de redelivery.

## Goal 4 — SaaS exploitable et commercialisable

**Statut : `DONE` — clôturé le 2026-08-13 après gates finaux.**

Audit de sortie :
[`goal4-final-audit-2026-08-12.md`](./goal4-final-audit-2026-08-12.md).

Ce Goal livre le jalon **M5**. Livrer portail/control plane, plans,
entitlements, quotas durables, événements d'usage idempotents, billing
réconciliable, audit privilégié, OpenTelemetry, SLO, politiques de rétention,
PITR et restauration logique d'un tenant. Le gate final est la définition de
« SaaS MVP prêt » du backlog multitenant. Le jalon **M6** reste volontairement
post-MVP et n'est lancé qu'après mesure des besoins de partitionnement,
analytics hors OLTP ou architecture cellulaire.

## Décisions conservées entre les Goals

- URL publique canonique unique, par exemple `https://mcp.tutor.example/mcp` ;
  aucun token ou tenant dans l'URL.
- Un utilisateur individuel reçoit un tenant personnel par défaut ; le choix du
  tenant n'apparaît que pour les utilisateurs multi-memberships.
- Monolithe Go modulaire avant microservices ; PostgreSQL pooled + RLS avant
  base par tenant ; architecture cellulaire seulement après mesure.
- L'identité du client OAuth Claude/ChatGPT peut être globale. Les grants,
  consentements, authorization codes et refresh tokens portent le tenant, le
  membership, la ressource et les scopes. Les clients enterprise dédiés et
  service accounts peuvent être tenant-scoped.
- Toute migration suit expand/contract et possède un rollback documenté.
