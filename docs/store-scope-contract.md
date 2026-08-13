# Contrat de scope du Store

Toute nouvelle méthode métier exportée sur `db.Store` doit recevoir un
`models.TenantScope` ou un `models.Principal`. Le test
`TestNoNewUnscopedStoreMethods` fige par SHA-256 l'ensemble des méthodes
legacy encore exposées sans ce type : ajouter ou renommer une méthode non
scopée casse le gate de CI.

Les exceptions gelées relèvent de quatre catégories : API pédagogique legacy
encore maintenue pendant le dual-read/write, authentification/control plane
global, migrations/santé, et accès de test (`RawDB`). Elles ne constituent pas
un modèle pour du nouveau code. `VerifySchemaCurrent` est l'exception santé
ajoutée le 2026-08-12 : elle ne lit que le ledger global de migrations pour
`/ready` et ne touche aucune donnée métier. Les méthodes SaaS, catalogue, RBAC, quotas,
usage, jobs et outbox utilisent toutes une frontière tenant typée.

Lorsqu'un credential doit nécessairement être résolu avant que le tenant soit
connu, la méthode reçoit une capability typée qui ne contient aucun sélecteur
tenant libre (`BillingWebhookCredential`, `ServiceAccountCredential`,
`SupportAccessCredential` ou assertion fédérée déjà vérifiée). La table de
routage globale ne conserve qu'un digest et fournit le tenant autoritatif ; la
lecture métier suivante se déroule immédiatement sous `SET LOCAL` et RLS.

Le remplacement progressif se fait sans modifier le hash à la hausse : une
méthode retirée peut faire évoluer l'empreinte après revue ; une nouvelle
exception exige une justification explicite dans ce document. Les requêtes
tenant-owned restent en plus protégées par transaction `SET LOCAL`, FK
composites et `FORCE ROW LEVEL SECURITY` sur PostgreSQL.
