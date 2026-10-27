# Tutor ACP 0.0.3 · Tutor MCP v0.7.2

Cette release livre la finition du client ACP pour Windows : une fenêtre unifiée,
une lecture plus confortable et un parcours de connexion regroupé.

## Client ACP 0.0.3

- Fenêtre sans barre de titre native, avec commandes personnalisées pour réduire,
  agrandir, restaurer et fermer. Déplacement, double clic et redimensionnement
  restent disponibles.
- Espacements, marges, boutons et champs harmonisés ; hiérarchie des titres,
  paragraphes, listes, citations et tableaux retravaillée.
- Code avec retour à la ligne et copie fidèle, confirmation de copie et tableaux
  adaptés aux petites fenêtres.
- Saisie qui grandit avec le message, exemples qui préparent la conversation,
  transitions discrètes et activité animée suivant les événements de l’agent.
  Les préférences de réduction des animations restent prioritaires.
- **Connexion et outils** regroupe Tutor, l’identité nécessaire, l’agent IA et
  les permissions MCP persistantes. Les connexions établies se replient en résumés.
- Choisir un agent ne rétablit plus une ancienne adresse Tutor. Changer la règle
  générale des autorisations conserve les exceptions définies par outil.
- La taille du texte se prévisualise immédiatement et s’enregistre au relâchement
  du curseur ou après l’action au clavier.

Télécharger **`Tutor_ACP_0.0.3_windows_x64-setup.exe`** et l’installer par-dessus
la version précédente. La version de produit est réellement **0.0.3**.
L’identifiant `fr.aguiovanna.tutor` et les emplacements des données sont conservés :
les comptes, clés, conversations et réglages restent en place. Les adaptateurs
Codex et Claude et leurs runtimes sont inclus, comme dans la version précédente.

## Distribution et vérifications

Les six archives de Tutor MCP v0.7.2 sont jointes pour conserver les téléchargements
serveur de la release **Latest**. Le code du serveur et ses migrations sont
inchangés depuis v0.7.1 ; cette livraison porte sur le client de bureau.

Les rapports joints identifient le commit et les workflows ayant construit les
fichiers. Le contrôle Windows installe réellement le client, exerce les commandes
de fenêtre, la navigation au clavier, les langues et les thèmes, puis vérifie les
réglages après fermeture et réouverture. Il utilise un profil de test hors ligne :
il ne constitue pas une séance avec un modèle authentifié ni un audit manuel
complet de Narrator.

`SHA256SUMS` couvre tous les assets à l’exception de lui-même. Les notices des
dépendances sont incluses dans les distributions. Les détails de l’interface sont
décrits dans la [fiche du client](https://github.com/ArnaudGuiovanna/tutor-mcp/blob/v0.7.2/client/docs/desktop-experience.md).
