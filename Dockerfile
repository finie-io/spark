FROM alpine:3.21
RUN apk add --no-cache ca-certificates docker-cli curl bash
WORKDIR /code
COPY spark /usr/local/bin/spark
ENTRYPOINT ["spark"]
