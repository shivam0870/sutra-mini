FROM node:24-bookworm-slim AS dependencies
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci

FROM dependencies AS source
COPY . .
ENV NEXT_TELEMETRY_DISABLED=1

FROM source AS e2e
RUN npx playwright install --with-deps chromium
CMD ["npm", "run", "test:e2e"]

FROM source AS build
ARG API_URL=http://api:8080
ENV API_URL=$API_URL
RUN npm run build

FROM node:24-bookworm-slim AS runtime
WORKDIR /app
ENV NODE_ENV=production NEXT_TELEMETRY_DISABLED=1 HOSTNAME=0.0.0.0 PORT=3000
COPY --from=build --chown=node:node /app/.next/standalone ./
COPY --from=build --chown=node:node /app/.next/static ./.next/static
USER node
EXPOSE 3000
CMD ["node", "server.js"]
