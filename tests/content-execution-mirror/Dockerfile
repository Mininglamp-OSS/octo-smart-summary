FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY migrations ./migrations
RUN CGO_ENABLED=0 go build -o /bin/summary-api ./cmd/summary-api \
    && CGO_ENABLED=0 go build -o /bin/summary-worker ./cmd/summary-worker

FROM alpine:3.19
ENV TZ=Asia/Shanghai
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /bin/summary-api /bin/summary-api
COPY --from=builder /bin/summary-worker /bin/summary-worker
ENTRYPOINT ["/bin/summary-api"]
