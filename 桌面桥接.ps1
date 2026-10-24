<#
  桌面桥接开关：让 Codex 桌面版和手机端共用同一个 app-server。

  原理：把用户环境变量 CODEX_CLI_PATH 指向 codex-mobile.exe。
  桌面版启动时会拉起它认为的 "codex CLI"，于是拉起的是我们的程序；
  我们的程序再把桌面端接到常驻服务上，两边用同一个 app-server，
  "这条对话现在正在电脑上用" 的冲突就从根上没有了。

  风险与锚点：桥接依赖的是官方客户端的解析行为，不是公开 API，桌面版任何一次升级都
  可能让它静默断裂。所以 -Install 会把当时的桌面端包版本记进 desktop-bridge.json，
  -Status 与 重启电脑端.ps1 都会比对，变了就告警。细节见开发文档 §9.2。

  用法：
    powershell -ExecutionPolicy Bypass -File 桌面桥接.ps1 -Status      # 看现状
    powershell -ExecutionPolicy Bypass -File 桌面桥接.ps1 -Install     # 打开（需要重启桌面版）
    powershell -ExecutionPolicy Bypass -File 桌面桥接.ps1 -Uninstall   # 关掉，恢复原样

  注意：这个文件必须保存成带 BOM 的 UTF-8，否则 Windows PowerShell 按 GBK 读会报错。
#>
[CmdletBinding()]
param(
  [switch]$Install,
  [switch]$Uninstall,
  [switch]$Status
)

$ErrorActionPreference = 'Stop'

$dataDir   = Join-Path $env:LOCALAPPDATA 'CodexMobile'
$stableExe = Join-Path $dataDir 'app\codex-mobile.exe'
$cfgPath   = Join-Path $dataDir 'desktop-bridge.json'

function Get-RealCodex {
  $root = Join-Path $env:LOCALAPPDATA 'OpenAI\Codex\bin'
  $matches = @(Get-ChildItem -Path (Join-Path $root '*\codex.exe') -ErrorAction SilentlyContinue |
    Sort-Object LastWriteTime -Descending)
  if ($matches.Count -eq 0) { return '' }
  return $matches[0].FullName
}

function Get-DesktopVersion {
  # 桌面端是 MSIX 包，版本号只能从包里取：bin\<hash>\codex.exe 自身的版本信息是空的（实测），
  # 而 <hash> 目录是按内容哈希分的（本机 8aaf1547… 装主程序、f1e5e369… 装 rg.exe），
  # 随内容变化，不能当"按哪个版本验证过"的标识。
  try {
    $pkg = Get-AppxPackage -Name 'OpenAI.Codex' -ErrorAction SilentlyContinue |
      Select-Object -First 1
    if ($pkg) { return [string]$pkg.Version }
  } catch { }
  return ''
}

function Show-Status {
  $current = [Environment]::GetEnvironmentVariable('CODEX_CLI_PATH', 'User')
  Write-Host '当前状态'
  Write-Host ("  CODEX_CLI_PATH (用户级) : " + $(if ([string]::IsNullOrWhiteSpace($current)) { '(未设置)' } else { $current }))
  Write-Host ("  服务端可执行文件        : " + $stableExe + $(if (Test-Path $stableExe) { '  [存在]' } else { '  [缺失]' }))
  $anchor = ''
  if (Test-Path $cfgPath) {
    $cfg = Get-Content -Raw $cfgPath | ConvertFrom-Json
    Write-Host ("  记录的真 codex          : " + $cfg.realCli)
    Write-Host ("  安装前的原值            : " + $(if ([string]::IsNullOrWhiteSpace($cfg.previousCliPath)) { '(空)' } else { $cfg.previousCliPath }))
    $anchor = [string]$cfg.desktopVersion
    $when   = [string]$cfg.desktopVersionAt
    $anchorText = '(没记录：桥接是按旧脚本装的，跑一次 -Install 补上)'
    if ($anchor) { $anchorText = $anchor; if ($when) { $anchorText = "$anchor  (记录于 $when)" } }
    Write-Host ("  桥接按此版本验证        : " + $anchorText)
  } else {
    Write-Host '  记录文件                : (还没有)'
  }
  $nowVersion = Get-DesktopVersion
  Write-Host ("  当前桌面端版本          : " + $(if ($nowVersion) { $nowVersion } else { '(读不到：Get-AppxPackage 没返回 OpenAI.Codex)' }))
  if ($current -ieq $stableExe) { Write-Host '结论：桥接已打开' -ForegroundColor Green }
  elseif ([string]::IsNullOrWhiteSpace($current)) { Write-Host '结论：桥接未打开（桌面版自己管 app-server）' -ForegroundColor Yellow }
  else { Write-Host "结论：CODEX_CLI_PATH 指向了别处：$current" -ForegroundColor Yellow }
  if ($anchor -and $nowVersion -and $anchor -ne $nowVersion) {
    Write-Host ''
    Write-Host '警告：桌面端升级过了，桥接是按旧版本验证的。' -ForegroundColor Yellow
    Write-Host ("  验证时：" + $anchor + "    现在：" + $nowVersion) -ForegroundColor Yellow
    Write-Host '  桥接走的是非官方扩展点，升级后可能静默断裂（表现为「无法加载组织设置」这类模糊报错）。' -ForegroundColor Yellow
    Write-Host '  手机端若异常，先 -Uninstall 关掉桥接看是否恢复；恢复后按开发文档 §9.2 重新验证并 -Install 更新锚点。' -ForegroundColor Yellow
  }
}

if ($Status -or (-not $Install -and -not $Uninstall)) {
  Show-Status
  return
}

if ($Uninstall) {
  $previous = ''
  if (Test-Path $cfgPath) {
    $cfg = Get-Content -Raw $cfgPath | ConvertFrom-Json
    $previous = [string]$cfg.previousCliPath
  }
  if ([string]::IsNullOrWhiteSpace($previous)) {
    [Environment]::SetEnvironmentVariable('CODEX_CLI_PATH', $null, 'User')
    Write-Host '已清除 CODEX_CLI_PATH（恢复成桌面版默认行为）' -ForegroundColor Green
  } else {
    [Environment]::SetEnvironmentVariable('CODEX_CLI_PATH', $previous, 'User')
    Write-Host ("已恢复 CODEX_CLI_PATH = " + $previous) -ForegroundColor Green
  }
  Write-Host '请完全退出并重新打开 Codex 桌面版，改动才会生效。'
  return
}

# ---- Install ----
if (-not (Test-Path $stableExe)) {
  throw "找不到服务端程序：$stableExe`n先跑一次 build-and-publish.ps1 -NoApk 把新版本装到那个位置。"
}
$real = Get-RealCodex
if ([string]::IsNullOrWhiteSpace($real)) {
  throw '找不到真正的 codex（%LOCALAPPDATA%\OpenAI\Codex\bin\*\codex.exe）。请确认 Codex 桌面版已安装。'
}

$previous = [Environment]::GetEnvironmentVariable('CODEX_CLI_PATH', 'User')
# 桥接已经开着时再跑一次 -Install（例如只想刷新版本锚点），不能把当前值
# ——也就是桥接程序自己——记成"安装前的原值"，否则 -Uninstall 会"恢复"成它，
# 桥接从此关不掉。这种情况沿用上一次记录的原值。
if ($previous -ieq $stableExe) {
  $previous = ''
  if (Test-Path $cfgPath) {
    $oldCfg = Get-Content -Raw $cfgPath | ConvertFrom-Json
    $previous = [string]$oldCfg.previousCliPath
  }
}
$desktopVersion = Get-DesktopVersion
New-Item -ItemType Directory -Force -Path $dataDir | Out-Null
@{ realCli          = $real
   previousCliPath  = [string]$previous
   desktopVersion   = $desktopVersion
   desktopVersionAt = (Get-Date).ToString('yyyy-MM-dd HH:mm') } |
  ConvertTo-Json | Set-Content -Path $cfgPath -Encoding UTF8

[Environment]::SetEnvironmentVariable('CODEX_CLI_PATH', $stableExe, 'User')

Write-Host '桥接已打开' -ForegroundColor Green
Write-Host ("  真 codex        : " + $real)
Write-Host ("  桥接程序        : " + $stableExe)
Write-Host ("  安装前的原值    : " + $(if ([string]::IsNullOrWhiteSpace($previous)) { '(空)' } else { $previous }))
Write-Host ("  桌面端版本锚点  : " + $(if ($desktopVersion) { $desktopVersion } else { '(读不到，锚点为空)' }))
Write-Host ''
Write-Host '接下来：完全退出 Codex 桌面版（托盘也退掉），再重新打开。'
Write-Host '重开之后桌面版和手机端就会共用同一个 app-server，手机上不再报"正在电脑上用"。'
