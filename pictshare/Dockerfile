FROM alpine:3.24

# PHP 8.4 (>= 8.4.27 for CVE-2026-109618/109619). No php84-session: pictshare never uses sessions.
RUN apk add --no-cache bash socat wget curl nginx file ffmpeg unzip zlib \
        php84 \
        php84-fpm \
        php84-opcache \
        php84-curl \
        php84-openssl \
        php84-mbstring \
        php84-gd \
        php84-exif \
        php84-fileinfo \
        php84-sodium \
        php84-gmp \
        php84-dom \
        php84-xml \
        php84-simplexml \
        php84-ctype \
        php84-ftp \
        php84-phar \
        php84-pcntl \
        php84-pdo

# php84 only installs /usr/bin/php84; the upload path spawns `php` for background re-encodes
RUN ln -s /usr/bin/php84 /usr/bin/php

RUN curl -sS https://getcomposer.org/installer | /usr/bin/php -- --install-dir=/usr/bin --filename=composer
RUN mkdir -p /var/www
WORKDIR /var/www

# AWS SDK for the S3 storage controller, pinned by composer.lock, S3 only (see composer.json)
COPY storage-controllers/s3/composer.json storage-controllers/s3/composer.lock /var/www/storage-controllers/s3/
RUN cd /var/www/storage-controllers/s3 && COMPOSER_ALLOW_SUPERUSER=1 composer install --no-dev --no-interaction --no-progress

COPY . /var/www/.

COPY docker/rootfs/start.sh /etc/start.sh
RUN chmod +x /etc/start.sh

# nginx stuff
COPY docker/rootfs/nginx.conf /etc/nginx/http.d/default.conf
RUN mkdir -p /run/nginx
RUN mkdir -p /var/log/nginx
RUN sed -i 's/nobody/nginx/g' /etc/php84/php-fpm.d/www.conf
# Increase PHP-FPM workers (default is 5, too low for S3 blocking I/O)
RUN sed -i 's/pm.max_children = 5/pm.max_children = 25/g' /etc/php84/php-fpm.d/www.conf
# Allow environment variables to be passed to PHP-FPM workers
RUN sed -i 's/;clear_env = no/clear_env = no/g' /etc/php84/php-fpm.d/www.conf

# Since requests can trigger conversion, let's give the server enough time to respond
RUN sed -i "/max_execution_time/c\max_execution_time=3600" /etc/php84/php.ini
RUN sed -i "/max_input_time/c\max_input_time=3600" /etc/php84/php.ini
# no X-Powered-By: PHP/<version>
RUN sed -i "/^expose_php/c\expose_php=Off" /etc/php84/php.ini

WORKDIR /var/www/

# Volumes to mount
#VOLUME /var/lib/influxdb
VOLUME /var/www/data

EXPOSE 80

ENTRYPOINT ["/etc/start.sh"]
