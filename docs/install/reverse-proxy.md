# HTTPS reverse proxy

Use one public HTTPS origin, with the panel listening only on loopback. Do not expose port 8080, the updater health probe, or RCON through the proxy. These are configuration examples, not evidence that TLS deployment has been tested on your host.

## Panel configuration

Keep the existing root-owned configuration and change these fields to your actual hostname:

```json
{
  "listen": "127.0.0.1:8080",
  "public_url": "https://control.example.net",
  "secure_cookies": true
}
```

This is a fragment, not a replacement for the installer configuration. Restart only `new-empire-control.service` after editing it. The configured public URL is immutable from the browser. HTTPS public URLs automatically require Secure cookies; the browser must use the exact configured origin, not an IP alias or a different port. Subpath hosting is not supported.

Preserve the browser's `Origin` header. **Never overwrite Origin with a trusted constant:** that would defeat the application's origin check. Do not cache authenticated responses. Preserve request bodies and tus headers, and allow streaming responses for server-sent events. Node.js is not needed on the deployed host.

## Caddy example

Install Caddy using its [official installation instructions](https://caddyserver.com/docs/install). With correct public DNS and the required certificate-validation connectivity, a minimal site is:

```caddyfile
control.example.net {
    @updater_health path /internal/health
    respond @updater_health 404

    reverse_proxy 127.0.0.1:8080 {
        flush_interval -1
    }
}
```

Validate the configuration with `caddy validate --config /etc/caddy/Caddyfile` before reloading Caddy. Automatic certificate issuance depends on the actual DNS, network and CA policy; it is not an application feature or guarantee.

## nginx example

Provide a valid certificate/key using your certificate-management process. Do not put the private key in the panel data directory or repository.

```nginx
server {
    listen 80;
    server_name control.example.net;
    return 308 https://control.example.net$request_uri;
}

server {
    listen 443 ssl;
    server_name control.example.net;
    ssl_certificate /etc/letsencrypt/live/control.example.net/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/control.example.net/privkey.pem;
    ssl_protocols TLSv1.2 TLSv1.3;

    # Match the configured max_upload_bytes (32 GiB in the installer).
    client_max_body_size 32g;
    client_body_timeout 300s;

    location = /internal/health {
        return 404;
    }

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $http_host;
        proxy_set_header Connection "";
        proxy_request_buffering off;
        proxy_buffering off;
        proxy_cache off;
        proxy_read_timeout 1h;
        proxy_send_timeout 300s;
    }
}
```

Run `nginx -t` before reloading nginx. Use the same upload limit at every upstream gateway; otherwise a large tus request can fail at the proxy before reaching the application. A disconnected upload resumes through its recorded offset; do not strip `Tus-Resumable`, `Upload-Offset`, `Upload-Length`, `Upload-Metadata`, or `Location` headers. No websocket upgrade rule is needed for the current SSE transport.

## Verify before exposing the panel

1. Confirm the panel socket is bound to loopback and is unreachable directly from another host. Permit public HTTPS only through the proxy; keep the runtime helper's RCON firewall guard intact.
2. Open the exact HTTPS origin. Verify the certificate chain and the `nec_session` cookie's Secure, HttpOnly and SameSite=Strict attributes after login. Never capture the cookie value in diagnostics.
3. Confirm `/internal/health` is denied through the public proxy. The independent updater uses its authenticated loopback probe directly, not the proxy.
4. Confirm an incorrect/missing mutation Origin or CSRF token is refused. Check login, a non-destructive settings operation, resumable upload, and the live invalidation stream using disposable data.
5. Check proxy timeouts, body limits, logging/redaction and disk capacity under the expected load. The application does not trust forwarded-IP headers: behind one proxy its process-local sign-in limiter is shared by that proxy's clients. Apply suitable edge controls without weakening the application checks.

The proxy examples do not add a Minecraft Docker provider, secure arbitrary local processes, or replace the [Linux/systemd](linux-systemd.md) least-privilege installation and [acceptance checks](../acceptance.md).
