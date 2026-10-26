# Qualification du client

La recette du client **0.0.2** couvre Codex et Claude par leurs adaptateurs ACP.
Elle distingue le lancement du processus, le protocole, le serveur pédagogique,
l’interface Windows installée et l’utilisation réelle d’un modèle. Le succès
d’une de ces étapes ne vaut pas qualification des autres.

Ce document décrit les contrôles implémentés et leurs limites. Une étape présente
dans le workflow ne constitue pas un résultat CI : sa réussite doit être constatée
sur le commit concerné et reliée à ses rapports bruts.

| Preuve | Périmètre | Limite |
|---|---|---|
| `tutor-packaged-rust.json` | Les deux agents Windows passent par `Backend::connect_agent`, le lancement Rust utilisé par l’application ; premier lancement, reprise et arrêt. | Négociation ACP sans génération ; le parcours d’identification fournisseur n’est pas exercé. |
| `tutor-client-accessibility.json` | Application installée depuis NSIS : noms UI Automation, clavier, palette Ctrl+K, quatre onglets, changement FR/EN/FR, sélection des thèmes et persistance après fermeture du processus. | Profil de test hors ligne, sans séance MCP ni audit manuel de Narrator. |
| `tutor-client-runtime.json` | Découverte native de l’adaptateur, négociation ACP et, avec `-TutorBinary`, persistance d’appels MCP directs. | Les appels directs ne prouvent pas leur utilisation par un modèle. |
| `tutor-client-wsl-runtime.json` | Découverte et exécution dans une véritable distribution WSL2 depuis Windows, arrêt des processus et choix du CLI. | Ne qualifie ni l’interface graphique Linux ni une génération de modèle. |
| Rapport distinct d’une séance avec fournisseur authentifié | Réponses réelles et appels MCP dans le profil pédagogique de test, avec OS, versions et données de test identifiés. | Un essai Linux avec données synthétiques ne qualifie pas l’exécutable Windows installé. |

## Vérification reproductible

Depuis la racine du dépôt, avec Node.js, Rust et Python 3.11+ installés :

```bash
bash scripts/verify-client.sh --install --real-adapter
```

Le script installe les dépendances verrouillées, vérifie TypeScript, teste et
construit l’interface, vérifie le format et Clippy, puis teste le cœur Rust sans
dépendance à une fenêtre. L’option `--real-adapter` négocie réellement ACP avec
les deux paquets Codex et Claude verrouillés dans
`client/scripts/adapter/package-lock.json`. Le test isole `CODEX_HOME` et
`CLAUDE_CONFIG_DIR` dans un dossier temporaire ; il ne copie pas de connexion
fournisseur et n’appelle aucun modèle.

Le contexte GitHub **Client (Windows)** ajoute la qualification des agents
intégrés par Rust, la compilation de Tutor, les outils MCP directs, la construction
NSIS, l’installation et les interactions UI Automation. Le
[workflow](../../.github/workflows/client.yml) prévoit de conserver l’installeur
et les quatre rapports nommés ci-dessus dans l’artefact
`tutor-client-windows-x64-<commit>`. La présence et la réussite de ces contrôles
doivent être vérifiées pour le commit livré.

## Windows natif : découverte et MCP direct

Dans PowerShell, après installation des dépendances verrouillées :

```powershell
./client/tests/windows/qualify-runtime.ps1 `
  -Environment native `
  -Adapter 'C:\chemin\tutor-mcp\client\scripts\adapter\node_modules\.bin\codex-acp.cmd' `
  -TutorBinary 'C:\chemin\tutor-mcp.exe' `
  -Report '.\native-qualification.json'
```

Ce programme réutilise les modules de découverte, de lancement et de transport
du client. L’option `-TutorBinary` ajoute un profil Tutor temporaire : création
d’une formation, rejeu idempotent, ouverture d’une séance, arrêt et redémarrage,
puis vérification des objets persistés. Ces appels MCP sont directs ; ils ne
prouvent pas que Codex les invoque correctement pendant un tutorat.

Le lancement JavaScript conserve les chemins canoniques pour vérifier les
limites du paquet npm, puis transmet à Node le chemin Windows ordinaire quand
celui-ci désigne exactement le même fichier. Cette conversion contourne une
[erreur du chargeur Node sur les entrées `\\?\C:\…`](https://github.com/nodejs/node/issues/62446),
présente notamment dans la version 24.13.0 utilisée par la CI. Le
[correctif amont](https://github.com/nodejs/node/pull/65378) décrit la résolution
incorrecte de la racine du lecteur. Les tests Windows lancent réellement un
script canonique avec espaces et Unicode et vérifient que les noms étendus
distincts restent inchangés.

Les exécutables de tests Tauri sous MSVC reçoivent aussi le manifeste Windows
Common-Controls v6. Le `build.rs` applique la
[solution utilisée par Tauri](https://github.com/tauri-apps/tauri/issues/13419)
à tous les artefacts, car le manifeste ajouté aux seuls binaires applicatifs
ne couvre pas `cargo test --lib`. Sans lui, le chargeur peut retourner
`STATUS_ENTRYPOINT_NOT_FOUND` avant le premier test. Le fichier
`client/src-tauri/windows-app-manifest.xml` conserve intégralement le manifeste
par défaut de `tauri-build 2.7.4` ; sa comparaison fait partie de la revue lors
d’une mise à jour de cette dépendance.

## Agents Windows intégrés : même lancement Rust que l’application

La vérification Python des ressources installées reste utile pour contrôler
l’inventaire et une négociation ACP. Elle ne remplace pas le test du lancement
Rust : c’est précisément la différence de chemin de lancement qui pouvait
laisser passer une erreur Windows dans les agents intégrés.

Depuis PowerShell sous Windows, à la racine du dépôt :

```powershell
python client/scripts/prepare-agents.py
if ($LASTEXITCODE -ne 0) { throw 'Préparation des agents refusée.' }
$env:TUTOR_PACKAGED_ROOT = (Resolve-Path 'client/bundled-agents').Path
$env:TUTOR_PACKAGED_REPORT = Join-Path (Get-Location) 'tutor-packaged-rust.json'
cargo test --manifest-path client/src-tauri/Cargo.toml --locked `
  --no-default-features --lib `
  installed_bundled_agents_use_the_same_rust_launch_as_the_application `
  -- --ignored --nocapture
if ($LASTEXITCODE -ne 0) { throw 'Le lancement Rust des agents a échoué.' }
if (-not (Test-Path -LiteralPath $env:TUTOR_PACKAGED_REPORT)) {
  throw 'Le rapport brut de qualification Rust est absent.'
}
```

Le test appelle `Backend::connect_agent` pour Codex et Claude, avec la racine
canonique des ressources et un dossier de travail contenant espaces et Unicode.
Il vérifie `initialize`, la déconnexion, puis une deuxième connexion et son
arrêt. Le workflow utilise les ressources Windows préparées avant la construction
NSIS ; le contrôle Python de l’installation complète cette preuve sur les
fichiers effectivement installés.

`tutor-packaged-rust.json` enregistre `sourceVersion`, `platform` et deux entrées
`agents` contenant notamment `agentInfo`, `coldStartMs`, `warmStartMs`,
`productionRustBackend`, `initialize`, `shutdown` et `modelPromptTested: false`.
Les durées mesurent la connexion ACP, pas la génération d’une réponse. Le cache
du paquet est partagé dans le processus : le premier lancement de chaque agent
ne constitue pas nécessairement une mesure avec cache disque froid indépendant.
Le rapport est relié au commit par le workflow et le nom de son artefact.

## Interface Windows installée et persistance après fermeture

La recette utilise l’installeur **0.0.2**, pas une prévisualisation web. Elle doit
être exécutée sur un compte Windows de test sans données Tutor existantes : le
script refuse de remplacer un profil présent et ne nettoie que ses propres
dossiers de test.

```powershell
$env:GITHUB_SHA = (git rev-parse HEAD).Trim()
./client/tests/windows/test-installed-client.ps1 `
  -Installer 'client/src-tauri/target/x86_64-pc-windows-msvc/release/bundle/nsis/Tutor_0.0.2_x64-setup.exe' `
  -Report (Join-Path (Get-Location) 'tutor-client-accessibility.json')
```

Le profil semé pour ce test est hors ligne, sans URL ni commande Tutor et sans
identité native. Le script UI Automation tape un message sans l’envoyer, vérifie
les noms accessibles et la navigation clavier. Il ouvre la palette avec Ctrl+K,
recherche « ergonomie », valide par Entrée et contrôle l’onglet sélectionné.
Il parcourt ensuite les quatre onglets
**Agents / Institution / Membres / Ergonomie**. Il change la langue
**Français → English → Français**, vérifie les libellés des paramètres dans les deux langues et
sélectionne les thèmes clair et sombre avec les contrôles accessibles.

Il laisse ensuite la langue anglaise et le thème clair, ferme proprement le
processus et relance le même exécutable avec le même profil, sans réinitialiser
les données. Une seconde passe vérifie ces valeurs dans l’interface et dans le
fichier de travail réellement enregistré avant de revenir au français et au
thème sombre.

Le rapport brut `tutor-client-accessibility.json` contient les preuves des deux
passes dans `exercise` et `reopened`, ainsi que `sourceCommit`, `sourceVersion`,
`binaryProductVersion`, `settingsTabs`, `languageSwitchFrEnFr`,
`commandPaletteKeyboardNavigation`, `themeSwitchLightDark`, `persistenceAfterProcessRestart` et
`persistedValuesVerifiedInWorkspace`. Le workflow exige ce fichier à l’emplacement
explicite sous `RUNNER_TEMP`, contrôle son commit et ses indicateurs avant de
l’ajouter à l’artefact. Une ligne de log ou un récépissé reconstruit ne remplace
pas ce JSON brut.

UI Automation vérifie ici les commandes et valeurs accessibles, pas à elle seule
le contraste visuel, le comportement de tous les lecteurs d’écran ou une séance
de tutorat. `narratorManualAudit: false` reste explicite. Les captures de navigateur
avec un pont IPC simulé peuvent compléter les contrôles de rendu, de zoom et de
défilement ; elles ne constituent pas une exécution de l’application Windows
installée ni une preuve d’utilisation du serveur pédagogique.

## WSL2 réel

Une machine Windows disposant déjà d’une distribution WSL2 et de l’adaptateur
Linux peut exécuter :

```powershell
./client/tests/windows/qualify-runtime.ps1 `
  -Environment wsl -Distribution Ubuntu `
  -AgentDirectory '/home/alice/mes cours' `
  -Report '.\wsl-qualification.json'
```

Ajouter `-Adapter '/chemin/linux/codex-acp'` si la détection ne trouve pas
l’installation. Le programme démarre l’agent dans la distribution et sous
l’utilisateur détectés, négocie ACP et arrête sa propre session. Il n’installe
pas WSL, ne change pas la distribution par défaut et ne termine pas les autres
processus de la distribution. Ce probe utilise l’environnement Codex de cet
utilisateur Linux, sans afficher ses identifiants.

Par défaut, le CLI détecté reste informatif et l’adaptateur utilise sa dépendance
compatible. Ajouter `-CodexBinary '/chemin/linux/codex'` vérifie `--version` puis
sélectionne explicitement ce CLI. Sous Windows natif, cette option exige un
`codex.exe`, pas un wrapper npm `.cmd`. Le rapport distingue ces choix avec
`codexRuntime: "adapter-bundled"` ou `"explicit-cli"`.

Le contexte **Client (Windows)** exécute également cette découverte depuis
Windows Server 2025 vers une véritable distribution WSL2 éphémère. Il importe
une image Ubuntu 24.04 officielle et un Node Linux dont les empreintes SHA256
sont épinglées dans `qualify-wsl-ci.ps1`, installe l’adaptateur avec le lockfile,
puis crée un utilisateur Linux ordinaire `tutorci`. L’agent et son CLI doivent
être trouvés sans chemin d’adaptateur forcé. Le test vérifie le pont stdio,
la négociation ACP et l’absence de processus de l’agent après arrêt. Seule la
distribution créée par ce test est supprimée ; aucune distribution existante
n’est reconfigurée ou arrêtée.

La fixture vérifie aussi la sélection manuelle du CLI déjà installé. Un wrapper
de traçage transmet tous les arguments au véritable Codex et écrit un marqueur
uniquement quand l’adaptateur le lance comme `app-server`. La négociation ACP
reste réelle : aucune réponse de protocole n’est simulée. Le marqueur doit être
absent en mode automatique et présent après sélection explicite.

Le rapport `tutor-client-wsl-runtime.json` conservé par la CI indique le système
hôte, la distribution, l’utilisateur Linux, les chemins réellement détectés,
la version du CLI et de l’adaptateur, les empreintes de la fixture et le résultat
du nettoyage. L’étape doit réussir pour rendre **Client (Windows)** vert.
Une installation WSL1 ne constitue pas un repli admissible. Les sources de
l’[image Windows 2025](https://github.com/actions/runner-images/blob/main/images/windows/Windows2025-Readme.md)
annoncent WSL2 ; son fonctionnement est vérifié par l’import et l’exécution,
plutôt que déduit de la seule présence du paquet.

Les tests Linux de parsing et du wrapper ne constituent pas un test de WSL2.
Seul un rapport d’exécution Windows/WSL2 réussi pour le commit concerné prouve
cette qualification ; la présence du scénario dans le dépôt ne suffit pas.

## Parcours avec un modèle

La réception fonctionnelle commence par une demande dans Conversation, par
exemple « Je veux apprendre le Go ». Elle n’exige aucun formulaire d’intention ni
validation préalable d’un brouillon. Les anciennes intentions restent des notes
historiques ; elles ne sont pas une étape du parcours.

Avec le fournisseur authentifié choisi par la personne, la recette doit prouver
la création immédiate d’une formation personnelle privée, sa sélection confirmée
sur le serveur, une activité, une interruption et une reprise sans duplication.
Elle couvre aussi trois créations personnelles successives et le parcours
Go → Python → fermeture → reprise → Go, avec progression et contexte distincts.
Les prochaines réponses suivent le choix français/anglais ; les messages et les
programmes historiques ne sont pas réécrits. L’annulation, le refus et les choix
d’autorisation persistants doivent rester effectifs après reprise.

Une génération réelle sous Linux avec des CLI fournisseurs authentifiés et un
Tutor éphémère contenant des données synthétiques prouve les étapes effectivement
tracées dans ce contexte. Une réponse ne valide pas à elle seule toute cette
recette : le rapport doit montrer les outils et objets réellement créés.
« Synthétiques » décrit ici les données de test, pas des réponses de modèle
simulées. Ce résultat doit rester séparé du lancement ACP des agents Windows et
de la recette UI Automation installée : ni l’un ni l’autre ne prouve une génération
de modèle dans l’exécutable Windows. Aucun compte ni historique du pilote n’est
nécessaire pour ces essais isolés.

Une négociation ACP seule, une fixture de protocole ou un outil MCP invoqué
directement ne prouve pas une séance générée par le modèle. Les rapports natifs
et WSL du probe conservent `modelPromptTested: false` et
`desktopUiTested: false` ; le rapport Rust des agents intégrés conserve également
`modelPromptTested: false`. Une validation avec modèle doit apporter sa propre
preuve, en indiquant explicitement le système d’exploitation, les versions,
le commit et les limites du scénario.

## Portée du retrait de compte et de l'effacement

Retirer un compte de cet appareil et demander l'effacement de ses données dans
une institution sont deux opérations distinctes. La demande d'effacement du
profil apprenant est asynchrone : l'interface affiche l'état réellement retourné
par le serveur, sans annoncer une suppression terminée dès son acceptation.
Une conservation légale ou le statut de dernier propriétaire peut empêcher son
exécution. Sans profil apprenant, le retrait révoque immédiatement l'adhésion et
ses sessions. Le dernier propriétaire doit transférer la propriété auparavant.

Ces opérations ne suppriment pas l'identité globale ni ses clés publiques sur
le serveur et ne retirent pas les accès du même compte dans d'autres institutions.
Les versions communes et les progressions des autres apprenants sont conservées.
Les tests serveur d'effacement et de contrôle des droits ne constituent pas,
à eux seuls, une recette de suppression dans l'application Windows installée.

## Revue des dépendances Rust

L’audit du 24 octobre 2026 avec `cargo-audit 0.22.2` et la base RustSec au commit
`21d909e4c8bd4e14c7aea58bf980c881a0053ab1` a retourné zéro avis dans la catégorie
`vulnerabilities`, quatre avis `unmaintained` et un avis `unsound` :

| Paquet verrouillé | Avis | Catégorie |
|---|---|---|
| `proc-macro-error 1.0.4` | RUSTSEC-2024-0370 | Non maintenu |
| `zbus_names 4.3.5` | RUSTSEC-2026-0371 | Non maintenu |
| `zvariant 5.15.1` | RUSTSEC-2026-0370 | Non maintenu |
| `zvariant_derive 5.15.1` | RUSTSEC-2026-0372 | Non maintenu |
| `glib 0.18.5` | RUSTSEC-2024-0429 | Sûreté mémoire |

Ces cinq couples paquet/version sont absents du graphe accessible avec
`cargo metadata --filter-platform x86_64-pc-windows-msvc --no-default-features
--features desktop`. Cette vérification a parcouru les 307 paquets accessibles
du verrou SHA-256 `fe9662d40ae410d444333ce026fd0db531f512200dceffbf469a6010ba36e3e0`.
Ils restent présents dans le verrou multiplateforme ; aucun avis n’a été masqué
par une règle d’exclusion.

L’avis `glib` décrit un problème de sûreté mémoire dans les itérateurs
`VariantStrIter`, corrigé à partir de `glib 0.20.0`. La chaîne GTK utilisée pour
une future distribution graphique Linux devra être examinée et mise à jour
avant cette qualification. Les tests Linux du cœur sans `desktop` et la
qualification Windows ne valident pas une application graphique Linux.
[Avis RustSec](https://rustsec.org/advisories/RUSTSEC-2024-0429.html).

Ce résultat daté ne signifie ni absence de tout avertissement ni garantie pour
une autre version du verrou ou une autre cible. Une mise à jour des dépendances
demande de réexaminer le graphe et les avis correspondants.
