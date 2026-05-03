# Deploy Scalable Byparr Solution to EC2

param(
    [string]$EC2Host = "3.84.15.178",
    [string]$KeyPath = "aws-secrets/aws-key.pem",
    [string]$AppDir = "/home/ubuntu/goondvr"
)

Write-Host "========================================" -ForegroundColor Cyan
Write-Host "  Deploying Scalable Byparr Solution" -ForegroundColor Cyan
Write-Host "========================================" -ForegroundColor Cyan
Write-Host ""

# Check if SSH key exists
if (-not (Test-Path $KeyPath)) {
    Write-Host "ERROR: SSH key not found at $KeyPath" -ForegroundColor Red
    exit 1
}

Write-Host "Step 1: Uploading configuration files..." -ForegroundColor Yellow
scp -i $KeyPath docker-compose.yml ubuntu@${EC2Host}:/tmp/docker-compose.yml
scp -i $KeyPath nginx.conf ubuntu@${EC2Host}:/tmp/nginx.conf

Write-Host "Step 2: Stopping current services..." -ForegroundColor Yellow
ssh -i $KeyPath ubuntu@$EC2Host "cd $AppDir && sudo docker compose down"

Write-Host "Step 3: Backing up old configuration..." -ForegroundColor Yellow
ssh -i $KeyPath ubuntu@$EC2Host "sudo cp $AppDir/docker-compose.yml $AppDir/docker-compose.yml.fixed-instances.backup 2>/dev/null || true"

Write-Host "Step 4: Installing new configuration..." -ForegroundColor Yellow
ssh -i $KeyPath ubuntu@$EC2Host "sudo mv /tmp/docker-compose.yml $AppDir/docker-compose.yml && sudo mv /tmp/nginx.conf $AppDir/nginx.conf && sudo chown ubuntu:ubuntu $AppDir/docker-compose.yml $AppDir/nginx.conf"

Write-Host "Step 5: Pulling latest Byparr image..." -ForegroundColor Yellow
ssh -i $KeyPath ubuntu@$EC2Host "cd $AppDir && sudo docker compose pull byparr"

# Count active channels
Write-Host "Step 6: Calculating optimal scale..." -ForegroundColor Yellow
$channelCount = ssh -i $KeyPath ubuntu@$EC2Host "jq '[.[] | select(.is_paused == false)] | length' $AppDir/conf/channels.json"
$channelCount = [int]$channelCount

$neededInstances = [Math]::Ceiling($channelCount / 10)
if ($neededInstances -lt 3) { $neededInstances = 3 }
if ($neededInstances -gt 20) { $neededInstances = 20 }

Write-Host "   Active channels: $channelCount" -ForegroundColor White
Write-Host "   Byparr instances: $neededInstances" -ForegroundColor White

Write-Host "Step 7: Starting services with $neededInstances Byparr instances..." -ForegroundColor Yellow
ssh -i $KeyPath ubuntu@$EC2Host "cd $AppDir && sudo docker compose up -d --scale byparr=$neededInstances"

Write-Host ""
Write-Host "========================================" -ForegroundColor Green
Write-Host "  Deployment Complete!" -ForegroundColor Green
Write-Host "========================================" -ForegroundColor Green
Write-Host ""
Write-Host "What changed:" -ForegroundColor Cyan
Write-Host "  ✅ Scalable Byparr service (can handle 10-200+ channels)" -ForegroundColor White
Write-Host "  ✅ Nginx load balancer (automatic failover)" -ForegroundColor White
Write-Host "  ✅ Auto-scaled to $neededInstances instances for $channelCount channels" -ForegroundColor White
Write-Host "  ✅ Easy scaling: docker compose up -d --scale byparr=N" -ForegroundColor White
Write-Host ""
Write-Host "Monitor logs:" -ForegroundColor Cyan
Write-Host "  ssh -i $KeyPath ubuntu@$EC2Host 'cd $AppDir && sudo docker compose logs -f recorder'" -ForegroundColor Gray
Write-Host ""
Write-Host "Check Byparr status:" -ForegroundColor Cyan
Write-Host "  ssh -i $KeyPath ubuntu@$EC2Host 'cd $AppDir && sudo docker compose ps'" -ForegroundColor Gray
Write-Host ""
Write-Host "Scale manually:" -ForegroundColor Cyan
Write-Host "  ssh -i $KeyPath ubuntu@$EC2Host 'cd $AppDir && sudo docker compose up -d --scale byparr=10 --no-recreate'" -ForegroundColor Gray
Write-Host ""
Write-Host "See SCALING_GUIDE.md for more details!" -ForegroundColor Yellow
Write-Host ""
