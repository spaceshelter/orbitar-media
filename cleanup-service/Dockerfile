FROM alpine:latest
RUN apk add --no-cache bash curl
COPY cleanup.sh /usr/local/bin/cleanup.sh
RUN chmod +x /usr/local/bin/cleanup.sh

# Verify cron installation
RUN which crond && \
    mkdir -p /var/log && \
    touch /var/log/cron.log

COPY cronfile /etc/crontabs/root
CMD ["crond", "-f"]
