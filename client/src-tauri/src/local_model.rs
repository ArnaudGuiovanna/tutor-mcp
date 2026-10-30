// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! Installs one pinned local model. A completed size/hash check precedes the
//! atomic rename; cancellation/failure only removes this job's private part.
//! No caller can supply a download URL. Status never contains paths or HTTP
//! errors (redirect URLs may contain signed credentials).

use serde::Serialize;
use sha2::{Digest, Sha256};
use std::{
    fs::{self, File, OpenOptions},
    future::Future,
    io::{Read, Write},
    path::{Path, PathBuf},
    sync::{
        atomic::{AtomicBool, Ordering},
        Arc, Mutex, MutexGuard,
    },
    time::Duration,
};
use tokio::sync::Notify;

pub const MODEL_FILENAME: &str = "Qwen3-4B-Instruct-2507-Q4_K_M.gguf";
pub const MODEL_URL: &str = "https://huggingface.co/unsloth/Qwen3-4B-Instruct-2507-GGUF/resolve/a06e946bb6b655725eafa393f4a9745d460374c9/Qwen3-4B-Instruct-2507-Q4_K_M.gguf";
pub const MODEL_BYTES: u64 = 2_497_281_120;
pub const MODEL_SHA256: &str = "3605803b982cb64aead44f6c1b2ae36e3acdb41d8e46c8a94c6533bc4c67e597";
const BUFFER_BYTES: usize = 256 * 1024;
const NETWORK_IDLE: Duration = Duration::from_secs(60);
const SIZE_ERROR: &str = "La taille du modèle ne correspond pas au profil attendu.";
const HASH_ERROR: &str = "La signature du modèle ne correspond pas au profil attendu.";
const IO_ERROR: &str = "Impossible de lire ou d’écrire le fichier du modèle.";
const NETWORK_ERROR: &str = "Le téléchargement du modèle a échoué ou a été interrompu.";

#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct ModelStatus {
    /// missing/checking/downloading/importing/ready/error/cancelled
    pub phase: String,
    pub downloaded: u64,
    pub total: u64,
    pub error: Option<String>,
    pub agent_available: bool,
}

struct Profile {
    filename: &'static str,
    url: &'static str,
    bytes: u64,
    sha256: String,
}

struct Job {
    cancelled: AtomicBool,
    wake: Notify,
}
impl Job {
    fn check(&self) -> Result<(), Fault> {
        if self.cancelled.load(Ordering::Acquire) {
            Err(Fault::Cancelled)
        } else {
            Ok(())
        }
    }
    async fn cancellation(&self) {
        if !self.cancelled.load(Ordering::Acquire) {
            // notify_one retains a permit if cancellation happens before await.
            self.wake.notified().await;
        }
    }
}

struct State {
    status: ModelStatus,
    active: Option<Arc<Job>>,
}

pub struct ModelManager {
    directory: PathBuf,
    profile: Profile,
    state: Mutex<State>,
}

enum Work {
    Check,
    Install(Option<PathBuf>),
}
#[derive(Debug)]
enum Fault {
    Cancelled,
    Failed(&'static str),
}

impl ModelManager {
    pub fn new(directory: PathBuf, agent_available: bool) -> Arc<Self> {
        Self::with_profile(
            directory,
            agent_available,
            Profile {
                filename: MODEL_FILENAME,
                url: MODEL_URL,
                bytes: MODEL_BYTES,
                sha256: MODEL_SHA256.into(),
            },
        )
    }

    fn with_profile(directory: PathBuf, agent_available: bool, profile: Profile) -> Arc<Self> {
        Arc::new(Self {
            directory,
            state: Mutex::new(State {
                status: ModelStatus {
                    phase: "missing".into(),
                    downloaded: 0,
                    total: profile.bytes,
                    error: None,
                    agent_available,
                },
                active: None,
            }),
            profile,
        })
    }

    fn state(&self) -> MutexGuard<'_, State> {
        self.state
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
    }

    pub fn path(&self) -> PathBuf {
        self.directory.join(self.profile.filename)
    }

    pub fn status(&self) -> ModelStatus {
        self.state().status.clone()
    }

    pub fn start(self: &Arc<Self>, source: Option<PathBuf>) -> Result<ModelStatus, String> {
        self.launch(Work::Install(source))
    }

    pub fn check_existing(self: &Arc<Self>) {
        // Busy means an explicit install/check already owns the only job.
        let _ = self.launch(Work::Check);
    }

    pub fn cancel(&self) -> ModelStatus {
        let mut state = self.state();
        // Publishing holds this same lock and sets ready before releasing it.
        // Cancellation after that linearization point does not undo success.
        if state.status.phase != "ready" {
            if let Some(job) = &state.active {
                job.cancelled.store(true, Ordering::Release);
                job.wake.notify_one();
                state.status.phase = "cancelled".into();
                state.status.error = None;
            }
        }
        state.status.clone()
    }

    fn launch(self: &Arc<Self>, work: Work) -> Result<ModelStatus, String> {
        let job = Arc::new(Job {
            cancelled: AtomicBool::new(false),
            wake: Notify::new(),
        });
        let initial = {
            let mut state = self.state();
            if state.active.is_some() {
                return Err("Une opération sur le modèle est déjà en cours.".into());
            }
            state.active = Some(job.clone());
            state.status.phase = "checking".into();
            state.status.downloaded = 0;
            state.status.error = None;
            state.status.clone()
        };
        let owner = self.clone();
        let worker_job = job.clone();
        // A dedicated worker keeps hashing and bounded filesystem operations
        // off the UI/runtime threads. new/status/start also need no ambient
        // Tokio runtime, including during desktop setup and unit tests.
        let spawned = std::thread::Builder::new()
            .name("tutor-local-model".into())
            .spawn(move || {
                let result = match tokio::runtime::Builder::new_current_thread()
                    .enable_all()
                    .build()
                {
                    Ok(runtime) => {
                        let result = runtime.block_on(owner.run(&worker_job, work));
                        // Dropping a runtime normally waits for blocking DNS
                        // resolution. A cancelled transfer must release its job
                        // even if that OS resolver has not returned yet.
                        runtime.shutdown_background();
                        result
                    }
                    Err(_) => Err(Fault::Failed(
                        "Impossible de démarrer la préparation du modèle.",
                    )),
                };
                owner.finish(&worker_job, result);
            });
        if spawned.is_err() {
            let message = "Impossible de démarrer la préparation du modèle.";
            self.finish(&job, Err(Fault::Failed(message)));
            return Err(message.into());
        }
        Ok(initial)
    }

    fn progress(&self, job: &Arc<Job>, phase: &str, downloaded: u64) -> Result<(), Fault> {
        let mut state = self.state();
        job.check()?;
        if !state
            .active
            .as_ref()
            .is_some_and(|active| Arc::ptr_eq(active, job))
        {
            return Err(Fault::Cancelled);
        }
        state.status.phase = phase.into();
        state.status.downloaded = downloaded;
        Ok(())
    }

    fn finish(&self, job: &Arc<Job>, result: Result<bool, Fault>) {
        let mut state = self.state();
        if !state
            .active
            .as_ref()
            .is_some_and(|active| Arc::ptr_eq(active, job))
        {
            return;
        }
        let result = if job.cancelled.load(Ordering::Acquire) {
            Err(Fault::Cancelled)
        } else {
            result
        };
        state.status.error = None;
        state.status.phase = match result {
            Ok(true) => {
                state.status.downloaded = self.profile.bytes;
                "ready"
            }
            Ok(false) => {
                state.status.downloaded = 0;
                "missing"
            }
            Err(Fault::Cancelled) => "cancelled",
            Err(Fault::Failed(message)) => {
                state.status.error = Some(message.into());
                "error"
            }
        }
        .into();
        // run has returned and its owned part has been closed/cleaned first.
        state.active = None;
    }

    async fn run(&self, job: &Arc<Job>, work: Work) -> Result<bool, Fault> {
        let existing = self.verify_existing(job);
        match (&work, existing) {
            (_, Ok(true)) => return Ok(true),
            (_, Err(Fault::Cancelled)) => return Err(Fault::Cancelled),
            (Work::Check, result) => return result,
            (Work::Install(_), _) => (),
        }
        let Work::Install(source) = work else {
            unreachable!()
        };
        job.check()?;
        fs::create_dir_all(&self.directory).map_err(|_| Fault::Failed(IO_ERROR))?;
        let mut part = Part::create(&self.directory)?;
        let mut hash = Sha256::new();
        let mut count = 0u64;
        if let Some(source) = source {
            self.progress(job, "importing", 0)?;
            let source_metadata = fs::metadata(&source).map_err(|_| Fault::Failed(IO_ERROR))?;
            if !source_metadata.is_file() || source_metadata.len() != self.profile.bytes {
                return Err(Fault::Failed(SIZE_ERROR));
            }
            let mut input = File::open(source).map_err(|_| Fault::Failed(IO_ERROR))?;
            let metadata = input.metadata().map_err(|_| Fault::Failed(IO_ERROR))?;
            if !metadata.is_file() || metadata.len() != self.profile.bytes {
                return Err(Fault::Failed(SIZE_ERROR));
            }
            let mut buffer = vec![0u8; BUFFER_BYTES];
            loop {
                job.check()?;
                let n = input
                    .read(&mut buffer)
                    .map_err(|_| Fault::Failed(IO_ERROR))?;
                if n == 0 {
                    break;
                }
                self.consume(
                    job,
                    &mut part,
                    &mut hash,
                    &mut count,
                    &buffer[..n],
                    "importing",
                )?;
            }
        } else {
            self.progress(job, "downloading", 0)?;
            let client = download_client()?;
            let mut response = network(job, client.get(self.profile.url).send()).await?;
            if response.status() != reqwest::StatusCode::OK {
                return Err(Fault::Failed(NETWORK_ERROR));
            }
            if response
                .content_length()
                .is_some_and(|length| length != self.profile.bytes)
            {
                return Err(Fault::Failed(SIZE_ERROR));
            }
            while let Some(chunk) = network(job, response.chunk()).await? {
                // Keep no whole-body accumulator. Reqwest owns its frame;
                // disk/hash processing uses slices no larger than 256 KiB.
                for bytes in chunk.chunks(BUFFER_BYTES) {
                    self.consume(job, &mut part, &mut hash, &mut count, bytes, "downloading")?;
                }
            }
        }
        job.check()?;
        if count != self.profile.bytes {
            return Err(Fault::Failed(SIZE_ERROR));
        }
        if format!("{:x}", hash.finalize()) != self.profile.sha256 {
            return Err(Fault::Failed(HASH_ERROR));
        }
        part.file
            .as_ref()
            .ok_or(Fault::Failed(IO_ERROR))?
            .sync_all()
            .map_err(|_| Fault::Failed(IO_ERROR))?;
        // Close before rename for Windows. Never delete the destination as a
        // replacement fallback: rename failure preserves the previous file.
        part.file.take();
        let mut state = self.state();
        job.check()?;
        if !state
            .active
            .as_ref()
            .is_some_and(|active| Arc::ptr_eq(active, job))
        {
            return Err(Fault::Cancelled);
        }
        fs::rename(&part.path, self.path()).map_err(|_| Fault::Failed(IO_ERROR))?;
        part.published = true;
        state.status.phase = "ready".into();
        state.status.downloaded = count;
        Ok(true)
    }

    fn consume(
        &self,
        job: &Arc<Job>,
        part: &mut Part,
        hash: &mut Sha256,
        count: &mut u64,
        bytes: &[u8],
        phase: &str,
    ) -> Result<(), Fault> {
        job.check()?;
        let next = count
            .checked_add(bytes.len() as u64)
            .ok_or(Fault::Failed(SIZE_ERROR))?;
        if next > self.profile.bytes {
            return Err(Fault::Failed(SIZE_ERROR));
        }
        part.file
            .as_mut()
            .ok_or(Fault::Failed(IO_ERROR))?
            .write_all(bytes)
            .map_err(|_| Fault::Failed(IO_ERROR))?;
        hash.update(bytes);
        *count = next;
        self.progress(job, phase, next)
    }

    fn verify_existing(&self, job: &Arc<Job>) -> Result<bool, Fault> {
        job.check()?;
        let path = self.path();
        let metadata = match fs::symlink_metadata(&path) {
            Ok(metadata) => metadata,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(false),
            Err(_) => return Err(Fault::Failed(IO_ERROR)),
        };
        if !metadata.is_file() || metadata.len() != self.profile.bytes {
            return Err(Fault::Failed(SIZE_ERROR));
        }
        let mut file = File::open(&path).map_err(|_| Fault::Failed(IO_ERROR))?;
        let mut buffer = vec![0u8; BUFFER_BYTES];
        let mut hash = Sha256::new();
        let mut count = 0u64;
        loop {
            job.check()?;
            let n = file
                .read(&mut buffer)
                .map_err(|_| Fault::Failed(IO_ERROR))?;
            if n == 0 {
                break;
            }
            count += n as u64;
            if count > self.profile.bytes {
                return Err(Fault::Failed(SIZE_ERROR));
            }
            hash.update(&buffer[..n]);
            self.progress(job, "checking", count)?;
        }
        if count != self.profile.bytes {
            return Err(Fault::Failed(SIZE_ERROR));
        }
        if format!("{:x}", hash.finalize()) != self.profile.sha256 {
            return Err(Fault::Failed(HASH_ERROR));
        }
        job.check()?;
        Ok(true)
    }
}

async fn network<T>(
    job: &Job,
    request: impl Future<Output = Result<T, reqwest::Error>>,
) -> Result<T, Fault> {
    job.check()?;
    tokio::select! {
        biased;
        _ = job.cancellation() => Err(Fault::Cancelled),
        result = tokio::time::timeout(NETWORK_IDLE, request) => {
            result.map_err(|_| Fault::Failed(NETWORK_ERROR))?.map_err(|_| Fault::Failed(NETWORK_ERROR))
        }
    }
}

fn safe_redirect(url: &reqwest::Url, previous: usize) -> bool {
    url.scheme() == "https"
        && url.username().is_empty()
        && url.password().is_none()
        && previous <= 5
}

fn download_client() -> Result<reqwest::Client, Fault> {
    reqwest::Client::builder()
        .https_only(true)
        .connect_timeout(Duration::from_secs(30))
        .read_timeout(NETWORK_IDLE)
        .referer(false)
        .no_gzip()
        .no_brotli()
        .no_deflate()
        .no_zstd()
        .redirect(reqwest::redirect::Policy::custom(|attempt| {
            if safe_redirect(attempt.url(), attempt.previous().len()) {
                attempt.follow()
            } else {
                attempt.error("model_redirect_rejected")
            }
        }))
        .build()
        .map_err(|_| Fault::Failed(NETWORK_ERROR))
}

struct Part {
    file: Option<File>,
    path: PathBuf,
    published: bool,
}
impl Part {
    fn create(directory: &Path) -> Result<Self, Fault> {
        let path = directory.join(format!(".tutor-model-{}.part", uuid::Uuid::new_v4()));
        let mut options = OpenOptions::new();
        options.write(true).create_new(true);
        #[cfg(unix)]
        {
            use std::os::unix::fs::OpenOptionsExt;
            options.mode(0o600);
        }
        let file = options.open(&path).map_err(|_| Fault::Failed(IO_ERROR))?;
        Ok(Self {
            file: Some(file),
            path,
            published: false,
        })
    }
}
impl Drop for Part {
    fn drop(&mut self) {
        self.file.take();
        if !self.published {
            let _ = fs::remove_file(&self.path);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::{json, Value};
    use std::time::Instant;

    fn fixture(directory: &Path, bytes: &[u8]) -> Arc<ModelManager> {
        ModelManager::with_profile(
            directory.join("models"),
            true,
            Profile {
                filename: "fixture.gguf",
                // An unexpected download fails locally instead of contacting
                // the Hub. Production accepts only the pinned constant URL.
                url: "invalid-fixture-url",
                bytes: bytes.len() as u64,
                sha256: format!("{:x}", Sha256::digest(bytes)),
            },
        )
    }

    fn settled(manager: &ModelManager) -> ModelStatus {
        let deadline = Instant::now() + Duration::from_secs(5);
        loop {
            let state = manager.state();
            if state.active.is_none() {
                return state.status.clone();
            }
            drop(state);
            assert!(Instant::now() < deadline, "model worker did not finish");
            std::thread::sleep(Duration::from_millis(2));
        }
    }

    fn attach_job(manager: &ModelManager) -> Arc<Job> {
        let job = Arc::new(Job {
            cancelled: AtomicBool::new(false),
            wake: Notify::new(),
        });
        let mut state = manager.state();
        state.active = Some(job.clone());
        state.status.phase = "downloading".into();
        job
    }

    fn no_parts(directory: &Path) {
        if directory.exists() {
            assert!(fs::read_dir(directory).unwrap().all(|entry| !entry
                .unwrap()
                .file_name()
                .to_string_lossy()
                .ends_with(".part")));
        }
    }

    #[test]
    fn pinned_constants_match_the_bundled_profile_and_status_wire_contract() {
        let profile: Value =
            serde_json::from_str(include_str!("../../local-agent/model-profile.json")).unwrap();
        assert_eq!(profile["gguf_filename"], MODEL_FILENAME);
        assert_eq!(profile["gguf_bytes"], MODEL_BYTES);
        assert_eq!(profile["gguf_sha256"], MODEL_SHA256);
        assert_eq!(
            format!(
                "https://huggingface.co/{}/resolve/{}/{}",
                profile["gguf_repository"].as_str().unwrap(),
                profile["gguf_revision"].as_str().unwrap(),
                MODEL_FILENAME
            ),
            MODEL_URL
        );
        let directory = tempfile::tempdir().unwrap();
        let manager = ModelManager::new(directory.path().join("models"), false);
        assert_eq!(
            manager.path(),
            directory.path().join("models").join(MODEL_FILENAME)
        );
        assert_eq!(
            serde_json::to_value(manager.status()).unwrap(),
            json!({
                "phase":"missing", "downloaded":0,"total":MODEL_BYTES,"error":null,"agentAvailable":false,
            })
        );
        assert_eq!(manager.cancel().phase, "missing");
        assert!(!manager.directory.exists());
    }

    #[test]
    fn imports_exact_bytes_and_reuses_valid_existing_without_opening_source_or_network() {
        let directory = tempfile::tempdir().unwrap();
        let bytes = b"GGUF tiny pinned model fixture";
        let manager = fixture(directory.path(), bytes);
        let source = directory.path().join("selected.gguf");
        fs::write(&source, bytes).unwrap();
        manager.start(Some(source.clone())).unwrap();
        let status = settled(&manager);
        assert_eq!(status.phase, "ready");
        assert_eq!(status.downloaded, bytes.len() as u64);
        assert_eq!(fs::read(manager.path()).unwrap(), bytes);
        assert_eq!(fs::read(source).unwrap(), bytes);
        manager
            .start(Some(directory.path().join("does-not-exist")))
            .unwrap();
        assert_eq!(settled(&manager).phase, "ready");
        manager.start(None).unwrap(); // The valid file must bypass HTTP entirely.
        assert_eq!(settled(&manager).phase, "ready");
        assert_eq!(manager.cancel().phase, "ready");
        no_parts(&manager.directory);
    }

    #[test]
    fn incorrect_size_hash_and_missing_source_never_replace_existing_data() {
        for corrupt in [b"bad!".as_slice(), b"oversize".as_slice(), b"x".as_slice()] {
            let directory = tempfile::tempdir().unwrap();
            let manager = fixture(directory.path(), b"good");
            fs::create_dir_all(&manager.directory).unwrap();
            fs::write(manager.path(), b"previous-file").unwrap();
            let source = directory.path().join("input.gguf");
            fs::write(&source, corrupt).unwrap();
            manager.start(Some(source)).unwrap();
            let status = settled(&manager);
            assert_eq!(status.phase, "error");
            assert_eq!(
                status.error.as_deref(),
                Some(if corrupt.len() == 4 {
                    HASH_ERROR
                } else {
                    SIZE_ERROR
                })
            );
            assert_eq!(fs::read(manager.path()).unwrap(), b"previous-file");
            no_parts(&manager.directory);
            manager
                .start(Some(directory.path().join("missing")))
                .unwrap();
            assert_eq!(settled(&manager).phase, "error");
            assert_eq!(fs::read(manager.path()).unwrap(), b"previous-file");
            no_parts(&manager.directory);
        }
    }

    #[test]
    fn startup_rechecks_content_on_every_new_manager_without_network() {
        let directory = tempfile::tempdir().unwrap();
        let first = fixture(directory.path(), b"good");
        first.check_existing();
        assert_eq!(settled(&first).phase, "missing");
        fs::create_dir_all(&first.directory).unwrap();
        fs::write(first.path(), b"good").unwrap();
        first.check_existing();
        assert_eq!(settled(&first).phase, "ready");
        // Same size, changed hash: startup must not trust size or an old ready.
        fs::write(first.path(), b"evil").unwrap();
        let restarted = fixture(directory.path(), b"good");
        restarted.check_existing();
        let status = settled(&restarted);
        assert_eq!(status.phase, "error");
        assert_eq!(status.error.as_deref(), Some(HASH_ERROR));
        assert_eq!(fs::read(restarted.path()).unwrap(), b"evil");
        no_parts(&restarted.directory);
    }

    #[tokio::test]
    async fn stalled_network_is_cancellable_and_cancel_does_not_release_job_early() {
        let directory = tempfile::tempdir().unwrap();
        let manager = fixture(directory.path(), b"good");
        let job = attach_job(&manager);
        let waiting_job = job.clone();
        let waiting = tokio::spawn(async move {
            network(
                &waiting_job,
                std::future::pending::<Result<(), reqwest::Error>>(),
            )
            .await
        });
        tokio::task::yield_now().await;
        assert!(manager.start(None).is_err());
        manager.check_existing(); // Startup cannot replace an active job.
        assert_eq!(manager.cancel().phase, "cancelled");
        assert!(manager.start(Some(directory.path().join("input"))).is_err());
        assert!(matches!(
            tokio::time::timeout(Duration::from_secs(1), waiting)
                .await
                .unwrap()
                .unwrap(),
            Err(Fault::Cancelled)
        ));
        manager.finish(&job, Err(Fault::Cancelled));
        assert_eq!(settled(&manager).phase, "cancelled");
        manager.check_existing();
        assert_eq!(settled(&manager).phase, "missing");
    }

    #[tokio::test]
    async fn already_cancelled_network_is_not_polled_and_errors_do_not_echo_urls() {
        let job = Job {
            cancelled: AtomicBool::new(true),
            wake: Notify::new(),
        };
        let request = async {
            panic!("cancelled request must not be polled");
            #[allow(unreachable_code)]
            Ok::<(), reqwest::Error>(())
        };
        assert!(matches!(
            network(&job, request).await,
            Err(Fault::Cancelled)
        ));
        let live = Job {
            cancelled: AtomicBool::new(false),
            wake: Notify::new(),
        };
        // https_only rejects locally, without touching the network.
        let result = network(
            &live,
            download_client()
                .unwrap()
                .get("http://private.example/file?token=DO_NOT_ECHO")
                .send(),
        )
        .await;
        assert!(matches!(result, Err(Fault::Failed(NETWORK_ERROR))));
    }

    #[test]
    fn oversized_or_cancelled_stream_cleans_only_its_owned_part() {
        let directory = tempfile::tempdir().unwrap();
        let manager = fixture(directory.path(), b"good");
        fs::create_dir_all(&manager.directory).unwrap();
        fs::write(manager.path(), b"good").unwrap();
        let unrelated = manager.directory.join("unrelated.part");
        fs::write(&unrelated, b"keep").unwrap();
        let job = attach_job(&manager);
        let path;
        {
            let mut part = Part::create(&manager.directory).unwrap();
            path = part.path.clone();
            let mut hash = Sha256::new();
            let mut count = 0;
            manager
                .consume(&job, &mut part, &mut hash, &mut count, b"go", "downloading")
                .unwrap();
            assert!(matches!(
                manager.consume(
                    &job,
                    &mut part,
                    &mut hash,
                    &mut count,
                    b"TOO LARGE",
                    "downloading"
                ),
                Err(Fault::Failed(SIZE_ERROR))
            ));
            assert_eq!(count, 2);
            manager.cancel();
            assert!(matches!(
                manager.consume(&job, &mut part, &mut hash, &mut count, b"od", "downloading"),
                Err(Fault::Cancelled)
            ));
        }
        assert!(!path.exists());
        assert_eq!(fs::read(unrelated).unwrap(), b"keep");
        assert_eq!(fs::read(manager.path()).unwrap(), b"good");
    }

    #[test]
    fn redirect_policy_only_allows_five_https_redirects_without_userinfo() {
        assert!(safe_redirect(
            &reqwest::Url::parse("https://cdn.example/model?signature=private").unwrap(),
            5
        ));
        assert!(!safe_redirect(
            &reqwest::Url::parse("https://cdn.example/model").unwrap(),
            6
        ));
        for url in [
            "http://cdn.example/model",
            "file:///tmp/model",
            "https://user:password@cdn.example/model",
        ] {
            assert!(!safe_redirect(&reqwest::Url::parse(url).unwrap(), 1));
        }
    }
}
