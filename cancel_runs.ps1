param()
foreach ($r in 1..10) {
    $repo = "lawdachuss/node-$r"
    $json = gh run list --repo $repo --limit 1 --json databaseId,status --jq ".[0]" 2>$null
    if ($json) {
        $obj = $json | ConvertFrom-Json
        if ($obj.status -eq "in_progress" -or $obj.status -eq "queued") {
            gh run cancel $obj.databaseId --repo $repo
            Write-Host ("node-${r}: cancelled $($obj.databaseId)")
        } else {
            Write-Host ("node-${r}: status=$($obj.status)")
        }
    }
}
