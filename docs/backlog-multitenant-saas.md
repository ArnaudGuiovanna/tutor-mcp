# Backlog — transformation multi-tenant SaaS

> **Orchestration.** La séquence et les gates des quatre Goals sont mémorisés
> dans [`saas-goals.md`](./saas-goals.md). Ce backlog est principalement livré
> par les Goals 2 à 4 ; le Goal 1 en sécurise les prérequis MCP/OAuth.
>
> **Objectif.** Transformer le runtime pédagogique mono-tenant actuel en SaaS
> capable d'héberger plusieurs organismes, formations, cohortes et apprenants,
> sans fuite inter-organismes et sans migration « big bang ».
>
> **Backlog complémentaire.** Les corrections de sécurité applicative,
> fiabilité et exploitation qui ne relèvent pas de l'isolation tenant sont dans
> [`backlog-p1-p2.md`](./backlog-p1-p2.md). Les P0 MCP/transactions déjà livrés
> sont des prérequis acquis.

## Convention de suivi

- Statuts : `TODO`, `READY`, `IN_PROGRESS`, `BLOCKED`, `DONE`.
- Effort relatif : `S`, `M`, `L`, `XL` ; il ne s'agit pas d'un engagement calendaire.
- Préfixes : `MT-FND` fondations, `MT-IAM` identité, `MT-DATA` données,
  `MT-LRN` apprentissage, `MT-RUN` runtime, `MT-CTL` plan de contrôle,
  `MT-OPS` exploitation et `MT-MIG` migration.
- Une tâche n'est `DONE` qu'après critères d'acceptation, tests d'isolation,
  métriques et procédure de rollback.

## Décisions structurantes

1. **Pooled multi-tenancy + PostgreSQL RLS pour le premier SaaS.** Une base
   par tenant n'est pas le point de départ.
2. **Utilisateur global, membership local.** L'email n'est jamais une preuve
   suffisante pour fusionner des identités entre organismes.
3. **Un token sélectionne exactement un tenant.** Le tenant ne vient jamais
   d'un simple header modifiable par le client.
4. **Contenu partagé, progression individuelle.** Une formation/version est
   publiée une fois ; l'état cognitif appartient à une inscription.
5. **Monolithe Go modulaire, deux processus.** API/MCP stateless d'un côté,
   worker asynchrone de l'autre, avant toute multiplication de microservices.
6. **Expand/contract.** Ajout nullable, backfill, dual-read/write, validation,
   contrainte stricte, puis suppression différée.
7. **Architecture cellulaire plus tard.** Elle devient utile pour les très gros
   tenants, la résidence des données ou les noisy neighbours, pas pour le MVP.

## Dépendances avec le backlog P1/P2

Ces lignes représentent une seule livraison technique, pas deux
implémentations concurrentes. Un même changement peut clôturer les deux tickets
si tous leurs critères d'acceptation sont satisfaits.

| Ticket multi-tenant | Prérequis dans `backlog-p1-p2.md` |
|---|---|
| MT-IAM-04 | P1-AUTH-01 à 03 |
| MT-IAM-05 | P1-OAUTH-01 à 02 et P2-OAUTH-01 |
| MT-RUN-02 | P1-MCP-01 |
| MT-DATA-03 à 04 | P1-TEST-01 |
| MT-RUN-03 | P1-JOB-01 à 02, P1-WEBHOOK-01 à 02 et P1-TEST-01 |
| MT-RUN-04 | P1-MEM-01 à 02 |
| MT-RUN-06 | P1-WEBHOOK-01 à 02 et P1-SEC-01 |
| MT-OPS-01 | P2-OBS-01 |
| MT-OPS-03 | P1-PRIV-01 |

## Invariants non négociables

- Toute table appartenant à un organisme porte `tenant_id NOT NULL`.
- Clés uniques, index et FK tenant-owned incluent `tenant_id`.
- Une FK composite interdit toute relation croisée entre tenants.
- RLS utilise `USING` **et** `WITH CHECK`, puis `FORCE ROW LEVEL SECURITY`.
- Le rôle runtime n'est ni propriétaire des tables ni `BYPASSRLS`.
- Le tenant PostgreSQL est posé avec `SET LOCAL` dans une transaction ; jamais
  avec un réglage de session susceptible de fuiter dans le pool.
- Toute méthode métier reçoit un scope/principal tenant-aware ; aucune nouvelle
  méthode `Get...ByID(id)` globale n'est acceptée.
- Un enrollment fige une `formation_version` publiée et immuable.
- Une mutation et son événement externe sont écrits dans la même transaction
  via une outbox.
- Jobs, webhooks et événements d'usage possèdent une clé d'idempotence incluant
  le tenant.
- Les logs, traces, exports, caches, objets et clés de queue conservent le scope
  tenant sans utiliser l'identifiant brut comme label métrique cardinal.

## Modèle métier cible

| Entité | Responsabilité | Relations essentielles |
|---|---|---|
| `tenants` | Organisme, statut, région/cellule, branding, politiques | Racine de toutes les données tenant-owned |
| `users` | Identité globale vérifiée | Aucune autorisation tenant directe |
| `external_identities` | OIDC/SAML/SCIM et identifiants fournisseurs | FK vers `users`, unicité par issuer/subject |
| `tenant_memberships` | Relation user/tenant, rôle, statut, version | Clé unique `(tenant_id, user_id)` |
| `formations` | Catalogue logique d'un organisme | Plusieurs versions et cohortes |
| `formation_versions` | Snapshot publié immuable | Une version référence modules/concepts stables |
| `modules`, `concepts`, `concept_prerequisites` | Curriculum normalisé | Toutes les FK incluent tenant/version |
| `cohorts` / `course_runs` | Session pédagogique datée, capacité, formateurs | Référence une version publiée |
| `enrollments` | Inscription d'un user à une cohorte | Fige tenant, user, cohorte et version |
| `learner_concept_states` | BKT/FSRS/IRT/PFA individuel | Clé `(tenant_id, enrollment_id, concept_id)` |
| sessions/interactions/évaluations | Preuves et événements d'apprentissage | Référencent enrollment et concept stables |
| `notification_channels`, `tenant_integrations` | Canaux et secrets séparés du user | Secrets chiffrés et versionnés |
| `plans`, `subscriptions`, `entitlements` | Droits commerciaux | Une source de vérité par tenant |
| `usage_events`, `usage_rollups` | Comptage idempotent et facturation | Clés d'événement stables |
| `jobs`, `outbox_events`, `webhook_deliveries` | Asynchrone durable | Scope tenant et retries bornés |
| `audit_events` | Journal privilégié append-only | Acteur, action, cible, tenant et trace |

## Jalons

| Jalon | Résultat | Tickets requis |
|---|---|---|
| M0 — contrat | Scope tenant présent dans toutes les frontières nouvelles | MT-FND-01 à 03 |
| M1 — identité/RBAC | Identités, memberships, tenant actif et autorisations centralisées | MT-IAM-01 à 05 |
| M2 — données isolées | Backfill terminé, FK composites et RLS forcées | MT-DATA-01 à 05, MT-MIG-01 |
| M3 — catalogue partagé | Formation versionnée, cohorte et enrollment | MT-LRN-01 à 05, MT-MIG-02 |
| M4 — runtime horizontal | API stateless, workers, outbox, object storage | MT-RUN-01 à 06 |
| M5 — SaaS exploitable | Quotas, usage, billing, audit, SLO et restauration | MT-CTL-01 à 05, MT-OPS-01 à 04, MT-OPS-08 |
| M6 — industrialisation | Cellules, analytics et restauration tenant | MT-OPS-05 à 07, MT-MIG-03 |

## Fondations et contrat de scope

### MT-FND-01 — Inventorier la propriété de chaque table

- **Statut / effort :** `TODO` / `M`
- **Actions :** produire une matrice table → propriétaire → volumétrie →
  politique de rétention → stratégie de backfill. Classer chaque table en
  globale, tenant-owned ou dérivée.
- **Critères d'acceptation :** aucune table ni objet fichier sans décision ;
  toutes les relations pouvant croiser learner/domain sont listées ; les
  anomalies ont une règle de quarantaine, pas une déduction silencieuse.

### MT-FND-02 — Introduire `Principal` et `TenantScope`

- **Statut / effort :** `TODO` / `M`
- **Cible :** remplacer le seul `learner_id` de `auth/middleware.go` par une
  structure telle que :

```go
type Principal struct {
    UserID       string
    TenantID     string
    MembershipID string
    Roles        []string
    Scopes       []string
    TokenVersion int64
}
```

- **Actions :** helpers de contexte typés, validation centralisée et compatibilité
  temporaire avec `GetLearnerID` pour le tenant legacy.
- **Critères d'acceptation :** principal obligatoire sur toute route métier ;
  tenant absent/ambigu refusé ; aucune valeur sensible issue d'un header libre.
- **Tests :** token sans tenant, membership suspendu, token ancien et utilisateur
  membre de deux tenants.

### MT-FND-03 — Interdire les accès Store non scopés

- **Statut / effort :** `TODO` / `L`
- **Actions :** interfaces étroites recevant `TenantScope`, méthodes SQL dont
  les prédicats commencent par `tenant_id`, garde/linter de revue pour toute
  nouvelle méthode globale.
- **Critères d'acceptation :** aucune méthode métier publique ne lit/écrit par
  ID seul ; exceptions globales documentées pour control plane/migrations.
- **Tests :** faux stores vérifiant le scope et tests négatifs A/B par méthode.

## Identité, memberships et autorisation

### MT-IAM-01 — Créer tenants, users, memberships et identités externes

- **Statut / effort :** `TODO` / `L`
- **Actions :** posséder le schéma des tables `tenants`, `users`,
  `external_identities` et `tenant_memberships` ; états `invited`, `active`,
  `suspended`, `revoked` et version de membership. MT-DATA-01 possède ensuite
  le provisioning et le backfill legacy.
- **Critères d'acceptation :** un user peut appartenir à plusieurs tenants sans
  partage de profil pédagogique ; révocation immédiate par version ; aucune
  fusion automatique sur le seul email.

### MT-IAM-02 — Émettre des tokens tenant-aware et rotatifs

- **Statut / effort :** `TODO` / `L`
- **Actions :** sélection explicite du tenant, claims `tid`, `membership_id`,
  `roles`, `scope`, `azp`, `jti`, `token_version`; clés asymétriques rotatives
  avec `kid`/JWKS ou IdP externe.
- **Critères d'acceptation :** un token ne vaut que pour un tenant ; rotation
  sans interruption ; audience/issuer/scope/tenant tous vérifiés.
- **Tests :** confusion d'issuer, mauvais tenant, rôle retiré et clé tournée.

### MT-IAM-03 — Centraliser RBAC et portée cohorte

- **Statut / effort :** `TODO` / `L`
- **Rôles initiaux :** owner, admin, responsable pédagogique, formateur,
  auditeur, billing admin, apprenant.
- **Actions :** permissions par action (`formation:write`, `cohort:manage`,
  `progress:read`, `learning:self`, `billing:manage`) et contraintes objet ; un
  formateur est limité à ses cohortes.
- **Critères d'acceptation :** deny-by-default ; audit de chaque action
  privilégiée ; tests matrice rôle × action × ressource.

### MT-IAM-04 — Invitations, MFA, SSO et provisioning

- **Statut / effort :** `TODO` / `XL`, livrable par incréments
- **Actions :** invitations tenant, email vérifié et MFA pour owners/admins ;
  OIDC/SAML ensuite, puis SCIM pour clients enterprise ; service accounts et
  accès support break-glass audité.
- **Critères d'acceptation :** révocation fournisseur répercutée ; MFA exigée
  selon politique tenant ; impersonation limitée, motivée et journalisée.

### MT-IAM-05 — Scoper clients OAuth et service accounts au tenant

- **Statut / effort :** `TODO` / `M`
- **Actions :** distinguer l'identité globale ou d'installation des clients
  partagés Claude/ChatGPT de l'autorité tenant ; scoper grants, consentements,
  authorization codes et refresh tokens au tenant/membership/resource. Les
  clients enterprise dédiés et service accounts peuvent être tenant-scoped ;
  appliquer scopes et quotas propres au tenant.
- **Critères d'acceptation :** un client d'A ne peut demander de token ou de
  consentement pour B ; rotation/révocation immédiates ; toutes les créations
  et délégations sont auditées.

## Schéma, backfill et RLS

### MT-DATA-01 — Provisionner le tenant legacy et backfiller les identités

- **Statut / effort :** `TODO` / `M`
- **Dépendance :** MT-IAM-01 possède le schéma tenants/users/memberships.
- **Actions :** provisionner un tenant `legacy` stable, puis mapper chaque
  learner courant vers les users/memberships déjà définis.
- **Critères d'acceptation :** comportement mono-tenant inchangé ; migration
  idempotente ; mapping et comptes ambigus exportables pour revue.

### MT-DATA-02 — Ajouter `tenant_id` en mode expand

- **Statut / effort :** `TODO` / `XL`
- **Actions :** colonnes nullable, index tenant-first créés en ligne, triggers ou
  dual-write contrôlé et backfill par clé primaire en lots.
- **Critères d'acceptation :** pas de long verrou bloquant ; progression et
  erreurs mesurées ; reprise depuis checkpoint ; toutes les nouvelles écritures
  portent un tenant.

### MT-DATA-03 — Ajouter les contraintes composites

- **Statut / effort :** `TODO` / `L`
- **Actions :** uniques `(tenant_id,id)`, FK composites `NOT VALID`, correction
  des anomalies puis `VALIDATE CONSTRAINT`; passer ensuite `tenant_id NOT NULL`.
- **Critères d'acceptation :** impossible d'associer session, interaction,
  concept, domaine ou enrollment à un objet d'un autre tenant, même par SQL.
- **Tests :** inserts/updates croisés refusés et migrations sur copie volumineuse.

### MT-DATA-04 — Activer et forcer RLS

- **Statut / effort :** `TODO` / `L`
- **Actions :** politiques `USING`/`WITH CHECK`, `FORCE ROW LEVEL SECURITY`,
  rôle runtime non propriétaire et transaction wrapper exécutant `SET LOCAL`.
- **Critères d'acceptation :** tenant A ne voit/modifie jamais B, y compris via
  requête oubliant le filtre ; aucune fuite de tenant dans le pool après commit,
  rollback, panic ou annulation.
- **Tests :** suite négative A/B pour SELECT/INSERT/UPDATE/DELETE, PgBouncer et
  réutilisation de connexion sous `-race`.

### MT-DATA-05 — Séparer rôles runtime, worker et migration

- **Statut / effort :** `TODO` / `M`
- **Actions :** credentials et privilèges minimaux ; le migrateur ne tourne
  plus automatiquement dans chaque instance API ; accès cross-tenant worker
  explicite et audité.
- **Critères d'acceptation :** le rôle API ne peut ni désactiver RLS ni modifier
  le schéma ; secrets séparés ; rotation testée.

## Formation partagée et progression par inscription

### MT-LRN-01 — Modéliser formations et versions immuables

- **Statut / effort :** `TODO` / `XL`
- **Actions :** `formations`, `formation_versions`, modules, concepts et
  prérequis normalisés ; JSONB uniquement pour métadonnées flexibles.
- **Critères d'acceptation :** draft éditable, publication atomique, version
  publiée immuable et identité stable des concepts.

### MT-LRN-02 — Créer cohortes et affectations formateurs

- **Statut / effort :** `TODO` / `L`
- **Actions :** `cohorts/course_runs`, dates, capacité, statut, formateurs et
  version de formation.
- **Critères d'acceptation :** capacité atomique ; formateur limité à ses
  cohortes ; fermeture/archivage sans perdre les preuves.

### MT-LRN-03 — Créer enrollments

- **Statut / effort :** `TODO` / `L`
- **Actions :** inscription d'un user à une cohorte/version, états invited,
  active, completed, suspended, cancelled et objectifs personnalisés séparés.
- **Critères d'acceptation :** unicité tenant/cohorte/user ; version figée ;
  réinscription explicite ; quotas réservés atomiquement.

### MT-LRN-04 — Re-cléer l'état cognitif par enrollment

- **Statut / effort :** `TODO` / `XL`
- **Actions :** migrer concept states, sessions, interactions, évaluations,
  intentions et snapshots vers `(tenant_id,enrollment_id,concept_id)`.
- **Critères d'acceptation :** deux inscriptions du même user ne mélangent
  jamais leurs progressions ; tous les algorithmes conservent leurs résultats
  sur le dataset de référence.
- **Tests :** même concept dans deux formations/versions et deux tenants.

### MT-LRN-05 — API de catalogue et administration pédagogique

- **Statut / effort :** `TODO` / `XL`
- **Actions :** endpoints/portail pour drafts, publication, cohortes,
  inscriptions, formateurs, exports et reporting agrégé.
- **Critères d'acceptation :** autorisation objet systématique, pagination,
  audit et idempotence des mutations.

## Runtime horizontal et asynchrone

### MT-RUN-01 — Séparer bootstrap API, worker et migrateur

- **Statut / effort :** `TODO` / `L`
- **Actions :** packages de composition réutilisables et binaires distincts ;
  API/MCP sans cron fan-out ni migration DDL au démarrage.
- **Critères d'acceptation :** API remplaçable horizontalement ; worker drainable ;
  déploiement API indépendant des migrations compatibles.

### MT-RUN-02 — Déployer MCP stateless

- **Statut / effort :** `TODO` / `M`
- **Dépendance :** étend P1-MCP-01 avec le scope tenant-aware.
- **Actions :** migration SDK/protocole, identité vérifiée à chaque requête et
  aucun état métier conservé dans la session transport.
- **Critères d'acceptation :** round-robin sans sticky session ; perte d'un nœud
  transparente ; clients legacy explicitement testés ou dépréciés.

### MT-RUN-03 — Introduire outbox, jobs et workers

- **Statut / effort :** `TODO` / `XL`
- **Dépendance :** étend P1-JOB-01 à 02 et P1-WEBHOOK-01 à 02 avec des clés et
  quotas tenant-aware ; validation PostgreSQL imposée par P1-TEST-01.
- **Actions :** outbox transactionnelle, relay, broker/queue, leases expirants,
  heartbeat, retries, DLQ et consommateurs idempotents.
- **Critères d'acceptation :** mutation + événement atomiques ; crash à chaque
  frontière sans perte ; lag et DLQ observables ; ordre défini par agrégat si requis.

### MT-RUN-04 — Déplacer la mémoire narrative en object storage

- **Statut / effort :** `TODO` / `L`
- **Dépendance :** étend P1-MEM-02 avec les clés tenant/enrollment.
- **Actions :** clés tenant/enrollment, version, ETag, checksum, chiffrement,
  lifecycle et index de métadonnées en DB.
- **Critères d'acceptation :** lecture cohérente multi-nœud, concurrence
  détectée, restauration et effacement ciblé d'un tenant.

### MT-RUN-05 — Limites instantanées et cache partagé

- **Statut / effort :** `TODO` / `L`
- **Actions :** Redis ou service équivalent pour rate limits par
  `(tenant,user,IP,outil)`, nonce/CSRF partagés et caches avec clés tenant-aware.
- **Critères d'acceptation :** vue fleet-wide, TTL systématique, stratégie de
  dégradation testée et aucune collision inter-tenant.

### MT-RUN-06 — Intégrations et webhooks propres au tenant

- **Statut / effort :** `TODO` / `L`
- **Dépendance :** étend P1-WEBHOOK-01 à 02 et P1-SEC-01. Contrairement aux
  notifications Discord directes, ces endpoints SaaS contrôlés peuvent définir
  un contrat de signature et de déduplication.
- **Actions :** `tenant_integrations`, endpoints autorisés, types d'événement,
  secret chiffré/versionné, signature HMAC avec timestamp et `event_id`,
  allowlist egress, retry et DLQ.
- **Critères d'acceptation :** aucune intégration cross-tenant ; rotation du
  secret sans perte ; contrat at-least-once et déduplication documentés ;
  historique de livraison consultable par les rôles autorisés.

## Plan de contrôle, quotas et facturation

### MT-CTL-01 — Plans, entitlements et quotas durables

- **Statut / effort :** `TODO` / `L`
- **Dimensions :** apprenants actifs, enrollments, formations publiées,
  cohortes, sessions simultanées, appels MCP, stockage, notifications et exports.
- **Critères d'acceptation :** réservation atomique ; dépassement explicite ;
  période de grâce ; aucun noisy neighbour non borné.

### MT-CTL-02 — Événements d'usage idempotents

- **Statut / effort :** `TODO` / `L`
- **Actions :** `usage_events` append-only, clé serveur, rollups et job de
  réconciliation.
- **Critères d'acceptation :** retry MCP facturé une seule fois ; rollup
  reproductible ; correction sans réécriture destructive de la source.

### MT-CTL-03 — Abonnement et fournisseur de paiement

- **Statut / effort :** `TODO` / `L`
- **Actions :** subscriptions, webhooks fournisseur signés/dédupliqués,
  périodes d'essai/grâce et portail billing.
- **Critères d'acceptation :** panne fournisseur sans coupure immédiate d'une
  session pédagogique ; événements rejouables ; droits réconciliés.

### MT-CTL-04 — Journal d'audit privilégié

- **Statut / effort :** `TODO` / `M`
- **Actions :** événement append-only avec tenant, acteur, membership, action,
  cible, résultat, raison, trace et horodatage ; stockage distinct des preuves
  pédagogiques.
- **Critères d'acceptation :** toutes les mutations admin/support/billing et
  impersonations couvertes ; export filtrable et rétention contrôlée.

### MT-CTL-05 — Plan de contrôle et routage tenant

- **Statut / effort :** `TODO` / `XL`
- **Actions :** service/module tenant, statut, région/cellule, plan, flags,
  domaine personnalisé et provisioning.
- **Critères d'acceptation :** création/suspension/réactivation idempotentes ;
  cache invalidable ; aucune dépendance du data plane à une lecture globale
  synchrone sur chaque appel.

## Observabilité, conformité et industrialisation

### MT-OPS-01 — OpenTelemetry et SLO

- **Statut / effort :** `TODO` / `L`
- **Actions :** traces, métriques et logs corrélés avec request/trace ID, tenant
  pseudonymisé, membership, outil, version de formation et latence DB/queue.
- **Critères d'acceptation :** SLO API/MCP et workers ; alertes saturation pool,
  p95/p99, queue lag, jobs abandonnés, DLQ et erreurs RLS.

### MT-OPS-02 — Liveness, readiness et déploiements sûrs

- **Statut / effort :** `TODO` / `M`
- **Actions :** `/live`, `/ready`, compatibilité de migration, feature flags,
  canary tenant/cellule et rollback.
- **Critères d'acceptation :** une instance incompatible ne reçoit pas de trafic ;
  rollout N/N-1 documenté ; canary isolable.

### MT-OPS-03 — RGPD et politiques par tenant

- **Statut / effort :** `TODO` / `XL`
- **Actions :** export, rectification, effacement, legal hold, rétention par type,
  clés de chiffrement et traitement des backups/object versions.
- **Critères d'acceptation :** DSAR traçable ; effacement en lots reprenable ;
  restauration ne réactive pas silencieusement une donnée supprimée.

### MT-OPS-04 — Tests de charge et noisy neighbours

- **Statut / effort :** `TODO` / `L`
- **Scénarios :** beaucoup de petits tenants, un gros tenant, cohortes massives,
  fan-out de notifications, imports et exports concurrents.
- **Critères d'acceptation :** budgets p95/p99, pool et queue documentés ; quotas
  protègent les autres tenants ; test reproductible en staging.

### MT-OPS-05 — Partitionnement des tables volumineuses

- **Statut / effort :** `TODO` / `L`, après mesures
- **Actions :** interactions, audit, usage et snapshots partitionnés d'abord par
  temps, puis éventuellement par cellule/hash tenant.
- **Critères d'acceptation :** pruning vérifié, maintenance/reindex bornés et
  aucune explosion du nombre de partitions par tenant.

### MT-OPS-06 — Analytics hors chemin transactionnel

- **Statut / effort :** `TODO` / `XL`
- **Actions :** rollups, CDC ou export vers entrepôt ; modèles agrégés par
  formation/cohorte sans scans OLTP.
- **Critères d'acceptation :** dashboards sans requêtes globales sur les tables
  transactionnelles ; fraîcheur et réconciliation mesurées.

### MT-OPS-07 — Architecture cellulaire

- **Statut / effort :** `TODO` / `XL`, hors MVP SaaS
- **Actions :** router un tenant vers une cellule régionale ; capacité, migration
  de cellule, cellule dédiée et control plane global minimal.
- **Critères d'acceptation :** aucune requête métier cross-cell ; déplacement
  tenant vérifié ; blast radius et résidence des données documentés.

### MT-OPS-08 — Sauvegardes, PITR et restauration logique d'un tenant

- **Statut / effort :** `TODO` / `L`, requis pour le MVP SaaS
- **Actions :** sauvegardes chiffrées, PITR PostgreSQL, inventaire object storage,
  RPO/RTO, restauration complète et procédure d'extraction/restauration logique
  d'un seul tenant sans exposer les autres.
- **Critères d'acceptation :** exercice automatisé en environnement isolé ; RPO
  et RTO mesurés ; clés restaurables séparément ; restauration tenant vérifiée
  par checksums et tests d'isolation ; runbook approuvé.

## Migration sans interruption

### MT-MIG-01 — Backfill tenant expand/contract

- **Statut / effort :** `TODO` / `XL`
- **Séquence :**
  1. créer tenant `legacy` et tables racines ;
  2. ajouter `tenant_id` nullable et index en ligne ;
  3. écrire tenant_id sur toutes les nouvelles mutations ;
  4. backfiller par lots avec checkpoint et checksum ;
  5. dual-read et comparer les résultats ;
  6. ajouter/valider FK composites ;
  7. passer `NOT NULL`, activer puis forcer RLS en canary ;
  8. retirer les lectures legacy après au moins deux versions compatibles.
- **Rollback :** désactiver le nouveau chemin par flag sans effacer les colonnes
  ni les données backfillées.

### MT-MIG-02 — Convertir domains en formations et enrollments

- **Statut / effort :** `TODO` / `XL`
- **Règle :** chaque domain existant devient initialement une formation distincte
  avec une version 1 et un enrollment. Ne jamais dédupliquer automatiquement
  par nom ou JSON.
- **Séquence :** snapshot, import, dual-write, comparaison des algorithmes,
  bascule par learner puis suppression différée du modèle ancien.
- **Critères d'acceptation :** aucune progression perdue ; preuve de mapping ;
  concepts ambigus mis en quarantaine ; replay pédagogique identique.

### MT-MIG-03 — Backfills et DDL réellement online

- **Statut / effort :** `TODO` / `L`
- **Actions :** séparer migrations transactionnelles et DDL online, supporter
  `CREATE INDEX CONCURRENTLY`, budgets de lock, pause/reprise et observabilité.
- **Critères d'acceptation :** migration sur copie de production sans dépassement
  du budget d'indisponibilité ; rollback/roll-forward répétés.

## Matrice de validation obligatoire

| Domaine | Tests minimaux |
|---|---|
| Isolation | A/B pour chaque Store/API/tool et chaque opération CRUD, avec et sans filtre applicatif |
| Pool/RLS | commit, rollback, panic, timeout, cancellation et réutilisation de connexion |
| Identité | multi-membership, révocation, suspension, rotation de clé, SSO et impersonation |
| Curriculum | publication immuable, même concept dans plusieurs versions et migration legacy |
| Progression | même user dans plusieurs enrollments/tenants sans contamination |
| Jobs/outbox | crash à chaque frontière, lease expiré, replay, poison job et DLQ |
| Quotas/billing | concurrence, retry idempotent, période de grâce et réconciliation |
| Object storage | ETag conflict, objet corrompu, versioning, lifecycle et restauration |
| Performance | beaucoup de petits tenants, gros tenant, noisy neighbour et imports concurrents |
| Continuité | PITR, restauration logique d'un tenant, perte d'un nœud et migration de cellule |

## Définition de « SaaS MVP prêt »

Le SaaS MVP est livrable lorsque M0 à M5 sont `DONE` et que :

- tous les P1 de [`backlog-p1-p2.md`](./backlog-p1-p2.md) sont `DONE` ;
- RLS forcée et FK composites ont passé la suite négative A/B ;
- formations, versions, cohortes et enrollments sont la source de vérité ;
- l'API/MCP est stateless et les traitements externes passent par jobs/outbox ;
- secrets, mémoire narrative et rate limits ne sont plus locaux au processus ;
- quotas, usage, audit et politiques de rétention sont opérationnels ;
- les SLO, runbooks, PITR et restauration d'un tenant ont été exercés ;
- aucune migration destructive n'est nécessaire pour revenir à la version N-1.

Le partitionnement avancé, l'entrepôt analytique et les cellules dédiées peuvent
rester après le MVP tant que les mesures de capacité ne les rendent pas
nécessaires.
