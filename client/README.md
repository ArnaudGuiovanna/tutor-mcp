# Tutor — client de bureau

Application Tauri pour apprendre avec Codex ou Claude via Agent Client Protocol (ACP).
Le client est développé dans ce dossier du dépôt Tutor ; le serveur pédagogique
reste à la racine. La cible de référence est **Windows x64**. Un agent peut être
exécuté nativement ou dans une distribution **WSL2** choisie explicitement.

## Agent Qwen local en développement

La préversion Windows CPU regroupe **Tutor 0.0.7 et l’agent local dans un seul
installateur**. Dans **Paramètres → Connexion et outils**, installer le modèle
Qwen avec le bouton de téléchargement, ou sélectionner le GGUF déjà disponible
sur l’ordinateur. L’application conserve le modèle dans son dossier de données
et vérifie sa taille et son SHA-256 avant activation. Il n’y a aucun script ni
variable d’environnement à configurer pour ce parcours.

Le modèle pèse environ **1,1 Go** : il est téléchargé depuis une révision
Hugging Face épinglée. Une connexion Internet est nécessaire pour cette
installation initiale, sauf si le GGUF exact est déjà disponible. L’inférence
est ensuite locale ; la connexion au serveur Tutor garde son fonctionnement
habituel. Le serveur MCP n’est pas embarqué ni modifié. La préversion permet
de tester la conversation, la reprise et deux lectures MCP ; elle n’active
pas encore l’accélération P2000. Les mesures Linux et leurs limites sont
publiées dans [le bilan de cette préversion](docs/local-cpu-preview.md).
Elles ne prédisent pas la latence sur la machine Windows cible.

Le composant `local-agent/` charge directement **Qwen3-1.7B Q4_K_M**, avec le
raisonnement interne désactivé, et sert ACP sur son entrée/sortie standard.
Il prend en charge la conversation texte, le streaming, l’arrêt et la reprise
depuis un journal local. Toutes les mutations restent refusées dans ce pilote :
préparation, soumission et évaluation pédagogiques doivent encore être intégrées.
Le tuteur complet reste donc en développement ; voir
[le backlog H00–H12](../docs/backlog-local-harness.yaml).

Un message ordinaire déclenche une seule génération, sans connexion MCP dans
l’agent natif ; le client de bureau conserve sa synchronisation du contexte
pédagogique. Les commandes entières `/alertes` et `/contexte` activent des
parcours déterministes. La première lit les alertes ; la seconde ne consulte
ensuite le contexte de l’élève que si les indicateurs `has_critical` et
`needs_domain_setup` sont présents et valent tous deux `false`. `/aide` affiche
les commandes sans charger le modèle. Les demandes libres restent de la
conversation : elles ne déclenchent pas une découverte autonome des outils.

Rust choisit les outils et les arguments, vérifie leurs contrats, injecte les
identités prouvées par l’hôte et demande les permissions ACP sur les opérations
réelles. Le catalogue, les schémas et les décisions de routage restent hors du
prompt. Le journal conserve les résultats complets ; seules les valeurs exactes
sélectionnées par le code sont données au modèle pour une unique génération
finale. Une sélection indispensable dépassant le budget est refusée entière.
L’ancien harness de découverte reste disponible dans la bibliothèque pour les
expérimentations ; il n’est plus utilisé par le parcours ACP livré.

Le moteur utilise le couple `llama-cpp-2`/`llama-cpp-sys-2` **0.1.146** épinglé
par Goose. L’accès direct permet de compter le prompt avec le tokenizer exact
du modèle. Les poids restent chargés entre les générations ; chaque génération
dispose d’un nouveau KV cache limité par défaut à **2 048 tokens**. Le gestionnaire
de contexte réserve **192 tokens de sortie** et **256 de marge**, puis sélectionne
des tours entiers. Les omissions sont journalisées ; aucune donnée indispensable
n’est tronquée silencieusement. Il n’y a pas de réutilisation du KV entre tours.

Le client fournit un dossier privé `local-agent` dans les données du profil.
Chaque session possède un journal SQLite lié au profil, au compte, à
l’institution, au serveur et à l’empreinte du modèle. Les échanges originaux
restent distincts des projections : `session/load` restitue l’historique
conservé à l’interface, tandis que Qwen reçoit uniquement le contexte du travail
en cours. Le renouvellement de connexion utilise `_tutor/rebind` pour une
session déjà chargée, sans relecture de l’historique. Les jetons MCP restent
dans la configuration en mémoire, hors du journal et des prompts.

Pour l’agent local, le client transmet le message de l’apprenant sans lui
préfixer l’historique déjà journalisé. Il fournit séparément le contexte de
formation obtenu du serveur dans `_meta.tutorLocal.task` : `conversationId`,
`language`, `trainingContext`. Le harness vérifie sa correspondance avec la
conversation de la connexion MCP ; le modèle ne choisit pas ces identifiants.
Les agents externes conservent leur construction de prompt habituelle.

Pour un usage ACP direct, fournir `--data-dir CHEMIN_PRIVE_ABSOLU` et
`_meta.tutorLocal.owner` avec les identifiants stables vérifiés
`profileId`, `accountId`, `tenantId`, `serverId` lors de la création/reprise.
Sans propriétaire, une session de configuration reste temporaire. Le journal
n’est pas chiffré ; il utilise les protections du dossier applicatif. Les API
de rétention refusent de supprimer une mutation dont le résultat reste inconnu.
Le pilote MCP nécessite une session durable et exactement un serveur nommé
`tutor`. Les lectures passent par une liste de contrats examinés ; une
annotation `readOnlyHint` ne suffit pas à autoriser un outil. Un échec de
transport n’entraîne aucun nouvel envoi automatique.

La compilation de cet agent séparé demande **Rust 1.95.0**, CMake ≥3.25,
libclang et un compilateur C++. Le client de bureau conserve Rust 1.93.

```sh
cargo +1.95.0 build --manifest-path client/local-agent/Cargo.toml --locked --release
```

Le fichier GGUF accepté par défaut est identifié dans
[`model-profile.json`](local-agent/model-profile.json), avec sa révision et son
SHA-256. Il n’est pas inclus dans le dépôt. Son téléchargement dans l’application
est déclenché explicitement par le bouton des paramètres. Pour le récupérer
avec la CLI Hugging Face :

```sh
hf download unsloth/Qwen3-1.7B-GGUF Qwen3-1.7B-Q4_K_M.gguf --revision d7f544eead698dbd1f15126ef60b45a1e1933222 --local-dir CHEMIN_MODELES
```

Pour un lancement de développement, définir `TUTOR_LOCAL_AGENT_PATH` avec le chemin absolu
du binaire natif et `TUTOR_LOCAL_MODEL_PATH` avec celui du GGUF. La cible
**Qwen — local (expérimental)** apparaît dans les agents. Le mode initial est
CPU ; `TUTOR_LOCAL_THREADS` (1–32) et `TUTOR_LOCAL_BATCH` (1–512) permettent de
borner le travail. L’agent reçoit uniquement des arguments natifs, sans shell.

Les options de compilation `--features cuda` ou `--features vulkan` ajoutent
un backend GPU. `TUTOR_LOCAL_GPU_LAYERS` sélectionne ensuite 0–36 couches.
La P2000 nécessite une chaîne CUDA compatible Pascal, par exemple CUDA 12.9,
et une compilation ciblant `sm_61`. **Windows/P2000 n’est pas encore qualifié** ;
le profil CPU et les tests de protocole ne démontrent pas cette compatibilité.

Vérification sans téléchargement de modèle :

```sh
bash scripts/verify-local-agent.sh
```

Qualification explicite du modèle réel (les chemins doivent être absolus) :

```sh
python client/scripts/qualify-local-agent.py --agent CHEMIN_AGENT --model CHEMIN_GGUF --output CHEMIN_RAPPORT_JSON
```

Essai distinct de la chaîne Qwen → harness → permission ACP → lecture MCP :

```sh
python client/scripts/qualify-local-harness.py --agent CHEMIN_AGENT --model CHEMIN_GGUF --output CHEMIN_RAPPORT_HARNESS_JSON --timeout 1800
```

Cet essai utilise un serveur HTTP local de test avec trois outils. Il vérifie
dans le journal la sélection du contrat, l’appel autorisé, la lecture ciblée
du résultat et son inclusion dans le contexte de réponse. Il exige aussi une
réponse contenant le fait retourné par le serveur. Il ne qualifie pas les
parcours du vrai Tutor, le catalogue complet ni la qualité pédagogique.

Ce rapport sépare l’hôte réellement testé de la cible Windows/P2000. Le test
natif historique `exact_qwen_text_grammar_budget_and_cancellation`, ignoré par
défaut, vérifie aussi la grammaire avec `TUTOR_LOCAL_MODEL_PATH`. **Son calcul
sous grammaire JSON échoue avec ce profil** ; le bilan documente cet échec.
Les parcours MCP livrés ne délèguent aucune décision JSON au modèle. Pour
reproduire séparément ce test expérimental :

```sh
cargo +1.95.0 test --manifest-path client/local-agent/Cargo.toml --locked --release exact_qwen_text_grammar_budget_and_cancellation -- --ignored --nocapture
```

Les contrôles GitHub construisent l’exécutable CPU Windows, puis l’installateur
intégré et ses reçus liés au commit. Ils vérifient aussi l’interface installée.
Ils n’exécutent pas le GGUF réel et ne constituent pas une qualification GPU.

## Parcours disponibles

Les parcours pédagogiques ci-dessous utilisent les agents externes connectés
aux outils Tutor. Le pilote Qwen local reste limité aux capacités décrites plus
haut ; ses commandes ne préparent ni quiz ni évaluation.

- Connexions séparées à Codex et à Tutor, avec état réel et erreurs explicites.
- Détection de l’adaptateur et de Codex sur Windows et dans WSL2 ; chemin manuel.
- Accueil Conversation : écrire ce que vous souhaitez apprendre crée un programme personnel privé et permet de commencer immédiatement.
- Conception et tutorat avec les vrais outils MCP, formations et catalogue du
  serveur, conversation progressive, permissions explicites et interruption.
- Conservation des échanges sur l’appareil et reprise de la séance agent avec
  le même compte Tutor. Les réponses interrompues restent identifiées comme telles.
- Interface 0.0.4 conforme à la [charte Tutor-MCP](../ui/charte-tutor-mcp.html) :
  bleu nuit, accent bleu, wordmark à curseur et polices Inter/JetBrains Mono embarquées.
- Fenêtre sans barre de titre native, boutons personnalisés, espacements et lecture retravaillés.
- **Connexion et outils** réunit Tutor, l’identité, l’agent et les autorisations MCP.
- **Ctrl+K** ouvre la palette : recherche sans accents, conversations, formations,
  compte, réglages et thèmes. Les flèches et Entrée choisissent une commande ;
  Échap ferme la palette et Tab reste dans le dialogue.

Les programmes sont enregistrés dans Tutor dès le premier échange. La bibliothèque
Formations provient d’une projection serveur paginée et recherchable. Les inscriptions, les permissions et les observations
restent contrôlées par le serveur. Les estimations ne constituent pas une preuve
de maîtrise. Le client ne crée ni votes ni publication communautaire inexistants.

### Quiz, flashcards et Planning (0.0.4)

Dans la saisie, `/` ouvre le choix des modalités. `/quiz` prépare une question
avec réponses cliquables et correction après validation ; `/flashcards` prépare
une carte à retourner, suivie des choix **Pas retrouvé**, **Retrouvé avec effort**
et **Retrouvé facilement**. Un sujet peut suivre la commande. Les modalités
utilisent la formation active ; sans formation, le tuteur demande ou reprend
explicitement le sujet avant de préparer l’activité.

La destination **Planning** affiche les échéances déjà calculées par Tutor-MCP.
Activez une formation et choisissez son format ; **Réviser maintenant** ouvre
le chat, confirme la formation côté serveur et démarre directement sa révision.
Les préférences sont conservées sur l’appareil par compte, connexion, institution
et formation. Désactiver une formation dans ce planning ne désactive pas le
suivi pédagogique ni l’algorithme de répétition espacée du serveur.

Les réponses et leurs contextes restent dans l’historique. Les demandes hors
ligne restent **À envoyer**. Retirer une réponse en attente conserve un reçu
local, sans envoi au tuteur, pour empêcher de répondre à nouveau à une carte
déjà révélée. Une autoévaluation de flashcard reste distincte d’une réponse
observée ; les échéances et les preuves pédagogiques demeurent sous contrôle
du serveur. Aucun changement du serveur MCP n’est nécessaire.

Le contrat de présentation utilise des blocs JSON `tutor-activity`,
`tutor-study-request` et `tutor-response`, validés par le client. Ils ne sont
jamais exécutés comme HTML ou JavaScript. Les quiz conservent la tentative,
le type d’activité et la rubrique préparée avant la réponse pour permettre une
reprise sans perdre les critères d’évaluation. Une activité interrompue ou
invalide est masquée et ne révèle pas sa correction.

## Installation Windows et Codex

Le client ACP **0.0.4** est distribué comme asset de release avec un
[lien durable](https://github.com/ArnaudGuiovanna/tutor-mcp/releases/download/v0.7.3/Tutor_ACP_0.0.4_windows_x64-setup.exe).
Le workflow **Client → Client (Windows)** construit NSIS, vérifie les agents
embarqués, installe l’application et teste sa fenêtre ainsi que son arbre
UI Automation et la saisie au clavier. Le rapport est conservé avec l’artefact CI.

Les paramètres regroupent Connexion et outils, Institution, Membres et Ergonomie.
Tutor, l’identité nécessaire, l’agent et les autorisations se règlent dans le premier
onglet, sans aller-retour entre plusieurs pages. La langue
Français/English, les thèmes et les réglages de lecture sont conservés par compte.
Les autorisations MCP par outil sont liées au serveur et à l'agent sélectionnés ;
elles peuvent s'appliquer à toutes les conversations et être retirées ensuite.
Le [backlog de la version](../docs/backlog-tutor-experience.yaml) fournit les
critères de validation et les preuves de chaque lot.
La fenêtre sans barre native, les commandes de fenêtre, les espacements et les
interactions sont décrits dans la [fiche de finition du client](docs/desktop-experience.md).
Une compilation Linux ne qualifie pas Windows. Les installateurs de développement
ne sont pas signés avec un certificat commercial.

**Codex Desktop n’est pas requis.** Le processus lancé est l’adaptateur
`@agentclientprotocol/codex-acp`, qui embarque une dépendance Codex compatible.
Une installation Codex détectée est informative : la sélection automatique
utilise ce runtime fourni. En configuration manuelle, remplir **Chemin de Codex**
choisit explicitement ce CLI via `CODEX_PATH`, après vérification de son accès et
de `--version`. Sous Windows, sélectionner le binaire natif `codex.exe`, et non
un wrapper npm `.cmd`. Sous WSL, utiliser un chemin Linux absolu vers le CLI
exécutable. Laisser ce champ vide conserve le runtime fourni par l’adaptateur.
La négociation et les opérations réelles de l’agent vérifient ensuite la
compatibilité du couple choisi.

L’installateur inclut **Codex ACP 2.2.2**, **Claude ACP 0.95.0**, leurs
runtimes compatibles et **Node.js 24.13.0**. Aucune installation npm/Node séparée
n’est nécessaire pour ces deux agents. Choisissez l’agent, puis connectez votre
compte fournisseur. Le dossier de séance est préparé automatiquement. Une
installation WSL2 existante reste accessible dans les réglages avancés.

La connexion Claude ouvre le parcours officiel du fournisseur dans une fenêtre
séparée : Tutor ne collecte pas les identifiants Claude. Les dépendances système
requises par le fournisseur (notamment Git Bash pour les usages shell de Claude
sur Windows) restent celles de son CLI. Les outils intégrés de fichiers/terminal
de Claude sont désactivés dans les séances pédagogiques ; ses outils MCP restent
soumis aux permissions de Tutor. Le module vocal natif facultatif de Codex est
exclu de ce client textuel. Les exécutables conservés restent inchangés.

Pour reconstruire les ressources depuis le dépôt :

```powershell
python client/scripts/prepare-agents.py
rustup toolchain install 1.95.0 --profile minimal --component rust-docs
./client/scripts/prepare-local-agent.ps1
```

Le script utilise les archives npm verrouillées et le ZIP officiel Node vérifié
par SHA256. Les ressources générées sont ignorées par Git. Le workflow teste
les deux agents installés sans Node externe dans PATH. Le second script construit
l’agent Qwen CPU avec son runtime C++ lié statiquement, vérifie son lancement et
collecte ses notices. Il requiert les outils Visual Studio C++, CMake et LLVM
sur la machine de compilation. L’utilisateur de l’installateur n’en a pas besoin.

Pour WSL2, installer l’adaptateur **dans la distribution Linux concernée**, puis
relancer la détection. Le lancement conserve le même utilisateur Linux, retrouve
son environnement Codex, et passe par `wsl.exe`. Bash et `setsid` (`util-linux`)
sont nécessaires. Les chemins `~/...`, Linux absolus et Windows sont résolus dans
la distribution avant la création de la session ACP. Le nettoyage termine le
groupe de processus de cette séance, sans arrêter la distribution.

La connexion au fournisseur IA suit les méthodes annoncées par l’adaptateur.
Le compte ou la clé fournisseur reste dans l’environnement Codex sélectionné.
Les environnements Windows et WSL ne partagent pas automatiquement leurs accès.
La connexion fournisseur et son éventuel coût restent distincts du service Tutor.

## Connexion à Tutor

**Local** : indiquer le binaire Tutor et les arguments `--local`. Le binaire doit
être utilisable dans le même environnement que l’agent : chemin Linux pour WSL,
chemin Windows pour un agent natif. Pour choisir un profil, ajouter
`--data-dir CHEMIN_ABSOLU`. Plusieurs processus Tutor partagent ce profil durable.

**Institution** : créez une identité locale (ou importez une clé Nostr `nsec`),
enregistrez sa sauvegarde chiffrée sur un support sûr. Une fois le fichier écrit
et relu, la confirmation est automatique et le badge passe à **Sauvegardée**.
Les clés restent identiques après un redémarrage avant sauvegarde ; la clé de
récupération est alors temporairement protégée dans le coffre DPAPI. Si un ancien
client a fermé une identité non confirmée sans conserver cette clé, vérifiez le
fichier déjà exporté ; l’application ne remplace jamais silencieusement vos clés.
Une sauvegarde d’une autre identité est refusée.
Le bouton de connexion à Tutor affiche le prérequis restant lorsqu’il est grisé.
L’identité utilise secp256k1 et les signatures BIP340, comme Buzz. L’application
présente uniquement les clés publiques `npub` de l’identité et de sa récupération.
L’administrateur autorise ces deux clés publiques dans l’institution ; le client
ne crée pas de compte par e-mail et ne demande pas de mot de passe de compte.

Saisissez l’origine HTTPS de l’institution, sans `/mcp`, puis vérifiez son nom et
son origine. Le client découvre `/.well-known/tutor-institution` et signe un défi
NIP-98 unique valable 120 secondes. La clé privée reste dans Rust, protégée pour
l’utilisateur Windows par DPAPI. La variante Unix de développement utilise des
fichiers privés `0600`, sans prétendre les chiffrer sur le poste.

La sauvegarde portable est une enveloppe `tutor-identity-nip49` version 1 avec
**deux clés chiffrées séparément au format standard NIP-49 `ncryptsec`**. Scrypt
(logN 16, 64 Mio), normalisation NFKC et XChaCha20-Poly1305 sont implémentés par
la bibliothèque Nostr. La phrase chiffre la sauvegarde et ne sert pas à se
connecter. Conservez le fichier hors ligne et sa phrase séparément. La clé privée
de récupération ne reste pas dans le coffre actif après confirmation ; son
équivalent public permet à l’institution de vérifier une récupération future.
Avant cette confirmation, aucune session institutionnelle ne peut être ouverte.

Le serveur retourne une session révocable de 30 minutes. Aucun jeton permanent
de rafraîchissement n’est conservé : l’application signe un nouveau défi à
l’approche de l’expiration. Seul le jeton d’accès temporaire de la session MCP est
délégué à l’agent. La clé privée, la sauvegarde et sa phrase ne passent jamais dans
ses paramètres, son environnement ou ses prompts. Une déconnexion révoque la
session distante et retire son accès local ; les erreurs de révocation sont
signalées. Les anciens coffres OAuth sont conservés sans être réutilisés.

Le menu **Mon compte et mes rôles** affiche l’identité locale, l’institution et
les rôles renvoyés par le serveur. Créez ou sélectionnez plusieurs comptes locaux
sur le même client ; chacun possède son coffre, ses conversations et son dossier
de travail. Un changement termine l’agent et révoque l’accès Tutor du compte
précédent. Un compte déjà connecté retrouve l’institution par une nouvelle signature
lors de sa sélection. Le compte historique reste dans son dossier d’origine.

Les deux destinations principales sont **Conversation** et **Formations**.
La dernière conversation du compte est reprise à l’ouverture. Son historique
reste vertical dans la sidebar ; une nouvelle conversation est toujours accessible.
Le compte, l’identité, les agents et l’institution ont leurs propres pages.
Les anciennes intentions sont conservées dans les notes historiques du compte.

Écrire « Je veux apprendre le Go » recherche d’abord un programme existant,
puis enregistre un programme personnel privé et ses concepts si nécessaire.
Aucun formulaire d’intention n’est requis. Un propriétaire ou formateur qui
commence à apprendre obtient son espace apprenant sans modifier ses droits d’équipe.
Les permissions de session utiles sont demandées avec la même identité.
Une configuration incomplète conserve le message **À envoyer** sur l’appareil ;
la connexion se termine dans cette fenêtre, puis le message reprend une seule fois.

Chaque conversation possède un contexte serveur avec sa formation active.
Les changements sont explicites et les tours conservent leur référence pédagogique.
Un domaine différent est refusé tant qu’il n’a pas été sélectionné dans cette
conversation. Les changements d’institution ou de compte ne rejouent aucun échange.

La fiche formation remplace la zone centrale et propose **Programme**,
**Progression**, **Partage et accès**. L’édition manuelle et **Modifier avec l’agent**
utilisent le même éditeur et la même prévisualisation serveur. Un conflit conserve
le texte à examiner ; il ne remplace pas silencieusement la version récente.
**Valider les modifications**, **Soumettre au propriétaire** et **Publier dans
l’institution** sont des actions distinctes.

Partager un programme personnel crée une copie de sa conception. Les échanges
et la progression privés restent séparés. Les contributions requièrent une
validation du propriétaire à chaque révision, sauf publication libre explicitement
autorisée pour cette formation. **Approuver et publier** est atomique.
Les offres nominatives restent cachées aux non-invités. « Tous les membres »
désigne uniquement les membres de l’institution. Les versions communes publiées
sont immuables et les inscrits conservent leur version jusqu’à une migration explicite.

Le texte de conversation mesure 20 px, avec interligne 1,65 et taille réglable
de 16 à 24 px dans le compte. Le code revient à la ligne et sa copie conserve
les caractères originaux. Les tableaux trop larges deviennent verticaux.
Les réglages et formations utilisent la même surface sombre. Voir le
[contrat et les captures de référence](../docs/tutor-conversation-formations.md).

La rotation exige l’approbation de la clé de récupération actuelle, lue
temporairement depuis la sauvegarde hors ligne. Une clé active volée ne peut donc
pas remplacer à elle seule la récupération. La rotation crée une nouvelle paire et son fichier de sauvegarde avant de
révoquer l’ancienne paire dans l’institution sélectionnée. La récupération signe
avec l’ancienne clé hors ligne et installe une paire neuve. Gardez les deux
sauvegardes jusqu’à la reconnexion réussie ; une erreur réseau après envoi peut
rendre l’issue distante incertaine. Un échec d’écriture locale n’efface pas
l’ancien coffre. Une récupération confirmée impose une nouvelle connexion.

### Identité depuis le terminal

Le binaire Rust `tutor-client-identity` est utilisable sans interface graphique.
Les secrets sont lus **sur stdin**, jamais dans les arguments ni les sorties.
Utilisez un gestionnaire de secrets ou un terminal privé (la saisie stdin brute
peut être visible dans un terminal). Exemple, phrase fournie par votre outil de
secrets :

```bash
cargo run --manifest-path client/src-tauri/Cargo.toml --no-default-features \
  --bin tutor-client-identity -- identity generate \
  --directory /chemin/prive/coffre --file /support-hors-ligne/identite.json
```

Sous-commandes : `generate` crée la paire et peut exporter/confirmer avec `--file` ;
`exportpub` exige une sauvegarde confirmée ; `backup --file FICHIER_NEUF` termine
une création non sauvegardée ; `import --file FICHIER` restaure une sauvegarde
(phrase sur stdin) ; `import --nsec` importe la clé privée lue sur stdin puis
exige une sauvegarde. Une identité différente ne remplace jamais implicitement
un coffre existant. Chaque commande affiche seulement l’état et les clés publiques.

Les intentions et conversations sont enregistrées dans `workspace.json`, dans
le dossier de données utilisateur Tauri (`fr.aguiovanna.tutor`). Elles peuvent
contenir des informations personnelles. Le fichier du coffre est séparé. Une
corruption est signalée et conservée pour récupération ; elle n’est pas effacée
par une réinitialisation automatique.

Pour sauvegarder l’espace local, fermer Tutor puis copier ce dossier dans un
emplacement privé. Le coffre DPAPI reste lié à l’utilisateur Windows qui l’a
créé : copier le dossier sur un autre compte ne transfère pas automatiquement
les accès. Les données pédagogiques du serveur suivent ses propres procédures
de sauvegarde et de restauration.

## Développement

Préparer Node.js 24, Rust 1.93 et les
[prérequis Tauri pour Windows](https://v2.tauri.app/start/prerequisites/), dont
les outils C++ MSVC et WebView2. Depuis ce dossier :

```powershell
npm ci
npm run tauri -- dev
```

Pour construire l’installateur, préparer les ressources des agents puis générer les notices selon le
[guide de distribution](docs/distribution-notices.md),
puis lancer `npm run tauri -- build --bundles nsis`.

Les commandes natives Rust portent la connexion, les processus et la persistance.
L’interface React/TypeScript appelle uniquement le bridge Tauri. Le navigateur de
prévisualisation affiche l’interface mais ne lance pas un agent ni un faux service.

Depuis la racine du dépôt, la vérification commune est :

```bash
bash scripts/verify-client.sh --install --real-adapter
```

Elle inclut les tests d’interaction, le build web, Rustfmt, Clippy, les tests du
cœur natif et une négociation avec l’adaptateur réel sans appel de modèle. Les
fixtures de protocole restent dans `tests/` et ne sont pas empaquetées avec l’UI.
La [qualification du runtime](docs/runtime-qualification.md) distingue les essais
natifs, WSL et les appels réels de modèle ; les tests synthétiques ne qualifient
pas à eux seuls un tutorat réel.

Les profils `local`, `hobby` et `institution` du serveur restent disponibles.
Un changement de connexion impose une reprise explicite ; une séance d’un compte
ne peut pas être rejouée dans un autre compte ou une autre institution.
