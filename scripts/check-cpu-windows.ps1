#requires -Version 7.2
[CmdletBinding()]
param(
    [ValidateRange(1, 20)][int]$Count = 3,
    [string]$GoBinary = 'go',
    [string]$OutputDirectory = '.tools/cpu-check'
)

$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $false
$repo = Split-Path -Parent $PSScriptRoot
function Get-SourceFingerprint {
    [string[]]$paths = @(& git ls-files --cached --others --exclude-standard -- '*.go' 'go.mod' 'go.sum' 'scripts/check-cpu-windows.ps1')
    if ($LASTEXITCODE -ne 0) { throw 'Could not list source files.' }
    [Array]::Sort($paths, [StringComparer]::Ordinal)
    $entries = foreach ($path in $paths) {
        $hash = (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash
        "$path $hash"
    }
    $algorithm = [Security.Cryptography.SHA256]::Create()
    try {
        $digest = $algorithm.ComputeHash([Text.Encoding]::UTF8.GetBytes(($entries -join "`n")))
        return [BitConverter]::ToString($digest).Replace('-', '').ToLowerInvariant()
    }
    finally { $algorithm.Dispose() }
}
Push-Location $repo
try {
    if (-not $IsWindows) { throw 'This check requires native Windows.' }
    $goCommand = (Get-Command $GoBinary -ErrorAction Stop).Source
    $goVersion = (& $goCommand version | Out-String).Trim()
    if ($LASTEXITCODE -ne 0) { throw 'Could not read Go version.' }
    $goEnvironmentText = (& $goCommand env -json GOOS GOARCH GOVERSION GOFLAGS GOTOOLCHAIN CGO_ENABLED GOEXPERIMENT | Out-String)
    if ($LASTEXITCODE -ne 0) { throw 'Could not read Go environment.' }
    $goEnvironment = $goEnvironmentText | ConvertFrom-Json -ErrorAction Stop
    if ($goEnvironment.GOOS -ne 'windows') { throw 'Go must target native Windows.' }
    $commit = (& git rev-parse HEAD | Out-String).Trim()
    if ($LASTEXITCODE -ne 0) { throw 'Could not read source commit.' }
    $dirty = -not [string]::IsNullOrWhiteSpace((& git status --porcelain | Out-String))
    if ($LASTEXITCODE -ne 0) { throw 'Could not read source status.' }
    $build = Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion'
    $sourceFingerprint = Get-SourceFingerprint
    $runName = (Get-Date -AsUTC -Format 'yyyyMMddTHHmmssZ') + '-' + [guid]::NewGuid().ToString('N')
    $run = Join-Path ([IO.Path]::GetFullPath($OutputDirectory)) $runName
    $null = New-Item -ItemType Directory -Path $run
    $started = [DateTimeOffset]::UtcNow
    $timeout = [Math]::Max(90, $Count * 45)
    $testArgs = @('test', '-json', './internal/supervisor', '-run', '^TestWorkerCPUBudgetContainsDescendantLoad$', '-count', "$Count", '-timeout', "${timeout}s")
    # Retain the original event stream, including failure diagnostics. Never
    # turn a partial run or zero matching tests into an enforcement success.
    $lines = @(& $goCommand @testArgs 2> (Join-Path $run 'stderr.log'))
    $goExit = $LASTEXITCODE
    $utf8 = [Text.UTF8Encoding]::new($false)
    [IO.File]::WriteAllLines((Join-Path $run 'events.jsonl'), [string[]]$lines, $utf8)
    $samples = [Collections.Generic.List[object]]::new()
    $passed = 0
    $invalidEvents = 0
    foreach ($line in $lines) {
        try { $event = $line | ConvertFrom-Json -ErrorAction Stop }
        catch { $invalidEvents++; continue }
        if ($event.Action -eq 'pass' -and $event.Test -match '^TestWorkerCPUBudgetContainsDescendantLoad/(unlimited|cpu_only|combined)$') { $passed++ }
        if ($event.Output -match 'cpu_sample mode=(unlimited|cpu_only|combined) (\{.*\})') {
            $mode = $Matches[1]
            try { $sample = $Matches[2] | ConvertFrom-Json -ErrorAction Stop }
            catch { $invalidEvents++; continue }
            if ($sample.Elapsed -le 0 -or $sample.Before.PID -le 0 -or $sample.Child.PID -le 0 -or
                $sample.Before.PID -ne $sample.After.PID -or $sample.Child.PID -ne $sample.ChildAfter.PID -or
                $sample.Before.PID -eq $sample.Child.PID) { $invalidEvents++; continue }
            $samples.Add([ordered]@{ mode = $mode; result = $sample })
        }
    }
    $complete = $passed -eq (3 * $Count) -and $samples.Count -eq (3 * $Count)
    foreach ($mode in @('unlimited', 'cpu_only', 'combined')) {
        if (@($samples | Where-Object { $_.mode -eq $mode }).Count -ne $Count) { $complete = $false }
    }
    $sourceChanged = $sourceFingerprint -ne (Get-SourceFingerprint)
    $report = [ordered]@{
        schema_version = 1
        started_utc = $started.ToString('o')
        finished_utc = [DateTimeOffset]::UtcNow.ToString('o')
        source_commit = $commit
        source_dirty = $dirty
        source_fingerprint_sha256 = $sourceFingerprint
        source_changed_during_run = $sourceChanged
        go_version = $goVersion
        go_environment = $goEnvironment
        windows_build = "$($build.CurrentBuild).$($build.UBR)"
        processor_count = [Environment]::ProcessorCount
        test_arguments = $testArgs
        repetitions = $Count
        go_exit_code = $goExit
        passed_trials = $passed
        invalid_events = $invalidEvents
        complete = $complete
        passed = ($goExit -eq 0 -and $complete -and $invalidEvents -eq 0 -and -not $sourceChanged)
        trials = @($samples.ToArray())
    }
    $reportPath = Join-Path $run 'summary.json'
    [IO.File]::WriteAllText($reportPath, ($report | ConvertTo-Json -Depth 12), $utf8)
    Write-Output $reportPath
    if (-not $report.passed) { exit 1 }
}
finally { Pop-Location }
