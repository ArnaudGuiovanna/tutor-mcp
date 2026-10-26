// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! Resolve packaged agents independently of PATH and all webview-supplied paths.
use crate::{discovery::AgentTarget, launch::LaunchSpec};
use serde::Deserialize;
use sha2::{Digest, Sha256};
use std::{
    collections::{HashMap, HashSet},
    path::{Component, Path, PathBuf},
    sync::{Mutex, OnceLock},
    time::SystemTime,
};

static ROOT: OnceLock<PathBuf> = OnceLock::new();
// Only verified bytes are cached. Every launch still revalidates canonical
// containment, file identity and metadata; replaced/changed assets are hashed
// again. This cache is process-local and cannot survive an application upgrade.
static VERIFIED: OnceLock<Mutex<HashMap<PathBuf, VerifiedAsset>>> = OnceLock::new();
#[derive(Clone, PartialEq, Eq)]
struct FileStamp {
    len: u64,
    modified: Option<SystemTime>,
    created: Option<SystemTime>,
    #[cfg(unix)]
    identity: (u64, u64, i64, i64),
    #[cfg(windows)]
    changed: i64,
}
struct VerifiedAsset {
    stamp: FileStamp,
    expected: String,
}
fn file_stamp(file: &std::fs::File) -> Result<FileStamp, String> {
    let m = file.metadata().map_err(|_| "Ressource illisible.")?;
    #[cfg(unix)]
    use std::os::unix::fs::MetadataExt;
    #[cfg(windows)]
    let changed = {
        use std::os::windows::io::AsRawHandle;
        use windows_sys::Win32::Storage::FileSystem::{
            FileBasicInfo, GetFileInformationByHandleEx, FILE_BASIC_INFO,
        };
        let mut basic = std::mem::MaybeUninit::<FILE_BASIC_INFO>::zeroed();
        // SAFETY: the file handle remains owned/live, and the output points to
        // an appropriately sized FILE_BASIC_INFO buffer.
        if unsafe {
            GetFileInformationByHandleEx(
                file.as_raw_handle(),
                FileBasicInfo,
                basic.as_mut_ptr().cast(),
                std::mem::size_of::<FILE_BASIC_INFO>() as u32,
            )
        } == 0
        {
            return Err("Vérification des métadonnées de l’agent impossible.".into());
        }
        // SAFETY: the successful call initialized the entire output structure.
        unsafe { basic.assume_init() }.ChangeTime
    };
    Ok(FileStamp {
        len: m.len(),
        modified: m.modified().ok(),
        created: m.created().ok(),
        #[cfg(unix)]
        identity: (m.dev(), m.ino(), m.ctime(), m.ctime_nsec()),
        #[cfg(windows)]
        changed,
    })
}
pub fn configure(root: PathBuf) {
    let _ = ROOT.set(root);
}

#[derive(Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
struct Manifest {
    format: u32,
    platform: String,
    node: String,
    agents: Vec<Agent>,
    files: Vec<Asset>,
}
#[derive(Clone, Deserialize)]
struct Agent {
    id: String,
    label: String,
    version: String,
    entry: String,
}
#[derive(Clone, Deserialize)]
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
        let stamp = file_stamp(&file)?;
        if stamp.len != asset.bytes {
            return Err("Agent intégré modifié. Réinstallez Tutor.".into());
        }
        let cache = VERIFIED.get_or_init(|| Mutex::new(HashMap::new()));
        let already_verified = cache
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .get(&path)
            .is_some_and(|known| known.stamp == stamp && known.expected == asset.sha256);
        if !already_verified {
            let mut digest = Sha256::new();
            std::io::copy(&mut file, &mut digest)
                .map_err(|_| "Vérification de l’agent impossible.")?;
            if format!("{:x}", digest.finalize()) != asset.sha256 || file_stamp(&file)? != stamp {
                cache
                    .lock()
                    .unwrap_or_else(|e| e.into_inner())
                    .remove(&path);
                return Err("Agent intégré modifié. Réinstallez Tutor.".into());
            }
            let mut verified = cache.lock().unwrap_or_else(|e| e.into_inner());
            if verified.len() >= 20000 {
                verified.clear();
            }
            verified.insert(
                path,
                VerifiedAsset {
                    stamp,
                    expected: asset.sha256.clone(),
                },
            );
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
        args: vec![crate::discovery::node_script_argument(&entry)],
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
    #[cfg(windows)]
    #[tokio::test]
    #[ignore = "requires the actual packaged Windows resources"]
    async fn installed_bundled_agents_use_the_same_rust_launch_as_the_application() {
        use std::sync::Arc;
        let root = std::env::var_os("TUTOR_PACKAGED_ROOT")
            .expect("TUTOR_PACKAGED_ROOT must name the packaged agents");
        // A verbatim resource root exercises the canonical path returned by
        // Tauri, including the Node entry-path regression for both agents.
        let root = PathBuf::from(root).canonicalize().unwrap();
        configure(root);
        let mut reports = Vec::new();
        for target in targets() {
            let directory = tempfile::Builder::new()
                .prefix("Tutor intégrés ")
                .tempdir()
                .unwrap();
            let backend =
                crate::backend::Backend::new(directory.path(), Arc::new(|_, _| {})).unwrap();
            let mut settings = backend.settings().await;
            settings.target_id = target.id.clone();
            backend.save_settings(settings).await.unwrap();
            let cold = std::time::Instant::now();
            let response = backend
                .connect_agent(target.clone(), String::new())
                .await
                .unwrap_or_else(|error| {
                    panic!("{} production Rust launch failed: {error}", target.id)
                });
            let cold_ms = cold.elapsed().as_millis();
            assert_eq!(response["protocolVersion"], 1);
            assert!(response.get("agentInfo").is_some());
            backend.disconnect_agent().await.unwrap();
            assert_eq!(backend.snapshot().await["agentConnected"], false);
            let warm = std::time::Instant::now();
            backend
                .connect_agent(target.clone(), String::new())
                .await
                .unwrap();
            let warm_ms = warm.elapsed().as_millis();
            backend.disconnect_agent().await.unwrap();
            reports.push(serde_json::json!({"agent":target.id,"agentInfo":response["agentInfo"],"coldStartMs":cold_ms,"warmStartMs":warm_ms,"productionRustBackend":true,"unicodeWorkingDirectory":true,"initialize":"passed","shutdown":"passed","modelPromptTested":false}));
        }
        assert_eq!(reports.len(), 2);
        let report = serde_json::json!({"format":1,"sourceVersion":env!("CARGO_PKG_VERSION"),"platform":"windows-x64","agents":reports});
        if let Some(path) = std::env::var_os("TUTOR_PACKAGED_REPORT") {
            std::fs::write(path, serde_json::to_vec_pretty(&report).unwrap()).unwrap();
        }
        println!("PACKAGED_RUST_QUALIFICATION {report}");
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
