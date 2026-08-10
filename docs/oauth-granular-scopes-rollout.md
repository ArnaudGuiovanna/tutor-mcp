# Déploiement des scopes OAuth granulaires

Ce runbook encadre le passage du scope historique borné `learner` aux scopes
par capacité `learner:read` et `learner:write`. Le changement est protégé par
`OAUTH_GRANULAR_SCOPES`, désactivé par défaut. Une valeur autre que `on` ou
`off` fait échouer le démarrage ; la casse et les espaces extérieurs sont
normalisés.

## Contrat du flag

- `off` conserve le mode de compatibilité : les nouvelles autorisations sont
  publiées et émises avec le bundle historique `learner`.
- `on` publie et émet les scopes granulaires attendus par chaque outil MCP.
  Les credentials `learner` déjà émis restent un bundle borné aux capacités
  lecture et écriture connues ; il ne devient jamais un wildcard pour de
  futurs scopes.
- Le flag ne retire aucune colonne et n'annule aucune migration. Il contrôle
  le comportement OAuth/MCP, pas le schéma.
- Tous les processus servant le même issuer `BASE_URL` doivent exposer la même
  valeur. Des métadonnées OAuth différentes derrière un même load balancer
  rendent la négociation client non déterministe.

## Préconditions

1. Sauvegarder la base et vérifier une restauration conformément à
   [`OPERATIONS.md`](../OPERATIONS.md). Conserver le point de restauration
   jusqu'à la fin de la fenêtre d'observation.
2. Vérifier que le binaire cible accepte les quatre représentations persistées
   canoniques : `learner`, `learner:read`, `learner:write` et
   `learner:read learner:write`.
3. Recenser les clients MCP critiques et préparer un scénario pour chacun :
   découverte, autorisation PKCE, lecture, mutation refusée avec un token
   lecture, refresh conservé/réduit et reconnexion après révocation.
4. Définir la fenêtre d'observation, le responsable de décision et les seuils
   de rollback sur les erreurs `/authorize`, `/token`, `invalid_scope`,
   `invalid_grant` et les refus d'outils pour scope insuffisant.

## Phase 1 — expansion compatible, flag OFF

1. Déployer le nouveau binaire avec `OAUTH_GRANULAR_SCOPES=off` sur tous les
   nœuds. Les migrations additives sont `0035_oauth_tool_scopes` pour SQLite
   et `postgres_0026_oauth_tool_scopes` pour PostgreSQL. Leur valeur par défaut
   `learner` permet aux processus N-1 qui omettent les nouvelles colonnes de
   continuer à écrire le bundle historique pendant le rolling deploy.
2. Attendre que tous les nœuds exécutent la version compatible avant de passer
   à la phase 2. Ne pas activer le flag sur un seul nœud d'un issuer partagé.
3. Vérifier la présence de la migration dans `schema_migrations`, puis examiner
   la distribution des valeurs sans modifier les données :

   ```sql
   SELECT version, applied_at
   FROM schema_migrations
   WHERE version IN ('0035_oauth_tool_scopes', 'postgres_0026_oauth_tool_scopes');

   SELECT scope, COUNT(*) FROM oauth_codes GROUP BY scope;
   SELECT scope, COUNT(*) FROM refresh_tokens GROUP BY scope;
   SELECT scope, COUNT(*) FROM learner_approved_clients GROUP BY scope;
   SELECT scope, COUNT(*) FROM account_tokens
   WHERE purpose = 'email_verification'
   GROUP BY scope;
   ```

4. En mode `off`, valider un parcours OAuth historique complet, y compris la
   rotation d'un refresh token, sur chaque type de client critique.

Le gate de phase 1 est atteint lorsque tous les nœuds sont compatibles, que les
migrations sont appliquées, qu'aucun scope hors vocabulaire canonique n'est
observé et que le taux d'erreur reste au niveau de référence.

## Phase 2 — activation contrôlée

1. Activer `OAUTH_GRANULAR_SCOPES=on` dans un pool canary isolé de l'issuer de
   production, ou basculer atomiquement tout le pool si cette isolation n'est
   pas possible. Un simple équilibrage aléatoire entre nœuds `on` et `off`
   n'est pas un canary valide.
2. Vérifier les métadonnées
   `/.well-known/oauth-authorization-server` et
   `/.well-known/oauth-protected-resource` : elles doivent publier les scopes
   granulaires sans demander au client de combiner l'alias historique
   `learner` avec eux.
3. Vérifier la découverte MCP : chaque outil annonce le scope correspondant à
   son effet réel. Avec un access token `learner:read`, les lectures réussissent
   et toute mutation est refusée avant son handler. Tester aussi
   `learner:write` et le couple `learner:read learner:write`.
4. Vérifier qu'un refresh sans paramètre `scope` conserve son grant, qu'une
   réduction est acceptée et qu'un élargissement retourne `invalid_scope` sans
   consommer le refresh token valide.
5. Vérifier qu'un ancien token `learner` fonctionne toujours comme le bundle
   borné lecture/écriture, puis surveiller les indicateurs définis en
   préconditions pendant toute la fenêtre d'observation.
6. Étendre progressivement le trafic canary, puis activer le flag sur tous les
   nœuds du même issuer en une seule opération de configuration.

## Rollback vers OFF

Le rollback du comportement ne supprime pas les données granulaires. Avant de
repasser l'issuer en mode `off`, arrêter les nouvelles autorisations, faire une
sauvegarde et compter les credentials granulaires. La révocation ci-dessous
est intentionnelle : les clients concernés devront se reconnecter.

Exécuter la procédure dans une transaction et vérifier les nombres de lignes
avant `COMMIT` :

```sql
BEGIN;

DELETE FROM oauth_codes
WHERE scope IN ('learner:read', 'learner:write', 'learner:read learner:write');

UPDATE refresh_tokens
SET revoked_at = COALESCE(revoked_at, CURRENT_TIMESTAMP)
WHERE family_id IN (
    SELECT family_id
    FROM refresh_tokens
    WHERE scope IN ('learner:read', 'learner:write', 'learner:read learner:write')
);

DELETE FROM learner_approved_clients
WHERE scope IN ('learner:read', 'learner:write', 'learner:read learner:write');

UPDATE account_tokens
SET consumed_at = COALESCE(consumed_at, CURRENT_TIMESTAMP)
WHERE purpose = 'email_verification'
  AND scope IN ('learner:read', 'learner:write', 'learner:read learner:write');

COMMIT;
```

Basculer ensuite tous les nœuds de l'issuer vers
`OAUTH_GRANULAR_SCOPES=off`. Les access tokens JWT déjà émis ne sont pas lus en
base et restent donc valides jusqu'à leur expiration, au maximum 30 minutes.
Attendre ce TTL avant de considérer le rollback de compatibilité terminé, puis
faire rejouer le parcours OAuth historique.

Ne pas redéployer un ancien binaire simplement parce que le flag est repassé à
`off`. Le rollback normal conserve le binaire compatible et le schéma étendu.
Un retour en N-1 exige sa propre preuve de compatibilité avec le schéma et les
données actuels.

## Révocation d'urgence

Si le motif est une erreur d'autorisation ou une suspicion de fuite, ne pas se
limiter aux credentials granulaires : révoquer toutes les familles de refresh
tokens, supprimer tous les codes et consentements actifs, et consommer les
liens de vérification OAuth en attente. Un changement coordonné de
`JWT_SECRET` sur tous les nœuds invalide immédiatement les access tokens, mais
déconnecte tous les utilisateurs ; l'utiliser seulement comme mesure
d'incident explicitement approuvée. Sans rotation de cette clé, maintenir le
service fermé ou restreint pendant le TTL maximal de 30 minutes.

Après tout rollback ou incident, conserver les comptages avant/après, la
chronologie, la cause, les clients affectés et la décision de réactivation dans
le journal d'exploitation.
