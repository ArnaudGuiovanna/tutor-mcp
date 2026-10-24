# Qualification du client

Le client est construit pour Windows, avec Codex accessible par l’adaptateur
ACP. Les tests distinguent le protocole, le serveur pédagogique, l’interface
installée et l’utilisation réelle d’un modèle. Le succès d’une de ces étapes
ne vaut pas qualification des autres.

## Vérification reproductible

Depuis la racine du dépôt, avec Node.js, Rust et Python 3.11+ installés :

```bash
bash scripts/verify-client.sh --install --real-adapter
```

Le script installe les dépendances verrouillées, vérifie TypeScript, teste et
construit l’interface, vérifie le format et Clippy, puis teste le cœur Rust sans
dépendance à une fenêtre. L’option `--real-adapter` négocie réellement ACP avec
le paquet de `client/scripts/adapter/package-lock.json`. Le test utilise un
profil Codex temporaire : il ne copie pas de connexion et n’appelle aucun modèle.

Le contexte GitHub **Client (Windows)** ajoute la compilation de Tutor,
l’exécution des outils MCP réels, la construction de l’installeur NSIS,
l’installation et l’ouverture d’une fenêtre de l’application. Il conserve
l’installeur et un rapport lié au commit. L’apparition d’une fenêtre n’établit
pas à elle seule la qualité de toutes les interactions graphiques.

## Windows natif

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

La réception fonctionnelle exige aussi, avec le fournisseur choisi par la
personne, une intention validée, une formation effectivement enregistrée par
les outils Tutor, une activité, une interruption et une reprise sans duplication.
L’annulation et le refus d’une demande de permission doivent rester utilisables.
Une négociation ACP seule, une fixture de protocole ou un outil MCP invoqué
directement ne prouvent pas ce parcours. Les rapports du probe l’indiquent par
`modelPromptTested: false` et `desktopUiTested: false`.

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
