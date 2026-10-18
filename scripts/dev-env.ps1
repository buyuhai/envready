# 本文件负责：为本地开发准备环境变量。
#
# 用法（在 PowerShell 里，注意前面有一个点，表示"在当前会话中执行"）：
#     . .\scripts\dev-env.ps1
#
# 设计约束：
#   - 只影响当前 PowerShell 会话，不修改系统环境变量、不改注册表、不需要管理员权限；
#   - 工具链装在仓库同级目录 _tools\ 下，不污染系统目录；
#   - 本机的 curl/git 默认走 schannel 会失败，因此这里把 git 的 TLS 后端切到 openssl。

$ErrorActionPreference = 'Stop'

$repoRoot = Split-Path -Parent $PSScriptRoot
$toolsDir = Join-Path (Split-Path -Parent $repoRoot) '_tools'

$goRoot = Join-Path $toolsDir 'go'
if (-not (Test-Path (Join-Path $goRoot 'bin\go.exe'))) {
    throw "未找到 Go 工具链：$goRoot\bin\go.exe。请先把 Go 解压到 $goRoot。"
}

$env:GOROOT = $goRoot
$env:PATH = (Join-Path $goRoot 'bin') + ';' + $env:PATH

$env:GOPATH = Join-Path $toolsDir 'gopath'
$env:GOMODCACHE = Join-Path $env:GOPATH 'pkg\mod'
$env:GOTMPDIR = Join-Path $toolsDir 'gotmp'

# 构建缓存默认落在用户目录（%LOCALAPPDATA%\go-build）。这里改到工具目录下，
# 一是避免污染用户目录，二是在受限环境里用户目录可能不可写。
$env:GOCACHE = Join-Path $toolsDir 'gocache'
New-Item -ItemType Directory -Force -Path $env:GOCACHE, $env:GOTMPDIR, $env:GOPATH | Out-Null

# proxy.golang.org 与 sum.golang.org 在部分网络下不可达，使用国内镜像
$env:GOPROXY = 'https://goproxy.cn,direct'

# 本地安装的 Go 工具（golangci-lint 等）放这里，并加入 PATH
$env:GOBIN = Join-Path $toolsDir 'bin'
$env:PATH = $env:GOBIN + ';' + $env:PATH

# golangci-lint 默认把缓存写到用户目录，这里改到工具目录下（与 GOCACHE 同理）
$env:GOLANGCI_LINT_CACHE = Join-Path $toolsDir 'golangci-cache'
New-Item -ItemType Directory -Force -Path $env:GOBIN, $env:GOLANGCI_LINT_CACHE | Out-Null

# 让 git 使用 openssl 后端（本机 schannel 不可用）
$env:GIT_SSL_BACKEND = 'openssl'

Write-Host "Go  : $(& go version)"
Write-Host "GOPATH: $env:GOPATH"
Write-Host "GOPROXY: $env:GOPROXY"
Write-Host "本会话已就绪（不修改系统环境）。"
