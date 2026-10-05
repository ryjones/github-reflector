# Build and run on Alpine. The binary is static (CGO off), so the runtime layer
# needs nothing but CA certificates to reach Discord over TLS.
FROM golang:1-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/github-reflector .

FROM alpine:3
RUN apk add --no-cache ca-certificates \
 && adduser -D -H -u 10001 reflector
COPY --from=build /out/github-reflector /usr/local/bin/github-reflector
USER reflector
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/github-reflector"]
