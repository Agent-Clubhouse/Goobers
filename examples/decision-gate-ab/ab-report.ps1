<#
.SYNOPSIS
  Compare a gate-off arm and a gate-on arm side by side.

.DESCRIPTION
  Scans both instance journals and daemon logs, then prints a reliability report
  for a paired decision-gate experiment. The report always prints counts and
  sample sizes. It prints percentages only when the denominator meets
  -MinSample (default 30); below that it refuses to print a percentage.

.PARAMETER GateOffInstance
  Path to the gate-off instance root.

.PARAMETER GateOffDaemonLogs
  One or more daemon log files for the gate-off arm. Comma-separated entries
  are accepted.

.PARAMETER GateOnInstance
  Path to the gate-on instance root.

.PARAMETER GateOnDaemonLogs
  One or more daemon log files for the gate-on arm. Comma-separated entries are
  accepted.

.PARAMETER Since
  Only include journal events and decision-gate log records at or after this
  UTC/local timestamp. Records without a parseable timestamp are kept.

.PARAMETER MinSample
  Minimum denominator required before the script prints a percentage.

.PARAMETER Json
  Optional path to write the structured report as JSON.
#>
param(
  [Parameter(Mandatory)][string]$GateOffInstance,
  [string[]]$GateOffDaemonLogs = @(),
  [Parameter(Mandatory)][string]$GateOnInstance,
  [string[]]$GateOnDaemonLogs = @(),
  [datetime]$Since = [datetime]::MinValue,
  [int]$MinSample = 30,
  [string]$Json
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Expand-LogPaths {
  param([string[]]$Paths)

  @($Paths | ForEach-Object { $_ -split ',' } | Where-Object { $_ })
}

function Read-LinesShared {
  param([string]$Path)

  if (-not (Test-Path -LiteralPath $Path)) {
    return @()
  }
  $fs = [System.IO.File]::Open($Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
  $sr = [System.IO.StreamReader]::new($fs)
  try {
    $lines = New-Object System.Collections.Generic.List[string]
    while (-not $sr.EndOfStream) {
      $lines.Add($sr.ReadLine())
    }
    return $lines
  } finally {
    $sr.Dispose()
    $fs.Dispose()
  }
}

function ConvertTo-NullableDateTime {
  param([string]$Value)

  if ([string]::IsNullOrWhiteSpace($Value)) {
    return $null
  }
  $parsed = [datetime]::MinValue
  if ([datetime]::TryParse($Value, [ref]$parsed)) {
    return $parsed
  }
  return $null
}

function Parse-LogFields {
  param([string]$Line)

  $fields = @{}
  foreach ($match in [regex]::Matches($Line, '(\w+)=("(?:[^"\\]|\\.)*"|\S+)')) {
    $value = $match.Groups[2].Value
    if ($value.Length -ge 2 -and $value.StartsWith('"') -and $value.EndsWith('"')) {
      $value = $value.Substring(1, $value.Length - 2)
    }
    $fields[$match.Groups[1].Value] = $value
  }
  $fields
}

function Resolve-KnownValidity {
  param([System.Collections.IDictionary]$Fields)

  if ($Fields.ContainsKey('inputKnown')) {
    switch -Regex ($Fields.inputKnown) {
      '^(?i:true)$' { return $true }
      '^(?i:false)$' { return $false }
    }
  }
  if ($Fields.ContainsKey('inputValid')) {
    return $true
  }
  return $false
}

function Is-TrueString {
  param([string]$Value)

  $Value -match '^(?i:true)$'
}

function Format-CountWithRate {
  param(
    [int]$Count,
    [int]$Sample,
    [int]$MinimumSample
  )

  if ($Sample -le 0) {
    return '0 (n=0)'
  }
  if ($Sample -lt $MinimumSample) {
    return "$Count (n=$Sample)"
  }
  $rate = [math]::Round((100.0 * $Count) / $Sample, 1)
  return "$Count/$Sample ($rate%)"
}

function New-OrderedCountMap {
  param([System.Collections.IEnumerable]$Values)

  $ordered = [ordered]@{}
  foreach ($group in ($Values | Group-Object | Sort-Object Name)) {
    $ordered[[string]$group.Name] = [int]$group.Count
  }
  $ordered
}

function Join-CountMap {
  param([System.Collections.IDictionary]$Map)

  if (-not $Map.Count) {
    return 'none'
  }
  @($Map.GetEnumerator() | Sort-Object Name | ForEach-Object { "$($_.Name)=$($_.Value)" }) -join ', '
}

function Get-MapCount {
  param(
    [System.Collections.IDictionary]$Map,
    [string]$Key
  )

  if ($null -eq $Map -or -not $Map.Contains($Key)) {
    return 0
  }
  [int]$Map[$Key]
}

function Resolve-InstanceJournalPath {
  param([string]$Instance)

  $scheduler = Join-Path $Instance 'scheduler'
  $pointer = Join-Path $scheduler 'events.jsonl.current'
  if (-not (Test-Path -LiteralPath $pointer)) {
    return Join-Path $scheduler 'events.jsonl'
  }
  $generation = (Get-Content -LiteralPath $pointer -Raw).Trim()
  if ($generation -notmatch '^\d+$') {
    throw "invalid instance journal generation '$generation' in $pointer"
  }
  return Join-Path $scheduler ("events.jsonl.gen-{0:D6}" -f [int]$generation)
}

function Read-ArmSummary {
  param(
    [string]$Name,
    [string]$Instance,
    [string[]]$DaemonLogs,
    [datetime]$SinceValue
  )

  $journalPaths = @(Get-ChildItem -LiteralPath (Join-Path $Instance 'gaggles') -Filter events.jsonl -Recurse -File -ErrorAction SilentlyContinue | ForEach-Object { $_.FullName })
  $stageRecords = New-Object System.Collections.Generic.List[object]
  $runPhases = New-Object System.Collections.Generic.List[string]
  $runTerminals = New-Object System.Collections.Generic.List[object]
  $failureCodes = New-Object System.Collections.Generic.List[string]

  foreach ($journalPath in $journalPaths) {
    $runID = Split-Path -Path (Split-Path -Path $journalPath -Parent) -Leaf
    $runDir = Split-Path -Path $journalPath -Parent
    $itemID = ''
    $itemPath = Join-Path $runDir 'inputs/item'
    if (Test-Path -LiteralPath $itemPath) {
      $item = Get-Content -LiteralPath $itemPath -Raw | ConvertFrom-Json
      if ($item.id) {
        $itemID = [string]$item.id
      }
    }
    $errorByAttempt = @{}
    foreach ($line in (Read-LinesShared -Path $journalPath)) {
      if ([string]::IsNullOrWhiteSpace($line)) {
        continue
      }
      if ($line -notmatch '"type":"(stage\.finished|run\.finished|error)"') {
        continue
      }
      $event = $line | ConvertFrom-Json
      $eventTime = ConvertTo-NullableDateTime -Value ([string]$event.time)

      if ($event.type -eq 'error' -and $event.stage -and $event.attempt -and $event.error.code) {
        $errorByAttempt["$($event.stage)|$($event.attempt)"] = [string]$event.error.code
        continue
      }
      if ($eventTime -and $eventTime -lt $SinceValue) {
        continue
      }
      switch ($event.type) {
        'stage.finished' {
          $key = "$($event.stage)|$($event.attempt)"
          $code = ''
          if ($errorByAttempt.ContainsKey($key)) {
            $code = $errorByAttempt[$key]
          }
          $stageRecords.Add([pscustomobject]@{
              RunID   = $runID
              Stage   = [string]$event.stage
              Attempt = [int]$event.attempt
              Status  = [string]$event.status
              Code    = $code
            })
          if ($code) {
            $failureCodes.Add($code)
          }
        }
        'run.finished' {
          if ($event.status) {
            $runPhases.Add([string]$event.status)
            if ($itemID) {
              $runTerminals.Add([pscustomobject]@{
                  RunID = $runID
                  ItemID = $itemID
                  RepositoryKey = ''
                  Status = [string]$event.status
                  Time = $eventTime
                  Matched = $false
                })
            }
          }
        }
      }
    }
  }

  $intakeRecords = New-Object System.Collections.Generic.List[object]
  $instanceJournalPath = Resolve-InstanceJournalPath -Instance $Instance
  $instanceEvents = @((Read-LinesShared -Path $instanceJournalPath) | Where-Object {
      -not [string]::IsNullOrWhiteSpace($_) -and $_ -match '"type":"runner\.annotation"'
    } | ForEach-Object { $_ | ConvertFrom-Json })
  $runRepositoryKeys = @{}
  foreach ($event in $instanceEvents) {
    if ($event.runner -and $event.runner.annotation -eq 'item-repo' -and
        $event.PSObject.Properties.Name -contains 'runId' -and $event.runId -and
        $event.runner.PSObject.Properties.Name -contains 'itemId' -and $event.runner.itemId -and
        $event.runner.PSObject.Properties.Name -contains 'repositoryKey' -and $event.runner.repositoryKey) {
      $runRepositoryKeys["$($event.runId)|$($event.runner.itemId)"] = [string]$event.runner.repositoryKey
    }
  }
  foreach ($terminal in $runTerminals) {
    $key = "$($terminal.RunID)|$($terminal.ItemID)"
    if ($runRepositoryKeys.ContainsKey($key)) {
      $terminal.RepositoryKey = $runRepositoryKeys[$key]
    }
  }
  $intakeEvents = @($instanceEvents | Where-Object {
      $_.runner -and $_.runner.annotation -eq 'backlog.intake-decision-shadow' -and
      $_.runner.PSObject.Properties.Name -contains 'repositoryKey' -and $_.runner.repositoryKey
    } | Sort-Object @{ Expression = { ConvertTo-NullableDateTime -Value ([string]$_.time) } })
  foreach ($event in $intakeEvents) {
    $eventTime = ConvertTo-NullableDateTime -Value ([string]$event.time)
    if ($eventTime -and $eventTime -lt $SinceValue) {
      continue
    }
    $terminal = @($runTerminals | Where-Object {
        -not $_.Matched -and
        $_.RepositoryKey -eq [string]$event.runner.repositoryKey -and
        $_.ItemID -eq [string]$event.runner.itemId -and
        $eventTime -and $_.Time -and $_.Time -ge $eventTime
      } | Sort-Object Time | Select-Object -First 1)
    if ($terminal.Count -eq 0) {
      continue
    }
    $terminal[0].Matched = $true
    $verdict = [string]$event.runner.verdict
    $errored = $event.runner.error -eq $true
    $uncertain = $errored -or $verdict -notin @('yes', 'no')
    $intakeRecords.Add([pscustomobject]@{
        ItemID = [string]$event.runner.itemId
        RepositoryKey = [string]$event.runner.repositoryKey
        Flagged = -not $uncertain -and $event.runner.flagged -eq $true
        Uncertain = $uncertain
        LaterEscalated = $terminal[0].Status -eq 'escalated'
      })
  }

  $decisionRecords = New-Object System.Collections.Generic.List[object]
  foreach ($logPath in (Expand-LogPaths -Paths $DaemonLogs)) {
    foreach ($line in (Read-LinesShared -Path $logPath)) {
      if ($line -notmatch 'decisiongate\.(shadow|enforce)') {
        continue
      }
      $fields = Parse-LogFields -Line $line
      $recordTime = $null
      if ($fields.ContainsKey('time')) {
        $recordTime = ConvertTo-NullableDateTime -Value $fields.time
      }
      if ($recordTime -and $recordTime -lt $SinceValue) {
        continue
      }
      $decisionRecords.Add([pscustomobject]@{
          Mode            = if ($line -match 'decisiongate\.enforce') { 'enforce' } else { 'shadow' }
          Outcome         = [string]($fields['outcome'])
          Verdict         = [string]($fields['verdict'])
          AgentClaimedBad = Is-TrueString -Value ([string]($fields['agentClaimedBad']))
          InputKnown      = Resolve-KnownValidity -Fields $fields
          InputValid      = Is-TrueString -Value ([string]($fields['inputValid']))
          Error           = [string]($fields['error'])
        })
    }
  }

  $stageStatusCounts = New-OrderedCountMap -Values ($stageRecords | ForEach-Object { $_.Status })
  $runPhaseCounts = New-OrderedCountMap -Values $runPhases
  $failureCodeCounts = New-OrderedCountMap -Values ($failureCodes | Where-Object { $_ })
  $decisionModeCounts = New-OrderedCountMap -Values ($decisionRecords | ForEach-Object { $_.Mode })
  $decisionVerdictCounts = New-OrderedCountMap -Values ($decisionRecords | Where-Object { $_.Verdict } | ForEach-Object { $_.Verdict })

  $knownValid = @($decisionRecords | Where-Object { $_.InputKnown -and $_.InputValid })
  $knownInvalid = @($decisionRecords | Where-Object { $_.InputKnown -and -not $_.InputValid })
  $unknownValidity = @($decisionRecords | Where-Object { -not $_.InputKnown })
  $validClaimedBad = @($decisionRecords | Where-Object { $_.InputKnown -and $_.InputValid -and $_.AgentClaimedBad })
  $validClaimedBadSuccess = @($validClaimedBad | Where-Object { $_.Outcome -eq 'success' })
  $validClaimedBadNonSuccess = @($validClaimedBad | Where-Object { $_.Outcome -eq 'non-success' })
  $validClaimedBadUnknownOutcome = @($validClaimedBad | Where-Object { $_.Outcome -ne 'success' -and $_.Outcome -ne 'non-success' })
  $modelErrors = @($decisionRecords | Where-Object { $_.Error -and $_.Error -notin @('', '<nil>') })
  $intakeCertain = @($intakeRecords | Where-Object { -not $_.Uncertain })
  $intakeFlagged = @($intakeCertain | Where-Object { $_.Flagged })
  $intakeUnflagged = @($intakeCertain | Where-Object { -not $_.Flagged })

  [pscustomobject]@{
    name = $Name
    journals = [pscustomobject]@{
      runsScanned = $journalPaths.Count
      stageFinishes = $stageRecords.Count
      byStatus = $stageStatusCounts
      terminalRuns = $runPhases.Count
      terminalByPhase = $runPhaseCounts
      failureCodes = $failureCodeCounts
    }
    decisionGate = [pscustomobject]@{
      records = $decisionRecords.Count
      byMode = $decisionModeCounts
      byVerdict = $decisionVerdictCounts
      knownValidSamples = $knownValid.Count
      knownInvalidSamples = $knownInvalid.Count
      unknownValiditySamples = $unknownValidity.Count
      validHandoffCalledBad = $validClaimedBad.Count
      validHandoffCalledBadSuccess = $validClaimedBadSuccess.Count
      validHandoffCalledBadNonSuccess = $validClaimedBadNonSuccess.Count
      validHandoffCalledBadUnknownOutcome = $validClaimedBadUnknownOutcome.Count
      modelErrors = $modelErrors.Count
    }
    backlogIntakeShadow = [pscustomobject]@{
      sampled = $intakeRecords.Count
      certain = $intakeCertain.Count
      flagged = $intakeFlagged.Count
      flaggedLaterEscalated = @($intakeFlagged | Where-Object { $_.LaterEscalated }).Count
      unflagged = $intakeUnflagged.Count
      unflaggedLaterEscalated = @($intakeUnflagged | Where-Object { $_.LaterEscalated }).Count
      uncertainOrError = @($intakeRecords | Where-Object { $_.Uncertain }).Count
    }
  }
}

function Write-ComparisonRow {
  param(
    [string]$Metric,
    [string]$GateOff,
    [string]$GateOn
  )

  '{0,-46} {1,-28} {2,-28}' -f $Metric, $GateOff, $GateOn
}

$gateOff = Read-ArmSummary -Name 'gate-off' -Instance $GateOffInstance -DaemonLogs $GateOffDaemonLogs -SinceValue $Since
$gateOn = Read-ArmSummary -Name 'gate-on' -Instance $GateOnInstance -DaemonLogs $GateOnDaemonLogs -SinceValue $Since

$report = [pscustomobject]@{
  since = if ($Since -eq [datetime]::MinValue) { $null } else { $Since.ToString('o') }
  minSample = $MinSample
  arms = [pscustomobject]@{
    gateOff = $gateOff
    gateOn = $gateOn
  }
}

if ($Json) {
  $report | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $Json
}

Write-Output 'Decision-gate A/B reliability report'
if ($Since -ne [datetime]::MinValue) {
  Write-Output ("Since: {0}" -f $Since.ToString('o'))
}
Write-Output ("Percentages require at least n={0}" -f $MinSample)
Write-Output ''
Write-Output ('{0,-46} {1,-28} {2,-28}' -f 'Metric', 'gate-off', 'gate-on')
Write-Output ('{0,-46} {1,-28} {2,-28}' -f ('-' * 46), ('-' * 28), ('-' * 28))
Write-Output (Write-ComparisonRow -Metric 'Runs scanned' -GateOff ([string]$gateOff.journals.runsScanned) -GateOn ([string]$gateOn.journals.runsScanned))
Write-Output (Write-ComparisonRow -Metric 'Stage finishes' -GateOff ([string]$gateOff.journals.stageFinishes) -GateOn ([string]$gateOn.journals.stageFinishes))
Write-Output (Write-ComparisonRow -Metric 'Stage success' -GateOff (Format-CountWithRate -Count (Get-MapCount -Map $gateOff.journals.byStatus -Key 'success') -Sample $gateOff.journals.stageFinishes -MinimumSample $MinSample) -GateOn (Format-CountWithRate -Count (Get-MapCount -Map $gateOn.journals.byStatus -Key 'success') -Sample $gateOn.journals.stageFinishes -MinimumSample $MinSample))
Write-Output (Write-ComparisonRow -Metric 'Stage blocked' -GateOff (Format-CountWithRate -Count (Get-MapCount -Map $gateOff.journals.byStatus -Key 'blocked') -Sample $gateOff.journals.stageFinishes -MinimumSample $MinSample) -GateOn (Format-CountWithRate -Count (Get-MapCount -Map $gateOn.journals.byStatus -Key 'blocked') -Sample $gateOn.journals.stageFinishes -MinimumSample $MinSample))
Write-Output (Write-ComparisonRow -Metric 'Stage failure' -GateOff (Format-CountWithRate -Count (Get-MapCount -Map $gateOff.journals.byStatus -Key 'failure') -Sample $gateOff.journals.stageFinishes -MinimumSample $MinSample) -GateOn (Format-CountWithRate -Count (Get-MapCount -Map $gateOn.journals.byStatus -Key 'failure') -Sample $gateOn.journals.stageFinishes -MinimumSample $MinSample))
Write-Output (Write-ComparisonRow -Metric 'Terminal escalated runs' -GateOff (Format-CountWithRate -Count (Get-MapCount -Map $gateOff.journals.terminalByPhase -Key 'escalated') -Sample $gateOff.journals.terminalRuns -MinimumSample $MinSample) -GateOn (Format-CountWithRate -Count (Get-MapCount -Map $gateOn.journals.terminalByPhase -Key 'escalated') -Sample $gateOn.journals.terminalRuns -MinimumSample $MinSample))
Write-Output (Write-ComparisonRow -Metric 'Terminal failed runs' -GateOff (Format-CountWithRate -Count (Get-MapCount -Map $gateOff.journals.terminalByPhase -Key 'failed') -Sample $gateOff.journals.terminalRuns -MinimumSample $MinSample) -GateOn (Format-CountWithRate -Count (Get-MapCount -Map $gateOn.journals.terminalByPhase -Key 'failed') -Sample $gateOn.journals.terminalRuns -MinimumSample $MinSample))
Write-Output (Write-ComparisonRow -Metric 'Decision records' -GateOff ([string]$gateOff.decisionGate.records) -GateOn ([string]$gateOn.decisionGate.records))
Write-Output (Write-ComparisonRow -Metric 'Known valid samples' -GateOff ([string]$gateOff.decisionGate.knownValidSamples) -GateOn ([string]$gateOn.decisionGate.knownValidSamples))
Write-Output (Write-ComparisonRow -Metric 'Unknown-validity samples' -GateOff ([string]$gateOff.decisionGate.unknownValiditySamples) -GateOn ([string]$gateOn.decisionGate.unknownValiditySamples))
Write-Output (Write-ComparisonRow -Metric 'Valid handoff called bad' -GateOff (Format-CountWithRate -Count $gateOff.decisionGate.validHandoffCalledBad -Sample $gateOff.decisionGate.knownValidSamples -MinimumSample $MinSample) -GateOn (Format-CountWithRate -Count $gateOn.decisionGate.validHandoffCalledBad -Sample $gateOn.decisionGate.knownValidSamples -MinimumSample $MinSample))
Write-Output (Write-ComparisonRow -Metric '... and still non-success' -GateOff (Format-CountWithRate -Count $gateOff.decisionGate.validHandoffCalledBadNonSuccess -Sample $gateOff.decisionGate.validHandoffCalledBad -MinimumSample $MinSample) -GateOn (Format-CountWithRate -Count $gateOn.decisionGate.validHandoffCalledBadNonSuccess -Sample $gateOn.decisionGate.validHandoffCalledBad -MinimumSample $MinSample))
Write-Output (Write-ComparisonRow -Metric '... recovered to success' -GateOff (Format-CountWithRate -Count $gateOff.decisionGate.validHandoffCalledBadSuccess -Sample $gateOff.decisionGate.validHandoffCalledBad -MinimumSample $MinSample) -GateOn (Format-CountWithRate -Count $gateOn.decisionGate.validHandoffCalledBadSuccess -Sample $gateOn.decisionGate.validHandoffCalledBad -MinimumSample $MinSample))
Write-Output (Write-ComparisonRow -Metric 'Model call errors' -GateOff (Format-CountWithRate -Count $gateOff.decisionGate.modelErrors -Sample $gateOff.decisionGate.records -MinimumSample $MinSample) -GateOn (Format-CountWithRate -Count $gateOn.decisionGate.modelErrors -Sample $gateOn.decisionGate.records -MinimumSample $MinSample))
Write-Output (Write-ComparisonRow -Metric 'Intake shadow sampled' -GateOff ([string]$gateOff.backlogIntakeShadow.sampled) -GateOn ([string]$gateOn.backlogIntakeShadow.sampled))
Write-Output (Write-ComparisonRow -Metric 'Intake shadow flagged' -GateOff (Format-CountWithRate -Count $gateOff.backlogIntakeShadow.flagged -Sample $gateOff.backlogIntakeShadow.certain -MinimumSample $MinSample) -GateOn (Format-CountWithRate -Count $gateOn.backlogIntakeShadow.flagged -Sample $gateOn.backlogIntakeShadow.certain -MinimumSample $MinSample))
Write-Output (Write-ComparisonRow -Metric '... flagged later escalated' -GateOff (Format-CountWithRate -Count $gateOff.backlogIntakeShadow.flaggedLaterEscalated -Sample $gateOff.backlogIntakeShadow.flagged -MinimumSample $MinSample) -GateOn (Format-CountWithRate -Count $gateOn.backlogIntakeShadow.flaggedLaterEscalated -Sample $gateOn.backlogIntakeShadow.flagged -MinimumSample $MinSample))
Write-Output (Write-ComparisonRow -Metric '... unflagged later escalated' -GateOff (Format-CountWithRate -Count $gateOff.backlogIntakeShadow.unflaggedLaterEscalated -Sample $gateOff.backlogIntakeShadow.unflagged -MinimumSample $MinSample) -GateOn (Format-CountWithRate -Count $gateOn.backlogIntakeShadow.unflaggedLaterEscalated -Sample $gateOn.backlogIntakeShadow.unflagged -MinimumSample $MinSample))
Write-Output (Write-ComparisonRow -Metric 'Intake shadow uncertain/error' -GateOff (Format-CountWithRate -Count $gateOff.backlogIntakeShadow.uncertainOrError -Sample $gateOff.backlogIntakeShadow.sampled -MinimumSample $MinSample) -GateOn (Format-CountWithRate -Count $gateOn.backlogIntakeShadow.uncertainOrError -Sample $gateOn.backlogIntakeShadow.sampled -MinimumSample $MinSample))
Write-Output ''
Write-Output ('gate-off decision modes:    {0}' -f (Join-CountMap -Map $gateOff.decisionGate.byMode))
Write-Output ('gate-on decision modes:     {0}' -f (Join-CountMap -Map $gateOn.decisionGate.byMode))
Write-Output ('gate-off verdicts:          {0}' -f (Join-CountMap -Map $gateOff.decisionGate.byVerdict))
Write-Output ('gate-on verdicts:           {0}' -f (Join-CountMap -Map $gateOn.decisionGate.byVerdict))
Write-Output ('gate-off failure codes:     {0}' -f (Join-CountMap -Map $gateOff.journals.failureCodes))
Write-Output ('gate-on failure codes:      {0}' -f (Join-CountMap -Map $gateOn.journals.failureCodes))
