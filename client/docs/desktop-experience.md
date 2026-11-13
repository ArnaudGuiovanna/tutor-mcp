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
et Tutor local, au moins une conversation ou une connexion distante déjà réussie,
sont considérés comme configurés. Une adresse distante seulement enregistrée ne
suffit pas : une configuration neuve interrompue en attente d’autorisation de la
clé reprend à l’étape « Serveur Tutor ». La preuve d’une connexion réussie est
`settings.tutorConnectedOrigin` (origine du dernier serveur atteint, écrite par la
couche native) ou, à défaut, une politique d’autorisations liée à cette origine
et à une institution.
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
L’onglet Institution affiche une seule fois le nom de l’institution, puis sa
politique d’apprentissage ; Membres traite les adhésions. Ces pages renvoient au
même écran de connexion si nécessaire.

Les onglets suivent le modèle ARIA (`tablist`/`tab`/`tabpanel`, flèches, Début,
Fin). L’onglet est piloté par la page de l’application : la palette et les
raccourcis le changent sans remonter l’espace des paramètres. Un panneau déjà
ouvert reste monté (masqué) : ses saisies et ses lectures sont conservées.

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

Chaque ligne porte un liseré de niveau aux couleurs de la charte : information
(bleu) pour l’attente et la reconnexion, alerte (rouge) pour l’échec,
avertissement (ambre) pour une limite du fournisseur.

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

**Limite d’actions d’un tour.** Un tour arrêté par son budget (`turn_limit`, ou à
défaut `stopReason: "max_turn_requests"` avec `turnLimit` dans le résultat de
`send_prompt`) affiche un seul avis informatif (liseré bleu, `role="status"`,
pas d’alerte), rattaché à la réponse interrompue de ce tour à la place de la
ligne « Réponse interrompue » : « Le tuteur a atteint la limite de 12 actions
pour cette réponse. » L’action « Continuer » envoie une relance courte par le
chemin d’envoi habituel. Le texte `message` de l’événement n’est pas affiché ;
un événement d’une autre conversation est ignoré, un événement tardif du même
tour ne duplique pas l’avis.

**Tour en cours côté interface.** Pendant une réponse, la connexion de l’agent
(choix, connexion, déconnexion, enregistrement, identification), la connexion
Tutor, les réglages du modèle et « Reconnecter » sont désactivés ; une note
visible, reliée par `aria-describedby`, explique « Interrompez la réponse en
cours pour modifier la connexion ou les réglages de l’agent. ». Si le refus
natif survient malgré tout, il s’affiche traduit comme toute erreur.

**Événements de session.** Les `session/update` ne portent qu’un identifiant de
session. L’interface les rattache au tour actif : session MCP courante de sa
conversation (`agentSessionId`, éventuellement `null` pour une conversation
neuve), ou une seule session inconnue apprise pour ce tour (session privée ou
nouvelle), après le début du prompt (`turn_phase` `waiting_model`). Les sessions
d’autres conversations, et les sessions remplacées de la même conversation,
sont refusées.

## Fiabilité de la connexion au harness (backend natif)

**Tour en cours.** Le verrou d’opération n’est tenu que pendant la préparation.
Dès que le tour est enregistré (`Prompt`, état de tour explicite), il est
relâché avant l’attente du modèle. Les opérations incompatibles (changer
d’agent ou de connexion Tutor, enregistrer des réglages, choisir un modèle,
commencer un autre tour) sont **refusées immédiatement** (« Interrompez la
réponse en cours… ») plutôt que d’annuler le tour : un changement d’agent
tuerait des outils qui peuvent être en train d’écrire la progression, et
l’annulation reste un geste explicite (Stop). `agent_configuration` répond sans
attendre avec la configuration déjà appliquée ; `select_conversation` ne change
que la conversation affichée et n’attend ni n’interrompt le tour, lié à sa
propre conversation et à sa session.

**Budget d’un tour.** Claude reçoit `maxTurns` = 12. Codex n’a pas d’option
équivalente : au-delà de 12 appels d’outil distincts (`tool_call` par
`toolCallId`), Tutor envoie `session/cancel` (relance du processus après 15 s
sans confirmation). Le tour se termine avec `stopReason: "max_turn_requests"`
et `turnLimit: 12`, la réponse est « interrompue », et l’événement
`acp:event {type:"turn_limit", conversationId, stopReason, limit, enforcedBy:
"client"|"agent", message}` donne un message lisible (aussi émis quand Claude
atteint `maxTurns`).

**Deux sessions par conversation.** `agentSessionId` est la session avec le
serveur MCP Tutor ; une seconde session sans serveur MCP, tenue par le
processus, exécute les tours à sortie privée (activité, évaluation, reprise
native). L’alternance ne recharge plus aucune session (`session/load`
recréait le processus Claude et son cache de prompt). Une session qui a manqué
des tours de l’autre reçoit l’historique récent avec son prompt suivant (état
persistant, interne, non exposé à l’interface). Une seule session privée reste
ouverte par processus ; les sessions remplacées sont fermées par
`session/close` lorsque l’adaptateur l’annonce. Les événements d’un tour privé
portent l’identifiant de la session privée, différent de `agentSessionId`.

**Relais MCP local.** Pour un harness natif (hors agent local et WSL), l’adaptateur
reçoit `http://127.0.0.1:<port>/mcp/<route>` et un bearer local aléatoire,
stables pour le processus Tutor ; le relais n’écoute que sur la boucle locale,
refuse un `Origin` de navigateur, un `Host` non local ou un bearer inconnu, et
relaie vers la ressource Tutor avec le jeton institutionnel courant : méthode,
en-têtes MCP (`Mcp-Session-Id`, `Mcp-Protocol-Version`, `Last-Event-ID`),
codes d’erreur et corps morceau par morceau (SSE en direct). Un 401 amont
renouvelle le jeton puis rejoue une seule fois la requête. Le renouvellement du
jeton ne change donc plus l’empreinte de la session. `NO_PROXY` inclut
`127.0.0.1,localhost` pour le harness. Vérifié : l’adaptateur Claude
annonce `mcpCapabilities.http` et transmet `headers`
(`claude-agent-acp/dist/acp-agent.js` l. 1401-1404 et 8505-8516), Codex
aussi (`codex-acp/dist/index.js` l. 35527-35530 et 42462-42465). Un harness WSL
garde la configuration directe (sa boucle locale n’est pas celle de Windows) ;
l’agent local aussi : il rafraîchit par `_tutor/rebind` sans recréer de
processus (`client/local-agent/src/mcp.rs`, `mcp_session.rs`).

**Réglages Claude hérités.** `settingSources: ["user"]` appliquait aussi les
plugins (`enabledPlugins`) et les règles `permissions.allow` de l’utilisateur.
Le SDK 0.3.309 n’offre aucune option pour les neutraliser une à une (les
tableaux d’autorisations s’unissent entre sources), mais `settingSources: []`
n’en charge aucune (`sdk.d.ts` l. 2305-2315) et le niveau `--settings`
(`options.settings`) porte `apiKeyHelper` : la CLI le lit dans les réglages
fusionnés sans exiger la confiance du dossier, réservée aux fichiers projet et
local (`Cg()`/`Ow()` dans la CLI embarquée). Pour un harness natif, Tutor lit
donc au lancement `~/.claude/settings.json` (ou `CLAUDE_CONFIG_DIR`) et ne
transmet que `apiKeyHelper`, `awsAuthRefresh`, `awsCredentialExport`,
`gcpAuthRefresh`, `proxyAuthHelper`, `forceLoginMethod`, `forceLoginOrgUUID`,
`model`, `modelOverrides` et les variables `env` de fournisseur, de proxy et de
certificat (`ANTHROPIC_*`, `AWS_*`, `CLAUDE_CODE_USE_*`, `GOOGLE_*`…), avec
`settingSources: []`. `model` reste nécessaire : l’adaptateur choisit le
modèle initial avec ses propres réglages en supposant que la CLI voit le même
(`session-model.js` l. 390-450). Limites : un fichier illisible ou invalide, et
un harness WSL, conservent `settingSources: ["user"]` ; une modification de ce
fichier s’applique au lancement suivant ; le gestionnaire de réglages de
l’adaptateur lit toujours toutes les sources pour `permissions.defaultMode` et
les listes de modèles (`settings.js`, `acp-agent.js` l. 8500-8568), ce que
Tutor ne peut pas désactiver (`permissionMode` est réservé à l’adaptateur).

**`disableAllHooks` et hooks de l’adaptateur.** Les hooks de l’adaptateur
(PostToolUse, PostModelSwitch, PostCompact, TaskCreated…) sont des callbacks
SDK passés dans `options.hooks` (`acp-agent.js` l. 8732-8860), pas des
réglages. Le SDK les déclare à la CLI à l’initialisation ; la CLI les enregistre
avec `origin: "sdkHost"` dans `registeredHooks`. Sa collecte des hooks (`dz`)
ne filtre parmi eux que les hooks de plugin (`pluginRoot`) et d’appareil
(`deviceOwner`), y compris sous un `disableAllHooks` de politique, et un
`disableAllHooks` de niveau `--settings` agit seulement via
`allowManagedHooksOnly` sur les hooks de fichiers de réglages et de plugins ;
la CLI journalise d’ailleurs « SDK callback hooks still run ». Aucune
correction n’est nécessaire.

## Lecture et interactions

- Hauteurs issues des jetons de la charte : boutons de 32 pixels
  (`--control-md`, 40 pour les actions principales), champs de 34 pixels
  (`--input-h`) ; un bouton posé à côté d’un champ en prend la hauteur. Marges
  de page proportionnelles et largeur de lecture limitée.
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

**Lecture et échec de lecture.** Les panneaux qui interrogent le serveur
(Formations de la barre latérale, Planning, Membres, politique de l’institution,
demandes d’effacement, panneau de formation) affichent le même indicateur
(`LoadingState` : texte et indicateur, ou squelette dans la barre latérale) puis,
en cas d’échec, la cause et « Réessayer » (`LoadError`).

**Styles.** Toutes les valeurs viennent des jetons de `src/styles.css` (:root) ;
aucun style en ligne. La taille et l’interligne de lecture passent par
`data-reading-size` et `data-reading-line-height`, la jauge de maîtrise par un
SVG dont seules la largeur et la position du seuil sont des attributs.

## Vérification

Les tests frontend couvrent la configuration guidée (verrou, reprise, adresse
normalisée, échecs de découverte et clé non autorisée, focus, migration), la
réponse d’exercice depuis le composeur, les états de reconnexion (attente,
échec et « Reconnecter »), la limite du fournisseur avec envoi suspendu puis
réactivé (temps simulé), l’échec rattaché au tour, le modèle automatique, la
saisie Tutor préservée pendant l’édition, ainsi que le parcours complet Tutor → agent → autorisations
sans changer d’onglet, la sauvegarde de l’identité sur place, les exceptions
persistantes, les suggestions, la copie fidèle et les commandes de fenêtre,
les états de lecture et « Réessayer » (`tests/frontend/UiStates.test.tsx`), les
formulaires d’identité (soumission, identifiants d’aide uniques, copie
désactivée pendant une opération) et les onglets contrôlés des paramètres.

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
