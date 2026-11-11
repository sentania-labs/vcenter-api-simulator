#!/usr/bin/env pwsh
<#
.SYNOPSIS
Smoke-test a running simulator the way an operator first would: Connect-VIServer,
Get-VMHost, Get-VM, and check the expected inventory came back.

.DESCRIPTION
This is the acceptance gate for the container image and the Helm chart in CI.
It needs VMware.PowerCLI; -Install fetches the pinned version from the
PowerShell Gallery into the current user's module path when it is missing.
The Phase 1 topology has four hosts and four VMs; pass -Hosts/-VMs for
another seed.

.EXAMPLE
tools/smoke/smoke.ps1 -Server 127.0.0.1 -Port 8443 -Install
tools/smoke/smoke.ps1 -Server vcsim.example.test -User operator -Password placeholder
#>
[CmdletBinding()]
param(
    [string]$Server = '127.0.0.1',
    [int]$Port = 8443,
    [string]$User = 'administrator@vsphere.local',
    [string]$Password = 'VMware123!VMware123!',
    [int]$Hosts = 4,
    [int]$VMs = 4,
    # Check examples/scenarios/storage.yaml, including its failed paths.
    [switch]$Storage,
    # Seconds to wait for /sdk/vimServiceVersions.xml before connecting.
    [int]$Wait = 60,
    # Install VMware.PowerCLI (pinned) if it is not available.
    [switch]$Install,
    [string]$PowerCLIVersion = '13.3.0.24145083'
)
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

function Fail([string]$msg) { Write-Host "smoke: FAIL: $msg"; exit 1 }

if (-not (Get-Module -ListAvailable -Name VMware.PowerCLI)) {
    if (-not $Install) { Fail 'VMware.PowerCLI is not installed; rerun with -Install' }
    Write-Host "smoke: installing VMware.PowerCLI $PowerCLIVersion"
    Set-PSRepository -Name PSGallery -InstallationPolicy Trusted
    Install-Module VMware.PowerCLI -RequiredVersion $PowerCLIVersion -Scope CurrentUser -Force -AllowClobber
}
Import-Module VMware.VimAutomation.Core
Set-PowerCLIConfiguration -InvalidCertificateAction Ignore -ParticipateInCEIP $false -DisplayDeprecationWarnings $false -Scope Session -Confirm:$false | Out-Null

$url = "https://${Server}:${Port}/sdk/vimServiceVersions.xml"
$ready = $false
for ($i = 0; $i -lt $Wait; $i++) {
    try {
        $r = Invoke-WebRequest -Uri $url -SkipCertificateCheck -TimeoutSec 3 -UseBasicParsing
        if ($r.Content -match '<version>') { $ready = $true; break }
    } catch { }
    Start-Sleep -Seconds 1
}
if (-not $ready) { Fail "$url not served within ${Wait}s" }

Write-Host "smoke: Connect-VIServer ${Server}:${Port} as $User"
try {
    $vi = Connect-VIServer -Server $Server -Port $Port -User $User -Password $Password -Force
} catch {
    Fail "Connect-VIServer: $($_.Exception.Message)"
}
Write-Host "smoke:   $($vi.ExtensionData.Content.About.FullName) (build $($vi.Build), uuid $($vi.InstanceUuid))"

try {
    Write-Host 'smoke: Get-VMHost'
    $hostList = @(Get-VMHost -Server $vi | Sort-Object Name)
    $hostList | ForEach-Object { Write-Host "smoke:   $($_.Name) $($_.ConnectionState) $($_.PowerState) cpu=$($_.NumCpu) mem=$([math]::Round($_.MemoryTotalGB, 1))GB" }
    if ($hostList.Count -ne $Hosts) { Fail "expected $Hosts hosts, got $($hostList.Count)" }

    Write-Host 'smoke: Get-VM'
    $vmList = @(Get-VM -Server $vi | Sort-Object Name)
    $vmList | ForEach-Object { Write-Host "smoke:   $($_.Name) $($_.PowerState) on $($_.VMHost.Name)" }
    if ($vmList.Count -ne $VMs) { Fail "expected $VMs VMs, got $($vmList.Count)" }
    if ($Storage) {
        $nfs = Get-Datastore -Server $vi -Name 'shared-nfs'
        $vmfs = Get-Datastore -Server $vi -Name 'shared-vmfs'
        if ($nfs.Type -ne 'NFS' -or $vmfs.Type -ne 'VMFS') { Fail 'storage datastore types differ from the scenario' }
        $badHost = $hostList | Where-Object { $_.Name -like 'esx02.*' }
        $hbas = @(Get-VMHostHba -VMHost $badHost -Type iScsi)
        if ($hbas.Count -ne 2) { Fail "expected two iSCSI adapters, got $($hbas.Count)" }
        $targets = @($hbas | Get-IScsiHbaTarget)
        if ($targets.Count -lt 2) { Fail 'iSCSI targets missing' }
        $luns = @(Get-ScsiLun -VMHost $badHost -LunType disk | Where-Object { $_.Vendor -eq 'SIMULATR' })
        if ($luns.Count -ne 1) { Fail 'shared iSCSI LUN missing' }
        $mount = $vmfs.ExtensionData.Host | Where-Object { $_.Key.Value -eq $badHost.ExtensionData.MoRef.Value }
        $nasMount = $nfs.ExtensionData.Host | Where-Object { $_.Key.Value -eq $badHost.ExtensionData.MoRef.Value }
        if ($mount.MountInfo.Accessible -ne $false -or $nasMount.MountInfo.Accessible -ne $false) { Fail 'host storage faults missing' }
        Write-Host 'smoke: storage datastores, adapters, targets, LUN and fault states verified'
    }
} finally {
    Disconnect-VIServer -Server $vi -Confirm:$false
}
Write-Host "smoke: PASS ($Hosts hosts, $VMs VMs)"
exit 0
