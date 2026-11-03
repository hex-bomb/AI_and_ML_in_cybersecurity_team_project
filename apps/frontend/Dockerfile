# Тег, а не digest: digest, снятый на arm64, не запускается на amd64-раннере CI.
FROM node:22-alpine AS build
WORKDIR /src
COPY package.json package-lock.json ./
COPY web-app/package.json ./web-app/
COPY widget/package.json ./widget/
RUN npm ci
COPY . .
RUN npm run build

FROM nginx:1.29-alpine
COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /src/web-app/dist /usr/share/nginx/html
EXPOSE 80
