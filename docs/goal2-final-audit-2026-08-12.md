# Audit final du Goal 2 — 2026-08-12

Cet audit clôt les jalons M0 à M3 : contrat tenant, identité/RBAC, isolation
PostgreSQL et progression canonique par enrollment. Le schéma final de cet
audit est SQLite `0063` / PostgreSQL `postgres_0054`.

## Audit exigence par exigence

| Exigence | Preuve durable | Verdict |
|---|---|---|
| Toute frontière nouvelle porte un tenant non ambigu | `models.Principal`/`TenantScope`, contexte auth typé et `TestPrincipalValidateRequiresUnambiguousTenantIdentity`. Le garde AST `TestNoNewUnscopedStoreMethods` fige les méthodes legacy ; seule la readiness globale en lecture du ledger est ajoutée et documentée. | `PASS` |
| Un user multi-memberships ne mélange ni profil ni token | `users`, `tenants`, `tenant_memberships`, claims `tid`/membership/rôles/version et binding de session. Les tests JWT/middleware couvrent token tenantless, ancien, membership suspendu et deux memberships. | `PASS` |
| Invitations, MFA, fédération et accès machine/support sont révocables | Invitations single-use, TOTP anti-replay requis pour owner/admin, provider OIDC/SAML/SCIM derrière une assertion vérifiée sans fusion par email, service accounts tenant-scoped et support break-glass read-only ≤ 1 h. Révocation/version invalident immédiatement le principal. | `PASS` |
| RBAC est deny-by-default et borné à la ressource | La matrice centralisée couvre owner/admin/pédagogie/formateur/auditeur/billing/apprenant ; un formateur doit être affecté à la cohorte et les actions cross-tenant sont refusées. | `PASS` |
| Le legacy est backfillé sans association silencieuse | `tenant_legacy`, mappings user/membership/domain→enrollment et concepts sont idempotents. `TestLegacyEnrollmentMigrationBackfillsAndQuarantinesWithoutGuessing` et les migrations de sessions/domaines exportent les ambiguïtés vers la quarantaine. | `PASS` |
| FK tenant composites empêchent les graphes croisés | Sessions, preuves, catalogue, enrollment et état cognitif portent tenant/enrollment/concept. Les FK de `learner_concept_states` imposent la même version de formation ; les tentatives d'association tenant/version étrangères échouent. | `PASS` |
| PostgreSQL force RLS même si un filtre applicatif manque | Toutes les tables tenant-owned activent et forcent RLS. `WithTenantTx` pose `SET LOCAL` ; `TestPostgresForcedRLSAllOperationsAndPoolReset` couvre SELECT/INSERT/UPDATE/DELETE, commit, rollback, panic, annulation et réutilisation du pool. | `PASS` |
| Les rôles runtime ne neutralisent pas RLS | `tutor_api`/`tutor_worker` sont non propriétaires, non superuser, sans `BYPASSRLS` ni `CREATE`; le migrateur est distinct. Le gate de démarrage production inspecte effectivement ces privilèges. L'acceptance SQL réelle a aussi prouvé que seuls les credentials de restore peuvent régler `session_replication_role`. | `PASS` |
| Formation publiée, cohorte et enrollment sont canoniques | Publication atomique et immuable, concepts/prérequis versionnés, capacité réservée atomiquement, formateurs bornés, mutations admin idempotentes et listes keyset. Le pont legacy crée une formation/version/cohorte/enrollment distincte par domaine, jamais par nom. | `PASS` |
| Deux enrollments ne contaminent pas leur progression | La clé canonique est `(tenant_id,enrollment_id,formation_concept_id)`. `TestEnrollmentConceptStateNeverMixesTwoEnrollments` couvre deux formations ; `TestNarrativeStoreCanonicalEnrollmentKeyIsolatesSameLearner` couvre le même learner/la même clé narrative dans deux enrollments et refuse un transplant de ciphertext par AAD. | `PASS` |

## Migration et rollback

La séquence est additive : tables racines, colonnes/mappings, backfill
déterministe, contraintes composites, `NOT NULL`/RLS, puis source canonique.
Les objets legacy restent en dual-write pour N-1 ; aucun rollback n'efface les
colonnes ou mappings. Une anomalie se résout explicitement dans
`tenant_migration_quarantine`, puis le batch est rejoué. Les checksums des
migrations appliquées sont immuables et toute dérive bloque API, worker et
migrateur.

## Matrice finale

Les gates finaux et leurs durées sont consignés avec les Goals 3–4 dans
[`goal4-final-audit-2026-08-12.md`](./goal4-final-audit-2026-08-12.md). Les
tests PostgreSQL ciblés RLS, narrative canonique, gouvernance et restauration
logique sont verts sur PostgreSQL 16 local.

## Décision

Goal 2 : `DONE`. Aucun waiver d'isolation n'est ouvert. Les optimisations DDL
online avancées de MT-MIG-03 restent explicitement M6 post-MVP ; elles ne sont
pas requises pour le rollout additif courant.
