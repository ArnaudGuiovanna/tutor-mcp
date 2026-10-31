// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! Trusted launch configuration for the packaged `tutor-local-agent`.
//!
//! Configuration belongs to the desktop process, never the webview target:
//! - TUTOR_LOCAL_AGENT_PATH: absolute native executable (or the exact sidecar
//!   name beside the running app when unset; no PATH/development-tree search).
//! - TUTOR_LOCAL_MODEL_PATH: optional developer override of the managed GGUF.
//! - TUTOR_LOCAL_GPU_LAYERS: 0..=36, default 0 until P2000 calibration.
//! - TUTOR_LOCAL_THREADS: 1..=32, default available CPUs capped at 6.
//! - TUTOR_LOCAL_BATCH: 1..=512, default 128.
//!
//! Context/output remain fixed at 2048/192 for the initial hardware profile.
//! File signatures only detect obvious misconfiguration. Model identity, GGUF
//! compatibility and ACP capabilities are established by the native agent/ACP
//! handshake; the agent must report empty authMethods. The desktop supplies the
//! installed resource root and managed model path, never a webview-supplied target.

use crate::{discovery::AgentTarget, launch::LaunchSpec};
use sha2::{Digest, Sha256};
use std::{
    ffi::OsString,
    fs::File,
    io::Read,
    path::{Path, PathBuf},
    sync::OnceLock,
};

struct InstalledPaths {
    root: PathBuf,
    model: PathBuf,
}
static INSTALLED: OnceLock<InstalledPaths> = OnceLock::new();

pub fn configure(root: PathBuf, model: PathBuf) {
    let _ = INSTALLED.set(InstalledPaths { root, model });
}

pub fn packaged_available() -> bool {
    INSTALLED
        .get()
        .is_some_and(|paths| verified_executable(&paths.root).is_ok())
}

fn verified_executable(root: &Path) -> Result<PathBuf, String> {
    let path = root.join(EXECUTABLE);
    checked_file(
        path.clone().into_os_string(),
        "L’agent local installé",
        true,
    )?;
    let receipt = root.join("local-agent-build.json");
    let mut bytes = Vec::new();
    File::open(&receipt)
        .map_err(|_| "Réinstallez Tutor : reçu de l’agent local absent.")?
        .take(65537)
        .read_to_end(&mut bytes)
        .map_err(|_| "Le reçu de l’agent local est illisible.")?;
    if bytes.len() > 65536 {
        return Err("Le reçu de l’agent local est invalide.".into());
    }
    let value: serde_json::Value =
        serde_json::from_slice(&bytes).map_err(|_| "Le reçu de l’agent local est invalide.")?;
    if value["backend"] != "llama.cpp CPU"
        || value["target"] != "x86_64-pc-windows-msvc"
        || value["staticCrt"] != true
    {
        return Err("Le profil de l’agent local installé est incompatible.".into());
    }
    let mut file = File::open(&path).map_err(|_| "L’agent local installé est illisible.")?;
    let mut digest = Sha256::new();
    let mut buffer = [0; 65536];
    loop {
        let size = file
            .read(&mut buffer)
            .map_err(|_| "L’agent local installé est illisible.")?;
        if size == 0 {
            break;
        }
        digest.update(&buffer[..size]);
    }
    if value["sha256"].as_str() != Some(format!("{:x}", digest.finalize()).as_str()) {
        return Err("Réinstallez Tutor : l’agent local ne correspond pas à son reçu.".into());
    }
    Ok(path)
}

pub(crate) const TARGET_ID: &str = "local:qwen";
const EXECUTABLE: &str = if cfg!(windows) {
    "tutor-local-agent.exe"
} else {
    "tutor-local-agent"
};

struct Configuration {
    executable: String,
    model: String,
    gpu_layers: u32,
    threads: u32,
    batch: u32,
}

impl Configuration {
    fn read() -> Result<Self, String> {
        let installed = INSTALLED.get();
        let packaged = if std::env::var_os("TUTOR_LOCAL_AGENT_PATH").is_none() {
            installed
                .map(|paths| verified_executable(&paths.root))
                .transpose()?
        } else {
            None
        };
        if std::env::var_os("TUTOR_LOCAL_MODEL_PATH").is_none()
            && installed.is_some_and(|paths| !paths.model.is_file())
        {
            return Err("Installez le modèle Qwen dans Paramètres → Connexion et outils.".into());
        }
        Self::from_environment(
            |name| {
                std::env::var_os(name).or_else(|| match name {
                    "TUTOR_LOCAL_AGENT_PATH" => {
                        packaged.as_ref().map(|path| path.clone().into_os_string())
                    }
                    "TUTOR_LOCAL_MODEL_PATH" => {
                        installed.map(|paths| paths.model.clone().into_os_string())
                    }
                    _ => None,
                })
            },
            std::env::current_exe().ok().as_deref(),
            std::thread::available_parallelism().map_or(1, usize::from),
        )
    }

    // Explicit inputs keep tests independent of process-global environment.
    fn from_environment(
        get: impl Fn(&str) -> Option<OsString>,
        app_executable: Option<&Path>,
        available_threads: usize,
    ) -> Result<Self, String> {
        let executable = match get("TUTOR_LOCAL_AGENT_PATH") {
            Some(value) => value,
            None => app_executable
                .and_then(Path::parent)
                .map(|directory| directory.join(EXECUTABLE).into_os_string())
                .ok_or("Indiquez TUTOR_LOCAL_AGENT_PATH : binaire local introuvable.")?,
        };
        let executable = checked_file(executable, "TUTOR_LOCAL_AGENT_PATH", true)?;
        let model = checked_file(
            get("TUTOR_LOCAL_MODEL_PATH")
                .ok_or("Indiquez TUTOR_LOCAL_MODEL_PATH : modèle GGUF local absent.")?,
            "TUTOR_LOCAL_MODEL_PATH",
            false,
        )?;
        Ok(Self {
            executable,
            model,
            gpu_layers: number(&get, "TUTOR_LOCAL_GPU_LAYERS", 0, 0, 36)?,
            threads: number(
                &get,
                "TUTOR_LOCAL_THREADS",
                available_threads.clamp(1, 6) as u32,
                1,
                32,
            )?,
            batch: number(&get, "TUTOR_LOCAL_BATCH", 128, 1, 512)?,
        })
    }

    fn launch(self, cwd: &str) -> Result<LaunchSpec, String> {
        validate_text(cwd, "Le dossier de travail")?;
        let directory = Path::new(cwd);
        if !directory.is_absolute() || !directory.is_dir() {
            return Err("Choisissez un dossier de travail existant avec un chemin absolu.".into());
        }
        Ok(LaunchSpec {
            program: self.executable,
            args: vec![
                "--model".into(),
                self.model,
                "--context".into(),
                "2048".into(),
                "--max-output".into(),
                "192".into(),
                "--gpu-layers".into(),
                self.gpu_layers.to_string(),
                "--threads".into(),
                self.threads.to_string(),
                "--batch".into(),
                self.batch.to_string(),
            ],
            env: vec![],
            cwd: Some(directory.to_path_buf()),
        })
    }
}

pub(crate) fn discover() -> AgentTarget {
    target_from(Configuration::read())
}

fn target_from(configuration: Result<Configuration, String>) -> AgentTarget {
    let mut target = AgentTarget {
        id: TARGET_ID.into(),
        label: "Qwen — local (expérimental)".into(),
        environment: "native".into(),
        distribution: None,
        wsl_user: None,
        adapter_path: None,
        codex_path: None,
        adapter_version: None,
        codex_version: None,
        available: false,
        detail: String::new(),
    };
    match configuration {
        Ok(configuration) => {
            target.adapter_path = Some(configuration.executable);
            target.available = true;
            target.detail = "Agent local expérimental : conversation, reprise et lectures Tutor. La préparation et l’évaluation des activités restent à intégrer. Le modèle sera vérifié au premier message.".into();
        }
        Err(error) => target.detail = error,
    }
    target
}

/// Re-read/revalidate trusted configuration at launch: a serialized target is
/// descriptive, not permission to substitute an executable or add arguments.
pub(crate) fn launch(cwd: &str) -> Result<LaunchSpec, String> {
    Configuration::read()?.launch(cwd)
}

/// The backend supplies its private profile directory. Neither the webview's
/// serialized target nor the working directory selects journal storage.
pub(crate) fn bind_data_directory(spec: &mut LaunchSpec, directory: &Path) -> Result<(), String> {
    if !directory.is_absolute() || !directory.is_dir() {
        return Err("Le stockage privé de l’agent local est indisponible.".into());
    }
    let data_directory = directory.join("local-agent");
    let path = data_directory
        .to_str()
        .ok_or("Le chemin du stockage privé de l’agent local est invalide.")?;
    validate_text(path, "Le stockage privé de l’agent local")?;
    if spec
        .args
        .iter()
        .any(|arg| arg == "--data-dir" || arg.starts_with("--data-dir="))
    {
        return Err("Le stockage privé de l’agent local est déjà configuré.".into());
    }
    spec.args.extend(["--data-dir".into(), path.into()]);
    Ok(())
}

fn validate_text(value: &str, label: &str) -> Result<(), String> {
    if value.trim().is_empty() || value.len() > 32768 || value.chars().any(char::is_control) {
        return Err(format!(
            "{label} est vide ou contient des caractères interdits."
        ));
    }
    Ok(())
}

fn checked_file(value: OsString, name: &str, executable: bool) -> Result<String, String> {
    let value = value
        .into_string()
        .map_err(|_| format!("{name} doit contenir un chemin Unicode valide."))?;
    validate_text(&value, name)?;
    let path = Path::new(&value);
    if !path.is_absolute() || !path.is_file() {
        return Err(format!(
            "{name} doit désigner un fichier existant avec un chemin absolu."
        ));
    }
    if executable {
        #[cfg(windows)]
        if !path
            .extension()
            .and_then(|extension| extension.to_str())
            .is_some_and(|extension| extension.eq_ignore_ascii_case("exe"))
        {
            return Err(format!(
                "{name} doit désigner un exécutable natif .exe, pas un script."
            ));
        }
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let metadata = path
                .metadata()
                .map_err(|_| format!("{name} est illisible."))?;
            if metadata.permissions().mode() & 0o111 == 0 {
                return Err(format!("{name} n’est pas un fichier exécutable."));
            }
        }
    }
    // Read only a signature, never the multi-gigabyte GGUF during discovery.
    let mut signature = [0_u8; 4];
    File::open(path)
        .and_then(|mut file| file.read_exact(&mut signature))
        .map_err(|_| format!("{name} est illisible ou incomplet."))?;
    if executable && !native_signature(signature) {
        return Err(format!(
            "{name} doit désigner un binaire natif, pas un script ou un wrapper."
        ));
    }
    if !executable && &signature != b"GGUF" {
        return Err(format!("{name} n’est pas un fichier GGUF."));
    }
    Ok(value)
}

fn native_signature(signature: [u8; 4]) -> bool {
    if cfg!(windows) {
        signature.starts_with(b"MZ")
    } else if cfg!(target_os = "macos") {
        matches!(
            signature,
            [0xfe, 0xed, 0xfa, 0xce]
                | [0xce, 0xfa, 0xed, 0xfe]
                | [0xfe, 0xed, 0xfa, 0xcf]
                | [0xcf, 0xfa, 0xed, 0xfe]
                | [0xca, 0xfe, 0xba, 0xbe]
                | [0xbe, 0xba, 0xfe, 0xca]
                | [0xca, 0xfe, 0xba, 0xbf]
                | [0xbf, 0xba, 0xfe, 0xca]
        )
    } else {
        &signature == b"\x7fELF"
    }
}

fn number(
    get: &impl Fn(&str) -> Option<OsString>,
    name: &str,
    default: u32,
    min: u32,
    max: u32,
) -> Result<u32, String> {
    let Some(value) = get(name) else {
        return Ok(default);
    };
    let value = value.to_str().and_then(|text| {
        text.bytes()
            .all(|byte| byte.is_ascii_digit())
            .then(|| text.parse::<u32>().ok())
            .flatten()
    });
    value
        .filter(|value| (min..=max).contains(value))
        .ok_or_else(|| format!("{name} doit être un entier compris entre {min} et {max}."))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::{collections::HashMap, path::PathBuf};

    #[tokio::test]
    #[ignore = "requires TUTOR_LOCAL_AGENT_PATH and TUTOR_LOCAL_MODEL_PATH"]
    async fn native_local_agent_uses_production_launch_and_acp_transport() {
        let target = discover();
        assert!(target.available, "{}", target.detail);
        let root = tempfile::Builder::new()
            .prefix("tutor ACP élève 日本語 ")
            .tempdir()
            .unwrap();
        let spec = crate::discovery::launch_spec(&target, root.path().to_str().unwrap())
            .await
            .unwrap();
        let (events, _receiver) = tokio::sync::mpsc::unbounded_channel();
        let rpc = crate::acp::AcpClient::spawn(spec, events).await.unwrap();
        let result = async {
            let initialized = rpc
                .request(
                    "initialize",
                    serde_json::json!({
                        "protocolVersion":1,"clientCapabilities":{},
                        "clientInfo":{"name":"tutor-native-qualification","version":"1"}
                    }),
                )
                .await?;
            if initialized["protocolVersion"] != 1
                || initialized["authMethods"] != serde_json::json!([])
                || initialized["agentInfo"]["name"] != "tutor-local-agent"
            {
                return Err("unexpected native handshake".to_owned());
            }
            let session = rpc
                .request(
                    "session/new",
                    serde_json::json!({
                        "cwd":root.path().to_str().unwrap(),"mcpServers":[]
                    }),
                )
                .await?;
            if session["sessionId"].as_str().is_none() {
                return Err("missing native session".to_owned());
            }
            Ok::<_, String>(())
        }
        .await;
        let stopped = rpc.stop().await;
        result.unwrap();
        stopped.unwrap();
        // This proves the real native launch/protocol path, not generation.
    }

    struct Fixture {
        root: tempfile::TempDir,
        executable: PathBuf,
        model: PathBuf,
        environment: HashMap<String, OsString>,
    }

    impl Fixture {
        fn new() -> Self {
            let root = tempfile::Builder::new()
                .prefix("tutor local élève 日本語 ")
                .tempdir()
                .unwrap();
            let executable = root.path().join(EXECUTABLE);
            // A native executable fixture, not a shell script. It is never run.
            let mut signature = [0; 4];
            File::open(std::env::current_exe().unwrap())
                .unwrap()
                .read_exact(&mut signature)
                .unwrap();
            std::fs::write(&executable, signature).unwrap();
            #[cfg(unix)]
            {
                use std::os::unix::fs::PermissionsExt;
                std::fs::set_permissions(&executable, std::fs::Permissions::from_mode(0o755))
                    .unwrap();
            }
            let model = root
                .path()
                .join("Qwen 'élève' 日本語 $(touch injected); &.gguf");
            std::fs::write(&model, b"GGUF\x03\0\0\0").unwrap();
            let environment = HashMap::from([
                (
                    "TUTOR_LOCAL_AGENT_PATH".into(),
                    executable.as_os_str().to_owned(),
                ),
                (
                    "TUTOR_LOCAL_MODEL_PATH".into(),
                    model.as_os_str().to_owned(),
                ),
            ]);
            Self {
                root,
                executable,
                model,
                environment,
            }
        }

        fn configuration(&self) -> Result<Configuration, String> {
            Configuration::from_environment(
                |name| self.environment.get(name).cloned(),
                Some(&self.root.path().join("Tutor.exe")),
                12,
            )
        }
    }

    #[test]
    fn installed_agent_must_match_the_cpu_build_receipt() {
        let fixture = Fixture::new();
        let receipt_path = fixture.root.path().join("local-agent-build.json");
        assert!(verified_executable(fixture.root.path()).is_err());
        let mut receipt = serde_json::json!({
            "backend": "llama.cpp CPU", "target": "x86_64-pc-windows-msvc", "staticCrt": true,
            "sha256": format!("{:x}", Sha256::digest(std::fs::read(&fixture.executable).unwrap()))
        });
        std::fs::write(&receipt_path, serde_json::to_vec(&receipt).unwrap()).unwrap();
        assert_eq!(
            verified_executable(fixture.root.path()).unwrap(),
            fixture.executable
        );
        receipt["backend"] = serde_json::json!("CUDA");
        std::fs::write(&receipt_path, serde_json::to_vec(&receipt).unwrap()).unwrap();
        assert!(verified_executable(fixture.root.path()).is_err());
        receipt["backend"] = serde_json::json!("llama.cpp CPU");
        receipt["sha256"] = serde_json::json!("0".repeat(64));
        std::fs::write(&receipt_path, serde_json::to_vec(&receipt).unwrap()).unwrap();
        assert!(verified_executable(fixture.root.path()).is_err());
    }

    #[test]
    fn launch_preserves_unicode_and_shell_characters_as_individual_arguments() {
        let fixture = Fixture::new();
        let spec = fixture
            .configuration()
            .unwrap()
            .launch(fixture.root.path().to_str().unwrap())
            .unwrap();
        assert_eq!(spec.program, fixture.executable.to_str().unwrap());
        assert_eq!(
            spec.args,
            [
                "--model",
                fixture.model.to_str().unwrap(),
                "--context",
                "2048",
                "--max-output",
                "192",
                "--gpu-layers",
                "0",
                "--threads",
                "6",
                "--batch",
                "128",
            ]
        );
        assert_eq!(spec.cwd.as_deref(), Some(fixture.root.path()));
        assert!(spec.env.is_empty());
        assert!(!fixture.root.path().join("injected").exists());
    }

    #[test]
    fn journal_directory_comes_from_private_appdata_not_the_working_directory() {
        let fixture = Fixture::new();
        let work = tempfile::tempdir().unwrap();
        let mut spec = fixture
            .configuration()
            .unwrap()
            .launch(work.path().to_str().unwrap())
            .unwrap();
        let original = spec.args.clone();
        bind_data_directory(&mut spec, fixture.root.path()).unwrap();
        assert_eq!(&spec.args[..original.len()], original);
        assert_eq!(
            &spec.args[original.len()..],
            [
                "--data-dir",
                fixture.root.path().join("local-agent").to_str().unwrap()
            ]
        );
        assert_eq!(spec.cwd.as_deref(), Some(work.path()));
        assert!(spec.env.is_empty());
        assert!(bind_data_directory(&mut spec, work.path()).is_err());
    }

    #[test]
    fn invalid_private_directory_does_not_modify_the_launch() {
        let fixture = Fixture::new();
        let mut spec = fixture
            .configuration()
            .unwrap()
            .launch(fixture.root.path().to_str().unwrap())
            .unwrap();
        let original = spec.args.clone();
        for directory in [
            Path::new("relative"),
            fixture.model.as_path(),
            &fixture.root.path().join("absent"),
        ] {
            assert!(bind_data_directory(&mut spec, directory).is_err());
            assert_eq!(spec.args, original);
        }
        spec.args.push("--data-dir=untrusted".into());
        assert!(bind_data_directory(&mut spec, fixture.root.path()).is_err());
    }

    #[test]
    fn discovery_is_honest_and_preserves_a_stable_native_identity() {
        let mut fixture = Fixture::new();
        let target = target_from(fixture.configuration());
        assert_eq!(target.id, "local:qwen");
        assert_eq!(target.environment, "native");
        assert!(target.available);
        assert!(target.codex_path.is_none());
        assert!(target.codex_version.is_none());
        assert!(target.adapter_version.is_none());
        assert!(target.distribution.is_none());
        assert!(target.wsl_user.is_none());
        fixture.environment.remove("TUTOR_LOCAL_MODEL_PATH");
        let missing = target_from(fixture.configuration());
        assert_eq!(missing.id, target.id);
        assert!(!missing.available);
        assert!(missing.detail.contains("TUTOR_LOCAL_MODEL_PATH"));
        fixture.environment.remove("TUTOR_LOCAL_AGENT_PATH");
        std::fs::remove_file(&fixture.executable).unwrap();
        let missing = target_from(fixture.configuration());
        assert!(!missing.available);
        assert!(missing.detail.contains("TUTOR_LOCAL_AGENT_PATH"));
    }

    #[test]
    fn sidecar_is_used_only_when_explicit_agent_path_is_unset() {
        let mut fixture = Fixture::new();
        fixture.environment.remove("TUTOR_LOCAL_AGENT_PATH");
        assert_eq!(
            fixture.configuration().unwrap().executable,
            fixture.executable.to_str().unwrap()
        );
        fixture.environment.insert(
            "TUTOR_LOCAL_AGENT_PATH".into(),
            fixture.root.path().join("absent").into_os_string(),
        );
        assert!(fixture.configuration().is_err());
        fixture
            .environment
            .insert("TUTOR_LOCAL_AGENT_PATH".into(), OsString::new());
        assert!(fixture.configuration().is_err());
        fixture.environment.remove("TUTOR_LOCAL_AGENT_PATH");
        assert!(Configuration::from_environment(
            |name| fixture.environment.get(name).cloned(),
            None,
            12
        )
        .is_err());
    }

    #[test]
    fn missing_relative_directory_control_and_non_model_files_are_rejected() {
        let mut fixture = Fixture::new();
        for name in ["TUTOR_LOCAL_AGENT_PATH", "TUTOR_LOCAL_MODEL_PATH"] {
            let original = fixture.environment[name].clone();
            for value in [
                OsString::from("relative.gguf"),
                OsString::new(),
                fixture.root.path().as_os_str().to_owned(),
                fixture.root.path().join("missing.gguf").into_os_string(),
                OsString::from("/tmp/invalid\npath"),
                OsString::from("/tmp/invalid\0path"),
            ] {
                fixture.environment.insert(name.into(), value);
                let target = target_from(fixture.configuration());
                assert!(!target.available);
                assert!(target.detail.contains(name));
            }
            fixture.environment.insert(name.into(), original);
        }
        for bytes in [b"".as_slice(), b"GG", b"not GGUF"] {
            std::fs::write(&fixture.model, bytes).unwrap();
            assert!(fixture.configuration().is_err());
        }
    }

    #[test]
    fn scripts_and_non_executable_files_are_rejected_without_running_them() {
        let fixture = Fixture::new();
        std::fs::write(&fixture.executable, "#!/bin/sh\ntouch injected\n").unwrap();
        assert!(fixture.configuration().is_err());
        assert!(!fixture.root.path().join("injected").exists());
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            std::fs::write(&fixture.executable, b"\x7fELF").unwrap();
            std::fs::set_permissions(&fixture.executable, std::fs::Permissions::from_mode(0o644))
                .unwrap();
            assert!(fixture.configuration().is_err());
        }
    }

    #[test]
    fn hardware_overrides_are_strict_bounded_and_never_command_fragments() {
        let mut fixture = Fixture::new();
        for (name, valid, invalid) in [
            ("TUTOR_LOCAL_GPU_LAYERS", "36", "37"),
            ("TUTOR_LOCAL_THREADS", "32", "33"),
            ("TUTOR_LOCAL_BATCH", "512", "513"),
        ] {
            fixture.environment.insert(name.into(), valid.into());
            assert!(fixture.configuration().is_ok());
            for value in [
                invalid,
                "-1",
                "",
                "+1",
                " 1",
                "1.0",
                "999999999999999999",
                "1 --model bad",
                "1;echo bad",
            ] {
                fixture.environment.insert(name.into(), value.into());
                let target = target_from(fixture.configuration());
                assert!(!target.available);
                assert!(target.detail.contains(name));
            }
            fixture.environment.remove(name);
        }
        for name in ["TUTOR_LOCAL_THREADS", "TUTOR_LOCAL_BATCH"] {
            fixture.environment.insert(name.into(), "0".into());
            assert!(fixture.configuration().is_err());
            fixture.environment.remove(name);
        }
        for cpus in [0, 1, 2, 12, usize::MAX] {
            let configuration = Configuration::from_environment(
                |name| fixture.environment.get(name).cloned(),
                None,
                cpus,
            )
            .unwrap();
            assert_eq!(configuration.threads as usize, cpus.clamp(1, 6));
        }
    }

    #[test]
    fn explicit_hardware_options_reach_only_the_native_argument_vector() {
        let mut fixture = Fixture::new();
        for (name, value) in [
            ("TUTOR_LOCAL_GPU_LAYERS", "17"),
            ("TUTOR_LOCAL_THREADS", "3"),
            ("TUTOR_LOCAL_BATCH", "64"),
        ] {
            fixture.environment.insert(name.into(), value.into());
        }
        let spec = fixture
            .configuration()
            .unwrap()
            .launch(fixture.root.path().to_str().unwrap())
            .unwrap();
        assert_eq!(
            &spec.args[6..],
            ["--gpu-layers", "17", "--threads", "3", "--batch", "64"]
        );
        assert!(spec.env.is_empty());
    }

    #[cfg(windows)]
    #[test]
    fn windows_requires_exe_even_when_a_wrapper_has_a_native_signature() {
        let mut fixture = Fixture::new();
        for extension in ["cmd", "bat", "ps1", "js", ""] {
            let wrapper = fixture.executable.with_extension(extension);
            std::fs::copy(&fixture.executable, &wrapper).unwrap();
            fixture
                .environment
                .insert("TUTOR_LOCAL_AGENT_PATH".into(), wrapper.into_os_string());
            assert!(fixture.configuration().is_err());
        }
    }

    #[test]
    fn launch_requires_an_absolute_existing_working_directory() {
        let fixture = Fixture::new();
        for directory in [
            "",
            "relative",
            "/tmp/invalid\0path",
            fixture.model.to_str().unwrap(),
        ] {
            assert!(fixture.configuration().unwrap().launch(directory).is_err());
        }
        assert!(fixture
            .configuration()
            .unwrap()
            .launch(fixture.root.path().join("absent").to_str().unwrap())
            .is_err());
    }

    #[cfg(unix)]
    #[test]
    fn non_unicode_configuration_is_rejected_instead_of_lossily_rewritten() {
        use std::os::unix::ffi::OsStringExt;
        let mut fixture = Fixture::new();
        for name in [
            "TUTOR_LOCAL_AGENT_PATH",
            "TUTOR_LOCAL_MODEL_PATH",
            "TUTOR_LOCAL_THREADS",
        ] {
            let old = fixture
                .environment
                .insert(name.into(), OsString::from_vec(vec![0xff]));
            assert!(fixture.configuration().is_err());
            fixture.environment.remove(name);
            if let Some(old) = old {
                fixture.environment.insert(name.into(), old);
            }
        }
    }
}
