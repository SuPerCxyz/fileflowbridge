FROM alpine:3.18

ENV FFB_HTTP_PORT=8888
ENV FFB_TCP_PORT=56789
ENV APP_HOME=/app

ARG TARGETARCH

WORKDIR ${APP_HOME}

RUN apk add --no-cache ca-certificates tzdata

COPY bin/fileflowbridge-linux-${TARGETARCH} ${APP_HOME}/fileflowbridge

RUN chmod +x ${APP_HOME}/fileflowbridge && \
    addgroup -S appgroup && adduser -S appuser -G appgroup

USER appuser

ENTRYPOINT ["/app/fileflowbridge"]
