# syntax=docker/dockerfile:1

FROM golang:1.27.3-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/enclava

FROM alpine:3.22

RUN apk add --no-cache ca-certificates poppler-utils tesseract-ocr tesseract-ocr-data-eng bubblewrap \
	&& addgroup -g 10001 app \
	&& adduser -D -u 10001 -G app app

COPY --from=build /out/app /usr/local/bin/app
COPY docker/enclava-go-cap-entrypoint /usr/local/bin/enclava-go-cap-entrypoint
RUN chmod 0555 /usr/local/bin/app /usr/local/bin/enclava-go-cap-entrypoint

USER 10001:10001
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/enclava-go-cap-entrypoint"]
