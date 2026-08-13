# Sauvegarde, PITR et restauration logique tenant

Ce runbook couvre le profil PostgreSQL SaaS. Toutes les commandes de
restauration ciblent une base isolée et exigent un changement approuvé. Une
sauvegarde ou archive tenant contient des données personnelles, des hashes de
credentials et des secrets déjà chiffrés : elle reste hautement sensible.

## Clés et sauvegardes chiffrées

Créer ou importer un certificat X.509 de chiffrement. Stocker sa clé privée
dans un KMS/HSM ou coffre distinct de la base, des sauvegardes et du manifeste
de keyring. `postgres-backup.sh` diffuse `pg_dump` directement dans une
enveloppe CMS AES-256-GCM ; aucun dump clair n'est écrit sur disque. Le fichier
`.keys.json` ne contient que les IDs/références d'escrow des keyrings, jamais
les clés brutes.

```bash
DATABASE_URL="$MIGRATOR_DATABASE_URL" \
BACKUP_PATH=/var/backups/tutor/postgres-2026-08-12.dump.cms \
BACKUP_RECIPIENT_CERT=/etc/tutor/backup-recipient.pem \
KEYRING_MANIFEST_PATH=/etc/tutor/keyring-manifest.json \
./deploy/postgres-backup.sh
```

Copier ensemble l'enveloppe, son `.sha256` et le manifeste vers un stockage
immuable hors site. CMS-GCM authentifie le contenu ; le SHA-256 détecte aussi
les erreurs de transport avant déchiffrement. Tester périodiquement la clé
privée par restauration, sans la copier à côté des objets.

## PITR PostgreSQL

La plateforme PostgreSQL doit activer `wal_level=replica`, `archive_mode=on`,
un `archive_command` idempotent vers un stockage chiffré et monitoré, et des
base backups réguliers. L'âge du dernier WAL définit le RPO réel. Le fournisseur
managé peut remplacer `archive_command`, mais doit exposer âge/erreurs et une
cible temporelle ou LSN.

Le test autonome du dépôt crée une source PostgreSQL isolée, prend un
`pg_basebackup`, archive le WAL, écrit des marqueurs avant/après une cible LSN,
rejoue jusqu'à cette cible puis promeut le serveur restauré :

```bash
ALLOW_DESTRUCTIVE_PITR_EXERCISE=yes \
PITR_REPORT_PATH=/var/lib/tutor-audits/pitr-$(date -u +%Y%m%dT%H%M%SZ).json \
PITR_DOCKER_IMAGE=postgres:17 \
./deploy/pitr-restore-exercise.sh
```

Les conteneurs et volumes ont des noms uniques et sont supprimés par le trap.
Le rapport créé en mode exclusif prouve LSN, fichier WAL, latence d'archive,
temps de récupération, présence du marqueur avant cible, absence de celui
d'après et promotion. Ne pas confondre cet exercice avec une restauration de
dump complète.

## Restauration complète

Préparer une base vide, jetable et réseau-isolée. La commande suivante vérifie
le hash, déchiffre l'enveloppe, restaure toutes les données puis compare les
checksums tenant attendus. Elle détruit le contenu de la base de restauration,
jamais celui de la source :

```bash
ALLOW_DESTRUCTIVE_RESTORE_EXERCISE=yes \
SOURCE_DATABASE_URL="$SOURCE_DATABASE_URL" \
RESTORE_DATABASE_URL="$DISPOSABLE_DATABASE_URL" \
BACKUP_PATH=/var/backups/tutor/postgres-2026-08-12.dump.cms \
BACKUP_RECIPIENT_CERT=/etc/tutor/backup-recipient.pem \
BACKUP_RECIPIENT_KEY=/run/secrets/backup-recipient-key.pem \
TENANT_ID=tenant_acme \
EXPECTED_CHECKSUM_PATH=/var/lib/tutor-audits/tenant-acme.expected.json \
./deploy/postgres-full-restore-exercise.sh
```

Vérifier ensuite le ledger, le nombre de tenants, les keyrings restaurés,
`/ready`, les legal holds et les DSAR achevées après le backup. Ne router aucun
trafic avant réapplication des effacements/rétentions postérieurs.

## Archive logique d'un tenant

L'export utilise une transaction repeatable-read et capture toutes les tables
possédant `tenant_id`, la ligne tenant, puis uniquement les dépendances globales
référencées (users/identités/MFA, clients OAuth et plan). Il ordonne par PK et
calcule un hash des données tenant ainsi qu'un hash de l'archive complète. Les
objets narratifs restent chiffrés et liés par AAD tenant/enrollment.

```bash
DATABASE_URL="$SOURCE_DATABASE_URL" TENANT_ID=tenant_acme \
ARCHIVE_PATH=/var/backups/tutor/tenant-acme-2026-08-12.json.cms \
ARCHIVE_RECIPIENT_CERT=/etc/tutor/backup-recipient.pem \
ARCHIVE_REASON='exercice trimestriel' ARCHIVE_REQUEST_ID=CHG-2026-0812-03 \
./deploy/tenant-logical-backup.sh
```

La restauration exige exactement la même version de schéma, un tenant absent
de la cible et le DSN temporaire du rôle `tutor_restore`. L'archive JSON ne
touche jamais le disque en clair :

```bash
ALLOW_DESTRUCTIVE_RESTORE_EXERCISE=yes \
SOURCE_DATABASE_URL="$SOURCE_DATABASE_URL" \
RESTORE_DATABASE_URL="$TUTOR_RESTORE_DATABASE_URL" \
TENANT_ID=tenant_acme \
ARCHIVE_PATH=/var/backups/tutor/tenant-acme-2026-08-12.json.cms \
ARCHIVE_RECIPIENT_CERT=/etc/tutor/backup-recipient.pem \
ARCHIVE_RECIPIENT_KEY=/run/secrets/backup-recipient-key.pem \
./deploy/tenant-logical-restore-exercise.sh
```

Le restore valide format, hashes, inventaire de tables et `tenant_id` de chaque
ligne, garde RLS active avec `SET LOCAL app.current_tenant`, charge le graphe
dans une transaction sérialisable, réactive les triggers puis vérifie toutes
les FK tenant et recapture la donnée avant commit. Une collision de dépendance
globale incohérente, un tenant déjà présent ou un octet altéré fait échouer
l'opération. L'audit de restauration est append-only.

Après succès : déchiffrer un objet narratif avec les keyrings restaurés,
comparer les checksums, exécuter les tests A/B contre un tenant témoin, vérifier
qu'aucun user/plan/client étranger n'est apparu, puis réconcilier billing,
usage, holds, DSAR et événements postérieurs à la capture. La bascule de
domaine/feature flag reste une mutation control-plane distincte et auditée.
