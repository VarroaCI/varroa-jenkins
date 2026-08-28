#!/bin/sh
# CGI wrapper: busybox httpd executes /cgi-bin/git and passes the rest of the
# URL as PATH_INFO, e.g. /cgi-bin/git/localdev-bundle.git/info/refs.
export GIT_PROJECT_ROOT=/srv/git
export GIT_HTTP_EXPORT_ALL=1
exec /usr/libexec/git-core/git-http-backend
