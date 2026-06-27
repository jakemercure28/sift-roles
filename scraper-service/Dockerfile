# TypeScript scraper worker.
# Build context is the repo root (see docker-compose.yml). The worker is now
# self-contained TypeScript; the only repo file it reads at runtime is
# data.example/companies.json (the onboarding gate's fallback profile baseline).
FROM node:20-bookworm-slim AS deps
WORKDIR /app/scraper-service
COPY scraper-service/package.json scraper-service/package-lock.json ./
RUN npm ci

FROM deps AS build
COPY scraper-service/tsconfig.json scraper-service/tsconfig.build.json ./
COPY scraper-service/src ./src
RUN npm run build

FROM node:20-bookworm-slim AS runtime-deps
WORKDIR /app/scraper-service
COPY scraper-service/package.json scraper-service/package-lock.json ./
RUN npm ci --omit=dev

FROM node:20-bookworm-slim AS runtime
WORKDIR /app

COPY --from=runtime-deps /app/scraper-service/node_modules ./scraper-service/node_modules
COPY --from=build /app/scraper-service/dist ./scraper-service/dist
COPY scraper-service/package.json scraper-service/package-lock.json ./scraper-service/

COPY data.example ./data.example

ENV APP_ROOT=/app \
    DATA_DIR=/app/data \
    SCRAPER_PORT=4040 \
    SCRAPER_HOST=0.0.0.0
EXPOSE 4040
CMD ["node", "scraper-service/dist/index.js"]
