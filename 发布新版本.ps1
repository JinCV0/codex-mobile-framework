<#
  Publish the freshly built APK to the desktop app's update folder,
  so phones can pick it up from "Me -> Check for updates".

  Usage:
    powershell -ExecutionPolicy Bypass -File .\发布新版本.ps1
    powershell -ExecutionPolicy Bypass -File .\发布新版本.ps1 -Notes "fixed scanner crash"

  ASCII only on purpose: Windows PowerShell reads .ps1 as GBK unless the file
  has a UTF-8 BOM, which turns Chinese comments into syntax errors.
#>
[CmdletBinding()]
param(
  [string]$Notes = '',
  [string]$Root = ''
)

$ErrorActionPreference = 'Stop'

if (-not $Root) { $Root = $PSScriptRoot }

$gradleFile = Join-Path $Root 'android\app\app\build.gradle.kts'
$apkFile    = Join-Path $Root 'android\app\app\build\outputs\apk\release\app-release.apk'

if (-not (Test-Path $gradleFile)) { throw "not found: $gradleFile" }
if (-not (Test-Path $apkFile)) {
  throw "APK not built yet. Run:  cd android\app ; gradle assembleRelease"
}

$txt = Get-Content -Raw $gradleFile
$verCode = [int]([regex]::Match($txt, 'versionCode\s*=\s*(\d+)').Groups[1].Value)
$verName = [regex]::Match($txt, 'versionName\s*=\s*"([^"]+)"').Groups[1].Value
if ($verCode -le 0 -or -not $verName) { throw "cannot read version from build.gradle.kts" }

# 1. keep a copy next to the project (same name the user sees in the folder)
$named = Join-Path $Root ("CodexMobile-v{0}.apk" -f $verName)
Copy-Item $apkFile $named -Force

# 2. publish into the desktop app's update folder
$dir = Join-Path $env:LOCALAPPDATA 'CodexMobile\update'
New-Item -ItemType Directory -Force -Path $dir | Out-Null
Copy-Item $apkFile (Join-Path $dir 'app.apk') -Force

$size = (Get-Item $apkFile).Length
$json = [ordered]@{
  versionCode = $verCode
  versionName = $verName
  notes       = $Notes
  size        = $size
  publishedAt = (Get-Date).ToString('yyyy-MM-dd HH:mm:ss')
}
[IO.File]::WriteAllText((Join-Path $dir 'version.json'), ($json | ConvertTo-Json),
  [Text.UTF8Encoding]::new($false))

# ---------------------------------------------------------------------------
# Codex desktop is an MSIX packaged app: the instance it launches is file-
# redirected, so its "update" folder actually lives under
#   ...\Packages\<pkg>\LocalCache\Local\CodexMobile\update
# That instance is the one the phone talks to (it owns the logged-in tsnet
# node and port 443). Publishing only to the real folder means phones keep
# seeing the old version.json forever. Mirror the payload into every MSIX
# store found, same trick as the restart script uses for the exe.
# ---------------------------------------------------------------------------
$msixRoot = Join-Path $env:LOCALAPPDATA 'Packages'
$msixDirs = @(Get-ChildItem -Path (Join-Path $msixRoot '*\LocalCache\Local\CodexMobile') -Directory -ErrorAction SilentlyContinue)
foreach ($d in $msixDirs) {
  $updDir = Join-Path $d.FullName 'update'
  New-Item -ItemType Directory -Force -Path $updDir | Out-Null
  Copy-Item $apkFile (Join-Path $updDir 'app.apk') -Force
  [IO.File]::WriteAllText((Join-Path $updDir 'version.json'), ($json | ConvertTo-Json),
    [Text.UTF8Encoding]::new($false))
  Write-Host ("  mirrored update -> {0}" -f $updDir) -ForegroundColor DarkGray
}

Write-Host ""
Write-Host ("published  v{0} (code {1}, {2:N2} MB)" -f $verName, $verCode, ($size / 1MB)) -ForegroundColor Green
Write-Host ("  apk     : {0}" -f $named)
Write-Host ("  update  : {0}" -f $dir)
Write-Host ""
Write-Host "phones will see it in Me -> Check for updates" -ForegroundColor Gray

# ---------------------------------------------------------------------------
# Keep the repo in step with the version we just published:
#   1. README records the current versionName - rewrite that line in place.
#   2. Tag the current commit v<verName>, so "roll back to a version" works.
# Both steps are best effort: a failure here must never break publishing.
# ASCII only, see the note at the top of this file.
# ---------------------------------------------------------------------------
try {
  $readme = Join-Path $Root 'README.md'
  if (Test-Path $readme) {
    $md = [IO.File]::ReadAllText($readme, [Text.UTF8Encoding]::new($false))
    $pat = 'versionName = "[0-9][^"]*"'
    if ($md -match $pat) {
      $fixed = [regex]::Replace($md, $pat, ('versionName = "' + $verName + '"'))
      if ($fixed -ne $md) {
        [IO.File]::WriteAllText($readme, $fixed, [Text.UTF8Encoding]::new($false))
        Write-Host ("  README version -> {0}" -f $verName) -ForegroundColor DarkGray
      } else {
        Write-Host ("  README already says {0}" -f $verName) -ForegroundColor DarkGray
      }
    } else {
      Write-Host "  README has no versionName line, skipped" -ForegroundColor Yellow
    }
  }
} catch {
  Write-Host ("  README version write-back failed: {0}" -f $_.Exception.Message) -ForegroundColor Yellow
}

try {
  if (Get-Command git -ErrorAction SilentlyContinue) {
    $tagName = 'v' + $verName
    if (git -C $Root tag -l $tagName) {
      Write-Host ("  tag {0} already exists, skipped" -f $tagName) -ForegroundColor DarkGray
    } else {
      $dirty = @(git -C $Root status --porcelain)
      $tagMsg = 'CodexMobile ' + $tagName + [Environment]::NewLine +
                'published ' + (Get-Date -Format 'yyyy-MM-dd HH:mm') + ', versionCode ' + $verCode
      git -C $Root tag -a $tagName -m $tagMsg
      if ($LASTEXITCODE -eq 0) {
        $head = git -C $Root rev-parse --short HEAD
        Write-Host ("  tagged {0} at {1}" -f $tagName, $head) -ForegroundColor Green
        if ($dirty.Count -gt 0) {
          Write-Host "  WARNING: working tree is dirty, the tag covers the last commit only" -ForegroundColor Yellow
        }
        Write-Host "  tag is local - push with: git push origin main --tags" -ForegroundColor Gray
      } else {
        Write-Host ("  git tag {0} failed" -f $tagName) -ForegroundColor Yellow
      }
    }
  }
} catch {
  Write-Host ("  git tag failed: {0}" -f $_.Exception.Message) -ForegroundColor Yellow
}
