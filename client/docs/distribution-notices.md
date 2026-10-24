# Notices distribuées avec le client

L’installeur Windows doit conserver le dossier `THIRD_PARTY_NOTICES` à côté de
l’application. Il contient la licence de Tutor, les textes tiers originaux et
un inventaire de leurs empreintes. La création des notices précède la construction
de l’installeur ; le test d’installation vérifie ensuite les fichiers réellement
installés.

```bash
cargo fetch --manifest-path client/src-tauri/Cargo.toml --locked --target x86_64-pc-windows-msvc
python3 client/scripts/collect-notices.py collect --target x86_64-pc-windows-msvc
python3 client/scripts/collect-notices.py verify-tree client/THIRD_PARTY_NOTICES
```

Le collecteur nécessite Python 3.11 ou plus récent. Il ne télécharge pas de
licence, n’interprète pas une expression SPDX comme le texte d’une licence et
ne remplace pas une attribution manquante par un modèle générique. Un composant
sans texte original identifiable bloque la construction. Un dossier de sortie
existant n’est remplacé qu’après vérification de son inventaire et préparation
complète du nouveau dossier.

Les notices du runtime demandent le composant du même outil Rust :
`rustup component add rust-docs`. La CI l’installe explicitement, y compris avec
un profil Rust minimal.

## Périmètre conservé

- Dépendances npm de production verrouillées et transitives, plus Vite pour
  son aide `modulepreload` embarquée dans le frontend.
- Graphe Cargo accessible depuis le client avec la fonctionnalité `desktop`,
  filtré pour la cible Windows. Ce graphe inclut un surensemble de dépendances
  de construction et de test ; il ne constitue pas une liste exacte des objets
  retenus par l’éditeur de liens.
- Notices du runtime Rust : `COPYRIGHT-library.html` et les textes associés du
  même outil `rustc` que celui utilisé pour la construction.
- Chargeur statique Microsoft WebView2, avec les textes `LICENSE.txt` et
  `NOTICE.txt` du SDK dont ses octets proviennent.
- NSIS et son extension Tauri, avec leurs licences originales et les empreintes
  des exécutables, extensions et du stub de décompression utilisés.

Les fichiers `LICENSE`, `NOTICE`, `COPYRIGHT`, `AUTHORS` et `PATENTS`, y compris
ceux des sous-dossiers, sont copiés. Les commentaires légaux des sources sont
également conservés sans reformulation dans `SOURCE-NOTICES.txt`. L’inventaire
indique versions, cibles, fonctionnalités Cargo, sources, notices, empreintes
des verrous et empreintes du frontend construit.

## Archives sans licence incluse

Certains paquets publiés omettent la licence qui se trouve à la racine de leur
dépôt. Les suppléments suivis sous `client/scripts/notices-supplements/`
conservent ces originaux et leur provenance dans `provenance.json`. Le collecteur
exige que la version et le commit `.cargo_vcs_info.json` du paquet correspondent
au supplément examiné. Une mise à jour de dépendance ne réutilise donc pas
silencieusement le texte d’une autre version.

Pour `selectors 0.38.0`, les en-têtes de l’archive désignent explicitement
[MPL 2.0 chez Mozilla](https://www.mozilla.org/MPL/2.0/). Le supplément conserve
le texte officiel référencé ; les en-têtes et l’archive source originale sont
aussi livrés.

## Disponibilité des sources MPL

Les archives `.crate` des composants MPL 2.0 sont incluses sous `sources/`.
Leur empreinte doit correspondre à `Cargo.lock`. Elles contiennent les sources
originales, sans modification, et peuvent être extraites avec un outil compatible
tar/gzip. L’inventaire indique l’archive de chaque composant. L’utilisateur n’a
pas besoin de contacter l’opérateur pour obtenir ces sources.

## WebView2 et dépendances externes

Le chargeur WebView2 est distinct du navigateur WebView2 Runtime. Le collecteur
vérifie le chargeur statique contre les empreintes du SDK Microsoft
`1.0.3800.47` : les bibliothèques distribuées dans `webview2-com-sys 0.39.1`
correspondent aux fichiers du paquet NuGet officiel. Les conditions du chargeur
et ses attributions tierces sont copiées intégralement.

Le WebView2 Runtime reste fourni par Microsoft. Avec le mode Tauri par défaut,
l’installeur télécharge le programme d’installation Microsoft si nécessaire ;
le client ne distribue pas un runtime fixe de navigateur. Passer à un mode
embarquant le bootstrapper, le runtime hors ligne ou un runtime fixe exige de
revoir les éléments distribués et leurs conditions. [Installation WebView2 avec
Tauri](https://v2.tauri.app/distribute/windows-installer/#webview2-installation-options).

L’adaptateur ACP et Codex sont installés séparément dans cette version. Le verrou
de `client/scripts/adapter/` sert à qualifier une version reproductible ; ses
binaires ne sont pas incorporés à l’installeur Tutor. Les prérequis du système
Windows et les outils installés séparément gardent leurs propres distributions.

## Installeur NSIS

La version verrouillée de Tauri CLI `2.12.4` sélectionne NSIS `3.13` et
`nsis-tauri-utils 0.5.3`. Leurs archives et bibliothèques ont été récupérées
depuis les versions amont ; leurs empreintes correspondent aux constantes de
la version de Tauri utilisée. Leurs textes de licence originaux sont conservés
dans les suppléments, y compris les licences des interfaces et extensions NSIS.

Le client choisit explicitement la compression **zlib**. Le texte `COPYING`
de NSIS couvre aussi d’autres composants, dont LZMA sous CPL 1.0. Cette version
du client utilise le stub zlib ; modifier la compression exige de revoir les
sources et obligations supplémentaires, au lieu de présenter le dossier actuel
comme couvrant cette nouvelle distribution.

Après la construction de l’installeur, la CI compare le cache d’outils réellement
utilisé avec les empreintes de l’inventaire :

```powershell
python client/scripts/collect-notices.py verify-installer-tools `
  --inventory client/THIRD_PARTY_NOTICES `
  --cache "$env:LOCALAPPDATA/tauri/NSIS"
```

L’extension Tauri est une bibliothèque précompilée amont. Son commit de source
et son empreinte sont conservés ; ce dépôt amont ne publie pas de verrou de ses
dépendances. Les termes originaux de ses familles de dépendances d’exécution
`windows-sys` et `semver` sont également fournis dans le graphe du client. Cet
inventaire ne prétend pas reconstituer des versions de compilation non publiées.
