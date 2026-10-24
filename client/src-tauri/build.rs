fn main() {
    #[cfg(feature = "desktop")]
    {
        let mut attributes = tauri_build::Attributes::new();
        let windows_msvc = std::env::var("CARGO_CFG_TARGET_OS").as_deref() == Ok("windows")
            && std::env::var("CARGO_CFG_TARGET_ENV").as_deref() == Ok("msvc");
        if windows_msvc {
            // tauri-build 2.7.4 links its resource manifest only into binaries.
            // Unit-test executables also import Common Controls v6 functions;
            // without this activation context they fail before the tests run.
            // Follow Tauri's own build-script workaround (tauri#13419), using
            // its complete locked-version manifest for every linked artifact.
            attributes = attributes
                .windows_attributes(tauri_build::WindowsAttributes::new_without_app_manifest());
        }
        tauri_build::try_build(attributes).expect("failed to run tauri-build");
        if windows_msvc {
            let manifest =
                std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("windows-app-manifest.xml");
            println!("cargo:rerun-if-changed={}", manifest.display());
            println!("cargo:rustc-link-arg=/MANIFEST:EMBED");
            println!("cargo:rustc-link-arg=/MANIFESTINPUT:{}", manifest.display());
            println!("cargo:rustc-link-arg=/WX");
        }
    }
}
