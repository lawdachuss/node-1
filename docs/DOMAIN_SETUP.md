# Domain Setup Guide: chuglii.in → GoOnDVR UI

## Current Setup
- **EC2 IP**: 32.193.245.111
- **Current URL**: http://32.193.245.111:8080
- **Domain**: chuglii.in (managed in Cloudflare)
- **Target URL**: http://chuglii.in (port 80)

---

## Step 1: Configure Cloudflare DNS

### 1.1 Login to Cloudflare
Go to https://dash.cloudflare.com/ and select your domain `chuglii.in`

### 1.2 Add DNS Record
1. Click **DNS** in the left sidebar
2. Click **Add record**
3. Configure:
   - **Type**: `A`
   - **Name**: `@` (for root domain) or `app` (for app.chuglii.in)
   - **IPv4 address**: `32.193.245.111`
   - **Proxy status**: 🟠 **Proxied** (orange cloud - RECOMMENDED)
   - **TTL**: Auto
4. Click **Save**

### 1.3 Optional: Add WWW Subdomain
1. Click **Add record** again
2. Configure:
   - **Type**: `CNAME`
   - **Name**: `www`
   - **Target**: `chuglii.in`
   - **Proxy status**: 🟠 **Proxied**
3. Click **Save**

---

## Step 2: Install Nginx on EC2

SSH into your EC2 instance:

```bash
ssh -i aws-secrets/aws-key.pem ubuntu@32.193.245.111
```

Install Nginx:

```bash
# Update package list
sudo apt update

# Install Nginx
sudo apt install nginx -y

# Check status
sudo systemctl status nginx
```

---

## Step 3: Configure Nginx Reverse Proxy

### 3.1 Create Nginx Configuration

```bash
sudo nano /etc/nginx/sites-available/chuglii.in
```

Paste this configuration:

```nginx
server {
    listen 80;
    listen [::]:80;
    server_name chuglii.in www.chuglii.in;

    # Security headers
    add_header X-Frame-Options "SAMEORIGIN" always;
    add_header X-Content-Type-Options "nosniff" always;
    add_header X-XSS-Protection "1; mode=block" always;

    # Increase timeouts for long-running requests
    proxy_connect_timeout 300s;
    proxy_send_timeout 300s;
    proxy_read_timeout 300s;
    send_timeout 300s;

    # Increase buffer sizes for large responses
    proxy_buffer_size 128k;
    proxy_buffers 4 256k;
    proxy_busy_buffers_size 256k;

    location / {
        proxy_pass http://localhost:8080;
        proxy_http_version 1.1;
        
        # WebSocket support (for live updates)
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        
        # Forward real client IP
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        
        # Disable buffering for SSE/streaming
        proxy_buffering off;
        proxy_cache off;
    }

    # Health check endpoint
    location /health {
        access_log off;
        return 200 "OK\n";
        add_header Content-Type text/plain;
    }
}
```

Save and exit (Ctrl+X, Y, Enter)

### 3.2 Enable the Site

```bash
# Create symbolic link
sudo ln -s /etc/nginx/sites-available/chuglii.in /etc/nginx/sites-enabled/

# Remove default site (optional)
sudo rm /etc/nginx/sites-enabled/default

# Test configuration
sudo nginx -t

# Reload Nginx
sudo systemctl reload nginx
```

---

## Step 4: Update EC2 Security Group

### 4.1 Via AWS Console
1. Go to **EC2 Console** → **Security Groups**
2. Select your security group: `sg-0f10a1cf097ce912d`
3. Click **Edit inbound rules**
4. Add rule:
   - **Type**: HTTP
   - **Port**: 80
   - **Source**: 0.0.0.0/0 (Anywhere IPv4)
   - **Description**: HTTP for chuglii.in
5. Add rule:
   - **Type**: HTTP
   - **Port**: 80
   - **Source**: ::/0 (Anywhere IPv6)
6. Click **Save rules**

### 4.2 Via AWS CLI (Alternative)

```bash
aws ec2 authorize-security-group-ingress \
    --group-id sg-0f10a1cf097ce912d \
    --protocol tcp \
    --port 80 \
    --cidr 0.0.0.0/0 \
    --region us-east-1
```

---

## Step 5: Test the Setup

### 5.1 Test Nginx Locally
```bash
curl http://localhost:80
```

Should return the GoOnDVR UI HTML.

### 5.2 Test from Your Computer
```bash
curl http://32.193.245.111
```

### 5.3 Test Domain (wait 1-5 minutes for DNS propagation)
```bash
curl http://chuglii.in
```

### 5.4 Open in Browser
Go to: **http://chuglii.in**

---

## Step 6: Enable HTTPS (Optional but Recommended)

### 6.1 Install Certbot

```bash
sudo apt install certbot python3-certbot-nginx -y
```

### 6.2 Get SSL Certificate

```bash
sudo certbot --nginx -d chuglii.in -d www.chuglii.in
```

Follow the prompts:
- Enter your email
- Agree to terms
- Choose whether to redirect HTTP to HTTPS (recommended: Yes)

### 6.3 Auto-Renewal

Certbot automatically sets up renewal. Test it:

```bash
sudo certbot renew --dry-run
```

---

## Step 7: Update Cloudflare SSL Settings

### 7.1 Go to Cloudflare Dashboard
1. Select `chuglii.in`
2. Click **SSL/TLS** in sidebar
3. Set **SSL/TLS encryption mode** to:
   - **Flexible** (if you didn't install SSL certificate)
   - **Full** (if you installed SSL certificate)
   - **Full (strict)** (if you want maximum security)

### 7.2 Enable Always Use HTTPS
1. Go to **SSL/TLS** → **Edge Certificates**
2. Enable **Always Use HTTPS**
3. Enable **Automatic HTTPS Rewrites**

---

## Final URLs

After setup, you can access GoOnDVR at:

- ✅ **http://chuglii.in** (if HTTP only)
- ✅ **https://chuglii.in** (if SSL enabled)
- ✅ **http://www.chuglii.in** (if CNAME added)
- ✅ **https://www.chuglii.in** (if SSL enabled)

Old URL still works:
- ✅ **http://32.193.245.111:8080** (direct access)

---

## Troubleshooting

### DNS Not Resolving
```bash
# Check DNS propagation
nslookup chuglii.in
dig chuglii.in

# Should return: 32.193.245.111
```

Wait 1-5 minutes for Cloudflare DNS to propagate.

### Nginx Not Working
```bash
# Check Nginx status
sudo systemctl status nginx

# Check Nginx error logs
sudo tail -f /var/log/nginx/error.log

# Restart Nginx
sudo systemctl restart nginx
```

### Port 80 Not Accessible
```bash
# Check if Nginx is listening
sudo netstat -tlnp | grep :80

# Check firewall
sudo ufw status

# If firewall is active, allow port 80
sudo ufw allow 80/tcp
```

### 502 Bad Gateway
```bash
# Check if GoOnDVR is running
sudo docker compose -f /home/ubuntu/goondvr/docker-compose.yml ps

# Check GoOnDVR logs
sudo docker compose -f /home/ubuntu/goondvr/docker-compose.yml logs recorder --tail=50
```

### SSL Certificate Issues
```bash
# Check certificate status
sudo certbot certificates

# Renew certificate manually
sudo certbot renew

# Check Nginx SSL configuration
sudo nginx -t
```

---

## Quick Setup Script

Save this as `setup-domain.sh`:

```bash
#!/bin/bash

echo "🌐 Setting up chuglii.in domain..."

# Install Nginx
echo "📦 Installing Nginx..."
sudo apt update
sudo apt install nginx -y

# Create Nginx config
echo "⚙️  Creating Nginx configuration..."
sudo tee /etc/nginx/sites-available/chuglii.in > /dev/null <<'EOF'
server {
    listen 80;
    listen [::]:80;
    server_name chuglii.in www.chuglii.in;

    location / {
        proxy_pass http://localhost:8080;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_buffering off;
    }
}
EOF

# Enable site
echo "🔗 Enabling site..."
sudo ln -sf /etc/nginx/sites-available/chuglii.in /etc/nginx/sites-enabled/
sudo rm -f /etc/nginx/sites-enabled/default

# Test and reload
echo "✅ Testing Nginx configuration..."
sudo nginx -t

echo "🔄 Reloading Nginx..."
sudo systemctl reload nginx

echo ""
echo "✅ Setup complete!"
echo ""
echo "📋 Next steps:"
echo "1. Add A record in Cloudflare: chuglii.in → 32.193.245.111"
echo "2. Add security group rule: Port 80 (HTTP)"
echo "3. Wait 1-5 minutes for DNS propagation"
echo "4. Visit: http://chuglii.in"
echo ""
echo "🔒 Optional: Enable HTTPS with:"
echo "   sudo apt install certbot python3-certbot-nginx -y"
echo "   sudo certbot --nginx -d chuglii.in -d www.chuglii.in"
```

Run it:
```bash
chmod +x setup-domain.sh
./setup-domain.sh
```

---

## Summary Checklist

- [ ] Add A record in Cloudflare (chuglii.in → 32.193.245.111)
- [ ] Enable Cloudflare proxy (orange cloud)
- [ ] Install Nginx on EC2
- [ ] Create Nginx reverse proxy config
- [ ] Add port 80 to security group
- [ ] Test domain access
- [ ] (Optional) Install SSL certificate
- [ ] (Optional) Enable HTTPS redirect

**Estimated Time**: 10-15 minutes

---

**Last Updated**: April 30, 2026  
**Domain**: chuglii.in  
**EC2 IP**: 32.193.245.111
