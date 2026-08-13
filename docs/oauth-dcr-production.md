# Dynamic Client Registration en production

Le serveur expose trois politiques strictes avec `OAUTH_DCR_MODE` :

- `open` conserve la compatibilité de développement. Toute requête qui passe
  les limites IP et la validation des métadonnées peut enregistrer un client.
  Ce mode est refusé au démarrage avec `DEPLOYMENT_PROFILE=production`.
- `token` publie `/register`, mais exige un Initial Access Token dans
  `Authorization: Bearer <token>`. L'admission précède le rate limiter partagé,
  la lecture du corps, le cleanup, bcrypt et toute écriture métier.
- `disabled` omet `registration_endpoint` de la discovery et ne monte pas la
  route `/register`. Le handler refuse également un montage direct.

En production, la variable doit être explicitement `token` ou `disabled`.

## Configurer le mode token

Générer une capacité aléatoire d'au moins 256 bits :

```sh
openssl rand -hex 32
```

Configurer sa valeur comme `OAUTH_DCR_INITIAL_ACCESS_TOKEN`. La syntaxe
acceptée est un base64url sans padding qui décode au moins 32 octets ; une
chaîne hexadécimale de 64 caractères satisfait cette contrainte. Le processus
valide la valeur, en calcule SHA-256 et inscrit idempotemment cette capacité de
bootstrap dans le registre partagé. Seul le digest est conservé. Un changement
de la valeur ajoute une capacité active au lieu de supprimer l'ancienne : le
déploiement peut donc chevaucher les deux versions, puis révoquer explicitement
l'ancienne. Le token brut ne doit jamais apparaître dans les logs ou fichiers
de configuration versionnés.

Les appels sans token, avec un token incorrect, plusieurs en-têtes
`Authorization` ou une valeur concaténée reçoivent tous le même `401`, le même
corps et le challenge :

```text
WWW-Authenticate: Bearer realm="oauth-dcr", error="invalid_token"
```

Un IAT valide autorise les clients publics PKCE et confidentiels : l'opérateur
qui distribue cette capacité assume explicitement cette autorité. Chaque IAT a
un quota transactionnel ; le token de bootstrap configuré au démarrage reçoit
1 000 créations. Les replays exacts ne consomment pas une seconde unité.

## Administration et rotation

La commande `tutor-dcr-admin` ouvre une base existante sans la créer. Les
actions de lecture et les previews utilisent SQLite en lecture seule ; toute
mutation exige `--apply` et un acteur d'audit explicite. Le secret est généré
par la commande, affiché une seule fois sur stdout et jamais accepté en
argument :

```sh
# Preview sans mutation ni génération de secret
go run ./cmd/tutor-dcr-admin \
  --action=create --label=partner-a --actor=ops@example \
  --max-registrations=100

# Création ; capturer stdout directement dans le gestionnaire de secrets
go run ./cmd/tutor-dcr-admin \
  --action=create --label=partner-a --actor=ops@example \
  --max-registrations=100 --expires-in=720h --apply

# Début d'une rotation : l'ancien token reste actif
go run ./cmd/tutor-dcr-admin \
  --action=rotate --previous-token-id=iat-old --label=partner-a-v2 \
  --actor=ops@example --max-registrations=100 --apply

# Après déploiement et vérification de la nouvelle capacité
go run ./cmd/tutor-dcr-admin \
  --action=revoke --token-id=iat-old --actor=ops@example \
  --reason=rotation-complete --apply

go run ./cmd/tutor-dcr-admin --action=list
go run ./cmd/tutor-dcr-admin --action=audit
```

Avec PostgreSQL, fournir `DB_DRIVER=postgres` et `DATABASE_URL`. Pour faire
tourner le bootstrap de configuration, déployer d'abord le nouvel
`OAUTH_DCR_INITIAL_ACCESS_TOKEN` : chaque nouveau processus l'ajoute au registre
et tous les anciens processus le voient immédiatement. Vérifier les deux
tokens, terminer le rollout, puis révoquer l'ancien. Un processus resté sur la
vieille configuration continue à servir mais refuse aussitôt le token révoqué ;
son prochain redémarrage échoue au lieu de ressusciter la capacité.

Le journal durable distingue `token_created`, `rotation_started`,
`token_revoked`, `client_registered` et `client_expired_deleted`. Une
révocation rejouée est idempotente. Les tokens ne sont jamais supprimés
physiquement, de sorte que leur historique reste réconciliable.

## Durée de vie

Un client nouvellement enregistré reste éphémère pendant 30 jours. Le premier
échange de code réussi met `oauth_clients.expires_at` à `NULL` dans la même
transaction que la consommation du code et la création du refresh token. La
mutation est idempotente et accepte les clients CIMD non persistés. Le cleanup
des clients jamais utilisés reste exclusivement dans le job horaire du
scheduler ; `/register` ne le déclenche plus. La suppression et son audit sont
dans la même transaction.

Les métadonnées effectives sont canonisées et indexées par empreinte. Deux
requêtes équivalentes, y compris concurrentes ou avec un ordre différent des
URI, obtiennent le même `client_id` et une seule consommation de quota. En
production, le secret d'un client confidentiel est conservé uniquement dans
une enveloppe AES-256-GCM liée au client et à l'empreinte, afin qu'un replay
exact rende le même credential sans plaintext dans le dump. Sans keyring en
développement, la première création confidentielle fonctionne mais un replay
est refusé plutôt que de créer un doublon ou exposer un secret irrécupérable.

## Déploiement et rollback

1. Déployer d'abord le binaire avec `OAUTH_DCR_MODE=disabled` si aucun client
   ne doit s'enregistrer, ou `token` avec un IAT de bootstrap géré comme secret.
2. Vérifier que la discovery correspond au mode et qu'un IAT invalide ne
   modifie ni les clients ni les compteurs partagés.
3. Pour couper DCR, basculer tous les nœuds vers `disabled`. Les clients déjà
   activés continuent de fonctionner.

Pour revenir en arrière pendant une rotation, conserver les deux capacités et
remettre l'ancienne valeur de configuration. Après révocation, un rollback
doit utiliser le nouveau token ou créer une nouvelle capacité : une révocation
auditée n'est jamais annulée automatiquement. Restaurer une sauvegarde exige de
restaurer aussi les versions du keyring nécessaires aux secrets confidentiels.
Le cap global, la limite IP et le budget bcrypt restent des défenses
complémentaires aux quotas durables par IAT.
