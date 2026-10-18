# 本文件负责：构建两个可执行文件，并把版本信息注入进去。
#
# 用法（在仓库根目录）：
#     .\scripts\build.ps1                 # 版本取 dev
#     .\scripts\build.ps1 -Version v0.1.0 # 指定版本
#
# 产物：bin\envready.exe（启动器）与 bin\envready-gui.exe（主程序）
#
# 说明：版本、提交号、构建时间通过链接器 -X 注入 internal/buildinfo，
# 因此无需改动源码即可让 --version 与日志显示正确信息（发版流程在 S4/S5 接入）。

param(
    [string]$Version = 'dev'
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
Set-Location $repoRoot

# ---- 找到 Go：优先用 PATH 里的，其次用仓库同级 _tools\go（本项目约定的本地工具链位置）----
$goExe = $null
$cmd = Get-Command go -ErrorAction SilentlyContinue
if ($cmd) {
    $goExe = $cmd.Source
} else {
    $localGo = Join-Path (Split-Path -Parent $repoRoot) '_tools\go\bin\go.exe'
    if (Test-Path $localGo) {
        $goExe = $localGo
        $env:PATH = (Split-Path -Parent $localGo) + ';' + $env:PATH
        Write-Host "使用本地 Go 工具链：$localGo"
    }
}
if (-not $goExe) {
    throw '未找到 Go。请把 Go 解压到仓库同级目录 _tools\go，或把它加入 PATH（可用 scripts\dev-shell.cmd 打开已配置的窗口）。'
}

# ---- 构建缓存：默认落在用户目录；若该目录不可写（受限环境），退到工具目录 ----
if (-not $env:GOCACHE) {
    $defaultCache = (& $goExe env GOCACHE 2>$null)
    $usable = $false
    if ($defaultCache) {
        try {
            $probe = Join-Path $defaultCache ('probe-' + [guid]::NewGuid().ToString('N'))
            New-Item -ItemType Directory -Path $probe -Force -ErrorAction Stop | Out-Null
            Remove-Item $probe -Recurse -Force -ErrorAction SilentlyContinue
            $usable = $true
        } catch {
            $usable = $false
        }
    }
    if (-not $usable) {
        $env:GOCACHE = Join-Path (Split-Path -Parent $repoRoot) '_tools\gocache'
        New-Item -ItemType Directory -Force -Path $env:GOCACHE | Out-Null
        Write-Host "默认构建缓存不可写，改用：$env:GOCACHE"
    }
}

# 提交号取不到时用 none，仍然是可读的
$commit = 'none'
try { $commit = (& git rev-parse --short HEAD 2>$null) } catch { }
if (-not $commit) { $commit = 'none' }

$buildDate = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')

$ldflags = @(
    "-X github.com/buyuhai/envready/internal/buildinfo.Version=$Version",
    "-X github.com/buyuhai/envready/internal/buildinfo.Commit=$commit",
    "-X github.com/buyuhai/envready/internal/buildinfo.BuildDate=$buildDate"
) -join ' '

New-Item -ItemType Directory -Force -Path (Join-Path $repoRoot 'bin') | Out-Null

Write-Host "构建版本：$Version  提交：$commit  时间：$buildDate"
& $goExe build -trimpath -ldflags $ldflags -o bin/ ./cmd/...
if ($LASTEXITCODE -ne 0) { throw "构建失败（exit $LASTEXITCODE）" }

Get-ChildItem (Join-Path $repoRoot 'bin') | Select-Object Name, Length | Format-Table -AutoSize
Write-Host '构建完成。'
