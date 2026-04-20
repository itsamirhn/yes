FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY go.mod ./
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 go build -o /client ./cmd/client
RUN CGO_ENABLED=0 go build -o /server ./cmd/server

FROM alpine:3.19
RUN apk add --no-cache ca-certificates
COPY --from=builder /client /usr/local/bin/client
COPY --from=builder /server /usr/local/bin/server
