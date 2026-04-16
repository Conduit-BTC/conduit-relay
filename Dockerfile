FROM golang:1.25-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /conduitl2 ./cmd/demo

FROM alpine:3.22
RUN apk add --no-cache ca-certificates
COPY --from=builder /conduitl2 /usr/local/bin/conduitl2
EXPOSE 3334
ENV PORT=3334
ENTRYPOINT ["conduitl2"]
