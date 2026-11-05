# Tutor ACP 0.0.13 · Tutor-MCP v0.7.5

La reprise d'une formation est préparée par le client avant la génération :
le serveur confirme la sélection, fournit le contexte et choisit la prochaine
activité. Codex ou Claude formule ensuite une seule réponse, sans appels MCP
intermédiaires pour cette reprise.

## Reprise et suivi pédagogique

- Le bouton Reprendre et les commandes directes telles que « Reprendre la
  formation Python » utilisent le même parcours. Un nom ambigu renvoie des
  choix autorisés ; aucune formation n'est choisie ou rejointe arbitrairement.
- Le moteur choisit une introduction, un exercice ouvert ou un conseil de
  séance. La reprise n'impose plus un quiz. Les commandes quiz et flashcards
  restent disponibles.
- La consigne et le barème d'un exercice sont figés avant affichage. La zone de
  réponse dédiée soumet la réponse avant notation ; un message de conversation
  ou une demande d'indice n'est pas soumis automatiquement comme une réponse.
- Une introduction est enregistrée comme instruction après affichage, sans
  gain de maîtrise. Les reprises d'enregistrement conservent leur idempotence.
- La sélection et la révision sont contrôlées après génération. Une réponse
  devenue incompatible reste masquée et ne peut pas être acquittée.
- Les identifiants de critères issus du curriculum sont acceptés dans le
  barème sans devoir les réécrire ou recommencer sa préparation.
- Une nouvelle conversation dont le premier message est une reprise prépare
  directement la bonne session ACP, sans création suivie d'un rechargement.

Le client exige un serveur comprenant la nouvelle préparation pour ce parcours.
Avec un serveur ancien, la demande reste conservée et un message demande sa
mise à jour ; le client ne sélectionne pas silencieusement une autre formation.
Cette optimisation vise les harness Codex et Claude. Elle ne qualifie pas
l'inférence locale expérimentale.

## Distribution

Le client cible Windows x64, avec un harness natif ou WSL. Le serveur distribue
quatre archives : Linux et Windows, amd64 et arm64. Les nouvelles releases
n'incluent plus d'archives Darwin ; la matrice CI et les contrôles obligatoires
sont alignés sur ce périmètre. Les anciennes releases restent inchangées.

L'installateur attendu est `Tutor_ACP_0.0.13_windows_x64-setup.exe`.
L'identifiant et les emplacements de données du client restent inchangés.
La mise à jour ne nécessite aucune nouvelle migration de base de données.
La publication des fichiers de release ne met pas à jour les serveurs distants.

## Mesures et limites

Après suppression du rechargement initial, les mesures réelles Linux donnent
19,5 s puis 11,9 s pour une reprise Codex GPT 6.1 sol en réflexion élevée,
et 14,9 s puis 7,0 s avec l'alias `sonnet` / `xhigh` annoncé par Claude. Aucun
appel outil du modèle n'a été nécessaire. Deux corrections chaudes ont pris
7,7 s et 6,7 s avec Codex, 6,7 s et 5,2 s avec Claude. Ce sont des mesures
natives de candidat, sans fenêtre Windows, avec deux échantillons par scénario.

La cible de réponse complète en une à cinq secondes reste **non atteinte**.
La génération externe demeure variable ; aucun délai maximal universel n'est
garanti. La réflexion et le modèle de l'utilisateur ne sont pas abaissés.
Les tests CI vérifient le fonctionnement ; ils ne remplacent pas les mesures
avec modèle réel jusqu'à l'affichage de la réponse dans la fenêtre Windows.
Voir le [protocole de qualification](qualification/acp-latency.md) pour la matrice,
la vérification des réglages effectifs et l'export local des mesures de rendu.
