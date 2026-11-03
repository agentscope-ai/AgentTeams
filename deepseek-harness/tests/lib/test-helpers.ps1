function Get-FreeTcpPort {
    $Listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0)
    $Listener.Start()
    try { return ([Net.IPEndPoint]$Listener.LocalEndpoint).Port }
    finally { $Listener.Stop() }
}

function Wait-TcpPort([int]$Port, [int]$TimeoutSeconds = 20, [string]$Label = 'local port') {
    $Deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    while ([DateTime]::UtcNow -lt $Deadline) {
        $Client = [Net.Sockets.TcpClient]::new()
        try {
            $Connected = $Client.ConnectAsync('127.0.0.1', $Port)
            if ($Connected.Wait(250) -and $Client.Connected) { return }
        }
        catch { }
        finally { $Client.Dispose() }
        Start-Sleep -Milliseconds 100
    }
    throw "Timed out waiting for $Label on port $Port"
}

function Get-SecretText([object]$Secret, [string]$Key) {
    $Property = $Secret.data.PSObject.Properties[$Key]
    if ($null -eq $Property) { throw "Secret is missing $Key" }
    return [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String([string]$Property.Value))
}

function Get-PodEnv([object]$Pod, [string]$Name) {
    $Entry = @($Pod.spec.containers[0].env) | Where-Object { $_.name -eq $Name } | Select-Object -First 1
    if ($null -eq $Entry -or [string]::IsNullOrWhiteSpace([string]$Entry.value)) {
        throw "Pod is missing required literal env $Name"
    }
    return [string]$Entry.value
}

function Invoke-MatrixJson([string]$Method, [string]$Url, [string]$Token, [object]$Body = $null) {
    $Headers = @{ Authorization = "Bearer $Token" }
    if ($null -eq $Body) {
        return Invoke-RestMethod -Method $Method -Uri $Url -Headers $Headers
    }
    return Invoke-RestMethod -Method $Method -Uri $Url -Headers $Headers -ContentType 'application/json' -Body ($Body | ConvertTo-Json -Depth 12 -Compress)
}
