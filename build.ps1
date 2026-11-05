# Builds release files into .\dist:
#   devpanel.exe                     the app
#   DevPanel-Setup-<ver>.exe         installer (same binary; the "Setup" name switches it to install mode)
#   DevPanel-<ver>-portable.zip      portable: unzip anywhere and run devpanel.exe
#   SHA256SUMS.txt
# Requires Go 1.26+ on PATH. Usage: .\build.ps1   (add -SkipTests to skip the unit tests)
param([switch]$SkipTests)
$ErrorActionPreference = 'Stop'
Set-Location $PSScriptRoot

$version = (Select-String -Path setup.go -Pattern 'const Version = "([^"]+)"').Matches[0].Groups[1].Value
Write-Host "DevPanel $version"

if (-not $SkipTests) { go test -tags desktop,production . ; if ($LASTEXITCODE) { throw 'tests failed' } }

New-Item -ItemType Directory -Force dist | Out-Null
go build -tags desktop,production -trimpath -ldflags "-w -s -H windowsgui" -o dist\devpanel.exe .
if ($LASTEXITCODE) { throw 'build failed' }

Copy-Item dist\devpanel.exe "dist\DevPanel-Setup-$version.exe" -Force

$portable = "dist\DevPanel-$version-portable"
Remove-Item $portable -Recurse -Force -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force $portable | Out-Null
Copy-Item dist\devpanel.exe, README.md, LICENSE, THIRD_PARTY_NOTICES.md $portable
Compress-Archive -Path "$portable\*" -DestinationPath "dist\DevPanel-$version-portable.zip" -Force
Remove-Item $portable -Recurse -Force

Get-ChildItem dist -File -Include *.exe, *.zip -Recurse |
  Where-Object Name -ne 'devpanel.exe' |
  ForEach-Object { "{0}  {1}" -f (Get-FileHash $_.FullName -Algorithm SHA256).Hash.ToLower(), $_.Name } |
  Set-Content dist\SHA256SUMS.txt
Get-Content dist\SHA256SUMS.txt
