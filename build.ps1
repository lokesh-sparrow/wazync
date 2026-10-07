# Builds the Claude Desktop extension: dist\wazync-<version>.mcpb
# Needs Go 1.26 or newer.
$ErrorActionPreference = "Stop"
$root = $PSScriptRoot
$version = (Get-Content "$root\extension\manifest.json" -Raw | ConvertFrom-Json).version
$stage = "$root\dist\stage"

Remove-Item $stage -Recurse -Force -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force "$stage\bridge", "$stage\server" | Out-Null

$env:CGO_ENABLED = "0"
Push-Location "$root\bridge"
try {
    go build -trimpath -buildvcs=false -ldflags "-s -w -buildid= -H=windowsgui -X main.version=$version" -o "$stage\bridge\wazync-bridge.exe" .
    if ($LASTEXITCODE -ne 0) { throw "go build failed" }
} finally { Pop-Location }

Copy-Item "$root\extension\manifest.json", "$root\extension\icon.png", "$root\LICENSE" $stage
Copy-Item "$root\server\index.js" "$stage\server"

$out = "$root\dist\wazync-$version.mcpb"
Remove-Item $out -ErrorAction SilentlyContinue
Add-Type -AssemblyName System.IO.Compression.FileSystem
[IO.Compression.ZipFile]::CreateFromDirectory($stage, $out)
Remove-Item $stage -Recurse -Force
Write-Host "Built $out"
