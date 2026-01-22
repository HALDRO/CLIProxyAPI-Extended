#!/usr/bin/env pwsh
# Quick start script for CLIProxyAPI server

[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
[Console]::InputEncoding = [System.Text.Encoding]::UTF8
$OutputEncoding = [System.Text.Encoding]::UTF8
$PSDefaultParameterValues['*:Encoding'] = 'utf8'
chcp 65001 | Out-Null

# Read port and API key from config
$config = if (Test-Path "config.yaml") { Get-Content "config.yaml" -Raw } else { "" }
$port = if ($config -match "port:\s*(\d+)") { [int]$matches[1] } else { 11434 }
$apiKey = if ($config -match "api-keys:\s*\n\s*-\s*""([^""]+)""") { $matches[1] } else { "123456" }
$exeName = "server.exe"

# Release port helper
function Release-Port {
    param([int]$PortNumber)
    $procIds = netstat -ano | Select-String ":$PortNumber\s" | ForEach-Object {
        ($_ -split '\s+')[-1]
    } | Where-Object { $_ -match '^\d+$' -and $_ -ne '0' } | Select-Object -Unique
    
    if ($procIds) {
        Write-Host "Releasing port $PortNumber..." -ForegroundColor Yellow
        foreach ($p in $procIds) {
            Stop-Process -Id $p -Force -ErrorAction SilentlyContinue
        }
        Start-Sleep -Milliseconds 500
    }
}

# Build
Write-Host "Building server..." -ForegroundColor Cyan
$buildResult = go build -o $exeName ./cmd/server 2>&1
if ($LASTEXITCODE -ne 0) {
    Write-Host "Build failed:" -ForegroundColor Red
    Write-Host $buildResult
    exit 1
}
Write-Host "Build successful" -ForegroundColor Green

# Release port
Release-Port -PortNumber $port

# Start exe in background
Write-Host "Starting server..." -ForegroundColor Cyan
$process = Start-Process -FilePath ".\$exeName" -PassThru -NoNewWindow

# Wait for server and fetch models
Start-Sleep -Seconds 3

# On startup, clients may load asynchronously, so /v1/models can temporarily return
# an incomplete list. Wait for a stable result.
$lastIds = @()
$stableHits = 0
for ($i = 0; $i -lt 20; $i++) {
    try {
        $json = (Invoke-WebRequest -Uri "http://localhost:$port/v1/models" -Headers @{"Authorization"="Bearer $apiKey"}).Content | ConvertFrom-Json
        $ids = @($json.data.id)

        if ($ids.Count -gt 0 -and $lastIds.Count -gt 0 -and ($ids -join "\n") -eq ($lastIds -join "\n")) {
            $stableHits++
        } else {
            $stableHits = 0
        }

        $lastIds = $ids

        # Consider the list stable after 2 identical responses in a row.
        if ($ids.Count -gt 0 -and $stableHits -ge 1) {
            Write-Host "`nAvailable models:" -ForegroundColor Green
            $ids | ForEach-Object { Write-Host $_ -ForegroundColor Yellow }
            Write-Host ""
            break
        }
    } catch {
        if ($i -eq 19) { Write-Host "Failed to fetch models" -ForegroundColor Red }
    }
    Start-Sleep -Seconds 1
}

# Wait for process exit
try {
    $process | Wait-Process
} finally {
    if (!$process.HasExited) {
        Stop-Process -Id $process.Id -Force -ErrorAction SilentlyContinue
    }
    Release-Port -PortNumber $port
}
