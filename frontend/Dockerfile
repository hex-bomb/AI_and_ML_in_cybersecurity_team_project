FROM node:22-alpine@sha256:85cdd100016a2e09927c0776bb7ebebcfefce5cd0af49893ada91cbc4d78ee22 AS build
WORKDIR /src
COPY package.json package-lock.json ./
COPY demo/package.json ./demo/
COPY widget/package.json ./widget/
RUN npm ci
COPY . .
RUN npm run build
FROM nginx:1.29-alpine@sha256:7dd09a6c4f8cab9a2d2cb98fb39790f220e8bc2ea106b2cebde64b90405e0be8
COPY --from=build /src/demo/dist /usr/share/nginx/html
EXPOSE 80
