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
1 000 créations. Le quota est vérifié avant toute déduplication : un IAT épuisé
refuse aussi une requête équivalente à un enregistrement existant. Une requête
publique dédupliquée ne consomme pas d'unité supplémentaire.

## Limites par IP

Indépendamment des quotas par IAT, `POST /register` est borné par adresse IP
cliente :

- une limite d'admission fixe de 30 requêtes par minute, locale au processus,
  appliquée avant la vérification de l'IAT ;
- une limite par minute et un budget quotidien glissant,
  `OAUTH_DCR_MAX_REGISTRATIONS_PER_IP_PER_DAY` (défaut `500`, plage
  1–1 000 000), appliqués après l'admission. Avec `RATELIMIT_BACKEND=postgres`,
  ces deux compteurs sont partagés entre instances.

Un dépassement reçoit `429`. Le défaut convient aux connecteurs hébergés qui
s'enregistrent depuis un petit nombre d'adresses de sortie ; l'augmenter si
un fournisseur légitime atteint le budget, le réduire pour un déploiement
dont les clients sont connus.

## Client ID Metadata Documents

`OAUTH_CIMD_MODE` contrôle la résolution des `client_id` de type URL HTTPS
(CIMD), indépendamment de `OAUTH_DCR_MODE` :

- `on` (défaut) résout les documents servis par n'importe quel hôte public ;
- `off` désactive CIMD ; la discovery publie
  `client_id_metadata_document_supported: false` ;
- une liste de noms d'hôtes DNS séparés par des virgules (par exemple
  `claude.ai,app.example.com`) ne résout que les `client_id` dont l'hôte
  correspond exactement à l'une des entrées. Schéma, port, chemin, joker et
  adresse IP sont refusés au démarrage.

Les clients déjà enregistrés en base ne sont pas concernés par ce réglage.

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

Un client nouvellement enregistré reste provisoire pendant 30 jours. Le premier
échange de code réussi met `oauth_clients.expires_at` à `NULL` dans la même
transaction que la consommation du code et la création du refresh token. La
mutation est idempotente et accepte les clients CIMD non persistés. Le cleanup
des clients jamais utilisés reste exclusivement dans le job horaire du
scheduler ; `/register` ne le déclenche plus. La suppression et son audit sont
dans la même transaction.

Le nombre total de clients enregistrés est plafonné (10 000). Lorsque le
plafond est atteint, un nouvel enregistrement évince d'abord le plus ancien
client provisoire jamais activé : aucun échange de code réussi, aucun refresh
token, aucun compte de service, et enregistré depuis plus d'une heure. Ses
codes en attente et approbations sont supprimés avec lui, dans la même
transaction, et le journal enregistre `client_evicted_deleted`. Si aucun client
n'est éligible, l'enregistrement est refusé. Les clients activés ne sont
jamais évincés.

## Ré-enregistrement

Les clients publics (sans secret) sont dédupliqués : leurs métadonnées
effectives sont canonisées et indexées par empreinte. Deux requêtes
équivalentes, y compris concurrentes ou avec un ordre différent des URI,
obtiennent le même `client_id` ; la première reçoit `201`, les suivantes `200`
avec ce même `client_id`, sans consommer de quota supplémentaire. Si le client
équivalent est provisoire et expiré, la requête est refusée jusqu'à son
nettoyage par le job horaire.

Les clients confidentiels ne sont pas dédupliqués. Chaque enregistrement
confidentiel admis crée un client distinct et reçoit `201` avec un nouveau
`client_id` et un nouveau `client_secret`, soumis au quota de l'IAT et au
plafond global. Le secret n'est renvoyé qu'une seule fois, à la création ;
seul son hachage bcrypt est conservé. Un client confidentiel qui a perdu son
secret doit se ré-enregistrer et utiliser le nouveau couple d'identifiants.
Les enregistrements antérieurs inutilisés expirent selon la règle des clients
provisoires ci-dessus.

## Formulaires et codes d'autorisation

Les formulaires servis par le serveur (consentement `/authorize` et
formulaires de compte) vérifient l'origine de la soumission en plus du jeton
CSRF. Quand le navigateur envoie `Sec-Fetch-Site`, seules les valeurs
`same-origin` et `none` sont acceptées ; sinon un en-tête `Origin` présent doit
correspondre à `BASE_URL` (schéma et hôte). Une requête sans ces en-têtes, ou
avec `Origin: null`, reste soumise au jeton CSRF. Derrière un reverse proxy,
`BASE_URL` doit donc être l'origine publique exacte vue par le navigateur.

Les codes d'autorisation ne sont stockés que sous forme hachée. Le format de la
colonne est inchangé et aucune migration n'est nécessaire. Conséquence
opérationnelle lors de la mise à jour : un code émis par une version antérieure
et non encore échangé devient invalide. Les codes expirant après 5 minutes,
l'effet est limité aux flux d'autorisation en cours pendant le déploiement ;
le client recommence simplement l'autorisation.

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
auditée n'est jamais annulée automatiquement. L'authentification des clients
confidentiels repose uniquement sur le hachage de leur secret et ne dépend pas
du keyring. Le plafond global, les limites IP et le budget bcrypt restent des
défenses complémentaires aux quotas durables par IAT.
