// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

use std::path::PathBuf;

pub const MODEL_ID: &str = "qwen3-4b-instruct-2507-q4_k_m";
pub const MODEL_SHA256: &str = "3605803b982cb64aead44f6c1b2ae36e3acdb41d8e46c8a94c6533bc4c67e597";
pub const CONTEXT_LIMIT: u32 = 4096;
pub const TOKEN_MARGIN: u32 = 256;

#[derive(Clone, Debug)]
pub struct Config {
    pub model: PathBuf,
    pub expected_sha256: Option<String>,
    pub context: u32,
    pub max_output: u32,
    pub gpu_layers: u32,
    pub threads: i32,
    pub batch: u32,
    pub data_dir: Option<PathBuf>,
}

impl Config {
    pub fn parse(args: impl IntoIterator<Item = String>) -> Result<Self, String> {
        let mut config = Self {
            model: PathBuf::new(),
            expected_sha256: Some(MODEL_SHA256.into()),
            context: CONTEXT_LIMIT,
            max_output: 768,
            gpu_layers: 0,
            threads: 6,
            batch: 128,
            data_dir: None,
        };
        let mut args = args.into_iter();
        let mut seen = std::collections::BTreeSet::new();
        while let Some(key) = args.next() {
            if !seen.insert(key.clone()) {
                return Err("duplicate_option".into());
            }
            let value = args.next().ok_or("missing_option_value")?;
            match key.as_str() {
                "--model" => config.model = value.into(),
                "--data-dir" => config.data_dir = Some(value.into()),
                "--sha256" => config.expected_sha256 = Some(value.to_ascii_lowercase()),
                "--context" => config.context = value.parse().map_err(|_| "invalid_context")?,
                "--max-output" => {
                    config.max_output = value.parse().map_err(|_| "invalid_output")?
                }
                "--gpu-layers" => {
                    config.gpu_layers = value.parse().map_err(|_| "invalid_gpu_layers")?
                }
                "--threads" => config.threads = value.parse().map_err(|_| "invalid_threads")?,
                "--batch" => config.batch = value.parse().map_err(|_| "invalid_batch")?,
                _ => return Err("unknown_option".into()),
            }
        }
        config.validate()?;
        Ok(config)
    }

    pub fn validate(&self) -> Result<(), String> {
        if !self.model.is_absolute() || !self.model.is_file() {
            return Err("model_file_unavailable".into());
        }
        if self
            .data_dir
            .as_ref()
            .is_some_and(|path| !path.is_absolute() || path.is_file())
        {
            return Err("invalid_data_directory".into());
        }
        if !(1024..=CONTEXT_LIMIT).contains(&self.context)
            || self.max_output == 0
            || self.max_output > self.context - TOKEN_MARGIN
            || self.gpu_layers > 99
            || !(1..=64).contains(&self.threads)
            || !(1..=512).contains(&self.batch)
        {
            return Err("invalid_runtime_limits".into());
        }
        if self
            .expected_sha256
            .as_ref()
            .is_some_and(|v| v.len() != 64 || !v.bytes().all(|c| c.is_ascii_hexdigit()))
        {
            return Err("invalid_model_digest".into());
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn configuration_keeps_unicode_path_and_bounds_allocations() {
        let dir = tempfile::tempdir().unwrap();
        let model = dir.path().join("modèle local.gguf");
        std::fs::write(&model, b"GGUF").unwrap();
        let args = ["--model".into(), model.display().to_string()];
        let config = Config::parse(args.clone()).unwrap();
        assert_eq!(config.model, model);
        assert_eq!(config.context, 4096);
        assert_eq!(config.gpu_layers, 0);
        for suffix in [
            ["--context", "32000"],
            ["--max-output", "4096"],
            ["--threads", "0"],
            ["--batch", "65536"],
            ["--gpu-layers", "100"],
            ["--sha256", "bad"],
        ] {
            assert!(
                Config::parse(args.clone().into_iter().chain(suffix.map(String::from))).is_err()
            );
        }
        assert!(Config::parse(args.clone().into_iter().chain(args)).is_err());
    }
}
