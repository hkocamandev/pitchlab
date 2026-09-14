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
FROM nginx:1.27-alpine

COPY nginx.conf /etc/nginx/conf.d/default.conf

COPY --from=build /app/dist /usr/share/nginx/html

EXPOSE 80

HEALTHCHECK --interval=10s --timeout=3s --retries=5 \
  CMD wget -q -O /dev/null http://localhost/ || exit 1
