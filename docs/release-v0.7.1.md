# Tutor MCP v0.7.1 · Client ACP 0.0.2

**Apprendre tout au long de la vie. Faire vivre des communautés d’apprentissage.**

Cette livraison apporte la nouvelle interface du client ACP pour Windows,
alignée sur la charte fournie pour Tutor-MCP et tutor-mcp.dev.

## Client ACP 0.0.2

- Nouvelle identité visuelle : fond bleu nuit `#0C131F`, accent `#3D8BFF`,
  wordmark à curseur, navigation et bibliothèque plus compactes, nouvelle icône.
- Polices Inter et JetBrains Mono embarquées : le client les utilise sans
  téléchargement de polices depuis un service externe.
- Palette de commandes **Ctrl+K** : recherche insensible aux accents, reprise
  d’une conversation, accès aux formations, au compte et aux réglages, choix du thème.
  Flèches et Entrée sélectionnent ; Échap ferme ; Tab reste dans le dialogue.
- Thèmes sombre, clair et système et préférences de lecture conservés par compte.
- Jauges de progression adaptées à la charte. Les valeurs proviennent du serveur
  et les estimations restent distinctes des preuves de maîtrise.

Le client conserve l’identifiant `fr.aguiovanna.tutor`. Installer cette version
par-dessus l’existante conserve les dossiers des comptes, les clés et les échanges.
Les sauvegardes d’identité restent nécessaires pour la récupération.

Téléchargement : **`Tutor_ACP_0.0.2_windows_x64-setup.exe`**.
La version de produit de l’application est réellement **0.0.2**.
Les adaptateurs Codex et Claude et leurs runtimes restent inclus.

## Bannière et documentation

La bannière du README reprend les couleurs, le wordmark et les polices de la
nouvelle charte. Les polices et leurs notices originales sont incluses dans le SVG.
L’organisation du README et la démonstration existante sont conservées.
Les sources du nouveau site fournies dans `ui/site` restent indépendantes de
cette livraison du client ; cette release ne constitue pas un déploiement web.

## Distribution du serveur

Les six distributions de **Tutor MCP v0.7.1** sont jointes pour que la release
**Latest** continue aussi de servir les installateurs Linux, macOS et Windows.
Elles contiennent leurs notices et disposent des mêmes suppléments de notices
que v0.7.0. Aucune nouvelle migration n’est ajoutée depuis v0.7.0.

`SHA256SUMS` couvre tous les assets, à l’exception de lui-même. Les rapports de
qualification relient les fichiers au commit et aux workflows utilisés.

## Portée des vérifications

Les rapports Windows couvrent le lancement natif des agents embarqués, la
persistance MCP directe, WSL2, l’application installée, la palette de commandes,
les langues, les thèmes et la réouverture du même profil. Le profil UI Automation
est hors ligne : ce contrôle ne revendique ni génération d’un modèle ni audit
manuel de Narrator. Les résultats effectifs et les éventuelles reprises de
workflow sont conservés dans les rapports joints.
