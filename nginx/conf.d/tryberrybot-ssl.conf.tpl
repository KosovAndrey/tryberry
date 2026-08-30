# ────────────────────────────────────────────────────────────────────────────
# tryberrybot.ru — HTTPS. Лежит как .tpl НАМЕРЕННО: nginx подхватывает только
# *.conf, а с несуществующим сертификатом он не стартует. После выпуска серта
# копируется в tryberrybot-ssl.conf (команда — в шапке tryberrybot.conf).
#
# Роль домена: ОСНОВНОЙ САЙТ (решение 2026-08-30). tryberry.ru остаётся
# открытым и рабочим, но канонические адреса, sitemap и OG ведут сюда —
# он одинаково доступен во всех браузерах, а tryberry.ru отрезан у Safari
# на iOS вердиктом Apple (docs/SAFE-BROWSING-APPEAL.md). Редиректа со
# старого домена нет намеренно: на нём висят вебхуки Telegram, VK, MAX и
# Робокассы, и уводить их нельзя. Поисковики переедут по canonical.
# ────────────────────────────────────────────────────────────────────────────

server {
    listen      443 ssl;
    listen      [::]:443 ssl;
    http2 on;
    server_name tryberrybot.ru;

    ssl_certificate     /etc/letsencrypt/live/tryberrybot.ru/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/tryberrybot.ru/privkey.pem;
    include             /etc/letsencrypt/options-ssl-nginx.conf;
    ssl_dhparam         /etc/letsencrypt/ssl-dhparams.pem;

    resolver 127.0.0.11 valid=10s ipv6=off;
    resolver_timeout 5s;

    # Security-заголовки — те же, что на tryberry.ru (грейд A на securityheaders).
    # X-Robots-Tag здесь НЕТ: домен основной и должен индексироваться.
    add_header Strict-Transport-Security "max-age=31536000; includeSubDomains" always;
    add_header X-Content-Type-Options "nosniff" always;
    add_header X-Frame-Options "SAMEORIGIN" always;
    add_header Referrer-Policy "strict-origin-when-cross-origin" always;
    add_header Permissions-Policy "camera=(), microphone=(), geolocation=()" always;
    add_header Cross-Origin-Opener-Policy "same-origin" always;
    add_header Cross-Origin-Resource-Policy "same-origin" always;
    add_header Content-Security-Policy "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data: https:; font-src 'self'; connect-src 'self'; frame-ancestors 'self'; base-uri 'self'; form-action 'self'; object-src 'none'" always;

    # Внутренние markdown-файлы (web/CLAUDE.md) наружу не отдаём — как и на
    # основном домене. Зеркало раздаёт тот же том ./web, поэтому правило нужно
    # и здесь: без него внутренняя инструкция утекала бы через новый домен.
    location ~* \.md$ {
        return 404;
    }

    # Удалённые страницы-заготовки промо (/screen*, /wall*): файлов нет, но
    # пусть ответ будет таким же однозначным, как на основном домене.
    location ~ ^/(screen|wall) {
        return 410;
    }

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

    # robots.txt и sitemap.xml отдаёт api — они строятся из PUBLIC_BASE_URL,
    # который теперь равен https://tryberrybot.ru.
    location = /robots.txt {
        set $upstream_api api:8081;
        proxy_pass http://$upstream_api/robots.txt;
        proxy_http_version 1.1;
        proxy_set_header   Host       $host;
        proxy_set_header   Connection "";
        proxy_read_timeout 10s;
        proxy_connect_timeout 5s;
    }

    location = /sitemap.xml {
        limit_req zone=api burst=5 nodelay;
        set $upstream_api api:8081;
        proxy_pass http://$upstream_api/sitemap.xml;
        proxy_http_version 1.1;
        proxy_set_header   Host       $host;
        proxy_set_header   Connection "";
        proxy_read_timeout 10s;
        proxy_connect_timeout 5s;
    }

    # ── Статика (те же правила кэширования, что на основном домене) ─────────
    location ^~ /assets/ {
        root  /usr/share/nginx/html;
        try_files $uri =404;
        add_header Cache-Control "public, max-age=31536000, immutable" always;
        add_header X-Content-Type-Options "nosniff" always;
    }
    location ^~ /vendor/ {
        root  /usr/share/nginx/html;
        try_files $uri =404;
        add_header Cache-Control "public, max-age=31536000, immutable" always;
        add_header X-Content-Type-Options "nosniff" always;
    }
    location ^~ /fonts/ {
        root  /usr/share/nginx/html;
        try_files $uri =404;
        add_header Cache-Control "public, max-age=31536000, immutable" always;
        add_header X-Content-Type-Options "nosniff" always;
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

# www.tryberrybot.ru → apex (сертификат должен включать оба имени).
server {
    listen      443 ssl;
    listen      [::]:443 ssl;
    http2 on;
    server_name www.tryberrybot.ru;

    ssl_certificate     /etc/letsencrypt/live/tryberrybot.ru/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/tryberrybot.ru/privkey.pem;
    include             /etc/letsencrypt/options-ssl-nginx.conf;
    ssl_dhparam         /etc/letsencrypt/ssl-dhparams.pem;

    return 301 https://tryberrybot.ru$request_uri;
}
