param(
    [switch]$RunApp
)

$ErrorActionPreference = 'Stop'

function Ensure-Command {
    param([string]$Name, [string]$InstallCmd)
    if (-not (Get-Command $Name -ErrorAction SilentlyContinue)) {
        throw "$Name no encontrado. $InstallCmd"
    }
}

Write-Host "[1/5] Verificando winget..."
Ensure-Command -Name 'winget' -InstallCmd 'Instala App Installer desde Microsoft Store.'

Write-Host "[2/5] Verificando MSYS2..."
if (-not (Test-Path 'C:\msys64\usr\bin\bash.exe')) {
    Write-Host "Instalando MSYS2..."
    winget install -e --id MSYS2.MSYS2 --accept-package-agreements --accept-source-agreements
}

Write-Host "[3/5] Instalando/actualizando GCC (UCRT64)..."
& 'C:\msys64\usr\bin\bash.exe' -lc "pacman -Sy --noconfirm mingw-w64-ucrt-x86_64-gcc"

Write-Host "[4/5] Configurando entorno actual..."
$gccBin = 'C:\msys64\ucrt64\bin'
if (-not (Test-Path $gccBin)) {
    throw "No existe $gccBin luego de instalar GCC."
}

if (-not (($env:Path -split ';') -contains $gccBin)) {
    $env:Path = "$gccBin;$env:Path"
}
$env:CGO_ENABLED = '1'

Write-Host "[5/5] Verificando toolchain..."
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    if (Test-Path 'C:\Program Files\Go\bin\go.exe') {
        $env:Path = "C:\Program Files\Go\bin;$env:Path"
    }
}
Ensure-Command -Name 'go' -InstallCmd 'Instala Go y reabre la terminal.'

Write-Host "go version: $(go version)"
Write-Host "gcc version:"
gcc --version | Select-Object -First 1
Write-Host "CGO_ENABLED=$env:CGO_ENABLED"

Write-Host "\nListo. En esta terminal ya podés correr:"
Write-Host "  go run ."

if ($RunApp) {
    Write-Host "\nCompilando app..."
    Push-Location $PSScriptRoot
    try {
        Get-Process | Where-Object { $_.ProcessName -in @('go','cgo','gcc','cc1') } | Stop-Process -Force -ErrorAction SilentlyContinue
        go build .
        Write-Host "Lanzando metal-player.exe..."
        Start-Process -FilePath (Join-Path $PSScriptRoot 'metal-player.exe') | Out-Null
        Write-Host "App iniciada (ventana: Metal Player)."
    }
    finally {
        Pop-Location
    }
}
