# TaskFlow Compliance — desktop shell

A Tauri window around `cmd/taskflow-desktop`. The Go binary is the whole
application; this adds a native window, an icon, and an installer.

## How the two halves meet

Tauri launches the Go binary as a **sidecar** and hands it `--web-dir`, pointing
at the frontend files bundled beside it. The binary binds `127.0.0.1:0`, lets
the kernel pick a free port, and prints the URL it got. Tauri reads that line
from stdout and points the window at it.

The window therefore loads the **Go server**, not Tauri's own asset protocol —
so the SPA and the API share one origin and `fetch('/api/...')` works unchanged.
That is the whole reason for the arrangement: no port baked into a config, no
CORS, and the exact same frontend build the served deployment uses.

```
 Tauri window ──▶ http://127.0.0.1:<port>/        (UI + API, one origin)
        │
        └── spawns ──▶ taskflow-desktop --web-dir <resources>/dist
                          ├── SQLite   (per-user app data)
                          ├── local queue (the tasks table)
                          └── pipeline (OCR → PII → report)
```

Nothing listens outside loopback, and no document leaves the machine.

The background sweeps run here too, from `internal/maintenance`: the reconciler
rescues a task left `processing` by a close mid-scan, and the retention sweep
destroys the raw material of documents that dead-lettered and discards uploads
that never got a task. Both run once at startup, because this process often
does not live long enough to reach a tick.

### Loopback is not access control

There are no user accounts, but loopback alone does not keep others out: any web
page the user visits can reach `127.0.0.1` (and, through DNS rebinding, read the
responses), and every account on a shared computer shares it. So the binary
refuses non-loopback addresses, answers only requests whose `Host` is the
loopback address it bound, and generates a random token at each launch. The URL
it prints is `http://127.0.0.1:<port>/?launch=<token>`; opening it exchanges the
token for an `HttpOnly`, `SameSite=Strict` cookie and redirects to a clean URL,
and every request without that cookie gets a 401. Tauri hands the URL to the
window and does not echo it. Running the binary by hand, open the printed URL
as-is. The details are in `cmd/taskflow-desktop/guard.go`.

## The OCR engine travels with the app

A scanned document needs OCR, and a user who just ran an installer has not
installed tesseract. So the installer carries it — and carries only what it
needs: `scripts/stage-tesseract.ps1` walks `tesseract.exe`'s import table with
`dumpbin` and takes the transitive closure of DLLs that ship alongside it. That
is 27 files and ~116 MB, against ~238 MB for the whole install directory, whose
bulk is training tools nothing runs and an ICU data blob nothing links.

The staged copy is **run before it is bundled**. A missing DLL would otherwise
surface on a user's machine, on the one document that happened to need OCR.

Two details that cost a build each to find:

- The bundled `tesseract.exe` has a `TESSDATA_PREFIX` compiled in pointing at
  the path on the machine that built it. The sidecar sets the variable to the
  bundled `tessdata` directory — tesseract 5 wants that directory itself, not
  its parent.
- Tauri resolves resource paths through Rust's `canonicalize`, which returns
  Windows' `\\?\` extended-length form. Go handles it; tesseract, a C program
  using plain `fopen`, does not. The sidecar strips the prefix at that boundary.

## Languages

`por` and `eng` are bundled, and the desktop build defaults to `por+eng`.
Portuguese is not decoration: without it the engine reads "DECLARAGAO" for
"DECLARAÇÃO" and mangles every accented word, which is most of a Brazilian
document.

The data is downloaded from a pinned `tessdata_best` release and checksum-
verified, rather than copied from whatever the build machine happens to have —
otherwise the bundle varies silently by build host. "best" is also the right
model: the most accurate LSTM data, and *smaller* than the standard set, which
carries legacy-engine data tesseract 5 does not use.

Add a language by putting its SHA-256 in `$TessdataSHA` and passing
`-Languages por,eng,spa`; `OCR_LANGS` selects among the bundled ones at runtime.

**One thing worth knowing before adding languages.** The Portuguese model fixes
the accents but reliably inserts a stray space into long digit runs, turning
`529.982.247-25` into `529.982 .247-25`. That would have made every CPF in a
scanned document invisible. The detectors now tolerate whitespace around the
separators — safe, because the mod-11 check digit is what actually decides,
which is the same two-stage arrangement the card and IBAN patterns always used.
A new language may bring its own noise; test detection, not just legibility.

## Prerequisites

- Rust (rustup) and, on Windows, the MSVC C++ build tools — `dumpbin` comes from
  the latter and the OCR staging needs it
- Node 22+ for the frontend
- WebView2 (preinstalled on Windows 11)
- The Tauri CLI: `cargo install tauri-cli --version '^2' --locked`
- A tesseract install to stage from: `winget install UB-Mannheim.TesseractOCR`

## Build

`scripts/build-desktop.sh` does the three steps in order — frontend, Go sidecar
named for the target triple, then the bundle:

```bash
bash scripts/build-desktop.sh          # installer in desktop/src-tauri/target/release/bundle
bash scripts/build-desktop.sh --dev    # run without packaging
```

The sidecar filename must carry the Rust target triple (Tauri's convention for
picking the right binary per platform); the script derives it from `rustc -vV`
rather than hardcoding, so cross-platform builds do not need editing.

## Signing

An installer that says "Publisher: Unknown" is asking a user to hand a folder of
documents full of CPFs to a binary nobody will vouch for. A signature does not
make the application trustworthy, but it makes it **attributable**: the user can
tell that the file they downloaded is the one we built, and that nothing altered
it on the way. For this tool in particular that is the whole point.

Give the build a certificate and the bundler signs the entire tree by itself —
the shell binary, the Go sidecar, every bundled `.exe`/`.dll` without a valid
signature of its own, the NSIS plugins, **the uninstaller** and the installer:

```bash
# A certificate in the Windows store -- USB token, cloud HSM, or imported .pfx
export TASKFLOW_SIGN_THUMBPRINT="<40 hex characters>"
bash scripts/build-desktop.sh
```

```bash
# Anything else: Azure Trusted Signing, a token vendor's tool, signtool + .pfx.
# "%1" is where the file goes. Tauri splits on spaces, so use a wrapper script
# if any path in it contains one.
export TASKFLOW_SIGN_COMMAND="azuresigntool sign -kvu https://... -tr http://timestamp.digicert.com -td sha256 %1"
bash scripts/build-desktop.sh
```

Neither variable set is allowed, and only warns. That is how this is developed;
it is not how it is distributed.

### The build checks the artifact, not the log

The bundler prints `Signing <file>` and never looks at the result, so
`scripts/verify-signatures.ps1` runs afterwards over the sidecar, the OCR
binaries and the installer. It insists on three separate things, because each
is its own way to ship a broken signature:

- **signed at all** — one unsigned DLL beside a signed installer still produces
  the warning the certificate was bought to remove
- **chain validates** — a signature nobody can check is decoration
- **timestamped** — without a countersigned timestamp the signature stops
  validating the day the certificate expires, retroactively, on copies users
  already downloaded. `tauri.conf.json` points at DigiCert's free RFC 3161
  server for this; it is free and there is no reason to skip it.

One file is deliberately *not* in that list: the shell binary on disk. The
bundler signs it, copies it into the installer, then restores the unsigned
original — it patches that binary per package type and a second signature on
top would fail verification. The honest place to check it is the installed
tree, which takes the same script:

```powershell
powershell -File scripts\verify-signatures.ps1 -Path "$env:LOCALAPPDATA\TaskFlow Compliance"
```

### Tesseract gets re-signed, on purpose

Re-signing someone else's binary is normally a smell. Here it is the right
call: UB-Mannheim's certificate expired on 2023-12-10 and `signtool verify`
now rejects `tesseract.exe` and `libtesseract-5.dll`. Shipping a signature that
fails to validate is worse than shipping none, and we are the ones distributing
those files inside our installer. Replacing it claims exactly what we can
honestly claim — this is the binary TaskFlow shipped. The other 25 DLLs in the
OCR set were never signed at all.

### Where to get a certificate

Two of these are free. Neither is free *and* unconditional, which is the whole
of the answer.

| | Cost | Available to a Brazilian developer | SmartScreen |
|---|---|---|---|
| **Microsoft Store, as MSIX** | free | yes, worldwide | no warning at all — Microsoft re-signs the package |
| **SignPath Foundation** | free | yes, if the project is open source | reputation builds normally |
| **OV certificate** (DigiCert, Sectigo, GlobalSign) | US$150–300/year | yes, worldwide | reputation builds normally |
| **Azure Artifact Signing** (ex-Trusted Signing) | ~US$10/month | **no** — organisations in US/CA/EU/UK only, individuals in US/CA only | reputation builds normally |
| **EV certificate** | US$400+/year | yes | same as OV since 2024 |
| **Self-signed** | free | yes | blocks public users; only viable when IT pushes the root via Intune or GPO |

Three things worth knowing before choosing:

- **EV no longer skips SmartScreen.** It used to bypass the warning outright,
  which was the entire reason to pay the premium. Microsoft removed that in
  2024, and an EV-signed file now builds reputation exactly like an OV one.
- **Azure Artifact Signing is geo-restricted**, and the restriction is about
  *who you are*, not where your resources live — "Brazil South" appears in the
  region list, which makes this easy to get wrong.
- **No signature is the one option with no path forward.** SmartScreen builds
  reputation per signing identity, so signed releases inherit what earlier ones
  earned. An unsigned build has only its own file hash to go on, so every
  release starts from zero, forever.

The Store route deserves a caveat: Tauri bundles NSIS and MSI, not MSIX, so
taking it means packaging the app again — and submitting an MSI/EXE installer
to the Store instead does *not* get it signed, that path requires your own
certificate.

### Checksums

Every build writes `SHA256SUMS` beside the installers, after signing — signing
rewrites the file, so a hash taken any earlier would describe something nobody
will download. Publish it with the release.

```bash
cd desktop/src-tauri/target/release/bundle && sha256sum -c SHA256SUMS
```

Whoever downloads the installer is on Windows and does not have `sha256sum`, so
give them the command they can actually run:

```powershell
Get-FileHash "TaskFlow Compliance_0.1.0_x64-setup.exe" -Algorithm SHA256
```

Be clear about what this buys, because it is easy to oversell. A hash printed
on the same page as the download is worth nothing against an attacker: whoever
can replace the file can replace the number beside it. What it does do is catch
a truncated or corrupted download, and — if the reader got the hash from
somewhere *else*, a repository, a release note, a mirror they already trust —
let them confirm the bytes match. That second use is the only one with any
security content, and it only works if the hash travels by a different route
than the installer. A signature needs no such arrangement, which is the
difference between the two.

### Testing the pipeline without a certificate

A self-signed certificate proves the plumbing — signatures applied everywhere,
timestamps obtained — and proves nothing about trust, which is the point:

```powershell
$c = New-SelfSignedCertificate -Type CodeSigningCert `
  -Subject "CN=TaskFlow TEST CERT - NOT A TRUSTED PUBLISHER" `
  -CertStoreLocation Cert:\CurrentUser\My `
  -KeyAlgorithm RSA -KeyLength 3072 -HashAlgorithm SHA256 `
  -NotAfter (Get-Date).AddDays(30)
$env:TASKFLOW_SIGN_THUMBPRINT = $c.Thumbprint
$env:TASKFLOW_SIGN_ALLOW_UNTRUSTED = "1"   # a self-signed chain cannot validate
```

Do **not** add that certificate to Trusted Root or Trusted Publishers. It would
make the verification pass on this one machine and tell you nothing about any
other, which is the opposite of what the check is for. Delete it and the
installer it produced when you are done — an artifact that looks signed and is
not is worse than an unsigned one.
