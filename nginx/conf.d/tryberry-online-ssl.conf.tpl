# ────────────────────────────────────────────────────────────────────────────
# tryberry.online — HTTPS. Лежит как .tpl НАМЕРЕННО: nginx подхватывает только
# *.conf, а с несуществующим сертификатом он не стартует. После выпуска серта
# копируется в tryberry-online-ssl.conf (команда — в шапке tryberry-online.conf).

# Мотивация и роль домена — в шапке tryberry-online.conf.
# ────────────────────────────────────────────────────────────────────────────

server {
    listen      443 ssl;
    listen      [::]:443 ssl;
    http2 on;
    server_name tryberry.online;

    ssl_certificate     /etc/letsencrypt/live/tryberry.online/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/tryberry.online/privkey.pem;
    include             /etc/letsencrypt/options-ssl-nginx.conf;
    ssl_dhparam         /etc/letsencrypt/ssl-dhparams.pem;

    resolver 127.0.0.11 valid=10s ipv6=off;
    resolver_timeout 5s;

    # Те же security-заголовки, что на основном домене (грейд A на
    # securityheaders). Отличие одно — noindex: зеркало не индексируем.
    add_header X-Robots-Tag "noindex, nofollow" always;
    add_header Strict-Transport-Security "max-age=31536000; includeSubDomains" always;
    add_header X-Content-Type-Options "nosniff" always;
    add_header X-Frame-Options "SAMEORIGIN" always;
    add_header Referrer-Policy "strict-origin-when-cross-origin" always;
    add_header Permissions-Policy "camera=(), microphone=(), geolocation=()" always;
    add_header Cross-Origin-Opener-Policy "same-origin" always;
    add_header Cross-Origin-Resource-Policy "same-origin" always;
    add_header Content-Security-Policy "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data: https:; font-src 'self'; connect-src 'self'; frame-ancestors 'self'; base-uri 'self'; form-action 'self'; object-src 'none'" always;

    # ── SSR-страницы графиков ───────────────────────────────────────────────
    location ^~ /p/ {
        limit_req zone=api burst=20 nodelay;
        if ($request_method !~ ^(GET|HEAD)$) { return 405; }
        set $upstream_api api:8081;
        proxy_pass http://$upstream_api;
        proxy_http_version 1.1;
        proxy_set_header   Host              $host;
        proxy_set_header   X-Real-IP         $remote_addr;
        proxy_set_header   X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header   X-Forwarded-Proto $scheme;
        proxy_set_header   Connection        "";
        proxy_read_timeout 10s;
        proxy_connect_timeout 5s;
    }

    location ^~ /api/ {
        limit_req zone=api burst=20 nodelay;
        if ($request_method !~ ^(GET|HEAD)$) { return 405; }
        set $upstream_api api:8081;
        proxy_pass http://$upstream_api;
        proxy_http_version 1.1;
        proxy_set_header   Host              $host;
        proxy_set_header   X-Real-IP         $remote_addr;
        proxy_set_header   X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header   X-Forwarded-Proto $scheme;
        proxy_set_header   Connection        "";
        proxy_read_timeout 10s;
        proxy_connect_timeout 5s;
    }

    # Зеркало не индексируем — свой robots.txt вместо проксирования на api.
    location = /robots.txt {
        add_header Content-Type text/plain;
        return 200 "User-agent: *\nDisallow: /\n";
    }

    # Карта сайта у зеркала своя не нужна: индексируется только основной домен.
    location = /sitemap.xml { return 404; }

    # ── Статика (те же правила кэширования, что на основном домене) ─────────
    location ^~ /assets/ {
        root  /usr/share/nginx/html;
        try_files $uri =404;
        add_header Cache-Control "public, max-age=31536000, immutable" always;
        add_header X-Content-Type-Options "nosniff" always;
        add_header X-Robots-Tag "noindex, nofollow" always;
    }
    location ^~ /vendor/ {
        root  /usr/share/nginx/html;
        try_files $uri =404;
        add_header Cache-Control "public, max-age=31536000, immutable" always;
        add_header X-Content-Type-Options "nosniff" always;
        add_header X-Robots-Tag "noindex, nofollow" always;
    }
    location ^~ /fonts/ {
        root  /usr/share/nginx/html;
        try_files $uri =404;
        add_header Cache-Control "public, max-age=31536000, immutable" always;
        add_header X-Content-Type-Options "nosniff" always;
        add_header X-Robots-Tag "noindex, nofollow" always;
        types { font/woff2 woff2; text/css css; }
    }

    location ~* \.webmanifest$ {
        root  /usr/share/nginx/html;
        default_type application/manifest+json;
    }

    location = /favicon.ico {
        root  /usr/share/nginx/html;
        try_files /logo.png =404;
        expires 7d;
        access_log off;
    }

    location / {
        root  /usr/share/nginx/html;
        index index.html;
        try_files $uri $uri/ =404;
        expires 1h;
    }

    # Вебхуки платёжек и мессенджеров живут только на основном домене.
    location ~ ^/(webhook|vk/callback|max/callback|yookassa/webhook|robokassa/result)$ {
        return 404;
    }

    location ~ ^/(health|metrics|live)$ {
        return 404;
    }
}

# www.tryberry.online → apex (сертификат должен включать оба имени).
server {
    listen      443 ssl;
    listen      [::]:443 ssl;
    http2 on;
    server_name www.tryberry.online;

    ssl_certificate     /etc/letsencrypt/live/tryberry.online/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/tryberry.online/privkey.pem;
    include             /etc/letsencrypt/options-ssl-nginx.conf;
    ssl_dhparam         /etc/letsencrypt/ssl-dhparams.pem;

    return 301 https://tryberry.online$request_uri;
}
