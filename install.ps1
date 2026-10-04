# Install Supersearch on Windows (macOS and Linux: install.sh).
#   powershell -ExecutionPolicy Bypass -File install.ps1
#   powershell -ExecutionPolicy Bypass -File install.ps1 -Vault "C:\Users\me\Notes"
#   powershell -ExecutionPolicy Bypass -File install.ps1 -Vault "C:\Users\me\Notes" -Yes
#
# 1. picks the vault   2. checks Windows   3. checks (and offers to install) what's needed
# 4. builds and installs the plugin into the vault   5. installs the `supersearch` CLI
# Works in Windows PowerShell 5.1 and PowerShell 7.
param([string]$Vault = "", [switch]$Yes)

$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue" # Invoke-WebRequest is many times faster without the progress bar
Set-Location $PSScriptRoot

function Step($m) { Write-Host ""; Write-Host "==> $m" -ForegroundColor Cyan }
function Ok($m) { Write-Host "  + $m" -ForegroundColor Green }
function Warn($m) { Write-Host "  ! $m" -ForegroundColor Yellow }
function Die($m) { Write-Host "error: $m" -ForegroundColor Red; exit 1 }
function Has($c) { [bool](Get-Command $c -ErrorAction SilentlyContinue) }
function Ask($q, $def) { # -Yes takes the default
	if ($Yes) { return $def -eq "y" }
	$hint = if ($def -eq "y") { "[Y/n]" } else { "[y/N]" }
	$a = Read-Host "$q $hint"
	if ($a -eq "") { return $def -eq "y" }
	return $a -match '^[Yy]'
}
# Native commands don't stop the script on failure: check every exit code.
function Run($what) {
	if ($LASTEXITCODE -ne 0) { Die "$what failed (exit $LASTEXITCODE)" }
}
# Installers change PATH for new processes only: re-read it for this one.
function Update-Path {
	$env:Path = [Environment]::GetEnvironmentVariable("Path", "Machine") + ";" + [Environment]::GetEnvironmentVariable("Path", "User")
}

$Config = Join-Path $env:USERPROFILE ".config\supersearch"

# The server looks in the same places (server/tools.go): PATH, then the usual
# install folders, since installers rarely add themselves to PATH.
function Find-Tool($name) {
	$cmd = Get-Command $name -ErrorAction SilentlyContinue
	if ($cmd) { return $cmd.Source }
	$winget = Join-Path $env:LOCALAPPDATA "Microsoft\WinGet"
	$dirs = @(
		(Join-Path $Config "tools\*"),
		(Join-Path $env:ProgramFiles "Tesseract-OCR"),
		(Join-Path $env:LOCALAPPDATA "Programs\Tesseract-OCR"),
		(Join-Path $winget "Links"),
		(Join-Path $winget "Packages\*\*\Library\bin"),
		(Join-Path $winget "Packages\*\*\bin"),
		(Join-Path $env:USERPROFILE "scoop\shims"),
		(Join-Path $env:ProgramData "chocolatey\bin")
	)
	foreach ($d in $dirs) {
		$hit = Get-ChildItem -Path (Join-Path $d "$name.exe") -ErrorAction SilentlyContinue | Select-Object -First 1
		if ($hit) { return $hit.FullName }
	}
	return $null
}

# Replaces a file that may be running: Windows can't delete a running .exe,
# but it can rename it, so the old copy moves aside and is cleaned up later.
function Copy-Over($src, $dest) {
	Get-ChildItem -Path "$dest.old-*" -ErrorAction SilentlyContinue | Remove-Item -Force -ErrorAction SilentlyContinue
	if (Test-Path $dest) {
		try { Remove-Item $dest -Force }
		catch { Move-Item $dest "$dest.old-$([DateTime]::Now.Ticks)" -Force }
	}
	Copy-Item $src $dest -Force
}

# ---------------------------------------------------------------- 1. Windows
Step "Checking your system"
if ($PSVersionTable.PSVersion.Major -ge 6 -and -not $IsWindows) { Die "this is the Windows installer: on macOS and Linux run ./install.sh" }
$Arch = $env:PROCESSOR_ARCHITECTURE
if ($env:PROCESSOR_ARCHITEW6432) { $Arch = $env:PROCESSOR_ARCHITEW6432 }
$os = [Environment]::OSVersion.Version
Ok "Windows $($os.Major).$($os.Minor) build $($os.Build) ($Arch), PowerShell $($PSVersionTable.PSVersion)"
if (-not [Environment]::Is64BitOperatingSystem) { Die "a 64-bit Windows is needed" }
$HasWinget = Has winget

# ---------------------------------------------------------------- 2. vault
Step "Choosing the vault"
$Vaults = @()
$obsidianJson = Join-Path $env:APPDATA "obsidian\obsidian.json"
if (Test-Path $obsidianJson) {
	try {
		$list = (Get-Content $obsidianJson -Raw | ConvertFrom-Json).vaults
		foreach ($p in $list.PSObject.Properties) {
			$path = $p.Value.path
			if ($path -and (Test-Path (Join-Path $path ".obsidian"))) { $Vaults += $path }
		}
	} catch { }
}
if (-not $Vault) {
	if ($Yes) {
		if ($Vaults.Count -ne 1) { Die "pass the vault path: install.ps1 -Vault C:\path\to\vault" }
		$Vault = $Vaults[0]
	} else {
		if ($Vaults.Count -gt 0) {
			Write-Host "  Your Obsidian vaults:"
			for ($i = 0; $i -lt $Vaults.Count; $i++) { Write-Host "    $($i + 1)) $($Vaults[$i])" }
			$Vault = Read-Host "  Pick a number, or type a path"
		} else {
			$Vault = Read-Host "  Path of your vault (the folder that contains .obsidian)"
		}
		if ($Vault -match '^\d+$') {
			$n = [int]$Vault
			if ($n -lt 1 -or $n -gt $Vaults.Count) { Die "no vault number $n" }
			$Vault = $Vaults[$n - 1]
		}
	}
}
$Vault = $Vault.Trim().Trim('"').TrimEnd('\', '/')
if (-not $Vault) { Die "no vault chosen" }
if (-not (Test-Path (Join-Path $Vault ".obsidian"))) { Die "not an Obsidian vault (no .obsidian folder): $Vault" }
$Vault = (Resolve-Path $Vault).Path
Ok $Vault

# ---------------------------------------------------------------- 3. requirements
Step "Checking requirements"
function Go-Ok {
	if (-not (Has go)) { return $false }
	try {
		$v = ((& go env GOVERSION) -replace '^go', '').Split('.')
		return ([int]$v[0] -gt 1) -or ([int]$v[1] -ge 21) # Go 1.21+ fetches the toolchain go.mod asks for
	} catch { return $true } # a devel build: assume it's new enough
}
function Node-Ok {
	if (-not (Has node) -or -not (Has npm)) { return $false }
	return [int](& node -p "process.versions.node.split('.')[0]") -ge 18
}

$Need = @() # winget package ids
if (Go-Ok) { Ok "go" } else { Warn "Go 1.21+ is missing (builds the server)"; $Need += "GoLang.Go" }
if (Node-Ok) { Ok "node + npm" } else { Warn "Node.js 18+ is missing (builds the plugin)"; $Need += "OpenJS.NodeJS.LTS" }
if (Find-Tool tesseract) { Ok "tesseract" } else { Warn "tesseract is missing (reads text in images)"; $Need += "UB-Mannheim.TesseractOCR" }
if (Find-Tool pdftotext) { Ok "pdftotext" } else { Warn "pdftotext is missing (reads PDFs; part of poppler)"; $Need += "oschwartz10612.Poppler" }
if (Find-Tool whisper-cli) { Ok "whisper-cli" } else { Warn "whisper-cli is missing (transcribes audio; optional)" }
if (Find-Tool antiword) { Ok "antiword" } else { Warn "antiword is missing (old .doc files; optional)" }
if (Has gcc) { Ok "C compiler (faster search index)" } else { Warn "no C compiler: the pure-Go SQLite is used (a bit slower, still fast)" }
if (Has ollama) { Ok "ollama (Ask your vault)" } else { Warn "ollama is missing (only for Ask your vault: https://ollama.com)" }

if ($Need.Count -gt 0) {
	if (-not $HasWinget) {
		Warn "winget is not available: install the missing tools yourself (see the README)"
	} elseif (Ask "  Install $($Need -join ', ') with winget?" "y") {
		foreach ($id in $Need) {
			Write-Host "  installing $id..."
			& winget install --id $id --exact --silent --accept-source-agreements --accept-package-agreements | Out-Host
			if ($LASTEXITCODE -ne 0) { Warn "winget could not install $id (exit $LASTEXITCODE)" }
		}
		Update-Path
	}
}
if (-not (Go-Ok)) { Die "Go 1.21 or newer is needed: https://go.dev/dl (then open a new PowerShell and run this again)" }
if (-not (Node-Ok)) { Die "Node.js 18 or newer is needed: https://nodejs.org (then open a new PowerShell and run this again)" }

# Audio transcription: whisper.cpp (official Windows build), ffmpeg, and a speech model.
$Models = Join-Path $Config "models"
if (-not (Get-ChildItem -Path (Join-Path $Models "ggml-*.bin") -ErrorAction SilentlyContinue)) {
	if (Ask "  Transcribe audio recordings too? Downloads whisper.cpp, ffmpeg and a 142 MB speech model" "n") {
		try {
			[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
			if (-not (Find-Tool whisper-cli)) {
				$rel = Invoke-RestMethod "https://api.github.com/repos/ggml-org/whisper.cpp/releases/latest" -Headers @{ "User-Agent" = "supersearch-installer" }
				$asset = $rel.assets | Where-Object { $_.name -eq "whisper-bin-x64.zip" } | Select-Object -First 1
				if (-not $asset) { throw "no whisper-bin-x64.zip in the latest whisper.cpp release" }
				$zip = Join-Path $env:TEMP "whisper-bin-x64.zip"
				Invoke-WebRequest $asset.browser_download_url -OutFile $zip
				$unpack = Join-Path $env:TEMP "whisper-unpack"
				if (Test-Path $unpack) { Remove-Item $unpack -Recurse -Force }
				Expand-Archive $zip $unpack -Force
				$exe = Get-ChildItem $unpack -Recurse -Filter "whisper-cli.exe" | Select-Object -First 1
				if (-not $exe) { throw "whisper-cli.exe not found in the download" }
				$dest = Join-Path $Config "tools\whisper"
				New-Item -ItemType Directory -Force $dest | Out-Null
				Copy-Item (Join-Path $exe.DirectoryName "*") $dest -Recurse -Force # with its DLLs
				Remove-Item $zip, $unpack -Recurse -Force
				Ok "whisper.cpp $($rel.tag_name)"
			}
			if (-not (Find-Tool ffmpeg)) {
				if ($HasWinget) {
					& winget install --id Gyan.FFmpeg --exact --silent --accept-source-agreements --accept-package-agreements | Out-Host
				} else { Warn "install ffmpeg yourself: https://ffmpeg.org/download.html" }
			}
			New-Item -ItemType Directory -Force $Models | Out-Null
			Write-Host "  downloading the speech model (142 MB)..."
			Invoke-WebRequest "https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-base.bin" -OutFile (Join-Path $Models "ggml-base.bin.part")
			Move-Item (Join-Path $Models "ggml-base.bin.part") (Join-Path $Models "ggml-base.bin") -Force
			Ok "speech model saved"
		} catch {
			Warn "audio setup failed: $($_.Exception.Message)"
		}
	}
}

# ---------------------------------------------------------------- 4. build + plugin
Step "Building"
if (-not $env:GOTOOLCHAIN) { $env:GOTOOLCHAIN = "auto" } # let Go fetch the version go.mod asks for
Push-Location server
try {
	$built = $false
	if (Has gcc) { # the C SQLite is about 2x faster per query
		$env:CGO_ENABLED = "1"
		& go build -tags sqlite_fts5 -trimpath "-ldflags=-s -w" -o supersearch-server.exe .
		$built = $LASTEXITCODE -eq 0
		if (-not $built) { Warn "C build failed; using the pure-Go SQLite" }
	}
	if (-not $built) {
		$env:CGO_ENABLED = "0"
		& go build -trimpath "-ldflags=-s -w" -o supersearch-server.exe .
		Run "go build"
	}
} finally { Pop-Location }
Ok "server"
Push-Location plugin
try {
	& npm install --silent --no-audit --no-fund
	Run "npm install"
	& npm run build --silent
	Run "plugin build"
} finally { Pop-Location }
Ok "plugin"

Step "Installing the plugin into the vault"
$Dest = Join-Path $Vault ".obsidian\plugins\supersearch"
New-Item -ItemType Directory -Force $Dest | Out-Null
Copy-Over "server\supersearch-server.exe" (Join-Path $Dest "supersearch-server.exe")
foreach ($f in "main.js", "manifest.json", "styles.css") { Copy-Item (Join-Path "plugin" $f) $Dest -Force }
Ok $Dest

# ---------------------------------------------------------------- 5. CLI
Step "Installing the supersearch command"
$Bin = Join-Path $env:LOCALAPPDATA "Programs\supersearch"
New-Item -ItemType Directory -Force $Bin | Out-Null
$Cli = Join-Path $Bin "supersearch.exe"
Copy-Over "server\supersearch-server.exe" $Cli
& $Cli vault $Vault
Run "supersearch vault"
$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if (-not $userPath) { $userPath = "" }
if (($userPath.Split(';') | ForEach-Object { $_.TrimEnd('\') }) -notcontains $Bin) {
	[Environment]::SetEnvironmentVariable("Path", ($userPath.TrimEnd(';') + ";" + $Bin).TrimStart(';'), "User")
	Ok "$Cli (added to your PATH: open a new terminal to use it)"
} else {
	Ok $Cli
}

# ---------------------------------------------------------------- done
Step "Done"
Write-Host "  In Obsidian: Settings -> Community plugins -> turn on Supersearch"
Write-Host "  (already on? toggle it off and on to load the new version)."
Write-Host "  Restart Obsidian if tools were just installed, so it sees them."
Write-Host ""
Write-Host "  To search from your phone, tablet or another computer, run:  supersearch"
Write-Host "  It prints the URL and token to paste into the plugin's settings there."
