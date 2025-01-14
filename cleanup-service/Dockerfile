FROM alpine:latest
RUN apk add --no-cache bash curl
COPY cleanup.sh /usr/local/bin/cleanup.sh
COPY cronfile /etc/crontabs/root
CMD ["crond", "-f"]
