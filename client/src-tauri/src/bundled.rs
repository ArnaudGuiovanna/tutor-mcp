// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! Resolve packaged agents independently of PATH and all webview-supplied paths.
use crate::{discovery::AgentTarget, launch::LaunchSpec};
use serde::Deserialize;
use sha2::{Digest, Sha256};
use std::{
    collections::HashSet,
    path::{Component, Path, PathBuf},
    sync::OnceLock,
};

static ROOT: OnceLock<PathBuf> = OnceLock::new();
pub fn configure(root: PathBuf) {
    let _ = ROOT.set(root);
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Manifest {
    format: u32,
    platform: String,
    node: String,
    agents: Vec<Agent>,
    files: Vec<Asset>,
}
#[derive(Deserialize)]
struct Agent {
    id: String,
    label: String,
    version: String,
    entry: String,
}
#[derive(Deserialize)]
struct Asset {
    path: String,
    bytes: u64,
    sha256: String,
}

fn safe_path(root: &Path, path: &str) -> Result<PathBuf, String> {
    if path.is_empty()
        || path.contains(['\\', ':'])
        || Path::new(path)
            .components()
            .any(|c| !matches!(c, Component::Normal(_)))
    {
        return Err("Chemin de ressource intégrée invalide.".into());
    }
    let target = root.join(path);
    let canonical = target
        .canonicalize()
        .map_err(|_| "Ressource intégrée manquante. Réinstallez Tutor.")?;
    if !canonical.starts_with(
        root.canonicalize()
            .map_err(|_| "Ressources intégrées manquantes.")?,
    ) || !canonical.is_file()
    {
        return Err("Ressource intégrée hors du dossier de l’application.".into());
    }
    Ok(canonical)
}
fn manifest(root: &Path) -> Result<Manifest, String> {
    let path = root.join("runtime-manifest.json");
    if std::fs::metadata(&path)
        .map_err(|_| "Agents intégrés absents.")?
        .len()
        > 4 * 1024 * 1024
    {
        return Err("Inventaire des agents trop volumineux.".into());
    }
    let m: Manifest = serde_json::from_slice(
        &std::fs::read(path).map_err(|_| "Inventaire des agents illisible.")?,
    )
    .map_err(|_| "Inventaire des agents invalide.")?;
    if m.format != 1 || m.platform != "windows-x64" || m.agents.len() != 2 || m.files.len() > 20000
    {
        return Err("Version des agents intégrés incompatible.".into());
    }
    Ok(m)
}
pub fn targets() -> Vec<AgentTarget> {
    let Some(root) = ROOT.get() else {
        return vec![];
    };
    let Ok(m) = manifest(root) else { return vec![] };
    m.agents
        .into_iter()
        .map(|a| AgentTarget {
            id: format!("bundled:{}", a.id),
            label: format!("{} — intégré", a.label),
            environment: "native".into(),
            distribution: None,
            wsl_user: None,
            adapter_path: None,
            codex_path: None,
            adapter_version: Some(a.version),
            codex_version: None,
            available: true,
            detail: "Inclus dans Tutor. Connectez ensuite votre compte auprès du fournisseur."
                .into(),
        })
        .collect()
}
pub fn launch(id: &str, cwd: &str) -> Result<LaunchSpec, String> {
    let root = ROOT
        .get()
        .ok_or("Les agents intégrés ne sont pas disponibles dans cette installation.")?;
    launch_from(root, id, cwd)
}
fn launch_from(root: &Path, id: &str, cwd: &str) -> Result<LaunchSpec, String> {
    let m = manifest(root)?;
    let agent = m
        .agents
        .iter()
        .find(|a| id == format!("bundled:{}", a.id))
        .ok_or("Agent intégré inconnu.")?;
    // Check the complete immutable package, including its native child runtime.
    // A changed installation never falls back to another executable on PATH.
    let mut seen = HashSet::new();
    for asset in &m.files {
        if !seen.insert(asset.path.clone()) || asset.bytes > 1024 * 1024 * 1024 {
            return Err("Inventaire des ressources incohérent.".into());
        }
        let path = safe_path(root, &asset.path)?;
        let mut file = std::fs::File::open(&path).map_err(|_| "Ressource illisible.")?;
        if file.metadata().map_err(|_| "Ressource illisible.")?.len() != asset.bytes {
            return Err("Agent intégré modifié. Réinstallez Tutor.".into());
        }
        let mut digest = Sha256::new();
        std::io::copy(&mut file, &mut digest).map_err(|_| "Vérification de l’agent impossible.")?;
        if format!("{:x}", digest.finalize()) != asset.sha256 {
            return Err("Agent intégré modifié. Réinstallez Tutor.".into());
        }
    }
    if !seen.contains(&m.node) || !seen.contains(&agent.entry) {
        return Err("Inventaire incomplet.".into());
    }
    let node = safe_path(root, &m.node)?;
    let entry = safe_path(root, &agent.entry)?;
    let mut paths = vec![node.parent().unwrap().to_path_buf()];
    paths.extend(std::env::split_paths(
        &std::env::var_os("PATH").unwrap_or_default(),
    ));
    let path = std::env::join_paths(paths).map_err(|_| "PATH de l’agent invalide.")?;
    let mut env = vec![("PATH".into(), path.to_string_lossy().into_owned())];
    let runtime = if id == "bundled:codex" {
        (
            "CODEX_PATH",
            "node_modules/@openai/codex-win32-x64/vendor/x86_64-pc-windows-msvc/bin/codex.exe",
        )
    } else {
        (
            "CLAUDE_CODE_EXECUTABLE",
            "node_modules/@anthropic-ai/claude-agent-sdk-win32-x64/claude.exe",
        )
    };
    if !seen.contains(runtime.1) {
        return Err("Runtime fournisseur absent de l’inventaire.".into());
    }
    env.push((
        runtime.0.into(),
        safe_path(root, runtime.1)?.to_string_lossy().into_owned(),
    ));
    Ok(LaunchSpec {
        program: node.to_string_lossy().into_owned(),
        args: vec![entry.to_string_lossy().into_owned()],
        env,
        cwd: Some(PathBuf::from(cwd)),
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn packaged_launch_pins_vendor_runtime_and_rejects_changed_bytes() {
        let directory = tempfile::tempdir().unwrap();
        let root = directory.path();
        let runtime =
            "node_modules/@openai/codex-win32-x64/vendor/x86_64-pc-windows-msvc/bin/codex.exe";
        let mut assets = vec![];
        for name in [
            "node/node.exe",
            "node_modules/codex.js",
            "node_modules/claude.js",
            runtime,
        ] {
            let path = root.join(name);
            std::fs::create_dir_all(path.parent().unwrap()).unwrap();
            std::fs::write(&path, b"fixture").unwrap();
            assets.push(serde_json::json!({"path":name,"bytes":7,"sha256":format!("{:x}",Sha256::digest(b"fixture"))}));
        }
        std::fs::write(root.join("runtime-manifest.json"),serde_json::to_vec(&serde_json::json!({"format":1,"platform":"windows-x64","node":"node/node.exe","agents":[{"id":"codex","label":"Codex","version":"test","entry":"node_modules/codex.js"},{"id":"claude","label":"Claude","version":"test","entry":"node_modules/claude.js"}],"files":assets})).unwrap()).unwrap();
        let spec = launch_from(root, "bundled:codex", root.to_str().unwrap()).unwrap();
        assert!(Path::new(&spec.program).is_absolute());
        assert!(spec
            .env
            .iter()
            .any(|(key, value)| key == "CODEX_PATH" && value.ends_with("codex.exe")));
        assert!(launch_from(root, "bundled:unknown", root.to_str().unwrap()).is_err());
        std::fs::write(root.join(runtime), b"changed").unwrap();
        assert!(launch_from(root, "bundled:codex", root.to_str().unwrap())
            .unwrap_err()
            .contains("modifié"));
    }
    #[test]
    fn resource_paths_cannot_escape() {
        let root = std::env::temp_dir();
        for p in [
            "../secret",
            "/etc/passwd",
            "C:/windows/node.exe",
            "a\\b",
            "",
        ] {
            assert!(safe_path(&root, p).is_err());
        }
    }
}
