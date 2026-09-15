# Verify that everything we ship is Authenticode-signed (Windows).
#
# The bundler logs "Signing <file>" and moves on; it never checks the result.
# That is the same shape of trap the OCR staging had -- a tool that reports
# success while producing an artifact that fails on a user's machine. So this
# inspects the files themselves.
#
# Three things are checked, and each one is a separate way to ship a broken
# signature:
#   - signed at all      : an unsigned DLL beside a signed exe still triggers
#                          the warning the signature was bought to avoid
#   - chain trusted      : a signature nobody can validate is decoration
#   - timestamped        : without a timestamp the signature stops validating
#                          the day the certificate expires, retroactively, on
#                          copies already downloaded
[CmdletBinding()]
param(
  # Files or directories. Directories are walked for .exe and .dll.
  [Parameter(Mandatory = $true)][string[]]$Path,
  # Accept a signature whose chain does not validate. Only for a self-signed
  # test certificate, where an untrusted chain is the expected outcome.
  [switch]$AllowUntrusted
)

$ErrorActionPreference = "Stop"

$files = @()
foreach ($p in $Path) {
  if (-not (Test-Path $p)) { Write-Error "not found: $p" }
  $item = Get-Item -LiteralPath $p
  if ($item.PSIsContainer) {
    # -Include is silently ignored next to -LiteralPath and let .traineddata
    # through, so filter on the extension itself.
    $files += Get-ChildItem -LiteralPath $p -Recurse -File |
      Where-Object { $_.Extension -eq ".exe" -or $_.Extension -eq ".dll" }
  } else {
    $files += $item
  }
}

if ($files.Count -eq 0) { Write-Error "no .exe or .dll found under: $($Path -join ', ')" }

$failures = @()
$signers = @{}

foreach ($f in $files) {
  $sig = Get-AuthenticodeSignature -LiteralPath $f.FullName

  if ($sig.Status -eq "NotSigned" -or -not $sig.SignerCertificate) {
    $failures += "UNSIGNED           $($f.FullName)"
    continue
  }

  # UnknownError is a catch-all: it is what an untrusted root reports, but it
  # is also what an EXPIRED certificate reports -- the bundled tesseract.exe
  # arrives in exactly that state. So -AllowUntrusted waves both through, which
  # is only acceptable because it exists for the self-signed test path. A real
  # certificate must never need it. Everything else stays distinguished from
  # the genuinely broken states (HashMismatch = the file changed after signing).
  $untrusted = $sig.Status -eq "UnknownError"
  if ($sig.Status -ne "Valid" -and -not ($untrusted -and $AllowUntrusted)) {
    $failures += ("{0,-18} {1}" -f $sig.Status, $f.FullName)
    continue
  }

  if (-not $sig.TimeStamperCertificate) {
    $failures += "NO TIMESTAMP       $($f.FullName)"
    continue
  }

  $subject = $sig.SignerCertificate.Subject
  if (-not $signers.ContainsKey($subject)) { $signers[$subject] = 0 }
  $signers[$subject]++
}

Write-Host ""
Write-Host ("checked {0} binaries" -f $files.Count)
foreach ($s in ($signers.Keys | Sort-Object)) {
  Write-Host ("  {0,4}  {1}" -f $signers[$s], $s)
}

if ($failures.Count -gt 0) {
  Write-Host ""
  Write-Host ("{0} file(s) failed:" -f $failures.Count)
  foreach ($f in $failures) { Write-Host "  $f" }
  Write-Host ""
  Write-Host "signature verification FAILED"
  exit 1
}

Write-Host ""
Write-Host "all signatures present, chained and timestamped"
exit 0
