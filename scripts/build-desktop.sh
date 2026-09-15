#!/usr/bin/env bash
# Build the desktop application: frontend, Go sidecar, Tauri bundle.
#
# The ordering is not arbitrary. Tauri copies the frontend and the sidecar into
# the bundle at package time, so both have to exist first — and the sidecar's
# filename has to carry the Rust target triple, which is Tauri's convention for
# choosing the right binary per platform.
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT="$(pwd)"
TAURI_DIR="$ROOT/desktop/src-tauri"

DEV=0
if [[ "${1:-}" == "--dev" ]]; then
  DEV=1
fi

need() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "error: $1 is required but not on PATH" >&2
    exit 1
  }
}
need go
need npm
need cargo
# The bundler is a separate tool: `cargo build` compiles the shell but produces
# no installer.
if ! cargo tauri --version >/dev/null 2>&1; then
  echo "error: the Tauri CLI is missing — install it with:" >&2
  echo "       cargo install tauri-cli --version '^2' --locked" >&2
  exit 1
fi

# Derived, not hardcoded: the same script then works for a cross-platform build
# without anyone remembering to edit a filename.
TRIPLE="$(rustc -vV | sed -n 's/^host: //p')"
if [[ -z "$TRIPLE" ]]; then
  echo "error: could not determine the Rust host triple" >&2
  exit 1
fi

EXT=""
case "$TRIPLE" in
*windows*) EXT=".exe" ;;
esac

echo "==> frontend"
(cd "$ROOT/web" && npm ci --silent && npm run build --silent)
rm -rf "$TAURI_DIR/dist"
cp -r "$ROOT/web/dist" "$TAURI_DIR/dist"

echo "==> go sidecar ($TRIPLE)"
mkdir -p "$TAURI_DIR/binaries"
# CGO stays off: the SQLite driver is pure Go, so the binary cross-compiles
# without a C toolchain on any host.
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
  -o "$TAURI_DIR/binaries/taskflow-desktop-$TRIPLE$EXT" \
  "$ROOT/cmd/taskflow-desktop"

echo "==> ocr engine"
# Bundling tesseract is what lets a scanned document work on a machine where the
# user installed nothing. A failure here FAILS THE BUILD rather than quietly
# producing an installer without OCR — a silently degraded artifact is worse
# than no artifact, because it only shows up on the one document that needed it.
rm -rf "$TAURI_DIR/tesseract"
case "$TRIPLE" in
*windows*)
  need powershell.exe
  powershell.exe -NoProfile -ExecutionPolicy Bypass \
    -File "$(cygpath -w "$ROOT/scripts/stage-tesseract.ps1")" \
    -Destination "$(cygpath -w "$TAURI_DIR/tesseract")"
  # PowerShell's exit code does not always reach us through the wrapper, so
  # check the thing we actually care about.
  if [[ ! -f "$TAURI_DIR/tesseract/tesseract.exe" ]]; then
    echo "error: tesseract was not staged; the installer would ship without OCR" >&2
    exit 1
  fi
  ;;
*)
  # Bundling on macOS/Linux means relocating dylibs/sos, which is a different
  # job from the Windows import-table walk. Not attempted rather than done
  # badly: the app falls back to a tesseract on PATH.
  echo "   (not bundled on this platform - install tesseract via your package manager)"
  mkdir -p "$TAURI_DIR/tesseract"
  ;;
esac

# --- code signing ---------------------------------------------------------
# Nothing is signed by hand here. Once the bundler has a certificate it signs
# the whole tree itself: the shell binary, the Go sidecar, every bundled
# .exe/.dll that does not already carry a *valid* signature, the NSIS plugins,
# the uninstaller and the installer. The job is to hand it a certificate and
# then check the artifact -- the bundler logs "Signing <file>" and never looks
# at the result, which is the same trap the OCR staging had.
#
# The certificate never lives in the repository. Pick one:
#
#   TASKFLOW_SIGN_THUMBPRINT  SHA-1 thumbprint of a certificate in the Windows
#                             certificate store. This covers USB tokens and
#                             cloud HSMs, which is how every code-signing
#                             certificate issued since June 2023 arrives -- the
#                             CA/Browser Forum requires the private key to stay
#                             on FIPS 140-2 hardware, so "a .pfx by email" is no
#                             longer a thing for new certificates.
#
#   TASKFLOW_SIGN_COMMAND     Any other signer, as a command line with %1 where
#                             the file goes: Azure Trusted Signing, a token
#                             vendor's tool, or signtool with an older .pfx.
#                             Tauri splits it on spaces, so if a path contains
#                             one, point this at a wrapper script instead.
#
# Neither set is allowed and only warns: an unsigned build is how this is
# developed. It is not how it is distributed.
SIGN_ARGS=()
SIGN_CONFIG=""

if [[ -n "${TASKFLOW_SIGN_THUMBPRINT:-}" && -n "${TASKFLOW_SIGN_COMMAND:-}" ]]; then
  echo "error: set TASKFLOW_SIGN_THUMBPRINT or TASKFLOW_SIGN_COMMAND, not both" >&2
  exit 1
fi

if [[ -n "${TASKFLOW_SIGN_THUMBPRINT:-}" || -n "${TASKFLOW_SIGN_COMMAND:-}" ]]; then
  need node
  SIGN_CONFIG="$TAURI_DIR/.sign.conf.json"
  # The merged config carries an identity, so it does not outlive the build.
  trap 'rm -f "$SIGN_CONFIG"' EXIT

  if [[ -n "${TASKFLOW_SIGN_THUMBPRINT:-}" ]]; then
    # The Windows certificate dialog copies thumbprints with spaces in them,
    # and pastes an invisible left-to-right mark in front. Strip both rather
    # than let signtool fail with "no certificates were found".
    THUMB="$(printf '%s' "$TASKFLOW_SIGN_THUMBPRINT" | tr -cd '[:alnum:]')"
    if [[ ! "$THUMB" =~ ^[0-9a-fA-F]{40}$ ]]; then
      echo "error: TASKFLOW_SIGN_THUMBPRINT is not a 40-character SHA-1 thumbprint" >&2
      exit 1
    fi
    echo "==> signing with certificate $THUMB"
    T="$THUMB" node -e 'process.stdout.write(JSON.stringify(
      { bundle: { windows: { certificateThumbprint: process.env.T } } }))' >"$SIGN_CONFIG"
  else
    echo "==> signing with a custom command"
    C="$TASKFLOW_SIGN_COMMAND" node -e 'process.stdout.write(JSON.stringify(
      { bundle: { windows: { signCommand: process.env.C } } }))' >"$SIGN_CONFIG"
  fi

  if command -v cygpath >/dev/null 2>&1; then
    SIGN_ARGS=(--config "$(cygpath -w "$SIGN_CONFIG")")
  else
    SIGN_ARGS=(--config "$SIGN_CONFIG")
  fi
else
  echo "==> signing SKIPPED (no TASKFLOW_SIGN_THUMBPRINT or TASKFLOW_SIGN_COMMAND)"
  echo "    the installer will warn every user that its publisher is unknown"
fi

echo "==> tauri"
cd "$TAURI_DIR"
if [[ "$DEV" == "1" ]]; then
  cargo tauri dev
  exit 0
fi

cargo tauri build "${SIGN_ARGS[@]}"
echo
echo "bundle output: $TAURI_DIR/target/release/bundle"

# --- verify ---------------------------------------------------------------
# Check the files, not the log. An unsigned DLL sitting beside a signed
# installer still produces the warning the certificate was bought to remove,
# and a signature without a timestamp stops validating the day the certificate
# expires -- retroactively, on copies users already downloaded.
if [[ -n "$SIGN_CONFIG" && "$TRIPLE" == *windows* ]]; then
  echo
  echo "==> verifying signatures"
  # The shell binary is deliberately absent from this list. The bundler signs
  # it, copies it into the installer, and then restores the unsigned original
  # on disk -- signing a binary it patches per package type would otherwise
  # leave two signatures on it. So the only honest place to check that one is
  # inside the installer; see the post-install check in desktop/README.md.
  VERIFY=("$TAURI_DIR/binaries" "$TAURI_DIR/tesseract")
  while IFS= read -r installer; do
    VERIFY+=("$installer")
  done < <(find "$TAURI_DIR/target/release/bundle" -name '*-setup.exe' -o -name '*.msi')

  PS_PATHS=""
  for p in "${VERIFY[@]}"; do
    [[ -e "$p" ]] || continue
    PS_PATHS+="'$(cygpath -w "$p")',"
  done

  EXTRA=""
  # A self-signed certificate cannot chain to a trusted root, so proving the
  # pipeline works needs this. A real certificate must never need it.
  if [[ "${TASKFLOW_SIGN_ALLOW_UNTRUSTED:-}" == "1" ]]; then
    echo "    (accepting untrusted chains: TASKFLOW_SIGN_ALLOW_UNTRUSTED=1)"
    EXTRA="-AllowUntrusted"
  fi

  powershell.exe -NoProfile -ExecutionPolicy Bypass \
    -Command "& '$(cygpath -w "$ROOT/scripts/verify-signatures.ps1")' -Path ${PS_PATHS%,} $EXTRA; exit \$LASTEXITCODE"
fi

# --- checksums ------------------------------------------------------------
# A hash published beside the download is not a weaker signature, it is a
# different thing. Whoever can replace the installer on a page can replace the
# hash printed next to it, so on its own it proves nothing about origin. What
# it does do is worth having: it catches a truncated or corrupted download, and
# it lets anyone who obtained the hash through a *different* channel -- the
# repository, a release note, a signed announcement -- confirm they received
# the bytes we built.
#
# This runs last on purpose. Signing rewrites the installer, so a hash taken
# any earlier would describe a file nobody will ever download.
BUNDLE="$TAURI_DIR/target/release/bundle"
if [[ -d "$BUNDLE" ]]; then
  echo
  echo "==> checksums"
  # -b so the output is byte-identical whatever host built it; sorted so the
  # file itself is stable across builds.
  (
    cd "$BUNDLE"
    find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 sha256sum -b >SHA256SUMS
  )
  sed 's|^|    |' "$BUNDLE/SHA256SUMS"
  echo
  echo "    written to $BUNDLE/SHA256SUMS"
  echo "    verify with: cd '$BUNDLE' && sha256sum -c SHA256SUMS"
fi
