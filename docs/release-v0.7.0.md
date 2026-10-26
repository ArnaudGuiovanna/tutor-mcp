# Tutor MCP v0.7.0 · Client ACP 0.0.1

**Apprendre tout au long de la vie. Progresser dans des communautés d’apprentissage.**

Tutor accompagne les envies, les projets et les changements de métier par des
parcours personnels que l’on peut reprendre dans la durée. Le client ACP donne
aux communautés un espace pour construire, examiner et partager des formations,
tandis que chacun conserve son rythme et sa progression.

## Tutor MCP 0.7.0

- Création immédiate de formations personnelles depuis la conversation,
  bibliothèque unifiée, recherche et reprise explicite de la formation active.
- Formations institutionnelles, invitations, contributions et validation des
  versions. Les destinataires nominatifs restent protégés et les versions
  publiées des apprenants restent immuables.
- Gestion des programmes, des accès, des inscriptions et du suivi pédagogique.
  Les auteurs peuvent partager une copie de leur conception sans publier leurs
  conversations ni leur progression privée.
- Progression individuelle, révisions et évaluations structurées. La calibration
  collective est isolée par institution, version et concept ; elle exige
  30 contributeurs ayant chacun cinq observations admissibles et exclut
  l’historique privé.
- Identités publiques et récupération, gestion des membres et effacement
  institutionnel contrôlé. Les actions du compte sont signées localement et liées
  à la session ; les droits administratifs ne sont pas confiés à l’agent.
- Autorisations MCP persistantes, nettoyage PostgreSQL avec un worker à droits
  minimaux, diagnostics sans contenu privé et abandon des connexions annulées
  dont la protection de lecture ne peut être remise à zéro en toute sûreté.

## Client ACP 0.0.1 pour Windows x64

Le nouvel asset `Tutor_ACP_0.0.1_windows_x64-setup.exe` est l’installeur du client,
avec Codex et Claude intégrés. Sa version de produit est réellement **0.0.1**.

- Conversation et formations, avec un espace Compte et des Paramètres organisés
  en Agents, Institution, Membres et Ergonomie.
- Interface française ou anglaise, thèmes clair/sombre/système, taille du texte,
  interligne et réduction des animations conservés par compte.
- Autorisations par outil, réglages de modèle et de réflexion proposés par
  l’agent, activité textuelle et journal de réponse récupérable.
- Archivage, restauration et suppression selon les droits du serveur ; retrait
  d’un compte local distinct de l’effacement institutionnel.

Cette numérotation ouvre la distribution du client ACP dans la release du
serveur. Les préversions techniques `tutor-client-v0.2.x` et `tutor-client-v0.3.0`
restent disponibles. L’identifiant de l’application et ses dossiers de données
sont conservés : installez cette version par-dessus l’existante sans régénérer
vos clés. Les sauvegardes exportées restent nécessaires pour la récupération.

## Téléchargements et vérifications

Les six archives du serveur couvrent Linux, macOS et Windows, sur amd64 et arm64.
Elles contiennent le binaire et ses notices originales. Les mêmes familles
d’assets de notices que dans v0.6.1 sont également disponibles séparément, avec
leur provenance et leurs sommes SHA-256. Les anciens assets ne sont pas modifiés.

`SHA256SUMS` vérifie tous les fichiers de cette release. Les rapports Windows
documentent séparément le protocole ACP, le transport MCP natif, les agents
embarqués et les interactions de l’application installée. Une négociation ACP
ne prouve pas une génération de modèle ; l’UI Automation ne constitue pas un
audit manuel de Narrator. La première vérification du paquet reste plus lente
que les reconnexions dans la même application.

## Mise à jour du serveur

Depuis v0.6.1, cette version inclut des évolutions de schéma et de permissions.
Sauvegardez la base et les clés ensemble. Pour une institution, exécutez le rôle
migrator puis appliquez `deploy/postgres-roles.sql` avec le propriétaire de la base
avant de redémarrer API et worker. Conservez les répertoires de données existants.
Aucune formation existante n’est publiée automatiquement.

Voir le [guide d’installation](https://github.com/ArnaudGuiovanna/tutor-mcp/blob/v0.7.0/docs/installation.md),
le [client ACP](https://github.com/ArnaudGuiovanna/tutor-mcp/blob/v0.7.0/client/README.md)
et les rapports joints pour les commandes et la portée des vérifications.
