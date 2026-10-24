// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! Discover ACP installations in the environment that will actually run them.
//! Nothing here installs a package, opens a terminal, or invokes `npx`.

use crate::launch::LaunchSpec;
use serde::{Deserialize, Serialize};
use std::collections::HashSet;
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::time::Duration;
use tokio::io::AsyncReadExt;
use tokio::process::Command;

const NATIVE_TIMEOUT: Duration = Duration::from_secs(5);
#[cfg(windows)]
const WSL_TIMEOUT: Duration = Duration::from_secs(8);
const MAX_PROBE_OUTPUT: u64 = 32 * 1024;
#[cfg(windows)]
const MAX_WSL_DISTRIBUTIONS: usize = 12;
#[cfg(any(windows, test))]
const PROBE_MARKER: &str = "TUTOR_ACP_DISCOVERY_1";

// Arguments are supplied after the script, never interpolated into shell text.
// NVM normally lives only in an interactive shell; load it explicitly without
// allowing its startup messages to corrupt the discovery response or ACP wire.
const WSL_ENV: &str = r#"
if [ -s "$HOME/.nvm/nvm.sh" ]; then . "$HOME/.nvm/nvm.sh" >/dev/null 2>&1; fi
PATH="$HOME/.local/bin:$HOME/.npm-global/bin:$HOME/.volta/bin:$PATH"
export PATH
"#;

const WSL_CWD: &str = r#"
workdir="$1"
case "$workdir" in
    '~') workdir="$HOME" ;;
    '~/'*) workdir="$HOME/${workdir#\~/}" ;;
    /*) ;;
    *) workdir="$(wslpath -u "$workdir")" || exit 72 ;;
esac
[ -d "$workdir" ] || { printf '%s\n' 'Dossier de travail WSL introuvable.' >&2; exit 72; }
cd -- "$workdir" || exit 72
"#;

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct AgentTarget {
    pub id: String,
    pub label: String,
    pub environment: String,
    pub distribution: Option<String>,
    #[serde(default)]
    pub wsl_user: Option<String>,
    pub adapter_path: Option<String>,
    pub codex_path: Option<String>,
    pub adapter_version: Option<String>,
    pub codex_version: Option<String>,
    pub available: bool,
    pub detail: String,
}

/// Return a native entry even when dependencies are missing, so setup can
/// explain the missing dependency. WSL entries refer to a distribution/user,
/// not to whatever distribution happens to be the default on the next launch.
#[allow(unused_mut)] // Windows appends WSL installations; other hosts do not.
pub async fn discover_agents() -> Vec<AgentTarget> {
    let mut targets = vec![discover_native().await];
    #[cfg(windows)]
    {
        let wsl = wsl_program();
        let list = LaunchSpec {
            program: wsl.clone(),
            args: vec!["--list".into(), "--verbose".into()],
            env: vec![],
            cwd: None,
        };
        if let Ok(output) = run_probe(list, WSL_TIMEOUT).await {
            let mut pending = tokio::task::JoinSet::new();
            for distro in parse_wsl_distributions(&decode_command_output(&output))
                .into_iter()
                .take(MAX_WSL_DISTRIBUTIONS)
            {
                let wsl = wsl.clone();
                pending.spawn(async move { discover_wsl(&wsl, &distro).await });
            }
            while let Some(result) = pending.join_next().await {
                if let Ok(target) = result {
                    targets.push(target);
                }
            }
            targets[1..].sort_by(|a, b| a.label.cmp(&b.label));
        }
    }
    targets
}

async fn discover_native() -> AgentTarget {
    let paths = native_search_paths();
    let adapter = std::env::var_os("TUTOR_CODEX_ACP_PATH")
        .map(PathBuf::from)
        .filter(|path| path.is_file())
        .or_else(|| find_program("codex-acp", &paths));
    let codex = find_program("codex", &paths);
    let mut target = AgentTarget {
        id: "native".into(),
        label: if cfg!(windows) { "Codex — Windows" } else { "Codex — local" }.into(),
        environment: "native".into(),
        distribution: None,
        wsl_user: None,
        adapter_path: adapter.as_ref().map(|p| p.to_string_lossy().into_owned()),
        codex_path: codex.as_ref().map(|p| p.to_string_lossy().into_owned()),
        adapter_version: None,
        codex_version: None,
        available: false,
        detail: "Adaptateur ACP introuvable. Installez codex-acp ou indiquez son chemin. Codex Desktop n’est pas requis.".into(),
    };
    if let Some(path) = codex {
        target.codex_version = probe_native_version(&path, &paths).await;
    }
    if let Some(path) = adapter {
        match adapter_invocation(&path, &paths) {
            Ok(mut spec) => {
                spec.args.push("--version".into());
                target.adapter_version = run_probe(spec, NATIVE_TIMEOUT)
                    .await
                    .ok()
                    .and_then(|out| clean_version(&decode_command_output(&out)));
                // The ACP initialize handshake performs the definitive version
                // check. An adapter can omit a --version switch and still work.
                target.available = true;
                target.detail = "Adaptateur ACP trouvé. Connexion et capacités à vérifier au démarrage ; le runtime Codex compatible de l’adaptateur sera utilisé.".into();
            }
            Err(message) => target.detail = message,
        }
    }
    target
}

/// Construct a launch in the same OS/distro as the adapter. The discovered
/// Codex path is informational: setting CODEX_PATH automatically could pair an
/// adapter with an incompatible CLI. Its bundled compatible CLI is the default.
/// A manually configured target can explicitly choose another versioned CLI.
pub async fn launch_spec(target: &AgentTarget, cwd: &str) -> Result<LaunchSpec, String> {
    validate_text(cwd, "Le dossier de travail")?;
    let adapter = target
        .adapter_path
        .as_deref()
        .filter(|path| !path.trim().is_empty())
        .ok_or_else(|| "Indiquez le chemin de l’adaptateur ACP.".to_string())?;
    validate_text(adapter, "Le chemin de l’adaptateur")?;
    match target.environment.as_str() {
        "native" => {
            let path = Path::new(adapter);
            if !path.is_absolute() || !path.is_file() {
                return Err(
                    "L’adaptateur doit être un fichier existant avec un chemin absolu.".into(),
                );
            }
            let directory = Path::new(cwd);
            if !directory.is_absolute() || !directory.is_dir() {
                return Err(
                    "Choisissez un dossier de travail existant avec un chemin absolu.".into(),
                );
            }
            let mut spec = adapter_invocation(path, &native_search_paths())?;
            spec.cwd = Some(directory.to_path_buf());
            if let Some(codex) = manual_codex_override(target)? {
                validate_native_codex(codex, directory).await?;
                spec.env.push(("CODEX_PATH".into(), codex.into()));
            }
            Ok(spec)
        }
        "wsl" => {
            if !cfg!(windows) {
                return Err("Le lancement WSL est disponible depuis Windows.".into());
            }
            let program = wsl_program();
            #[cfg(windows)]
            {
                let output = run_probe(
                    LaunchSpec {
                        program: program.clone(),
                        args: vec!["--list".into(), "--verbose".into()],
                        env: vec![],
                        cwd: None,
                    },
                    WSL_TIMEOUT,
                )
                .await?;
                let distro = target.distribution.as_deref().unwrap_or_default();
                if !parse_wsl_distributions(&decode_command_output(&output))
                    .iter()
                    .any(|name| name == distro)
                {
                    return Err(
                        "Cette distribution WSL2 n’est plus disponible. Relancez la détection."
                            .into(),
                    );
                }
            }
            if let Some(codex) = manual_codex_override(target)? {
                let output = run_probe(wsl_codex_probe(&program, target, codex)?, Duration::from_secs(8)).await
                    .map_err(|_| "Le Codex Linux choisi est introuvable, non exécutable ou ne répond pas à --version.".to_string())?;
                validate_codex_version(&output)?;
            }
            build_wsl_launch(&program, target, cwd)
        }
        _ => Err("Environnement d’agent inconnu.".into()),
    }
}

fn manual_codex_override(target: &AgentTarget) -> Result<Option<&str>, String> {
    if target.id != "manual" {
        return Ok(None);
    }
    let Some(path) = target
        .codex_path
        .as_deref()
        .filter(|value| !value.trim().is_empty())
    else {
        return Ok(None);
    };
    validate_text(path, "Le chemin de Codex")?;
    if target.environment == "wsl" && !path.starts_with('/') {
        return Err("Le Codex choisi dans WSL doit utiliser un chemin Linux absolu.".into());
    }
    Ok(Some(path))
}

async fn validate_native_codex(path: &str, cwd: &Path) -> Result<(), String> {
    let codex = Path::new(path);
    #[cfg(windows)]
    if !codex
        .extension()
        .and_then(|value| value.to_str())
        .is_some_and(|extension| extension.eq_ignore_ascii_case("exe"))
    {
        return Err("Choisissez le binaire natif codex.exe ; les wrappers npm .cmd ne conviennent pas à CODEX_PATH. Laissez ce champ vide pour utiliser le runtime fourni par l’adaptateur.".into());
    }
    if !codex.is_absolute() || !executable_candidate(codex) {
        return Err(
            "Le Codex choisi doit être un exécutable existant avec un chemin absolu.".into(),
        );
    }
    let output = run_probe(
        LaunchSpec {
            program: path.into(),
            args: vec!["--version".into()],
            env: vec![],
            cwd: Some(cwd.to_path_buf()),
        },
        NATIVE_TIMEOUT,
    )
    .await
    .map_err(|_| "Le Codex choisi ne répond pas correctement à --version.".to_string())?;
    validate_codex_version(&output)
}

fn validate_codex_version(output: &[u8]) -> Result<(), String> {
    let text = decode_command_output(output);
    let version = clean_version(&text)
        .ok_or_else(|| "Le programme choisi n’annonce pas de version Codex CLI.".to_string())?;
    if !version
        .strip_prefix("codex-cli ")
        .is_some_and(|value| value.starts_with(|ch: char| ch.is_ascii_digit()))
    {
        return Err("Le programme choisi n’annonce pas de version Codex CLI.".into());
    }
    Ok(())
}

fn wsl_codex_probe(wsl: &str, target: &AgentTarget, codex: &str) -> Result<LaunchSpec, String> {
    validate_text(codex, "Le chemin de Codex")?;
    if !codex.starts_with('/') {
        return Err("Le Codex WSL doit utiliser un chemin Linux absolu.".into());
    }
    let distro = target
        .distribution
        .as_deref()
        .ok_or_else(|| "Choisissez une distribution WSL2.".to_string())?;
    let mut args = wsl_args(distro, target.wsl_user.as_deref())?;
    args.extend([
        format!(
            r#"{WSL_ENV}
codex="$1"
[ -f "$codex" ] && [ -x "$codex" ] || exit 72
PATH="$(dirname "$codex"):$PATH"
export PATH
unset CODEX_PATH
exec "$codex" --version
"#
        ),
        "tutor-codex-version".into(),
        codex.into(),
    ]);
    Ok(LaunchSpec {
        program: wsl.into(),
        args,
        env: vec![],
        cwd: None,
    })
}

/// Launch a configured local MCP command in the same native/WSL environment
/// as the selected agent. Arguments remain separate across both OS boundaries.
pub async fn command_spec(
    target: &AgentTarget,
    command: &str,
    args: &[String],
    cwd: &str,
) -> Result<LaunchSpec, String> {
    validate_text(command, "La commande locale")?;
    if args.len() > 128
        || args
            .iter()
            .any(|arg| arg.len() > 32768 || arg.contains('\0'))
    {
        return Err("Arguments de commande locale invalides ou trop longs.".into());
    }
    let resolved = match target.environment.as_str() {
        "native" => {
            let path = Path::new(command);
            if path.is_absolute() {
                path.to_path_buf()
            } else if path.components().count() == 1 {
                find_program(command, &native_search_paths()).ok_or_else(|| {
                    "Commande locale introuvable ; indiquez son chemin absolu.".to_string()
                })?
            } else {
                return Err("Indiquez un chemin absolu pour la commande locale.".into());
            }
        }
        .to_string_lossy()
        .into_owned(),
        "wsl" => {
            if command.starts_with('/') {
                command.to_string()
            } else {
                if !cfg!(windows) {
                    return Err("Le lancement WSL est disponible depuis Windows.".into());
                }
                if command.starts_with('-')
                    || command.contains('/')
                    || command.chars().any(char::is_whitespace)
                {
                    return Err(
                        "Indiquez le nom simple ou le chemin Linux absolu de la commande.".into(),
                    );
                }
                let distro = target
                    .distribution
                    .as_deref()
                    .ok_or_else(|| "Choisissez une distribution WSL2.".to_string())?;
                let mut probe_args = wsl_args(distro, target.wsl_user.as_deref())?;
                probe_args.extend([format!("{WSL_ENV}\nresolved=\"$(command -v \"$1\")\" || exit 72\nprintf 'TUTOR_COMMAND_1\\0%s\\0' \"$resolved\""), "tutor-command".into(), command.into()]);
                let output = run_probe(
                    LaunchSpec {
                        program: wsl_program(),
                        args: probe_args,
                        env: vec![],
                        cwd: None,
                    },
                    Duration::from_secs(8),
                )
                .await?;
                let text =
                    String::from_utf8(output).map_err(|_| "Réponse WSL invalide.".to_string())?;
                let resolved = text
                    .split_once("TUTOR_COMMAND_1\0")
                    .and_then(|(_, path)| path.split('\0').next())
                    .ok_or_else(|| "Commande locale introuvable dans WSL.".to_string())?;
                if !resolved.starts_with('/')
                    || validate_text(resolved, "La commande Linux").is_err()
                {
                    return Err("La commande WSL n’est pas un exécutable absolu.".into());
                }
                resolved.to_string()
            }
        }
        _ => return Err("Environnement d’agent inconnu.".into()),
    };
    let mut configured = target.clone();
    configured.adapter_path = Some(resolved);
    // Tutor is a separate local MCP process; an agent's optional CLI override
    // must neither be probed nor propagated into that command.
    configured.codex_path = None;
    let mut spec = launch_spec(&configured, cwd).await?;
    spec.args.extend_from_slice(args);
    Ok(spec)
}

/// Resolve the absolute directory sent in ACP session/new or session/load.
/// The Windows relay directory and the Linux agent directory are different.
pub async fn agent_cwd(target: &AgentTarget, cwd: &str) -> Result<String, String> {
    validate_text(cwd, "Le dossier de travail")?;
    match target.environment.as_str() {
        "native" => {
            let path = Path::new(cwd);
            if !path.is_absolute() || !path.is_dir() {
                return Err(
                    "Choisissez un dossier de travail existant avec un chemin absolu.".into(),
                );
            }
            Ok(cwd.to_string())
        }
        "wsl" => {
            if !cfg!(windows) {
                return Err("Le lancement WSL est disponible depuis Windows.".into());
            }
            let spec = wsl_cwd_probe(&wsl_program(), target, cwd)?;
            let output = run_probe(spec, Duration::from_secs(8)).await?;
            let text =
                String::from_utf8(output).map_err(|_| "Réponse WSL invalide.".to_string())?;
            let resolved = text
                .split_once("TUTOR_ACP_CWD_1\0")
                .and_then(|(_, value)| value.split('\0').next())
                .ok_or_else(|| "Le dossier WSL n’a pas pu être résolu.".to_string())?;
            validate_text(resolved, "Le dossier WSL")?;
            if !resolved.starts_with('/') {
                return Err("WSL n’a pas retourné de chemin Linux absolu.".into());
            }
            Ok(resolved.to_string())
        }
        _ => Err("Environnement d’agent inconnu.".into()),
    }
}

fn wsl_cwd_probe(wsl: &str, target: &AgentTarget, cwd: &str) -> Result<LaunchSpec, String> {
    validate_wsl_cwd(cwd)?;
    let distro = target
        .distribution
        .as_deref()
        .ok_or_else(|| "Choisissez une distribution WSL2.".to_string())?;
    let mut args = wsl_args(distro, target.wsl_user.as_deref())?;
    args.extend([
        format!("{WSL_CWD}\nprintf 'TUTOR_ACP_CWD_1\\0%s\\0' \"$(pwd -P)\""),
        "tutor-cwd".into(),
        cwd.into(),
    ]);
    Ok(LaunchSpec {
        program: wsl.into(),
        args,
        env: vec![],
        cwd: None,
    })
}

fn validate_text(value: &str, label: &str) -> Result<(), String> {
    if value.trim().is_empty() || value.len() > 32768 || value.chars().any(char::is_control) {
        return Err(format!(
            "{label} est vide ou contient des caractères interdits."
        ));
    }
    Ok(())
}

fn native_search_paths() -> Vec<PathBuf> {
    let mut dirs = Vec::new();
    // A deliberately bundled executable takes precedence over PATH entries.
    if let Ok(executable) = std::env::current_exe() {
        if let Some(parent) = executable.parent() {
            dirs.push(parent.to_path_buf());
            dirs.push(parent.join("binaries"));
        }
    }
    if let Some(path) = std::env::var_os("PATH") {
        dirs.extend(std::env::split_paths(&path));
    }
    if let Some(home) = std::env::var_os(if cfg!(windows) { "USERPROFILE" } else { "HOME" }) {
        let home = PathBuf::from(home);
        dirs.push(home.join(".local").join("bin"));
        dirs.push(home.join(".npm-global").join("bin"));
        dirs.push(home.join(".volta").join("bin"));
    }
    if cfg!(windows) {
        if let Some(appdata) = std::env::var_os("APPDATA") {
            dirs.push(PathBuf::from(appdata).join("npm"));
        }
        if let Some(nvm) = std::env::var_os("NVM_SYMLINK") {
            dirs.push(PathBuf::from(nvm));
        }
    }
    let mut seen = HashSet::new();
    dirs.retain(|dir| !dir.as_os_str().is_empty() && seen.insert(dir.clone()));
    dirs
}

fn find_program(name: &str, paths: &[PathBuf]) -> Option<PathBuf> {
    let extensions: &[&str] = if cfg!(windows) {
        &[".exe", ".cmd", ""]
    } else {
        &[""]
    };
    paths.iter().find_map(|dir| {
        extensions.iter().find_map(|extension| {
            let candidate = dir.join(format!("{name}{extension}"));
            executable_candidate(&candidate).then(|| candidate.canonicalize().unwrap_or(candidate))
        })
    })
}

fn executable_candidate(path: &Path) -> bool {
    if !path.is_file() {
        return false;
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        path.metadata()
            .map(|metadata| metadata.permissions().mode() & 0o111 != 0)
            .unwrap_or(false)
    }
    #[cfg(not(unix))]
    true
}

/// Resolve npm's .cmd shim to its package bin. Executing a .cmd shim via
/// cmd.exe would introduce a shell boundary and quoting/injection differences.
fn adapter_invocation(path: &Path, paths: &[PathBuf]) -> Result<LaunchSpec, String> {
    let extension = path
        .extension()
        .and_then(|v| v.to_str())
        .unwrap_or_default();
    if extension.eq_ignore_ascii_case("cmd") || extension.eq_ignore_ascii_case("bat") {
        let script = npm_package_bin(path, "@agentclientprotocol/codex-acp", "codex-acp")?;
        return node_invocation(&script, paths);
    }
    if extension.eq_ignore_ascii_case("js")
        || extension.eq_ignore_ascii_case("mjs")
        || extension.eq_ignore_ascii_case("cjs")
    {
        return node_invocation(path, paths);
    }
    if !executable_candidate(path) {
        return Err("L’adaptateur existe mais n’est pas un fichier exécutable.".into());
    }
    Ok(LaunchSpec {
        program: path.to_string_lossy().into_owned(),
        args: vec![],
        env: vec![],
        cwd: None,
    })
}

fn node_invocation(script: &Path, paths: &[PathBuf]) -> Result<LaunchSpec, String> {
    // Node must itself be a native executable. A node.cmd shim would re-create
    // the shell invocation we deliberately avoided for npm's ACP wrapper.
    let node_name = if cfg!(windows) { "node.exe" } else { "node" };
    let node = paths.iter().map(|dir| dir.join(node_name)).find(|path| executable_candidate(path))
        .ok_or_else(|| "Node.js est requis pour cet adaptateur JavaScript. Installez Node.js ou choisissez l’exécutable autonome de l’adaptateur.".to_string())?;
    Ok(LaunchSpec {
        program: node.to_string_lossy().into_owned(),
        args: vec![script.to_string_lossy().into_owned()],
        env: vec![],
        cwd: None,
    })
}

fn npm_package_bin(shim: &Path, package: &str, command: &str) -> Result<PathBuf, String> {
    let parent = shim
        .parent()
        .ok_or_else(|| "Chemin npm invalide.".to_string())?;
    let roots = [
        parent.join("node_modules").join(package),
        parent.join("..").join(package),
    ];
    for root in roots {
        let Ok(bytes) = std::fs::read(root.join("package.json")) else {
            continue;
        };
        let Ok(metadata) = serde_json::from_slice::<serde_json::Value>(&bytes) else {
            continue;
        };
        let Some(bin) = metadata.get("bin") else {
            continue;
        };
        let Some(relative) = bin
            .as_str()
            .or_else(|| bin.get(command).and_then(|v| v.as_str()))
        else {
            continue;
        };
        let candidate = root.join(relative);
        let (Ok(canonical_root), Ok(canonical_bin)) =
            (root.canonicalize(), candidate.canonicalize())
        else {
            continue;
        };
        if canonical_bin.starts_with(canonical_root) && canonical_bin.is_file() {
            return Ok(canonical_bin);
        }
    }
    Err("Le wrapper npm a été trouvé, mais son paquet est incomplet. Réinstallez l’adaptateur ou choisissez son exécutable autonome.".into())
}

async fn probe_native_version(path: &Path, paths: &[PathBuf]) -> Option<String> {
    let extension = path
        .extension()
        .and_then(|v| v.to_str())
        .unwrap_or_default();
    let mut spec = if extension.eq_ignore_ascii_case("cmd") {
        let script = npm_package_bin(path, "@openai/codex", "codex").ok()?;
        node_invocation(&script, paths).ok()?
    } else {
        adapter_invocation(path, paths).ok()?
    };
    spec.args.push("--version".into());
    run_probe(spec, NATIVE_TIMEOUT)
        .await
        .ok()
        .and_then(|out| clean_version(&decode_command_output(&out)))
}

fn clean_version(text: &str) -> Option<String> {
    text.lines()
        .map(str::trim)
        .find(|line| !line.is_empty() && line.len() <= 200 && !line.chars().any(char::is_control))
        .map(str::to_string)
}

fn wsl_program() -> String {
    std::env::var_os("SystemRoot")
        .map(PathBuf::from)
        .map(|root| root.join("System32").join("wsl.exe"))
        .filter(|path| path.is_file())
        .map(|path| path.to_string_lossy().into_owned())
        .unwrap_or_else(|| "wsl.exe".into())
}

/// `wsl --list --verbose` is localized. Only the final version column is
/// interpreted; names with spaces and the default marker remain supported.
#[cfg(any(windows, test))]
fn parse_wsl_distributions(output: &str) -> Vec<String> {
    let mut names = Vec::new();
    for line in output.lines() {
        let line = line.trim().trim_start_matches('*').trim();
        let Some((before_version, version)) = line.rsplit_once(char::is_whitespace) else {
            continue;
        };
        if version.trim() != "2" {
            continue;
        }
        let trimmed = before_version.trim_end();
        // A localized state can contain single spaces. Table columns are
        // separated by runs of spaces (or tabs), not by one translated word.
        let name_end = trimmed.char_indices().find_map(|(offset, ch)| {
            (ch == '\t'
                || (ch.is_whitespace()
                    && trimmed[offset + ch.len_utf8()..]
                        .chars()
                        .next()
                        .is_some_and(char::is_whitespace)))
            .then_some(offset)
        });
        let name = if let Some(end) = name_end {
            trimmed[..end].trim()
        } else if let Some((name, _state)) = trimmed.rsplit_once(char::is_whitespace) {
            name.trim()
        } else {
            continue;
        };
        if !name.is_empty()
            && !name.to_ascii_lowercase().starts_with("docker-desktop")
            && validate_name(name).is_ok()
            && !names.iter().any(|prior| prior == name)
        {
            names.push(name.to_string());
        }
    }
    names
}

fn decode_command_output(bytes: &[u8]) -> String {
    if bytes.starts_with(&[0xff, 0xfe]) || (bytes.len() >= 4 && bytes[1] == 0 && bytes[3] == 0) {
        let start = usize::from(bytes.starts_with(&[0xff, 0xfe])) * 2;
        let units = bytes[start..]
            .chunks_exact(2)
            .map(|pair| u16::from_le_bytes([pair[0], pair[1]]))
            .collect::<Vec<_>>();
        String::from_utf16_lossy(&units)
    } else {
        String::from_utf8_lossy(bytes)
            .trim_start_matches('\u{feff}')
            .to_string()
    }
}

fn validate_name(name: &str) -> Result<(), String> {
    validate_text(name, "La distribution ou l’utilisateur WSL")?;
    if name.starts_with('-') || name.len() > 256 {
        return Err("Nom WSL invalide.".into());
    }
    Ok(())
}

fn wsl_args(distribution: &str, user: Option<&str>) -> Result<Vec<String>, String> {
    validate_name(distribution)?;
    let mut args = vec!["--distribution".into(), distribution.into()];
    if let Some(user) = user {
        validate_name(user)?;
        args.extend(["--user".into(), user.into()]);
    }
    // Startup files may print banners to stdout, which would corrupt ACP.
    // Load supported runtime managers explicitly in WSL_ENV instead.
    args.extend([
        "--exec".into(),
        "/bin/bash".into(),
        "--noprofile".into(),
        "--norc".into(),
        "-c".into(),
    ]);
    Ok(args)
}

#[cfg(windows)]
async fn discover_wsl(wsl: &str, distribution: &str) -> AgentTarget {
    let mut target = AgentTarget {
        id: format!("wsl:{distribution}"),
        label: format!("Codex — WSL · {distribution}"),
        environment: "wsl".into(),
        distribution: Some(distribution.into()),
        wsl_user: None,
        adapter_path: None,
        codex_path: None,
        adapter_version: None,
        codex_version: None,
        available: false,
        detail:
            "Distribution inaccessible ou démarrage trop long. Réessayez après avoir ouvert WSL."
                .into(),
    };
    let script = format!("{WSL_ENV}\nprintf '%s\\0%s\\0%s\\0%s\\0' '{PROBE_MARKER}' \"$(command -v codex-acp || true)\" \"$(command -v codex || true)\" \"$(id -un)\"");
    let Ok(mut args) = wsl_args(distribution, None) else {
        return target;
    };
    args.push(script);
    let spec = LaunchSpec {
        program: wsl.into(),
        args,
        env: vec![],
        cwd: None,
    };
    if let Ok(output) = run_probe(spec, WSL_TIMEOUT).await {
        if let Some((adapter, codex, user)) = parse_wsl_probe(&output) {
            target.adapter_path = adapter;
            target.codex_path = codex;
            target.wsl_user = Some(user);
            target.available = target.adapter_path.is_some();
            target.detail = if target.available {
                "Adaptateur trouvé dans WSL2. Il utilisera la connexion Codex du même utilisateur Linux et son runtime compatible.".into()
            } else if target.codex_path.is_some() {
                "Codex est installé dans WSL2 ; l’adaptateur codex-acp manque dans cette distribution.".into()
            } else {
                "Installez l’adaptateur codex-acp dans cette distribution WSL2, puis relancez la détection.".into()
            };
        }
    }
    target
}

#[cfg(any(windows, test))]
fn parse_wsl_probe(bytes: &[u8]) -> Option<(Option<String>, Option<String>, String)> {
    let text = String::from_utf8(bytes.to_vec()).ok()?;
    let marker = format!("{PROBE_MARKER}\0");
    let start = text.find(&marker)? + marker.len();
    let mut fields = text[start..].split('\0');
    let adapter = fields.next()?;
    let codex = fields.next()?;
    let user = fields.next()?;
    validate_name(user).ok()?;
    for path in [adapter, codex] {
        if !path.is_empty()
            && (!path.starts_with('/') || validate_text(path, "Le chemin Linux").is_err())
        {
            return None;
        }
    }
    Some((
        (!adapter.is_empty()).then(|| adapter.into()),
        (!codex.is_empty()).then(|| codex.into()),
        user.into(),
    ))
}

fn build_wsl_launch(wsl: &str, target: &AgentTarget, cwd: &str) -> Result<LaunchSpec, String> {
    let codex_override = manual_codex_override(target)?.unwrap_or_default();
    let distro = target
        .distribution
        .as_deref()
        .ok_or_else(|| "Choisissez une distribution WSL2.".to_string())?;
    let adapter = target
        .adapter_path
        .as_deref()
        .ok_or_else(|| "Indiquez l’adaptateur Linux.".to_string())?;
    if !adapter.starts_with('/') || validate_text(adapter, "Le chemin de l’adaptateur").is_err() {
        return Err("L’adaptateur WSL doit utiliser un chemin Linux absolu.".into());
    }
    validate_wsl_cwd(cwd)?;
    let mut args = wsl_args(distro, target.wsl_user.as_deref())?;
    let script = format!(
        r#"{WSL_ENV}
adapter="$2"
codex_override="$3"
{WSL_CWD}
shift 3
if [ -n "$codex_override" ]; then
    export CODEX_PATH="$codex_override"
else
    unset CODEX_PATH
fi
export INITIAL_AGENT_MODE=read-only
[ -x "$adapter" ] || {{ printf '%s\n' 'Adaptateur WSL introuvable ou non exécutable.' >&2; exit 72; }}
PATH="$(dirname "$adapter"):$PATH"
export PATH
# WSL is a process bridge, not the Linux process owner. Give this session its
# own process group and clean it on normal EOF, cancellation and relay signals.
# Never terminate a whole distribution, which may contain unrelated work.
command -v setsid >/dev/null 2>&1 || {{ printf '%s\n' 'setsid (util-linux) est requis pour superviser l’agent WSL.' >&2; exit 72; }}
set +m
setsid "$adapter" "$@" <&0 &
agent_pid=$!
cleanup_agent() {{
    kill -TERM -- "-$agent_pid" 2>/dev/null || true
    sleep 0.2
    kill -KILL -- "-$agent_pid" 2>/dev/null || true
}}
trap cleanup_agent EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
wait "$agent_pid"
agent_status=$?
exit "$agent_status"
"#
    );
    args.extend([
        script,
        "tutor-acp".into(),
        cwd.into(),
        adapter.into(),
        codex_override.into(),
    ]);
    Ok(LaunchSpec {
        program: wsl.into(),
        args,
        env: vec![],
        cwd: None,
    })
}

fn validate_wsl_cwd(cwd: &str) -> Result<(), String> {
    validate_text(cwd, "Le dossier de travail")?;
    if !(cwd.starts_with('/') || cwd == "~" || cwd.starts_with("~/") || is_windows_absolute(cwd)) {
        return Err(
            "Indiquez un chemin Linux absolu, ~/… ou un chemin Windows absolu pour WSL.".into(),
        );
    }
    Ok(())
}

fn is_windows_absolute(value: &str) -> bool {
    let bytes = value.as_bytes();
    (bytes.len() >= 3
        && bytes[0].is_ascii_alphabetic()
        && bytes[1] == b':'
        && (bytes[2] == b'\\' || bytes[2] == b'/'))
        || value.starts_with("\\\\")
}

async fn run_probe(spec: LaunchSpec, deadline: Duration) -> Result<Vec<u8>, String> {
    let mut command = Command::new(&spec.program);
    command
        .args(&spec.args)
        .envs(spec.env.iter().map(|(key, value)| (key, value)))
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .kill_on_drop(true);
    if let Some(cwd) = spec.cwd {
        command.current_dir(cwd);
    }
    #[cfg(windows)]
    command.creation_flags(0x08000000); // CREATE_NO_WINDOW
    let mut child = command
        .spawn()
        .map_err(|_| "Exécutable introuvable ou impossible à lancer.".to_string())?;
    let stdout = child
        .stdout
        .take()
        .ok_or_else(|| "Sortie du processus indisponible.".to_string())?;
    let response = tokio::time::timeout(deadline, async {
        let mut bytes = Vec::new();
        stdout
            .take(MAX_PROBE_OUTPUT + 1)
            .read_to_end(&mut bytes)
            .await
            .map_err(|_| "Lecture du processus impossible.".to_string())?;
        if bytes.len() as u64 > MAX_PROBE_OUTPUT {
            return Err("Sortie de détection trop volumineuse.".into());
        }
        let status = child
            .wait()
            .await
            .map_err(|_| "État du processus indisponible.".to_string())?;
        if !status.success() {
            return Err("La commande de détection a échoué.".into());
        }
        Ok(bytes)
    })
    .await;
    match response {
        Ok(Ok(bytes)) => Ok(bytes),
        other => {
            let _ = child.kill().await;
            match other {
                Ok(Err(error)) => Err(error),
                _ => Err("Délai de détection dépassé.".into()),
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn wsl_target() -> AgentTarget {
        AgentTarget {
            id: "wsl:Ubuntu".into(),
            label: "Ubuntu".into(),
            environment: "wsl".into(),
            distribution: Some("Ubuntu test".into()),
            wsl_user: Some("alice".into()),
            adapter_path: Some("/home/alice/node tools/codex-acp".into()),
            codex_path: Some("/usr/bin/codex".into()),
            adapter_version: None,
            codex_version: None,
            available: true,
            detail: String::new(),
        }
    }

    #[test]
    fn wsl_list_supports_localized_states_utf16_and_names_with_spaces() {
        let source = "  NAME                   STATE           VERSION\r\n* Ubuntu                 Running         2\r\n  Debian test            Arrêté          2\r\n  Ancient                Stopped         1\r\n  docker-desktop         Running         2\r\n  docker-desktop-data    Stopped         2\r\n";
        let bytes = source
            .encode_utf16()
            .flat_map(u16::to_le_bytes)
            .collect::<Vec<_>>();
        assert_eq!(
            parse_wsl_distributions(&decode_command_output(&bytes)),
            vec!["Ubuntu", "Debian test"]
        );
        assert_eq!(
            parse_wsl_distributions(source),
            vec!["Ubuntu", "Debian test"]
        );
        let with_bom = [vec![0xff, 0xfe], bytes].concat();
        assert_eq!(
            parse_wsl_distributions(&decode_command_output(&with_bom)),
            vec!["Ubuntu", "Debian test"]
        );
        assert_eq!(
            parse_wsl_distributions("  Ubuntu test    En cours d’exécution    2\n"),
            vec!["Ubuntu test"]
        );
    }

    #[test]
    fn wsl_launch_keeps_paths_out_of_shell_source_and_pins_user() {
        let cwd = "/home/alice/études $(touch hacked); 'test'";
        let spec = build_wsl_launch("C:\\Windows\\System32\\wsl.exe", &wsl_target(), cwd).unwrap();
        assert_eq!(
            &spec.args[..9],
            [
                "--distribution",
                "Ubuntu test",
                "--user",
                "alice",
                "--exec",
                "/bin/bash",
                "--noprofile",
                "--norc",
                "-c"
            ]
        );
        assert!(!spec.args[9].contains(cwd));
        assert_eq!(spec.args[10], "tutor-acp");
        assert_eq!(spec.args[11], cwd);
        assert_eq!(spec.args[12], "/home/alice/node tools/codex-acp");
        assert!(spec.env.is_empty());
        assert!(spec.cwd.is_none());
    }

    #[test]
    fn wsl_windows_path_translation_is_explicit() {
        let spec =
            build_wsl_launch("wsl.exe", &wsl_target(), "C:\\Users\\Alice\\Mes cours").unwrap();
        assert!(spec.args[9].contains("wslpath -u"));
        assert_eq!(spec.args[11], "C:\\Users\\Alice\\Mes cours");
        assert!(build_wsl_launch("wsl.exe", &wsl_target(), "relative/path").is_err());
        assert!(build_wsl_launch("wsl.exe", &wsl_target(), "~/cours").is_ok());
    }

    #[test]
    fn invalid_targets_do_not_become_options_or_shell_programs() {
        let mut target = wsl_target();
        target.distribution = Some("--unregister".into());
        assert!(build_wsl_launch("wsl.exe", &target, "/home/alice").is_err());
        target.distribution = Some("Ubuntu".into());
        target.adapter_path = Some("codex-acp; unwanted".into());
        assert!(build_wsl_launch("wsl.exe", &target, "/home/alice").is_err());
        target.adapter_path = Some("/bin/codex-acp\nexit".into());
        assert!(build_wsl_launch("wsl.exe", &target, "/home/alice").is_err());
        assert!(validate_text("a\0b", "path").is_err());
    }

    #[test]
    fn probe_parser_ignores_profile_noise_and_preserves_unicode() {
        let bytes = b"profile notice\nTUTOR_ACP_DISCOVERY_1\0/home/alice/node tools/codex-acp\0/usr/bin/codex\0alice\0";
        let (adapter, codex, user) = parse_wsl_probe(bytes).unwrap();
        assert_eq!(adapter.as_deref(), Some("/home/alice/node tools/codex-acp"));
        assert_eq!(codex.as_deref(), Some("/usr/bin/codex"));
        assert_eq!(user, "alice");
        assert!(parse_wsl_probe(b"TUTOR_ACP_DISCOVERY_1\0relative\0\0alice\0").is_none());
        assert_eq!(
            decode_command_output("français 日本語".as_bytes()),
            "français 日本語"
        );
    }

    #[tokio::test]
    async fn launch_rejects_missing_adapter_and_unknown_environment() {
        let mut target = wsl_target();
        target.environment = "native".into();
        assert!(launch_spec(&target, "/tmp").await.is_err());
        target.environment = "remote-shell".into();
        assert!(launch_spec(&target, "/tmp").await.is_err());
        assert!(agent_cwd(&target, "/tmp").await.is_err());
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn agent_directory_is_absolute_existing_and_resolved_for_wsl() {
        let directory = std::env::temp_dir();
        let mut target = wsl_target();
        target.environment = "native".into();
        assert_eq!(
            agent_cwd(&target, directory.to_str().unwrap())
                .await
                .unwrap(),
            directory.to_string_lossy()
        );
        assert!(agent_cwd(&target, "relative").await.is_err());
        target.environment = "wsl".into();
        let probe = wsl_cwd_probe("wsl.exe", &target, "~").unwrap();
        let local_probe = LaunchSpec {
            program: "/bin/bash".into(),
            args: probe.args[6..].to_vec(),
            env: vec![("HOME".into(), directory.to_string_lossy().into_owned())],
            cwd: None,
        };
        let output = run_probe(local_probe, Duration::from_secs(1))
            .await
            .unwrap();
        let expected = format!(
            "TUTOR_ACP_CWD_1\0{}\0",
            directory.canonicalize().unwrap().to_string_lossy()
        );
        assert_eq!(output, expected.as_bytes());
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn local_command_spec_preserves_each_argument_and_resolves_path() {
        let mut target = wsl_target();
        target.environment = "native".into();
        let directory = std::env::temp_dir();
        let args = vec!["%s".into(), "quoted ' value $(not executed)".into()];
        let spec = command_spec(&target, "printf", &args, directory.to_str().unwrap())
            .await
            .unwrap();
        assert!(Path::new(&spec.program).is_absolute());
        assert_eq!(spec.args, args);
        assert_eq!(
            run_probe(spec, Duration::from_secs(1)).await.unwrap(),
            args[1].as_bytes()
        );
        assert!(command_spec(
            &target,
            "printf",
            &["invalid\0argument".into()],
            directory.to_str().unwrap()
        )
        .await
        .is_err());
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn only_an_explicit_manual_cli_overrides_the_bundled_runtime() {
        use std::os::unix::fs::PermissionsExt;
        let root = std::env::temp_dir().join(format!(
            "tutor-cli-override-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        std::fs::create_dir_all(&root).unwrap();
        let adapter = root.join("adapter");
        let codex = root.join("Codex 'manuel' 日本語");
        std::fs::write(&adapter, "#!/bin/sh\nexit 0\n").unwrap();
        std::fs::write(&codex, "#!/bin/sh\nprintf 'codex-cli 0.164.0\\n'\n").unwrap();
        for path in [&adapter, &codex] {
            std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o755)).unwrap();
        }
        let mut target = wsl_target();
        target.environment = "native".into();
        target.adapter_path = Some(adapter.to_string_lossy().into_owned());
        target.codex_path = Some(codex.to_string_lossy().into_owned());
        target.id = "native".into();
        assert!(launch_spec(&target, root.to_str().unwrap())
            .await
            .unwrap()
            .env
            .is_empty());
        target.id = "manual".into();
        assert_eq!(
            launch_spec(&target, root.to_str().unwrap())
                .await
                .unwrap()
                .env,
            vec![("CODEX_PATH".into(), codex.to_string_lossy().into_owned())]
        );
        target.codex_path = Some(root.join("missing-cli").to_string_lossy().into_owned());
        assert!(launch_spec(&target, root.to_str().unwrap()).await.is_err());
        // A local Tutor command does not depend on the agent CLI override.
        assert!(command_spec(
            &target,
            adapter.to_str().unwrap(),
            &[],
            root.to_str().unwrap()
        )
        .await
        .unwrap()
        .env
        .is_empty());
        target.codex_path = Some(codex.to_string_lossy().into_owned());
        std::fs::write(&codex, "#!/bin/sh\nprintf 'some unrelated program\\n'\n").unwrap();
        assert!(launch_spec(&target, root.to_str().unwrap()).await.is_err());
        std::fs::set_permissions(&codex, std::fs::Permissions::from_mode(0o644)).unwrap();
        assert!(launch_spec(&target, root.to_str().unwrap()).await.is_err());
        std::fs::remove_dir_all(root).unwrap();
    }

    #[cfg(windows)]
    #[tokio::test]
    async fn windows_manual_codex_requires_a_native_executable_not_an_npm_shim() {
        let root = std::env::temp_dir();
        let error = validate_native_codex("C:\\Users\\Alice\\npm\\codex.cmd", &root)
            .await
            .unwrap_err();
        assert!(error.contains("codex.exe") && error.contains(".cmd") && error.contains("vide"));
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn probes_enforce_timeout_output_limit_and_exit_status() {
        fn shell(command: &str) -> LaunchSpec {
            LaunchSpec {
                program: "/bin/sh".into(),
                args: vec!["-c".into(), command.into()],
                env: vec![],
                cwd: None,
            }
        }
        assert_eq!(
            run_probe(shell("printf 'version 1.0'"), Duration::from_secs(1))
                .await
                .unwrap(),
            b"version 1.0"
        );
        assert!(run_probe(shell("exit 1"), Duration::from_secs(1))
            .await
            .is_err());
        assert!(run_probe(shell("exec sleep 10"), Duration::from_millis(25))
            .await
            .unwrap_err()
            .contains("Délai"));
        assert!(
            run_probe(shell("head -c 40000 /dev/zero"), Duration::from_secs(1))
                .await
                .unwrap_err()
                .contains("volumineuse")
        );
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn linux_launch_script_preserves_paths_and_expands_home_without_injection() {
        use std::os::unix::fs::PermissionsExt;
        let root = std::env::temp_dir().join(format!(
            "tutor-wsl-script-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        let workdir = root.join("études $(touch injected); 'citation'");
        std::fs::create_dir_all(&workdir).unwrap();
        let adapter = root.join("adapter avec espaces");
        std::fs::write(&adapter, "#!/bin/sh\npwd\n").unwrap();
        std::fs::set_permissions(&adapter, std::fs::Permissions::from_mode(0o755)).unwrap();
        let mut target = wsl_target();
        target.adapter_path = Some(adapter.to_string_lossy().into_owned());
        for requested_cwd in [
            workdir.to_string_lossy().into_owned(),
            format!("~/{}", workdir.file_name().unwrap().to_string_lossy()),
        ] {
            let wsl_spec = build_wsl_launch("wsl.exe", &target, &requested_cwd).unwrap();
            let bash_spec = LaunchSpec {
                program: "/bin/bash".into(),
                args: wsl_spec.args[6..].to_vec(),
                env: vec![("HOME".into(), root.to_string_lossy().into_owned())],
                cwd: None,
            };
            let output = run_probe(bash_spec, Duration::from_secs(2)).await.unwrap();
            assert_eq!(
                String::from_utf8(output).unwrap().trim(),
                workdir.to_string_lossy()
            );
            assert!(!root.join("injected").exists());
            assert!(!workdir.join("injected").exists());
        }
        // MCP command arguments survive the same wrapper unchanged, including
        // spaces, option-like strings and shell syntax supplied as data.
        std::fs::write(&adapter, "#!/bin/sh\nprintf '%s\\000' \"$@\"\n").unwrap();
        let mut wsl_spec = build_wsl_launch("wsl.exe", &target, workdir.to_str().unwrap()).unwrap();
        wsl_spec.args.extend([
            "--local".into(),
            "argument $(touch injected) 'quoted'".into(),
        ]);
        let spec = LaunchSpec {
            program: "/bin/bash".into(),
            args: wsl_spec.args[6..].to_vec(),
            env: vec![("HOME".into(), root.to_string_lossy().into_owned())],
            cwd: None,
        };
        let output = run_probe(spec, Duration::from_secs(2)).await.unwrap();
        assert_eq!(output, b"--local\0argument $(touch injected) 'quoted'\0");
        assert!(!workdir.join("injected").exists());
        std::fs::remove_dir_all(root).unwrap();
    }

    #[cfg(target_os = "linux")]
    #[tokio::test]
    async fn wsl_wrapper_cleans_linux_descendants_when_adapter_exits() {
        use std::os::unix::fs::PermissionsExt;
        let root = std::env::temp_dir().join(format!(
            "tutor-wsl-cleanup-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        std::fs::create_dir_all(&root).unwrap();
        let adapter = root.join("adapter");
        std::fs::write(&adapter, "#!/bin/sh\nsleep 30 &\nprintf '%s\\n' \"$!\"\n").unwrap();
        std::fs::set_permissions(&adapter, std::fs::Permissions::from_mode(0o755)).unwrap();
        let mut target = wsl_target();
        target.adapter_path = Some(adapter.to_string_lossy().into_owned());
        let wsl_spec = build_wsl_launch("wsl.exe", &target, root.to_str().unwrap()).unwrap();
        let bash_spec = LaunchSpec {
            program: "/bin/bash".into(),
            args: wsl_spec.args[6..].to_vec(),
            env: vec![("HOME".into(), root.to_string_lossy().into_owned())],
            cwd: None,
        };
        let output = run_probe(bash_spec, Duration::from_secs(3)).await.unwrap();
        let pid: u32 = String::from_utf8(output).unwrap().trim().parse().unwrap();
        // A terminated orphan may briefly remain a zombie until init reaps it.
        if let Ok(stat) = std::fs::read_to_string(format!("/proc/{pid}/stat")) {
            let state = stat.rsplit_once(") ").unwrap().1.chars().next().unwrap();
            assert_eq!(
                state, 'Z',
                "adapter descendant remains alive after wrapper exit"
            );
        }
        std::fs::remove_dir_all(root).unwrap();
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn wsl_manual_override_and_read_only_mode_reach_the_linux_process() {
        use std::os::unix::fs::PermissionsExt;
        let root = std::env::temp_dir().join(format!(
            "tutor-wsl-override-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        std::fs::create_dir_all(&root).unwrap();
        let adapter = root.join("adapter");
        let codex = root.join("Codex $(touch injected) '日本語'");
        std::fs::write(
            &adapter,
            "#!/bin/sh\nprintf '%s\\000%s\\000' \"${CODEX_PATH-unset}\" \"$INITIAL_AGENT_MODE\"\n",
        )
        .unwrap();
        std::fs::write(&codex, "#!/bin/sh\nprintf 'codex-cli 0.164.0\\n'\n").unwrap();
        for path in [&adapter, &codex] {
            std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o755)).unwrap();
        }
        let mut target = wsl_target();
        target.adapter_path = Some(adapter.to_string_lossy().into_owned());
        target.codex_path = Some(codex.to_string_lossy().into_owned());
        for manual in [false, true] {
            target.id = if manual { "manual" } else { "wsl:fixture" }.into();
            let spec = build_wsl_launch("wsl.exe", &target, root.to_str().unwrap()).unwrap();
            assert_eq!(
                spec.args[13],
                if manual { codex.to_str().unwrap() } else { "" }
            );
            assert!(!spec.args[9].contains(codex.to_str().unwrap()));
            let local = LaunchSpec {
                program: "/bin/bash".into(),
                args: spec.args[6..].to_vec(),
                env: vec![
                    ("HOME".into(), root.to_string_lossy().into_owned()),
                    ("CODEX_PATH".into(), "must-not-leak-from-Windows".into()),
                    ("INITIAL_AGENT_MODE".into(), "agent-full-access".into()),
                ],
                cwd: None,
            };
            let output = run_probe(local, Duration::from_secs(2)).await.unwrap();
            let expected = format!(
                "{}\0read-only\0",
                if manual {
                    codex.to_str().unwrap()
                } else {
                    "unset"
                }
            );
            assert_eq!(output, expected.as_bytes());
        }
        let probe = wsl_codex_probe("wsl.exe", &target, codex.to_str().unwrap()).unwrap();
        let local_probe = LaunchSpec {
            program: "/bin/bash".into(),
            args: probe.args[6..].to_vec(),
            env: vec![("HOME".into(), root.to_string_lossy().into_owned())],
            cwd: None,
        };
        validate_codex_version(
            &run_probe(local_probe, Duration::from_secs(2))
                .await
                .unwrap(),
        )
        .unwrap();
        assert!(!root.join("injected").exists());
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn npm_shims_resolve_package_metadata_without_cmd_shell() {
        let root = std::env::temp_dir().join(format!(
            "tutor-discovery-npm-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        let package = root.join("node_modules/@agentclientprotocol/codex-acp");
        std::fs::create_dir_all(package.join("dist")).unwrap();
        std::fs::write(
            root.join("codex-acp.cmd"),
            "untrusted shell text is never parsed",
        )
        .unwrap();
        std::fs::write(
            package.join("package.json"),
            r#"{"bin":{"codex-acp":"dist/index.js"}}"#,
        )
        .unwrap();
        std::fs::write(package.join("dist/index.js"), "// adapter").unwrap();
        assert_eq!(
            npm_package_bin(
                &root.join("codex-acp.cmd"),
                "@agentclientprotocol/codex-acp",
                "codex-acp"
            )
            .unwrap(),
            package.join("dist/index.js").canonicalize().unwrap()
        );
        let node = root.join(if cfg!(windows) { "node.exe" } else { "node" });
        std::fs::write(&node, "native Node placeholder; not executed by this test").unwrap();
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            std::fs::set_permissions(&node, std::fs::Permissions::from_mode(0o755)).unwrap();
        }
        let spec =
            adapter_invocation(&root.join("codex-acp.cmd"), std::slice::from_ref(&root)).unwrap();
        assert_eq!(spec.program, node.to_string_lossy());
        assert_eq!(
            spec.args,
            vec![package
                .join("dist/index.js")
                .canonicalize()
                .unwrap()
                .to_string_lossy()
                .into_owned()]
        );
        std::fs::write(
            package.join("package.json"),
            r#"{"bin":{"codex-acp":"../../../outside.js"}}"#,
        )
        .unwrap();
        std::fs::write(root.join("outside.js"), "// outside").unwrap();
        assert!(npm_package_bin(
            &root.join("codex-acp.cmd"),
            "@agentclientprotocol/codex-acp",
            "codex-acp"
        )
        .is_err());
        std::fs::remove_dir_all(root).unwrap();
    }
}
