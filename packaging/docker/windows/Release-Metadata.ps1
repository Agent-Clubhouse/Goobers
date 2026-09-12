# Use the .NET Framework/Core JSON reader to preserve JSON string values.
# ConvertFrom-Json on PowerShell 6+ converts ISO dates to DateTime by default;
# -DateKind String is only available from 7.5, not Windows PowerShell 5.1.
Add-Type -AssemblyName System.Runtime.Serialization

function Read-ImageRelease {
    param([Parameter(Mandatory = $true)][string]$Path)
    $raw = [System.IO.File]::ReadAllBytes($Path)
    $reader = [System.Runtime.Serialization.Json.JsonReaderWriterFactory]::CreateJsonReader($raw, [System.Xml.XmlDictionaryReaderQuotas]::Max)
    try {
        $document = New-Object System.Xml.XmlDocument
        $document.XmlResolver = $null
        $document.Load($reader)
        if ($document.DocumentElement.GetAttribute('type') -ne 'object') { throw 'Expected a release metadata object' }
        $release = @{}
        foreach ($name in @('schemaVersion', 'kind', 'version', 'commit', 'date', 'platform')) {
            $nodes = $document.SelectNodes('/root/' + $name)
            $type = 'string'
            if ($name -eq 'schemaVersion') { $type = 'number' }
            if ($nodes.Count -ne 1 -or $nodes[0].GetAttribute('type') -ne $type) { throw "Expected one $type release field: $name" }
            $release[$name] = $nodes[0].InnerText
        }
        return [PSCustomObject]$release
    } finally { $reader.Dispose() }
}

function Assert-ImageReleaseStamp {
    param(
        [Parameter(Mandatory = $true)]$Release,
        [Parameter(Mandatory = $true)][string]$Output,
        [Parameter(Mandatory = $true)][string]$Binary
    )
    $stamp = "$($Release.version) (commit $($Release.commit), built $($Release.date), "
    if (-not $Output.Contains($stamp) -or -not $Output.EndsWith('windows/amd64)')) { throw "$Binary release stamp mismatch: $Output" }
}
