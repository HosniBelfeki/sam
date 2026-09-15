FROM node:22-slim@sha256:83f487e0a63425e5b4d146fb5e5be574bcbe1b7b843d3ebafdd95eaf7767a7e5

# The review agent: Google's Gemini CLI.
RUN npm install -g @google/gemini-cli

WORKDIR /srv
COPY package.json review_server.mjs lease-token.mjs ./
RUN npm install

# Dev-only: replace <API_KEY> with a Google AI Studio free-tier key before `make kind`.
# Keep the edit local so it is never committed:
#   git update-index --skip-worktree development/examples/code-reviewer-pool/reviewer/Dockerfile
ENV GEMINI_API_KEY=<API_KEY>
ENV GEMINI_CLI_TRUST_WORKSPACE=true

EXPOSE 7779
CMD ["node", "review_server.mjs"]
