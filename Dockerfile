FROM golang:1.23-alpine AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /sharemk ./cmd/sharemk

# ----

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /sharemk /usr/local/bin/sharemk

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/sharemk"]
