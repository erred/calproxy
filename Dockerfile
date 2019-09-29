FROM golang:alpine AS build

WORKDIR /app
ENV GO111MODULE=on CGO_ENABLED=0
RUN apk add --no-cache ca-certificates
COPY . .
RUN go build -mod=vendor -o app

FROM scratch

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /app/app /bin/

ENTRYPOINT ["/bin/app"]
