# Сборка бэкенда (контекст сборки — корень проекта: docker build -f server/Dockerfile .)
FROM golang:1.24-alpine AS builder
WORKDIR /build
COPY server/ ./server/
RUN cd server && go mod download && go build -o ../wedding-rsvp .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /build/wedding-rsvp .
COPY index.html styles.css script.js magic-ring_14234767.png ./static/
COPY ["2026-02-02 19.31.22.jpg", "./static/"]

ENV STATIC_DIR=/app/static
ENV PORT=8080
EXPOSE 8080
CMD ["./wedding-rsvp"]
