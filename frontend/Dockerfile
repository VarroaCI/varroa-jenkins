# FRONTEND_DIST selects how the dist tree reaches the image. source (the default)
# builds it in this file. prebuilt copies it from the named build context
# prebuilt-dist, which holds the output of npm run build.
ARG FRONTEND_DIST=source

# Stage 1: Build
FROM node:20-alpine AS dist-source

WORKDIR /app

COPY package.json package-lock.json* ./
RUN npm ci

COPY . .
RUN npm run build

# Stage 1: dist tree copied from the prebuilt-dist build context
FROM scratch AS dist-prebuilt
COPY --from=prebuilt-dist / /app/dist/

FROM dist-${FRONTEND_DIST} AS dist

# Stage 2: Serve
FROM nginxinc/nginx-unprivileged:1.27-alpine

COPY nginx.conf /etc/nginx/templates/default.conf.template
COPY entrypoint.sh /entrypoint.sh
COPY --from=dist /app/dist /usr/share/nginx/html

EXPOSE 8080

ENTRYPOINT ["/entrypoint.sh"]
CMD ["nginx", "-g", "daemon off;"]
