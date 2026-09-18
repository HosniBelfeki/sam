FROM node:26-slim@sha256:14bf3eac4bf209d906d3c41256597d3ab1f926b2e93a79e9bdfe1efd32454239

# The buddy: Google's Gemini CLI.
RUN npm install -g --ignore-scripts --no-audit --no-fund @google/gemini-cli@0.60.0

WORKDIR /srv
COPY package.json package-lock.json buddy_server.mjs ./
RUN npm ci --omit=dev --ignore-scripts --no-audit --no-fund

# Dev-only: replace <API_KEY> with a Google AI Studio free-tier key before `make kind`.
# Keep the edit local so it is never committed:
#   git update-index --skip-worktree development/examples/gemini-buddy-mcp/Dockerfile
ENV GEMINI_API_KEY=<API_KEY>
ENV GEMINI_CLI_TRUST_WORKSPACE=true

EXPOSE 7780
CMD ["node", "buddy_server.mjs"]
