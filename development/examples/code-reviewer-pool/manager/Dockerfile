FROM node:22-slim@sha256:83f487e0a63425e5b4d146fb5e5be574bcbe1b7b843d3ebafdd95eaf7767a7e5

WORKDIR /srv
COPY package.json server.mjs lease-token.mjs ./
RUN npm install

EXPOSE 7780
CMD ["node", "server.mjs"]
