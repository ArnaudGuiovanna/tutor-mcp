// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

use std::path::PathBuf;

/// A process invocation, with arguments kept separate from executable names.
/// The ACP transport owns this process and its piped stdin/stdout lifecycle.
#[derive(Clone, Debug)]
pub struct LaunchSpec {
    pub program: String,
    pub args: Vec<String>,
    pub env: Vec<(String, String)>,
    pub cwd: Option<PathBuf>,
}
