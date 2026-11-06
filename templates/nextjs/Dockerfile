# syntax=docker/dockerfile:1
FROM docker.io/library/node:22-alpine AS build
WORKDIR /src
COPY package.json ./
RUN npm install --no-audit --no-fund
COPY . .
RUN npm run build

FROM docker.io/library/node:22-alpine
LABEL org.opencontainers.image.source="https://github.com/[[.Owner]]/[[.Repo]]"
ENV NODE_ENV=production PORT=[[.Port]] HOSTNAME=0.0.0.0 NEXT_TELEMETRY_DISABLED=1
WORKDIR /app
COPY --from=build --chown=1000:1000 /src/.next/standalone ./
COPY --from=build --chown=1000:1000 /src/.next/static ./.next/static
COPY --from=build --chown=1000:1000 /src/public ./public
USER 1000:1000
EXPOSE [[.Port]]
CMD ["node", "server.js"]
