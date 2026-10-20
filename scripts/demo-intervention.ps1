<#
.SYNOPSIS
  Shows LoopGuard's interventions (nudge -> deny -> halt) on a scripted
  stuck session, without touching your real LoopGuard install.

.DESCRIPTION
  Builds loopguard.exe and loopguard-hook.exe into a temporary directory,
  starts a daemon there with its own temporary LoopGuard home in enforce
  mode, and pipes the scripted session in testdata\scenarios\stuck-npm-test.jsonl
  through loopguard-hook.exe, one hook event at a time, as Claude Code would.
  It prints each event and LoopGuard's reply, then 'loopguard status' and
  'loopguard stats' for the temporary home, stops the daemon, and deletes
  the temporary directory.

  Your own install is never involved: LOOPGUARD_HOME is set only in the
  environment of the processes this script starts (never in your shell),
  the hooks never start a daemon of their own (LOOPGUARD_NO_AUTOSTART=1),
  and nothing under %USERPROFILE%\.loopguard or %USERPROFILE%\.claude is
  read or written. The demo daemon takes the first free port above your
  real daemon's.

.PARAMETER Keep
  Keep the temporary directory (binaries, home with loopguard.db, logs) and
  print its path.

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File scripts\demo-intervention.ps1
#>
param([switch]$Keep)

$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
$scenario = Join-Path $repo 'testdata\scenarios\stuck-npm-test.jsonl'
$tmp = Join-Path ([IO.Path]::GetTempPath()) ('loopguard-demo-' + [guid]::NewGuid().ToString('N').Substring(0, 8))
$bin = Join-Path $tmp 'bin'
$lgHome = Join-Path $tmp 'home'
$project = Join-Path $tmp 'shop'
$lg = Join-Path $bin 'loopguard.exe'
$hook = Join-Path $bin 'loopguard-hook.exe'

$realHome = Join-Path $env:USERPROFILE '.loopguard'
if ($lgHome.StartsWith($realHome, [StringComparison]::OrdinalIgnoreCase)) { throw "refusing to use $lgHome as the demo home" }

# Runs an executable with LOOPGUARD_HOME pointing at the demo home, feeding
# it $InputText as UTF-8 (no BOM), and returns its stdout. Only the child's
# environment changes.
function Invoke-Demo([string]$Exe, [string[]]$Arguments, [string]$InputText = '') {
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = $Exe
    $psi.Arguments = ($Arguments | ForEach-Object { if ($_ -match '\s') { '"' + $_ + '"' } else { $_ } }) -join ' '
    $psi.UseShellExecute = $false
    $psi.RedirectStandardInput = $true
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $psi.StandardOutputEncoding = New-Object System.Text.UTF8Encoding($false)
    $psi.StandardErrorEncoding = New-Object System.Text.UTF8Encoding($false)
    $psi.WorkingDirectory = $tmp
    $psi.EnvironmentVariables['LOOPGUARD_HOME'] = $lgHome
    $psi.EnvironmentVariables['LOOPGUARD_NO_AUTOSTART'] = '1'
    $psi.EnvironmentVariables['LOOPGUARD_HOOK_TIMEOUT_MS'] = '5000'
    $psi.EnvironmentVariables.Remove('LOOPGUARD_DISABLE')
    $p = [System.Diagnostics.Process]::Start($psi)
    $bytes = (New-Object System.Text.UTF8Encoding($false)).GetBytes($InputText)
    $p.StandardInput.BaseStream.Write($bytes, 0, $bytes.Length)
    $p.StandardInput.Close()
    $out = $p.StandardOutput.ReadToEnd()
    $err = $p.StandardError.ReadToEnd()
    $p.WaitForExit()
    if ($p.ExitCode -ne 0) { throw "$Exe $($psi.Arguments) exited $($p.ExitCode): $err" }
    return $out
}

function Write-Indented([string]$Text, [string]$Color) {
    foreach ($line in ($Text -split "`n")) { Write-Host ('      ' + $line) -ForegroundColor $Color }
}

# A one-line description of a hook payload: event, tool, and its input.
function Describe-Event($p) {
    $s = $p.hook_event_name
    if ($p.tool_name) {
        $s += ' ' + $p.tool_name
        if ($p.tool_input.command) { $s += ': ' + $p.tool_input.command }
        elseif ($p.tool_input.file_path) { $s += ': ' + (Split-Path -Leaf $p.tool_input.file_path) }
    }
    if ($p.prompt) { $s += ': "' + $p.prompt + '"' }
    if ($p.error) { $s += ' (' + (($p.error -split "`n") | Where-Object { $_ -match 'FAIL|Expected|Received' } | Select-Object -First 1).Trim() + ' ...)' }
    return $s
}

$oldOutputEncoding = [Console]::OutputEncoding
try {
    [Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false)
    New-Item -ItemType Directory -Force -Path $bin, $lgHome, (Join-Path $project 'src') | Out-Null
    Set-Content -Path (Join-Path $lgHome 'config.yaml') -Value "mode: enforce`n" -Encoding ASCII

    Write-Host "Building into $bin ..." -ForegroundColor DarkGray
    Push-Location $repo
    try {
        & go build -o $lg ./cmd/loopguard
        if ($LASTEXITCODE -ne 0) { throw 'go build ./cmd/loopguard failed' }
        & go build -o $hook ./cmd/loopguard-hook
        if ($LASTEXITCODE -ne 0) { throw 'go build ./cmd/loopguard-hook failed' }
    } finally { Pop-Location }

    Write-Host "Demo home (enforce mode): $lgHome" -ForegroundColor DarkGray
    Write-Host "Your install ($realHome) is not touched." -ForegroundColor DarkGray
    Write-Host (Invoke-Demo $lg @('daemon', '--detach')).Trim() -ForegroundColor DarkGray
    Write-Host ''

    $cwdJson = ($project | ConvertTo-Json).Trim('"')
    $n = 0
    foreach ($line in (Get-Content -Path $scenario -Encoding UTF8)) {
        if (-not $line.Trim()) { continue }
        $n++
        $step = $line | ConvertFrom-Json
        # The payload, exactly as in the file, with the project directory.
        $start = $line.IndexOf('"payload":') + 10
        $payload = $line.Substring($start, $line.Length - $start - 1).Replace('<CWD>', $cwdJson)
        $p = $payload | ConvertFrom-Json

        Write-Host ('[{0,2}] {1}' -f $n, $step.note) -ForegroundColor White
        Write-Host ('      -> ' + (Describe-Event $p)) -ForegroundColor DarkGray
        $reply = (Invoke-Demo $hook @('--agent', 'claude-code') $payload).Trim()
        if (-not $reply) {
            Write-Host '      <- (no output: Claude Code carries on)' -ForegroundColor DarkGray
            continue
        }
        $r = $reply | ConvertFrom-Json
        $h = $r.hookSpecificOutput
        if ($r.continue -eq $false) {
            Write-Host '      <- HALT  {"continue": false, ...}  the turn ends; shown to you:' -ForegroundColor Red
            Write-Indented $r.stopReason Red
            Write-Host ('      systemMessage: ' + $r.systemMessage) -ForegroundColor Red
        } elseif ($h.permissionDecision -eq 'deny') {
            Write-Host '      <- DENY  permissionDecision "deny"; the tool call does not run. Reason given to Claude:' -ForegroundColor Yellow
            Write-Indented $h.permissionDecisionReason Yellow
        } elseif ($h.additionalContext) {
            Write-Host ('      <- NUDGE additionalContext on ' + $h.hookEventName + ', next to the tool result:') -ForegroundColor Cyan
            Write-Indented $h.additionalContext Cyan
        } else {
            Write-Host ('      <- ' + $reply) -ForegroundColor Magenta
        }
    }

    Write-Host ''
    Write-Host '--- loopguard status (demo home) ---' -ForegroundColor White
    Write-Host (Invoke-Demo $lg @('status'))
    Write-Host '--- loopguard stats (demo home) ---' -ForegroundColor White
    Write-Host (Invoke-Demo $lg @('stats'))
} finally {
    [Console]::OutputEncoding = $oldOutputEncoding
    if (Test-Path $lg) {
        try { Invoke-Demo $lg @('daemon', 'stop') | Out-Null } catch { Write-Warning "stopping the demo daemon: $_" }
    }
    if ($Keep) {
        Write-Host "Kept $tmp" -ForegroundColor DarkGray
    } elseif (Test-Path $tmp) {
        Start-Sleep -Milliseconds 300 # let the daemon release its files
        Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
        if (Test-Path $tmp) { Write-Warning "could not delete $tmp completely; delete it by hand" }
    }
}
