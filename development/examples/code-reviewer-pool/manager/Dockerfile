FROM node:26-slim@sha256:14bf3eac4bf209d906d3c41256597d3ab1f926b2e93a79e9bdfe1efd32454239

WORKDIR /srv
COPY package.json package-lock.json server.mjs lease-token.mjs ./
RUN npm ci --omit=dev --ignore-scripts --no-audit --no-fund

EXPOSE 7780
CMD ["node", "server.mjs"]
