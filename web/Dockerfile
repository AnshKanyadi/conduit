# syntax=docker/dockerfile:1
# ---- Stage 1: Build the React app ------------------------------------------
FROM node:20-alpine AS builder

WORKDIR /app
COPY package*.json ./
RUN npm ci --prefer-offline

COPY . .
RUN npm run build        # emits to /app/dist

# ---- Stage 2: Serve with nginx ----------------------------------------------
FROM nginx:1.27-alpine

# envsubst is available in this image; we use it to inject CONDUIT_UPSTREAM
# at container startup so nginx.conf can reference the backend address without
# baking it into the image.
COPY --from=builder /app/dist  /usr/share/nginx/html
COPY nginx.conf                /etc/nginx/templates/default.conf.template

# Default upstream: the docker-compose service name.
ENV CONDUIT_UPSTREAM=server:8080

EXPOSE 80
