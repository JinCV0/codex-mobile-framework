<#
  Restart the desktop side with the freshly built exe.

  It does three things:
    1. stop the running codex-mobile
    2. copy codex-mobile\codex-mobile.exe to %LOCALAPPDATA%\CodexMobile\app
       (that is the copy the machine actually runs from)
    3. start it again and check the API answers

  Usage:  double-click this file, or run it with
          powershell -ExecutionPolicy Bypass -File .\restart-desktop.ps1

  ASCII only on purpose: Windows PowerShell reads .ps1 as GBK unless the file
  has a UTF-8 BOM, and the project path contains non-ASCII characters.
  $PSScriptRoot is used for the same reason, so no Chinese path is written here.
#>
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'

$root = $PSScriptRoot
$src  = Join-Path $root 'codex-mobile\codex-mobile.exe'
if (-not (Test-Path -LiteralPath $src)) {
  throw "not found: $src  (build the desktop side first)"
}

$appDir = Join-Path $env:LOCALAPPDATA 'CodexMobile\app'
$dst    = Join-Path $appDir 'codex-mobile.exe'

Write-Host 'stopping old instance ...'
Get-Process -Name codex-mobile -ErrorAction SilentlyContinue |
  ForEach-Object { Stop-Process -Id $_.Id -Force }
Start-Sleep -Seconds 3

New-Item -ItemType Directory -Force -Path $appDir | Out-Null
Copy-Item -LiteralPath $src -Destination $dst -Force
Write-Host ('copied  {0:N1} MB -> {1}' -f ((Get-Item -LiteralPath $dst).Length / 1MB), $dst)

# ---------------------------------------------------------------------------
# Codex desktop is an MSIX packaged app. Child processes it launches get file
# system redirection, so %LOCALAPPDATA% actually lands under
#   ...\Packages\<pkg>\LocalCache\Local\CodexMobile
# The desktop runs THAT copy of the exe. If it is not refreshed too, the
# desktop keeps launching an old build -- which is exactly what stopped the
# desktop from starting (the old build mis-parsed its own -tray=false flag,
# handed it to the real codex, and the app-server never came up).
# ---------------------------------------------------------------------------
$msixRoot = Join-Path $env:LOCALAPPDATA 'Packages'
$msixDirs = @(Get-ChildItem -Path (Join-Path $msixRoot '*\LocalCache\Local\CodexMobile') -Directory -ErrorAction SilentlyContinue)
if ($msixDirs.Count -eq 0) {
  Write-Host '  no MSIX desktop store found - nothing to sync' -ForegroundColor DarkGray
}
foreach ($d in $msixDirs) {
  $exeDst = Join-Path $d.FullName 'app\codex-mobile.exe'
  New-Item -ItemType Directory -Force -Path (Split-Path $exeDst) | Out-Null
  # The old exe is sometimes held open (AV scan, desktop child handle...).
  # Rename is allowed on a locked file when delete is not, so park it and retry.
  $synced = $false
  for ($try = 1; $try -le 5 -and -not $synced; $try++) {
    try {
      Copy-Item -LiteralPath $src -Destination $exeDst -Force -ErrorAction Stop
      $synced = $true
    } catch {
      try {
        $parked = $exeDst + '.old'
        if (Test-Path -LiteralPath $parked) { Remove-Item -LiteralPath $parked -Force -ErrorAction SilentlyContinue }
        Rename-Item -LiteralPath $exeDst -NewName 'codex-mobile.exe.old' -ErrorAction Stop
        Copy-Item -LiteralPath $src -Destination $exeDst -Force -ErrorAction Stop
        $synced = $true
      } catch {
        Write-Host ('  sync try {0} failed: {1}' -f $try, $_.Exception.Message) -ForegroundColor DarkYellow
        Start-Sleep -Seconds 3
      }
    }
  }
  if ($synced) {
    Write-Host ('  synced exe -> {0}' -f $exeDst) -ForegroundColor DarkGray
  } else {
    Write-Host ('  WARNING: could not sync {0} - desktop side keeps the old build' -f $exeDst) -ForegroundColor Red
  }

  # A stale attach.json here shadows the real one: the desktop-side proxy reads
  # the redirected store first, dials a dead port and then needlessly spawns a
  # second service. The service always writes the REAL path, so any copy in
  # here is stale by definition - park it instead of deleting.
  $staleAttach = Join-Path $d.FullName 'attach.json'
  if (Test-Path -LiteralPath $staleAttach) {
    Move-Item -LiteralPath $staleAttach -Destination ($staleAttach + '.stale') -Force
    Write-Host '  parked stale attach.json' -ForegroundColor DarkGray
  }
}

# Pick the data dir the service must run with. The phone pairs through the
# Funnel node authenticated inside the MSIX-packaged store; the plain
# %LOCALAPPDATA%\CodexMobile node is a different, usually-unlogged-in tsnet
# identity. Starting without -data here silently swaps the node and the phone
# goes offline, so prefer the MSIX store whenever it exists.
$msixPackagesRoot = Join-Path $env:LOCALAPPDATA 'Packages'
$msixDataDirs = @(Get-ChildItem -Path (Join-Path $msixPackagesRoot '*\LocalCache\Local\CodexMobile') -Directory -ErrorAction SilentlyContinue | ForEach-Object { $_.FullName })
if ($msixDataDirs.Count -gt 0) {
  $runDataDir = $msixDataDirs[0]
} else {
  $runDataDir = Join-Path $env:LOCALAPPDATA 'CodexMobile'
}

Write-Host ('starting ...  data = ' + $runDataDir)
# Same flags the desktop bridge uses when it spawns the service (no tray, no QR).
Start-Process -FilePath $dst -WorkingDirectory $appDir -WindowStyle Hidden `
  -ArgumentList @('-tray=false', '-qr=false', '-data', $runDataDir)
Start-Sleep -Seconds 15

$p = Get-Process -Name codex-mobile -ErrorAction SilentlyContinue
if ($p) {
  Write-Host ('running  pid {0}' -f ($p | Select-Object -First 1).Id) -ForegroundColor Green
} else {
  Write-Host 'WARNING: not running, check the log under the data dir' -ForegroundColor Yellow
}

# Check against the token of the dir we actually started with.
$tokenFile = Join-Path $runDataDir 'token.txt'
if (Test-Path -LiteralPath $tokenFile) {
  $token = (Get-Content -Raw -LiteralPath $tokenFile).Trim()
  foreach ($api in 'state', 'sysinfo') {
    try {
      $r = Invoke-WebRequest -Uri "http://127.0.0.1:7799/api/$api`?k=$token" -UseBasicParsing -TimeoutSec 20
      Write-Host ('api {0,-7} ok  {1} bytes' -f $api, $r.Content.Length) -ForegroundColor Green
    } catch {
      Write-Host ('api {0,-7} FAILED: {1}' -f $api, $_.Exception.Message) -ForegroundColor Red
    }
  }
  # The entry URL is what the phone actually uses. It is the tailnet address
  # by default and the Funnel address only when -funnel is enabled.
  try {
    $st = (Invoke-WebRequest -Uri "http://127.0.0.1:7799/api/state?k=$token" -UseBasicParsing -TimeoutSec 20).Content | ConvertFrom-Json
    Write-Host ('entryUrl   ' + $st.publicUrl)
    if ($st.publicUrl) {
      $funnelTry = {
        param($u)
        try {
          $f = Invoke-WebRequest -Uri ($u -replace '/\?k=','/api/state?k=') -UseBasicParsing -TimeoutSec 25
          Write-Host ('entry      ok  {0} bytes' -f $f.Content.Length) -ForegroundColor Green
          return $true
        } catch { return $false }
      }
      if (-not (& $funnelTry $st.publicUrl)) {
        Write-Host 'entry      not ready yet, retrying once ...' -ForegroundColor DarkYellow
        Start-Sleep -Seconds 20
        if (-not (& $funnelTry $st.publicUrl)) {
          Write-Host 'entry      FAILED - check Tailscale login and 日志.log' -ForegroundColor Red
        }
      }
    } else {
      Write-Host 'entryUrl   EMPTY - tsnet node not logged in; phone cannot connect!' -ForegroundColor Red
    }
  } catch {
    Write-Host ('state parse failed: {0}' -f $_.Exception.Message) -ForegroundColor Red
  }
}

# ---------------------------------------------------------------------------
# The desktop bridge (the sibling switch script with -Install / -Uninstall)
# rides on an unofficial hook: it
# points CODEX_CLI_PATH at codex-mobile.exe so the Codex desktop app launches
# us instead of the real CLI. A desktop app update can break that silently, so
# compare the version recorded when the bridge was installed with the one
# installed now, and say it out loud here - this is the script you reach for
# when the phone misbehaves for no visible reason.
# ---------------------------------------------------------------------------
$bridgeCfg = Join-Path $env:LOCALAPPDATA 'CodexMobile\desktop-bridge.json'
$cliPath   = [Environment]::GetEnvironmentVariable('CODEX_CLI_PATH', 'User')
if ((Test-Path -LiteralPath $bridgeCfg) -and -not [string]::IsNullOrWhiteSpace($cliPath)) {
  try {
    $bcfg = Get-Content -Raw -LiteralPath $bridgeCfg | ConvertFrom-Json
    $pkg  = Get-AppxPackage -Name 'OpenAI.Codex' -ErrorAction SilentlyContinue | Select-Object -First 1
    $now  = if ($pkg) { [string]$pkg.Version } else { '' }
    $was  = [string]$bcfg.desktopVersion
    if (-not $was) {
      Write-Host 'bridge     no version anchor recorded - re-run the bridge script -Install' -ForegroundColor DarkYellow
    } elseif ($now -and $was -ne $now) {
      Write-Host ('bridge     VERSION DRIFT: bridge verified on {0}, desktop app is now {1}' -f $was, $now) -ForegroundColor Yellow
      Write-Host 'bridge     the bridge rides on an unofficial hook - suspect it first when the phone misbehaves' -ForegroundColor Yellow
      Write-Host 'bridge     recovery: bridge script -Uninstall, restart the desktop app, then see dev doc 9.2' -ForegroundColor DarkYellow
    } else {
      Write-Host ('bridge     ok (verified on {0})' -f $was) -ForegroundColor DarkGray
    }
  } catch {
    Write-Host ('bridge     version check failed: {0}' -f $_.Exception.Message) -ForegroundColor DarkYellow
  }
}

Write-Host ''
Write-Host 'done. on the phone: pull to refresh, or reopen the page.' -ForegroundColor Green
