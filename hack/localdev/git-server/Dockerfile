# Smart-HTTP git server for the localdev sample bundle.
# git-http-backend (apk git-daemon) served as a CGI by busybox httpd —
# the operator's shallow clones (--depth=1) need the smart protocol; plain
# nginx dumb-HTTP cannot serve them.
FROM alpine:3.22
RUN apk add --no-cache git git-daemon busybox-extras
COPY git-cgi.sh /srv/www/cgi-bin/git
RUN chmod +x /srv/www/cgi-bin/git && mkdir -p /srv/git
EXPOSE 8080
CMD ["httpd", "-f", "-vv", "-p", "8080", "-h", "/srv/www"]
