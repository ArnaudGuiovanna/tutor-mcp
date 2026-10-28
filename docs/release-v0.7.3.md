# Tutor ACP 0.0.4 · Tutor MCP v0.7.3

Cette release ajoute des activités visuelles dans la conversation et un planning
personnel des révisions, en s’appuyant sur le moteur pédagogique existant.

## Client ACP 0.0.4

- Saisir `/` ouvre le choix **Quiz** ou **Flashcards**. Les commandes `/quiz` et
  `/flashcards` acceptent aussi un sujet après leur nom.
- Les quiz proposent des réponses cliquables, une validation puis une correction
  expliquée. La tentative et sa rubrique sont préparées avant la réponse.
- Les flashcards se retournent au clic et recueillent une autoévaluation :
  **Pas retrouvé**, **Retrouvé avec effort**, **Retrouvé facilement**. Cette
  déclaration reste distincte d’une réponse vérifiée dans un quiz.
- **Planning**, dans la sidebar, permet d’activer les révisions par formation,
  de choisir leur format et de consulter les échéances calculées par Tutor-MCP.
  **Réviser maintenant** ouvre le chat et confirme la bonne formation avant de
  démarrer l’activité.
- Les préférences sont conservées par compte, connexion, institution et
  formation. Désactiver une formation dans le Planning ne désactive pas le suivi
  pédagogique du serveur.
- Les réponses sont conservées après fermeture et protégées contre les doubles
  envois. Les demandes hors ligne restent **À envoyer**. Retirer une réponse en
  attente conserve un reçu local, sans enregistrement serveur ni nouvelle tentative.
- Les activités invalides ou interrompues ne dévoilent pas leur correction.
  La question reste visible à l’ouverture des cartes sur les petites fenêtres ;
  souris, clavier, français/anglais et thèmes clair/sombre sont pris en charge.

## Installation Windows

Télécharger **`Tutor_ACP_0.0.4_windows_x64-setup.exe`** et l’installer par-dessus
la version précédente. L’identifiant `fr.aguiovanna.tutor`, les comptes, les clés,
les conversations et les réglages existants sont conservés. Les adaptateurs
Codex et Claude et leurs runtimes restent inclus.

## Distribution et vérifications

Les six archives serveur de Tutor-MCP v0.7.3 et leurs notices sont jointes pour
conserver les téléchargements habituels de la release **Latest**. Aucun code
du serveur ni migration ne change depuis v0.7.2.

Les contrôles obligatoires sont exécutés sur `staging` avant la promotion de ce
même commit vers `main`. Le workflow Windows construit, installe et démarre le
client, vérifie son interface native et les adaptateurs ACP. Les rapports joints
identifient le commit et les workflows ayant produit les fichiers.

La qualification visuelle des nouvelles modalités utilise des données de test
et couvre les deux thèmes, le clavier et les petites largeurs. Ces contrôles ne
constituent pas une séance pédagogique avec un fournisseur IA authentifié ni
une validation expérimentale de gains d’apprentissage.

`SHA256SUMS` couvre tous les assets à l’exception de lui-même. Les détails de
l’utilisation sont disponibles dans le [guide du client](https://github.com/ArnaudGuiovanna/tutor-mcp/blob/v0.7.3/client/README.md).
