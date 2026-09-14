# Build stage. The dashboard is a static bundle: there is no server-side
# rendering, so nothing from this stage ships except the compiled assets.
FROM node:22-alpine AS build

WORKDIR /app

# Dependencies are installed from the lockfile and cached separately from the
# sources, so editing a component does not reinstall the world.
COPY package.json package-lock.json ./
RUN npm ci

COPY tsconfig*.json vite.config.ts index.html ./
COPY src ./src

# The type check runs here rather than only in CI: an image that builds from a
# tree that does not typecheck is an image nobody should deploy.
RUN npx tsc -b && npm run build

# Serve stage.
#
# The unprivileged image, not the standard one. Stock nginx starts its master
# process as root and drops privileges only for the workers; this variant runs
# entirely as an unprivileged user and listens on 8080 because a non-root
# process cannot bind a port below 1024. Nothing here needs root, and a
# container that has it is a container that can be asked to use it.
FROM nginxinc/nginx-unprivileged:1.27-alpine

COPY nginx.conf /etc/nginx/conf.d/default.conf

COPY --from=build /app/dist /usr/share/nginx/html

EXPOSE 8080

HEALTHCHECK --interval=10s --timeout=3s --retries=5 \
  CMD wget -q -O /dev/null http://localhost:8080/ || exit 1
