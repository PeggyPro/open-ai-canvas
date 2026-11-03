[CmdletBinding()]
param()

$ErrorActionPreference = "Stop"

. (Join-Path $PSScriptRoot "windows-proxy.ps1")
Import-CanvasWindowsProxy

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$backendDir = Join-Path $repoRoot "backend"
$webDir = Join-Path $repoRoot "web"
$dataDir = Join-Path $repoRoot ".local\project-workbench-debug"
$adapterDir = Join-Path $backendDir "scripts"
$adapterScript = Join-Path $adapterDir "h3_comfyui_adapter.py"
$adapterDataDir = Join-Path $repoRoot ".local\h3-comfyui-adapter"
$goBuildCache = Join-Path $repoRoot ".local\cache\go-build"
$goModuleCache = Join-Path $repoRoot ".local\cache\go-mod"

$pythonCommand = Get-Command python.exe -ErrorAction SilentlyContinue
if (-not $pythonCommand) { $pythonCommand = Get-Command py.exe -ErrorAction SilentlyContinue }
if (-not $pythonCommand) { throw "未找到 Python 3，无法启动 H3 ComfyUI 适配器。" }
$pythonPath = $pythonCommand.Source
$pythonUsesLauncher = [IO.Path]::GetFileName($pythonPath).ToLowerInvariant() -eq "py.exe"

foreach ($commandName in @("go", "bun")) {
    if (-not (Get-Command $commandName -ErrorAction SilentlyContinue)) {
        throw "未找到 $commandName，请先安装项目要求的运行时。"
    }
}

foreach ($directory in @($dataDir, $goBuildCache, $goModuleCache, $adapterDataDir)) {
    New-Item -ItemType Directory -Force -Path $directory | Out-Null
}

$viteBinary = Join-Path $webDir "node_modules\.bin\vite"
if (-not (Test-Path -LiteralPath $viteBinary)) {
    Write-Host "web/node_modules 不存在，正在执行 bun install --frozen-lockfile..." -ForegroundColor Yellow
    Push-Location $webDir
    try {
        & bun install --frozen-lockfile
        if ($LASTEXITCODE -ne 0) {
            throw "bun install 失败，无法启动前端。"
        }
    } finally {
        Pop-Location
    }
}

function Test-ListeningPort([int]$Port) {
    try {
        return $null -ne (Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction Stop | Select-Object -First 1)
    } catch {
        return $false
    }
}

foreach ($port in @(3000, 8080, 5683)) {
    if (Test-ListeningPort $port) {
        throw "端口 $port 已被占用，请先关闭占用进程后重试。"
    }
}

$powerShellPath = (Get-Command pwsh -ErrorAction SilentlyContinue).Source
if (-not $powerShellPath) {
    $powerShellPath = (Get-Command powershell.exe -ErrorAction SilentlyContinue).Source
}
if (-not $powerShellPath) {
    throw "未找到 PowerShell，无法打开前后端独立窗口。"
}

function ConvertTo-PowerShellLiteral([string]$Value) {
    return "'" + $Value.Replace("'", "''") + "'"
}

$backendDirLiteral = ConvertTo-PowerShellLiteral $backendDir
$webDirLiteral = ConvertTo-PowerShellLiteral $webDir
$dataDirLiteral = ConvertTo-PowerShellLiteral $dataDir
$adapterScriptLiteral = ConvertTo-PowerShellLiteral $adapterScript
$adapterDataDirLiteral = ConvertTo-PowerShellLiteral $adapterDataDir
$pythonPathLiteral = ConvertTo-PowerShellLiteral $pythonPath

$backendCommand = @"
`$ErrorActionPreference = 'Stop'
Set-Location -LiteralPath $backendDirLiteral
`$env:CANVAS_BACKEND_ADDR = '127.0.0.1:8080'
`$env:CANVAS_BACKEND_DATA_DIR = $dataDirLiteral
`$env:GOCACHE = $(ConvertTo-PowerShellLiteral $goBuildCache)
`$env:GOMODCACHE = $(ConvertTo-PowerShellLiteral $goModuleCache)
Write-Host '影策后端：http://127.0.0.1:8080' -ForegroundColor Cyan
go run ./cmd/server
"@

$webCommand = @"
`$ErrorActionPreference = 'Stop'
Set-Location -LiteralPath $webDirLiteral
`$env:VITE_API_PROXY_TARGET = 'http://127.0.0.1:8080'
Write-Host '影策前端：http://localhost:3000' -ForegroundColor Cyan
bun run dev
"@

$adapterCommand = @"
`$ErrorActionPreference = 'Stop'
Set-Location -LiteralPath $adapterDirLiteral
`$env:H3_ADAPTER_DATA_DIR = $adapterDataDirLiteral
`$env:H3_COMFY_NATIVE_URL = 'http://127.0.0.1:8188'
Write-Host 'H3 ComfyUI 适配器：http://127.0.0.1:5683' -ForegroundColor Cyan
if ($pythonUsesLauncher) {
    & $pythonPathLiteral -3 $adapterScriptLiteral
} else {
    & $pythonPathLiteral $adapterScriptLiteral
}
"@

$backendProcess = Start-Process -FilePath $powerShellPath -WindowStyle Normal -WorkingDirectory $backendDir -PassThru -ArgumentList @("-NoLogo", "-NoExit", "-NoProfile", "-Command", $backendCommand)
$adapterProcess = Start-Process -FilePath $powerShellPath -WindowStyle Normal -WorkingDirectory $adapterDir -PassThru -ArgumentList @("-NoLogo", "-NoExit", "-NoProfile", "-Command", $adapterCommand)
$webProcess = Start-Process -FilePath $powerShellPath -WindowStyle Normal -WorkingDirectory $webDir -PassThru -ArgumentList @("-NoLogo", "-NoExit", "-NoProfile", "-Command", $webCommand)

Write-Host "已打开前后端开发窗口。" -ForegroundColor Green
Write-Host "后端窗口 PID: $($backendProcess.Id)；适配器窗口 PID: $($adapterProcess.Id)；前端窗口 PID: $($webProcess.Id)"
Write-Host "访问 http://localhost:3000；适配器 http://127.0.0.1:5683；分别在服务窗口按 Ctrl+C 停止。"


