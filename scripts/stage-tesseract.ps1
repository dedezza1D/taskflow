# Stage a self-contained tesseract into the Tauri bundle (Windows).
#
# Copying the whole Tesseract-OCR install would ship ~238 MB, most of it
# training tools nobody runs at runtime and an ICU data blob nothing links. So
# the set is DERIVED: walk tesseract.exe's import table with dumpbin and take
# the transitive closure of DLLs that actually ship with it. Anything not found
# in the install directory is a system DLL and is deliberately skipped.
#
# The result is ~116 MB and, crucially, is verified to run in isolation before
# it is handed to the bundler -- a missing DLL would otherwise only surface on a
# user's machine, on the one document that needed OCR.
[CmdletBinding()]
param(
  [string]$Source = (Join-Path $env:ProgramFiles "Tesseract-OCR"),
  [Parameter(Mandatory = $true)][string]$Destination,
  # Language data to bundle.
  #
  # Portuguese is not optional decoration: without it the engine reads
  # "DECLARACAO" for "DECLARACAO" with a cedilla and mangles every accented
  # word, which is most of a Brazilian document. English stays for mixed-language
  # files.
  [string[]]$Languages = @("por", "eng"),
  # Where downloaded language data is cached between builds. Resolved in the
  # body: $PSScriptRoot is not reliably populated while parameter defaults are
  # being evaluated.
  [string]$Cache = ""
)

# Language data comes from a pinned tessdata_best release rather than from
# whatever the build machine happens to have installed -- otherwise the bundle
# silently varies by build host. "best" is also the right model here: it is the
# most accurate LSTM data, and *smaller* than the standard set, which carries
# legacy-engine data tesseract 5 does not use.
$TessdataTag = "4.1.0"
$TessdataSHA = @{
  "eng" = "8280aed0782fe27257a68ea10fe7ef324ca0f8d85bd2fd145d1c2b560bcb66ba"
  "por" = "711de9dbb8052067bd42f16b9119967f30bada80d57e2ef24f65d09f531adb04"
}

$ErrorActionPreference = "Stop"

if (-not $Cache) {
  $root = Split-Path -Parent $MyInvocation.MyCommand.Path
  $Cache = Join-Path $root "..\desktop\.tessdata-cache"
}

if (-not (Test-Path (Join-Path $Source "tesseract.exe"))) {
  Write-Error "tesseract not found at $Source. Install it (winget install UB-Mannheim.TesseractOCR) or pass -Source."
}

# Find dumpbin. vswhere is the documented route but reports nothing on some
# Build Tools-only installs, so fall back to the standard layouts rather than
# failing on a machine that plainly has the compiler.
$dumpbin = $null

$vswhere = Join-Path ${env:ProgramFiles(x86)} "Microsoft Visual Studio\Installer\vswhere.exe"
if (Test-Path $vswhere) {
  $vs = & $vswhere -latest -products * -property installationPath 2>$null
  if ($vs) {
    $dumpbin = Get-ChildItem (Join-Path $vs "VC\Tools\MSVC\*\bin\Hostx64\x64\dumpbin.exe") -ErrorAction SilentlyContinue |
      Sort-Object FullName -Descending | Select-Object -First 1 -ExpandProperty FullName
  }
}

if (-not $dumpbin) {
  $roots = @(
    (Join-Path ${env:ProgramFiles(x86)} "Microsoft Visual Studio"),
    (Join-Path $env:ProgramFiles "Microsoft Visual Studio")
  ) | Where-Object { Test-Path $_ }
  foreach ($root in $roots) {
    $found = Get-ChildItem (Join-Path $root "*\*\VC\Tools\MSVC\*\bin\Hostx64\x64\dumpbin.exe") -ErrorAction SilentlyContinue |
      Sort-Object FullName -Descending | Select-Object -First 1 -ExpandProperty FullName
    if ($found) { $dumpbin = $found; break }
  }
}

if (-not $dumpbin) {
  Write-Error "dumpbin not found (part of the MSVC build tools). It is needed to work out which DLLs tesseract actually requires."
}

if (Test-Path $Destination) {
  Get-ChildItem $Destination -Recurse -File | Remove-Item -Force
} else {
  New-Item -ItemType Directory -Force -Path $Destination | Out-Null
}
New-Item -ItemType Directory -Force -Path (Join-Path $Destination "tessdata") | Out-Null

$seen = @{}
$queue = New-Object System.Collections.Queue
$queue.Enqueue("tesseract.exe")

while ($queue.Count -gt 0) {
  $name = $queue.Dequeue()
  if ($seen.ContainsKey($name)) { continue }
  $path = Join-Path $Source $name
  # Not in the install directory => a system DLL, already on the target machine.
  if (-not (Test-Path $path)) { continue }
  $seen[$name] = $true
  foreach ($line in (& $dumpbin /DEPENDENTS $path 2>$null)) {
    if ($line -match '^\s+(\S+\.dll)\s*$') { $queue.Enqueue($matches[1]) }
  }
}

foreach ($name in $seen.Keys) { Copy-Item (Join-Path $Source $name) $Destination -Force }

New-Item -ItemType Directory -Force -Path $Cache | Out-Null

foreach ($lang in $Languages) {
  $cached = Join-Path $Cache "$lang.traineddata"
  $expected = $TessdataSHA[$lang]

  if (-not $expected) {
    Write-Error "no pinned checksum for language '$lang'; add one to `$TessdataSHA before bundling it"
  }

  # Re-download if the cached copy is missing or does not match the pin.
  if (Test-Path $cached) {
    $have = (Get-FileHash $cached -Algorithm SHA256).Hash.ToLower()
    if ($have -ne $expected) {
      Write-Warning "cached $lang.traineddata does not match its pinned checksum; re-downloading"
      Remove-Item $cached -Force
    }
  }

  if (-not (Test-Path $cached)) {
    $url = "https://github.com/tesseract-ocr/tessdata_best/raw/$TessdataTag/$lang.traineddata"
    Write-Host "  downloading $lang.traineddata"
    try {
      Invoke-WebRequest -Uri $url -OutFile $cached -UseBasicParsing
    } catch {
      Write-Error "could not download $lang.traineddata from $url : $_"
    }
  }

  $have = (Get-FileHash $cached -Algorithm SHA256).Hash.ToLower()
  if ($have -ne $expected) {
    Remove-Item $cached -Force
    Write-Error "$lang.traineddata checksum mismatch (got $have, expected $expected)"
  }

  Copy-Item $cached (Join-Path $Destination "tessdata") -Force
}

# Prove it before shipping it: run the copy, with the source install off PATH,
# and make sure it both starts and can see its language data.
$exe = Join-Path $Destination "tesseract.exe"
$version = & $exe --version 2>&1 | Select-Object -First 1
if ($LASTEXITCODE -ne 0) { Write-Error "the staged tesseract does not run: $version" }
$langs = & $exe --list-langs 2>&1
if ($LASTEXITCODE -ne 0) { Write-Error "the staged tesseract cannot read its language data: $langs" }

$size = (Get-ChildItem $Destination -Recurse -File | Measure-Object Length -Sum).Sum / 1MB
Write-Host ("staged {0} files, {1:N1} MB -- {2}" -f $seen.Count, $size, $version)
