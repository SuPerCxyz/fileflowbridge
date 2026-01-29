FROM alpine:3.18

# Set default environment variables
ENV FFB_HTTP_PORT=8000
ENV FFB_TCP_PORT=8888
ENV FFB_MAX_FILE_SIZE=100
ENV FFB_TOKEN_LEN=8
ENV FFB_LOG_LEVEL=INFO
ENV FFB_LOG_PATH=/var/log/fileflow_bridge.log
ENV APP_HOME=/app

ARG TARGETARCH

WORKDIR ${APP_HOME}

RUN apk add --no-cache ca-certificates tzdata

# Create log directory
RUN mkdir -p /var/log

# Copy static files directory to the expected location
COPY bridge/static ./bridge/static

# Copy and setup the appropriate binary based on TARGETARCH
COPY bin/fileflowbridge-linux-${TARGETARCH} ./fileflowbridge

RUN chmod +x ./fileflowbridge && \
    addgroup -S appgroup && adduser -S appuser -G appgroup

USER appuser

ENTRYPOINT ["./fileflowbridge"]