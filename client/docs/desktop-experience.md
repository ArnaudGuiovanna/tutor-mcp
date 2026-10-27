# Finition du client de bureau

Le client conserve la charte Tutor-MCP : bleu, fonds bleu nuit, Inter et
JetBrains Mono, avec leurs équivalents clairs. La fenêtre et son contenu forment
une seule surface. Les couleurs, les rayons, les espacements et les transitions
sont définis dans `src/styles.css`.

## Fenêtre

La configuration Tauri désactive les décorations natives. `WindowChrome` reste
présent pendant le chargement et fournit les boutons Réduire, Agrandir/Restaurer
et Fermer. Les commandes appellent la fenêtre native ; le bouton d’agrandissement
suit aussi les changements effectués par Windows. Le titre se déplace par sa
surface vide et s’agrandit par double clic. Les bordures continuent à permettre
le redimensionnement. Les commandes ont un nom accessible, un état de focus et
une zone de 46 × 44 pixels CSS.

Les permissions de fenêtre sont limitées à ces commandes et au déplacement,
pour la fenêtre locale `main`. Elles n’accordent aucun accès MCP supplémentaire.
Cette intégration suit la [personnalisation des fenêtres Tauri](https://v2.tauri.app/learn/window-customization/).

## Connexion et outils

Le premier onglet des paramètres regroupe trois étapes :

1. **Tutor** : adresse de l’institution, vérification et connexion. La création
   ou la restauration de l’identité, ainsi que sa sauvegarde, restent disponibles
   ici lorsqu’elles sont nécessaires.
2. **Agent IA** : choix de Codex, Claude ou d’une installation existante,
   connexion fournisseur, puis modèle et niveau de réflexion réellement proposés.
3. **Outils et autorisations** : règle persistante et exceptions par outil, avec
   recherche et affichage progressif du catalogue retourné par le serveur.

Les connexions établies se replient en résumés consultables. Les trois raccourcis
amènent directement à la section voulue, avec déplacement du focus au clavier.
L’onglet Institution traite la politique d’apprentissage ; Membres traite les
adhésions. Ces pages renvoient au même écran de connexion si nécessaire.

Chaque formulaire ne sauvegarde que ses propres champs. Une modification du
choix d’agent ne rétablit pas une ancienne adresse Tutor. Les exceptions des
outils restent conservées lorsque la règle générale change. Les permissions
restent liées au compte, à l’origine, à l’institution et à l’agent ; aucune
permission n’est accordée par défaut par cette refonte.

## Lecture et interactions

- Navigation de 42 pixels minimum, boutons usuels de 40 pixels, champs de
  42 pixels, marges de page proportionnelles et largeur de lecture limitée.
- Titres, paragraphes, listes, citations et tableaux ont une hiérarchie distincte.
  Le code revient à la ligne, affiche son langage et se copie sans modification.
  Les tableaux larges se présentent verticalement quand la place manque.
- Les exemples d’accueil préparent le message et placent le focus dans la
  saisie. Ils ne l’envoient pas. La saisie grandit avec son contenu.
- L’activité reflète les événements de l’agent ; ses points respirent doucement.
  Les changements d’écran et les actions utilisent des transitions brèves.
  Les préférences de réduction des animations du système et du compte priment.
- Le curseur de taille donne un aperçu immédiat et enregistre sa valeur au
  relâchement, au clavier ou à la sortie du champ. La confirmation de sauvegarde
  ne déplace pas le formulaire.

## Vérification

Les tests frontend couvrent le parcours complet Tutor → agent → autorisations
sans changer d’onglet, la sauvegarde de l’identité sur place, les exceptions
persistantes, les suggestions, la copie fidèle et les commandes de fenêtre.

Le contrôle CI `Client (Windows)` installe l’exécutable NSIS dans un compte de
qualification isolé. `test-window-chrome.ps1`, appelé par la recette UI Automation,
observe la géométrie native et exerce réduction, agrandissement/restauration,
déplacement, double clic et redimensionnement. La fermeture utilise les boutons
personnalisés dans les deux langues ; le processus doit réellement se terminer.
La seconde ouverture vérifie les réglages conservés. Le reçu JSON et les captures
Windows accompagnent l’installeur dans l’artefact du workflow.

La revue visuelle locale couvre l’accueil, la conversation, les connexions,
les autorisations, le compte, les formations et la palette entre 320 et
1 240 pixels, les thèmes sombre et clair, le texte à 24 pixels, les lignes de
code et tableaux longs, ainsi que la réduction des animations. Cette revue avec
un pont de démonstration ne remplace pas les contrôles natifs Windows.
