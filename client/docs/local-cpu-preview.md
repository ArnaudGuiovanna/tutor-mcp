# Préversion locale CPU : petit modèle et parcours fixes

Le profil 0.0.7 utilise **Qwen3-1.7B Q4_K_M**, fichier de 1 107 409 472
octets, à la place du 4B Q4_K_M de 2 497 281 120 octets (environ 56 % de
poids en moins). Révision, empreinte SHA-256 et licence Apache-2.0 sont épinglées
dans `local-agent/model-profile.json` et les notices. Le chargeur vérifie aussi
l'architecture, les dimensions et le type de quantification.

Le raisonnement interne est désactivé par le template. Le profil reprend les
[paramètres non-thinking de Qwen](https://huggingface.co/Qwen/Qwen3-1.7B#best-practices) :
température 0,7, top-k 20, top-p 0,8, min-p 0, avec une graine fixe de 42 pour
reproduire les essais. Ces paramètres ne prennent aucune décision d'autorisation.
Cette préversion cherche à réduire la latence ; elle ne qualifie pas le modèle
comme tuteur autonome. Une réponse libre n'est jamais une évaluation validée.

## Utilisation

- Un message ordinaire déclenche une seule génération, sans connexion MCP dans
  l’agent natif. Le client conserve sa synchronisation du contexte pédagogique.
- `/alertes` (ou `/alerts`, « Lis mes alertes ») consulte les alertes.
- `/contexte` (ou `/context`) consulte d'abord les alertes, puis le contexte de
  l'élève seulement si `has_critical` et `needs_domain_setup` valent `false`.
  Un indicateur absent ou mal formé bloque cette deuxième lecture.
- `/aide` présente ces commandes sans charger le modèle.

Les commandes sont entières, sans paramètres libres. Une mention de `/alertes`
dans une phrase ne déclenche aucun outil. Les autres demandes en langage naturel
restent de la conversation ; la découverte autonome de tout le catalogue ne fait
pas partie de ce profil. Les activités et évaluations ne sont pas activées.

Rust choisit les outils, valide leurs contrats et arguments, injecte seulement
les identités prouvées par l'hôte et conserve les autorisations ACP. Un refus ou
une annulation empêche l'envoi. Aucun nom d'outil, schéma ou pointeur de résultat
n'est choisi par le modèle. Le catalogue reste hors contexte. Après la lecture,
une seule génération résume les sélections exactes du journal.

Le journal conserve les échanges et les reçus complets. Le contexte est projeté
avec le tokenizer exact, dans 2 048 tokens dont 192 réservés à la réponse et 256
de marge. Les omissions d'historique sont enregistrées. Une sélection obligatoire
ne tenant pas dans le budget est refusée entière, jamais tronquée silencieusement.
Chaque sélection de résultat est également bornée à 4 096 octets. Il n'y a pas de
résumé récursif ni de réutilisation du KV entre générations dans cette version.

Le serveur Tutor n'est pas modifié. Le modèle reste chargé pendant la vie du
processus. L'installateur fonctionne sur CPU ; la P2000 n'est pas utilisée ni
qualifiée par ces essais. L'ancien GGUF reste sur disque. Installer le nouveau
depuis les paramètres ; l'ancien fichier ne passe pas sa vérification. Un
changement de profil ouvre une nouvelle session native et conserve l'historique
affiché dans le client, sans recharger le journal attaché à l'ancien modèle.

## Mesures et limites, 31 octobre 2026

Essais Linux CPU à quatre threads, llama.cpp via les bindings 0.1.146, contexte
2 048 et sortie maximale 192. Observations uniques : ce ne sont ni des percentiles
ni des mesures de l'i7/P2000. Le chargement et la vérification des poids sont inclus
au premier tour, pas au suivant.

| Essai réel | Premier texte | Réponse terminée |
| --- | ---: | ---: |
| Bonjour avec Tutor configuré mais inaccessible, premier tour natif | 12,25 s | 13,20 s |
| Même message, deuxième tour natif | 3,37 s | 4,15 s |
| Calcul de contrôle, premier tour ACP | 9,69 s | 9,87 s |
| Même calcul, deuxième tour dans le processus | 2,23 s | 2,45 s |
| `/alertes`, fixture MCP avec permission et journal | 18,29 s | 27,52 s |

Le banc ACP standard réussit : calcul exact à froid et à chaud, métadonnées,
annulation (77 ms observées) et arrêt du processus. Le scénario `/alertes` réussit :
une opération, trois sélections exactes, zéro décision d'outil par le modèle,
une génération de 386 tokens d'entrée et 104 de sortie. Le nombre et le marqueur
présents uniquement dans le résultat MCP sont retrouvés dans la réponse.

Le test GGUF de projection après cent tours réussit. **Le test expérimental
historique de calcul sous grammaire JSON échoue** : JSON valide mais valeur `1`
au lieu de `2`. Il reste présent et n'a pas été assoupli. Ce résultat interdit de
présenter le modèle comme décideur fiable de paramètres ou de calculs structurés.
Le parcours ACP livré n'utilise aucune génération JSON pour les outils : ces
choix appartiennent au code Rust. Ces essais ne qualifient ni le coaching complet,
ni toutes les compétences du modèle, ni le serveur réel.

Qwen 0.6B Q4_K_M (397 Mo), Qwen 0.6B Q8_0 (639 Mo) et LFM2.5-1.2B Q4_K_M
(731 Mo) ont été explorés avant de retenir le 1.7B. Les 0.6B présentent des échecs
de calcul ou de format et révèlent une solution malgré une consigne contraire ;
LFM ne fournit pas l'exercice demandé dans l'exemple essayé. Quelques exemples
ne constituent pas un classement ni une comparaison exhaustive. Le prompt compte
également : retirer les consignes de reçus d'une conversation sans reçus a fait
passer les contrôles ACP du 1.7B. Les règles de provenance restent présentes quand
le contexte contient des données ou des contrats.

## Reproduire les contrôles

Après compilation, les scripts suivants acceptent les chemins absolus du binaire
et du GGUF épinglé. Ils ne téléchargent rien et produisent un rapport sans texte
privé. Sous Windows, utiliser le binaire extrait du paquet natif correspondant.

```text
python client/scripts/qualify-local-agent.py --agent CHEMIN_AGENT --model CHEMIN_GGUF --output RAPPORT_ACP --threads 4 --batch 128 --max-output 192
python client/scripts/qualify-local-harness.py --agent CHEMIN_AGENT --model CHEMIN_GGUF --output RAPPORT_MCP --threads 4 --batch 128 --max-output 192
```

Le second script exige désormais le parcours fixe pour une qualification réelle.
Les anciennes traces de découverte restent acceptées uniquement dans les tests
de protocole. Aucun test simulé ne peut déclarer une inférence réelle qualifiée.

À valider sur le PC cible avant de considérer le profil utilisable : latence au
premier message et aux suivants, tenue d'une conversation longue, exactitude des
résumés Tutor, refus/annulation et qualité pédagogique. Le prochain développement
du tutorat doit fournir les exercices et critères depuis des parcours validés,
avec une correction déterministe lorsque possible ; un petit modèle seul ne suffit pas.
