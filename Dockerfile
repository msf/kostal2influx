# Build Stage
FROM golang:1.26-bookworm AS build-stage

RUN apt-get update && apt-get install -y \
        ca-certificates

WORKDIR /app

COPY Makefile go.mod go.sum ./
RUN make setup

COPY . ./
RUN make

# Final Stage
# Use the official Alpine image for a lean production container.
# https://hub.docker.com/_/alpine
# https://docs.docker.com/develop/develop-images/multistage-build/#use-multi-stage-builds
FROM alpine:3.21

RUN apk add --no-cache ca-certificates

WORKDIR /app

COPY --from=build-stage /app/kostal2influx  /app/
RUN chmod +x /app/

CMD ["/app/kostal2influx"]
