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

## Configuration guidée

Avant tout accès à l’application, un parcours plein écran (sous `WindowChrome`,
sans rail ni barre latérale) présente une étape par écran, avec l’indicateur
« Étape n sur 3 », Retour et une action principale. Le focus se place sur le
titre à chaque changement d’étape ; les erreurs sont annoncées (`role="alert"`).

1. **Compte** : nom, phrase de sauvegarde (12 caractères minimum) et sa
   confirmation. Un seul bouton renomme le compte, crée l’identité si besoin,
   exporte la sauvegarde chiffrée puis la confirme. Le lien « J’ai déjà un
   compte » restaure une sauvegarde existante.
2. **Assistant IA** : deux cartes Claude et Codex issues de `bundledAgents()`,
   avec leur statut de détection. Continuer enregistre `targetId` puis lance
   l’agent ; si le harness demande une connexion, « Ouvrir la connexion
   officielle » appelle `authenticateAgent`.
3. **Serveur Tutor** : un seul champ d’adresse. « Tester et connecter »
   normalise l’adresse (espaces retirés, `https://` ajouté, chemin, requête et
   fragment supprimés : `https://hôte/mcp` devient `https://hôte`), refuse
   `http://` hors `localhost`, `127.0.0.0/8` et `[::1]` avec un message distinct
   par cas, découvre l’institution, enregistre l’adresse puis se connecte. Une clé
   refusée (HTTP 401 ou 403) affiche « En attente d’autorisation » avec
   l’identifiant public à copier et « Réessayer ». Aucun lien vers le mode local
   n’est proposé : Tutor local exige le chemin d’un programme `tutor-mcp` que
   l’application ne fournit pas.

L’étape se déduit de l’état (`src/onboardingModel.ts`) : une configuration
interrompue reprend à la première étape incomplète, sans stockage
supplémentaire. Le réglage `setupCompleted` est posé à la fin de l’étape 3 et
termine définitivement le parcours. Les profils antérieurs, avec un agent choisi
et Tutor local ou au moins une conversation, sont considérés comme configurés.
Pendant le parcours, la connexion automatique de l’agent et les raccourcis
Ctrl K / Ctrl N sont neutralisés. Paramètres → Connexion et outils propose
« Relancer la configuration guidée » ; un compte créé depuis Mon compte repasse
par le parcours. Si l’état local ne peut pas être lu, un écran bloquant propose
« Réessayer » au lieu d’ouvrir la conversation.

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

**Modèle et rapidité.** Sans modèle choisi, le sélecteur indique « Automatique
(rapide et économe) » : le harness connu utilise son petit modèle et l’effort de
réflexion le plus bas, sauf préférence « Approfondi ». Sous le sélecteur, le
modèle et l’effort réellement appliqués à la session (`effective`) sont
affichés. La préférence « Rapide » est décrite comme « petit modèle, effort
bas » ; un niveau de réflexion explicite reste prioritaire.

Chaque formulaire ne sauvegarde que ses propres champs. Une saisie non
enregistrée (adresse Tutor, programme, arguments, dossier de travail) n’est pas
remplacée par un nouvel état reçu du client ; le formulaire se resynchronise
après enregistrement ou connexion. Une modification du
choix d’agent ne rétablit pas une ancienne adresse Tutor. Les exceptions des
outils restent conservées lorsque la règle générale change. Les permissions
restent liées au compte, à l’origine, à l’institution et à l’agent ; aucune
permission n’est accordée par défaut par cette refonte.

## États de connexion de l’agent

L’interface lance une seule fois le harness mémorisé à l’ouverture (et après un
changement de compte, d’agent ou de dossier). Elle ne relance plus rien
d’elle-même : après un arrêt inattendu, la supervision native reconnecte avec
un délai croissant et publie son état (`agentStatus.reconnect`, événements
`agent_reconnect`). Une ligne discrète au-dessus du composeur l’indique :

- **en attente** : « Reconnexion à l’agent dans N s (tentative k) », avec un
  décompte à la seconde (seul un libellé stable est annoncé aux lecteurs
  d’écran) ;
- **en cours** : « Reconnexion à l’agent en cours… » ;
- **échec définitif**, ou lancement initial impossible : cause lisible et bouton
  « Reconnecter » ; la fin de la sortie d’erreur du harness (`stderrTail`),
  lorsqu’elle existe, est repliée dans « Détails techniques ».

Une déconnexion volontaire reste arrêtée. Un message envoyé alors que l’agent
n’est pas connecté le relance pour ce tour.

**Limites du fournisseur.** Pendant une limite (`limitedUntil`, `limitKind`,
événement `agent_limited`), la même zone affiche « Limite atteinte (requêtes |
surcharge du fournisseur | quota d’usage), reprise à HH:MM » et un décompte. Le
message reste modifiable, mais l’envoi est désactivé, sa raison étant donnée en
description accessible du bouton ; il se réactive automatiquement à l’heure dite.

**Erreurs.** L’échec d’un tour (`turn_error`) s’affiche dans la conversation,
après ce tour, avec « Réessayer » ; il n’occupe pas le bandeau global. Le
bandeau global garde un seul message, traduit une seule fois, et se ferme.

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
- Un exercice en attente se répond dans le composeur principal, seul champ de
  saisie de l’écran ; la carte n’affiche que l’en-tête et la consigne, puis
  « Réponse enregistrée ». Une bascule visible choisit « Réponse à l’exercice »
  (par défaut) ou « Question au tuteur », et porte l’action « Un indice ». La
  réponse est envoyée sous forme de bloc `tutor-response` (`kind: exercise`),
  sans pièce jointe et dans la limite de 12 000 octets. Seul un exercice de la
  formation active, sans réponse ni nouvelle demande d’activité, est concerné.
- La liste des messages est une région nommée et défilable au clavier, pas un
  journal annoncé en continu. Les libellés « Vous » et « Tutor » restent lus par
  les lecteurs d’écran sans être affichés. La fin d’une réponse annonce
  seulement « Réponse terminée ». Dans un quiz, la légende nomme le groupe,
  chaque option est nommée par son texte et la correction en est la description.
- Les raccourcis affichés utilisent « ⌘ » sur macOS et « Ctrl » ailleurs.
- Le curseur de taille donne un aperçu immédiat et enregistre sa valeur au
  relâchement, au clavier ou à la sortie du champ. La confirmation de sauvegarde
  ne déplace pas le formulaire.

## Vérification

Les tests frontend couvrent la configuration guidée (verrou, reprise, adresse
normalisée, échecs de découverte et clé non autorisée, focus, migration), la
réponse d’exercice depuis le composeur, les états de reconnexion (attente,
échec et « Reconnecter »), la limite du fournisseur avec envoi suspendu puis
réactivé (temps simulé), l’échec rattaché au tour, le modèle automatique, la
saisie Tutor préservée pendant l’édition, ainsi que le parcours complet Tutor → agent → autorisations
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
