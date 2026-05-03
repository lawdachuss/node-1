# Deploy Byparr Fix to EC2
# This script replaces FlareSolverr with Byparr (modern alternative)

param(
    [string]$EC2Host = "3.84.15.178",
    [string]$KeyPath = "aws-secrets/aws-key.pem",
    [string]$AppDir = "/home/ubuntu/goondvr"
)

Write-Host "========================================" -ForegroundColor Cyan
Write-Host "  Deploying Byparr Fix to EC2" -ForegroundColor Cyan
Write-Host "========================================" -ForegroundColor Cyan
Write-Host ""

# Check if SSH key exists
if (-not (Test-Path $KeyPath)) {
    Write-Host "ERROR: SSH key not found at $KeyPath" -ForegroundColor Red
    exit 1
}

Write-Host "Step 1: Uploading new docker-compose.yml..." -ForegroundColor Yellow
scp -i $KeyPath docker-compose.yml ubuntu@${EC2Host}:/tmp/docker-compose.yml
if ($LASTEXITCODE -ne 0) {
    Write-Host "ERROR: Failed to upload docker-compose.yml" -ForegroundColor Red
    exit 1
}

Write-Host "Step 2: Stopping old FlareSolverr containers..." -ForegroundColor Yellow
ssh -i $KeyPath ubuntu@$EC2Host "cd $AppDir && sudo docker compose down"

Write-Host "Step 3: Backing up old docker-compose.yml..." -ForegroundColor Yellow
ssh -i $KeyPath ubuntu@$EC2Host "sudo cp $AppDir/docker-compose.yml $AppDir/docker-compose.yml.flaresolverr.backup"

Write-Host "Step 4: Installing new docker-compose.yml..." -ForegroundColor Yellow
ssh -i $KeyPath ubuntu@$EC2Host "sudo mv /tmp/docker-compose.yml $AppDir/docker-compose.yml && sudo chown ubuntu:ubuntu $AppDir/docker-compose.yml"

Write-Host "Step 5: Pulling Byparr images..." -ForegroundColor Yellow
ssh -i $KeyPath ubuntu@$EC2Host "cd $AppDir && sudo docker compose pull byparr-1 byparr-2 byparr-3"

Write-Host "Step 6: Starting services with Byparr..." -ForegroundColor Yellow
ssh -i $KeyPath ubuntu@$EC2Host "cd $AppDir && sudo docker compose up -d"

Write-Host ""
Write-Host "========================================" -ForegroundColor Green
Write-Host "  Deployment Complete!" -ForegroundColor Green
Write-Host "========================================" -ForegroundColor Green
Write-Host ""
Write-Host "What changed:" -ForegroundColor Cyan
Write-Host "  - Replaced FlareSolverr (deprecated) with Byparr (modern alternative)" -ForegroundColor White
Write-Host "  - Byparr uses Camoufox (Firefox-based anti-detection browser)" -ForegroundColor White
Write-Host "  - Better success rate against modern Cloudflare (2026)" -ForegroundColor White
Write-Host "  - Reduced from 5 to 3 instances (more efficient)" -ForegroundColor White
Write-Host ""
Write-Host "Monitor logs:" -ForegroundColor Cyan
Write-Host "  ssh -i $KeyPath ubuntu@$EC2Host 'cd $AppDir && sudo docker compose logs -f recorder'" -ForegroundColor Gray
Write-Host ""
Write-Host "Check Byparr status:" -ForegroundColor Cyan
Write-Host "  ssh -i $KeyPath ubuntu@$EC2Host 'cd $AppDir && sudo docker compose ps'" -ForegroundColor Gray
Write-Host ""
Write-Host "Rollback if needed:" -ForegroundColor Cyan
Write-Host "  ssh -i $KeyPath ubuntu@$EC2Host 'cd $AppDir && sudo cp docker-compose.yml.flaresolverr.backup docker-compose.yml && sudo docker compose up -d'" -ForegroundColor Gray
Write-Host ""
