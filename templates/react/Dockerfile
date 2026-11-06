# syntax=docker/dockerfile:1
FROM docker.io/library/node:22-alpine AS build
WORKDIR /src
COPY package.json ./
RUN npm install --no-audit --no-fund
COPY . .
RUN npm run build

# nginx-unprivileged runs as uid 101 and listens on a high port; it only writes to /tmp.
FROM docker.io/nginxinc/nginx-unprivileged:1.27-alpine
LABEL org.opencontainers.image.source="https://github.com/[[.Owner]]/[[.Repo]]"
COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /src/dist /usr/share/nginx/html
USER [[.UID]]
EXPOSE [[.Port]]
