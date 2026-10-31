// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

use std::sync::{Arc, Mutex};
use tutor_local_agent::{acp, config::Config, runtime};

#[tokio::main]
async fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    if args == ["--version"] {
        println!("tutor-local-agent {}", env!("CARGO_PKG_VERSION"));
        return;
    }
    if args == ["--help"] {
        println!("tutor-local-agent --model ABSOLUTE_GGUF_PATH [--sha256 HEX] [--data-dir ABSOLUTE_PRIVATE_DIRECTORY] [--context 2048] [--max-output 192] [--gpu-layers 0] [--threads 6] [--batch 128]\nACP over stdin/stdout. CUDA or Vulkan features are required for GPU offload.");
        return;
    }
    let config = match Config::parse(args) {
        Ok(config) => config,
        Err(code) => {
            eprintln!("tutor-local-agent: {code}");
            std::process::exit(2);
        }
    };
    let options = acp::Options {
        data_dir: config.data_dir.clone(),
        model_sha256: config
            .expected_sha256
            .clone()
            .unwrap_or_else(|| tutor_local_agent::config::MODEL_SHA256.into()),
    };
    let engine = match runtime::create(config) {
        Ok(engine) => Arc::new(Mutex::new(engine)),
        Err(code) => {
            eprintln!("tutor-local-agent: {code}");
            std::process::exit(2);
        }
    };
    if acp::serve_with_options(
        tokio::io::BufReader::new(tokio::io::stdin()),
        tokio::io::stdout(),
        engine,
        options,
    )
    .await
    .is_err()
    {
        eprintln!("tutor-local-agent: protocol_failed");
        std::process::exit(1);
    }
}
