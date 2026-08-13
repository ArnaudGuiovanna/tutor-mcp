# Exploitation du runtime SaaS

État de référence : 2026-08-12. Le profil SaaS supporté utilise PostgreSQL,
des processus séparés et les rôles SQL de moindre privilège. SQLite reste un
profil local, mono-processus ; il n'est pas une autorité SaaS horizontale.

## Topologie et démarrage

Construire une seule image immuable, puis l'exécuter avec trois identités et
trois DSN distincts :

| Processus | Configuration principale | Responsabilité |
|---|---|---|
| migrateur | `PROCESS_ROLE=migrator` | verrou de migration, DDL et sortie immédiate |
| API/MCP | `PROCESS_ROLE=api` | HTTP/OAuth/MCP stateless, aucune migration ni cron |
| worker | `PROCESS_ROLE=worker`, `SCHEDULER_MODE=distributed` | outbox, jobs, webhooks, DSAR et maintenance tenant |

Le profil production exige aussi `DEPLOYMENT_PROFILE=production`,
`DB_DRIVER=postgres`, `RATELIMIT_BACKEND=postgres`, mémoire narrative
`database`, TLS PostgreSQL `verify-full`, SMTP STARTTLS, keyrings et allowlist
`TENANT_INTEGRATION_ALLOWED_HOSTS`. L'API et le worker refusent de démarrer si
une migration connue manque ou si son checksum diffère. Les entrées additives
N+1 sont ignorées par N : une version précédente reste donc amorçable tant que
la migration n'a supprimé ni renommé son contrat.

Appliquer [`postgres-roles.sql`](../deploy/postgres-roles.sql) avec le
propriétaire après les migrations, puis accorder à chaque login exactement un
groupe `tutor_api`, `tutor_worker` ou `tutor_restore`. Le migrateur conserve un
login propriétaire séparé. En production, le runtime vérifie qu'il n'est ni
superuser, ni `BYPASSRLS`, ni propriétaire d'une table RLS et qu'il ne possède
pas `CREATE` sur le schéma. `tutor_restore` n'est activé que pendant une
restauration approuvée.

## Déploiement N vers N+1

1. Produire une sauvegarde chiffrée et vérifier sa copie hors site.
2. Exécuter N+1 en `PROCESS_ROLE=migrator`, une fois. Ne jamais lancer la DDL
   dans chaque replica API.
3. Démarrer un worker N+1 puis une API N+1 canary. `/live` prouve seulement que
   le processus vit ; `/ready` vérifie DB et compatibilité du ledger.
4. Router un tenant interne par feature flag/domaine vérifié. Observer pendant
   au moins deux fenêtres de job : erreurs, p95/p99, attente du pool, lag et
   DLQ selon [`saas-slo.md`](./saas-slo.md).
5. Étendre progressivement, puis drainer les workers N. Les leases expirants
   et l'idempotence autorisent la reprise par N+1.

Rollback applicatif : retirer N+1 du trafic, arrêter ses workers et relancer N.
Les migrations MVP sont additives et N ignore les lignes de ledger N+1 ; ne
pas supprimer les nouvelles tables/colonnes. Si un nouveau chemin a écrit des
objets que N ne comprend pas, désactiver d'abord son feature flag et laisser
N+1 drainer ses jobs. Un checksum modifié ou une migration destructive est un
incident, pas un rollback automatisable.

## Control plane

Chaque mutation exige un motif et un identifiant de changement. La commande
refuse un schéma incomplet et produit du JSON ; le token de vérification de
domaine n'est affiché qu'une fois.

```bash
DATABASE_URL="$CONTROL_PLANE_DATABASE_URL" go run ./cmd/tutor-control-plane \
  -action=plan-upsert -plan=standard -name='Standard' -status=active \
  -entitlements='{"active_learners":500,"mcp_calls_month":100000}' \
  -reason='catalogue tarifaire approuvé' -request-id=CHG-2026-0812-01

DATABASE_URL="$CONTROL_PLANE_DATABASE_URL" go run ./cmd/tutor-control-plane \
  -action=provision -slug=acme -name='Acme' -region=eu-west -plan=standard \
  -reason='contrat signé' -request-id=CHG-2026-0812-02
```

Les actions disponibles couvrent provisionnement, suspension/réactivation,
flags, début/fin de validation de domaine, plans et affectations de plan. Les
mutations tenant alimentent `audit_events`; les plans globaux alimentent le
journal append-only `platform_audit_events`. Conserver la sortie avec le ticket
de changement et corréler `request_id`/`trace_id`.

## Quotas, usage et billing

Une consommation suit `ReserveEntitlement` puis `FinishEntitlementReservation`.
La réservation atomique protège le quota avant le travail coûteux ; un timeout
est récupéré par expiration. L'achèvement `consumed` crée un événement d'usage
append-only et idempotent. Une correction est un nouvel événement signé d'un
motif, jamais un `UPDATE` de la source. Réconcilier régulièrement rollups,
réservations terminales et événements du fournisseur.

Les statuts `trialing` et `active` autorisent les réservations. `grace` exige
une échéance future et maintient le service jusqu'à celle-ci ; les autres
statuts refusent les nouvelles consommations. Ne pas interrompre une session
déjà réservée sur une indisponibilité du fournisseur. Une baisse de plan est
refusée si `used + reserved` dépasse la nouvelle limite.

## Workers, webhooks et RGPD

Les mutations productrices et l'outbox partagent une transaction. Le relay
crée des jobs idempotents par intégration ; claims, heartbeats, backoff, limite
de tentatives et DLQ sont durables. Une perte de nœud ne requiert pas de sticky
session. Sur arrêt, retirer le worker du scheduler, laisser finir les appels
HTTP bornés à 12 secondes, puis attendre l'expiration des leases restants.

Les webhooks SaaS sont signés sur
`timestamp + "." + event_id + "." + payload` avec HMAC-SHA-256. Le destinataire
doit vérifier `X-Tutor-Timestamp`, `X-Tutor-Event-ID`,
`X-Tutor-Secret-Version` et `X-Tutor-Signature`, rejeter une horloge trop
ancienne et dédupliquer durablement `event_id`. Le contrat est at-least-once.
La rotation garde l'ancienne version entre 5 minutes et 7 jours selon la
fenêtre choisie. Les notifications Discord directes conservent leur procédure
de quarantaine distincte dans
[`webhook-delivery-operations.md`](./webhook-delivery-operations.md).

Une demande DSAR autorisée crée un job `tenant_dsar`. Les exports sont
manifestés et les effacements avancent par phases/lots ; un legal hold place la
demande en `blocked`. Après levée documentée du hold, `ResumeTenantDSAR`
réenfile la même demande. Toute restauration antérieure à un effacement impose
une réconciliation DSAR/rétention avant retour au trafic.

## Télémétrie et diagnostic

Configurer l'export OTLP avec `OTEL_EXPORTER_OTLP_ENDPOINT` ou les endpoints
traces/métriques séparés. Les variables standard `OTEL_RESOURCE_ATTRIBUTES`,
headers et certificats sont lues par les SDK. `ENVIRONMENT`, puis
`DEPLOYMENT_PROFILE`, renseigne l'environnement. Sans endpoint, le runtime
reste fonctionnel avec des instruments no-op.

Les spans HTTP propagent W3C Trace Context/Baggage. Les spans workers et les
spans MCP portent un tenant pseudonymisé ; MCP ajoute membership pseudonymisé,
nom d'outil et résultat. Les métriques couvrent durée/résultat des outils et
workers, lag/transitions de queue et états/attentes du pool SQL. Aucun DSN,
payload, URL secrète, contenu pédagogique ou identifiant tenant brut ne doit
être ajouté. La version de formation reste dans les preuves pédagogiques
tenant-scoped et se joint à une trace par l'enrollment/audit, pas comme label
de métrique à forte cardinalité.

Pour une panne : partir du `request_id` ou du `trace_id` d'audit, vérifier
`/ready`, saturation du pool, lag/DLQ, puis `worker_tenant_runs`. N'ouvrir un
accès support qu'avec le grant break-glass en lecture seule, motif/request ID
et TTL maximal d'une heure.
