FROM golang:1.27.2-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/cabv-server ./cmd/cabv-server \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/cabv-migrate ./cmd/cabv-migrate \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/cabv-seed ./cmd/cabv-seed

FROM alpine:3.23
RUN apk add --no-cache ca-certificates \
    && addgroup -S cabv && adduser -S -G cabv cabv
WORKDIR /app
RUN mkdir -p /app/resources && chown cabv:cabv /app/resources && chmod 700 /app/resources
COPY --from=build /out/ /usr/local/bin/
COPY migrations/*.sql ./migrations/
USER root
ENV HTTP_ADDR=0.0.0.0:8080
EXPOSE 8080
CMD ["/bin/sh", "-c", "mkdir -p /app/resources && chown cabv:cabv /app/resources && chmod 700 /app/resources && exec su -p -s /bin/sh cabv -c 'exec cabv-server'"]
