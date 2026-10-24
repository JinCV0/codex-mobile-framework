<#
  Codex 手机端 · Android 打包工具链一键安装（免管理员、全绿色）

  装完之后可以反复用：
    - 编译 gomobile 的 .aar
    - 用 Gradle 打 APK

  用法：
    powershell -ExecutionPolicy Bypass -File .\setup-android.ps1
    powershell -ExecutionPolicy Bypass -File .\setup-android.ps1 -SkipNdk     # 不装 NDK（约省 1GB）
    powershell -ExecutionPolicy Bypass -File .\setup-android.ps1 -Verify      # 只检查现状

  所有东西都放在 -Root 指定的目录下（默认 D:\tools），不写系统目录，不需要管理员。
#>
[CmdletBinding()]
param(
  [string]$Root = 'D:\tools',
  [string]$SdkPlatform = 'android-35',
  [string]$BuildTools = '35.0.0',
  [string]$NdkVersion = '27.2.12479018',
  [string]$GradleVersion = '8.14',
  [switch]$SkipNdk,
  [switch]$Verify
)

# 用 Continue：sdkmanager 之类的外部命令会往 stderr 写警告，
# 在 Stop 模式下会被当成致命错误。真正的失败我们自己 throw。
$ErrorActionPreference = 'Continue'
$ProgressPreference = 'SilentlyContinue'

$JdkDir    = Join-Path $Root 'jdk17'
$SdkDir    = Join-Path $Root 'android-sdk'
$GradleDir = Join-Path $Root "gradle-$GradleVersion"
$DlDir     = Join-Path $Root 'downloads'

$JdkUrl     = 'https://mirrors.tuna.tsinghua.edu.cn/Adoptium/17/jdk/x64/windows/OpenJDK17U-jdk_x64_windows_hotspot_17.0.20.1_1.zip'
$ToolsUrl   = 'https://dl.google.com/android/repository/commandlinetools-win-16111833_latest.zip'
$GradleUrl  = "https://mirrors.cloud.tencent.com/gradle/gradle-$GradleVersion-bin.zip"

function Say($msg, $color = 'Gray') { Write-Host $msg -ForegroundColor $color }
function Step($msg) { Write-Host ""; Write-Host "==> $msg" -ForegroundColor Cyan }

function Get-RemoteFile {
  param([string]$Url, [string]$Dest, [string]$Label)
  if (Test-Path $Dest) {
    $size = (Get-Item $Dest).Length
    if ($size -gt 1MB) { Say ("   已有 {0} ({1:N1} MB)，跳过下载" -f $Label, ($size / 1MB)); return }
  }
  Say ("   下载 {0} ..." -f $Label)
  & curl.exe -L --fail --retry 3 --retry-delay 2 -m 3600 -o $Dest $Url
  if ($LASTEXITCODE -ne 0 -or -not (Test-Path $Dest)) { throw "下载失败: $Url" }
  Say ("   完成 {0:N1} MB" -f ((Get-Item $Dest).Length / 1MB)) 'Green'
}

function Expand-To {
  param([string]$Zip, [string]$Dest)
  $stage = Join-Path $DlDir ('x_' + [IO.Path]::GetFileNameWithoutExtension($Zip))
  if (Test-Path $stage) { Remove-Item -LiteralPath $stage -Recurse -Force }
  New-Item -ItemType Directory -Force -Path $stage | Out-Null
  Say "   解压 $(Split-Path $Zip -Leaf) ..."
  try {
    Expand-Archive -Path $Zip -DestinationPath $stage -Force
  } catch {
    throw "解压失败: $Zip`n$($_.Exception.Message)"
  }
  if (Test-Path $Dest) { Remove-Item -LiteralPath $Dest -Recurse -Force }
  $entries = Get-ChildItem -LiteralPath $stage
  if ($entries.Count -eq 1 -and $entries[0].PSIsContainer) {
    Move-Item -LiteralPath $entries[0].FullName -Destination $Dest
  } else {
    Move-Item -LiteralPath $stage -Destination $Dest
  }
  if (Test-Path $stage) { Remove-Item -LiteralPath $stage -Recurse -Force }
}

function Add-UserPath {
  param([string]$Dir)
  $cur = [Environment]::GetEnvironmentVariable('Path', 'User')
  if ($cur -notlike "*$Dir*") {
    [Environment]::SetEnvironmentVariable('Path', ($cur.TrimEnd(';') + ';' + $Dir), 'User')
    Say "   已加入用户 PATH: $Dir"
  }
}

function Load-Env {
  $env:JAVA_HOME = $JdkDir
  $env:ANDROID_HOME = $SdkDir
  $env:ANDROID_SDK_ROOT = $SdkDir
  $env:Path = (@($JdkDir + '\bin', (Join-Path $SdkDir 'platform-tools'),
                  (Join-Path $SdkDir 'cmdline-tools\latest\bin'), (Join-Path $GradleDir 'bin'),
                  $env:Path) -join ';')
}

# ---------------------------------------------------------------- 检查模式
if ($Verify) {
  Step "检查现有工具链"
  $java = Join-Path $JdkDir 'bin\java.exe'
  if (Test-Path $java) { & $java -version 2>&1 | Select-Object -First 1 | ForEach-Object { Say "  JDK      $_" 'Green' } }
  else { Say "  JDK      未安装 ($JdkDir)" 'Yellow' }

  $sdkmanager = Join-Path $SdkDir 'cmdline-tools\latest\bin\sdkmanager.bat'
  if (Test-Path $sdkmanager) { Say "  SDK      已安装 ($SdkDir)" 'Green' }
  else { Say "  SDK      未安装 ($SdkDir)" 'Yellow' }

  $ndk = Join-Path $SdkDir "ndk\$NdkVersion"
  if (Test-Path $ndk) { Say "  NDK      已安装 ($ndk)" 'Green' } else { Say "  NDK      未安装" 'Yellow' }

  $gradle = Join-Path $GradleDir 'bin\gradle.bat'
  if (Test-Path $gradle) { Say "  Gradle   已安装 ($GradleDir)" 'Green' } else { Say "  Gradle   未安装" 'Yellow' }

  Say ""
  Say "  ANDROID_HOME = $([Environment]::GetEnvironmentVariable('ANDROID_HOME','User'))"
  Say "  JAVA_HOME    = $([Environment]::GetEnvironmentVariable('JAVA_HOME','User'))"
  return
}

# ---------------------------------------------------------------- 1. 目录
Step "准备目录"
New-Item -ItemType Directory -Force -Path $Root, $DlDir | Out-Null
Say "  根目录: $Root"

# ---------------------------------------------------------------- 2. JDK
Step "JDK 17"
if (Test-Path (Join-Path $JdkDir 'bin\java.exe')) {
  Say "  已存在，跳过"
} else {
  $zip = Join-Path $DlDir 'jdk17.zip'
  Get-RemoteFile -Url $JdkUrl -Dest $zip -Label 'Temurin JDK 17'
  Expand-To -Zip $zip -Dest $JdkDir
}

# ---------------------------------------------------------------- 3. Gradle
Step "Gradle $GradleVersion"
if (Test-Path (Join-Path $GradleDir 'bin\gradle.bat')) {
  Say "  已存在，跳过"
} else {
  $zip = Join-Path $DlDir "gradle-$GradleVersion-bin.zip"
  Get-RemoteFile -Url $GradleUrl -Dest $zip -Label "Gradle $GradleVersion"
  Expand-To -Zip $zip -Dest $GradleDir
}

# ---------------------------------------------------------------- 4. SDK 命令行工具
Step "Android SDK 命令行工具"
$sdkmanager = Join-Path $SdkDir 'cmdline-tools\latest\bin\sdkmanager.bat'
if (Test-Path $sdkmanager) {
  Say "  已存在，跳过"
} else {
  New-Item -ItemType Directory -Force -Path (Join-Path $SdkDir 'cmdline-tools') | Out-Null
  $zip = Join-Path $DlDir 'commandlinetools-win.zip'
  Get-RemoteFile -Url $ToolsUrl -Dest $zip -Label 'cmdline-tools'
  # 这个 zip 解出来是 cmdline-tools/，必须放到 cmdline-tools/latest/ 才算正规布局
  Expand-To -Zip $zip -Dest (Join-Path $SdkDir 'cmdline-tools\latest')
}

Load-Env

# ---------------------------------------------------------------- 5. SDK 组件
Step "SDK 组件（platform-tools / $SdkPlatform / build-tools;$BuildTools）"
$packages = @('platform-tools', "platforms;$SdkPlatform", "build-tools;$BuildTools")
if (-not $SkipNdk) { $packages += "ndk;$NdkVersion" }

# 新的 cmdline-tools 用 `android sdk install`，老的 sdkmanager 已废弃但仍可用。
$androidCli = Join-Path $SdkDir 'cmdline-tools\latest\bin\android.exe'
if (Test-Path $androidCli) {
  Say "  使用新版 android CLI（sdkmanager 已废弃）"
  foreach ($pkg in $packages) {
    Say "    安装 $pkg ..."
    & $androidCli "--sdk=$SdkDir" sdk install $pkg 2>&1 | Select-Object -Last 2 | ForEach-Object { Say "      $_" }
    if ($LASTEXITCODE -ne 0) { throw "安装 $pkg 失败（返回码 $LASTEXITCODE）" }
  }
} else {
  Say "  接受许可协议 ..."
  $yes = 1..80 | ForEach-Object { 'y' }
  $yes | & $sdkmanager "--sdk_root=$SdkDir" --licenses 2>&1 | Select-Object -Last 3 | ForEach-Object { Say "    $_" }
  Say "  安装: $($packages -join ', ')"
  & $sdkmanager "--sdk_root=$SdkDir" @packages 2>&1 | Select-Object -Last 8 | ForEach-Object { Say "    $_" }
  if ($LASTEXITCODE -ne 0) { throw "SDK 组件安装失败（返回码 $LASTEXITCODE）" }
}

# ---------------------------------------------------------------- 6. 环境变量
Step "写入用户环境变量（不需要管理员）"
foreach ($pair in @(
    @('JAVA_HOME', $JdkDir),
    @('ANDROID_HOME', $SdkDir),
    @('ANDROID_SDK_ROOT', $SdkDir),
    @('GRADLE_HOME', $GradleDir))) {
  [Environment]::SetEnvironmentVariable($pair[0], $pair[1], 'User')
  Say ("  {0} = {1}" -f $pair[0], $pair[1])
}
Add-UserPath (Join-Path $JdkDir 'bin')
Add-UserPath (Join-Path $GradleDir 'bin')
Add-UserPath (Join-Path $SdkDir 'platform-tools')
Add-UserPath (Join-Path $SdkDir 'cmdline-tools\latest\bin')

# ---------------------------------------------------------------- 7. 验收
Step "验收"
Load-Env
& (Join-Path $JdkDir 'bin\java.exe') -version 2>&1 | Select-Object -First 1 | ForEach-Object { Say "  JDK      $_" 'Green' }
$gv = & (Join-Path $GradleDir 'bin\gradle.bat') --version 2>&1 | Select-String '^Gradle '
Say "  Gradle   $gv" 'Green'
$ndkState = '未安装'
if (Test-Path (Join-Path $SdkDir "ndk\$NdkVersion")) { $ndkState = '已安装' }
Say "  NDK      $ndkState" 'Green'
Say ""
Say "  完成。新开的终端会自动带上这些环境变量。" 'Green'
Say "  当前会话也可以先执行：" 'Gray'
Say "    `$env:JAVA_HOME='$JdkDir'; `$env:ANDROID_HOME='$SdkDir'; `$env:ANDROID_SDK_ROOT='$SdkDir'" 'Gray'
