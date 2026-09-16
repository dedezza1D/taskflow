// Native shell for TaskFlow Compliance.
//
// This process owns a window and a child process, and nothing else. The Go
// sidecar is the application: it opens the local database, runs the pipeline,
// and serves both the API and the UI. We start it, learn which port the kernel
// gave it, and point the window there.
//
// Loading the Go server directly — rather than serving the frontend from
// Tauri's own asset protocol — means the SPA and the API share one origin, so
// `fetch('/api/...')` works with no port baked in anywhere and no CORS.

#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

use std::sync::mpsc::{channel, RecvTimeoutError};
use std::time::Duration;

use tauri::{Emitter, Manager, WebviewUrl, WebviewWindowBuilder};
use tauri_plugin_shell::process::CommandEvent;
use tauri_plugin_shell::ShellExt;

/// How long to wait for the sidecar to report its address before giving up.
/// Generous: the first launch also creates the database and applies the schema.
const STARTUP_TIMEOUT: Duration = Duration::from_secs(30);

/// The sidecar prints this, followed by the URL it bound to.
const READY_PREFIX: &str = "TaskFlow Compliance is running at ";

/// Where the bundled OCR engine lands inside the app's resources. The DLLs and
/// `tessdata/` sit beside it, which is how tesseract finds them.
#[cfg(windows)]
const BUNDLED_TESSERACT: &str = "tesseract/tesseract.exe";
#[cfg(not(windows))]
const BUNDLED_TESSERACT: &str = "tesseract/tesseract";

fn main() {
    tauri::Builder::default()
        .plugin(tauri_plugin_shell::init())
        .setup(|app| {
            let handle = app.handle().clone();

            // The frontend build ships as a bundled resource; the sidecar
            // serves it. Resolving the path at runtime keeps the installed
            // layout and the dev layout working from the same code.
            let web_dir = app
                .path()
                .resolve("dist", tauri::path::BaseDirectory::Resource)
                .map_err(|e| format!("could not locate bundled frontend: {e}"))?;

            let mut args: Vec<String> = vec!["--web-dir".into(), web_dir.to_string_lossy().into()];

            // Tesseract ships inside the bundle so a scanned document works on a
            // machine where nothing was installed. Its absence is not fatal: the
            // sidecar falls back to PATH, and documents with a text layer never
            // need it at all.
            if let Ok(tess) = app
                .path()
                .resolve(BUNDLED_TESSERACT, tauri::path::BaseDirectory::Resource)
            {
                if tess.exists() {
                    args.push("--tesseract".into());
                    args.push(tess.to_string_lossy().into());
                }
            }

            let (tx, rx) = channel::<Result<String, String>>();

            let (mut events, _child) = app
                .shell()
                .sidecar("taskflow-desktop")
                .map_err(|e| format!("sidecar not found: {e}"))?
                .args(args)
                .spawn()
                .map_err(|e| format!("could not start the local server: {e}"))?;

            // Drain the sidecar's output for the lifetime of the app: the first
            // useful line is the address, and everything after is diagnostics
            // worth keeping visible rather than discarding.
            tauri::async_runtime::spawn(async move {
                let mut reported = false;

                while let Some(event) = events.recv().await {
                    match event {
                        CommandEvent::Stdout(line) => {
                            let text = String::from_utf8_lossy(&line).to_string();
                            if !reported {
                                if let Some(url) = text.trim().strip_prefix(READY_PREFIX) {
                                    reported = true;
                                    // The URL carries the per-launch token that
                                    // stands in for authentication, so it goes
                                    // to the window and nowhere else.
                                    let _ = tx.send(Ok(url.trim().to_string()));
                                    println!("{READY_PREFIX}(launch link handed to the window)");
                                    continue;
                                }
                            }
                            println!("{}", text.trim_end());
                        }
                        CommandEvent::Stderr(line) => {
                            eprintln!("{}", String::from_utf8_lossy(&line).trim_end());
                        }
                        CommandEvent::Terminated(status) => {
                            // If it dies before announcing, unblock setup with a
                            // real error instead of letting it wait out the
                            // timeout and report something vaguer.
                            if !reported {
                                let _ = tx.send(Err(format!(
                                    "the local server exited before it was ready (code {:?})",
                                    status.code
                                )));
                            }
                            let _ = handle.emit("server-exited", status.code);
                            break;
                        }
                        _ => {}
                    }
                }
            });

            let url = match rx.recv_timeout(STARTUP_TIMEOUT) {
                Ok(Ok(url)) => url,
                Ok(Err(e)) => return Err(e.into()),
                Err(RecvTimeoutError::Timeout) => {
                    return Err("the local server did not start in time".into())
                }
                Err(RecvTimeoutError::Disconnected) => {
                    return Err("lost contact with the local server while starting".into())
                }
            };

            let parsed = url
                .parse()
                .map_err(|e| format!("the local server reported an unusable address {url:?}: {e}"))?;

            WebviewWindowBuilder::new(app, "main", WebviewUrl::External(parsed))
                .title("TaskFlow Compliance")
                .inner_size(1280.0, 860.0)
                .min_inner_size(900.0, 600.0)
                .build()?;

            Ok(())
        })
        .run(tauri::generate_context!())
        .expect("error while running TaskFlow Compliance");
}
