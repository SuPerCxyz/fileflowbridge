FROM golang:1.24-alpine AS builder

WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /out/fileflowbridge ./bridge/

FROM alpine:3.18

ENV FFB_HTTP_PORT=8000
ENV FFB_TCP_PORT=8888
ENV FFB_MAX_FILE_SIZE=100
ENV FFB_TOKEN_LEN=8
ENV FFB_LOG_LEVEL=INFO
ENV APP_HOME=/app

WORKDIR ${APP_HOME}

RUN apk add --no-cache ca-certificates tzdata
RUN mkdir -p /var/log

COPY bridge/static ./static
COPY --from=builder /out/fileflowbridge ./fileflowbridge

RUN chmod +x ./fileflowbridge && \
    addgroup -S appgroup && adduser -S appuser -G appgroup

USER appuser

ENTRYPOINT ["./fileflowbridge"]