// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

pub mod acp;
pub mod backend;
pub mod discovery;
pub mod identity;
pub mod launch;
pub mod native_auth;
pub mod vault;
pub mod workspace;

#[cfg(feature = "desktop")]
mod desktop;
#[cfg(feature = "desktop")]
pub use desktop::run;
