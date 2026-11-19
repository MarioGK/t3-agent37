function Install-T3Agent37 {
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue'

    if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) {
        throw 'This installer supports Windows.'
    }

    $nativeArch = $env:PROCESSOR_ARCHITEW6432
    if (-not $nativeArch) { $nativeArch = $env:PROCESSOR_ARCHITECTURE }
    switch ($nativeArch) {
        AMD64 { $arch = 'amd64' }
        ARM64 { $arch = 'arm64' }
        default { throw 'This installer supports x86-64 and ARM64.' }
    }

    $release = Invoke-RestMethod 'https://api.github.com/repos/MarioGK/t3-agent37/releases/latest'
    $tag = $release.tag_name
    if ($tag -cnotmatch '^v[0-9]+\.[0-9]+\.[0-9]+$') {
        throw 'Could not identify the latest release.'
    }
    $asset = "t3-agent37-windows-$arch.exe"
    $base = "https://github.com/MarioGK/t3-agent37/releases/download/$tag"

    $destination = $env:T3_AGENT37_INSTALL_DIR
    if (-not $destination) {
        $destination = Join-Path $env:LOCALAPPDATA 'Programs\t3-agent37'
    }
    $destination = [IO.Path]::GetFullPath($destination)
    [void][IO.Directory]::CreateDirectory($destination)
    $temp = Join-Path $destination ('.install-' + [guid]::NewGuid().ToString('N'))
    [void][IO.Directory]::CreateDirectory($temp)

    try {
        $manifest = Join-Path $temp 'SHA256SUMS'
        $download = Join-Path $temp $asset
        Invoke-WebRequest -UseBasicParsing "$base/SHA256SUMS" -OutFile $manifest
        $pattern = '^[a-fA-F0-9]{64}  ' + [regex]::Escape($asset) + '$'
        $entries = @(Get-Content -LiteralPath $manifest | Where-Object { $_ -cmatch $pattern })
        if ($entries.Count -ne 1) { throw "No unique checksum for $asset in $tag." }

        Invoke-WebRequest -UseBasicParsing "$base/$asset" -OutFile $download
        $actual = (Get-FileHash -LiteralPath $download -Algorithm SHA256).Hash
        if ($actual -ne $entries[0].Substring(0, 64)) { throw "Checksum verification failed for $asset." }

        $target = Join-Path $destination 't3-agent37.exe'
        Move-Item -LiteralPath $download -Destination $target -Force
    } finally {
        Remove-Item -LiteralPath $temp -Recurse -Force
    }

    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    $existing = ($userPath -split ';') | Where-Object { $_ -and $_ -ne $destination }
    $newPath = (@($destination) + @($existing)) -join ';'
    if ($newPath -ne $userPath) {
        [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
    }
    if (($env:Path -split ';') -notcontains $destination) {
        $env:Path = "$destination;$env:Path"
    }
    Write-Output "Installed t3-agent37 $tag at $target"
}

Install-T3Agent37
