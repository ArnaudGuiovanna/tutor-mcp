# Tutor Desktop 0.3.0

Tutor dispose désormais d'un écran Compte et de Paramètres avec quatre onglets :
Agents, Institution, Membres et Ergonomie. Le choix Français/English change
l'interface et la langue des prochaines réponses, sans traduire les conversations
ou programmes déjà enregistrés. Les thèmes clair, sombre et système, la taille
du texte, l'interligne et la réduction des animations sont conservés par compte.

Les agents intégrés utilisent le même lancement Rust qualifié que l'application.
Les chemins Windows Node sont corrigés et les vérifications du paquet inchangé
sont mises en cache. Le streaming enregistre un journal incrémental récupérable
au lieu de réécrire tout l'historique à chaque fragment. L'activité du tuteur est
présentée en texte dans la conversation, suivant les événements réels.

Dans Agents, les autorisations MCP peuvent être conservées entre les
conversations et redémarrages : autoriser, demander ou refuser par outil, ou
autoriser tous les outils Tutor. La liste provient du serveur connecté. Ces choix
restent liés au compte, à l'institution et à l'agent ; ils ne donnent aucun rôle
supplémentaire. Un refus est aussi appliqué côté serveur.

Compte explique l'identité publique, la signature locale et la paire de
récupération, avec une sauvegarde chiffrée. Il permet de retirer un compte de
l'appareil ou de demander séparément l'effacement des données institutionnelles.
Les membres, invitations, rôles et transferts de propriété utilisent les droits
réels. Ces actions exigent une preuve signée localement, distincte du seul accès
MCP confié à l'agent. Le dernier propriétaire reste protégé.

Les formations personnelles peuvent être archivées, restaurées ou supprimées.
Les brouillons et publications disposent d'actions adaptées ; le retrait d'une
publication conserve les versions et progressions des apprenants déjà inscrits.
Le contrat MCP de sélection précise maintenant les champs réellement obligatoires,
pour reprendre une formation existante sans créer de doublon.

Le serveur corrige le nettoyage PostgreSQL avec le rôle worker limité et expose
des durées et résultats MCP exploitables sans journaliser le contenu privé.

Installer `Tutor_0.3.0_x64-setup.exe` par-dessus la version précédente. Les clés,
comptes, conversations et progressions existants sont conservés ; aucune
réimportation ni régénération de clé n'est nécessaire pour la mise à jour.

Le [backlog avec critères de validation](https://github.com/ArnaudGuiovanna/tutor-mcp/blob/tutor-client-v0.3.0/docs/backlog-tutor-experience.yaml) distingue
les changements, leurs preuves et la recette de distribution. Les rapports de
qualification et sommes SHA-256 accompagnent la release. Une négociation ACP
ou un audit UI Automation ne constitue pas à elle seule une séance avec un modèle
ni un audit manuel complet de Narrator.
